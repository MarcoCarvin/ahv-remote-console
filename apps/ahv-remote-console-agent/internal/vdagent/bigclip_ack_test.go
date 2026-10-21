package vdagent

import (
	"context"
	"sync"
	"testing"
)

// TestSendSideChannel_OverwritesOwnProtocolAck is the repro for the host→guest
// large-paste livelock: a lingering owner-less PROTOCOL token (here a keep-warm
// ack) sat in the single grab cache slot that no CLIPBOARD_REQUEST had drained.
// The old gate deferred every blob ACK behind ANY fresh cache occupancy, so the
// ACK never went out, the host retransmitted the same frame for grabCacheTTL,
// and the transfer never completed. A blob ACK MUST supersede an owner-less
// protocol token (it carries no operator data) and be sent.
func TestSendSideChannel_OverwritesOwnProtocolAck(t *testing.T) {
	port := &fakePort{}
	a := newTestAgent(port, &fakeClipboard{})

	// Owner-less protocol occupant with no request draining it (the field trigger).
	a.setGrabCache([]byte(keepwarmAckText), true)

	ack := blobAck("cafef00d", 2, "ok")
	if err := a.sendBlobAck(context.Background(), ack); err != nil {
		t.Fatalf("sendBlobAck: %v", err)
	}

	if port.writtenCount() == 0 {
		t.Fatal("blob ACK was deferred behind our own owner-less protocol token — the livelock; expected it to be sent")
	}
	if got := string(grabCacheState(a)); got != ack {
		t.Fatalf("grab cache = %q, want the blob ACK %q (ACK must supersede the stale protocol token)", got, ack)
	}
}

// TestSendSideChannel_DefersBehindOperatorCopy guards the data-loss case the
// deferral exists for: a REAL operator guest→host copy (protocol==false) must
// NOT be clobbered by a blob ACK, and the ACK must defer (the host retransmit
// re-produces it once the operator copy drains).
func TestSendSideChannel_DefersBehindOperatorCopy(t *testing.T) {
	port := &fakePort{}
	a := newTestAgent(port, &fakeClipboard{})

	const operator = "operator secret"
	a.setGrabCache([]byte(operator), false) // real operator copy, not yet requested

	if err := a.sendBlobAck(context.Background(), blobAck("dead", 1, "ok")); err != nil {
		t.Fatalf("sendBlobAck: %v", err)
	}

	if port.writtenCount() != 0 {
		t.Fatal("blob ACK clobbered a pending operator copy — operator data would be lost")
	}
	if got := string(grabCacheState(a)); got != operator {
		t.Fatalf("operator copy lost/overwritten: grab cache = %q, want %q", got, operator)
	}
}

// TestSendSideChannel_DefersBehindReverseFrame guards the clause a one-line
// "protocol only" fix would miss: an in-flight reverse-BCP frame is stamped
// protocol==true but with a non-empty grabCacheOwner (it IS operator data, mid
// guest→host chunking). A forward blob ACK must defer behind it, not clobber it.
func TestSendSideChannel_DefersBehindReverseFrame(t *testing.T) {
	port := &fakePort{}
	a := newTestAgent(port, &fakeClipboard{})

	const xfer = "deadbeef"
	a.blobTx.activeID = xfer // make this the active reverse transfer
	frame := []byte(blobFramePrefix + xfer + ":1:3:60:00000000:AAAA")
	if ch := a.armFrame(xfer, 1, frame); ch == nil {
		t.Fatal("armFrame returned nil — reverse frame was not stamped")
	}
	writesAfterArm := port.writtenCount()

	if err := a.sendBlobAck(context.Background(), blobAck("cafef00d", 1, "ok")); err != nil {
		t.Fatalf("sendBlobAck: %v", err)
	}

	if port.writtenCount() != writesAfterArm {
		t.Fatal("forward blob ACK sent a grab while a reverse-BCP frame owned the slot — it would clobber the reverse frame")
	}
	if got := string(grabCacheState(a)); got != string(frame) {
		t.Fatalf("reverse frame overwritten: grab cache = %q, want the reverse frame", got)
	}
	a.mu.Lock()
	owner := a.grabCacheOwner
	a.mu.Unlock()
	if owner != xfer {
		t.Fatalf("grabCacheOwner = %q, want %q (reverse-frame ownership must survive a contending forward ACK)", owner, xfer)
	}
}

// TestSendSideChannel_ConcurrentArmFrameRace exercises the one property the three
// sequential tests above cannot: the ATOMIC check-and-stamp that setGrabCacheLocked
// was split out to make possible. A forward blob ACK and a reverse-BCP armFrame
// race for the single grab slot on separate goroutines, thousands of times.
//
// In the correct code a.mu fully serializes sendSideChannel's check+stamp against
// armFrame's stamp, so the reverse frame (operator data) is ALWAYS the surviving
// writer on EVERY interleaving: either sendSideChannel sees owner!="" and defers,
// or it stamps its ack first and armFrame overwrites it as the last writer. The
// assertion below therefore never flakes on correct code.
//
// Regression guard: revert only the atomicity — an unlocked check followed by a
// re-locking a.setGrabCache(...) — and the check→armFrame-stamps→ack-overwrites
// interleaving silently clobbers the in-flight reverse frame (operator data). That
// TOCTOU is a logic race, invisible to `go test -race` and to all three sequential
// tests; this loop is what catches it. Runs clean under -race.
func TestSendSideChannel_ConcurrentArmFrameRace(t *testing.T) {
	port := &fakePort{}
	a := newTestAgent(port, &fakeClipboard{})

	const xfer = "deadbeef"
	a.blobTx.activeID = xfer // this reverse transfer stays authoritative throughout
	frame := []byte(blobFramePrefix + xfer + ":1:3:60:00000000:AAAA")
	fwdAck := blobAck("cafef00d", 1, "ok")

	const iters = 5000
	for i := 0; i < iters; i++ {
		a.wipeGrabCache() // empty slot so each iteration re-runs the check-vs-stamp window

		start := make(chan struct{})
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			a.armFrame(xfer, 1, frame) // reverse operator frame claims the slot
		}()
		go func() {
			defer wg.Done()
			<-start
			_ = a.sendBlobAck(context.Background(), fwdAck) // forward ack contends for it
		}()
		close(start)
		wg.Wait()

		// The reverse frame must win the slot on every interleaving. If a forward
		// ACK ever displaces it, in-flight operator data was lost — the atomic
		// check-and-stamp regressed.
		if got := grabCacheState(a); string(got) != string(frame) {
			t.Fatalf("iter %d: grab slot = %q, want the reverse frame %q — a forward ACK clobbered in-flight operator data (atomic check-and-stamp regressed)", i, got, frame)
		}
		a.mu.Lock()
		owner := a.grabCacheOwner
		a.mu.Unlock()
		if owner != xfer {
			t.Fatalf("iter %d: grabCacheOwner = %q, want %q", i, owner, xfer)
		}
	}
}
