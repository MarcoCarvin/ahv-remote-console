package api

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"
)

// TestSetPowerStateV3_OffUsesACPIMechanism pins the v3 graceful-shutdown wire
// shape: the spec round-trip PUT carries power_state=OFF AND
// power_state_mechanism.mechanism=ACPI (NEVER a hard off), preserves
// metadata.spec_version, and drops status.
func TestSetPowerStateV3_OffUsesACPIMechanism(t *testing.T) {
	mock := &vmMock{t: t, vmUUID: "vm-1"}
	mock.vmBody = vmMountJSON("vm-1", "ON", emptyCDROMDisks(), nil, 11, nil)
	srv := httptest.NewServer(mock.handler())
	defer srv.Close()
	c := newTestClient(t, srv)

	if err := c.SetPowerStateV3(context.Background(), "vm-1", "OFF"); err != nil {
		t.Fatalf("set power state: %v", err)
	}
	if mock.putHit != 1 {
		t.Fatalf("expected exactly one PUT, got %d", mock.putHit)
	}
	meta, _, resources, hasStatus := decodePutSpec(t, mock.putBody)
	if meta == nil || meta["spec_version"] != float64(11) {
		t.Errorf("PUT must round-trip metadata.spec_version: %s", mock.putBody)
	}
	if hasStatus {
		t.Errorf("PUT body must not include status: %s", mock.putBody)
	}
	if resources["power_state"] != "OFF" {
		t.Errorf("power_state = %v, want OFF", resources["power_state"])
	}
	mech, _ := resources["power_state_mechanism"].(map[string]any)
	if mech == nil || mech["mechanism"] != "ACPI" {
		t.Errorf("power_state_mechanism = %v, want {mechanism: ACPI}", resources["power_state_mechanism"])
	}
}

// TestSetPowerStateV3_OnDoesNotForceMechanism: power ON is a plain
// power_state flip; no mechanism is injected.
func TestSetPowerStateV3_OnDoesNotForceMechanism(t *testing.T) {
	mock := &vmMock{t: t, vmUUID: "vm-1"}
	mock.vmBody = vmMountJSON("vm-1", "OFF", emptyCDROMDisks(), nil, 11, nil)
	srv := httptest.NewServer(mock.handler())
	defer srv.Close()
	c := newTestClient(t, srv)

	if err := c.SetPowerStateV3(context.Background(), "vm-1", "ON"); err != nil {
		t.Fatalf("set power state: %v", err)
	}
	_, _, resources, _ := decodePutSpec(t, mock.putBody)
	if resources["power_state"] != "ON" {
		t.Errorf("power_state = %v, want ON", resources["power_state"])
	}
	if _, ok := resources["power_state_mechanism"]; ok {
		t.Errorf("power ON must not inject a mechanism: %s", mock.putBody)
	}
}

// TestSetPowerStateV3_RejectsUnknownState: only ON/OFF are valid v3 spec
// power states; anything else is refused before any request is sent.
func TestSetPowerStateV3_RejectsUnknownState(t *testing.T) {
	mock := &vmMock{t: t, vmUUID: "vm-1"}
	mock.vmBody = vmMountJSON("vm-1", "ON", emptyCDROMDisks(), nil, 11, nil)
	srv := httptest.NewServer(mock.handler())
	defer srv.Close()
	c := newTestClient(t, srv)

	if err := c.SetPowerStateV3(context.Background(), "vm-1", "RESET"); err == nil {
		t.Fatal("expected error for non-ON/OFF state")
	}
	if mock.getHit != 0 || mock.putHit != 0 {
		t.Errorf("no request must be sent for an invalid state (get=%d put=%d)", mock.getHit, mock.putHit)
	}
}

// TestWaitForPowerState_ReachedAndTimeout covers both arms of the poll.
func TestWaitForPowerState_ReachedAndTimeout(t *testing.T) {
	mock := &vmMock{t: t, vmUUID: "vm-1"}
	mock.vmBody = vmMountJSON("vm-1", "OFF", emptyCDROMDisks(), nil, 11, nil)
	srv := httptest.NewServer(mock.handler())
	defer srv.Close()
	c := newTestClient(t, srv)

	if err := c.WaitForPowerState(context.Background(), "vm-1", "OFF", 100*time.Millisecond, 5*time.Millisecond); err != nil {
		t.Fatalf("expected OFF to be observed immediately, got %v", err)
	}
	if err := c.WaitForPowerState(context.Background(), "vm-1", "ON", 30*time.Millisecond, 5*time.Millisecond); err == nil {
		t.Fatal("expected timeout waiting for ON on an OFF VM")
	}
}
