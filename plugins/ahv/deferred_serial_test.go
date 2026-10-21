package ahv

import (
	"context"
	"testing"
)

// TestAddSerialPort_ArmsDeferredOnPowerCycleRequired: the standalone button
// path on a running VM ends PowerCycleRequired — keeps its existing note AND
// arms the deferred apply.
func TestAddSerialPort_ArmsDeferredOnPowerCycleRequired(t *testing.T) {
	iso := []byte("FAKEISO")
	f := newPrismFake(t, "ON", nil, iso)
	p := newCyclePlugin(t, f, iso)

	res, err := p.AddSerialPort(context.Background(), "ep1/"+cycleVMUUID)
	if err != nil {
		t.Fatalf("add serial port: %v", err)
	}
	if res.Note != "serial_port_power_off_required" {
		t.Errorf("note = %q, want serial_port_power_off_required", res.Note)
	}
	if !p.deferredArmed("ep1", cycleVMUUID) {
		t.Errorf("deferred apply must be armed when the button path needs a power cycle")
	}
	if got := f.PowerPuts(); len(got) != 0 {
		t.Errorf("AddSerialPort must never send power ops: %v", got)
	}
}

// TestDeferredSerial_AppliesOnObservedPowerOff: the keep-alive tick watches
// the pending VM; while it is ON nothing happens, once OFF is OBSERVED the
// port is added and the entry dropped — with zero power ops throughout.
func TestDeferredSerial_AppliesOnObservedPowerOff(t *testing.T) {
	iso := []byte("FAKEISO")
	f := newPrismFake(t, "ON", nil, iso)
	p := newCyclePlugin(t, f, iso)
	p.armDeferredSerialAdd("ep1", cycleVMUUID)

	// Still ON: the tick must not touch the spec and must keep the entry.
	p.keepAliveTick(context.Background())
	if f.SpecPuts() != 0 {
		t.Fatalf("tick on a running VM must not PUT, got %d", f.SpecPuts())
	}
	if !p.deferredArmed("ep1", cycleVMUUID) {
		t.Fatal("entry must survive while the VM is ON")
	}

	// Operator (or anything else) powers the VM off out-of-band.
	f.SetPower("OFF")
	p.keepAliveTick(context.Background())
	if !hasSerialIndex(f.Serial(), 2) {
		t.Errorf("COM3 must be added at the observed power-off: %v", f.Serial())
	}
	if p.deferredArmed("ep1", cycleVMUUID) {
		t.Errorf("entry must be dropped after a successful apply")
	}
	if got := f.PowerPuts(); len(got) != 0 {
		t.Errorf("the deferred path must NEVER send power ops: %v", got)
	}
}

// TestDeferredSerial_ManualAddClearsWithoutPUT: the operator added COM3
// manually while the entry was pending — the apply re-checks absence and
// clears the entry with NO spec PUT.
func TestDeferredSerial_ManualAddClearsWithoutPUT(t *testing.T) {
	iso := []byte("FAKEISO")
	f := newPrismFake(t, "OFF", []int{2}, iso)
	p := newCyclePlugin(t, f, iso)
	p.armDeferredSerialAdd("ep1", cycleVMUUID)

	p.applyDeferredSerial(context.Background())
	if f.SpecPuts() != 0 {
		t.Errorf("manual add must clear without a PUT, got %d", f.SpecPuts())
	}
	if p.deferredArmed("ep1", cycleVMUUID) {
		t.Errorf("entry must be dropped when the port is already present")
	}
	if got := f.PowerPuts(); len(got) != 0 {
		t.Errorf("the deferred path must NEVER send power ops: %v", got)
	}
}

// TestDeferredSerial_ArmIsIdempotent: arming the same VM twice keeps one entry.
func TestDeferredSerial_ArmIsIdempotent(t *testing.T) {
	iso := []byte("FAKEISO")
	f := newPrismFake(t, "ON", nil, iso)
	p := newCyclePlugin(t, f, iso)
	p.armDeferredSerialAdd("ep1", cycleVMUUID)
	p.armDeferredSerialAdd("ep1", cycleVMUUID)
	p.mu.RLock()
	n := len(p.pendingSerial)
	p.mu.RUnlock()
	if n != 1 {
		t.Errorf("pending set size = %d, want 1", n)
	}
}
