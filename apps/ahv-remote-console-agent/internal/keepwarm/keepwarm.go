// Package keepwarm keeps the QEMU display minimally dirty while a console
// stream is attached, so the host's adaptive vnc_refresh timer stays near its
// 30ms base instead of backing off toward 3000ms on an idle desktop (the
// "first frame after idle is janky" ceiling).
//
// The proxy arms a TTL lease over the clipboard side-channel
// ("__AHVRC_KEEPWARM__<ttl_s>", renewed while the console is attached and
// cancelled with ttl=0 on detach); while the lease is live the agent runs a
// tiny platform "dirty op" every 25ms (tickInterval) — on Windows, toggling one pixel of an
// invisible-in-practice 1x1 layered window by 1 LSB in the screen's far
// corner. When no console is attached (lease expired or cancelled) the guest
// is left completely alone.
//
// The same lease also drives a guest cursor takeover on Windows: while a
// console is attached every standard system cursor is swapped for an
// invisible one, so the guest composites no pointer into the framebuffer and
// the operator's instant local OS arrow is THE pointer (no trailing second
// cursor a frame behind over VNC). The cursors are restored the moment the
// lease ends — disarm, TTL expiry, or daemon shutdown — and defensively at
// daemon startup (New) in case a previous daemon died mid-lease.
//
// Operator transparency: this IS guest-side drawing, and it DOES hide the
// guest's own pointer while a console is attached — but both ops are gated
// on that attached console (the operator is looking at the screen they
// affect), bounded by the lease TTL when the host vanishes, scoped to the
// operator's own user session, and never touch the guest clipboard, input,
// or any user-visible pixel beyond a 1-LSB corner toggle.
package keepwarm

import (
	"log/slog"
	"runtime"
	"sync"
	"time"
)

// tickInterval is the dirty-op duty cycle while a lease is live. It must be
// SHORTER than QEMU's 30ms vnc_refresh base: the refresh interval saw-tooths
// between dirty hits (reset to 30ms on damage, +50ms per clean scan), so a
// 250ms pulse let it climb 30->80->130->180 and a click from visual idle
// waited ~100ms average / ~180ms+ worst for the next scan — perceived as
// "slow clicks". At 25ms every scan finds damage and the interval stays
// pinned at the 30ms floor, the same cadence an actively-changing screen
// produces anyway. Cost: ~40 one-pixel ops/s in the guest and ~30 one-pixel
// framebuffer updates/s on the wire (a few KB/s) — both negligible.
const tickInterval = 25 * time.Millisecond

// op is one platform lease-scoped operation: the display dirty op or the
// cursor takeover. It is created and driven on a single locked OS thread
// (Win32 windows are thread-affine).
type op interface {
	// Tick performs one duty-cycle step (a no-op for level-triggered ops
	// like the cursor takeover, which acts on create/Close).
	Tick() error
	// Close releases the op's resources (destroys the window / restores
	// the cursors).
	Close()
}

// Lease is the keep-warm lease state machine: Arm starts/extends it for a
// TTL, Disarm stops it immediately, and expiry stops it without host help
// (the backstop when a cancel never arrives). All state is mutex-protected;
// the dirty loop runs on its own goroutine only while the lease is live.
type Lease struct {
	log *slog.Logger

	// interval and the op factories are fixed at construction; tests inject
	// fakes. newOp is the display dirty op; newCursorOp is the cursor
	// takeover (creation = hide, Close = restore). Each latches unsupported
	// independently so a platform missing one still runs the other.
	interval    time.Duration
	newOp       func() (op, error)
	newCursorOp func() (op, error)

	mu                sync.Mutex
	expiry            time.Time // zero = disarmed
	running           bool      // an op loop goroutine is live
	dirtyUnsupported  bool      // newOp failed once — never retry
	cursorUnsupported bool      // newCursorOp failed once — never retry
}

// New returns a Lease driving the platform ops at the standard duty cycle.
//
// It also heals crash leftovers: if a previous daemon died mid-lease nothing
// restored the hidden guest cursors, so New restores the user's registry
// cursor scheme defensively. New runs exactly once per daemon (vdagent
// constructs its keep-warm controller once at startup), and the restore is
// harmless when the cursors are already the user's own.
func New(log *slog.Logger) *Lease {
	restoreDefaultCursors()
	return &Lease{log: log, interval: tickInterval, newOp: newPlatformOp, newCursorOp: newPlatformCursorOp}
}

// Arm starts the lease (or extends a live one) for ttl. A ttl <= 0 disarms.
func (l *Lease) Arm(ttl time.Duration) {
	if ttl <= 0 {
		l.Disarm()
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.dirtyUnsupported && l.cursorUnsupported {
		return
	}
	l.expiry = time.Now().Add(ttl)
	if !l.running {
		l.running = true
		go l.run()
	}
}

// Disarm stops the lease immediately. The dirty loop observes the cleared
// expiry within one tick and exits.
func (l *Lease) Disarm() {
	l.mu.Lock()
	l.expiry = time.Time{}
	l.mu.Unlock()
}

// Stop disarms the lease and blocks until the op loop has closed its ops —
// i.e. the guest cursor restore has actually run — bounded by a few ticks.
// Process exit does not undo SetSystemCursor, so a daemon shutting down
// mid-lease must wait here; the startup heal in New only helps the NEXT
// daemon start. The ops are closed on the loop's locked OS thread (they own
// thread-affine Win32 handles), never from the caller's goroutine.
func (l *Lease) Stop() {
	l.Disarm()
	deadline := time.Now().Add(4 * l.interval)
	for time.Now().Before(deadline) {
		l.mu.Lock()
		running := l.running
		l.mu.Unlock()
		if !running {
			return
		}
		time.Sleep(l.interval / 4)
	}
	l.log.Warn("keepwarm: op loop did not stop within the shutdown budget — cursor restore may be pending")
}

// openOps creates this activation's platform ops on the locked OS thread,
// latching any factory that fails so it is never retried. The cursor
// takeover happens inside its factory: by the time openOps returns, the
// guest pointer is hidden (and Close on the returned op restores it).
func (l *Lease) openOps() []op {
	l.mu.Lock()
	dirtyDown, cursorDown := l.dirtyUnsupported, l.cursorUnsupported
	l.mu.Unlock()

	var ops []op
	if !dirtyDown {
		if o, err := l.newOp(); err != nil {
			l.mu.Lock()
			l.dirtyUnsupported = true
			l.mu.Unlock()
			l.log.Info("keepwarm: no display dirty-op on this platform — frame keep-warm disabled", "err", err)
		} else {
			ops = append(ops, o)
		}
	}
	if !cursorDown {
		if o, err := l.newCursorOp(); err != nil {
			l.mu.Lock()
			l.cursorUnsupported = true
			l.mu.Unlock()
			l.log.Info("keepwarm: no cursor-takeover op on this platform — guest pointer stays visible", "err", err)
		} else {
			ops = append(ops, o)
		}
	}
	return ops
}

// run is the op loop: one per live lease, on a locked OS thread (the
// platform ops own thread-affine resources). It exits when the lease expires
// or is disarmed; the expiry check and the running=false handoff happen under
// the same lock acquisition, so an Arm racing the exit either extends the
// live loop or observes running=false and starts a fresh one — never neither.
func (l *Lease) run() {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	ops := l.openOps()
	if len(ops) == 0 {
		l.mu.Lock()
		l.expiry = time.Time{}
		l.running = false
		l.mu.Unlock()
		return
	}
	l.log.Info("keepwarm: lease armed — op loop started", "ops", len(ops))

	t := time.NewTicker(l.interval)
	defer t.Stop()
	for range t.C {
		l.mu.Lock()
		live := !l.expiry.IsZero() && time.Now().Before(l.expiry)
		if !live {
			// Close (and so the cursor restore) runs INSIDE the critical
			// section that publishes running=false: an Arm racing this exit
			// can only observe running=false — and spawn the fresh loop
			// whose cursor op re-hides — after the restore has completed,
			// so a quick disarm/re-arm can never leave the new lease's hide
			// undone by the old lease's restore.
			for _, o := range ops {
				o.Close()
			}
			l.running = false
			l.mu.Unlock()
			l.log.Info("keepwarm: lease ended — op loop stopped")
			return
		}
		l.mu.Unlock()
		for _, o := range ops {
			if err := o.Tick(); err != nil {
				l.log.Debug("keepwarm: op tick failed", "err", err)
			}
		}
	}
}
