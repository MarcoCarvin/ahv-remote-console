// Package proxy bridges the Electron renderer to a plugin's upstream
// WebSocket endpoint. It is the resilience-critical component of the runtime.
//
// Properties:
//
//   - Renderer ↔ runtime WS uses the standard gorilla upgrade with permessage-
//     deflate disabled (low-latency console traffic).
//   - Upstream WS dial uses TLS 1.3 with the plugin-provided tls.Config (which
//     enables certificate pinning and trust-store overrides).
//   - Heartbeat: PING every 15s with a 30s pong-deadline. Stalled framebuffer
//     traffic triggers the watchdog independently of TCP keep-alive.
//   - Reconnect: on upstream failure, the proxy attempts a session-aware
//     resumption via plugin.ResumeSession. If unsupported, it opens a fresh
//     stream and emits a RFB-Reset control frame to the renderer so it can
//     redraw.
//   - RFB-aware clipboard: injects Extended Clipboard encoding into
//     SetEncodings, translates Extended Clipboard ServerCutText (negative
//     length, zlib PROVIDE) to legacy ServerCutText for the renderer.
//     ClientCutText flows through unmodified — QEMU routes it to the
//     vdagent chardev on the guest serial port.
package proxy

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/binary"
	"encoding/json"
	"errors"
	"log/slog"
	"math/rand/v2"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"

	"github.com/MarcoCarvin/ahv-remote-console/apps/runtime/internal/plugins"
	"github.com/MarcoCarvin/ahv-remote-console/apps/runtime/internal/rfb"
	"github.com/MarcoCarvin/ahv-remote-console/apps/runtime/internal/telemetry"
)

const (
	heartbeatInterval = 15 * time.Second
	pongDeadline      = 30 * time.Second
	writeDeadline     = 15 * time.Second
	maxBackoff        = 30 * time.Second
	minBackoff        = 500 * time.Millisecond
	// Renderer→proxy direction: RFB client messages (key / pointer / cut-text)
	// are small, so an 8 MiB cap is ample and bounds a misbehaving local sender.
	maxMessageSize = 8 * 1024 * 1024
	// Upstream (Prism/QEMU)→proxy direction: must cover a FULL raw framebuffer
	// at the largest display the renderer allows (the 7680×4320 / 8K clamp in
	// applyResize): 7680*4320*4 ≈ 133 MB, plus header / cursor / multi-rect
	// headroom. The old shared 8 MiB cap sat right on a 1920×1080 raw frame
	// (8.29 MB), so the most common resolution tripped gorilla's read limit
	// (close 1009); the proxy then re-dialed + re-handshaked into the SAME
	// oversized frame — an infinite reconnect loop that never self-healed. Sized
	// so NO legitimate frame at any supported resolution is ever rejected; Tight
	// (the negotiated encoding) keeps real messages tiny, so this ceiling only
	// matters for a raw fallback, and a frame beyond it is handled gracefully
	// (see upstreamToClient → frame_too_large).
	maxUpstreamMessageSize = 160 * 1024 * 1024
	// Shrink threshold for the upstream pump's reused read buffer: capacity
	// above this is dropped for GC before the next message is read. Set ABOVE
	// the largest routine full-frame Tight update so steady-state streaming —
	// including the full-frame redraw burst after a reconnect — never regrows
	// the buffer, while bounding worst-case retention after a rare oversized
	// (raw-fallback) frame to one buffer, not 160MB held for the session.
	upstreamReadBufShrinkCap = 8 * 1024 * 1024
)

// Keep-warm lease side-channel. QEMU's single adaptive vnc_refresh timer backs
// off 30ms -> 3000ms on an idle framebuffer, so the first frame after idle is
// seconds stale; while a console stream is ATTACHED the in-guest agent keeps
// the display minimally dirty (1-LSB pixel toggle) under a TTL lease the proxy
// arms and renews over the clipboard side-channel — the same channel as the
// __AHVRC_RESIZE__ tokens, with the same rule: these are magic commands, never
// operator clipboard data. When the stream detaches (or the proxy vanishes and
// the TTL lapses) the guest is left completely alone.
const (
	// keepwarmProbeToken is the version-skew-safe capability probe, sent
	// INSIDE the resize namespace: every shipped agent intercepts
	// __AHVRC_RESIZE__ BEFORE its guest clipboard write and swallows an
	// unparseable payload (handleResizeCommand warns and returns nil), so
	// probing a STALE agent pollutes nothing. A NEW agent answers with
	// keepwarmAckText; only after that ack do real lease tokens go out.
	keepwarmProbeToken = "__AHVRC_RESIZE__keepwarm-probe" // #nosec G101 -- clipboard keep-warm protocol marker, not a credential
	// keepwarmAckText is the agent's guest->host ack to the probe, riding an
	// ordinary clipboard transfer. The proxy swallows it (never forwarded to
	// the renderer, never written to the host clipboard, content never logged).
	keepwarmAckText = "__AHVRC_KEEPWARM_ACK__" // #nosec G101 -- clipboard keep-warm protocol marker, not a credential
	// keepwarmTokenPrefix is the lease/cancel namespace (distinct from the
	// ack, which continues "_ACK__" after "KEEPWARM"). Used to recognize a
	// SIBLING console's tokens echoed back by QEMU's clipboard manager.
	keepwarmTokenPrefix = "__AHVRC_KEEPWARM__" // #nosec G101 -- clipboard keep-warm protocol marker, not a credential
	// keepwarmLeaseToken arms/extends the agent's lease for 45 seconds — the
	// agent-side backstop if a cancel or renewal never arrives.
	keepwarmLeaseToken = keepwarmTokenPrefix + "45"
	// keepwarmCancelToken disarms the lease immediately on stream detach.
	keepwarmCancelToken = keepwarmTokenPrefix + "0"
	// keepwarmRenewInterval paces LEASE renewals once the agent has acked; well
	// inside the 45s TTL so several Condition-A deferrals never let a live lease
	// lapse.
	keepwarmRenewInterval = 20 * time.Second
	// keepwarmColdInterval paces PROBES before the agent acks. On a cold attach
	// the agent's vdagent backend isn't live until the console-following libvirt
	// hook swaps COM3 (~3s debounce) and the agent re-negotiates caps (~5s), so
	// the first probe (on the first clip tick) is often sent before the agent can
	// hear it. Retrying at the renew interval (20s) left up to ~20s of unopposed
	// QEMU vnc_refresh idle-backoff after resume; probing fast until acked pulls
	// keep-warm engagement down to the agent's ~8s floor. Probes are
	// resize-namespace and harmlessly swallowed by a stale agent, so the extra
	// cold sends cost only a few log lines on a genuinely-unsupported guest.
	keepwarmColdInterval = 2 * time.Second
	// keepwarmClipQuiet defers keep-warm sends while REAL clipboard traffic is
	// this fresh — the side-channel must never starve an operator copy (the
	// regression class that killed the per-SetDesktopSize resize use of this
	// channel).
	keepwarmClipQuiet = 3 * time.Second
	// keepwarmMaxProbes bounds unanswered probes per upstream connection. At the
	// cold cadence this spans ~20s — comfortably past the agent's ~8s engagement
	// floor — then gives up (a stale agent logs at most this many "malformed
	// resize command" warns, and we stop probing sooner in wall-clock than the
	// old 3x20s).
	keepwarmMaxProbes = 10
)

// upstreamReadDeadline is the rolling read deadline armed on the UPSTREAM Prism
// VNC conn — the upstream half-open watchdog. It mirrors the client-side
// pongDeadline pattern (heartbeat() arms it on the client at :997/:1000): once
// the upstream has proven it pongs (upstreamPongSeen), every successful upstream
// read or pong re-arms it, so a half-open upstream that goes silent trips a read
// error within this window and run() re-dials, instead of freezing the console
// forever while the UI still shows "connected". Gating on a seen pong means an
// upstream that never pongs does NOT spuriously reconnect a healthy idle VM. It
// is a var (not a const) only so a test can shrink it to keep the suite fast.
var upstreamReadDeadline = pongDeadline

var upgrader = websocket.Upgrader{
	ReadBufferSize:  1024 * 1024,
	WriteBufferSize: 1024 * 1024,
	CheckOrigin: func(r *http.Request) bool {
		return originIsLocal(r.Header.Get("Origin"))
	},
	Subprotocols:      []string{"hc.console.v1"},
	EnableCompression: false,
}

// originIsLocal reports whether a WebSocket upgrade's Origin is acceptable.
// The upgrade is already HMAC-authenticated (a forged cross-origin request
// can't sign), so this is defense-in-depth against a malicious website
// attempting DNS-rebinding/cross-origin access to the loopback runtime: we
// reject only genuinely-remote http(s) origins. Empty/null origins (native
// WS clients, opaque file:// pages) and non-http(s) schemes (the packaged
// Electron shell's file://, app://, or custom scheme) are allowed so we
// never break the legitimate renderer.
func originIsLocal(origin string) bool {
	if origin == "" || origin == "null" {
		return true
	}
	rest, ok := strings.CutPrefix(origin, "http://")
	if !ok {
		rest, ok = strings.CutPrefix(origin, "https://")
	}
	if !ok {
		return true // non-http(s) scheme → desktop shell
	}
	if i := strings.IndexByte(rest, '/'); i >= 0 {
		rest = rest[:i]
	}
	host := rest
	if strings.HasPrefix(host, "[") {
		// Bracketed IPv6, e.g. "[::1]" or "[::1]:8080" — keep through the ']'.
		if end := strings.IndexByte(host, ']'); end >= 0 {
			host = host[:end+1]
		}
	} else if i := strings.LastIndexByte(host, ':'); i >= 0 {
		host = host[:i]
	}
	return host == "127.0.0.1" || host == "localhost" || host == "[::1]" || host == "::1"
}

// RoleChecker returns the session's presence role. If the session has
// no presence entry (single-viewer mode), it returns "controller".
type RoleChecker func(sessionID string) string

// SpanFinish ends a span; err annotates a failure. Matches sdk.SpanFinish.
type SpanFinish func(err error)

// Tracer is the minimal tracing surface the proxy needs for OBS-01. It mirrors
// sdk.Telemetry.StartSpan so the runtime can pass its telemetry adapter
// directly. The proxy depends on this local interface (not the SDK type) to
// avoid widening the package's import surface; a *telemetry.Tracer-backed
// adapter satisfies it.
type Tracer interface {
	StartSpan(ctx context.Context, name string) (context.Context, SpanFinish)
}

// noopTracer is the default when no tracer is plumbed in — StartSpan is a
// no-op so the proxy's span call sites are always safe to invoke. Replaced by
// the real telemetry adapter once runtime-ctl threads it through (see OBS-01
// note); until then these spans simply do nothing rather than nil-panic.
type noopTracer struct{}

func (noopTracer) StartSpan(ctx context.Context, _ string) (context.Context, SpanFinish) {
	return ctx, func(error) {}
}

// HandleStream upgrades the request and runs a bidirectional proxy.
func HandleStream(w http.ResponseWriter, r *http.Request, sess *plugins.Session, log *slog.Logger) {
	HandleStreamWithRole(w, r, sess, log, nil)
}

// HandleStreamWithRole is like HandleStream but accepts a role checker
// for input filtering.
func HandleStreamWithRole(w http.ResponseWriter, r *http.Request, sess *plugins.Session, log *slog.Logger, roleCheck RoleChecker) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Warn("client upgrade failed", "err", err, "session", sess.ID)
		return
	}
	defer func() { _ = conn.Close() }()
	conn.SetReadLimit(maxMessageSize)
	if nc := conn.NetConn(); nc != nil {
		ensureNoDelay(nc, log)
		setDSCPEF(nc, log)
	}

	clog := log.With("session", sess.ID, "plugin", sess.PluginID)
	clog.Info("client connected")

	// Bracket this renderer stream's lifetime so the host reaper can tell an
	// actively-proxied session from an abandoned one (AHV-03).
	sess.StreamConnected()
	defer sess.StreamDisconnected()

	// Re-arm the session's re-auth signal for this fresh attach. The session is
	// kept alive across an expiry (so the SAME id reconnects after an in-console
	// re-auth), and the signal is closed on expiry; without re-arming, this new
	// proxy's watcher would observe the stale closed channel and emit
	// session_expired immediately — wedging the console in a re-auth loop even
	// with valid refreshed credentials. Done once per WS attach (this is the
	// single upgrade entry point), NOT per reconnect iteration: a genuine
	// mid-stream credential lapse re-closes the armed channel and still drives
	// SH2 correctly.
	sess.ClearReauth()

	stream := rfb.NewStreamProcessor()

	p := &proxy{
		log:       clog,
		sess:      sess,
		client:    conn,
		stream:    stream,
		roleCheck: roleCheck,
	}
	p.openUpstream = p.sess.OpenStream

	// Clipboard flows through the vdagent serial channel (COM3).
	// The proxy's role is limited to:
	//   1. Injecting Extended Clipboard encoding into SetEncodings
	//   2. Translating Extended Clipboard ServerCutText to legacy format

	p.run(r.Context())
	clog.Info("client disconnected")
}

type proxy struct {
	log        *slog.Logger
	sess       *plugins.Session
	client     *websocket.Conn
	upstream   atomic.Pointer[websocket.Conn]
	wg         sync.WaitGroup
	hs         rfb.HandshakeTracker
	stream     *rfb.StreamProcessor
	clientMu   sync.Mutex
	upstreamMu sync.Mutex
	roleCheck  RoleChecker

	// tracer instruments the proxy's hot paths (dial / handshake / reconnect)
	// for OBS-01. May be nil when constructed directly (tests, or before
	// runtime-ctl threads the real telemetry adapter through HandleStream) —
	// always access via p.tr() which substitutes a no-op.
	tracer Tracer

	// Extended Clipboard REQUEST tracking: deadline after which we
	// re-send REQUEST if PROVIDE hasn't arrived. Zero means no pending.
	// clipReqAttempts counts re-sends so we give up (and tell the renderer)
	// instead of waiting forever on a dropped PROVIDE.
	clipReqDeadline atomic.Int64 // unix-nano
	clipReqAttempts atomic.Int32

	// expectStaleReannounce is armed the instant we (re-)advertise ext-clip CAPS
	// upstream — the ONLY thing that makes QEMU enable ext-clip and then re-announce
	// its STALE last-writer-wins clipboard to the freshly-attached client. The very
	// first NOTIFY after that CAPS event is that stale re-announce; the guest agent
	// has no fresh data to answer, so REQUESTing + retrying it (3×) hammers QEMU's
	// single clipboard slot and starves real operator pastes (521 "guest copy lost"
	// in the field). We skip exactly that ONE NOTIFY, then flow normally.
	//
	// Anchoring on the CAPS *event* (not a wall-clock timer, and not only the
	// reconnect branch) is the fix for the earlier dead guard: a fresh proxy struct
	// is created per WS upgrade with first=true — "resumed" never fires in the field
	// — so a reconnect-branch-only, timer-based guard never armed. CAPS re-advertise
	// happens on EVERY handshake, so this fires on the dominant fresh reopens too.
	// It suppresses ONE NOTIFY per attach — never a real copy's (the 2nd+ NOTIFY
	// flows), never a global mute — so it cannot re-introduce the removed circuit
	// breaker's blackout.
	expectStaleReannounce atomic.Bool

	// upstreamRTTMicros is the last measured round-trip to the UPSTREAM Prism
	// VNC WebSocket (microseconds) — the meaningful network latency to the
	// cluster, not the ~0 loopback to the renderer. Measured via WS ping/pong
	// (heartbeat() pings, dialUpstream's pong handler records) and surfaced to
	// the renderer as latency_ms. 0 = not yet measured.
	upstreamRTTMicros atomic.Int64

	// upstreamPongSeen reports whether THIS upstream connection has answered a
	// WS ping with a pong. The half-open read-deadline watchdog (REL-01) only
	// arms once this is true: a Prism/QEMU upstream that never pongs would
	// otherwise have its deadline fire on a perfectly healthy but IDLE VM (no
	// framebuffer reads to re-arm it) → a spurious reconnect loop. Reset to false
	// on every (re)dial so each connection independently proves it pongs before
	// the watchdog arms; if it never pongs we keep the original no-deadline
	// blocking read (half-open freeze possible, but no false reconnect churn).
	upstreamPongSeen atomic.Bool

	// openUpstream dials a fresh upstream WS. Defaults to sess.OpenStream;
	// overridable in tests to inject a controllable fake upstream.
	openUpstream func(context.Context) (*websocket.Conn, error)
	// clientClosed is set by a pump when the CLIENT (renderer) ended the
	// session — a terminal condition the run loop must NOT try to reconnect.
	// An upstream failure leaves it false → the loop re-dials + re-handshakes.
	clientClosed atomic.Bool
	// reauthRequested is set when the session's credentials were invalidated
	// out-of-band (Session.ReauthCh fired — the plugin keep-alive saw a 401).
	// The run loop then emits a terminal session_expired so the console enters
	// the re-auth arm proactively, before the live stream drops (SH2).
	reauthRequested atomic.Bool

	// extClipCapsSent guards the proactive Extended Clipboard CAPS handshake
	// (PROT-05): once the client's SetEncodings has been injected with the
	// ext-clip pseudo-encoding, we advertise our clipboard capabilities upstream
	// ONCE rather than waiting for QEMU to send CAPS first (it may never). Reset
	// on reconnect so a fresh upstream re-negotiates.
	extClipCapsSent atomic.Bool

	// Keep-warm lease sender state (see the keepwarm* consts). keepwarmAcked
	// latches once the agent answers the probe — only then do real lease
	// tokens go upstream (version-skew safety; it survives reconnects, the
	// agent doesn't change mid-session). lastClipActivity timestamps the last
	// REAL clipboard traffic in either direction (Condition A deferral);
	// the mid-dance hard-block reuses clipReqDeadline above.
	keepwarmAcked    atomic.Bool
	keepwarmNextSend atomic.Int64 // unix-nano of the next allowed send; 0 = now
	keepwarmProbes   atomic.Int32
	lastClipActivity atomic.Int64 // unix-nano

	// (pendingResize was removed — see comment in handleClientBinary.
	// The renderer no longer auto-requests SetDesktopSize because AHV
	// guests have a driver-pinned resolution we can't change.)
}

type ctrlFrame struct {
	Type string `json:"type"`
	// Cause is a stable, machine-readable classification on a terminal
	// "fatal" frame (session_expired | rbac_denied | unreachable) so the
	// renderer can offer the right one-click recovery rather than guessing
	// from a bare socket close.
	Cause    string `json:"cause,omitempty"`
	Reason   string `json:"reason,omitempty"`
	Latency  int64  `json:"latency_ms,omitempty"`
	BytesIn  uint64 `json:"bytes_in,omitempty"`
	BytesOut uint64 `json:"bytes_out,omitempty"`
	// Attempt/Max annotate a reconnect_attempt frame so the renderer can show
	// "reconnecting — attempt N of M" instead of an unbounded spinner.
	Attempt int `json:"attempt,omitempty"`
	Max     int `json:"max,omitempty"`
}

// handshakeDeadline bounds a (re)handshake so a renderer that fails to
// re-handshake after an rfb_reset cannot hang the proxy forever.
const handshakeDeadline = 30 * time.Second

// maxReconnectFailures bounds consecutive dial/handshake failures before we
// give up and close the client (matches the old reconnect()'s 60-attempt cap).
const maxReconnectFailures = 60

// poweredOffPollInterval is how often we re-dial when the upstream reports the
// VM is powered OFF. A powered-off VM is NOT a failure — it's an expected
// state (e.g. the operator powered it down to add a serial port), so we poll
// indefinitely (bounded only by the client staying connected) without counting
// it toward maxReconnectFailures, and surface a calm "VM is powered off" state
// to the renderer instead of the alarming reconnect spinner.
const poweredOffPollInterval = 3 * time.Second

// isPoweredOffErr reports whether a dial error is the VM-powered-off condition
// (the Prism VNC proxy returns "...: VM is not powered on"). String-matched
// because that's all the upstream gives us — same approach as the
// "session expired" check below.
func isPoweredOffErr(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "not powered on") || strings.Contains(s, "powered off")
}

// isStartingErr reports whether a dial error is the transient "VM is powered on
// but its console is not serving yet" condition — an upstream HTTP 5xx during
// the window after power-on while the guest / QEMU VNC comes up. Like
// isPoweredOffErr this is a string match on the error text (all the upstream
// rejection gives us — see plugin OpenStream, which formats "dial VNC
// websocket: HTTP <status>"). Unlike a 4xx, it is self-resolving, so the run
// loop surfaces a calm "VM is starting" state and keeps retrying rather than
// flapping an alarming reconnect or giving up.
func isStartingErr(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "http 500") ||
		strings.Contains(s, "http 502") ||
		strings.Contains(s, "http 503") ||
		strings.Contains(s, "http 504")
}

// classifyFatal maps a dial/handshake error to a TERMINAL cause the renderer
// can act on, plus the WS close code to send. Returns ("", 0) for transient
// errors that are worth retrying. Terminal causes fast-fail on BOTH the first
// dial and a mid-session reconnect, so a dead session / deleted VM / bad pin
// surfaces immediately with the right recovery affordance instead of burning
// the full reconnect budget (~tens of minutes) into a generic "unreachable".
//
// The strings are the STABLE causes the AHV plugin emits (its OpenStream
// power-state preflight + credential paths) and the runtime session layer's own
// sentinels — never the (credential-reflecting) upstream body. Same string-match
// approach the codebase already uses for isPoweredOffErr.
func classifyFatal(err error) (cause string, closeCode int) {
	if err == nil {
		return "", 0
	}
	s := strings.ToLower(err.Error())
	switch {
	case strings.Contains(s, "session expired"),
		strings.Contains(s, "no credentials available"),
		strings.Contains(s, "session cookies missing"):
		// Cookie is dead/absent — retrying just replays it. Route to the
		// in-console SSO re-auth (F5), not a futile Reconnect.
		return "session_expired", websocket.ClosePolicyViolation
	case strings.Contains(s, "not authorized"):
		return "rbac_denied", websocket.ClosePolicyViolation
	case strings.Contains(s, "vm not found"):
		// The VM was deleted (or vanished cross-cluster) — re-dialing it for
		// ~27 min is pointless; tell the operator it's gone.
		return "vm_not_found", websocket.ClosePolicyViolation
	case strings.Contains(s, "session closed"),
		strings.Contains(s, "unknown session"):
		// The runtime already tore the session down (operator closed the tab,
		// the reaper, or a DELETE) — exit cleanly instead of retrying a dead id.
		return "session_closed", websocket.ClosePolicyViolation
	case strings.Contains(s, "pinned spki"),
		strings.Contains(s, "tls verification disabled"):
		// DETERMINISTIC, fail-closed config faults the plugin emits (a pinned-SPKI
		// mismatch, or allow_insecure_tls without a pin) — retrying never clears
		// them, so they are terminal. We DELIBERATELY do NOT match the generic
		// Go-stack "x509"/"certificate" substrings here: those also fire for
		// TRANSIENT TLS conditions on a mid-session re-dial (an LB failover
		// presenting a not-yet-chaining backend cert, post-wake clock skew on a
		// non-pinned endpoint), which must stay RETRYABLE (fall through to the
		// bounded "unreachable" backoff) and recover on their own. Marking those
		// terminal stranded a once-connected console with no Reconnect affordance.
		return "config_error", websocket.ClosePolicyViolation
	default:
		return "", 0
	}
}

// closeFatal emits a typed terminal "fatal" control frame and a matching WS
// close frame before the socket goes away, so the renderer always learns WHY
// the console ended (and can offer the right recovery) instead of guessing at a
// bare close. Every give-up / fatal exit in run() routes through this.
func (p *proxy) closeFatal(cause, reason string, closeCode int) {
	p.sendCtrl(ctrlFrame{Type: "fatal", Cause: cause, Reason: reason})
	closeMsg := websocket.FormatCloseMessage(closeCode, truncateCloseReason(cause))
	p.clientMu.Lock()
	_ = p.client.SetWriteDeadline(time.Now().Add(writeDeadline))
	_ = p.client.WriteMessage(websocket.CloseMessage, closeMsg)
	p.clientMu.Unlock()
	time.Sleep(150 * time.Millisecond)
}

// tr returns the proxy's tracer, substituting a no-op when none was plumbed in
// (direct test construction, or before runtime-ctl wires the telemetry adapter
// through HandleStream — OBS-01). Span call sites can always call p.tr() safely.
func (p *proxy) tr() Tracer {
	if p.tracer == nil {
		return noopTracer{}
	}
	return p.tracer
}

// startSpan opens a span on the proxy's hot paths (OBS-01). The session_id is
// attached automatically; call-site attrs (attempt/reconnect/…) are appended.
//
// The shared sdk.Telemetry.StartSpan signature is (ctx, name) with no structured
// attribute argument, so — to instrument the call sites WITHOUT widening that
// cross-package interface (runtime-ctl owns it) — attributes are encoded into
// the span NAME as space-separated key=value pairs (e.g.
// "proxy.dial session_id=abc attempt=2 reconnect=true"). When runtime-ctl makes
// StartSpan emit real OTel spans, these names carry the dimensions; promoting
// them to first-class span attributes is the small follow-up plumbing noted
// under OBS-01 incomplete.
func (p *proxy) startSpan(ctx context.Context, name string, attrs ...string) (context.Context, SpanFinish) {
	var b strings.Builder
	b.WriteString(name)
	b.WriteString(" ")
	b.WriteString(attrStr("session_id", p.sess.ID))
	for _, a := range attrs {
		b.WriteString(" ")
		b.WriteString(a)
	}
	return p.tr().StartSpan(ctx, b.String())
}

func attrStr(k, v string) string     { return k + "=" + v }
func attrInt(k string, v int) string { return k + "=" + strconv.Itoa(v) }
func attrBool(k string, v bool) string {
	if v {
		return k + "=true"
	}
	return k + "=false"
}

// run drives the whole session as a reconnect LOOP: dial → (re)handshake →
// pumps → wait. On upstream failure the pumps exit (each unblocks its sibling
// via teardown()), wg.Wait returns, and we loop: a reconnect is a brand-new
// VNC connection, so we re-run the RFB handshake (resetting our tracker and
// telling the renderer to recreate its RFBClient via an rfb_reset control
// frame, which also re-injects Extended Clipboard when it re-sends
// SetEncodings). This replaces the old in-goroutine reconnect() that re-dialed
// WITHOUT re-handshaking and silently desynced the whole stream after a guest
// reboot.
func (p *proxy) run(parent context.Context) {
	defer func() {
		if u := p.upstream.Swap(nil); u != nil {
			// Stream detach: best-effort keep-warm cancel so the agent stops
			// dirtying the display now instead of waiting out the 45s TTL.
			p.sendKeepwarmCancel(u)
			_ = u.Close()
		}
	}()

	backoff := minBackoff
	first := true
	failures := 0

	for {
		if parent.Err() != nil {
			return
		}

		// OBS-01: span the dial hot path. attempt is the consecutive-failure
		// count (0 on a clean dial); reconnect=true once we're past the first
		// dial so a reconnect is distinguishable in traces from the initial one.
		dialCtx, finishDial := p.startSpan(parent, "proxy.dial",
			attrInt("attempt", failures),
			attrBool("reconnect", !first))
		dialErr := p.dialUpstream(dialCtx)
		finishDial(dialErr)
		if err := dialErr; err != nil {
			// VM powered off is an expected, recoverable state — not a failure.
			// Surface it calmly and poll for power-on indefinitely (whether on
			// the first dial or a mid-session reconnect), so the operator can
			// power the VM off (e.g. to add a serial port) and the console
			// resumes automatically when it comes back. Bounded by the client
			// staying connected — closing the tab cancels parent and exits.
			if isPoweredOffErr(err) {
				p.log.Info("upstream VM powered off — awaiting power-on", "err", err)
				p.sendCtrl(ctrlFrame{Type: "vm_powered_off"})
				failures = 0
				backoff = minBackoff
				select {
				case <-parent.Done():
					return
				case <-time.After(jitter(poweredOffPollInterval)):
				}
				continue
			}
			if isStartingErr(err) {
				// VM is powered on but its console isn't serving yet (just
				// booted / QEMU VNC still coming up). Self-resolving, like
				// powered-off — surface a calm "VM is starting" state and keep
				// retrying with backoff instead of an alarming reconnect or an
				// immediate fatal. Still bounded by maxReconnectFailures as a
				// generous (tens-of-minutes) backstop so a genuinely stuck
				// upstream eventually surfaces. Applies on the first dial too:
				// "power on, then open the console" must wait, not fail.
				p.log.Info("upstream console not ready yet — VM starting", "err", err, "backoff", backoff.String())
				p.sendCtrl(ctrlFrame{Type: "vm_starting"})
				failures++
				if failures >= maxReconnectFailures {
					p.closeFatal("unreachable", "VM console did not become ready", websocket.CloseInternalServerErr)
					return
				}
				select {
				case <-parent.Done():
					return
				case <-time.After(jitter(backoff)):
				}
				backoff *= 2
				if backoff > maxBackoff {
					backoff = maxBackoff
				}
				continue
			}
			if first {
				// Initial dial failure is fatal: classify the cause so the
				// renderer can offer the right recovery, then close the client.
				p.log.Warn("upstream dial failed", "err", err)
				cause, code := classifyFatal(err)
				if cause == "" {
					cause, code = "unreachable", websocket.CloseInternalServerErr
				}
				p.closeFatal(cause, err.Error(), code)
				return
			}
			// Terminal causes (expired/absent session, RBAC denial, deleted VM,
			// closed session, permanent TLS/config fault) cannot be fixed by
			// retrying — stop now and tell the renderer the specific cause,
			// instead of burning the retry budget on a dead session (which would
			// otherwise surface ~minutes later as a generic "unreachable").
			if cause, code := classifyFatal(err); cause != "" {
				p.log.Warn("reconnect aborted — fatal cause", "cause", cause, "err", err)
				p.closeFatal(cause, err.Error(), code)
				return
			}
			p.log.Warn("reconnect dial failed", "err", err, "backoff", backoff.String())
			p.sendCtrl(ctrlFrame{Type: "reconnect_attempt", Reason: err.Error(), Attempt: failures + 1, Max: maxReconnectFailures})
			telemetry.IncReconnectAttempt()
			failures++
			if failures >= maxReconnectFailures {
				p.log.Error("upstream unreachable after retries — closing client")
				p.closeFatal("unreachable", "could not reconnect after repeated attempts", websocket.CloseInternalServerErr)
				return
			}
			select {
			case <-parent.Done():
				return
			case <-time.After(jitter(backoff)):
			}
			backoff *= 2
			if backoff > maxBackoff {
				backoff = maxBackoff
			}
			continue
		}
		backoff = minBackoff

		if first {
			p.sendCtrl(ctrlFrame{Type: "connected"})
		} else {
			// Re-handshake against the fresh upstream. Reset our tracker and
			// have the renderer recreate its RFBClient (back to the version
			// state) so the handshake flows again; clear any read deadline a
			// prior teardown armed so the handshake reads block normally.
			p.hs.Reset()
			// A reconnect is a fresh upstream — re-advertise ext-clip CAPS once
			// the renderer re-sends its (re-injected) SetEncodings (PROT-05).
			p.extClipCapsSent.Store(false)
			// Clear any in-flight Extended Clipboard REQUEST that straddled the
			// drop. Otherwise maybeRetryClipRequest sees the still-armed expired
			// deadline and fires a REQUEST at the FRESH upstream that never sent a
			// NOTIFY — dead-transaction noise into the new stream (#23).
			p.clipReqDeadline.Store(0)
			p.clipReqAttempts.Store(0)
			// The stale-re-announce guard is armed where CAPS are re-advertised
			// (below), NOT here — that event, not this reset, is what triggers
			// QEMU's re-announce, and arming there also covers the dominant
			// fresh-proxy reopens (this reset only runs on in-place re-dials).
			// Fresh upstream: reset the probe budget and let the next 1s tick
			// re-send the probe/lease right away so the agent's lease doesn't
			// lapse across a reconnect (keepwarmAcked itself persists — the
			// agent doesn't change mid-session).
			p.keepwarmProbes.Store(0)
			p.keepwarmNextSend.Store(0)
			p.clientMu.Lock()
			_ = p.client.SetReadDeadline(time.Time{})
			p.clientMu.Unlock()
			p.sendCtrl(ctrlFrame{Type: "rfb_reset"})
			p.sendCtrl(ctrlFrame{Type: "resumed"})
		}
		reconnecting := !first
		first = false

		// Per-connection context for THIS handshake+pumps cycle. Cancelled
		// explicitly on every exit path below (a pump cancels it on failure;
		// we cancel it after wg.Wait / on handshake failure) so there is no
		// leaked context across reconnects.
		connCtx, connCancel := context.WithCancel(parent)

		// Bound the (re)handshake, then drive it. OBS-01: span the handshake hot
		// path; reconnect=true distinguishes a post-drop re-handshake (the
		// desync-prone path) from the initial one in traces.
		p.clientMu.Lock()
		_ = p.client.SetReadDeadline(time.Now().Add(handshakeDeadline))
		p.clientMu.Unlock()
		hsCtx, finishHS := p.startSpan(connCtx, "proxy.handshake",
			attrInt("attempt", failures),
			attrBool("reconnect", reconnecting))
		p.pumpHandshake(hsCtx)
		if p.hs.Done() {
			finishHS(nil)
		} else {
			finishHS(errors.New("handshake did not complete"))
		}
		p.clientMu.Lock()
		_ = p.client.SetReadDeadline(time.Time{})
		p.clientMu.Unlock()

		if !p.hs.Done() {
			// Handshake didn't complete (upstream died mid-handshake, or the
			// renderer didn't re-handshake in time). Discard + reconnect.
			connCancel()
			if u := p.upstream.Swap(nil); u != nil {
				_ = u.Close()
			}
			if parent.Err() != nil {
				return
			}
			// The renderer closed/abandoned the loopback ws (set during the
			// handshake pump). Re-dialing upstream would just thrash against a
			// dead client conn for the whole failure budget — this session is
			// done; the renderer's self-heal reopen drives a fresh one.
			if p.clientClosed.Load() {
				p.log.Info("handshake aborted — client ws closed; ending session for reopen")
				return
			}
			failures++
			if failures >= maxReconnectFailures {
				p.log.Error("handshake never completed after retries — closing client")
				p.closeFatal("unreachable", "console handshake could not complete", websocket.CloseInternalServerErr)
				return
			}
			p.sendCtrl(ctrlFrame{Type: "reconnecting"})
			// Back off before re-dialing. A booting VM whose upstream accepts
			// the WS but closes before the RFB greeting would otherwise spin
			// this loop with no delay, burning the whole failure budget in
			// milliseconds — the fast "console handshake could not complete".
			select {
			case <-parent.Done():
				return
			case <-time.After(jitter(backoff)):
			}
			backoff *= 2
			if backoff > maxBackoff {
				backoff = maxBackoff
			}
			continue
		}
		failures = 0

		// The upstream half-open watchdog (REL-01) is NOT armed here: it arms on
		// the FIRST upstream pong (dialUpstream's pong handler) and re-arms on
		// every subsequent read or pong, after which a silent half-open upstream
		// trips a read error within upstreamReadDeadline and this loop re-dials.
		// Gating on a seen pong means a Prism that never pongs keeps the original
		// no-deadline blocking read, so a healthy but IDLE VM can't spuriously
		// reconnect (REL-01 hardening — see upstreamPongSeen).

		p.clientClosed.Store(false)
		// Watch connCtx so a cancellation that does NOT originate from a pump's
		// own I/O error — graceful shutdown (parent/BaseContext cancel, REL-02) —
		// still unblocks the pumps' blocked ReadMessage by arming teardown's
		// immediate read deadline. Without this the pumps would block on
		// ReadMessage until the conn drops, and srv.Shutdown would wait out the
		// full grace on a live console. The watcher is joined (watcherDone) before
		// the next loop iteration so a stale teardown can't race the next
		// (re)handshake's read deadlines. It always terminates: connCancel is
		// called after wg.Wait below, firing connCtx.Done() even on the normal
		// pump-exit path (teardown is idempotent).
		watcherDone := make(chan struct{})
		go func() {
			defer close(watcherDone)
			// Also wake on a proactive re-auth signal (SH2): the plugin keep-alive
			// invalidated this session's credentials, so unblock the pumps and let
			// the run loop emit session_expired. ReauthCh is closed-once, so every
			// iteration's watcher observes it.
			select {
			case <-connCtx.Done():
			case <-p.sess.ReauthCh():
				p.reauthRequested.Store(true)
			}
			p.teardown()
		}()
		p.wg.Add(3)
		go p.clientToUpstream(connCtx, connCancel)
		go p.upstreamToClient(connCtx, connCancel)
		go p.heartbeat(connCtx)
		p.wg.Wait()
		connCancel()
		<-watcherDone

		if u := p.upstream.Swap(nil); u != nil {
			// Terminal detach (shutdown / operator closed the console /
			// re-auth): best-effort keep-warm cancel while the upstream is
			// still writable, so the agent stops dirtying the display now
			// instead of waiting out the 45s TTL. A reconnect iteration must
			// NOT cancel — the lease persists across re-dials.
			if parent.Err() != nil || p.clientClosed.Load() || p.reauthRequested.Load() {
				p.sendKeepwarmCancel(u)
			}
			_ = u.Close()
		}

		if parent.Err() != nil || p.clientClosed.Load() {
			// Graceful shutdown (parent cancelled): tell the renderer the runtime
			// is going away so it can show "disconnected" cleanly instead of a bare
			// socket drop. Best-effort, short write deadline.
			if parent.Err() != nil {
				p.clientMu.Lock()
				_ = p.client.SetWriteDeadline(time.Now().Add(time.Second))
				_ = p.client.WriteMessage(websocket.CloseMessage,
					websocket.FormatCloseMessage(websocket.CloseGoingAway, "runtime shutting down"))
				p.clientMu.Unlock()
			}
			return // terminal: shutdown, or the operator closed the console
		}
		if p.reauthRequested.Load() {
			// SH2: the session's Prism credentials were invalidated out-of-band
			// (the plugin keep-alive saw a 401). Re-dialing would just fail auth,
			// so proactively tell the renderer to re-authenticate — it enters the
			// re-auth arm (auto-open SSO on the focused console) and reconnects the
			// SAME session id once fresh cookies are captured.
			p.log.Info("session credentials invalidated — prompting re-auth")
			p.closeFatal("session_expired", "Prism session expired — re-authenticate to resume", websocket.ClosePolicyViolation)
			return
		}
		// OBS-01: mark the reconnect decision after a live session dropped (not
		// the operator closing). The following loop iteration's dial/handshake
		// spans carry reconnect=true; this marker pins WHEN the proxy chose to
		// reconnect and at which attempt count.
		_, finishReconnect := p.startSpan(parent, "proxy.reconnect", attrInt("attempt", failures))
		finishReconnect(nil)
		p.sendCtrl(ctrlFrame{Type: "reconnecting"})
	}
}

// teardown unblocks both pump goroutines' blocked ReadMessage by arming an
// immediate read deadline, WITHOUT closing the client (the session continues
// across an upstream reconnect). Idempotent; each pump defers it so whichever
// fails first releases its sibling. The run loop clears the client deadline
// before the next (re)handshake.
func (p *proxy) teardown() {
	p.clientMu.Lock()
	_ = p.client.SetReadDeadline(time.Now())
	p.clientMu.Unlock()
	if u := p.upstream.Load(); u != nil {
		_ = u.SetReadDeadline(time.Now())
	}
}

func (p *proxy) dialUpstream(ctx context.Context) error {
	dial, err := p.openUpstream(ctx)
	if err != nil {
		return err
	}
	p.upstream.Store(dial)
	dial.SetReadLimit(maxUpstreamMessageSize)
	// Fresh connection: it must re-prove it pongs before the half-open watchdog
	// arms (REL-01 hardening — see upstreamPongSeen).
	p.upstreamPongSeen.Store(false)
	// Reset the RTT to "unmeasured" (0) on every (re)dial. Without this, the
	// first post-handshake heartbeat carries the PRIOR connection's last RTT —
	// after a latency spike that is e.g. 113000ms, which (a) freezes the
	// console's latency badge at a stale huge value and (b) far worse, feeds the
	// renderer's adaptive quality machine a >150ms sample at the exact moment of
	// a re-dial, demoting the tier and pinning the FRESH connection to sticky
	// 16bpp RGB565 for its whole life. 0 reads as "unmeasured" until the first
	// real pong of the new connection lands. (Do NOT clamp the genuine value in
	// the pong handler — a real ongoing stall must stay visible.)
	p.upstreamRTTMicros.Store(0)
	telemetry.SetUpstreamRTTMillis(0)
	// Record the real runtime<->Prism RTT from the pong echo of the timestamp
	// heartbeat() pings with. The pong handler runs on upstreamToClient's
	// ReadMessage. Prism that ignores WS pings just leaves the RTT unmeasured.
	dial.SetPongHandler(func(payload string) error {
		// A pong proves the upstream is alive — record it (which enables the
		// watchdog) and arm/re-arm the rolling upstream read deadline, mirroring
		// the client pong handler at :997. See upstreamPongSeen / REL-01.
		p.upstreamPongSeen.Store(true)
		_ = dial.SetReadDeadline(time.Now().Add(upstreamReadDeadline))
		if t, err := time.Parse(time.RFC3339Nano, payload); err == nil {
			rtt := time.Since(t).Microseconds()
			if rtt >= 0 {
				p.upstreamRTTMicros.Store(rtt)
				telemetry.SetUpstreamRTTMillis(rtt / 1000)
			}
		}
		return nil
	})
	if nc := dial.NetConn(); nc != nil {
		ensureNoDelay(nc, p.log)
		setDSCPEF(nc, p.log)
		setQuickACK(nc, p.log)
		setUpstreamKeepAlive(nc, p.log)
	}
	p.log.Info("upstream connected")
	// NOTE: the run loop emits the connected / rfb_reset+resumed control
	// frame — dialUpstream stays silent so reconnects re-handshake correctly.
	return nil
}

// pumpHandshake drives the RFB handshake synchronously. It alternates
// reading from server and client, forwarding each message while tracking
// state. Returns once ServerInit is parsed or on error.
func (p *proxy) pumpHandshake(ctx context.Context) {
	u := p.upstream.Load()
	if u == nil {
		return
	}

	for !p.hs.Done() {
		if ctx.Err() != nil {
			return
		}

		// PROT-03/PROT-04: the tracker entered the terminal error state on an
		// auth/security failure (non-zero SecurityResult, SecurityTypes count==0,
		// or an unsupported chosen security type). It already consumed the
		// trailing RFB 3.8 reason, so the stream is fully drained but DEAD —
		// stop driving the handshake (Done() will never become true). Returning
		// with hs.Done()==false routes through the run loop's "handshake didn't
		// complete" path (bounded retry / give-up); we don't advance a desynced
		// stream into normal message flow.
		if p.hs.Failing() {
			p.log.Warn("RFB handshake failed — security/auth rejected", "reason", p.hs.ErrReason, "state", p.hs.State)
			return
		}

		switch p.hs.State {
		case rfb.HsServerVersion, rfb.HsSecurityTypes, rfb.HsSecurityResult, rfb.HsServerInit:
			mt, data, err := u.ReadMessage()
			if err != nil {
				return
			}
			atomic.AddUint64(&p.sess.Stats.BytesFromUpstream, uint64(len(data)))
			p.hs.FeedServer(data)

			p.clientMu.Lock()
			_ = p.client.SetWriteDeadline(time.Now().Add(writeDeadline))
			writeErr := p.client.WriteMessage(mt, data)
			p.clientMu.Unlock()
			if writeErr != nil {
				// A write failure to the CLIENT means the renderer's loopback ws
				// is gone (it closed it — e.g. its decoder desynced on the upstream
				// drop and it will reopen a fresh stream). Re-dialing upstream only
				// thrashes against a dead client conn, so mark this terminal and let
				// the run loop exit; the renderer's reopen establishes a new session.
				p.clientClosed.Store(true)
				return
			}

		case rfb.HsClientVersion, rfb.HsSecurityChoice, rfb.HsClientInit:
			mt, data, err := p.client.ReadMessage()
			if err != nil {
				// Same as the client write path: the renderer abandoned this ws.
				// Terminal — don't re-dial upstream against a dead client conn.
				p.clientClosed.Store(true)
				return
			}
			atomic.AddUint64(&p.sess.Stats.BytesFromClient, uint64(len(data)))

			if mt == websocket.TextMessage {
				if p.handleClientControl(data) {
					continue
				}
			}

			p.hs.FeedClient(data)

			_ = u.SetWriteDeadline(time.Now().Add(writeDeadline))
			if err := u.WriteMessage(mt, data); err != nil {
				return
			}

		default:
			return
		}
	}

	// Sync the negotiated bytes-per-pixel from ServerInit so the stream parser
	// can walk variable-length encodings correctly. (The old data-region
	// machinery — pixel-strip clipboard channel + clipboard_ready frame — was
	// removed now that the live transport is vdagent-over-serial: SCAF-02.)
	if p.hs.ServerInfo != nil {
		bpp := int(p.hs.ServerInfo.BPP) / 8
		if bpp < 1 {
			bpp = 4
		}
		p.stream.SetBPP(bpp)
	} else {
		p.log.Warn("handshake completed but ServerInfo is nil")
	}
}

func (p *proxy) clientToUpstream(ctx context.Context, cancel context.CancelFunc) {
	// Defer order (LIFO): cancel() first (stops heartbeat extending the read
	// deadline), then teardown() (unblocks the sibling pump), then the panic
	// guard, then wg.Done().
	defer p.wg.Done()
	// Symmetric panic containment with the upstream pump: no pump goroutine may
	// crash the whole runtime process.
	defer func() {
		if r := recover(); r != nil {
			p.log.Error("recovered panic in client→upstream pump — terminating session", "panic", r)
			p.clientClosed.Store(true)
		}
	}()
	defer p.teardown()
	defer cancel()
	for {
		mt, data, err := p.client.ReadMessage()
		if err != nil {
			// A normal/going-away close is the OPERATOR closing the console →
			// terminal. Any other error (incl. our teardown read-deadline when
			// the upstream failed) is NOT a client close — leave clientClosed
			// false so the run loop reconnects.
			if websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway) {
				p.clientClosed.Store(true)
			}
			p.log.Debug("client read end", "err", err)
			return
		}
		atomic.AddUint64(&p.sess.Stats.BytesFromClient, uint64(len(data)))
		p.sess.Touch() // mark the session alive for the host reaper (AHV-03)

		if mt == websocket.TextMessage {
			if p.handleClientControl(data) {
				continue
			}
		}

		u := p.upstream.Load()
		if u == nil {
			// Upstream is gone (being reconnected by the run loop). Drop this
			// frame and exit; the loop re-dials, re-handshakes, and respawns.
			return
		}

		if mt == websocket.BinaryMessage {
			filtered := p.handleClientBinary(data)
			if filtered != nil {
				data = filtered
				if len(data) == 0 {
					continue
				}
			}
		}

		p.upstreamMu.Lock()
		_ = u.SetWriteDeadline(time.Now().Add(writeDeadline))
		writeErr := u.WriteMessage(mt, data)
		p.upstreamMu.Unlock()
		if writeErr != nil {
			// Upstream write failed → upstream is down. Exit; the run loop
			// reconnects (clientClosed stays false).
			p.log.Warn("upstream write failed", "err", writeErr)
			return
		}
	}
}

// handleClientBinary parses client messages for logging and input filtering.
// Returns filtered data (with input events stripped for non-controllers),
// or nil to forward the original data unchanged.
func (p *proxy) handleClientBinary(data []byte) []byte {
	msgs, remainder, err := p.stream.ProcessClientData(data)
	if err != nil {
		return nil
	}

	isController := true
	if p.roleCheck != nil {
		role := p.roleCheck(p.sess.ID)
		isController = role == "" || role == "controller"
	}

	needsFilter := false
	needsExtClipInject := false
	hasSetDesktopSize := false
	for _, msg := range msgs {
		if msg.Type == rfb.MsgClientCutText {
			// Renderer-originated host->guest copy: real clipboard traffic, so
			// keep-warm sends back off (Condition A). The proxy's own keep-warm
			// tokens are written upstream directly and never pass through here.
			p.markClipActivity()
		}
		if msg.Type == rfb.MsgSetEncodings {
			needsExtClipInject = true
		}
		if msg.Type == rfb.MsgSetDesktopSize {
			hasSetDesktopSize = true
		}
		if !isController &&
			(msg.Type == rfb.MsgKeyEvent ||
				msg.Type == rfb.MsgPointerEvent ||
				msg.Type == rfb.MsgQEMUClientMessage) {
			needsFilter = true
		}
	}

	if needsExtClipInject || needsFilter || hasSetDesktopSize {
		// Non-nil empty slice on purpose: handleClientBinary's contract is
		// nil = "forward the original frame unchanged", non-nil = "use this
		// rewritten frame (drop it if empty)". A viewer frame carrying ONLY
		// input events strips down to zero bytes; with a nil buf the caller
		// would read that as "forward unchanged" and the viewer's keystroke
		// would reach the guest — the AHV-02 control would be dead even when
		// wired. Starting non-nil makes a fully-stripped frame a real drop.
		buf := make([]byte, 0, len(data))
		for _, msg := range msgs {
			if !isController &&
				(msg.Type == rfb.MsgKeyEvent ||
					msg.Type == rfb.MsgPointerEvent ||
					msg.Type == rfb.MsgQEMUClientMessage) {
				// Strip viewer input — including ext-key (msg 255) frames, which
				// previously slipped through unfiltered when a frame failed to
				// parse, letting a non-controller's keystrokes reach the guest.
				continue
			}
			if msg.Type == rfb.MsgSetEncodings {
				injected := rfb.InjectExtendedClipboard(msg.Raw)
				if injected != nil {
					p.log.Info("injected Extended Clipboard encoding into SetEncodings")
					buf = append(buf, injected...)
					// PROT-05: proactively advertise our ext-clip CAPS upstream
					// ONCE, immediately AFTER the injected SetEncodings (same
					// upstream frame, correct order: QEMU enables the encoding
					// from SetEncodings, then sees our CAPS). QEMU may never send
					// CAPS first, and without ours it never enables the Extended
					// Clipboard PROVIDE path, so a guest copy would never reach
					// the operator. CompareAndSwap makes it once-per-connection;
					// extClipCapsSent is reset on reconnect.
					if p.extClipCapsSent.CompareAndSwap(false, true) {
						buf = append(buf, rfb.BuildExtClipCapsResponse()...)
						p.log.Info("appended proactive Extended Clipboard CAPS upstream")
						// Arm the stale-re-announce guard: enabling ext-clip makes QEMU
						// re-announce its stale clipboard, and the first NOTIFY that
						// follows is that re-announce — skip its REQUEST so it cannot
						// storm the single clipboard slot (see onExtClipNotify).
						p.expectStaleReannounce.Store(true)
					}
					continue
				}
			}
			// SetDesktopSize → clipboard side-channel translation was
			// removed. AHV's QEMU stdvga ignores SetDesktopSize and our
			// previous workaround (encode WxH as __AHVRC_RESIZE__
			// ClientCutText, route via clipboard) hijacked the clipboard
			// transport and starved real operator copies. The guest
			// resolution is now driver-pinned at boot. SetDesktopSize
			// messages still pass through to the upstream untouched —
			// QEMU will silently ignore them.
			buf = append(buf, msg.Raw...)
		}
		// Preserve any incomplete trailing bytes (a message split across WS
		// frames) so the upstream stream stays byte-aligned.
		buf = append(buf, remainder...)
		return buf
	}

	return nil
}

func (p *proxy) upstreamToClient(ctx context.Context, cancel context.CancelFunc) {
	// Defer order (LIFO): cancel() first, then teardown() (unblock sibling),
	// then the panic guard (so it sets clientClosed BEFORE wg.Done releases
	// run()'s wg.Wait), then wg.Done().
	defer p.wg.Done()
	// Contain any panic to THIS session. The upstream RFB stream is untrusted
	// (semi-trusted/MITM-able Prism path); an unrecovered panic in a pump
	// goroutine would otherwise terminate the entire runtime process, killing
	// every concurrent console session for every operator. The stream is now
	// misaligned/un-trustworthy, so terminate rather than reconnect into the
	// same poison frame (windows/386 length overflow).
	defer func() {
		if r := recover(); r != nil {
			p.log.Error("recovered panic in upstream→client pump — terminating session", "panic", r)
			p.clientClosed.Store(true)
		}
	}()
	defer p.teardown()
	defer cancel()
	u := p.upstream.Load()
	if u == nil {
		return // run loop is (re)dialing; it will respawn us after handshake
	}
	// Hot path: ReadMessage = NextReader + io.ReadAll, i.e. a fresh growing
	// buffer per message — at 30fps motion that is megabytes/s of allocation
	// churn driving GC mark-assist jitter on this goroutine. Reuse ONE buffer
	// across iterations instead. Safe because the pump is its single owner
	// (one pump per connect cycle; respawned by run() after each re-dial) and
	// every frame is fully consumed — the client WriteMessage flushes before
	// returning — before the next read overwrites it. See the non-retention
	// contract on handleServerBinary. The Grow also keeps Bytes() non-nil so
	// an empty first message still forwards as an empty frame instead of
	// being swallowed by the nil-means-drop contract below.
	var readBuf bytes.Buffer
	readBuf.Grow(64 * 1024)
	for {
		mt, r, err := u.NextReader()
		if err == nil {
			if readBuf.Cap() > upstreamReadBufShrinkCap {
				readBuf = bytes.Buffer{} // drop an outlier frame's capacity for GC
				readBuf.Grow(64 * 1024)
			}
			readBuf.Reset()
			// websocket.ErrReadLimit surfaces verbatim from ReadFrom (a non-EOF
			// reader error is returned as-is) just as from NextReader, so the
			// errors.Is classification below is unchanged.
			_, err = readBuf.ReadFrom(r)
		}
		if err != nil {
			// A frame larger than maxMessageSize is unforwardable AND would
			// recur on every reconnect (the renderer re-requests the same full
			// frame) → an infinite re-dial loop. Terminate with an actionable
			// fatal instead of looping into the same poison frame. With the
			// 8K-sized cap above this should never fire for a real resolution;
			// it's the graceful floor for a beyond-clamp or malformed frame.
			if errors.Is(err, websocket.ErrReadLimit) {
				p.log.Error("upstream frame exceeds max message size — terminating session",
					"limit_bytes", maxUpstreamMessageSize)
				p.clientClosed.Store(true)
				p.closeFatal(
					"frame_too_large",
					"the cluster sent a display frame larger than this client supports",
					websocket.CloseMessageTooBig,
				)
				return
			}
			// Otherwise the upstream is down (a real close OR the half-open
			// watchdog's rolling read deadline firing on a silent upstream).
			// Exit; the run loop re-dials, re-handshakes, and respawns the pumps.
			// (clientClosed stays false → reconnect, not terminate.)
			p.log.Debug("upstream read end", "err", err)
			return
		}
		data := readBuf.Bytes()
		// A successful read means the upstream is alive — re-arm the half-open
		// watchdog, but only once this upstream has proven it pongs, so a
		// non-ponging upstream never carries a deadline that could fire while
		// idle (REL-01 hardening).
		if p.upstreamPongSeen.Load() {
			_ = u.SetReadDeadline(time.Now().Add(upstreamReadDeadline))
		}
		atomic.AddUint64(&p.sess.Stats.BytesFromUpstream, uint64(len(data)))
		p.sess.Touch() // mark the session alive for the host reaper (AHV-03)

		if mt == websocket.BinaryMessage {
			data = p.handleServerBinary(data)
			if data == nil {
				continue
			}
		}

		p.clientMu.Lock()
		_ = p.client.SetWriteDeadline(time.Now().Add(writeDeadline))
		writeErr := p.client.WriteMessage(mt, data)
		p.clientMu.Unlock()
		if writeErr != nil {
			// Client write failed → the renderer's WS is broken. Terminal.
			p.log.Warn("client write failed", "err", writeErr)
			p.clientClosed.Store(true)
			return
		}
	}
}

// handleServerBinary inspects (and where needed rewrites) one upstream binary
// WS frame. Non-retention contract: data aliases the upstream pump's REUSED
// read buffer and is invalid after the caller's loop iteration — neither this
// function nor anything it calls may retain data or a sub-slice of it past
// return. Returning data itself (passthrough) is fine: the caller writes it
// out before the next read overwrites the buffer. Any rewritten frame must be
// (and is) a freshly allocated slice.
func (p *proxy) handleServerBinary(data []byte) []byte {
	if len(data) == 0 {
		return data
	}

	// Fast path: FramebufferUpdate (type 0) and Bell (type 2) need no modification,
	// so forward them raw — the proxy does not decode framebuffer updates (the
	// renderer does), and re-walking Tight/Hextile tile trees here is wasted work.
	// This is the literal baseline passthrough. PROT-07 once gated it on a full
	// reparse to translate a theoretical coalesced "FBU || ext-clip ServerCutText"
	// WS frame, but AHV sends each RFB message in its own frame, the proxy cannot
	// reliably reframe a Tight FBU, and gating regressed EVERY Tight frame into a
	// renderer desync loop — so we keep the unconditional passthrough and accept
	// that a (never-observed) ext-clip coalesced onto an FBU frame is not translated.
	if data[0] == rfb.MsgFramebufferUpdate || data[0] == rfb.MsgBell {
		return data
	}

	msgs, remainder, err := p.stream.ProcessServerData(data)
	if err != nil || len(remainder) > 0 {
		// Can't parse — check for Extended Clipboard ServerCutText
		// directly on the raw data (it may fail RFB parsing due to
		// negative length).
		// A real Extended Clipboard ServerCutText is msg-type 3 with a ZERO 3-byte
		// padding field and a negative length. AHV splits large Tight
		// FramebufferUpdates across WS messages, so a CONTINUATION frame's first
		// byte is arbitrary Tight-compressed data — and one that happens to be 0x03
		// with a negative int32 at [4:8] was mis-detected here as ext-clip and
		// DROPPED (return nil), silently removing ~16 KB of pixel data and
		// desyncing the renderer's Tight stream (PROT-09). Requiring the zero
		// padding drops the false-positive rate to ~1-in-4-billion, and we NEVER
		// drop a frame we're not certain about — forward it raw.
		if len(data) >= 8 && data[0] == rfb.MsgServerCutText &&
			data[1] == 0 && data[2] == 0 && data[3] == 0 {
			length := int32(binary.BigEndian.Uint32(data[4:8])) // #nosec G115 -- Extended Clipboard uses negative length as format signal
			if length < 0 && length != -0x80000000 {
				absLen := int(-length)
				if absLen > 16*1024*1024 || len(data) < 8+absLen {
					// Not a complete in-frame ext-clip message: almost certainly a
					// FramebufferUpdate continuation frame. Forward raw — dropping it
					// was the desync bug; the renderer reframes the byte stream.
					return data
				}
				translated := rfb.TranslateExtClipServerCutText(data)
				if translated != nil {
					p.clipReqDeadline.Store(0)
					p.clipReqAttempts.Store(0)
					// The agent's keep-warm ack is side-channel, not operator
					// data: swallow it (no renderer forward, no host clipboard,
					// no content log) and enable lease sends.
					if isKeepwarmAck(translated) {
						p.handleKeepwarmAck()
						return nil
					}
					// A sibling console's keep-warm token broadcast back by
					// QEMU — side-channel, not operator data: swallow.
					if isKeepwarmEcho(translated) {
						return nil
					}
					p.markClipActivity()
					p.log.Info("translated Extended Clipboard → legacy ServerCutText",
						"original_len", -length, "text_len", len(translated)-8)
					return translated
				}
				// NOTIFY = server announces it has clipboard data.
				// Respond with REQUEST so the server sends PROVIDE.
				if rfb.IsExtClipNotify(data) {
					p.onExtClipNotify()
					return nil
				}
				// A PROVIDE that failed to translate (zlib error / oversize /
				// truncated) is a LOST guest copy — surface it instead of
				// silently treating it as a control message.
				if rfb.IsExtClipProvide(data) {
					p.log.Warn("Extended Clipboard PROVIDE failed to decode — guest copy not delivered", "original_len", -length)
					p.clipReqDeadline.Store(0)
					p.clipReqAttempts.Store(0)
					p.sendCtrl(ctrlFrame{Type: "clipboard_failed", Reason: "guest clipboard could not be decoded"})
					telemetry.IncClipboardFailed()
					return nil
				}
				// Other Extended Clipboard (CAPS, PEEK, etc.) — respond
				// with our CAPS and suppress.
				capsResp := rfb.BuildExtClipCapsResponse()
				u := p.upstream.Load()
				if u != nil {
					p.upstreamMu.Lock()
					_ = u.SetWriteDeadline(time.Now().Add(writeDeadline))
					_ = u.WriteMessage(websocket.BinaryMessage, capsResp)
					p.upstreamMu.Unlock()
				}
				return nil
			}
		}
		return data
	}

	modified := false
	for i, msg := range msgs {
		if msg.Type == rfb.MsgServerCutText && len(msg.Raw) >= 8 {
			length := int32(binary.BigEndian.Uint32(msg.Raw[4:8])) // #nosec G115 -- Extended Clipboard uses negative length as format signal
			if length < 0 {
				translated := rfb.TranslateExtClipServerCutText(msg.Raw)
				if translated != nil {
					p.clipReqDeadline.Store(0)
					p.clipReqAttempts.Store(0)
					// Keep-warm ack: side-channel, swallowed (see the raw-path
					// twin above).
					if isKeepwarmAck(translated) {
						p.handleKeepwarmAck()
						msgs[i].Raw = nil
						modified = true
						continue
					}
					if isKeepwarmEcho(translated) {
						msgs[i].Raw = nil
						modified = true
						continue
					}
					p.markClipActivity()
					p.log.Info("translated Extended Clipboard → legacy ServerCutText",
						"text_len", len(translated)-8)
					msgs[i].Raw = translated
					modified = true
				} else {
					msgs[i].Raw = nil
					modified = true
					if rfb.IsExtClipNotify(msg.Raw) {
						p.onExtClipNotify()
					} else if rfb.IsExtClipProvide(msg.Raw) {
						p.log.Warn("Extended Clipboard PROVIDE failed to decode — guest copy not delivered")
						p.clipReqDeadline.Store(0)
						p.clipReqAttempts.Store(0)
						p.sendCtrl(ctrlFrame{Type: "clipboard_failed", Reason: "guest clipboard could not be decoded"})
						telemetry.IncClipboardFailed()
					} else {
						capsResp := rfb.BuildExtClipCapsResponse()
						u := p.upstream.Load()
						if u != nil {
							p.upstreamMu.Lock()
							_ = u.SetWriteDeadline(time.Now().Add(writeDeadline))
							_ = u.WriteMessage(websocket.BinaryMessage, capsResp)
							p.upstreamMu.Unlock()
						}
					}
				}
				continue
			}
			// LEGACY ServerCutText (non-negative length) — normally forwarded
			// untouched. Intercept the agent's keep-warm ack and any sibling
			// console's keep-warm tokens in case QEMU delivered them
			// legacy-style (ext-clip not negotiated); any other legacy
			// cut-text is a real guest copy (Condition A activity).
			if isKeepwarmAck(msg.Raw) {
				p.handleKeepwarmAck()
				msgs[i].Raw = nil
				modified = true
				continue
			}
			if isKeepwarmEcho(msg.Raw) {
				msgs[i].Raw = nil
				modified = true
				continue
			}
			p.markClipActivity()
		}
		// FramebufferUpdate data-region extraction removed —
		// clipboard flows through vdagent serial channel.
	}

	if !modified {
		return data
	}

	var buf []byte
	for _, msg := range msgs {
		if msg.Raw != nil {
			buf = append(buf, msg.Raw...)
		}
	}
	if len(buf) == 0 {
		return nil
	}
	return buf
}

// latencyMS converts a measured RTT (microseconds) to the latency_ms the
// renderer shows. 0 means unmeasured; a real round-trip never reports 0 — a
// sub-millisecond link rounds up to 1ms, since "0ms" is misleading.
func latencyMS(micros int64) int64 {
	if micros <= 0 {
		return 0
	}
	if ms := (micros + 500) / 1000; ms >= 1 {
		return ms
	}
	return 1
}

// sendStats emits the live console counter frame (cumulative byte totals +
// measured upstream RTT). Sent immediately on connect and ~every second so the
// console's ↓/↑ rail reflects real traffic instead of sitting at 0 between the
// 15s liveness pings.
func (p *proxy) sendStats() {
	p.sendCtrl(ctrlFrame{
		Type:     "heartbeat",
		Latency:  latencyMS(p.upstreamRTTMicros.Load()),
		BytesIn:  atomic.LoadUint64(&p.sess.Stats.BytesFromUpstream),
		BytesOut: atomic.LoadUint64(&p.sess.Stats.BytesFromClient),
	})
}

func (p *proxy) heartbeat(ctx context.Context) {
	defer p.wg.Done()
	t := time.NewTicker(heartbeatInterval)
	defer t.Stop()
	// Separate fast ticker drives the Extended Clipboard REQUEST re-send so a
	// dropped PROVIDE is retried within ~1s, not at the 15s heartbeat cadence.
	clipT := time.NewTicker(time.Second)
	defer clipT.Stop()

	p.client.SetPongHandler(func(string) error {
		// Once this session's ctx is cancelled (a pump is tearing down for an
		// upstream reconnect), stop extending the client read deadline — else
		// a late pong could re-arm a long deadline and race teardown()'s
		// immediate deadline, delaying clientToUpstream's unblock.
		if ctx.Err() != nil {
			return nil
		}
		_ = p.client.SetReadDeadline(time.Now().Add(pongDeadline))
		return nil
	})
	_ = p.client.SetReadDeadline(time.Now().Add(pongDeadline))

	// Prime the upstream RTT immediately so latency_ms is populated by the
	// first heartbeat frame rather than one interval later.
	if u := p.upstream.Load(); u != nil {
		_ = u.WriteControl(websocket.PingMessage,
			[]byte(time.Now().Format(time.RFC3339Nano)), time.Now().Add(writeDeadline))
	}
	// Emit one stats frame at once so the console counter populates immediately
	// on (re)connect instead of waiting for the first 15s tick.
	p.sendStats()
	// (The keep-warm probe/lease is NOT kicked here: its first send rides the
	// first 1s clipT tick below, which leaves the renderer's immediate
	// post-handshake messages — SetEncodings + the ext-clip CAPS injection —
	// first in line on the fresh upstream.)

	for {
		select {
		case <-ctx.Done():
			return
		case <-clipT.C:
			// Bail if the session is tearing down: select can pick this case in
			// the same tick connCtx is cancelled, and we don't want a stats frame
			// written to a client that's already closing.
			if ctx.Err() != nil {
				return
			}
			p.maybeRetryClipRequest()
			// Piggyback a live stats frame on the 1s clip ticker so the console
			// ↓/↑ counter updates ~every second, not every 15s.
			p.sendStats()
			// Keep-warm rides the same 1s ticker: probes/renewals every
			// keepwarmRenewInterval, with deferred sends retried each tick.
			p.maybeSendKeepwarm()
		case ts := <-t.C:
			p.clientMu.Lock()
			_ = p.client.SetWriteDeadline(time.Now().Add(writeDeadline))
			pingErr := p.client.WriteControl(websocket.PingMessage,
				[]byte(ts.Format(time.RFC3339Nano)), time.Now().Add(writeDeadline))
			p.clientMu.Unlock()
			if pingErr != nil {
				p.log.Warn("ping failed", "err", pingErr)
				return
			}
			// Ping the UPSTREAM too so the dial's pong handler can measure the
			// real runtime<->Prism RTT — the meaningful latency. The client ping
			// above is loopback (~0). WriteControl is safe concurrent with the
			// pump's writes (gorilla). A write error here means the upstream conn
			// is broken (e.g. half-open: the OS finally surfaced the dead socket
			// on write) — previously discarded, which let the session linger on a
			// dead upstream. Tear the session down so the run loop re-dials and
			// re-handshakes (REL-01).
			if u := p.upstream.Load(); u != nil {
				if err := u.WriteControl(websocket.PingMessage,
					[]byte(ts.Format(time.RFC3339Nano)), time.Now().Add(writeDeadline)); err != nil {
					p.log.Warn("upstream ping failed — tearing down for reconnect", "err", err)
					p.teardown()
					return
				}
			}
			// No sendStats() here: the 1s clipT case above already emits the live
			// counter, so a stats frame on the 15s boundary would just duplicate it.
		}
	}
}

// (The old in-goroutine reconnect() was removed: reconnection is now owned by
// the run() loop, which re-runs the RFB handshake on every reconnect — the
// previous reconnect() re-dialed WITHOUT re-handshaking and silently desynced
// the whole stream after a guest reboot. See run() + teardown().)

func (p *proxy) handleClientControl(data []byte) bool {
	var ctl ctrlFrame
	if err := json.Unmarshal(data, &ctl); err != nil {
		return false
	}
	switch ctl.Type {
	case "ping":
		p.sendCtrl(ctrlFrame{Type: "pong"})
		return true
	}
	return false
}

const (
	clipRequestTimeout     = 5 * time.Second
	clipRequestMaxAttempts = 3
)

// onExtClipNotify handles a server→client Extended Clipboard NOTIFY. Normally it
// REQUESTs the guest copy. But the FIRST NOTIFY after we (re-)advertised ext-clip
// CAPS is QEMU re-announcing its STALE clipboard to the fresh stream (see
// expectStaleReannounce) — requesting + retrying it storms the single clipboard
// slot and starves real pastes, so we skip exactly that one. Every subsequent
// NOTIFY (a genuine guest copy) flows normally, so this can never black out the
// channel like the removed circuit breaker.
func (p *proxy) onExtClipNotify() {
	if p.expectStaleReannounce.CompareAndSwap(true, false) {
		p.log.Info("Extended Clipboard NOTIFY is QEMU's post-attach stale re-announce — skipping its REQUEST")
		return
	}
	p.log.Info("received Extended Clipboard NOTIFY, sending REQUEST")
	p.startExtClipRequest()
}

func (p *proxy) startExtClipRequest() {
	// A guest->host transfer is starting — keep-warm backs off (Condition A;
	// the armed clipReqDeadline below also hard-blocks it until PROVIDE).
	p.markClipActivity()
	p.clipReqAttempts.Store(0)
	p.writeExtClipRequest()
}

// writeExtClipRequest sends the REQUEST bytes to the upstream and arms the
// re-send deadline.
func (p *proxy) writeExtClipRequest() {
	p.clipReqDeadline.Store(time.Now().Add(clipRequestTimeout).UnixNano())
	req := rfb.BuildExtClipRequest()
	u := p.upstream.Load()
	if u != nil {
		p.upstreamMu.Lock()
		_ = u.SetWriteDeadline(time.Now().Add(writeDeadline))
		_ = u.WriteMessage(websocket.BinaryMessage, req)
		p.upstreamMu.Unlock()
	}
}

// maybeRetryClipRequest re-sends an Extended Clipboard REQUEST when a PROVIDE
// has not arrived within the deadline (e.g. the PROVIDE dropped around a guest
// reboot / chardev swap). Previously clipReqDeadline was write-only — the
// promised re-send did not exist, so a single dropped PROVIDE silently lost
// the guest copy forever. Bounded; on final failure it clears the pending
// state and tells the renderer the copy was lost rather than letting the
// operator paste stale content. Driven ~1 Hz from heartbeat.
func (p *proxy) maybeRetryClipRequest() {
	dl := p.clipReqDeadline.Load()
	if dl == 0 || time.Now().UnixNano() < dl {
		return
	}
	if p.clipReqAttempts.Add(1) > clipRequestMaxAttempts {
		p.clipReqDeadline.Store(0)
		p.clipReqAttempts.Store(0)
		p.log.Warn("Extended Clipboard PROVIDE never arrived after retries — guest copy lost")
		p.sendCtrl(ctrlFrame{Type: "clipboard_failed", Reason: "guest clipboard request timed out"})
		return
	}
	p.log.Info("Extended Clipboard PROVIDE overdue — re-sending REQUEST", "attempt", p.clipReqAttempts.Load())
	p.writeExtClipRequest()
}

// markClipActivity records that REAL operator clipboard data moved (either
// direction). Keep-warm sends defer while this is fresh (Condition A): the
// side-channel must never contend with an in-progress operator copy.
func (p *proxy) markClipActivity() {
	p.lastClipActivity.Store(time.Now().UnixNano())
}

// isKeepwarmAck reports whether a LEGACY-format ServerCutText frame carries
// exactly the agent's keep-warm ack token.
func isKeepwarmAck(legacy []byte) bool {
	return len(legacy) == 8+len(keepwarmAckText) && string(legacy[8:]) == keepwarmAckText
}

// isKeepwarmEcho reports whether a LEGACY-format ServerCutText frame carries a
// keep-warm side-channel token other than the ack. QEMU's clipboard manager
// broadcasts one client's cut-text to every attached peer, so with a second
// console on the same VM the SIBLING proxy's probe/lease/cancel tokens arrive
// here as if they were guest clipboard. They are never operator data: callers
// swallow them WITHOUT marking clipboard activity (a sibling's 20s renewals
// must not perpetually defer our own sends via Condition A) and WITHOUT
// latching keepwarmAcked (only the genuine agent ack proves capability).
func isKeepwarmEcho(legacy []byte) bool {
	if len(legacy) < 8 {
		return false
	}
	text := legacy[8:]
	return string(text) == keepwarmProbeToken ||
		bytes.HasPrefix(text, []byte(keepwarmTokenPrefix))
}

// handleKeepwarmAck consumes the agent's ack to the keep-warm probe. Callers
// swallow the carrying frame — it must never reach the renderer or the host
// clipboard — and its content is never logged.
func (p *proxy) handleKeepwarmAck() {
	if p.keepwarmAcked.CompareAndSwap(false, true) {
		p.log.Info("keepwarm: agent acked probe — display keep-warm enabled")
	}
	p.keepwarmNextSend.Store(0) // first real lease goes out on the next tick
}

// maybeSendKeepwarm drives the keep-warm lease sender from the heartbeat's 1s
// clipboard ticker (no goroutine/timer of its own): it sends ONE legacy
// ClientCutText upstream — the capability probe at keepwarmColdInterval until the
// agent acks, then lease renewals at keepwarmRenewInterval. Probing fast while
// unacked catches a cold-attach agent (vdagent backend + caps come up over
// several seconds) instead of leaving up to a full renew interval of unopposed
// QEMU idle-backoff after resume. Condition A: it HARD-BLOCKS while a
// guest->host Extended Clipboard REQUEST is mid-dance (clipReqDeadline armed —
// the in-flight transfer state the proxy already tracks; the host->guest
// direction is a single ClientCutText covered by the activity window) and
// DEFERS while real clipboard traffic was seen within keepwarmClipQuiet. A
// deferred send retries on the next 1s tick; the agent's 45s TTL comfortably
// absorbs the delay. Version skew: until keepwarmAcked, only resize-namespace
// probes go out (a stale agent swallows those without clipboard pollution),
// capped at keepwarmMaxProbes per connection.
func (p *proxy) maybeSendKeepwarm() {
	now := time.Now()
	if now.UnixNano() < p.keepwarmNextSend.Load() {
		return
	}
	if p.clipReqDeadline.Load() != 0 {
		return // hard-block: guest->host transfer mid-dance
	}
	if last := p.lastClipActivity.Load(); last != 0 && now.UnixNano()-last < int64(keepwarmClipQuiet) {
		return // defer: operator clipboard traffic is fresh
	}
	acked := p.keepwarmAcked.Load()
	token := keepwarmLeaseToken
	if !acked {
		if p.keepwarmProbes.Load() >= keepwarmMaxProbes {
			return
		}
		token = keepwarmProbeToken
	}
	u := p.upstream.Load()
	if u == nil {
		return
	}
	p.upstreamMu.Lock()
	_ = u.SetWriteDeadline(time.Now().Add(writeDeadline))
	err := u.WriteMessage(websocket.BinaryMessage, rfb.BuildClientCutText([]byte(token)))
	p.upstreamMu.Unlock()
	if err != nil {
		return // upstream is dying; the pumps/heartbeat own the teardown
	}
	// Probe fast until the agent acks (cold-attach engagement), then settle into
	// the lease-renewal cadence. `acked` is the pre-send state captured above.
	interval := keepwarmRenewInterval
	if !acked {
		p.log.Info("keepwarm: probe sent", "attempt", p.keepwarmProbes.Add(1))
		interval = keepwarmColdInterval
	} else {
		p.log.Debug("keepwarm: lease renewed")
	}
	p.keepwarmNextSend.Store(now.Add(interval).UnixNano())
}

// sendKeepwarmCancel best-effort disarms the agent's lease on stream detach.
// Only sent once the agent proved it understands keep-warm tokens (acked) — a
// stale agent would write the cancel token into its guest clipboard. If the
// send fails, the agent-side TTL expiry (<=45s) is the backstop.
func (p *proxy) sendKeepwarmCancel(u *websocket.Conn) {
	if !p.keepwarmAcked.Load() {
		return
	}
	p.upstreamMu.Lock()
	_ = u.SetWriteDeadline(time.Now().Add(time.Second))
	err := u.WriteMessage(websocket.BinaryMessage, rfb.BuildClientCutText([]byte(keepwarmCancelToken)))
	p.upstreamMu.Unlock()
	if err == nil {
		p.log.Info("keepwarm: lease cancelled on stream detach")
	}
}

func (p *proxy) sendCtrl(c ctrlFrame) {
	data, err := json.Marshal(c)
	if err != nil {
		return
	}
	p.clientMu.Lock()
	defer p.clientMu.Unlock()
	_ = p.client.SetWriteDeadline(time.Now().Add(writeDeadline))
	_ = p.client.WriteMessage(websocket.TextMessage, data)
}

// ensureNoDelay sets TCP_NODELAY on the underlying TCP connection.
// Go defaults this to true, but custom dialers or TLS wrappers may
// override it. For a 6-byte pointer event this avoids a 40ms Nagle
// delay — every serious VNC implementation does this.
func ensureNoDelay(conn net.Conn, log *slog.Logger) {
	switch c := conn.(type) {
	case *net.TCPConn:
		_ = c.SetNoDelay(true)
		log.Debug("TCP_NODELAY set on plain TCP conn")
	case *tls.Conn:
		if tc, ok := c.NetConn().(*net.TCPConn); ok {
			_ = tc.SetNoDelay(true)
			log.Debug("TCP_NODELAY set on TLS-wrapped TCP conn")
		}
	}
}

// upstreamKeepAlivePeriod is the TCP keepalive idle/probe period armed on the
// upstream conn (REL-01 backstop).
const upstreamKeepAlivePeriod = 30 * time.Second

// setUpstreamKeepAlive enables OS-level TCP keepalive on the upstream conn so a
// silently-dead / black-holed upstream (VPN black-hole, NAT idle-drop, Mercury
// dropping the backend) eventually trips a read/write error and forces a
// reconnect EVEN IF it never answered a WS pong — the pong-gated read deadline
// (upstreamPongSeen) cannot cover that case. Keepalive probes are ACKed by a
// healthy idle VM's kernel regardless of application activity, so this does NOT
// spuriously reconnect an idle-but-live console.
func setUpstreamKeepAlive(conn net.Conn, log *slog.Logger) {
	tcp, ok := conn.(*net.TCPConn)
	if !ok {
		if tc, isTLS := conn.(*tls.Conn); isTLS {
			tcp, ok = tc.NetConn().(*net.TCPConn)
		}
	}
	if !ok || tcp == nil {
		return
	}
	// Bound death-detection IN CODE. A bare SetKeepAlivePeriod leaves the probe
	// COUNT at the OS default (Linux 9 / macOS 8), so a black-holed upstream
	// takes ~4-5 min to trip. KeepAliveConfig pins idle + interval + count for a
	// deterministic ~60s ceiling (30s idle + 3×10s) that fires when the peer
	// kernel stops ACKing — independent of application reads, and still never
	// false-fires on an idle-but-live VM whose kernel keeps ACKing. (Go 1.23+.)
	cfg := net.KeepAliveConfig{
		Enable:   true,
		Idle:     upstreamKeepAlivePeriod,
		Interval: 10 * time.Second,
		Count:    3,
	}
	if err := tcp.SetKeepAliveConfig(cfg); err != nil {
		// Fall back to the period-only form if the platform rejects the full
		// config (keeps keepalive enabled, just at the OS default probe count).
		_ = tcp.SetKeepAlive(true)
		_ = tcp.SetKeepAlivePeriod(upstreamKeepAlivePeriod)
		log.Debug("TCP keepalive: KeepAliveConfig unsupported, used period only", "err", err.Error())
		return
	}
	log.Debug("TCP keepalive armed on upstream conn",
		"idle", cfg.Idle.String(), "interval", cfg.Interval.String(), "count", cfg.Count)
}

// setDSCPEF and setQuickACK use raw socket options whose syscall
// signatures differ between Windows (fd is syscall.Handle) and POSIX
// (fd is int). Implementations live in proxy_sockopt_{unix,windows}.go
// behind build tags; both expose the same public signature.

func truncateCloseReason(s string) string {
	if len(s) <= 120 {
		return s
	}
	return s[:117] + "..."
}

func jitter(d time.Duration) time.Duration {
	if d <= 0 {
		return 0
	}
	// ±25% from a real PRNG. The old form derived the offset from
	// time.Now().UnixNano()%50 — a 50ns-period function that, on a coarse-clock
	// platform (macOS quantizes UnixNano to ~1000ns), collapses to a CONSTANT
	// offset, so N goroutines released by one event (runtime restart / VPN flap)
	// re-dialed Prism in lockstep instead of spreading. math/rand/v2 is
	// goroutine-safe and auto-seeded.
	pct := rand.Float64()*0.5 - 0.25 // #nosec G404 -- backoff jitter, not security-sensitive
	return d + time.Duration(float64(d)*pct)
}
