package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// snapMock is an httptest-backed Prism v3 mock for the REAL recovery-point
// endpoints that back VM snapshots:
//
//	POST /api/nutanix/v3/vms/{uuid}/snapshot               — create
//	POST /api/nutanix/v3/vm_recovery_points/list           — list
//	POST /api/nutanix/v3/vm_recovery_points/{uuid}/restore — restore (creates a VM)
//	GET  /api/nutanix/v3/tasks/{uuid}                       — task poll
//
// It captures the create/restore request bodies + paths so tests can assert the
// wire shape, and serves a fixed list payload for the filter test. Create and
// restore reply with a procedural_response {task_uuid}.
type snapMock struct {
	t  *testing.T
	mu sync.Mutex

	createHit   int
	createBody  []byte
	createPath  string
	listHit     int
	restoreHit  int
	restoreBody []byte
	restorePath string

	// listEntities is the raw entities array served by /vm_recovery_points/list.
	listEntities []map[string]any
}

func (m *snapMock) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/nutanix/v3/vm_recovery_points/list", func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		m.listHit++
		entities := m.listEntities
		m.mu.Unlock()
		writeJSON(w, http.StatusOK, map[string]any{"entities": entities, "api_version": "3.1.0"})
	})
	// /vm_recovery_points/{uuid}/restore.
	mux.HandleFunc("/api/nutanix/v3/vm_recovery_points/", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		m.mu.Lock()
		m.restoreHit++
		m.restoreBody = body
		m.restorePath = r.URL.Path
		m.mu.Unlock()
		writeJSON(w, http.StatusAccepted, map[string]any{"task_uuid": "task-restore"})
	})
	// POST /vms/{uuid}/snapshot — only the .../snapshot suffix lands here.
	mux.HandleFunc("/api/nutanix/v3/vms/", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		m.mu.Lock()
		m.createHit++
		m.createBody = body
		m.createPath = r.URL.Path
		m.mu.Unlock()
		writeJSON(w, http.StatusAccepted, map[string]any{"task_uuid": "task-create"})
	})
	mux.HandleFunc("/api/nutanix/v3/tasks/", func(w http.ResponseWriter, r *http.Request) {
		out := prismTaskResp{Status: "SUCCEEDED"}
		out.ProgressInfo.PercentageComplete = 100
		writeJSON(w, http.StatusOK, out)
	})
	return mux
}

// recoveryPointEntity builds one /vm_recovery_points/list entity whose source VM
// (parent_vm_reference) is vmUUID.
func recoveryPointEntity(uuid, name, vmUUID, createdAt string) map[string]any {
	return map[string]any{
		"metadata": map[string]any{
			"uuid":          uuid,
			"creation_time": createdAt,
			"kind":          "vm_recovery_point",
		},
		"spec": map[string]any{
			"name": name,
			"resources": map[string]any{
				"recovery_point_type": "CRASH_CONSISTENT",
				"parent_vm_reference": map[string]any{"kind": "vm", "uuid": vmUUID},
			},
		},
		"status": map[string]any{
			"name":  name,
			"state": "COMPLETE",
			"resources": map[string]any{
				"creation_time":       createdAt,
				"expiration_time":     "2026-12-31T00:00:00Z",
				"recovery_point_type": "CRASH_CONSISTENT",
				"parent_vm_reference": map[string]any{"kind": "vm", "uuid": vmUUID},
			},
		},
	}
}

// TestCreateSnapshot_PostsToVMSnapshotEndpoint: create POSTs exactly once to
// /vms/{uuid}/snapshot carrying name + recovery_point_type, and awaits the task.
func TestCreateSnapshot_PostsToVMSnapshotEndpoint(t *testing.T) {
	mock := &snapMock{t: t}
	srv := httptest.NewServer(mock.handler())
	defer srv.Close()
	c := newTestClient(t, srv)

	taskUUID, snapUUID, err := c.CreateSnapshot(context.Background(), "vm-1", "before-upgrade")
	if err != nil {
		t.Fatalf("create snapshot: %v", err)
	}
	if mock.createHit != 1 {
		t.Fatalf("expected exactly one POST to /vms/{uuid}/snapshot, got %d", mock.createHit)
	}
	if !strings.HasSuffix(mock.createPath, "/vms/vm-1/snapshot") {
		t.Errorf("create path = %q, want .../vms/vm-1/snapshot", mock.createPath)
	}
	if taskUUID != "task-create" {
		t.Errorf("taskUUID = %q, want task-create", taskUUID)
	}
	// procedural_response carries no recovery-point UUID, so snapUUID is empty.
	if snapUUID != "" {
		t.Errorf("snapshotUUID = %q, want empty (procedural_response has no RP uuid)", snapUUID)
	}
	var body map[string]any
	if err := json.Unmarshal(mock.createBody, &body); err != nil {
		t.Fatalf("decode create body: %v", err)
	}
	if body["name"] != "before-upgrade" {
		t.Errorf("create body must carry name=before-upgrade: %s", mock.createBody)
	}
	if body["recovery_point_type"] != "CRASH_CONSISTENT" {
		t.Errorf("create body must carry recovery_point_type=CRASH_CONSISTENT: %s", mock.createBody)
	}
}

// TestCreateSnapshot_RejectsEmptyName: an empty name is refused BEFORE any
// request is made.
func TestCreateSnapshot_RejectsEmptyName(t *testing.T) {
	mock := &snapMock{t: t}
	srv := httptest.NewServer(mock.handler())
	defer srv.Close()
	c := newTestClient(t, srv)

	if _, _, err := c.CreateSnapshot(context.Background(), "vm-1", ""); err == nil {
		t.Fatal("expected error for empty snapshot name")
	}
	if mock.createHit != 0 {
		t.Errorf("no request must be sent for an empty name (createHit=%d)", mock.createHit)
	}
}

// TestListSnapshots_FiltersToVM: list filters client-side to the source VM via
// parent_vm_reference, dropping recovery points of other VMs, and maps the
// recovery-point fields (name, creation/expiration time, state, type).
func TestListSnapshots_FiltersToVM(t *testing.T) {
	mock := &snapMock{t: t}
	mock.listEntities = []map[string]any{
		recoveryPointEntity("rp-a", "older", "vm-1", "2026-06-01T10:00:00Z"),
		recoveryPointEntity("rp-other", "foreign", "vm-2", "2026-06-02T10:00:00Z"),
		recoveryPointEntity("rp-b", "newer", "vm-1", "2026-06-03T10:00:00Z"),
	}
	srv := httptest.NewServer(mock.handler())
	defer srv.Close()
	c := newTestClient(t, srv)

	snaps, err := c.ListSnapshots(context.Background(), "vm-1")
	if err != nil {
		t.Fatalf("list snapshots: %v", err)
	}
	if mock.listHit != 1 {
		t.Fatalf("expected exactly one POST to /vm_recovery_points/list, got %d", mock.listHit)
	}
	if len(snaps) != 2 {
		t.Fatalf("expected 2 recovery points for vm-1 (vm-2's dropped), got %d: %+v", len(snaps), snaps)
	}
	for _, s := range snaps {
		if s.UUID == "rp-other" {
			t.Errorf("vm-2's recovery point leaked into vm-1's list: %+v", s)
		}
	}
	// Newest-first.
	if snaps[0].UUID != "rp-b" || snaps[1].UUID != "rp-a" {
		t.Errorf("expected newest-first [rp-b, rp-a], got [%s, %s]", snaps[0].UUID, snaps[1].UUID)
	}
	// Field mapping on the newest entry.
	top := snaps[0]
	if top.Name != "newer" {
		t.Errorf("Name = %q, want newer", top.Name)
	}
	if top.CreatedAt != "2026-06-03T10:00:00Z" {
		t.Errorf("CreatedAt = %q, want 2026-06-03T10:00:00Z", top.CreatedAt)
	}
	if top.ExpiresAt != "2026-12-31T00:00:00Z" {
		t.Errorf("ExpiresAt = %q, want 2026-12-31T00:00:00Z", top.ExpiresAt)
	}
	if top.State != "COMPLETE" {
		t.Errorf("State = %q, want COMPLETE", top.State)
	}
	if top.SnapshotType != "CRASH_CONSISTENT" {
		t.Errorf("SnapshotType = %q, want CRASH_CONSISTENT", top.SnapshotType)
	}
}

// TestRestoreSnapshot_PostsToRecoveryPointRestorePath: restore POSTs to
// /vm_recovery_points/{uuid}/restore and awaits the task. The recovery point
// UUID rides the PATH (URL-escaped).
func TestRestoreSnapshot_PostsToRecoveryPointRestorePath(t *testing.T) {
	mock := &snapMock{t: t}
	srv := httptest.NewServer(mock.handler())
	defer srv.Close()
	c := newTestClient(t, srv)

	taskUUID, err := c.RestoreSnapshot(context.Background(), "vm-1", "rp-a")
	if err != nil {
		t.Fatalf("restore snapshot: %v", err)
	}
	if mock.restoreHit != 1 {
		t.Fatalf("expected exactly one POST to the restore path, got %d", mock.restoreHit)
	}
	if !strings.HasSuffix(mock.restorePath, "/vm_recovery_points/rp-a/restore") {
		t.Errorf("restore path = %q, want .../vm_recovery_points/rp-a/restore", mock.restorePath)
	}
	if taskUUID != "task-restore" {
		t.Errorf("taskUUID = %q, want task-restore", taskUUID)
	}
}

// TestRestoreSnapshot_RejectsEmptyArgs: an empty snapshotUUID or vmUUID is
// refused BEFORE any request is made.
func TestRestoreSnapshot_RejectsEmptyArgs(t *testing.T) {
	mock := &snapMock{t: t}
	srv := httptest.NewServer(mock.handler())
	defer srv.Close()
	c := newTestClient(t, srv)

	if _, err := c.RestoreSnapshot(context.Background(), "vm-1", ""); err == nil {
		t.Error("expected error for empty snapshotUUID")
	}
	if _, err := c.RestoreSnapshot(context.Background(), "", "rp-a"); err == nil {
		t.Error("expected error for empty vmUUID")
	}
	if mock.restoreHit != 0 {
		t.Errorf("no request must be sent for empty args (restoreHit=%d)", mock.restoreHit)
	}
}
