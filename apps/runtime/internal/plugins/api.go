// Package plugins is the runtime-internal plugin host. It uses the public
// types from apps/runtime/pkg/sdk so that plugins (which import the SDK) and
// the host agree on the contract without import cycles.
package plugins

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"

	"github.com/MarcoCarvin/ahv-remote-console/apps/runtime/pkg/sdk"
)

// Aliases re-exported for internal package use.
type (
	Plugin       = sdk.Plugin
	Manifest     = sdk.Manifest
	Dependencies = sdk.Dependencies
	Logger       = sdk.Logger
	Keystore     = sdk.Keystore
	NetEvent     = sdk.NetEvent
	Telemetry    = sdk.Telemetry
	SpanFinish   = sdk.SpanFinish
	ConfigReader = sdk.ConfigReader
	EndpointRef  = sdk.EndpointRef
	ListQuery    = sdk.ListQuery
	Resource     = sdk.Resource
	ListResult   = sdk.ListResult
	SessionInfo  = sdk.SessionInfo
)

// Session is the runtime-side handle returned to the proxy layer. It is
// independent of the SDK.SessionInfo (which is the wire representation).
type Session struct {
	ID         string
	PluginID   string
	ResourceID string
	// Metadata carries the plugin's per-session descriptors (notably vm_name +
	// cluster_uuid). Preserved here so listSessions can surface a stable display
	// name to the renderer even when the session's resource_id no longer matches
	// the loaded inventory (stale/removed VM) — otherwise the lobby's "live now"
	// strip falls back to the generic "Console" label.
	Metadata map[string]string
	plugin   Plugin
	Stats    SessionStats
	mu       sync.Mutex
	closed   atomic.Bool

	// streams counts how many renderer WS streams are currently attached to
	// this session (normally 0 or 1). The reaper treats a session with no
	// attached stream as a reap candidate once it has been idle past the TTL.
	streams atomic.Int32
	// lastSeen is unix-nano of the most recent proxied frame in either
	// direction (bumped by Touch from the proxy pumps). The reaper uses it to
	// decide whether a stream-less session has been abandoned long enough to
	// close — so an abandoned renderer that never sent DELETE /sessions can't
	// leak the upstream session forever (AHV-03).
	lastSeen atomic.Int64
	// reauthCh is CLOSED when this session's credentials go invalid out-of-band
	// — the plugin's keep-alive saw a 401 and called SignalReauth. The proxy
	// selects on it and proactively emits `session_expired` so the console
	// enters the re-auth arm BEFORE the live stream drops (SH2). Closed (not
	// sent) so every proxy (re)connect iteration's watcher observes it.
	//
	// CRITICAL: the signal is RE-ARMABLE, not close-once. The session is kept
	// alive across an expiry (the plugin keeps p.sessions so the SAME id
	// reconnects after re-auth). A once-closed channel could never un-fire, so
	// a fresh proxy attaching to the re-authed session would observe the stale
	// closed channel and emit session_expired immediately — wedging the console
	// in an endless re-auth loop even though the new credentials are valid. The
	// proxy calls ClearReauth() on each fresh WS attach to re-arm it.
	//
	// Guarded by a DEDICATED mutex, NOT s.mu: OpenStream holds s.mu across the
	// whole upstream dial (~20s), and SignalReauth is called off the keep-alive
	// goroutine while ReauthCh is read by the proxy watcher's select — neither
	// may block on the dial lock.
	reauthMu     sync.Mutex
	reauthCh     chan struct{}
	reauthClosed bool
}

// SignalReauth marks the session's credentials invalid so the proxy proactively
// drives its console into the re-auth arm. Idempotent within an arm cycle and
// nil-safe (a Session constructed without a reauthCh — e.g. in a test — is a
// no-op). ClearReauth re-arms it.
func (s *Session) SignalReauth() {
	s.reauthMu.Lock()
	defer s.reauthMu.Unlock()
	if s.reauthCh == nil || s.reauthClosed {
		return
	}
	close(s.reauthCh)
	s.reauthClosed = true
}

// ClearReauth re-arms the signal: a fresh, open channel so a subsequent
// SignalReauth fires again. Called on each fresh WS attach so a console
// re-attaching to a kept-alive session after an in-console re-auth does not
// inherit a permanently-closed channel (the loop this fixes). Nil-safe.
func (s *Session) ClearReauth() {
	s.reauthMu.Lock()
	defer s.reauthMu.Unlock()
	s.reauthCh = make(chan struct{})
	s.reauthClosed = false
}

// ReauthCh fires (is closed) once SignalReauth has been called, until the next
// ClearReauth re-arms it. A nil channel (uninitialised session) blocks forever
// in a select, i.e. never fires.
func (s *Session) ReauthCh() <-chan struct{} {
	s.reauthMu.Lock()
	defer s.reauthMu.Unlock()
	return s.reauthCh
}

type SessionStats struct {
	OpenedAt          time.Time
	BytesFromClient   uint64
	BytesFromUpstream uint64
}

// OpenStream is invoked by the proxy on initial connect and every reconnect.
func (s *Session) OpenStream(ctx context.Context) (*websocket.Conn, error) {
	if s.closed.Load() {
		return nil, errors.New("session closed")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.plugin.OpenStream(ctx, s.ID)
}

func (s *Session) MarkClosed() { s.closed.Store(true) }

// Touch records that a frame was just proxied in either direction, marking the
// session alive for the reaper. Cheap enough to call per frame (single atomic
// store) on the proxy hot path.
func (s *Session) Touch() { s.lastSeen.Store(time.Now().UnixNano()) }

// StreamConnected / StreamDisconnected bracket a renderer WS stream's lifetime
// so the reaper can tell an actively-attached session from an abandoned one.
// Touch is also called so a freshly-attached session is never immediately a
// reap candidate.
func (s *Session) StreamConnected() {
	s.streams.Add(1)
	s.Touch()
}
func (s *Session) StreamDisconnected() {
	s.streams.Add(-1)
	s.Touch()
}

// StreamAttached reports whether at least one renderer stream is currently
// proxying this session.
func (s *Session) StreamAttached() bool { return s.streams.Load() > 0 }

// LastSeen returns the unix-nano timestamp of the last proxied frame, or the
// session-open time if no frame has been proxied yet.
func (s *Session) LastSeen() int64 { return s.lastSeen.Load() }
