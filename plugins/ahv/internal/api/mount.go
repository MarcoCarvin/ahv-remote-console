// Package api — merged installer-mount spec update for Prism v3.
//
// AttachCDROMAndSerialPort folds the two single-purpose VM edits the mount
// flow used to issue back-to-back (AttachCDROM's media change and
// EnsureSerialPort's port append) into ONE GET → mutate → PUT → task wait.
// One spec round-trip means one spec_version bump and one Prism task — no
// window for the second edit to 409 on the first one's in-flight task.
//
// Like the single-purpose ops, the round-trip preserves every unmodeled
// field: the mutation happens on the generic string-keyed document (the
// EnsureSerialPort pattern), metadata — including spec_version — rides
// through untouched, and status is dropped before the PUT (Prism rejects
// status on write).

package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"time"
)

// isConflict409 reports whether err is an upstream HTTP 409 — Prism's
// CONCURRENT_REQUESTS_NOT_ALLOWED / spec_version conflict on a VM spec PUT.
// Both resolve the same way: a fresh GET picks up the new spec_version, so
// the spec-update paths retry exactly once on it.
func isConflict409(err error) bool {
	return HTTPStatus(err) == http.StatusConflict
}

// MountUpdateResult describes what AttachCDROMAndSerialPort changed.
type MountUpdateResult struct {
	// TaskUUID of the single spec-update task ("" when nothing needed a PUT).
	TaskUUID string
	// DriveLetterHint is the best-guess guest drive letter (see AttachCDROM).
	DriveLetterHint string
	// AttachReused is true when the image was already in a CD-ROM drive (no
	// media change was part of the PUT).
	AttachReused bool
	// SerialAdded is true when the serial port was appended in this PUT.
	SerialAdded bool
	// SerialAlreadyPresent is true when a port at the requested index was
	// already in the spec (no serial mutation was part of the PUT).
	SerialAlreadyPresent bool
	// PowerState is status.resources.power_state at read time ("ON"/"OFF").
	PowerState string
}

// AttachCDROMAndSerialPort mounts imageUUID into the VM's first CD-ROM drive
// AND ensures a serial port at serialIndex, in a single spec round-trip.
// Idempotent per edit: an already-mounted image / already-present port is
// skipped; if neither edit is needed, no PUT is made at all.
//
// Retries exactly once on HTTP 409 (fresh GET, re-apply mutations, one more
// PUT) — Prism rejects concurrent VM spec edits and the runtime has observed
// real attach 409s in the field.
//
// NOTE: the serial edit only APPLIES while the VM is powered OFF (AHV cannot
// hot-add a serial port); callers gate on the power state. The CD-ROM media
// change is hot-pluggable either way.
func (c *Client) AttachCDROMAndSerialPort(ctx context.Context, vmUUID, imageUUID string, serialIndex int) (*MountUpdateResult, error) {
	if vmUUID == "" {
		return nil, errors.New("attach cdrom+serial: vmUUID required")
	}
	if imageUUID == "" {
		return nil, errors.New("attach cdrom+serial: imageUUID required")
	}
	res, err := c.attachCDROMAndSerialPortOnce(ctx, vmUUID, imageUUID, serialIndex)
	if isConflict409(err) {
		if c.log != nil {
			c.log.Warn("merged spec update hit 409 — refetching spec and retrying once",
				"vm", vmUUID)
		}
		res, err = c.attachCDROMAndSerialPortOnce(ctx, vmUUID, imageUUID, serialIndex)
	}
	return res, err
}

// attachCDROMAndSerialPortOnce is a single GET → mutate-both → PUT → task
// wait attempt; AttachCDROMAndSerialPort wraps it with the one-shot 409 retry.
func (c *Client) attachCDROMAndSerialPortOnce(ctx context.Context, vmUUID, imageUUID string, serialIndex int) (*MountUpdateResult, error) {
	var full json.RawMessage
	if err := c.doJSON(ctx, http.MethodGet, "/api/nutanix/v3/vms/"+url.PathEscape(vmUUID), nil, &full); err != nil {
		return nil, fmt.Errorf("get vm: %w", err)
	}

	var parsed struct {
		Spec struct {
			Resources struct {
				SerialPortList []struct {
					Index int `json:"index"`
				} `json:"serial_port_list"`
				DiskList []VMDisk `json:"disk_list"`
			} `json:"resources"`
		} `json:"spec"`
		Status struct {
			Resources struct {
				PowerState string `json:"power_state"`
			} `json:"resources"`
		} `json:"status"`
	}
	if err := json.Unmarshal(full, &parsed); err != nil {
		return nil, fmt.Errorf("parse vm spec: %w", err)
	}

	out := &MountUpdateResult{
		DriveLetterHint: driveLetterForCDROM(0),
		PowerState:      parsed.Status.Resources.PowerState,
	}
	for _, sp := range parsed.Spec.Resources.SerialPortList {
		if sp.Index == serialIndex {
			out.SerialAlreadyPresent = true
			break
		}
	}
	// Same media-change strategy as AttachCDROM: mutate the FIRST existing
	// CD-ROM's data_source_reference; a missing CD-ROM device is an
	// operator-actionable error (AHV can't hot-add IDE/SATA devices).
	cdromIdx := -1
	for i, d := range parsed.Spec.Resources.DiskList {
		if d.DeviceProperties.DeviceType != "CDROM" {
			continue
		}
		if d.DataSourceReference != nil && d.DataSourceReference.UUID == imageUUID {
			out.AttachReused = true
		}
		if cdromIdx == -1 {
			cdromIdx = i
		}
	}
	if cdromIdx == -1 {
		return nil, fmt.Errorf(
			"VM has no CD-ROM device — power off the VM in Prism, add an empty CD-ROM device, restart, then retry")
	}
	if out.AttachReused && out.SerialAlreadyPresent {
		return out, nil // both edits already in place — nothing to PUT
	}

	var doc map[string]interface{}
	if err := json.Unmarshal(full, &doc); err != nil {
		return nil, fmt.Errorf("parse vm doc: %w", err)
	}
	spec, _ := doc["spec"].(map[string]interface{})
	if spec == nil {
		return nil, fmt.Errorf("vm spec is nil")
	}
	resources, _ := spec["resources"].(map[string]interface{})
	if resources == nil {
		return nil, fmt.Errorf("vm resources is nil")
	}

	if !out.SerialAlreadyPresent {
		portList, _ := resources["serial_port_list"].([]interface{})
		portList = append(portList, map[string]interface{}{
			"index":        serialIndex,
			"is_connected": true,
		})
		resources["serial_port_list"] = portList
		out.SerialAdded = true
	}
	if !out.AttachReused {
		diskList, _ := resources["disk_list"].([]interface{})
		if cdromIdx >= len(diskList) {
			return nil, fmt.Errorf("vm disk_list shape mismatch")
		}
		disk, _ := diskList[cdromIdx].(map[string]interface{})
		if disk == nil {
			return nil, fmt.Errorf("vm disk entry shape mismatch")
		}
		disk["data_source_reference"] = map[string]interface{}{
			"kind": "image",
			"uuid": imageUUID,
		}
	}

	// PUT the whole document back minus status; metadata (spec_version
	// included) round-trips verbatim, exactly like the single-purpose ops.
	delete(doc, "status")
	var putResp prismCreateImageResp // /vms PUT shares this shape
	if err := c.doJSON(ctx, http.MethodPut, "/api/nutanix/v3/vms/"+url.PathEscape(vmUUID), doc, &putResp); err != nil {
		return nil, fmt.Errorf("update vm: %w", err)
	}
	out.TaskUUID = string(putResp.Status.ExecutionContext.TaskUUID)
	// 5 minutes mirrors AttachCDROM — the merged task includes the media
	// change, which can be slow on a busy cluster.
	if err := c.waitForTask(ctx, out.TaskUUID, 5*time.Minute); err != nil {
		return nil, fmt.Errorf("wait for merged update task: %w", err)
	}
	return out, nil
}

// SerialPortState reports, with one GET and no mutation, whether the VM's
// spec already carries a serial port at index and what the VM's power state
// is. The mount flow uses it to choose between the merged single-PUT update,
// the CD-ROM-only attach, and the consent-gated power cycle.
func (c *Client) SerialPortState(ctx context.Context, vmUUID string, index int) (present bool, powerState string, err error) {
	var resp struct {
		Spec struct {
			Resources struct {
				SerialPortList []struct {
					Index int `json:"index"`
				} `json:"serial_port_list"`
			} `json:"resources"`
		} `json:"spec"`
		Status struct {
			Resources struct {
				PowerState string `json:"power_state"`
			} `json:"resources"`
		} `json:"status"`
	}
	if err := c.doJSON(ctx, http.MethodGet, "/api/nutanix/v3/vms/"+url.PathEscape(vmUUID), nil, &resp); err != nil {
		return false, "", fmt.Errorf("get vm: %w", err)
	}
	for _, sp := range resp.Spec.Resources.SerialPortList {
		if sp.Index == index {
			present = true
			break
		}
	}
	return present, resp.Status.Resources.PowerState, nil
}
