package api

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

// vmSerialJSON builds a VM payload with an explicit power_state and a
// (possibly empty) serial_port_list. EnsureSerialPort reads
// status.resources.power_state and spec.resources.serial_port_list.
func vmSerialJSON(uuid, powerState string, serialIndexes []int) []byte {
	ports := make([]map[string]any, 0, len(serialIndexes))
	for _, idx := range serialIndexes {
		ports = append(ports, map[string]any{"index": idx, "is_connected": true})
	}
	out := map[string]any{
		"metadata": map[string]any{
			"kind":         "vm",
			"uuid":         uuid,
			"spec_version": 11,
		},
		"spec": map[string]any{
			"name": "test-vm",
			"resources": map[string]any{
				"serial_port_list": ports,
				"num_sockets":      2,
			},
		},
		"status": map[string]any{
			"resources": map[string]any{
				"power_state": powerState,
			},
		},
	}
	b, _ := json.Marshal(out)
	return b
}

func newSerialMock(t *testing.T, uuid, powerState string, indexes []int, taskStatus string) *vmMock {
	return &vmMock{
		t:          t,
		vmUUID:     uuid,
		vmBody:     vmSerialJSON(uuid, powerState, indexes),
		taskStatus: taskStatus,
	}
}

// TestEnsureSerialPort_AlreadyPresent: a port at the requested index already
// exists — no PUT, no power-cycle prompt.
func TestEnsureSerialPort_AlreadyPresent(t *testing.T) {
	mock := newSerialMock(t, "vm-1", "ON", []int{2}, "SUCCEEDED")
	srv := httptest.NewServer(mock.handler())
	defer srv.Close()
	c := newTestClient(t, srv)

	res, err := c.EnsureSerialPort(context.Background(), "vm-1", 2)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !res.AlreadyPresent || res.Added {
		t.Errorf("expected AlreadyPresent only, got %+v", res)
	}
	if mock.putHit != 0 {
		t.Errorf("expected no PUT when port already present, got %d", mock.putHit)
	}
}

// TestEnsureSerialPort_AddedWhenOff: powered-off VM, task SUCCEEDED — the port
// is added, no power cycle needed, and the PUT body carries the appended port
// while preserving metadata.spec_version and dropping status.
func TestEnsureSerialPort_AddedWhenOff(t *testing.T) {
	mock := newSerialMock(t, "vm-1", "OFF", nil, "SUCCEEDED")
	srv := httptest.NewServer(mock.handler())
	defer srv.Close()
	c := newTestClient(t, srv)

	res, err := c.EnsureSerialPort(context.Background(), "vm-1", 2)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !res.Added || res.PowerCycleRequired {
		t.Errorf("expected Added without power cycle on OFF VM, got %+v", res)
	}
	if mock.putHit != 1 {
		t.Fatalf("expected exactly one PUT, got %d", mock.putHit)
	}

	var put map[string]any
	if err := json.Unmarshal(mock.putBody, &put); err != nil {
		t.Fatalf("put body parse: %v", err)
	}
	meta, _ := put["metadata"].(map[string]any)
	if meta == nil || meta["spec_version"] == nil {
		t.Errorf("PUT dropped metadata.spec_version (would 409): %s", mock.putBody)
	}
	if _, ok := put["status"]; ok {
		t.Errorf("PUT body must not include status: %s", mock.putBody)
	}
	spec, _ := put["spec"].(map[string]any)
	resources, _ := spec["resources"].(map[string]any)
	list, _ := resources["serial_port_list"].([]any)
	found := false
	for _, e := range list {
		m, _ := e.(map[string]any)
		if m != nil && m["index"] == float64(2) && m["is_connected"] == true {
			found = true
		}
	}
	if !found {
		t.Errorf("PUT body missing serial_port_list[index=2,is_connected=true]: %s", mock.putBody)
	}
}

// TestEnsureSerialPort_PowerCycleWhenOnTaskFails: VM is ON and AHV rejects the
// hot-add (task FAILED). We must NOT report a hard error — we report
// PowerCycleRequired so the operator gets actionable "power the VM off/on"
// guidance. This is the sysvpn10 case.
func TestEnsureSerialPort_PowerCycleWhenOnTaskFails(t *testing.T) {
	mock := newSerialMock(t, "vm-1", "ON", nil, "FAILED")
	srv := httptest.NewServer(mock.handler())
	defer srv.Close()
	c := newTestClient(t, srv)

	res, err := c.EnsureSerialPort(context.Background(), "vm-1", 2)
	if err != nil {
		t.Fatalf("expected no hard error on powered-on VM, got %v", err)
	}
	if !res.PowerCycleRequired || res.Added {
		t.Errorf("expected PowerCycleRequired without Added, got %+v", res)
	}
}

// TestEnsureSerialPort_PowerCycleWhenOnTaskSucceeds: builds that accept the
// spec edit on a running VM still require a power cycle for the device to
// enumerate.
func TestEnsureSerialPort_PowerCycleWhenOnTaskSucceeds(t *testing.T) {
	mock := newSerialMock(t, "vm-1", "ON", nil, "SUCCEEDED")
	srv := httptest.NewServer(mock.handler())
	defer srv.Close()
	c := newTestClient(t, srv)

	res, err := c.EnsureSerialPort(context.Background(), "vm-1", 2)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !res.Added || !res.PowerCycleRequired {
		t.Errorf("expected Added + PowerCycleRequired on ON VM, got %+v", res)
	}
}

// TestEnsureSerialPort_ErrorWhenOffTaskFails: a genuine failure on a
// powered-off VM (schema/permission) must surface as an error — never a silent
// success (the original fire-and-forget bug).
func TestEnsureSerialPort_ErrorWhenOffTaskFails(t *testing.T) {
	mock := newSerialMock(t, "vm-1", "OFF", nil, "FAILED")
	srv := httptest.NewServer(mock.handler())
	defer srv.Close()
	c := newTestClient(t, srv)

	if _, err := c.EnsureSerialPort(context.Background(), "vm-1", 2); err == nil {
		t.Fatal("expected error when task FAILED on powered-off VM")
	} else if !strings.Contains(err.Error(), "task failed") && !strings.Contains(err.Error(), "FAILED") {
		t.Errorf("expected task-failure error, got %q", err.Error())
	}
}
