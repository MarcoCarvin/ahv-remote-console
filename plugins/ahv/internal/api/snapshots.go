// Package api — Prism v3 VM snapshots, modeled as VM RECOVERY POINTS.
//
// v3 has NO /vm_snapshots resource; a "VM snapshot" is a vm_recovery_point.
// Endpoints in this file:
//
//	POST /api/nutanix/v3/vms/{uuid}/snapshot              — create a snapshot
//	POST /api/nutanix/v3/vm_recovery_points/list          — list recovery points
//	POST /api/nutanix/v3/vm_recovery_points/{uuid}/restore — restore (creates a VM)
//	GET  /api/nutanix/v3/tasks/{uuid}                      — task poll (shared)
//
// Create and restore are procedural POSTs: they return 202 + a bare
// procedural_response {task_uuid} (NOT the status.execution_context.task_uuid
// shape the /images and /vms PUT flows use), so we read task_uuid directly and
// await it like power_v3.go / mount.go.
//
// RESTORE SEMANTICS (v3): /vm_recovery_points/{uuid}/restore CREATES a NEW VM
// from the recovery point. It is NOT a guaranteed in-place revert of the source
// VM. The runtime/renderer copy must say so honestly; this client never prompts.

package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"time"
)

// VMSnapshot is one recovery point, trimmed to what the console panel renders.
// Timestamps are RFC3339-ish upstream strings (passed through verbatim — the
// renderer formats them).
type VMSnapshot struct {
	UUID         string
	Name         string
	CreatedAt    string
	ExpiresAt    string
	State        string
	SnapshotType string // recovery_point_type: CRASH_CONSISTENT / APPLICATION_CONSISTENT / LIVE
}

// CreateSnapshot creates a crash-consistent recovery point named `name` of the
// VM and waits for the create task. Returns the create task UUID.
//
// The snapshotUUID return is always empty: POST /vms/{uuid}/snapshot returns a
// procedural_response that carries ONLY the task UUID, not the new recovery
// point's UUID. Callers that need the new entry re-list. The second return is
// kept so the plugin's (task, snap, err) call site stays stable.
//
// A snapshot create is a VM-config write; the plugin records it as vm_update
// evidence.
func (c *Client) CreateSnapshot(ctx context.Context, vmUUID, name string) (taskUUID string, snapshotUUID string, err error) {
	if vmUUID == "" {
		return "", "", errors.New("create snapshot: vmUUID required")
	}
	if name == "" {
		return "", "", errors.New("create snapshot: name required")
	}
	// vm_snapshot_input: name + recovery_point_type. No metadata/spec envelope —
	// this is a procedural call on the VM, not an intentful resource create.
	body := map[string]any{
		"name":                name,
		"recovery_point_type": "CRASH_CONSISTENT",
	}
	var resp proceduralResp
	path := "/api/nutanix/v3/vms/" + url.PathEscape(vmUUID) + "/snapshot"
	if err := c.doJSON(ctx, http.MethodPost, path, body, &resp); err != nil {
		return "", "", fmt.Errorf("create snapshot: %w", err)
	}
	taskUUID = string(resp.TaskUUID)
	if err := c.waitForTask(ctx, taskUUID, 5*time.Minute); err != nil {
		return taskUUID, "", fmt.Errorf("create snapshot task: %w", err)
	}
	return taskUUID, "", nil
}

// ListSnapshots returns the VM's recovery points, newest-first. The list
// endpoint's FIQL `filter` is deprecated and unreliable across PC versions, so
// we list and filter client-side by the recovery point's
// parent_vm_reference.uuid (the source VM). length:250 caps the page — typical
// per-VM recovery-point counts are well under that.
func (c *Client) ListSnapshots(ctx context.Context, vmUUID string) ([]VMSnapshot, error) {
	if vmUUID == "" {
		return nil, errors.New("list snapshots: vmUUID required")
	}
	body := map[string]any{"kind": "vm_recovery_point", "length": 250}
	var resp struct {
		Entities []struct {
			Metadata struct {
				UUID         string `json:"uuid"`
				CreationTime string `json:"creation_time"`
			} `json:"metadata"`
			Spec struct {
				Name      string `json:"name"`
				Resources struct {
					CreationTime      string `json:"creation_time"`
					ExpirationTime    string `json:"expiration_time"`
					RecoveryPointType string `json:"recovery_point_type"`
					ParentVMReference struct {
						UUID string `json:"uuid"`
					} `json:"parent_vm_reference"`
				} `json:"resources"`
			} `json:"spec"`
			Status struct {
				Name      string `json:"name"`
				State     string `json:"state"`
				Resources struct {
					CreationTime      string `json:"creation_time"`
					ExpirationTime    string `json:"expiration_time"`
					RecoveryPointType string `json:"recovery_point_type"`
					ParentVMReference struct {
						UUID string `json:"uuid"`
					} `json:"parent_vm_reference"`
				} `json:"resources"`
			} `json:"status"`
		} `json:"entities"`
	}
	if err := c.doJSON(ctx, http.MethodPost, "/api/nutanix/v3/vm_recovery_points/list", body, &resp); err != nil {
		return nil, fmt.Errorf("list snapshots: %w", err)
	}
	out := make([]VMSnapshot, 0, len(resp.Entities))
	for _, e := range resp.Entities {
		// Source VM lives on parent_vm_reference. Prefer status (server truth),
		// fall back to spec for entries still realizing.
		parent := e.Status.Resources.ParentVMReference.UUID
		if parent == "" {
			parent = e.Spec.Resources.ParentVMReference.UUID
		}
		if parent != vmUUID {
			continue
		}
		name := e.Status.Name
		if name == "" {
			name = e.Spec.Name
		}
		// creation_time: status.resources first, then spec.resources, then the
		// metadata-level creation_time as a last resort.
		created := firstNonEmpty(e.Status.Resources.CreationTime, e.Spec.Resources.CreationTime, e.Metadata.CreationTime)
		expires := firstNonEmpty(e.Status.Resources.ExpirationTime, e.Spec.Resources.ExpirationTime)
		rpType := firstNonEmpty(e.Status.Resources.RecoveryPointType, e.Spec.Resources.RecoveryPointType)
		out = append(out, VMSnapshot{
			UUID:         e.Metadata.UUID,
			Name:         name,
			CreatedAt:    created,
			ExpiresAt:    expires,
			State:        e.Status.State,
			SnapshotType: rpType,
		})
	}
	// Newest-first. CreatedAt is an RFC3339 string, so a reverse string sort
	// orders correctly; empty timestamps sort last.
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].CreatedAt > out[j].CreatedAt
	})
	return out, nil
}

// RestoreSnapshot restores the given recovery point and waits for the task.
//
// IMPORTANT v3 SEMANTIC: this CREATES A NEW VM from the recovery point — it is
// NOT a guaranteed in-place revert of the source VM. The vmUUID arg is kept for
// the plugin call site and evidence logging; the restore is keyed entirely on
// the recovery point's own UUID (in the PATH — URL-escaped here).
//
// This client NEVER prompts; the caller MUST have confirmed with the operator.
func (c *Client) RestoreSnapshot(ctx context.Context, vmUUID, snapshotUUID string) (taskUUID string, err error) {
	if vmUUID == "" {
		return "", errors.New("restore snapshot: vmUUID required")
	}
	if snapshotUUID == "" {
		return "", errors.New("restore snapshot: snapshotUUID required")
	}
	// vm_recovery_point_restore_input. We send the minimal body and let the
	// backend create a VM with the recovery point's captured spec; we do not
	// override the name or resources.
	body := map[string]any{}
	var resp proceduralResp
	path := "/api/nutanix/v3/vm_recovery_points/" + url.PathEscape(snapshotUUID) + "/restore"
	if err := c.doJSON(ctx, http.MethodPost, path, body, &resp); err != nil {
		return "", fmt.Errorf("restore snapshot: %w", err)
	}
	taskUUID = string(resp.TaskUUID)
	// Restore is slower than create (it materializes a VM from the RP), so allow
	// more time.
	if err := c.waitForTask(ctx, taskUUID, 10*time.Minute); err != nil {
		return taskUUID, fmt.Errorf("restore snapshot task: %w", err)
	}
	return taskUUID, nil
}

// proceduralResp models the v3 procedural_response: a 202 body carrying only a
// task UUID. task_uuid is returned as a string here, but reuse the
// string-or-slice decoder to tolerate the older array-form quirk seen on other
// v3 procedural endpoints.
type proceduralResp struct {
	TaskUUID jsonStringOrSlice `json:"task_uuid"`
}

// firstNonEmpty returns the first non-empty string of its args, or "".
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
