package proxy

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"log/slog"
	"testing"

	"github.com/MarcoCarvin/ahv-remote-console/apps/runtime/internal/plugins"
	"github.com/MarcoCarvin/ahv-remote-console/apps/runtime/internal/rfb"
)

// buildExtClipProvide builds an Extended Clipboard PROVIDE ServerCutText (the
// server delivering clipboard text): a ServerCutText with NEGATIVE length whose
// payload is [4 flags=PROVIDE|TEXT][zlib( [4-byte BE textLen][text] )]. This is
// the exact shape TranslateExtClipServerCutText decodes.
func buildExtClipProvide(t *testing.T, text string) []byte {
	t.Helper()
	var zbuf bytes.Buffer
	zw := zlib.NewWriter(&zbuf)
	hdr := make([]byte, 4)
	binary.BigEndian.PutUint32(hdr, uint32(len(text)))
	if _, err := zw.Write(hdr); err != nil {
		t.Fatalf("zlib write hdr: %v", err)
	}
	if _, err := zw.Write([]byte(text)); err != nil {
		t.Fatalf("zlib write text: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("zlib close: %v", err)
	}

	payload := make([]byte, 4+zbuf.Len())
	binary.BigEndian.PutUint32(payload[0:4], rfb.ExtClipProvide|rfb.ExtClipText)
	copy(payload[4:], zbuf.Bytes())

	msg := make([]byte, 8+len(payload))
	msg[0] = rfb.MsgServerCutText
	binary.BigEndian.PutUint32(msg[4:8], uint32(-int32(len(payload)))) // negative length = ext-clip
	copy(msg[8:], payload)
	return msg
}

// PROT-07 REGRESSION GUARD: a FramebufferUpdate WS frame must be forwarded RAW,
// byte-for-byte, ALWAYS — even when an ext-clip ServerCutText is coalesced into
// the same frame. PROT-07 once gated the FBU fast-path on a full reparse to
// translate the trailing PROVIDE, but AHV's Tight FBUs are not reframable at the
// proxy, so that gate diverted EVERY real frame into a mangling path → the
// renderer's "stream desync, realigning" reconnect loop. The proxy is a
// passthrough: it never withholds or rewrites an FBU. (A coalesced trailing
// ext-clip is intentionally NOT translated — AHV sends each RFB message in its
// own WS frame; withholding video to chase a never-observed coalesced clipboard
// copy is the wrong trade.)
func TestHandleServerBinaryCoalescedFBUAndExtClip(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(nopWriter{}, &slog.HandlerOptions{Level: slog.LevelError}))
	p := &proxy{log: logger, sess: &plugins.Session{ID: "sess-prot07"}, stream: rfb.NewStreamProcessor()}

	// A small Raw FBU (1x1, 4bpp) followed immediately by an ext-clip PROVIDE.
	fbu := buildRawFBUpdate(0, 0, 1, 1, make([]byte, 4))
	provide := buildExtClipProvide(t, "copied-from-guest")
	frame := append(append([]byte{}, fbu...), provide...)

	out := p.handleServerBinary(frame)
	// The whole frame must be forwarded byte-for-byte: an FBU is never withheld
	// or reframed (that withholding was the live desync regression).
	if !bytes.Equal(out, frame) {
		t.Fatalf("FBU frame not forwarded raw: got %d bytes, want the original %d-byte frame", len(out), len(frame))
	}
}

// buildRawFBUpdate mirrors the rfb test helper (1 Raw rect) for proxy-package use.
func buildRawFBUpdate(x, y, w, h uint16, pixels []byte) []byte {
	buf := make([]byte, 4+12+len(pixels))
	buf[0] = rfb.MsgFramebufferUpdate
	binary.BigEndian.PutUint16(buf[2:4], 1) // nRects
	binary.BigEndian.PutUint16(buf[4:6], x)
	binary.BigEndian.PutUint16(buf[6:8], y)
	binary.BigEndian.PutUint16(buf[8:10], w)
	binary.BigEndian.PutUint16(buf[10:12], h)
	binary.BigEndian.PutUint32(buf[12:16], uint32(rfb.EncRaw))
	copy(buf[16:], pixels)
	return buf
}

// PROT-07 (negative control): a frame that is EXACTLY one FBU with no trailing
// bytes must still take the cheap fast-path and be forwarded raw, unchanged.
func TestHandleServerBinaryLoneFBUFastPath(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(nopWriter{}, &slog.HandlerOptions{Level: slog.LevelError}))
	p := &proxy{log: logger, sess: &plugins.Session{ID: "sess-prot07b"}, stream: rfb.NewStreamProcessor()}

	fbu := buildRawFBUpdate(0, 0, 2, 2, make([]byte, 2*2*4))
	out := p.handleServerBinary(fbu)
	if out == nil {
		t.Fatal("lone FBU returned nil — must be forwarded")
	}
	if !bytes.Equal(out, fbu) {
		t.Fatal("lone FBU was modified — fast path must forward it unchanged")
	}
}

// PROT-05: after the renderer's SetEncodings is injected with the Extended
// Clipboard pseudo-encoding, the proxy must PROACTIVELY advertise our ext-clip
// CAPS upstream ONCE (QEMU may never send CAPS first, leaving the PROVIDE path
// disabled). The CAPS rides upstream in the same rewritten frame, immediately
// AFTER the injected SetEncodings. A second SetEncodings must NOT re-send it.
func TestProactiveExtClipCapsAfterFirstSetEncodings(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(nopWriter{}, &slog.HandlerOptions{Level: slog.LevelError}))
	p := &proxy{log: logger, sess: &plugins.Session{ID: "sess-prot05"}, stream: rfb.NewStreamProcessor()}

	// Renderer SetEncodings advertising one (Raw) encoding — triggers injection.
	setEnc := []byte{rfb.MsgSetEncodings, 0, 0, 1, 0, 0, 0, 0}
	out := p.handleClientBinary(setEnc)
	if out == nil {
		t.Fatal("SetEncodings was not rewritten (no ext-clip injection)")
	}

	// The rewritten upstream frame must be: injected SetEncodings (with the
	// ext-clip encoding) THEN the proactive CAPS ClientCutText.
	msgs := splitClientMessages(t, out)
	if len(msgs) != 2 {
		t.Fatalf("rewritten frame has %d messages, want 2 (SetEncodings + CAPS): %v", len(msgs), out)
	}
	if !setEncodingsHasExtClip(msgs[0]) {
		t.Fatal("first message is not a SetEncodings carrying the ext-clip encoding")
	}
	if !isExtClipCaps(msgs[1]) {
		t.Fatalf("second message is not a proactive ext-clip CAPS ClientCutText: %v", msgs[1])
	}

	// A second SetEncodings must NOT re-append CAPS (once per connection).
	out2 := p.handleClientBinary(setEnc)
	if out2 == nil {
		t.Fatal("second SetEncodings was not rewritten")
	}
	msgs2 := splitClientMessages(t, out2)
	if len(msgs2) != 1 {
		t.Fatalf("second rewritten frame has %d messages, want 1 (no repeated CAPS)", len(msgs2))
	}

	// After a reconnect resets the guard, CAPS is advertised again.
	p.extClipCapsSent.Store(false)
	out3 := p.handleClientBinary(setEnc)
	if n := len(splitClientMessages(t, out3)); n != 2 {
		t.Fatalf("post-reconnect frame has %d messages, want 2 (CAPS re-advertised)", n)
	}
}

// splitClientMessages walks a client→server byte buffer into its constituent
// RFB messages using the same parser the proxy uses.
func splitClientMessages(t *testing.T, buf []byte) [][]byte {
	t.Helper()
	var out [][]byte
	for len(buf) > 0 {
		msg, n, err := rfb.ParseClientMessage(buf)
		if err != nil {
			t.Fatalf("parse client message: %v (remaining %v)", err, buf)
		}
		out = append(out, msg.Raw)
		buf = buf[n:]
	}
	return out
}

// isExtClipCaps reports whether b is an Extended Clipboard CAPS ClientCutText:
// a ClientCutText (msg 6) with NEGATIVE length whose payload's first 4-byte
// flags word has the CAPS bit set.
func isExtClipCaps(b []byte) bool {
	if len(b) < 12 || b[0] != rfb.MsgClientCutText {
		return false
	}
	length := int32(binary.BigEndian.Uint32(b[4:8]))
	if length >= 0 {
		return false
	}
	flags := binary.BigEndian.Uint32(b[8:12])
	return flags&rfb.ExtClipCaps != 0
}

// PROT-09 REGRESSION GUARD: AHV splits a large Tight FramebufferUpdate across WS
// messages, so a CONTINUATION frame's first byte is arbitrary Tight-compressed
// data. One that coincidentally starts with 0x03 (MsgServerCutText) and a
// negative int32 at [4:8] is NOT a real Extended Clipboard message — its 3-byte
// ServerCutText padding is non-zero. It MUST be forwarded byte-for-byte. The old
// code mis-detected it as ext-clip and dropped it (return nil), silently removing
// ~16 KB of Tight pixel data and desyncing the renderer ("invalid block type" /
// "unsupported encoding" / black screen, fired randomly per redraw).
func TestContinuationFrameStartingWith03ForwardedRaw(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(nopWriter{}, &slog.HandlerOptions{Level: slog.LevelError}))
	p := &proxy{log: logger, sess: &plugins.Session{ID: "sess-prot09a"}, stream: rfb.NewStreamProcessor()}

	// 0x03, then NON-ZERO padding, then a negative int32 length, then
	// compressed-looking payload — a plausible Tight continuation frame.
	frame := []byte{
		0x03, 0xab, 0xcd, 0xef, // msg=3 but padding is NON-ZERO → not a real ServerCutText
		0xff, 0x00, 0x00, 0x01, // length high bit set → would-be ext-clip negative signal
		0x94, 0xfd, 0xd9, 0x92, 0xeb, 0xce, 0x96, 0xe6, // arbitrary compressed bytes
	}
	out := p.handleServerBinary(frame)
	if out == nil {
		t.Fatal("continuation frame was DROPPED (return nil) — the PROT-09 desync bug")
	}
	if !bytes.Equal(out, frame) {
		t.Fatalf("continuation frame not forwarded raw: got %d bytes %v", len(out), out)
	}
}

// PROT-09: even with ZERO padding, an apparent ext-clip frame whose negative
// length is not fully present in THIS frame (absLen > remaining bytes) is an
// incomplete/false match — a continuation frame, not a real single-frame message.
// It must be forwarded raw, never dropped.
func TestIncompleteExtClipFrameForwardedRawNotDropped(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(nopWriter{}, &slog.HandlerOptions{Level: slog.LevelError}))
	p := &proxy{log: logger, sess: &plugins.Session{ID: "sess-prot09b"}, stream: rfb.NewStreamProcessor()}

	// 0x03 + zero padding + negative length (absLen=65536) but only 10 bytes here.
	frame := []byte{0x03, 0x00, 0x00, 0x00, 0xff, 0xff, 0x00, 0x00, 0xaa, 0xbb}
	out := p.handleServerBinary(frame)
	if out == nil {
		t.Fatal("incomplete ext-clip frame was DROPPED (return nil) — the PROT-09 desync bug")
	}
	if !bytes.Equal(out, frame) {
		t.Fatalf("incomplete ext-clip frame not forwarded raw: got %d bytes %v", len(out), out)
	}
}
