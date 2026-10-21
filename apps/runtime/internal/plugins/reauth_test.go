package plugins

import "testing"

// chanClosed reports whether ch is closed without consuming a real send.
func chanClosed(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

// TestSessionReauthSignalRearmable is the regression guard for the post-expiry
// re-auth loop: the signal must be RE-ARMABLE, not close-once. A session is kept
// alive across an expiry so the SAME id reconnects after an in-console re-auth;
// if SignalReauth could only ever close the channel, a fresh proxy attaching to
// the re-authed session would see the stale closed channel and emit
// session_expired forever. ClearReauth must reopen it.
func TestSessionReauthSignalRearmable(t *testing.T) {
	s := &Session{}
	s.ClearReauth() // arm (mirrors OpenSession)

	if chanClosed(s.ReauthCh()) {
		t.Fatal("freshly armed reauth channel must be open")
	}

	s.SignalReauth()
	if !chanClosed(s.ReauthCh()) {
		t.Fatal("SignalReauth must close the channel")
	}
	// Idempotent within an arm cycle (must not double-close panic).
	s.SignalReauth()

	// Re-arm: a fresh attach must get an OPEN channel again.
	s.ClearReauth()
	if chanClosed(s.ReauthCh()) {
		t.Fatal("ClearReauth must re-arm (reopen) the channel")
	}

	// And it must fire again on the next out-of-band expiry (SH2 still works).
	s.SignalReauth()
	if !chanClosed(s.ReauthCh()) {
		t.Fatal("SignalReauth after ClearReauth must close again")
	}
}

// TestSessionReauthNilSafe: a Session constructed without arming (no OpenSession
// / ClearReauth, e.g. a bare test session) has a nil channel that never fires,
// and SignalReauth is a no-op rather than a nil-close panic.
func TestSessionReauthNilSafe(t *testing.T) {
	s := &Session{}
	s.SignalReauth() // must not panic
	if s.ReauthCh() != nil {
		t.Fatal("an unarmed reauth channel must be nil (never fires in a select)")
	}
}
