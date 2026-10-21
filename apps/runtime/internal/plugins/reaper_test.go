package plugins

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/MarcoCarvin/ahv-remote-console/apps/runtime/pkg/sdk"
)

// fakePlugin implements sdk.Plugin with just enough behavior for the host
// session-lifecycle tests. OpenSession returns a fresh ID; CloseSession records
// the call so the reaper test can assert the session was actually closed.
type fakePlugin struct {
	id        string
	openCalls atomic.Int32
	closes    atomic.Int32
	openErr   error
}

func (f *fakePlugin) Manifest() sdk.Manifest                       { return sdk.Manifest{ID: f.id, Version: "test"} }
func (f *fakePlugin) Init(context.Context, sdk.Dependencies) error { return nil }
func (f *fakePlugin) Shutdown(context.Context) error               { return nil }
func (f *fakePlugin) AuthStart(context.Context, json.RawMessage) (any, error) {
	return nil, nil
}
func (f *fakePlugin) AuthComplete(context.Context, json.RawMessage) (any, error) {
	return nil, nil
}
func (f *fakePlugin) AuthRefresh(context.Context) error { return nil }
func (f *fakePlugin) Logout(context.Context) error      { return nil }
func (f *fakePlugin) ListResources(context.Context, sdk.ListQuery) (sdk.ListResult, error) {
	return sdk.ListResult{}, nil
}
func (f *fakePlugin) RefreshResources(context.Context) error { return nil }
func (f *fakePlugin) OpenSession(_ context.Context, resourceID string) (sdk.SessionInfo, error) {
	if f.openErr != nil {
		return sdk.SessionInfo{}, f.openErr
	}
	f.openCalls.Add(1)
	return sdk.SessionInfo{ResourceID: resourceID}, nil
}
func (f *fakePlugin) OpenStream(context.Context, string) (*websocket.Conn, error) {
	return nil, errors.New("not used in test")
}
func (f *fakePlugin) CloseSession(context.Context, string) error {
	f.closes.Add(1)
	return nil
}

func newTestHost(t *testing.T) *Host {
	t.Helper()
	h, err := NewHost(Options{
		Logger: slog.New(slog.NewTextHandler(discard{}, &slog.HandlerOptions{Level: slog.LevelError})),
	})
	if err != nil {
		t.Fatalf("NewHost: %v", err)
	}
	// Stop the background reaper goroutine started by NewHost; these tests drive
	// reapOnce deterministically. Cancel the base context and wait for it.
	h.cancelBase()
	<-h.reaperDone
	// Re-arm a fresh, un-cancelled base context so OpenSession / CloseSession
	// (which use h.baseCtx via the reaper path) behave normally.
	h.baseCtx, h.cancelBase = context.WithCancel(context.Background())
	return h
}

type discard struct{}

func (discard) Write(p []byte) (int, error) { return len(p), nil }

// AHV-03: a session with no attached renderer stream whose last proxied frame
// is older than the idle TTL must be reaped (CloseSession called, removed from
// the host) — otherwise an abandoned renderer leaks the upstream session.
func TestReaperClosesIdleStreamlessSession(t *testing.T) {
	h := newTestHost(t)
	h.idleTTL = 50 * time.Millisecond
	fp := &fakePlugin{id: "ahv"}

	info, err := h.OpenSession(context.Background(), fp, "vm-1")
	if err != nil {
		t.Fatalf("OpenSession: %v", err)
	}
	sess, ok := h.GetSession(info.ID)
	if !ok {
		t.Fatal("session not registered")
	}
	// No stream ever attached; force lastSeen well into the past.
	sess.lastSeen.Store(time.Now().Add(-time.Hour).UnixNano())

	h.reapOnce(time.Now())

	if _, ok := h.GetSession(info.ID); ok {
		t.Fatal("idle stream-less session past TTL was NOT reaped")
	}
	if fp.closes.Load() != 1 {
		t.Fatalf("expected plugin CloseSession called once, got %d", fp.closes.Load())
	}
}

// A session with an ATTACHED stream must never be reaped, even if its last
// frame is old (a console can legitimately sit idle while attached).
func TestReaperSkipsAttachedSession(t *testing.T) {
	h := newTestHost(t)
	h.idleTTL = 50 * time.Millisecond
	fp := &fakePlugin{id: "ahv"}

	info, err := h.OpenSession(context.Background(), fp, "vm-1")
	if err != nil {
		t.Fatalf("OpenSession: %v", err)
	}
	sess, _ := h.GetSession(info.ID)
	sess.StreamConnected() // renderer attached
	sess.lastSeen.Store(time.Now().Add(-time.Hour).UnixNano())

	h.reapOnce(time.Now())

	if _, ok := h.GetSession(info.ID); !ok {
		t.Fatal("attached session was wrongly reaped")
	}
	if fp.closes.Load() != 0 {
		t.Fatalf("expected no CloseSession for attached session, got %d", fp.closes.Load())
	}
}

// A recently-touched stream-less session (within the TTL grace) must NOT be
// reaped — e.g. a session just opened, renderer about to attach its stream.
func TestReaperSkipsRecentSession(t *testing.T) {
	h := newTestHost(t)
	h.idleTTL = time.Hour
	fp := &fakePlugin{id: "ahv"}

	info, err := h.OpenSession(context.Background(), fp, "vm-1")
	if err != nil {
		t.Fatalf("OpenSession: %v", err)
	}
	// OpenSession seeded lastSeen to now; with a 1h TTL it's well within grace.
	h.reapOnce(time.Now())

	if _, ok := h.GetSession(info.ID); !ok {
		t.Fatal("recently-opened session was wrongly reaped")
	}
}

// AHV-03: the concurrent-session cap must reject opens past the limit so a
// runaway open loop can't exhaust upstream console slots.
func TestOpenSessionCapRejects(t *testing.T) {
	h := newTestHost(t)
	fp := &fakePlugin{id: "ahv"}

	for i := 0; i < maxConcurrentSessions; i++ {
		if _, err := h.OpenSession(context.Background(), fp, "vm"); err != nil {
			t.Fatalf("OpenSession %d unexpectedly failed: %v", i, err)
		}
	}
	if _, err := h.OpenSession(context.Background(), fp, "vm"); !errors.Is(err, ErrTooManySessions) {
		t.Fatalf("expected ErrTooManySessions past the cap, got %v", err)
	}
}
