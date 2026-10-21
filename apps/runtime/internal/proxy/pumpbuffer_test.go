package proxy

import (
	"bytes"
	"context"
	"encoding/binary"
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

// The upstream→client pump reuses ONE read buffer across loop iterations
// (allocating a fresh buffer per FramebufferUpdate was ~12MB/s of churn at
// 30fps motion). The contract that makes reuse safe is single ownership:
// each frame is fully written to the client before the next read overwrites
// the buffer, and any rewritten frame (ext-clip translation) is a freshly
// allocated slice. This test drives FBU → ext-clip PROVIDE → FBU through the
// REAL pump path and asserts both FBUs arrive byte-for-byte intact around the
// translated clipboard frame — a missing Reset() concatenates frames, and any
// aliasing/retention across iterations corrupts the earlier FBU's bytes.
func TestPumpForwardsFBUsIntactAcrossInterleavedExtClipTranslation(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(nopWriter{}, &slog.HandlerOptions{Level: slog.LevelError}))
	up := &websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}

	// Two FBUs with distinct sizes and pixel patterns so stale-buffer reuse
	// (frame 2 carrying frame 1's tail) cannot pass the equality checks.
	px1 := make([]byte, 64*64*4)
	for i := range px1 {
		px1[i] = byte(i*7 + 1)
	}
	fbu1 := buildRawFBUpdate(0, 0, 64, 64, px1)
	px2 := make([]byte, 8*8*4)
	for i := range px2 {
		px2[i] = byte(0xC3 - i)
	}
	fbu2 := buildRawFBUpdate(4, 4, 8, 8, px2)
	const clipText = "guest-copy-between-frames"
	provide := buildExtClipProvide(t, clipText)

	release := make(chan struct{})
	upstreamSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer c.Close()
		playServerHandshake(t, c)
		// Drain the renderer's (rewritten) SetEncodings so the handshake settles.
		_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))
		_, _, _ = c.ReadMessage()
		writeBin(t, c, fbu1)
		writeBin(t, c, provide)
		writeBin(t, c, fbu2)
		<-release // hold open so the proxy doesn't start reconnecting mid-assert
	}))
	defer upstreamSrv.Close()
	defer close(release)

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

	got1 := readBin(t, client)
	if !bytes.Equal(got1, fbu1) {
		t.Fatalf("first FBU corrupted: got %d bytes, want %d intact", len(got1), len(fbu1))
	}
	got2 := readBin(t, client)
	if len(got2) < 8 || got2[0] != rfb.MsgServerCutText {
		t.Fatalf("expected translated legacy ServerCutText between the FBUs, got %v…", got2[:min(len(got2), 8)])
	}
	if l := int32(binary.BigEndian.Uint32(got2[4:8])); l < 0 || string(got2[8:]) != clipText {
		t.Fatalf("ext-clip PROVIDE not translated to legacy ServerCutText: len=%d text=%q", l, got2[8:])
	}
	got3 := readBin(t, client)
	if !bytes.Equal(got3, fbu2) {
		t.Fatalf("second FBU corrupted after interleaved translation: got %d bytes, want %d intact", len(got3), len(fbu2))
	}
}

// frame_too_large contract: an upstream frame over the connection's read limit
// must classify EXACTLY as before — websocket.ErrReadLimit from the read path
// (whether it surfaces from the first frame's header or a continuation read),
// a terminal "fatal" control frame with cause frame_too_large, and a WS close
// 1009 — never a reconnect loop. Runs the pump directly against a small read
// limit so the test doesn't need a >160MB frame.
func TestUpstreamFrameOverReadLimitClassifiesFrameTooLarge(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(nopWriter{}, &slog.HandlerOptions{Level: slog.LevelError}))
	up := &websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	const testLimit = 32 * 1024

	release := make(chan struct{})
	upstreamSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer c.Close()
		writeBin(t, c, make([]byte, testLimit+1))
		<-release
	}))
	defer upstreamSrv.Close()
	defer close(release)

	proxySrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		uc, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(upstreamSrv.URL, "http"), nil)
		if err != nil {
			return
		}
		defer uc.Close()
		uc.SetReadLimit(testLimit)
		p := &proxy{log: logger, sess: &plugins.Session{}, client: conn, stream: rfb.NewStreamProcessor()}
		p.upstream.Store(uc)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		p.wg.Add(1)
		p.upstreamToClient(ctx, cancel)
		if !p.clientClosed.Load() {
			t.Error("read-limit overflow must mark clientClosed (terminal), not leave the session reconnectable")
		}
	}))
	defer proxySrv.Close()

	client, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(proxySrv.URL, "http"), nil)
	if err != nil {
		t.Fatalf("client dial: %v", err)
	}
	defer client.Close()

	var sawFatal bool
	_ = client.SetReadDeadline(time.Now().Add(5 * time.Second))
	for {
		mt, data, rerr := client.ReadMessage()
		if rerr != nil {
			if !websocket.IsCloseError(rerr, websocket.CloseMessageTooBig) {
				t.Fatalf("expected WS close %d (message too big), got %v", websocket.CloseMessageTooBig, rerr)
			}
			break
		}
		if mt == websocket.TextMessage &&
			strings.Contains(string(data), `"fatal"`) && strings.Contains(string(data), `"frame_too_large"`) {
			sawFatal = true
		}
	}
	if !sawFatal {
		t.Fatal(`expected a "fatal" control frame with cause "frame_too_large" before the close`)
	}
}
