package proxy

import (
	"context"
	"encoding/binary"
	"errors"
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

// Validates the reconnect/reboot RFB-handshake re-sync (audit #1). After an
// upstream drop the proxy must:
//   - emit an rfb_reset control frame to the renderer (so it recreates its
//     RFBClient and re-handshakes), and
//   - dial a FRESH upstream and begin a new RFB handshake on it (re-sending
//     ServerVersion) — rather than resuming post-handshake and forwarding the
//     new upstream's handshake bytes as RFB messages (the old silent desync).
//
// It also asserts the proxy injects the Extended Clipboard pseudo-encoding into
// the renderer's SetEncodings (the same code path runs on the reconnect
// handshake, restoring guest→host clipboard).

const rfbVersion = "RFB 003.008\n"

// 0xC0A1E5CE as a signed int32 — the encoding the proxy injects.
const extClipEncoding = int32(-0x3F5E1A32)

type nopWriter struct{}

func (nopWriter) Write(p []byte) (int, error) { return len(p), nil }

func writeBin(t *testing.T, c *websocket.Conn, b []byte) {
	t.Helper()
	_ = c.SetWriteDeadline(time.Now().Add(3 * time.Second))
	if err := c.WriteMessage(websocket.BinaryMessage, b); err != nil {
		t.Fatalf("write: %v", err)
	}
}

func readAny(t *testing.T, c *websocket.Conn) (int, []byte) {
	t.Helper()
	_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))
	mt, data, err := c.ReadMessage()
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	return mt, data
}

// readBin returns the next BINARY frame, skipping interleaved text control
// frames (connected / resumed / heartbeat / …).
func readBin(t *testing.T, c *websocket.Conn) []byte {
	t.Helper()
	for {
		if mt, data := readAny(t, c); mt == websocket.BinaryMessage {
			return data
		}
	}
}

func playServerHandshake(t *testing.T, c *websocket.Conn) {
	t.Helper()
	writeBin(t, c, []byte(rfbVersion)) // ServerVersion
	readBin(t, c)                      // ClientVersion
	writeBin(t, c, []byte{1, 1})       // SecurityTypes: count=1, None(1)
	readBin(t, c)                      // SecurityChoice
	writeBin(t, c, []byte{0, 0, 0, 0}) // SecurityResult: OK
	readBin(t, c)                      // ClientInit
	si := make([]byte, 24)
	binary.BigEndian.PutUint16(si[0:2], 1024)
	binary.BigEndian.PutUint16(si[2:4], 768)
	si[4] = 32
	binary.BigEndian.PutUint32(si[20:24], 4)
	si = append(si, []byte("test")...)
	writeBin(t, c, si) // ServerInit
}

// finishClientHandshake plays the renderer half after ServerVersion was read.
func finishClientHandshake(t *testing.T, c *websocket.Conn) {
	t.Helper()
	writeBin(t, c, []byte(rfbVersion))             // ClientVersion
	readBin(t, c)                                  // SecurityTypes
	writeBin(t, c, []byte{1})                      // SecurityChoice
	readBin(t, c)                                  // SecurityResult
	writeBin(t, c, []byte{1})                      // ClientInit
	readBin(t, c)                                  // ServerInit
	writeBin(t, c, []byte{2, 0, 0, 1, 0, 0, 0, 0}) // SetEncodings [Raw]
}

func setEncodingsHasExtClip(b []byte) bool {
	if len(b) < 4 || b[0] != 2 {
		return false
	}
	count := int(binary.BigEndian.Uint16(b[2:4]))
	for i := 0; i < count; i++ {
		off := 4 + i*4
		if off+4 > len(b) {
			break
		}
		if int32(binary.BigEndian.Uint32(b[off:off+4])) == extClipEncoding {
			return true
		}
	}
	return false
}

func TestReconnectReHandshakesAndEmitsRFBReset(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(nopWriter{}, &slog.HandlerOptions{Level: slog.LevelError}))
	up := &websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}

	var dials int32
	var extClipInjected atomic.Bool
	dialed := make(chan int32, 8) // signaled when the proxy dials an upstream
	firstHandshakeDone := make(chan struct{})
	dropFirst := make(chan struct{})

	upstreamSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer c.Close()
		n := atomic.AddInt32(&dials, 1)
		dialed <- n
		if n == 1 {
			// First connection: complete the handshake + verify the proxy
			// injected Extended Clipboard into the renderer's SetEncodings.
			playServerHandshake(t, c)
			if _, se, err := c.ReadMessage(); err == nil && setEncodingsHasExtClip(se) {
				extClipInjected.Store(true)
			}
			close(firstHandshakeDone)
			<-dropFirst // hold open until the test drops us
			return      // closing simulates the guest/upstream dropping
		}
		// Reconnect: send ServerVersion to prove the proxy re-runs the
		// handshake on the fresh upstream, then hold briefly. (We don't fully
		// replay here — the assertions are "rfb_reset emitted" + "re-dialed".)
		writeBin(t, c, []byte(rfbVersion))
		_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))
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

	// Initial handshake (renderer half). readBin skips the "connected" text frame.
	if n := <-dialed; n != 1 {
		t.Fatalf("expected first dial, got %d", n)
	}
	readBin(t, client) // ServerVersion
	finishClientHandshake(t, client)
	<-firstHandshakeDone
	if !extClipInjected.Load() {
		t.Fatal("Extended Clipboard was NOT injected into the renderer's SetEncodings")
	}

	// Drop the first upstream → must trigger reconnect (rfb_reset + re-dial).
	close(dropFirst)

	var sawReset bool
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		_ = client.SetReadDeadline(time.Now().Add(4 * time.Second))
		mt, data, rerr := client.ReadMessage()
		if rerr != nil {
			break
		}
		if mt == websocket.TextMessage && strings.Contains(string(data), `"rfb_reset"`) {
			sawReset = true
			break
		}
	}
	if !sawReset {
		t.Fatal("proxy did not emit rfb_reset on reconnect — the renderer would never re-handshake")
	}

	// And it must have dialed a fresh upstream (the re-handshake target).
	select {
	case n := <-dialed:
		if n < 2 {
			t.Fatalf("expected reconnect dial (>=2), got %d", n)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("proxy did not re-dial a fresh upstream on reconnect")
	}
}

// isStartingErr must recognize the transient post-power-on 5xx ("VM is powered
// on but its console isn't serving yet") and nothing else — a 4xx, an auth
// error, or the powered-off condition must NOT be treated as "starting".
func TestIsStartingErr(t *testing.T) {
	cases := map[string]bool{
		"dial VNC websocket: HTTP 500":                true,
		"dial VNC websocket: HTTP 502":                true,
		"dial VNC websocket: HTTP 503":                true,
		"dial VNC websocket: HTTP 504":                true,
		"dial VNC websocket: HTTP 404":                false,
		"not authorized to open console for this VM":  false,
		"session expired: upstream returned HTTP 401": false,
		"upstream: VM is not powered on":              false,
	}
	for msg, want := range cases {
		if got := isStartingErr(errors.New(msg)); got != want {
			t.Errorf("isStartingErr(%q) = %v, want %v", msg, got, want)
		}
	}
	if isStartingErr(nil) {
		t.Error("isStartingErr(nil) = true, want false")
	}
}

// classifyFatal must map the STABLE cause strings the AHV plugin's OpenStream
// (and the runtime session layer) actually emit to the right terminal cause, so
// a deleted VM / dead cookie / bad pin / closed session fast-fails with an
// actionable overlay instead of burning the ~27-minute reconnect budget into a
// generic "unreachable". The literals here are the verbatim plugin/api strings.
func TestClassifyFatal(t *testing.T) {
	cases := map[string]string{
		// auth / credential — route to in-console SSO re-auth (F5)
		"session expired: upstream returned HTTP 401 on WS upgrade":      "session_expired",
		"session expired: VM read returned HTTP 401 after WS upgrade":    "session_expired",
		"no credentials available — sign in first":                      "session_expired",
		"not authorized to open console for this VM":                    "rbac_denied",
		// VM gone / session torn down — fast-fail, no retry budget
		"vm not found: 5f3c-...":                                        "vm_not_found",
		"session closed":                                                "session_closed",
		"unknown session: abc":                                          "session_closed",
		// permanent, DETERMINISTIC fail-closed config faults — terminal
		"pinned SPKI mismatch for https://pc.example":                   "config_error",
		`endpoint "pc": TLS verification disabled without a pinned certificate — refusing`: "config_error",
		// transient — must NOT be treated as terminal. A generic Go-stack x509/cert
		// error on a mid-session re-dial (LB failover, post-wake clock skew on a
		// non-pinned endpoint) is recoverable and must stay retryable, NOT terminal.
		"x509: certificate signed by unknown authority":                 "",
		"dial VNC websocket: HTTP 500":                                  "",
		"dial VNC websocket: HTTP 404":                                  "",
		"connection refused":                                            "",
		"vm powered off: console upgrade rejected while VM power_state=OFF": "",
	}
	for msg, want := range cases {
		got, code := classifyFatal(errors.New(msg))
		if got != want {
			t.Errorf("classifyFatal(%q) cause = %q, want %q", msg, got, want)
		}
		if want == "" && code != 0 {
			t.Errorf("classifyFatal(%q) transient but code = %d, want 0", msg, code)
		}
		if want != "" && code == 0 {
			t.Errorf("classifyFatal(%q) terminal but code = 0", msg)
		}
	}
	if c, _ := classifyFatal(nil); c != "" {
		t.Errorf("classifyFatal(nil) = %q, want empty", c)
	}
}

// A transient upstream 5xx after power-on must surface a calm vm_starting state
// and keep retrying — NOT flap an alarming reconnect or go fatal during the
// VM-warming-up window (the bug behind the "dial VNC websocket: HTTP 500 →
// console handshake could not complete" flap).
func TestUpstream5xxEmitsVMStartingNotFatal(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(nopWriter{}, &slog.HandlerOptions{Level: slog.LevelError}))
	up := &websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}

	proxySrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		conn.SetReadLimit(maxMessageSize)
		p := &proxy{log: logger, sess: &plugins.Session{}, client: conn, stream: rfb.NewStreamProcessor()}
		// Console not serving yet: every dial returns a transient 5xx.
		p.openUpstream = func(ctx context.Context) (*websocket.Conn, error) {
			return nil, errors.New("dial VNC websocket: HTTP 500")
		}
		p.run(r.Context())
	}))
	defer proxySrv.Close()

	client, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(proxySrv.URL, "http"), nil)
	if err != nil {
		t.Fatalf("client dial: %v", err)
	}
	defer client.Close()

	var sawStarting, sawFatal bool
	// Scan every control frame the proxy emits during a 2s warming window using
	// a SINGLE read deadline, and STOP on the first read error. gorilla panics
	// after repeated reads on a failed connection; the old per-iteration 1s
	// deadline + `continue` spun ReadMessage thousands of times once the
	// deadline set readErr, tripping that panic ("repeated read on failed
	// websocket connection"). Blocking once until the 2s deadline still sees
	// all frames, so a stray "fatal" in the window is still caught.
	_ = client.SetReadDeadline(time.Now().Add(2 * time.Second))
	for {
		mt, data, rerr := client.ReadMessage()
		if rerr != nil {
			break
		}
		if mt == websocket.TextMessage {
			s := string(data)
			if strings.Contains(s, `"vm_starting"`) {
				sawStarting = true
			}
			if strings.Contains(s, `"fatal"`) {
				sawFatal = true
			}
		}
	}
	if !sawStarting {
		t.Fatal("expected a vm_starting control frame while the upstream returns transient 5xx")
	}
	if sawFatal {
		t.Fatal("must NOT go fatal during the VM-starting (console warming up) window")
	}
}
