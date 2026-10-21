package proxy

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/MarcoCarvin/ahv-remote-console/apps/runtime/internal/plugins"
	"github.com/MarcoCarvin/ahv-remote-console/apps/runtime/internal/rfb"
)

// REL-01: the upstream Prism VNC conn must carry a rolling read deadline so a
// half-open upstream — one that completes the handshake then goes silent,
// never sending a frame or answering a ping — trips a read error within
// ~upstreamReadDeadline and run() re-dials, instead of freezing the console
// forever while the UI still shows "connected".
//
// Before the fix the upstream conn had NO read deadline: dial #1 stayed open
// indefinitely and the proxy never re-dialed. After the fix the watchdog fires
// and the proxy dials a fresh upstream (dial #2).
func TestUpstreamHalfOpenTripsReadDeadlineAndRedials(t *testing.T) {
	// Shrink the watchdog so the silent upstream trips quickly; restore after.
	prev := upstreamReadDeadline
	upstreamReadDeadline = 300 * time.Millisecond
	defer func() { upstreamReadDeadline = prev }()

	logger := slog.New(slog.NewTextHandler(nopWriter{}, &slog.HandlerOptions{Level: slog.LevelError}))
	up := &websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}

	var dials int32
	dialed := make(chan int32, 8)
	releaseSilent := make(chan struct{})

	upstreamSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer c.Close()
		n := atomic.AddInt32(&dials, 1)
		dialed <- n
		if n == 1 {
			// Dial #1: complete the handshake, prove the upstream pongs ONCE so the
			// half-open watchdog arms (REL-01 hardening only enforces the read
			// deadline after a pong is seen — otherwise a never-ponging upstream
			// must NOT reconnect a healthy idle VM), then go SILENT — stop ponging
			// and never send a frame. The watchdog must now trip and force a re-dial.
			playServerHandshake(t, c)
			// Swallow the renderer's SetEncodings so the handshake settles.
			_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
			_, _, _ = c.ReadMessage()
			// Stop auto-ponging further pings, then send ONE unsolicited pong to
			// arm the proxy's watchdog (upstreamPongSeen); after that, silence is
			// the only thing left and the rolling read deadline must fire.
			c.SetPingHandler(func(string) error { return nil })
			_ = c.WriteControl(websocket.PongMessage,
				[]byte(time.Now().Format(time.RFC3339Nano)),
				time.Now().Add(time.Second))
			<-releaseSilent // hold open (alive TCP, dead app layer) until teardown
			return
		}
		// Dial #2 (the re-dial we are asserting): send ServerVersion to prove
		// the proxy re-handshakes on the fresh upstream, then hold briefly.
		writeBin(t, c, []byte(rfbVersion))
		_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
		_, _, _ = c.ReadMessage()
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
	defer close(releaseSilent)

	// Drive the renderer half of the first handshake.
	if n := <-dialed; n != 1 {
		t.Fatalf("expected first dial, got %d", n)
	}
	readBin(t, client) // ServerVersion
	finishClientHandshake(t, client)

	// Keep reading client frames in the background so the loopback ws stays
	// healthy (otherwise the client write buffer could stall the proxy). We are
	// only asserting on the UPSTREAM re-dial.
	go func() {
		for {
			_ = client.SetReadDeadline(time.Now().Add(5 * time.Second))
			if _, _, e := client.ReadMessage(); e != nil {
				return
			}
		}
	}()

	// The silent upstream must trip the read deadline and force a re-dial.
	// Bound generously relative to the shrunk watchdog.
	select {
	case n := <-dialed:
		if n < 2 {
			t.Fatalf("expected re-dial (>=2), got %d", n)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("upstream half-open never tripped the read deadline — proxy did not re-dial")
	}
}

// TestDialUpstreamResetsRTT: every (re)dial must reset upstreamRTTMicros to 0
// ("unmeasured"). Without this, the first post-handshake heartbeat carries the
// PRIOR connection's last RTT — after a spike that is e.g. 113000ms, which both
// freezes the latency badge and demotes the renderer quality tier, pinning the
// FRESH connection to sticky 16bpp for its whole life.
func TestDialUpstreamResetsRTT(t *testing.T) {
	up := &websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	upstreamSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		// Hold the conn open briefly; dialUpstream only dials + sets up.
		_ = c.SetReadDeadline(time.Now().Add(time.Second))
		_, _, _ = c.ReadMessage()
		_ = c.Close()
	}))
	defer upstreamSrv.Close()

	logger := slog.New(slog.NewTextHandler(nopWriter{}, &slog.HandlerOptions{Level: slog.LevelError}))
	p := &proxy{log: logger, sess: &plugins.Session{}, stream: rfb.NewStreamProcessor()}
	p.openUpstream = func(ctx context.Context) (*websocket.Conn, error) {
		c, _, err := websocket.DefaultDialer.DialContext(ctx, "ws"+strings.TrimPrefix(upstreamSrv.URL, "http"), nil)
		return c, err
	}

	// Simulate a stale RTT left over from a prior, spiking connection.
	p.upstreamRTTMicros.Store(113_000_000)

	if err := p.dialUpstream(context.Background()); err != nil {
		t.Fatalf("dialUpstream: %v", err)
	}
	defer func() {
		if u := p.upstream.Load(); u != nil {
			_ = u.Close()
		}
	}()

	if got := p.upstreamRTTMicros.Load(); got != 0 {
		t.Errorf("upstreamRTTMicros after re-dial = %d, want 0 (must reset, not inherit the stale spike)", got)
	}
}

// REL-02: graceful shutdown cancels active proxy sessions. The IPC router roots
// each proxy session's parent context in the host's shutdown context; when that
// is cancelled (shutdown start) the proxy pumps — even though blocked on
// ReadMessage on a healthy, quiet connection — must unblock and run() must
// return PROMPTLY rather than waiting out the connection's natural lifetime.
func TestParentCancelTearsDownActiveSession(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(nopWriter{}, &slog.HandlerOptions{Level: slog.LevelError}))
	up := &websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}

	releaseUpstream := make(chan struct{})
	upstreamSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer c.Close()
		// Healthy, quiet upstream: complete the handshake then hold the conn open
		// and silent (no frames) until the test lets it go. The pump is blocked on
		// ReadMessage here — only the parent-cancel watcher can unblock it.
		playServerHandshake(t, c)
		_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
		_, _, _ = c.ReadMessage()
		<-releaseUpstream
	}))
	defer upstreamSrv.Close()
	defer close(releaseUpstream)

	dialUpstream := func(ctx context.Context) (*websocket.Conn, error) {
		c, _, err := websocket.DefaultDialer.DialContext(ctx, "ws"+strings.TrimPrefix(upstreamSrv.URL, "http"), nil)
		return c, err
	}

	runReturned := make(chan struct{})
	// parentCtx stands in for the router's host-rooted parent context.
	parentCtx, cancelParent := context.WithCancel(context.Background())
	defer cancelParent()

	proxySrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		conn.SetReadLimit(maxMessageSize)
		p := &proxy{log: logger, sess: &plugins.Session{}, client: conn, stream: rfb.NewStreamProcessor()}
		p.openUpstream = dialUpstream
		p.run(parentCtx)
		close(runReturned)
	}))
	defer proxySrv.Close()

	client, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(proxySrv.URL, "http"), nil)
	if err != nil {
		t.Fatalf("client dial: %v", err)
	}
	defer client.Close()

	// Drive the renderer half of the handshake so the pumps are running and
	// blocked on a healthy, quiet connection.
	readBin(t, client) // ServerVersion
	finishClientHandshake(t, client)

	// Let the session settle into steady state, then simulate shutdown.
	time.Sleep(150 * time.Millisecond)
	cancelParent()

	select {
	case <-runReturned:
		// good — run() unwound promptly on parent cancel.
	case <-time.After(3 * time.Second):
		t.Fatal("run() did not return promptly after parent cancel — shutdown would wait out the full grace on a live console")
	}
}
