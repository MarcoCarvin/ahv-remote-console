package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// vmMock is an httptest.Server-backed Prism v3 mock covering the
// VM-side endpoints AttachCDROM exercises:
//
//	GET /api/nutanix/v3/vms/{uuid}
//	PUT /api/nutanix/v3/vms/{uuid}
//	GET /api/nutanix/v3/tasks/{uuid}
//
// Tests adjust the VM payload (existing disk_list contents) and
// the task status to drive each path.
type vmMock struct {
	t  *testing.T
	mu sync.Mutex

	// vmUUID is the path component the test rig and mock agree on.
	vmUUID string
	// vmBody is the JSON body returned for GET /vms/{uuid}.
	// Tests build it via vmJSON below.
	vmBody []byte

	// putBody captures the last PUT /vms/{uuid} body so tests
	// can assert on the disk_list mutation.
	putBody []byte
	// putHit increments on each PUT call.
	putHit int
	// getHit increments on each GET /vms/{uuid} call, so retry tests
	// can assert a fresh GET preceded the second PUT.
	getHit int
	// onPut, when set, decides the PUT response per attempt (1-based)
	// instead of the default 202 + task. Called outside the mock lock
	// with the captured body, so it may mutate mock state (e.g. bump
	// vmBody's spec_version to simulate a concurrent edit).
	onPut func(attempt int, body []byte) (status int, resp any)

	// taskStatus is what GET /tasks/{uuid} returns. Default
	// SUCCEEDED.
	taskStatus string
	taskError  string

	// extraSpecFields is folded into the mock VM body so we can
	// confirm the splice preserves them.
	extraSpecFields map[string]any
}

// vmJSON builds a minimal VM payload that GetVMSpec will accept.
func vmJSON(uuid string, disks []VMDisk, extraSpecFields map[string]any) []byte {
	spec := map[string]any{
		"name": "test-vm",
		"resources": map[string]any{
			"disk_list":   disks,
			"num_sockets": 2,
		},
	}
	for k, v := range extraSpecFields {
		spec[k] = v
	}
	out := map[string]any{
		"metadata": map[string]any{
			"kind":         "vm",
			"uuid":         uuid,
			"spec_version": 7,
		},
		"spec": spec,
	}
	b, _ := json.Marshal(out)
	return b
}

func (m *vmMock) handler() http.Handler {
	mux := http.NewServeMux()
	getPath := "/api/nutanix/v3/vms/" + m.vmUUID
	mux.HandleFunc(getPath, func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			m.mu.Lock()
			m.getHit++
			body := m.vmBody
			m.mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(body)
		case http.MethodPut:
			body, _ := io.ReadAll(io.LimitReader(r.Body, 32*1024*1024))
			m.mu.Lock()
			m.putBody = body
			m.putHit++
			attempt := m.putHit
			hook := m.onPut
			m.mu.Unlock()
			if hook != nil {
				status, resp := hook(attempt, body)
				writeJSON(w, status, resp)
				return
			}
			out := prismCreateImageResp{}
			out.Metadata.UUID = m.vmUUID
			out.Status.ExecutionContext.TaskUUID = "task-attach"
			writeJSON(w, http.StatusAccepted, out)
		default:
			http.Error(w, "method", http.StatusMethodNotAllowed)
		}
	})
	mux.HandleFunc("/api/nutanix/v3/tasks/", func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		status := m.taskStatus
		errDetail := m.taskError
		m.mu.Unlock()
		if status == "" {
			status = "SUCCEEDED"
		}
		out := prismTaskResp{Status: status, ErrorDetail: errDetail}
		out.ProgressInfo.PercentageComplete = 100
		writeJSON(w, http.StatusOK, out)
	})
	return mux
}

// TestAttachCDROM_MountsIntoExistingEmptyCDROM verifies the
// expected runtime path: VM has an empty CD-ROM device, AttachCDROM
// mutates its data_source_reference in place rather than appending
// a new device (AHV refuses IDE/SATA hot-add on powered-on VMs).
func TestAttachCDROM_MountsIntoExistingEmptyCDROM(t *testing.T) {
	existing := []VMDisk{
		{
			DeviceProperties: VMDeviceProperties{
				DeviceType: "CDROM",
				DiskAddress: VMDiskAddress{
					AdapterType: "IDE",
					DeviceIndex: 0,
				},
			},
			// No DataSourceReference — drive is empty.
		},
	}
	mock := &vmMock{t: t, vmUUID: "vm-1"}
	mock.vmBody = vmJSON(mock.vmUUID, existing, nil)
	srv := httptest.NewServer(mock.handler())
	defer srv.Close()
	c := newTestClient(t, srv)

	taskUUID, drive, reused, err := c.AttachCDROM(context.Background(), "vm-1", "image-42")
	if err != nil {
		t.Fatalf("attach: %v", err)
	}
	if reused {
		t.Errorf("expected reused=false on fresh mount")
	}
	if taskUUID == "" {
		t.Errorf("expected a task UUID, got empty")
	}
	if drive != "D:" {
		t.Errorf("expected drive hint D:, got %q", drive)
	}
	if mock.putHit != 1 {
		t.Errorf("expected exactly one PUT, got %d", mock.putHit)
	}

	// The PUT body's disk_list must still contain ONE device — the
	// same CD-ROM, now with our image in it. Verifying we didn't
	// add a new disk.
	var put map[string]json.RawMessage
	if err := json.Unmarshal(mock.putBody, &put); err != nil {
		t.Fatalf("decode PUT body: %v", err)
	}
	var spec map[string]json.RawMessage
	if err := json.Unmarshal(put["spec"], &spec); err != nil {
		t.Fatalf("decode spec: %v", err)
	}
	var resources map[string]json.RawMessage
	if err := json.Unmarshal(spec["resources"], &resources); err != nil {
		t.Fatalf("decode resources: %v", err)
	}
	var diskList []VMDisk
	if err := json.Unmarshal(resources["disk_list"], &diskList); err != nil {
		t.Fatalf("decode disk_list: %v", err)
	}
	if len(diskList) != 1 {
		t.Fatalf("expected ONE disk entry (mount, not add), got %d", len(diskList))
	}
	if diskList[0].DeviceProperties.DeviceType != "CDROM" {
		t.Errorf("device_type = %q, want CDROM", diskList[0].DeviceProperties.DeviceType)
	}
	// Adapter type stays whatever it was — we're mounting media,
	// not changing the device's bus.
	if diskList[0].DeviceProperties.DiskAddress.AdapterType != "IDE" {
		t.Errorf("adapter_type changed to %q; should preserve IDE",
			diskList[0].DeviceProperties.DiskAddress.AdapterType)
	}
	if diskList[0].DataSourceReference == nil {
		t.Fatalf("data_source_reference missing after mount")
	}
	if diskList[0].DataSourceReference.UUID != "image-42" {
		t.Errorf("data_source_reference.uuid = %q, want image-42",
			diskList[0].DataSourceReference.UUID)
	}
}

// TestAttachCDROM_FailsWhenVMHasNoCDROM surfaces the operator-
// actionable error when the VM has no CD-ROM device at all (AHV
// can't hot-add one). Operator must add a CD-ROM in Prism with
// the VM powered off, then retry.
func TestAttachCDROM_FailsWhenVMHasNoCDROM(t *testing.T) {
	// No CD-ROMs — only a DISK device.
	existing := []VMDisk{
		{
			DeviceProperties: VMDeviceProperties{
				DeviceType: "DISK",
				DiskAddress: VMDiskAddress{
					AdapterType: "SCSI",
					DeviceIndex: 0,
				},
			},
		},
	}
	mock := &vmMock{t: t, vmUUID: "vm-1"}
	mock.vmBody = vmJSON(mock.vmUUID, existing, nil)
	srv := httptest.NewServer(mock.handler())
	defer srv.Close()
	c := newTestClient(t, srv)

	_, _, _, err := c.AttachCDROM(context.Background(), "vm-1", "image-42")
	if err == nil {
		t.Fatalf("expected error when VM has no CD-ROM device")
	}
	if !strings.Contains(err.Error(), "no CD-ROM device") {
		t.Errorf("error should explain the missing CD-ROM; got %q", err.Error())
	}
	if mock.putHit != 0 {
		t.Errorf("no PUT should fire when validation rejects; got %d", mock.putHit)
	}
}

func TestAttachCDROM_DedupesOnAlreadyAttached(t *testing.T) {
	existing := []VMDisk{
		{
			DeviceProperties: VMDeviceProperties{
				DeviceType: "CDROM",
				DiskAddress: VMDiskAddress{
					AdapterType: "SATA",
					DeviceIndex: 0,
				},
			},
			DataSourceReference: &VMDataSourceRef{
				Kind: "image",
				UUID: "image-42",
			},
		},
	}
	mock := &vmMock{t: t, vmUUID: "vm-1"}
	mock.vmBody = vmJSON(mock.vmUUID, existing, nil)
	srv := httptest.NewServer(mock.handler())
	defer srv.Close()
	c := newTestClient(t, srv)

	taskUUID, drive, reused, err := c.AttachCDROM(context.Background(), "vm-1", "image-42")
	if err != nil {
		t.Fatalf("attach: %v", err)
	}
	if !reused {
		t.Errorf("expected reused=true when image already attached")
	}
	if taskUUID != "" {
		t.Errorf("expected no task UUID on no-op, got %q", taskUUID)
	}
	if drive != "D:" {
		t.Errorf("expected drive hint D:, got %q", drive)
	}
	if mock.putHit != 0 {
		t.Errorf("expected zero PUTs on dedupe, got %d", mock.putHit)
	}
}

// TestAttachCDROM_MutatesFirstCDROMOnly verifies that when a VM
// has multiple CD-ROM devices, AttachCDROM mutates only the FIRST
// one and leaves the rest unchanged. Disk count stays constant.
func TestAttachCDROM_MutatesFirstCDROMOnly(t *testing.T) {
	existing := []VMDisk{
		{
			DeviceProperties: VMDeviceProperties{
				DeviceType: "CDROM",
				DiskAddress: VMDiskAddress{
					AdapterType: "IDE",
					DeviceIndex: 0,
				},
			},
			DataSourceReference: &VMDataSourceRef{
				Kind: "image",
				UUID: "image-other-1",
			},
		},
		{
			DeviceProperties: VMDeviceProperties{
				DeviceType: "CDROM",
				DiskAddress: VMDiskAddress{
					AdapterType: "SATA",
					DeviceIndex: 1,
				},
			},
			DataSourceReference: &VMDataSourceRef{
				Kind: "image",
				UUID: "image-other-2",
			},
		},
	}
	mock := &vmMock{t: t, vmUUID: "vm-1"}
	mock.vmBody = vmJSON(mock.vmUUID, existing, nil)
	srv := httptest.NewServer(mock.handler())
	defer srv.Close()
	c := newTestClient(t, srv)

	_, drive, reused, err := c.AttachCDROM(context.Background(), "vm-1", "image-new")
	if err != nil {
		t.Fatalf("attach: %v", err)
	}
	if reused {
		t.Errorf("expected new mount, not reused")
	}
	if drive != "D:" {
		t.Errorf("expected drive hint D: (first CD-ROM), got %q", drive)
	}

	var put map[string]json.RawMessage
	_ = json.Unmarshal(mock.putBody, &put)
	var spec map[string]json.RawMessage
	_ = json.Unmarshal(put["spec"], &spec)
	var resources map[string]json.RawMessage
	_ = json.Unmarshal(spec["resources"], &resources)
	var diskList []VMDisk
	_ = json.Unmarshal(resources["disk_list"], &diskList)

	// Same number of disks as before — no add, no remove.
	if len(diskList) != 2 {
		t.Fatalf("expected 2 entries (no add), got %d", len(diskList))
	}
	// First CD-ROM now points at image-new.
	if diskList[0].DataSourceReference == nil ||
		diskList[0].DataSourceReference.UUID != "image-new" {
		t.Errorf("first CD-ROM should mount image-new; got %+v",
			diskList[0].DataSourceReference)
	}
	// Second CD-ROM untouched.
	if diskList[1].DataSourceReference == nil ||
		diskList[1].DataSourceReference.UUID != "image-other-2" {
		t.Errorf("second CD-ROM should be untouched; got %+v",
			diskList[1].DataSourceReference)
	}
}

func TestAttachCDROM_PreservesUnmodeledSpecFields(t *testing.T) {
	// The mock VM carries spec.foo and spec.resources.num_sockets,
	// neither of which our typed struct reads. After AttachCDROM
	// the PUT body must still carry both — that's the
	// schema-drift-safe invariant.
	extra := map[string]any{
		"foo":         "bar",
		"description": "extra fields the type doesn't model",
	}
	emptyCDROM := []VMDisk{{
		DeviceProperties: VMDeviceProperties{
			DeviceType:  "CDROM",
			DiskAddress: VMDiskAddress{AdapterType: "IDE", DeviceIndex: 0},
		},
	}}
	mock := &vmMock{t: t, vmUUID: "vm-1", extraSpecFields: extra}
	mock.vmBody = vmJSON(mock.vmUUID, emptyCDROM, extra)
	srv := httptest.NewServer(mock.handler())
	defer srv.Close()
	c := newTestClient(t, srv)

	_, _, _, err := c.AttachCDROM(context.Background(), "vm-1", "image-1")
	if err != nil {
		t.Fatalf("attach: %v", err)
	}

	// Decode PUT body and check spec.foo + resources.num_sockets.
	var put map[string]json.RawMessage
	if err := json.Unmarshal(mock.putBody, &put); err != nil {
		t.Fatalf("decode PUT body: %v", err)
	}
	var spec map[string]json.RawMessage
	if err := json.Unmarshal(put["spec"], &spec); err != nil {
		t.Fatalf("decode spec: %v", err)
	}
	if !strings.Contains(string(spec["foo"]), "bar") {
		t.Errorf("spec.foo lost in PUT body; got %s", string(spec["foo"]))
	}
	if !strings.Contains(string(spec["description"]), "extra fields") {
		t.Errorf("spec.description lost; got %s", string(spec["description"]))
	}
	var resources map[string]json.RawMessage
	_ = json.Unmarshal(spec["resources"], &resources)
	if !strings.Contains(string(resources["num_sockets"]), "2") {
		t.Errorf("resources.num_sockets lost; got %s", string(resources["num_sockets"]))
	}

	// Also confirm metadata round-tripped — Prism uses
	// spec_version for optimistic concurrency.
	var meta map[string]any
	if err := json.Unmarshal(put["metadata"], &meta); err != nil {
		t.Fatalf("decode metadata: %v", err)
	}
	if meta["spec_version"] == nil {
		t.Errorf("metadata.spec_version dropped: %+v", meta)
	}
}

func TestAttachCDROM_TaskFailedIsWrapped(t *testing.T) {
	emptyCDROM := []VMDisk{{
		DeviceProperties: VMDeviceProperties{
			DeviceType:  "CDROM",
			DiskAddress: VMDiskAddress{AdapterType: "IDE", DeviceIndex: 0},
		},
	}}
	mock := &vmMock{
		t:          t,
		vmUUID:     "vm-1",
		taskStatus: "FAILED",
		taskError:  "disk attach blew up",
	}
	mock.vmBody = vmJSON(mock.vmUUID, emptyCDROM, nil)
	srv := httptest.NewServer(mock.handler())
	defer srv.Close()
	c := newTestClient(t, srv)

	_, _, _, err := c.AttachCDROM(context.Background(), "vm-1", "image-1")
	if err == nil {
		t.Fatalf("expected error on task FAILED")
	}
	if !strings.Contains(err.Error(), "FAILED") {
		t.Errorf("expected FAILED in error chain, got %q", err.Error())
	}
	if !strings.Contains(err.Error(), "disk attach blew up") {
		t.Errorf("expected error_detail in chain, got %q", err.Error())
	}
}

// TestAttachCDROM_RetriesOnceOn409: a concurrent-edit 409 on the PUT (the
// field-observed CONCURRENT_REQUESTS_NOT_ALLOWED) triggers a fresh GET +
// one more PUT; a second 409 would surface as the error.
func TestAttachCDROM_RetriesOnceOn409(t *testing.T) {
	mock := &vmMock{t: t, vmUUID: "vm-1"}
	mock.vmBody = vmJSON(mock.vmUUID, []VMDisk{{
		DeviceProperties: VMDeviceProperties{
			DeviceType:  "CDROM",
			DiskAddress: VMDiskAddress{AdapterType: "IDE", DeviceIndex: 0},
		},
	}}, nil)
	mock.onPut = func(attempt int, _ []byte) (int, any) {
		if attempt == 1 {
			return http.StatusConflict, map[string]any{"code": 409}
		}
		out := prismCreateImageResp{}
		out.Metadata.UUID = mock.vmUUID
		out.Status.ExecutionContext.TaskUUID = "task-attach"
		return http.StatusAccepted, out
	}
	srv := httptest.NewServer(mock.handler())
	defer srv.Close()
	c := newTestClient(t, srv)

	_, _, _, err := c.AttachCDROM(context.Background(), "vm-1", "image-42")
	if err != nil {
		t.Fatalf("expected 409-then-success to succeed, got %v", err)
	}
	if mock.putHit != 2 || mock.getHit != 2 {
		t.Errorf("expected fresh GET + retry PUT (got %d GETs, %d PUTs)", mock.getHit, mock.putHit)
	}
}

func TestAttachCDROM_RejectsEmptyInputs(t *testing.T) {
	srv := httptest.NewServer(http.NewServeMux())
	defer srv.Close()
	c := newTestClient(t, srv)

	if _, _, _, err := c.AttachCDROM(context.Background(), "", "image-1"); err == nil {
		t.Errorf("expected error on empty vmUUID")
	}
	if _, _, _, err := c.AttachCDROM(context.Background(), "vm-1", ""); err == nil {
		t.Errorf("expected error on empty imageUUID")
	}
}

func TestAttachCDROM_VMGetUnauthorizedWraps(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/nutanix/v3/vms/vm-1", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"message":"go away"}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := newTestClient(t, srv)

	_, _, _, err := c.AttachCDROM(context.Background(), "vm-1", "image-1")
	if err == nil {
		t.Fatalf("expected error")
	}
	var ae *APIError
	if !errors.As(err, &ae) {
		t.Fatalf("expected APIError in chain, got %T: %v", err, err)
	}
	if ae.Status != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", ae.Status)
	}
}

// putDiskList decodes the disk_list from a captured PUT /vms body.
func putDiskList(t *testing.T, putBody []byte) []VMDisk {
	t.Helper()
	var put map[string]json.RawMessage
	if err := json.Unmarshal(putBody, &put); err != nil {
		t.Fatalf("decode PUT body: %v", err)
	}
	var spec map[string]json.RawMessage
	if err := json.Unmarshal(put["spec"], &spec); err != nil {
		t.Fatalf("decode spec: %v", err)
	}
	var resources map[string]json.RawMessage
	if err := json.Unmarshal(spec["resources"], &resources); err != nil {
		t.Fatalf("decode resources: %v", err)
	}
	var diskList []VMDisk
	if err := json.Unmarshal(resources["disk_list"], &diskList); err != nil {
		t.Fatalf("decode disk_list: %v", err)
	}
	return diskList
}

// TestDetachCDROM_ClearsFirstCDROMMedia verifies an eject clears the first
// CD-ROM's data_source_reference and reports ejected=true.
func TestDetachCDROM_ClearsFirstCDROMMedia(t *testing.T) {
	existing := []VMDisk{
		{
			DeviceProperties: VMDeviceProperties{
				DeviceType:  "CDROM",
				DiskAddress: VMDiskAddress{AdapterType: "SATA", DeviceIndex: 0},
			},
			DataSourceReference: &VMDataSourceRef{Kind: "image", UUID: "image-42"},
		},
	}
	mock := &vmMock{t: t, vmUUID: "vm-1"}
	mock.vmBody = vmJSON(mock.vmUUID, existing, nil)
	srv := httptest.NewServer(mock.handler())
	defer srv.Close()
	c := newTestClient(t, srv)

	taskUUID, ejected, err := c.DetachCDROM(context.Background(), "vm-1")
	if err != nil {
		t.Fatalf("detach: %v", err)
	}
	if !ejected {
		t.Errorf("expected ejected=true when media was present")
	}
	if taskUUID == "" {
		t.Errorf("expected a task UUID")
	}
	if mock.putHit != 1 {
		t.Fatalf("expected exactly one PUT, got %d", mock.putHit)
	}
	diskList := putDiskList(t, mock.putBody)
	if len(diskList) != 1 {
		t.Fatalf("expected ONE disk entry (eject, not remove), got %d", len(diskList))
	}
	if diskList[0].DeviceProperties.DeviceType != "CDROM" {
		t.Errorf("device_type = %q, want CDROM (device untouched)", diskList[0].DeviceProperties.DeviceType)
	}
	if diskList[0].DataSourceReference != nil {
		t.Errorf("data_source_reference should be cleared, got %+v", diskList[0].DataSourceReference)
	}
}

// TestDetachCDROM_NoopWhenAlreadyEmpty: an empty CD-ROM ejects to a no-op
// (no PUT, ejected=false) — idempotent.
func TestDetachCDROM_NoopWhenAlreadyEmpty(t *testing.T) {
	existing := []VMDisk{
		{
			DeviceProperties: VMDeviceProperties{
				DeviceType:  "CDROM",
				DiskAddress: VMDiskAddress{AdapterType: "IDE", DeviceIndex: 0},
			},
			// No DataSourceReference — drive already empty.
		},
	}
	mock := &vmMock{t: t, vmUUID: "vm-1"}
	mock.vmBody = vmJSON(mock.vmUUID, existing, nil)
	srv := httptest.NewServer(mock.handler())
	defer srv.Close()
	c := newTestClient(t, srv)

	taskUUID, ejected, err := c.DetachCDROM(context.Background(), "vm-1")
	if err != nil {
		t.Fatalf("detach: %v", err)
	}
	if ejected {
		t.Errorf("expected ejected=false on already-empty drive")
	}
	if taskUUID != "" {
		t.Errorf("expected no task UUID on no-op, got %q", taskUUID)
	}
	if mock.putHit != 0 {
		t.Errorf("expected zero PUTs on no-op, got %d", mock.putHit)
	}
}

// TestDetachCDROM_NeverTouchesDisk is the HARD INVARIANT guard: a VM with a
// DISK and a CD-ROM ejects ONLY the CD-ROM; the DISK entry must be byte-
// identical in the PUT body (nulling a disk's data_source_reference is
// data-loss).
func TestDetachCDROM_NeverTouchesDisk(t *testing.T) {
	disk := VMDisk{
		UUID: "disk-uuid-1",
		DeviceProperties: VMDeviceProperties{
			DeviceType:  "DISK",
			DiskAddress: VMDiskAddress{AdapterType: "SCSI", DeviceIndex: 0},
		},
		DataSourceReference: &VMDataSourceRef{Kind: "image", UUID: "golden-image"},
		DiskSizeBytes:       42949672960,
	}
	cdrom := VMDisk{
		DeviceProperties: VMDeviceProperties{
			DeviceType:  "CDROM",
			DiskAddress: VMDiskAddress{AdapterType: "SATA", DeviceIndex: 0},
		},
		DataSourceReference: &VMDataSourceRef{Kind: "image", UUID: "mounted-iso"},
	}
	mock := &vmMock{t: t, vmUUID: "vm-1"}
	mock.vmBody = vmJSON(mock.vmUUID, []VMDisk{disk, cdrom}, nil)
	srv := httptest.NewServer(mock.handler())
	defer srv.Close()
	c := newTestClient(t, srv)

	_, ejected, err := c.DetachCDROM(context.Background(), "vm-1")
	if err != nil {
		t.Fatalf("detach: %v", err)
	}
	if !ejected {
		t.Errorf("expected ejected=true (CD-ROM had media)")
	}
	diskList := putDiskList(t, mock.putBody)
	if len(diskList) != 2 {
		t.Fatalf("expected 2 entries (eject, not remove), got %d", len(diskList))
	}
	// The DISK entry must be byte-identical to what we sent in.
	gotDisk, err := json.Marshal(diskList[0])
	if err != nil {
		t.Fatalf("marshal got disk: %v", err)
	}
	wantDisk, err := json.Marshal(disk)
	if err != nil {
		t.Fatalf("marshal want disk: %v", err)
	}
	if string(gotDisk) != string(wantDisk) {
		t.Errorf("DISK entry was modified!\n got: %s\nwant: %s", gotDisk, wantDisk)
	}
	// The CD-ROM's media must be cleared.
	if diskList[1].DataSourceReference != nil {
		t.Errorf("CD-ROM media should be cleared, got %+v", diskList[1].DataSourceReference)
	}
}

// TestDetachCDROM_NoopWhenNoCDROM: a VM with only a DISK has nothing to eject —
// no PUT, ejected=false (never an error, never a disk touch).
func TestDetachCDROM_NoopWhenNoCDROM(t *testing.T) {
	existing := []VMDisk{
		{
			DeviceProperties: VMDeviceProperties{
				DeviceType:  "DISK",
				DiskAddress: VMDiskAddress{AdapterType: "SCSI", DeviceIndex: 0},
			},
			DataSourceReference: &VMDataSourceRef{Kind: "image", UUID: "golden"},
		},
	}
	mock := &vmMock{t: t, vmUUID: "vm-1"}
	mock.vmBody = vmJSON(mock.vmUUID, existing, nil)
	srv := httptest.NewServer(mock.handler())
	defer srv.Close()
	c := newTestClient(t, srv)

	_, ejected, err := c.DetachCDROM(context.Background(), "vm-1")
	if err != nil {
		t.Fatalf("detach (no CD-ROM) should be a no-op, got err: %v", err)
	}
	if ejected {
		t.Errorf("expected ejected=false when VM has no CD-ROM")
	}
	if mock.putHit != 0 {
		t.Errorf("expected zero PUTs when there is no CD-ROM, got %d", mock.putHit)
	}
}

// TestDetachCDROM_PreservesUnmodeledSpecFields confirms the schema-drift-safe
// splice survives an eject — fields outside disk_list round-trip untouched.
func TestDetachCDROM_PreservesUnmodeledSpecFields(t *testing.T) {
	existing := []VMDisk{{
		DeviceProperties: VMDeviceProperties{
			DeviceType:  "CDROM",
			DiskAddress: VMDiskAddress{AdapterType: "SATA", DeviceIndex: 0},
		},
		DataSourceReference: &VMDataSourceRef{Kind: "image", UUID: "image-42"},
	}}
	// vmJSON folds extraSpecFields at spec top-level and always sets
	// resources.num_sockets — neither is modeled by our typed struct, so both
	// must survive the eject's splice (the schema-drift-safe invariant).
	extra := map[string]any{
		"foo":         "bar",
		"description": "extra fields the type doesn't model",
	}
	mock := &vmMock{t: t, vmUUID: "vm-1"}
	mock.vmBody = vmJSON(mock.vmUUID, existing, extra)
	srv := httptest.NewServer(mock.handler())
	defer srv.Close()
	c := newTestClient(t, srv)

	if _, _, err := c.DetachCDROM(context.Background(), "vm-1"); err != nil {
		t.Fatalf("detach: %v", err)
	}
	var put map[string]json.RawMessage
	_ = json.Unmarshal(mock.putBody, &put)
	var spec map[string]json.RawMessage
	_ = json.Unmarshal(put["spec"], &spec)
	if !strings.Contains(string(spec["foo"]), "bar") {
		t.Errorf("spec.foo lost on eject; got %s", string(spec["foo"]))
	}
	var resources map[string]json.RawMessage
	_ = json.Unmarshal(spec["resources"], &resources)
	if !strings.Contains(string(resources["num_sockets"]), "2") {
		t.Errorf("resources.num_sockets lost on eject; got %s", string(resources["num_sockets"]))
	}
	// metadata.spec_version must round-trip (optimistic concurrency).
	var meta map[string]any
	_ = json.Unmarshal(put["metadata"], &meta)
	if meta["spec_version"] == nil {
		t.Errorf("metadata.spec_version dropped on eject: %+v", meta)
	}
}

// TestDetachCDROM_RetriesOnceOn409 mirrors AttachCDROM's one-shot 409 retry.
func TestDetachCDROM_RetriesOnceOn409(t *testing.T) {
	mock := &vmMock{t: t, vmUUID: "vm-1"}
	mock.vmBody = vmJSON(mock.vmUUID, []VMDisk{{
		DeviceProperties: VMDeviceProperties{
			DeviceType:  "CDROM",
			DiskAddress: VMDiskAddress{AdapterType: "IDE", DeviceIndex: 0},
		},
		DataSourceReference: &VMDataSourceRef{Kind: "image", UUID: "image-42"},
	}}, nil)
	mock.onPut = func(attempt int, _ []byte) (int, any) {
		if attempt == 1 {
			return http.StatusConflict, map[string]any{"code": 409}
		}
		out := prismCreateImageResp{}
		out.Metadata.UUID = mock.vmUUID
		out.Status.ExecutionContext.TaskUUID = "task-detach"
		return http.StatusAccepted, out
	}
	srv := httptest.NewServer(mock.handler())
	defer srv.Close()
	c := newTestClient(t, srv)

	_, ejected, err := c.DetachCDROM(context.Background(), "vm-1")
	if err != nil {
		t.Fatalf("expected 409-then-success to succeed, got %v", err)
	}
	if !ejected {
		t.Errorf("expected ejected=true after retry")
	}
	if mock.putHit != 2 || mock.getHit != 2 {
		t.Errorf("expected fresh GET + retry PUT (got %d GETs, %d PUTs)", mock.getHit, mock.putHit)
	}
}

func TestDetachCDROM_RejectsEmptyUUID(t *testing.T) {
	srv := httptest.NewServer(http.NewServeMux())
	defer srv.Close()
	c := newTestClient(t, srv)
	if _, _, err := c.DetachCDROM(context.Background(), ""); err == nil {
		t.Errorf("expected error on empty vmUUID")
	}
}

func TestDriveLetterForCDROM(t *testing.T) {
	cases := []struct {
		existing int
		want     string
	}{
		{0, "D:"},
		{1, "E:"},
		{2, "F:"},
		{22, "Z:"},
		{23, ""}, // out of letters
	}
	for _, tc := range cases {
		got := driveLetterForCDROM(tc.existing)
		if got != tc.want {
			t.Errorf("driveLetterForCDROM(%d) = %q, want %q", tc.existing, got, tc.want)
		}
	}
}

func TestGetVMSpec_ParsesAndPreservesRaw(t *testing.T) {
	disks := []VMDisk{
		{
			DeviceProperties: VMDeviceProperties{
				DeviceType: "DISK",
				DiskAddress: VMDiskAddress{
					AdapterType: "SCSI",
					DeviceIndex: 0,
				},
			},
			DiskSizeBytes: 1024 * 1024 * 1024 * 32,
		},
	}
	mock := &vmMock{t: t, vmUUID: "vm-2"}
	mock.vmBody = vmJSON(mock.vmUUID, disks, map[string]any{"extra": "round-tripped"})
	srv := httptest.NewServer(mock.handler())
	defer srv.Close()
	c := newTestClient(t, srv)

	state, err := c.GetVMSpec(context.Background(), "vm-2")
	if err != nil {
		t.Fatalf("get vm spec: %v", err)
	}
	if state.Spec.Name != "test-vm" {
		t.Errorf("Spec.Name = %q, want test-vm", state.Spec.Name)
	}
	if len(state.Spec.Resources.DiskList) != 1 {
		t.Fatalf("expected one disk, got %d", len(state.Spec.Resources.DiskList))
	}
	if state.Spec.Resources.DiskList[0].DeviceProperties.DeviceType != "DISK" {
		t.Errorf("device_type wrong: %s",
			state.Spec.Resources.DiskList[0].DeviceProperties.DeviceType)
	}
	// rawSpec should be populated and contain the "extra" field
	// that our typed Spec doesn't model.
	if !strings.Contains(string(state.rawSpec), "round-tripped") {
		t.Errorf("rawSpec did not preserve extra field; got %s",
			string(state.rawSpec))
	}
}

func TestGetVMSpec_RejectsEmptyUUID(t *testing.T) {
	c, _ := NewClient(ClientOptions{BaseURL: "https://example.invalid"})
	_, err := c.GetVMSpec(context.Background(), "")
	if err == nil {
		t.Errorf("expected error on empty vmUUID")
	}
}
