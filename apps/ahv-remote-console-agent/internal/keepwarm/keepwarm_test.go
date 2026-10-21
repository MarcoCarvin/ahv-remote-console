package keepwarm

import (
	"errors"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"
)

// fakeOp counts ticks and closes.
type fakeOp struct {
	ticks  atomic.Int64
	closes atomic.Int64
}

func (f *fakeOp) Tick() error { f.ticks.Add(1); return nil }
func (f *fakeOp) Close()      { f.closes.Add(1) }

// fakes bundles the injected dirty and cursor ops with their factory-call
// counters. For the cursor op, creation IS the hide and Close IS the
// restore, so cursorCreates counts hides and cursor.closes counts restores.
type fakes struct {
	dirty, cursor               fakeOp
	dirtyCreates, cursorCreates atomic.Int64
}

// newTestLease wires a Lease to fake op factories with a fast tick so the
// state-machine tests run in milliseconds. dirtyErr/cursorErr != nil
// simulate a platform missing that op.
func newTestLease(dirtyErr, cursorErr error) (*Lease, *fakes) {
	f := &fakes{}
	l := &Lease{
		log:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		interval: 2 * time.Millisecond,
		newOp: func() (op, error) {
			f.dirtyCreates.Add(1)
			if dirtyErr != nil {
				return nil, dirtyErr
			}
			return &f.dirty, nil
		},
		newCursorOp: func() (op, error) {
			f.cursorCreates.Add(1)
			if cursorErr != nil {
				return nil, cursorErr
			}
			return &f.cursor, nil
		},
	}
	return l, f
}

// waitFor polls cond until true or the deadline lapses.
func waitFor(t *testing.T, d time.Duration, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("condition not met within %v: %s", d, msg)
}

func (l *Lease) isRunning() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.running
}

func TestLeaseArmTicksAndExpiryStops(t *testing.T) {
	l, f := newTestLease(nil, nil)
	l.Arm(40 * time.Millisecond)

	waitFor(t, 2*time.Second, func() bool { return f.dirty.ticks.Load() >= 2 },
		"dirty op never ticked after Arm")
	if got := f.cursorCreates.Load(); got != 1 {
		t.Fatalf("cursor not hidden exactly once after Arm: %d hides", got)
	}

	// TTL expiry is the host-vanished backstop: the loop must stop on its
	// own and close BOTH ops — the cursor restore rides the same expiry.
	waitFor(t, 2*time.Second, func() bool {
		return !l.isRunning() && f.dirty.closes.Load() == 1 && f.cursor.closes.Load() == 1
	}, "ops not closed after TTL expiry")
	n := f.dirty.ticks.Load()
	time.Sleep(20 * time.Millisecond)
	if f.dirty.ticks.Load() != n {
		t.Fatalf("dirty op still ticking after expiry: %d -> %d", n, f.dirty.ticks.Load())
	}

	// Re-arm after expiry must re-hide (a fresh cursor op).
	l.Arm(10 * time.Second)
	waitFor(t, 2*time.Second, func() bool { return f.cursorCreates.Load() == 2 },
		"re-arm after expiry did not re-hide the cursor")
	l.Disarm()
	waitFor(t, 2*time.Second, func() bool { return !l.isRunning() }, "loop did not stop")
}

func TestLeaseExtendOutlivesOriginalTTL(t *testing.T) {
	l, f := newTestLease(nil, nil)
	l.Arm(30 * time.Millisecond)
	waitFor(t, 2*time.Second, func() bool { return f.dirty.ticks.Load() >= 1 }, "no first tick")

	// Renew well past the original expiry; the SAME loop must keep ticking.
	for i := 0; i < 4; i++ {
		l.Arm(30 * time.Millisecond)
		time.Sleep(15 * time.Millisecond)
	}
	if !l.isRunning() {
		t.Fatal("lease stopped despite renewals")
	}
	if got := f.dirtyCreates.Load(); got != 1 {
		t.Fatalf("extend restarted the dirty op: %d creates, want 1", got)
	}
	if got := f.cursorCreates.Load(); got != 1 {
		t.Fatalf("extend re-hid the cursor: %d hides, want 1", got)
	}
	l.Disarm()
}

func TestLeaseDisarmStopsPromptly(t *testing.T) {
	l, f := newTestLease(nil, nil)
	l.Arm(10 * time.Second)
	waitFor(t, 2*time.Second, func() bool { return f.dirty.ticks.Load() >= 1 }, "no first tick")

	l.Disarm()
	waitFor(t, 2*time.Second, func() bool {
		return !l.isRunning() && f.dirty.closes.Load() == 1 && f.cursor.closes.Load() == 1
	}, "ops not closed (cursor not restored) after Disarm")
}

func TestLeaseRearmAfterStop(t *testing.T) {
	l, f := newTestLease(nil, nil)
	l.Arm(10 * time.Second)
	waitFor(t, 2*time.Second, func() bool { return f.dirty.ticks.Load() >= 1 }, "no first tick")
	l.Disarm()
	waitFor(t, 2*time.Second, func() bool { return !l.isRunning() }, "loop did not stop")

	// Re-arm: a fresh loop and fresh ops (the old window was destroyed and
	// the cursors restored — the new lease must re-hide).
	n := f.dirty.ticks.Load()
	l.Arm(10 * time.Second)
	waitFor(t, 2*time.Second, func() bool { return f.dirty.ticks.Load() > n }, "no tick after re-arm")
	if got := f.dirtyCreates.Load(); got != 2 {
		t.Fatalf("re-arm did not recreate the dirty op: %d creates, want 2", got)
	}
	if got := f.cursorCreates.Load(); got != 2 {
		t.Fatalf("re-arm did not re-hide the cursor: %d hides, want 2", got)
	}
	l.Disarm()
}

func TestLeaseArmZeroTTLDisarms(t *testing.T) {
	l, f := newTestLease(nil, nil)
	l.Arm(10 * time.Second)
	waitFor(t, 2*time.Second, func() bool { return f.dirty.ticks.Load() >= 1 }, "no first tick")

	l.Arm(0) // ttl=0 token = cancel
	waitFor(t, 2*time.Second, func() bool { return !l.isRunning() && f.cursor.closes.Load() == 1 },
		"Arm(0) did not stop the loop and restore the cursor")
}

func TestLeaseArmHidesAndDisarmRestoresCursor(t *testing.T) {
	l, f := newTestLease(nil, nil)
	l.Arm(10 * time.Second)
	waitFor(t, 2*time.Second, func() bool { return f.cursorCreates.Load() == 1 },
		"cursor not hidden after Arm")
	if got := f.cursor.closes.Load(); got != 0 {
		t.Fatalf("cursor restored while the lease is live: %d restores", got)
	}
	l.Disarm()
	waitFor(t, 2*time.Second, func() bool { return !l.isRunning() && f.cursor.closes.Load() == 1 },
		"cursor not restored after Disarm")
}

func TestLeaseDoubleArmDoesNotDoubleHide(t *testing.T) {
	l, f := newTestLease(nil, nil)
	l.Arm(10 * time.Second)
	waitFor(t, 2*time.Second, func() bool { return f.cursorCreates.Load() == 1 },
		"cursor not hidden after first Arm")

	n := f.dirty.ticks.Load()
	l.Arm(10 * time.Second) // extend while live
	waitFor(t, 2*time.Second, func() bool { return f.dirty.ticks.Load() > n },
		"loop not ticking after double Arm")
	if got := f.cursorCreates.Load(); got != 1 {
		t.Fatalf("double Arm double-hid the cursor: %d hides, want 1", got)
	}
	if got := f.cursor.closes.Load(); got != 0 {
		t.Fatalf("double Arm restored the cursor mid-lease: %d restores", got)
	}
	l.Disarm()
}

func TestLeaseCursorUnsupportedLatchesIndependently(t *testing.T) {
	l, f := newTestLease(nil, errors.ErrUnsupported)
	l.Arm(10 * time.Second)
	// The dirty op must keep running despite the missing cursor op.
	waitFor(t, 2*time.Second, func() bool { return f.dirty.ticks.Load() >= 1 }, "no dirty tick")
	if got := f.cursorCreates.Load(); got != 1 {
		t.Fatalf("cursor factory tries: %d, want 1", got)
	}
	l.mu.Lock()
	latched := l.cursorUnsupported && !l.dirtyUnsupported
	l.mu.Unlock()
	if !latched {
		t.Fatal("cursor-op failure did not latch independently of the dirty op")
	}

	// A latched cursor factory is never retried, even across a re-arm.
	l.Disarm()
	waitFor(t, 2*time.Second, func() bool { return !l.isRunning() }, "loop did not stop")
	l.Arm(10 * time.Second)
	waitFor(t, 2*time.Second, func() bool { return f.dirtyCreates.Load() == 2 }, "no re-arm loop")
	if got := f.cursorCreates.Load(); got != 1 {
		t.Fatalf("latched cursor factory retried: %d tries, want 1", got)
	}
	l.Disarm()
}

func TestLeaseUnsupportedPlatformLatches(t *testing.T) {
	l, f := newTestLease(errors.ErrUnsupported, errors.ErrUnsupported)
	l.Arm(10 * time.Second)
	waitFor(t, 2*time.Second, func() bool { return !l.isRunning() },
		"unsupported ops did not stop the loop")
	l.mu.Lock()
	latched := l.dirtyUnsupported && l.cursorUnsupported
	l.mu.Unlock()
	if !latched {
		t.Fatal("unsupported not latched after op create failures")
	}
	// Subsequent Arms are no-ops: no loop is spawned at all.
	l.Arm(10 * time.Second)
	if l.isRunning() {
		t.Fatal("Arm spawned a loop on a latched-unsupported lease")
	}
	if d, c := f.dirtyCreates.Load(), f.cursorCreates.Load(); d != 1 || c != 1 {
		t.Fatalf("latched factories retried: dirty=%d cursor=%d, want 1/1", d, c)
	}
}

// TestLeaseStopBlocksUntilCursorRestored: Stop must not return before the op
// loop has closed its ops — the daemon-shutdown path relies on this so the
// process cannot exit with the guest cursors still hidden (process death does
// not undo SetSystemCursor; only the NEXT daemon start would heal it).
func TestLeaseStopBlocksUntilCursorRestored(t *testing.T) {
	l, f := newTestLease(nil, nil)
	l.Arm(10 * time.Second)
	waitFor(t, 2*time.Second, func() bool { return f.cursorCreates.Load() == 1 },
		"cursor not hidden after Arm")

	l.Stop()
	// No waitFor here — the restore must have happened BY THE TIME Stop
	// returned, synchronously from the caller's point of view.
	if got := f.cursor.closes.Load(); got != 1 {
		t.Fatalf("Stop returned before the cursor restore ran: %d restores, want 1", got)
	}
	if l.isRunning() {
		t.Fatal("op loop still running after Stop returned")
	}
}
