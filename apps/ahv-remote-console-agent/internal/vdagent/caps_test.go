package vdagent

import (
	"context"
	"encoding/binary"
	"testing"
	"time"
)

// capsWriteCount counts the frames written so far that carry an
// ANNOUNCE_CAPABILITIES message (the agent's outbound caps).
func capsWriteCount(port *fakePort) int {
	port.mu.Lock()
	defer port.mu.Unlock()
	n := 0
	for _, raw := range port.writes {
		if len(raw) < ChunkHeaderSize+8 {
			continue
		}
		if binary.LittleEndian.Uint32(raw[ChunkHeaderSize+4:ChunkHeaderSize+8]) == MsgAnnounceCapabilities {
			n++
		}
	}
	return n
}

// waitForCapsAtLeast polls until at least n caps frames have been written or
// the deadline elapses, returning the final count.
func waitForCapsAtLeast(port *fakePort, n int, within time.Duration) int {
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if c := capsWriteCount(port); c >= n {
			return c
		}
		time.Sleep(2 * time.Millisecond)
	}
	return capsWriteCount(port)
}

// TestCapsHeartbeat_ContinuesAfterNegotiated pins the new behavior: once
// clipboard is negotiated the caps heartbeat MUST keep sending at the
// heartbeat cadence. Before this change capsRetryLoop returned on the first
// post-negotiation tick, so caps stopped — and a fresh vdagent peer after a
// backend swap / live migration would never be answered.
func TestCapsHeartbeat_ContinuesAfterNegotiated(t *testing.T) {
	port := &fakePort{}
	a := newTestAgent(port, &fakeClipboard{})

	// Already negotiated before the loop starts: the loop must NOT stop.
	a.mu.Lock()
	a.clipboardEnabled = true
	a.mu.Unlock()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Tiny intervals so the test runs fast; heartbeat cadence is exercised.
	go a.capsRetryLoop(ctx, 2*time.Millisecond, 5*time.Millisecond)

	// Several heartbeat beats must land — old behavior produced at most one.
	if got := waitForCapsAtLeast(port, 3, 2*time.Second); got < 3 {
		t.Fatalf("caps frames after negotiation = %d, want >= 3 (heartbeat must keep sending)", got)
	}
}

// TestCapsHeartbeat_FreshPeerReNegotiates simulates a fresh vdagent peer
// announcing AFTER the first negotiation (a backend swap / migration
// re-attach): the heartbeat keeps caps flowing, and handleCapabilities
// re-negotiates the new peer, re-enabling clipboard without a restart.
func TestCapsHeartbeat_FreshPeerReNegotiates(t *testing.T) {
	port := &fakePort{}
	a := newTestAgent(port, &fakeClipboard{})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go a.capsRetryLoop(ctx, 2*time.Millisecond, 5*time.Millisecond)

	// First peer announces clipboard caps: negotiation succeeds.
	announce := capsBody(false, CapClipboardByDemand, CapClipboardSelection)
	if err := a.handleCapabilities(ctx, announce); err != nil {
		t.Fatalf("handleCapabilities (first peer): %v", err)
	}
	a.mu.Lock()
	enabled := a.clipboardEnabled
	a.mu.Unlock()
	if !enabled {
		t.Fatal("clipboard not enabled after first peer announce")
	}

	// Confirm the heartbeat did NOT stop after negotiation.
	before := capsWriteCount(port)
	if got := waitForCapsAtLeast(port, before+2, 2*time.Second); got < before+2 {
		t.Fatalf("heartbeat stopped after negotiation: caps went %d -> %d", before, got)
	}

	// A fresh peer (re)appears and re-announces. The heartbeat's request=true
	// caps are what prompt this; handleCapabilities must re-negotiate it.
	a.mu.Lock()
	a.clipboardEnabled = false // a fresh peer starts un-negotiated
	a.connected = false
	a.mu.Unlock()
	if err := a.handleCapabilities(ctx, announce); err != nil {
		t.Fatalf("handleCapabilities (fresh peer): %v", err)
	}
	a.mu.Lock()
	reEnabled := a.clipboardEnabled
	reConnected := a.connected
	a.mu.Unlock()
	if !reEnabled || !reConnected {
		t.Fatalf("fresh peer not re-negotiated: enabled=%v connected=%v", reEnabled, reConnected)
	}
}

// TestCapsHeartbeat_CtxCancelStops verifies ctx cancellation stops the
// heartbeat: no further caps are sent after cancel.
func TestCapsHeartbeat_CtxCancelStops(t *testing.T) {
	port := &fakePort{}
	a := newTestAgent(port, &fakeClipboard{})
	a.mu.Lock()
	a.clipboardEnabled = true
	a.mu.Unlock()

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		a.capsRetryLoop(ctx, 2*time.Millisecond, 5*time.Millisecond)
		close(done)
	}()

	// Let a few beats happen, then cancel.
	waitForCapsAtLeast(port, 2, 2*time.Second)
	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("capsRetryLoop did not return after ctx cancel")
	}

	// No new caps after the loop has returned.
	after := capsWriteCount(port)
	time.Sleep(30 * time.Millisecond)
	if got := capsWriteCount(port); got != after {
		t.Fatalf("caps kept being sent after cancel: %d -> %d", after, got)
	}
}
