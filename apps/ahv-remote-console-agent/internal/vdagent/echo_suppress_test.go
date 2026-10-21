package vdagent

import (
	"context"
	"testing"
	"time"
)

// TestPollClipboard_SuppressesSlowHostEcho is the regression for the
// channel-wedging bug: a large host→guest paste writes the guest clipboard, and
// the OS change-event echo of that write must NOT be sent back to the host —
// even when the echo is observed long after the write (a slow large paste). The
// old 3-second wall-clock window failed to cover that, so the echo escaped,
// looped back through reverse-BCP, and wedged the channel ("worked once, then
// stopped"). The single-consume marker suppresses it regardless of timing.
func TestPollClipboard_SuppressesSlowHostEcho(t *testing.T) {
	port := &fakePort{}
	clip := &fakeClipboard{}
	clip.watchCh = make(chan struct{})
	a := newTestAgent(port, clip)
	a.connected = true
	a.clipboardEnabled = true

	const pasted = "line one\nline two\nline three"
	if err := a.sanitizeAndWriteClipboard(context.Background(), pasted); err != nil {
		t.Fatalf("sanitizeAndWriteClipboard: %v", err)
	}

	// Simulate a SLOW echo: the marker was recorded 10 s ago — beyond the OLD 3 s
	// TTL but well inside the 30 s backstop — and the OS re-exposes the text as
	// CRLF (Windows round-trip), which canonicalisation must see through.
	a.mu.Lock()
	a.lastClipTime = time.Now().Add(-10 * time.Second)
	a.mu.Unlock()
	clip.mu.Lock()
	clip.readText = "line one\r\nline two\r\nline three"
	clip.mu.Unlock()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { a.pollClipboard(ctx); close(done) }()
	defer func() { cancel(); <-done }()

	clip.watchCh <- struct{}{}
	waitFor(t, "echo marker consumed", func() bool {
		a.mu.Lock()
		defer a.mu.Unlock()
		return !a.lastClipEchoPending
	})
	if n := port.writtenCount(); n != 0 {
		t.Fatalf("slow host echo was NOT suppressed — %d grab(s) sent (would loop back through reverse-BCP and wedge the channel)", n)
	}

	// A platform double-fire of the SAME echo, now that the marker is consumed,
	// must still be dropped by the lastText guard — not leak a grab.
	clip.watchCh <- struct{}{}
	time.Sleep(50 * time.Millisecond)
	if n := port.writtenCount(); n != 0 {
		t.Fatalf("double-fire of the consumed echo leaked %d grab(s)", n)
	}
}

// TestPollClipboard_SendsGenuineOperatorCopy guards the other side: after a host
// paste sets the echo marker, a genuinely DIFFERENT guest copy (a real operator
// copy) must still be announced to the host — suppression must not swallow it.
func TestPollClipboard_SendsGenuineOperatorCopy(t *testing.T) {
	port := &fakePort{}
	clip := &fakeClipboard{}
	clip.watchCh = make(chan struct{})
	a := newTestAgent(port, clip)
	a.connected = true
	a.clipboardEnabled = true

	if err := a.sanitizeAndWriteClipboard(context.Background(), "host pasted text"); err != nil {
		t.Fatalf("sanitizeAndWriteClipboard: %v", err)
	}
	clip.mu.Lock()
	clip.readText = "operator copied something else entirely"
	clip.mu.Unlock()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { a.pollClipboard(ctx); close(done) }()
	defer func() { cancel(); <-done }()

	clip.watchCh <- struct{}{}
	waitFor(t, "genuine operator copy grabbed", func() bool { return port.writtenCount() > 0 })
}

// TestPollClipboard_ConsumeRaceKeepsNewerMarker exercises the interleaving the
// single-threaded tests can't: a SECOND host paste lands while pollClipboard is
// mid-read of the FIRST paste's echo. Consuming the first echo must NOT clear
// the second paste's live marker — otherwise the second echo escapes into
// reverse-BCP and re-wedges the channel (the exact bug this fix targets). In
// production the second paste is >64 KB (routed to startReverseBlob); the marker
// logic is size-independent, so a distinct payload suffices here.
func TestPollClipboard_ConsumeRaceKeepsNewerMarker(t *testing.T) {
	port := &fakePort{}
	clip := &fakeClipboard{}
	clip.watchCh = make(chan struct{})
	a := newTestAgent(port, clip)
	a.connected = true
	a.clipboardEnabled = true

	const t1 = "first host paste content"
	const t2 = "second host paste — a different payload entirely"
	const probe = "a genuine operator copy"

	if err := a.sanitizeAndWriteClipboard(context.Background(), t1); err != nil {
		t.Fatalf("write t1: %v", err)
	}

	// Three events are sequenced by the UNBUFFERED watch channel (each send
	// blocks until pollClipboard has fully processed the previous one). Each
	// event's read text is armed via a read-hook so it is never raced against
	// pollClipboard's own read snapshot.
	//   e1: t1's echo — and DURING that read, paste t2 lands (H2 goes live).
	//   e2: t2's echo — must be suppressed (H2 must have survived e1's consume).
	//   e3: a genuine probe copy — must grab, and is our completion signal.
	clip.mu.Lock()
	clip.readText = t1
	clip.readHook = func() {
		_ = a.sanitizeAndWriteClipboard(context.Background(), t2) // H2 goes live mid-read
		clip.mu.Lock()
		clip.readText = t2
		clip.readHook = func() { // runs on e2's read; arm e3
			clip.mu.Lock()
			clip.readText = probe
			clip.readHook = nil
			clip.mu.Unlock()
		}
		clip.mu.Unlock()
	}
	clip.mu.Unlock()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { a.pollClipboard(ctx); close(done) }()
	defer func() { cancel(); <-done }()

	clip.watchCh <- struct{}{} // e1
	clip.watchCh <- struct{}{} // e2 (blocks until e1 done)
	clip.watchCh <- struct{}{} // e3 (blocks until e2 done)

	waitFor(t, "probe grabbed (e2 complete)", func() bool {
		return string(grabCacheState(a)) == probe
	})

	// e1's echo is suppressed (no grab); e2's echo must ALSO be suppressed
	// because H2 survived — so only the probe grabbed: exactly ONE grab. With
	// the consume race, e1's consume clears H2, e2's echo escapes into a grab,
	// and we'd see TWO.
	if n := port.writtenCount(); n != 1 {
		t.Fatalf("expected 1 grab (probe only), got %d — the second paste's echo escaped (consume cleared the wrong marker)", n)
	}
}

// TestSanitizeAndWriteClipboard_ClearsMarkerOnWriteFailure: if the guest write
// fails, the pre-write echo marker must be cleared, so a later genuine identical
// operator copy is NOT wrongly suppressed as an echo of a paste that never
// landed.
func TestSanitizeAndWriteClipboard_ClearsMarkerOnWriteFailure(t *testing.T) {
	port := &fakePort{}
	clip := &fakeClipboard{writeErr: context.DeadlineExceeded}
	a := newTestAgent(port, clip)

	if err := a.sanitizeAndWriteClipboard(context.Background(), "never lands"); err == nil {
		t.Fatal("expected write failure to surface an error")
	}
	a.mu.Lock()
	pending := a.lastClipEchoPending
	a.mu.Unlock()
	if pending {
		t.Fatal("echo marker left pending after a FAILED write — a later identical copy would be wrongly suppressed")
	}
}
