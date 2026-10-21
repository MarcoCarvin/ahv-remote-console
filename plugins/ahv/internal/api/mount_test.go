package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// vmMountJSON builds a VM payload carrying BOTH a disk_list and a
// serial_port_list, plus unmodeled spec fields — the shape the merged
// AttachCDROMAndSerialPort round-trip must preserve.
func vmMountJSON(uuid, powerState string, disks []VMDisk, serialIndexes []int, specVersion int, extraSpecFields map[string]any) []byte {
	ports := make([]map[string]any, 0, len(serialIndexes))
	for _, idx := range serialIndexes {
		ports = append(ports, map[string]any{"index": idx, "is_connected": true})
	}
	spec := map[string]any{
		"name": "test-vm",
		"resources": map[string]any{
			"disk_list":        disks,
			"serial_port_list": ports,
			"num_sockets":      2,
		},
	}
	for k, v := range extraSpecFields {
		spec[k] = v
	}
	out := map[string]any{
		"metadata": map[string]any{
			"kind":         "vm",
			"uuid":         uuid,
			"spec_version": specVersion,
		},
		"spec": spec,
		"status": map[string]any{
			"resources": map[string]any{
				"power_state": powerState,
			},
		},
	}
	b, _ := json.Marshal(out)
	return b
}

func emptyCDROMDisks() []VMDisk {
	return []VMDisk{{
		DeviceProperties: VMDeviceProperties{
			DeviceType:  "CDROM",
			DiskAddress: VMDiskAddress{AdapterType: "IDE", DeviceIndex: 0},
		},
	}}
}

// decodePutSpec pulls (metadata, spec.resources) maps out of a captured PUT
// body for assertion.
func decodePutSpec(t *testing.T, body []byte) (meta, spec, resources map[string]any, hasStatus bool) {
	t.Helper()
	var put map[string]any
	if err := json.Unmarshal(body, &put); err != nil {
		t.Fatalf("decode PUT body: %v", err)
	}
	meta, _ = put["metadata"].(map[string]any)
	spec, _ = put["spec"].(map[string]any)
	if spec != nil {
		resources, _ = spec["resources"].(map[string]any)
	}
	_, hasStatus = put["status"]
	return meta, spec, resources, hasStatus
}

func serialIndexesOf(resources map[string]any) []int {
	list, _ := resources["serial_port_list"].([]any)
	out := make([]int, 0, len(list))
	for _, e := range list {
		m, _ := e.(map[string]any)
		if m == nil {
			continue
		}
		if f, ok := m["index"].(float64); ok {
			out = append(out, int(f))
		}
	}
	return out
}

func firstCDROMImage(t *testing.T, resources map[string]any) string {
	t.Helper()
	list, _ := resources["disk_list"].([]any)
	for _, e := range list {
		m, _ := e.(map[string]any)
		if m == nil {
			continue
		}
		dp, _ := m["device_properties"].(map[string]any)
		if dp == nil || dp["device_type"] != "CDROM" {
			continue
		}
		dsr, _ := m["data_source_reference"].(map[string]any)
		if dsr == nil {
			return ""
		}
		u, _ := dsr["uuid"].(string)
		return u
	}
	return ""
}

// TestAttachCDROMAndSerialPort_MergedPUTCarriesBothEdits: ONE GET, ONE PUT
// carrying BOTH the CD-ROM media change and the appended serial port, while
// preserving metadata.spec_version and unmodeled spec fields, and dropping
// status — the same round-trip contract the single-purpose ops pin.
func TestAttachCDROMAndSerialPort_MergedPUTCarriesBothEdits(t *testing.T) {
	extra := map[string]any{"foo": "bar"}
	mock := &vmMock{t: t, vmUUID: "vm-1"}
	mock.vmBody = vmMountJSON("vm-1", "OFF", emptyCDROMDisks(), nil, 11, extra)
	srv := httptest.NewServer(mock.handler())
	defer srv.Close()
	c := newTestClient(t, srv)

	res, err := c.AttachCDROMAndSerialPort(context.Background(), "vm-1", "image-42", 2)
	if err != nil {
		t.Fatalf("merged attach: %v", err)
	}
	if !res.SerialAdded || res.SerialAlreadyPresent || res.AttachReused {
		t.Errorf("expected SerialAdded on fresh merged update, got %+v", res)
	}
	if res.DriveLetterHint != "D:" {
		t.Errorf("drive hint = %q, want D:", res.DriveLetterHint)
	}
	if !strings.EqualFold(res.PowerState, "OFF") {
		t.Errorf("power state = %q, want OFF", res.PowerState)
	}
	if mock.putHit != 1 {
		t.Fatalf("expected exactly ONE PUT (merged), got %d", mock.putHit)
	}
	if mock.getHit != 1 {
		t.Errorf("expected exactly ONE GET (merged), got %d", mock.getHit)
	}

	meta, spec, resources, hasStatus := decodePutSpec(t, mock.putBody)
	if meta == nil || meta["spec_version"] != float64(11) {
		t.Errorf("PUT must round-trip metadata.spec_version: %s", mock.putBody)
	}
	if hasStatus {
		t.Errorf("PUT body must not include status: %s", mock.putBody)
	}
	if !strings.Contains(string(mock.putBody), `"foo":"bar"`) {
		t.Errorf("unmodeled spec field lost in PUT: %s", mock.putBody)
	}
	if ns, ok := resources["num_sockets"].(float64); !ok || ns != 2 {
		t.Errorf("resources.num_sockets lost: %v", resources["num_sockets"])
	}
	if got := serialIndexesOf(resources); len(got) != 1 || got[0] != 2 {
		t.Errorf("PUT serial_port_list = %v, want [2]", got)
	}
	if got := firstCDROMImage(t, resources); got != "image-42" {
		t.Errorf("first CD-ROM data_source_reference = %q, want image-42", got)
	}
	_ = spec
}

// TestAttachCDROMAndSerialPort_OnlyCDROMWhenSerialPresent: a port at the
// requested index already exists, so the single PUT mutates ONLY the CD-ROM —
// the serial_port_list rides through unchanged.
func TestAttachCDROMAndSerialPort_OnlyCDROMWhenSerialPresent(t *testing.T) {
	mock := &vmMock{t: t, vmUUID: "vm-1"}
	mock.vmBody = vmMountJSON("vm-1", "OFF", emptyCDROMDisks(), []int{2}, 11, nil)
	srv := httptest.NewServer(mock.handler())
	defer srv.Close()
	c := newTestClient(t, srv)

	res, err := c.AttachCDROMAndSerialPort(context.Background(), "vm-1", "image-42", 2)
	if err != nil {
		t.Fatalf("merged attach: %v", err)
	}
	if res.SerialAdded || !res.SerialAlreadyPresent {
		t.Errorf("expected SerialAlreadyPresent without SerialAdded, got %+v", res)
	}
	if mock.putHit != 1 {
		t.Fatalf("expected exactly one PUT, got %d", mock.putHit)
	}
	_, _, resources, _ := decodePutSpec(t, mock.putBody)
	if got := serialIndexesOf(resources); len(got) != 1 || got[0] != 2 {
		t.Errorf("serial_port_list must be unchanged, got %v", got)
	}
	if got := firstCDROMImage(t, resources); got != "image-42" {
		t.Errorf("first CD-ROM data_source_reference = %q, want image-42", got)
	}
}

// TestAttachCDROMAndSerialPort_NoPUTWhenNothingToDo: image already in the
// drive AND serial already present — fully idempotent, zero PUTs.
func TestAttachCDROMAndSerialPort_NoPUTWhenNothingToDo(t *testing.T) {
	disks := emptyCDROMDisks()
	disks[0].DataSourceReference = &VMDataSourceRef{Kind: "image", UUID: "image-42"}
	mock := &vmMock{t: t, vmUUID: "vm-1"}
	mock.vmBody = vmMountJSON("vm-1", "OFF", disks, []int{2}, 11, nil)
	srv := httptest.NewServer(mock.handler())
	defer srv.Close()
	c := newTestClient(t, srv)

	res, err := c.AttachCDROMAndSerialPort(context.Background(), "vm-1", "image-42", 2)
	if err != nil {
		t.Fatalf("merged attach: %v", err)
	}
	if !res.AttachReused || !res.SerialAlreadyPresent || res.SerialAdded {
		t.Errorf("expected full no-op result, got %+v", res)
	}
	if mock.putHit != 0 {
		t.Errorf("expected zero PUTs on no-op, got %d", mock.putHit)
	}
}

// TestAttachCDROMAndSerialPort_NoCDROMDeviceErrors mirrors AttachCDROM's
// operator-actionable error when the VM has no CD-ROM device at all.
func TestAttachCDROMAndSerialPort_NoCDROMDeviceErrors(t *testing.T) {
	disks := []VMDisk{{
		DeviceProperties: VMDeviceProperties{
			DeviceType:  "DISK",
			DiskAddress: VMDiskAddress{AdapterType: "SCSI", DeviceIndex: 0},
		},
	}}
	mock := &vmMock{t: t, vmUUID: "vm-1"}
	mock.vmBody = vmMountJSON("vm-1", "OFF", disks, nil, 11, nil)
	srv := httptest.NewServer(mock.handler())
	defer srv.Close()
	c := newTestClient(t, srv)

	_, err := c.AttachCDROMAndSerialPort(context.Background(), "vm-1", "image-42", 2)
	if err == nil {
		t.Fatal("expected error when VM has no CD-ROM device")
	}
	if !strings.Contains(err.Error(), "no CD-ROM device") {
		t.Errorf("error should explain the missing CD-ROM; got %q", err.Error())
	}
	if mock.putHit != 0 {
		t.Errorf("no PUT should fire when validation rejects; got %d", mock.putHit)
	}
}

// TestAttachCDROMAndSerialPort_RetriesOnceOn409: the first PUT is rejected
// with HTTP 409 (Prism CONCURRENT_REQUESTS_NOT_ALLOWED); the op must do a
// FRESH GET, re-apply both mutations against the new document (bumped
// spec_version), and PUT exactly once more.
func TestAttachCDROMAndSerialPort_RetriesOnceOn409(t *testing.T) {
	mock := &vmMock{t: t, vmUUID: "vm-1"}
	mock.vmBody = vmMountJSON("vm-1", "OFF", emptyCDROMDisks(), nil, 11, nil)
	mock.onPut = func(attempt int, _ []byte) (int, any) {
		if attempt == 1 {
			// Simulate the concurrent edit the 409 signals: the next GET
			// must observe a NEW spec_version.
			mock.mu.Lock()
			mock.vmBody = vmMountJSON("vm-1", "OFF", emptyCDROMDisks(), nil, 12, nil)
			mock.mu.Unlock()
			return http.StatusConflict, map[string]any{"code": 409, "message_list": []map[string]any{{"message": "CONCURRENT_REQUESTS_NOT_ALLOWED"}}}
		}
		out := prismCreateImageResp{}
		out.Metadata.UUID = "vm-1"
		out.Status.ExecutionContext.TaskUUID = "task-attach"
		return http.StatusAccepted, out
	}
	srv := httptest.NewServer(mock.handler())
	defer srv.Close()
	c := newTestClient(t, srv)

	res, err := c.AttachCDROMAndSerialPort(context.Background(), "vm-1", "image-42", 2)
	if err != nil {
		t.Fatalf("expected 409-then-success to succeed, got %v", err)
	}
	if !res.SerialAdded {
		t.Errorf("expected SerialAdded after retry, got %+v", res)
	}
	if mock.putHit != 2 {
		t.Fatalf("expected exactly TWO PUTs (409 then retry), got %d", mock.putHit)
	}
	if mock.getHit != 2 {
		t.Fatalf("expected a FRESH GET before the retry PUT, got %d GETs", mock.getHit)
	}
	meta, _, resources, _ := decodePutSpec(t, mock.putBody)
	if meta == nil || meta["spec_version"] != float64(12) {
		t.Errorf("retry PUT must carry the re-fetched spec_version 12: %s", mock.putBody)
	}
	if got := serialIndexesOf(resources); len(got) != 1 || got[0] != 2 {
		t.Errorf("retry PUT lost the serial mutation: %v", got)
	}
	if got := firstCDROMImage(t, resources); got != "image-42" {
		t.Errorf("retry PUT lost the CD-ROM mutation: %q", got)
	}
}

// TestAttachCDROMAndSerialPort_409TwiceIsError: the retry is one-shot — a
// second consecutive 409 surfaces as an error.
func TestAttachCDROMAndSerialPort_409TwiceIsError(t *testing.T) {
	mock := &vmMock{t: t, vmUUID: "vm-1"}
	mock.vmBody = vmMountJSON("vm-1", "OFF", emptyCDROMDisks(), nil, 11, nil)
	mock.onPut = func(int, []byte) (int, any) {
		return http.StatusConflict, map[string]any{"code": 409}
	}
	srv := httptest.NewServer(mock.handler())
	defer srv.Close()
	c := newTestClient(t, srv)

	_, err := c.AttachCDROMAndSerialPort(context.Background(), "vm-1", "image-42", 2)
	if err == nil {
		t.Fatal("expected error when the retry also 409s")
	}
	if HTTPStatus(err) != http.StatusConflict {
		t.Errorf("expected the 409 APIError in the chain, got %v", err)
	}
	if mock.putHit != 2 {
		t.Errorf("expected exactly TWO PUTs (no further retries), got %d", mock.putHit)
	}
}

// TestSerialPortState reads presence + power without mutating anything.
func TestSerialPortState(t *testing.T) {
	mock := &vmMock{t: t, vmUUID: "vm-1"}
	mock.vmBody = vmMountJSON("vm-1", "ON", emptyCDROMDisks(), []int{2}, 11, nil)
	srv := httptest.NewServer(mock.handler())
	defer srv.Close()
	c := newTestClient(t, srv)

	present, power, err := c.SerialPortState(context.Background(), "vm-1", 2)
	if err != nil {
		t.Fatalf("serial port state: %v", err)
	}
	if !present || power != "ON" {
		t.Errorf("got present=%v power=%q, want present=true power=ON", present, power)
	}
	if mock.putHit != 0 {
		t.Errorf("SerialPortState must never PUT, got %d", mock.putHit)
	}

	mock.mu.Lock()
	mock.vmBody = vmMountJSON("vm-1", "OFF", emptyCDROMDisks(), nil, 11, nil)
	mock.mu.Unlock()
	present, power, err = c.SerialPortState(context.Background(), "vm-1", 2)
	if err != nil {
		t.Fatalf("serial port state: %v", err)
	}
	if present || power != "OFF" {
		t.Errorf("got present=%v power=%q, want present=false power=OFF", present, power)
	}
}
