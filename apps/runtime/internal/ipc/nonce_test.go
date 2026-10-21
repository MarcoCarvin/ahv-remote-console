package ipc

import (
	"testing"
	"time"
)

func TestNonceRejectsReplay(t *testing.T) {
	c := newNonceCache(16, 30*time.Second)
	now := time.Now()
	if !c.use("n1", now) {
		t.Fatal("first use should succeed")
	}
	if c.use("n1", now) {
		t.Fatal("replay should be rejected")
	}
	if !c.use("n2", now) {
		t.Fatal("distinct nonce should be accepted")
	}
}

func TestNonceCapacityEviction(t *testing.T) {
	c := newNonceCache(4, 30*time.Second)
	now := time.Now()
	for i := 0; i < 8; i++ {
		if !c.use(string(rune('a'+i)), now) {
			t.Fatalf("nonce %d should be unique", i)
		}
	}
	// At least the most recently-inserted nonce must still be considered "seen".
	if c.use("h", now) {
		t.Fatal("most recent nonce should still trigger replay rejection")
	}
}

// SEC-03: cap-pressure eviction must drop the OLDEST nonces by timestamp, not a
// random map-iteration sample — a freshly inserted nonce still has its full
// replay window ahead of it and must not be evicted while older ones remain.
func TestNonceEvictsOldestFirst(t *testing.T) {
	c := newNonceCache(4, time.Hour) // long TTL → only cap-pressure eviction fires
	base := time.Now()
	for i := 0; i < 4; i++ {
		if !c.use("o"+string(rune('0'+i)), base.Add(time.Duration(i)*time.Second)) {
			t.Fatalf("o%d should be unique", i)
		}
	}
	// Full at cap; a newer nonce forces eviction of the oldest ~25% (o0).
	if !c.use("new", base.Add(10*time.Second)) {
		t.Fatal("new nonce should be accepted")
	}
	if c.forcedEvictions != 1 {
		t.Fatalf("forcedEvictions = %d, want 1", c.forcedEvictions)
	}
	// Recent nonce o3 retained → replay rejected (the dup check is side-effect-free).
	if c.use("o3", base.Add(3*time.Second)) {
		t.Error("recent nonce o3 must be retained and rejected on replay")
	}
	// Oldest nonce o0 was evicted → no longer 'seen' → accepted as new.
	if !c.use("o0", base) {
		t.Error("oldest nonce o0 should have been evicted (oldest-first)")
	}
}
