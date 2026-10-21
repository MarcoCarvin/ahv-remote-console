package plugins

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/MarcoCarvin/ahv-remote-console/apps/runtime/internal/build"
	"github.com/MarcoCarvin/ahv-remote-console/apps/runtime/internal/config"
	"github.com/MarcoCarvin/ahv-remote-console/apps/runtime/internal/keystore"
	"github.com/MarcoCarvin/ahv-remote-console/apps/runtime/internal/netstate"
	"github.com/MarcoCarvin/ahv-remote-console/apps/runtime/internal/telemetry"
	"github.com/MarcoCarvin/ahv-remote-console/apps/runtime/pkg/sdk"
)

// Session-lifecycle bounds (AHV-03):
//   - maxConcurrentSessions caps how many upstream console sessions can be open
//     at once, so a renderer bug (or a runaway open loop) can't exhaust upstream
//     console slots / runtime resources. Single-operator desktop tool: a handful
//     of simultaneous consoles is generous.
//   - sessionIdleTTL is how long a session with NO attached renderer stream may
//     sit idle before the reaper closes it. A live console keeps a stream
//     attached (and Touches per frame), so only an abandoned session — renderer
//     crashed / closed without DELETE /sessions — is reaped. Set generously
//     (10 min) so a temporary renderer disconnect — laptop sleep, a VPN
//     transition, or a network blip exceeding the renderer's bounded self-heal
//     window — does NOT reap a session the operator is about to resume. At 2 min
//     a sleep/VPN gap reaped the LIVE session, so the reconnect 404'd and
//     surfaced "WebSocket error". A deliberate window close still DELETEs the
//     session immediately; this TTL is only the crash/abandon backstop, and a
//     single-operator tool capped at 16 sessions can afford the longer grace.
//   - reaperInterval is how often the reaper scans. Kept well under the TTL so a
//     reap lands within ~one interval of the grace expiring.
const (
	maxConcurrentSessions = 16
	sessionIdleTTL        = 10 * time.Minute
	reaperInterval        = 30 * time.Second
)

// ErrTooManySessions is returned by OpenSession when the concurrent-session cap
// is reached.
var ErrTooManySessions = errors.New("too many concurrent sessions")

type Host struct {
	dir      string
	log      *slog.Logger
	plugins  map[string]Plugin
	sessions map[string]*Session
	keystore keystore.Store
	tel      *telemetry.Tracer
	netstate *netstate.Observer
	config   *config.Manager
	// auditLogPath / runtimeLogPath are tail-included in the diagnostics bundle
	// (DX-S-03). Empty means "not available, skip".
	auditLogPath   string
	runtimeLogPath string
	mu             sync.RWMutex

	// baseCtx is cancelled at Shutdown. The reaper loops on it, and the IPC
	// router roots each proxy session's parent context in it so that on graceful
	// shutdown every active proxy pump sees ctx.Done() and tears down — instead
	// of the http.Server waiting out the full grace on hijacked WS conns (REL-02).
	baseCtx    context.Context
	cancelBase context.CancelFunc
	// reaperDone is closed when the reaper goroutine exits, so Shutdown can wait
	// for it deterministically.
	reaperDone chan struct{}
	// idleTTL / reapEvery are fields (defaulted from the consts) only so tests
	// can shrink them to keep the suite fast.
	idleTTL   time.Duration
	reapEvery time.Duration

	// loadErr is the aggregate error from the most recent LoadAll, surfaced by
	// the readiness endpoint (OBS-01). Guarded by mu. nil once all plugins load
	// cleanly. Stored separately from the returned error so readiness can report
	// it after LoadAll's caller has handled the return value.
	loadErr error
}

type Options struct {
	Dir       string
	Logger    *slog.Logger
	Keystore  keystore.Store
	Telemetry *telemetry.Tracer
	NetState  *netstate.Observer
	Config    *config.Manager
	// AuditLogPath / RuntimeLogPath are the runtime-owned log files tail-included
	// in the diagnostics bundle (DX-S-03) so a runtime-reachable bundle is not
	// logless. Empty paths are skipped (e.g. RuntimeLogPath is empty when logs go
	// to stderr; audit is empty when audit is disabled).
	AuditLogPath   string
	RuntimeLogPath string
}

func NewHost(opts Options) (*Host, error) {
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	baseCtx, cancelBase := context.WithCancel(context.Background())
	h := &Host{
		dir:            opts.Dir,
		log:            opts.Logger.With("component", "plugins"),
		plugins:        make(map[string]Plugin),
		sessions:       make(map[string]*Session),
		keystore:       opts.Keystore,
		tel:            opts.Telemetry,
		netstate:       opts.NetState,
		config:         opts.Config,
		auditLogPath:   opts.AuditLogPath,
		runtimeLogPath: opts.RuntimeLogPath,
		baseCtx:        baseCtx,
		cancelBase:     cancelBase,
		reaperDone:     make(chan struct{}),
		idleTTL:        sessionIdleTTL,
		reapEvery:      reaperInterval,
	}
	go h.reap()
	return h, nil
}

// BaseContext is the host's shutdown-rooted context: cancelled when Shutdown is
// called. The IPC router roots each proxy session's parent in it so a graceful
// shutdown promptly cancels active proxy pumps instead of waiting out the full
// grace on hijacked WS connections (REL-02).
func (h *Host) BaseContext() context.Context { return h.baseCtx }

// BeginShutdown cancels the base context to signal active proxy sessions and
// the reaper to stop, WITHOUT yet closing plugins/sessions. main.go calls this
// before srv.Shutdown so hijacked WS pumps drain first — otherwise the server's
// graceful shutdown waits out the full grace on live consoles. Idempotent;
// Shutdown also calls cancelBase (safe to call twice). (REL-02)
func (h *Host) BeginShutdown() { h.cancelBase() }

// reap periodically closes sessions that have no attached renderer stream and
// have been idle past the TTL — an abandoned renderer (crashed / closed the tab
// without DELETE /sessions) would otherwise leak its upstream console session
// indefinitely (AHV-03). Stops when baseCtx is cancelled (Shutdown).
func (h *Host) reap() {
	defer close(h.reaperDone)
	t := time.NewTicker(h.reapEvery)
	defer t.Stop()
	for {
		select {
		case <-h.baseCtx.Done():
			return
		case <-t.C:
			h.reapOnce(time.Now())
		}
	}
}

// reapOnce closes every session with no attached stream whose last proxied
// frame is older than the idle TTL. Split out from reap() so a test can drive a
// single deterministic pass. CloseSession is called WITHOUT holding h.mu (it
// takes the lock itself), so we snapshot the reap set under a read lock first.
func (h *Host) reapOnce(now time.Time) {
	cutoff := now.Add(-h.idleTTL).UnixNano()
	var dead []string
	h.mu.RLock()
	for id, s := range h.sessions {
		if !s.StreamAttached() && s.LastSeen() < cutoff {
			dead = append(dead, id)
		}
	}
	h.mu.RUnlock()
	for _, id := range dead {
		h.reapIfStillIdle(h.baseCtx, id, cutoff)
	}
}

// reapIfStillIdle closes a session ONLY if it is STILL stream-less and idle,
// re-checked under the lock ATOMICALLY with the delete. The reap set was
// snapshotted lock-free, so a stream that re-attached in the gap (a laptop wake
// / VPN flap resuming the SAME session past its idle TTL) must NOT be reaped out
// from under the live reconnect — GetSession would 404 it and the console the
// operator just resumed would die (#14). Deliberate close stays on CloseSession,
// which intentionally closes even an attached session (the stream is tearing down).
func (h *Host) reapIfStillIdle(ctx context.Context, id string, cutoff int64) {
	h.mu.Lock()
	s, ok := h.sessions[id]
	if !ok {
		h.mu.Unlock()
		return
	}
	if s.StreamAttached() || s.LastSeen() >= cutoff {
		h.mu.Unlock() // re-attached / active since the snapshot — leave it alone
		return
	}
	delete(h.sessions, id)
	h.mu.Unlock()
	h.log.Info("reaping idle session with no attached stream", "session", id)
	s.MarkClosed()
	if err := s.plugin.CloseSession(ctx, id); err != nil {
		h.log.Warn("reap close failed", "session", id, "err", err)
	}
}

// signalSessionsExpired drives the named sessions' consoles into the re-auth arm
// (SH2). A plugin calls this (via Dependencies.OnSessionsExpired) the moment an
// endpoint's credentials go invalid, so the proxy can emit session_expired
// BEFORE the live stream drops. Best-effort: unknown sessions are skipped, and
// the session is NOT closed — a re-auth refreshes the endpoint cookies and the
// SAME session reconnects.
func (h *Host) signalSessionsExpired(sessionIDs []string) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	for _, id := range sessionIDs {
		if s, ok := h.sessions[id]; ok {
			s.SignalReauth()
		}
	}
}

func (h *Host) LoadAll() error {
	builders := sdk.Registered()
	var loadErrors []error
	for _, mk := range builders {
		p := mk()
		m := p.Manifest()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		err := p.Init(ctx, Dependencies{
			Logger:   newSlogAdapter(h.log.With("plugin", m.ID)),
			Keystore: newKeystoreAdapter(h.keystore, m.ID),
			// NetState is intentionally nil: the netstate event-subscription
			// machinery was removed (no plugin consumed it; the send-path raced a
			// closed channel — GO-01/GO-R-05). The Observer still serves Snapshot()
			// for diagnostics; it just no longer feeds plugin events.
			NetState:  nil,
			Telemetry: newTelemetryAdapter(h.tel, m.ID),
			Config:    newConfigAdapter(h.config),
			Version:   build.Version,
			// Proactive re-auth (SH2): the plugin's keep-alive calls this with the
			// sessions whose credentials just went invalid; we signal each so its
			// proxy emits session_expired before the live stream drops.
			OnSessionsExpired: h.signalSessionsExpired,
		})
		cancel()
		if err != nil {
			loadErrors = append(loadErrors, fmt.Errorf("plugin %s init: %w", m.ID, err))
			continue
		}
		h.mu.Lock()
		h.plugins[m.ID] = p
		h.mu.Unlock()
		h.log.Info("plugin loaded", "id", m.ID, "version", m.Version)
	}
	var joined error
	if len(loadErrors) > 0 {
		joined = errors.Join(loadErrors...)
	}
	h.mu.Lock()
	h.loadErr = joined
	h.mu.Unlock()
	return joined
}

// Readiness reports the host's plugin-load state for the readiness endpoint
// (OBS-01): how many plugins are currently loaded and the last aggregate load
// error (empty when all plugins loaded cleanly). Distinct from liveness — the
// process can be alive (serving /healthz) while no plugin has loaded yet.
func (h *Host) Readiness() (pluginsLoaded int, lastLoadErr string) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	if h.loadErr != nil {
		lastLoadErr = h.loadErr.Error()
	}
	return len(h.plugins), lastLoadErr
}

// ActiveSessions returns the number of currently-tracked console sessions, for
// the /metrics gauge (OBS-01).
func (h *Host) ActiveSessions() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.sessions)
}

func (h *Host) Shutdown(ctx context.Context) {
	// Cancel the base context FIRST: this stops the reaper and signals every
	// active proxy session (rooted in BaseContext via the router) to tear down,
	// so the http.Server's graceful shutdown doesn't wait out the full grace on
	// hijacked WS conns (REL-02). Then wait (bounded by ctx) for the reaper.
	h.cancelBase()
	select {
	case <-h.reaperDone:
	case <-ctx.Done():
	}

	h.mu.Lock()
	defer h.mu.Unlock()
	for sid, s := range h.sessions {
		s.MarkClosed()
		_ = s.plugin.CloseSession(ctx, sid)
		delete(h.sessions, sid)
	}
	for id, p := range h.plugins {
		if err := p.Shutdown(ctx); err != nil {
			h.log.Warn("plugin shutdown error", "plugin", id, "err", err)
		}
	}
}

func (h *Host) Get(id string) (Plugin, bool) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	p, ok := h.plugins[id]
	return p, ok
}

func (h *Host) Summaries() []map[string]any {
	h.mu.RLock()
	defer h.mu.RUnlock()
	out := make([]map[string]any, 0, len(h.plugins))
	for _, p := range h.plugins {
		m := p.Manifest()
		out = append(out, map[string]any{
			"id":           m.ID,
			"name":         m.Name,
			"version":      m.Version,
			"description":  m.Description,
			"vendor":       m.Vendor,
			"capabilities": m.Capabilities,
		})
	}
	return out
}

func (h *Host) OpenSession(ctx context.Context, p Plugin, resourceID string) (SessionInfo, error) {
	// Control-plane span (OBS-01): no-op when telemetry is disabled.
	ctx, finish := h.tel.StartSpan(ctx, "plugins.OpenSession",
		attribute.String("plugin_id", p.Manifest().ID),
		attribute.String("resource_id", resourceID),
	)
	span := trace.SpanFromContext(ctx)

	// Enforce the concurrent-session cap before reaching out to the plugin so a
	// runaway open loop can't exhaust upstream console slots (AHV-03).
	h.mu.RLock()
	n := len(h.sessions)
	h.mu.RUnlock()
	if n >= maxConcurrentSessions {
		finish(ErrTooManySessions)
		return SessionInfo{}, ErrTooManySessions
	}

	info, err := p.OpenSession(ctx, resourceID)
	if err != nil {
		finish(err)
		return SessionInfo{}, err
	}
	if info.ID == "" {
		info.ID = uuid.NewString()
	}
	if info.OpenedAt.IsZero() {
		info.OpenedAt = time.Now().UTC()
	}
	if info.PluginID == "" {
		info.PluginID = p.Manifest().ID
	}
	if info.ResourceID == "" {
		info.ResourceID = resourceID
	}
	sess := &Session{
		ID:         info.ID,
		PluginID:   info.PluginID,
		ResourceID: info.ResourceID,
		Metadata:   info.Metadata,
		plugin:     p,
		reauthCh:   make(chan struct{}),
		Stats: SessionStats{
			OpenedAt: info.OpenedAt,
		},
	}
	// Seed lastSeen so a freshly-opened session (the renderer hasn't attached
	// its stream yet) gets the full idle TTL grace before the reaper considers
	// it abandoned, rather than being reaped at the next tick.
	sess.Touch()
	h.mu.Lock()
	h.sessions[info.ID] = sess
	h.mu.Unlock()
	span.SetAttributes(attribute.String("session_id", info.ID))
	finish(nil)
	return info, nil
}

func (h *Host) GetSession(id string) (*Session, bool) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	s, ok := h.sessions[id]
	return s, ok
}

func (h *Host) Sessions() []SessionInfo {
	h.mu.RLock()
	defer h.mu.RUnlock()
	out := make([]SessionInfo, 0, len(h.sessions))
	for _, s := range h.sessions {
		out = append(out, SessionInfo{
			ID:         s.ID,
			PluginID:   s.PluginID,
			ResourceID: s.ResourceID,
			OpenedAt:   s.Stats.OpenedAt,
			Metadata:   s.Metadata,
		})
	}
	return out
}

func (h *Host) CloseSession(ctx context.Context, id string) error {
	// Control-plane span (OBS-01): no-op when telemetry is disabled.
	ctx, finish := h.tel.StartSpan(ctx, "plugins.CloseSession",
		attribute.String("session_id", id),
	)
	h.mu.Lock()
	s, ok := h.sessions[id]
	if ok {
		delete(h.sessions, id)
	}
	h.mu.Unlock()
	if !ok {
		err := errors.New("session not found")
		finish(err)
		return err
	}
	trace.SpanFromContext(ctx).SetAttributes(attribute.String("plugin_id", s.PluginID))
	s.MarkClosed()
	err := s.plugin.CloseSession(ctx, id)
	finish(err)
	return err
}

// WriteDiagnostics emits a redacted zip bundle.
func (h *Host) WriteDiagnostics(w io.Writer) error {
	zw := zip.NewWriter(w)
	defer func() { _ = zw.Close() }()

	add := func(name string, payload any) error {
		f, err := zw.Create(name)
		if err != nil {
			return err
		}
		enc := json.NewEncoder(f)
		enc.SetIndent("", "  ")
		return enc.Encode(payload)
	}

	if err := add("runtime-state.json", map[string]any{
		"sessions": h.Sessions(),
		"plugins":  h.Summaries(),
		"network":  h.netstate.Snapshot(),
		"time":     time.Now().UTC(),
	}); err != nil {
		return err
	}

	cfg := h.config.Get()
	cfg = redactConfig(cfg)
	if err := add("config.json", cfg); err != nil {
		return err
	}

	// DX-S-03: tail-include the runtime-owned logs so a runtime-reachable
	// diagnostics bundle isn't logless. Best-effort — a missing/unreadable log
	// is recorded as a note in the bundle rather than failing the whole export
	// (an operator gathering diagnostics during an incident still gets the rest
	// of the bundle).
	addLogTail := func(name, path string) error {
		if path == "" {
			return nil
		}
		f, err := zw.Create(name)
		if err != nil {
			return err
		}
		data, rerr := tailFile(path, diagnosticsLogTailBytes)
		if rerr != nil {
			_, _ = io.WriteString(f, "diagnostics: could not read "+name+": "+rerr.Error()+"\n")
			return nil
		}
		_, werr := f.Write(data)
		return werr
	}
	if err := addLogTail("audit.log", h.auditLogPath); err != nil {
		return err
	}
	if err := addLogTail("runtime.log", h.runtimeLogPath); err != nil {
		return err
	}
	return nil
}

// diagnosticsLogTailBytes bounds how much of each log we copy into the bundle
// so a long-running runtime's multi-MB logs don't bloat the export — the recent
// tail is what's diagnostic for an incident.
const diagnosticsLogTailBytes = 256 * 1024

// tailFile returns up to the last maxBytes of the file at path. If the file is
// larger than maxBytes the returned slice begins at a record boundary (the byte
// after the first newline in the window) so the bundle doesn't open on a
// half-truncated JSONL line.
func tailFile(path string, maxBytes int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	size := info.Size()
	if size <= maxBytes {
		return io.ReadAll(f)
	}
	if _, err := f.Seek(size-maxBytes, io.SeekStart); err != nil {
		return nil, err
	}
	data, err := io.ReadAll(f)
	if err != nil {
		return nil, err
	}
	if i := bytes.IndexByte(data, '\n'); i >= 0 && i+1 < len(data) {
		data = data[i+1:]
	}
	return data, nil
}

func redactConfig(c config.Config) config.Config {
	c.Security.CustomCAFiles = nil
	for i := range c.Endpoints {
		c.Endpoints[i].PinnedCertB64 = ""
	}
	return c
}
