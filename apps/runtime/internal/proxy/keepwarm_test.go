package proxy

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/MarcoCarvin/ahv-remote-console/apps/runtime/internal/plugins"
	"github.com/MarcoCarvin/ahv-remote-console/apps/runtime/internal/rfb"
)

// newKeepwarmProxy returns a directly-constructed proxy wired to a fake
// upstream WS that forwards every binary message it receives to the returned
// channel.
func newKeepwarmProxy(t *testing.T) (*proxy, chan []byte) {
	t.Helper()
	got := make(chan []byte, 16)
	up := &websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		for {
			mt, data, err := c.ReadMessage()
			if err != nil {
				return
			}
			if mt == websocket.BinaryMessage {
				got <- data
			}
		}
	}))
	t.Cleanup(srv.Close)

	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if err != nil {
		t.Fatalf("dial fake upstream: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	logger := slog.New(slog.NewTextHandler(nopWriter{}, &slog.HandlerOptions{Level: slog.LevelError}))
	p := &proxy{log: logger, sess: &plugins.Session{ID: "sess-keepwarm"}, stream: rfb.NewStreamProcessor()}
	p.upstream.Store(conn)
	return p, got
}

// recvCutText reads one upstream binary message and asserts it is a legacy
// ClientCutText, returning its text.
func recvCutText(t *testing.T, got chan []byte) string {
	t.Helper()
	select {
	case data := <-got:
		if len(data) < 8 || data[0] != rfb.MsgClientCutText {
			t.Fatalf("upstream got %v, want a ClientCutText", data)
		}
		return string(data[8:])
	case <-time.After(2 * time.Second):
		t.Fatal("no upstream message within 2s")
		return ""
	}
}

func recvNone(t *testing.T, got chan []byte) {
	t.Helper()
	select {
	case data := <-got:
		t.Fatalf("unexpected upstream message: %q", data)
	case <-time.After(150 * time.Millisecond):
	}
}

// TestKeepwarmProbeAckLeaseFlow pins the full sender state machine: attach
// sends ONE resize-namespace probe (paced — an immediate retry sends
// nothing), the agent's ack flips the sender to lease mode, and the next
// tick sends a real lease token with the renewal pacing armed.
func TestKeepwarmProbeAckLeaseFlow(t *testing.T) {
	p, got := newKeepwarmProxy(t)

	p.maybeSendKeepwarm()
	if text := recvCutText(t, got); text != keepwarmProbeToken {
		t.Fatalf("first send = %q, want the probe token", text)
	}
	// Paced: nothing more until the next interval. While UNACKED the cadence is
	// the fast cold interval (cold-attach engagement), NOT the 20s renew.
	p.maybeSendKeepwarm()
	recvNone(t, got)
	next := p.keepwarmNextSend.Load()
	if next < time.Now().Add(keepwarmColdInterval-time.Second).UnixNano() {
		t.Fatal("cold-probe pacing not armed after the probe")
	}
	if next > time.Now().Add(keepwarmRenewInterval-time.Second).UnixNano() {
		t.Fatal("unacked pacing should be the fast cold interval, not the renew interval")
	}

	// The agent acks via an ext-clip PROVIDE: swallowed (nil), never counted
	// as operator clipboard activity, and the sender flips to lease mode.
	if out := p.handleServerBinary(buildExtClipProvide(t, keepwarmAckText)); out != nil {
		t.Fatalf("ack PROVIDE forwarded to the renderer: %v", out)
	}
	if !p.keepwarmAcked.Load() {
		t.Fatal("ack not latched")
	}
	if p.lastClipActivity.Load() != 0 {
		t.Fatal("ack counted as operator clipboard activity")
	}

	p.maybeSendKeepwarm()
	if text := recvCutText(t, got); text != keepwarmLeaseToken {
		t.Fatalf("post-ack send = %q, want the lease token", text)
	}
	// Now that the agent acked, pacing relaxes to the 20s lease-renewal cadence.
	if next := p.keepwarmNextSend.Load(); next < time.Now().Add(keepwarmRenewInterval-time.Second).UnixNano() {
		t.Fatal("renewal pacing not armed after the lease send")
	}
}

// TestKeepwarmConditionA pins the anti-starvation gates: a mid-dance
// guest->host transfer (armed clipReqDeadline) HARD-BLOCKS sends, and fresh
// real clipboard activity defers them; both release once clear.
func TestKeepwarmConditionA(t *testing.T) {
	p, got := newKeepwarmProxy(t)

	// Hard-block: an Extended Clipboard REQUEST is awaiting its PROVIDE.
	p.clipReqDeadline.Store(time.Now().Add(clipRequestTimeout).UnixNano())
	p.maybeSendKeepwarm()
	recvNone(t, got)

	// Deferral: transfer done, but real clipboard traffic is <3s fresh.
	p.clipReqDeadline.Store(0)
	p.markClipActivity()
	p.maybeSendKeepwarm()
	recvNone(t, got)

	// Quiet long enough — the deferred send goes out on the next tick.
	p.lastClipActivity.Store(time.Now().Add(-keepwarmClipQuiet - time.Second).UnixNano())
	p.maybeSendKeepwarm()
	if text := recvCutText(t, got); text != keepwarmProbeToken {
		t.Fatalf("send after quiet window = %q, want the probe token", text)
	}
}

// TestKeepwarmSkewSafetyProbeCap: without an ack, the sender emits at most
// keepwarmMaxProbes probes per connection and NEVER a __AHVRC_KEEPWARM__
// token — a stale agent must never see a lease token (it would land in the
// guest clipboard).
func TestKeepwarmSkewSafetyProbeCap(t *testing.T) {
	p, got := newKeepwarmProxy(t)

	for i := 0; i < keepwarmMaxProbes; i++ {
		p.keepwarmNextSend.Store(0) // force past pacing
		p.maybeSendKeepwarm()
		if text := recvCutText(t, got); text != keepwarmProbeToken {
			t.Fatalf("unacked send %d = %q, want the probe token", i+1, text)
		}
	}
	p.keepwarmNextSend.Store(0)
	p.maybeSendKeepwarm()
	recvNone(t, got) // probe budget exhausted, still no lease token
}

// TestKeepwarmDetachCancel: on stream detach the cancel token goes out ONLY
// once the agent acked (a stale agent would write it to the guest clipboard);
// before the ack, detach sends nothing.
func TestKeepwarmDetachCancel(t *testing.T) {
	p, got := newKeepwarmProxy(t)
	u := p.upstream.Load()

	p.sendKeepwarmCancel(u)
	recvNone(t, got)

	p.keepwarmAcked.Store(true)
	p.sendKeepwarmCancel(u)
	if text := recvCutText(t, got); text != keepwarmCancelToken {
		t.Fatalf("detach sent %q, want the cancel token", text)
	}
}

// TestKeepwarmAckLegacySwallowedAndRealCopyForwarded: an ack delivered as a
// LEGACY ServerCutText is swallowed too, while a real legacy guest copy is
// forwarded untouched and marks clipboard activity (Condition A input).
func TestKeepwarmAckLegacySwallowedAndRealCopyForwarded(t *testing.T) {
	p, _ := newKeepwarmProxy(t)

	if out := p.handleServerBinary(rfb.BuildServerCutText([]byte(keepwarmAckText))); out != nil {
		t.Fatalf("legacy ack forwarded to the renderer: %v", out)
	}
	if !p.keepwarmAcked.Load() {
		t.Fatal("legacy ack not latched")
	}

	real := rfb.BuildServerCutText([]byte("operator copy"))
	out := p.handleServerBinary(real)
	if string(out) != string(real) {
		t.Fatalf("real legacy copy not forwarded untouched: %v", out)
	}
	if p.lastClipActivity.Load() == 0 {
		t.Fatal("real guest copy did not mark clipboard activity")
	}
}

// TestKeepwarmSiblingEchoSwallowed: QEMU's clipboard manager broadcasts one
// client's cut-text to every attached peer — a SIBLING console's keep-warm
// tokens arriving from upstream must be swallowed in both delivery formats,
// without marking clipboard activity (a sibling's 20s renewals would
// perpetually defer our own sends via Condition A) and without latching
// keepwarmAcked (only the genuine agent ack proves capability).
func TestKeepwarmSiblingEchoSwallowed(t *testing.T) {
	p, _ := newKeepwarmProxy(t)

	for _, tok := range []string{keepwarmLeaseToken, keepwarmCancelToken, keepwarmProbeToken} {
		if out := p.handleServerBinary(buildExtClipProvide(t, tok)); out != nil {
			t.Fatalf("ext-clip echo %q forwarded to the renderer: %v", tok, out)
		}
		if out := p.handleServerBinary(rfb.BuildServerCutText([]byte(tok))); out != nil {
			t.Fatalf("legacy echo %q forwarded to the renderer: %v", tok, out)
		}
	}
	if p.lastClipActivity.Load() != 0 {
		t.Fatal("sibling echo counted as operator clipboard activity")
	}
	if p.keepwarmAcked.Load() {
		t.Fatal("sibling echo latched keepwarmAcked")
	}
}

// TestKeepwarmDetachCancelWiredIntoRunLoop: closing the console (client ws)
// must actually deliver the cancel token to the still-open upstream — pinning
// the run()-loop call site, not just the sendKeepwarmCancel helper (whose
// original defer-only wiring always saw an already-nil upstream).
func TestKeepwarmDetachCancelWiredIntoRunLoop(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(nopWriter{}, &slog.HandlerOptions{Level: slog.LevelError}))
	up := &websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}

	cutTexts := make(chan string, 32)
	upstreamSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer c.Close()
		playServerHandshake(t, c)
		// Latch keep-warm via a legacy ack, then collect every ClientCutText
		// the proxy sends (probes, leases, and — the assertion — the cancel).
		writeBin(t, c, rfb.BuildServerCutText([]byte(keepwarmAckText)))
		for {
			_ = c.SetReadDeadline(time.Now().Add(10 * time.Second))
			mt, data, rerr := c.ReadMessage()
			if rerr != nil {
				return
			}
			if mt == websocket.BinaryMessage && len(data) >= 8 && data[0] == rfb.MsgClientCutText {
				cutTexts <- string(data[8:])
			}
		}
	}))
	defer upstreamSrv.Close()

	dialUpstream := func(ctx context.Context) (*websocket.Conn, error) {
		c, _, err := websocket.DefaultDialer.DialContext(ctx, "ws"+strings.TrimPrefix(upstreamSrv.URL, "http"), nil)
		return c, err
	}

	proxySrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		conn.SetReadLimit(maxMessageSize)
		p := &proxy{log: logger, sess: &plugins.Session{}, client: conn, stream: rfb.NewStreamProcessor()}
		p.openUpstream = dialUpstream
		p.run(r.Context())
	}))
	defer proxySrv.Close()

	client, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(proxySrv.URL, "http"), nil)
	if err != nil {
		t.Fatalf("client dial: %v", err)
	}
	defer client.Close()

	readBin(t, client) // ServerVersion
	finishClientHandshake(t, client)

	// Wait for a lease token: proves the ack was processed and the sender is
	// in lease mode (the cancel is acked-gated, so closing earlier would
	// legitimately send nothing).
	leaseDeadline := time.After(8 * time.Second)
	for leased := false; !leased; {
		select {
		case text := <-cutTexts:
			leased = text == keepwarmLeaseToken
		case <-leaseDeadline:
			t.Fatal("upstream never received a lease token after the ack")
		}
	}

	// Operator closes the console: the run loop must cancel the lease on the
	// still-open upstream before closing it.
	_ = client.WriteMessage(websocket.CloseMessage,
		websocket.FormatCloseMessage(websocket.CloseNormalClosure, "console closed"))
	_ = client.Close()

	cancelDeadline := time.After(8 * time.Second)
	for {
		select {
		case text := <-cutTexts:
			if text == keepwarmCancelToken {
				return // wired correctly
			}
		case <-cancelDeadline:
			t.Fatal("upstream never received the keep-warm cancel token after client close")
		}
	}
}
