package proxy

import (
	"log/slog"
	"testing"

	"github.com/MarcoCarvin/ahv-remote-console/apps/runtime/internal/plugins"
	"github.com/MarcoCarvin/ahv-remote-console/apps/runtime/internal/rfb"
)

// TestExtClipReannounceGuard: after we re-advertise ext-clip CAPS, QEMU
// re-announces its STALE clipboard (a spurious NOTIFY). Requesting + retrying
// that hammers the single clipboard slot and starved real pastes (521 "guest
// copy lost" storms in the field). onExtClipNotify must swallow exactly that
// first re-announce, then behave normally — never permanently pausing real
// requests (which is what the removed circuit breaker did wrong).
func TestExtClipReannounceGuard(t *testing.T) {
	newProxy := func() *proxy {
		logger := slog.New(slog.NewTextHandler(nopWriter{}, &slog.HandlerOptions{Level: slog.LevelError}))
		return &proxy{log: logger, sess: &plugins.Session{ID: "grace"}, stream: rfb.NewStreamProcessor()}
	}

	// 1) With the guard armed (CAPS just re-advertised), the FIRST NOTIFY is the
	//    stale re-announce: skipped (no REQUEST armed) and the guard is consumed.
	p := newProxy()
	p.expectStaleReannounce.Store(true)
	p.onExtClipNotify()
	if p.clipReqDeadline.Load() != 0 {
		t.Fatal("stale re-announce NOTIFY must NOT arm a REQUEST")
	}
	if p.expectStaleReannounce.Load() {
		t.Fatal("guard must be consumed after the single skipped re-announce")
	}

	// 2) The NEXT NOTIFY (guard consumed) requests normally — a genuine guest
	//    copy is never suppressed beyond that one re-announce.
	p.onExtClipNotify()
	if p.clipReqDeadline.Load() == 0 {
		t.Fatal("a normal NOTIFY after the re-announce must arm a REQUEST")
	}

	// 3) With the guard NOT armed, a NOTIFY requests (the common steady state) —
	//    the guard is not a global mute, so it can never black out the channel.
	p = newProxy()
	p.onExtClipNotify()
	if p.clipReqDeadline.Load() == 0 {
		t.Fatal("a NOTIFY with the guard unarmed must arm a REQUEST")
	}
}
