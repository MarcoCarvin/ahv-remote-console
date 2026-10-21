package ahv

import (
	"context"
	"testing"
)

// TestGetVMStats_HappyPath: a known resourceID returns the parsed sample the
// fake serves on the v3 groups query. Read-only — it issues no writes.
func TestGetVMStats_HappyPath(t *testing.T) {
	iso := []byte("FAKEISO")
	f := newPrismFake(t, "ON", nil, iso)
	p := newCyclePlugin(t, f, iso)

	stats, err := p.GetVMStats(context.Background(), "ep1/"+cycleVMUUID)
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	if stats.CPUUsagePpm != 250000 {
		t.Errorf("cpu = %d, want 250000", stats.CPUUsagePpm)
	}
	if stats.MemoryUsagePpm != 500000 {
		t.Errorf("mem = %d, want 500000", stats.MemoryUsagePpm)
	}
	if stats.DiskReadBps != 1024 || stats.DiskWriteBps != 2048 {
		t.Errorf("disk = %d/%d, want 1024/2048", stats.DiskReadBps, stats.DiskWriteBps)
	}
	if stats.NetTxBps != 4096 || stats.NetRxBps != 8192 {
		t.Errorf("net tx/rx = %d/%d, want 4096/8192", stats.NetTxBps, stats.NetRxBps)
	}
	if stats.SampledAt.IsZero() {
		t.Error("SampledAt not set")
	}
	// A pure read must never write to the VM spec or power state.
	if f.SpecPuts() != 0 || len(f.PowerPuts()) != 0 {
		t.Errorf("stats read issued writes: spec=%d power=%v", f.SpecPuts(), f.PowerPuts())
	}
}

// TestGetVMStats_UnknownEndpoint: a resourceID pointing at an endpoint the
// plugin doesn't manage surfaces a wrapped error rather than a stats sample.
func TestGetVMStats_UnknownEndpoint(t *testing.T) {
	iso := []byte("FAKEISO")
	f := newPrismFake(t, "ON", nil, iso)
	p := newCyclePlugin(t, f, iso)

	if _, err := p.GetVMStats(context.Background(), "nope/"+cycleVMUUID); err == nil {
		t.Fatal("expected error for unknown endpoint")
	}
}

// TestGetVMStats_MalformedResourceID: a resourceID whose VM segment is not a
// canonical UUID is rejected by splitResourceID before any upstream call.
func TestGetVMStats_MalformedResourceID(t *testing.T) {
	iso := []byte("FAKEISO")
	f := newPrismFake(t, "ON", nil, iso)
	p := newCyclePlugin(t, f, iso)

	if _, err := p.GetVMStats(context.Background(), "ep1/not-a-uuid"); err == nil {
		t.Fatal("expected error for malformed resource ID")
	}
}
