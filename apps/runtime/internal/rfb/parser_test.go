package rfb

import (
	"encoding/binary"
	"errors"
	"testing"
)

func buildQEMUExtKey(down bool, keysym, keycode uint32) []byte {
	buf := make([]byte, 12)
	buf[0] = MsgQEMUClientMessage
	buf[1] = QEMUSubmsgExtKey
	if down {
		binary.BigEndian.PutUint16(buf[2:4], 1)
	}
	binary.BigEndian.PutUint32(buf[4:8], keysym)
	binary.BigEndian.PutUint32(buf[8:12], keycode)
	return buf
}

func TestParseQEMUExtKey(t *testing.T) {
	msg, n, err := ParseClientMessage(buildQEMUExtKey(true, 0x61, 30))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if n != 12 {
		t.Fatalf("consumed %d, want 12", n)
	}
	if msg.Type != MsgQEMUClientMessage {
		t.Fatalf("type = %d, want %d", msg.Type, MsgQEMUClientMessage)
	}
}

func TestParseQEMUExtKeyIncomplete(t *testing.T) {
	full := buildQEMUExtKey(true, 0x61, 30)
	for _, n := range []int{1, 2, 11} {
		if _, _, err := ParseClientMessage(full[:n]); !errors.Is(err, ErrIncomplete) {
			t.Fatalf("len %d: err = %v, want ErrIncomplete", n, err)
		}
	}
}

func TestParseQEMUBadSubmessage(t *testing.T) {
	buf := buildQEMUExtKey(true, 0x61, 30)
	buf[1] = 9 // not the Extended Key subtype — must not be silently swallowed
	if _, _, err := ParseClientMessage(buf); !errors.Is(err, ErrUnknown) {
		t.Fatalf("err = %v, want ErrUnknown", err)
	}
}

func TestProcessMixedFrameWithExtKey(t *testing.T) {
	// One WS frame: SetEncodings || QEMU ext-key || KeyEvent. The ext-key must
	// be framed so the surrounding messages still parse — otherwise clipboard
	// injection + viewer input-filtering would be silently skipped.
	enc := make([]byte, 4+2*4)
	enc[0] = MsgSetEncodings
	binary.BigEndian.PutUint16(enc[2:4], 2)
	binary.BigEndian.PutUint32(enc[4:8], uint32(EncRaw))
	binary.BigEndian.PutUint32(enc[8:12], 0xfffffefe) // -258, the ext-key pseudo-encoding
	buf := append(enc, buildQEMUExtKey(true, 0x40, 3)...)
	buf = append(buf, BuildKeyEvent(true, 0x61)...)

	sp := NewStreamProcessor()
	msgs, rem, err := sp.ProcessClientData(buf)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(rem) != 0 {
		t.Fatalf("remainder = %d bytes, want 0", len(rem))
	}
	if len(msgs) != 3 {
		t.Fatalf("msgs = %d, want 3 (SetEncodings, QEMU, KeyEvent)", len(msgs))
	}
	if msgs[1].Type != MsgQEMUClientMessage {
		t.Fatalf("msg[1].type = %d, want QEMUClientMessage", msgs[1].Type)
	}
}

func TestParseServerCutText(t *testing.T) {
	text := []byte("hello clipboard")
	buf := BuildServerCutText(text)

	msg, n, err := ParseServerMessage(buf, 4)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if n != len(buf) {
		t.Fatalf("consumed %d, want %d", n, len(buf))
	}
	if msg.Type != MsgServerCutText {
		t.Fatalf("type = %d, want %d", msg.Type, MsgServerCutText)
	}
	if string(msg.CutText) != "hello clipboard" {
		t.Fatalf("cuttext = %q, want %q", msg.CutText, "hello clipboard")
	}
}

func TestParseClientCutText(t *testing.T) {
	text := []byte("paste this")
	buf := BuildClientCutText(text)

	msg, n, err := ParseClientMessage(buf)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if n != len(buf) {
		t.Fatalf("consumed %d, want %d", n, len(buf))
	}
	if msg.Type != MsgClientCutText {
		t.Fatalf("type = %d, want %d", msg.Type, MsgClientCutText)
	}
	if string(msg.CutText) != "paste this" {
		t.Fatalf("cuttext = %q, want %q", msg.CutText, "paste this")
	}
}

// GO-02: a ServerCutText with a NEGATIVE length is RFB Extended Clipboard and
// must be recognized as such. Before the int32 fix, int(uint32(negative)) was a
// huge positive on 64-bit, so the message was treated as a multi-GB legacy
// payload that never completed (ErrIncomplete forever).
func TestParseServerCutText_ExtendedNegativeLength(t *testing.T) {
	buf := make([]byte, 12)
	buf[0] = MsgServerCutText
	var negLen int32 = -4 // Extended Clipboard sentinel: negative length, absLen 4
	binary.BigEndian.PutUint32(buf[4:8], uint32(negLen))
	// buf[8:12] is the 4-byte extended payload.

	msg, n, err := ParseServerMessage(buf, 4)
	if err != nil {
		t.Fatalf("extended (negative-length) ServerCutText must parse, got %v", err)
	}
	if n != 12 {
		t.Fatalf("consumed %d, want 12 (8 header + 4 ext payload)", n)
	}
	if msg.Type != MsgServerCutText {
		t.Fatalf("type = %d, want %d", msg.Type, MsgServerCutText)
	}
}

// GO-03: ExtendedDesktopSize encodes number-of-screens in the FIRST data byte;
// bytes 1-3 are padding. Before the fix it read data[1], under-counting screens
// and consuming the wrong rect length.
func TestParseExtendedDesktopSizeLen_ScreenCount(t *testing.T) {
	data := make([]byte, 4+16) // 1 screen: 4-byte header + 1*16
	data[0] = 1
	n, err := parseExtendedDesktopSizeLen(data, Rect{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if n != 4+16 {
		t.Fatalf("consumed %d, want %d (4 header + 1 screen*16)", n, 4+16)
	}
}

func TestParseKeyEvent(t *testing.T) {
	buf := BuildKeyEvent(true, 0xFF0D) // Enter key
	msg, n, err := ParseClientMessage(buf)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if n != 8 {
		t.Fatalf("consumed %d, want 8", n)
	}
	if msg.Type != MsgKeyEvent {
		t.Fatalf("type = %d, want %d", msg.Type, MsgKeyEvent)
	}
}

func TestParsePointerEvent(t *testing.T) {
	buf := BuildPointerEvent(1, 100, 200)
	msg, n, err := ParseClientMessage(buf)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if n != 6 {
		t.Fatalf("consumed %d, want 6", n)
	}
	if msg.Type != MsgPointerEvent {
		t.Fatalf("type = %d, want %d", msg.Type, MsgPointerEvent)
	}
}

func TestParseBell(t *testing.T) {
	buf := []byte{MsgBell}
	msg, n, err := ParseServerMessage(buf, 4)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if n != 1 {
		t.Fatalf("consumed %d, want 1", n)
	}
	if msg.Type != MsgBell {
		t.Fatalf("type = %d, want %d", msg.Type, MsgBell)
	}
}

func TestParseSetEncodings(t *testing.T) {
	// SetEncodings with 3 encodings
	buf := make([]byte, 4+3*4)
	buf[0] = MsgSetEncodings
	// buf[1] padding
	binary.BigEndian.PutUint16(buf[2:4], 3)
	binary.BigEndian.PutUint32(buf[4:8], uint32(EncRaw))
	binary.BigEndian.PutUint32(buf[8:12], uint32(EncHextile))
	binary.BigEndian.PutUint32(buf[12:16], uint32(EncCopyRect))

	msg, n, err := ParseClientMessage(buf)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if n != 16 {
		t.Fatalf("consumed %d, want 16", n)
	}
	if msg.Type != MsgSetEncodings {
		t.Fatalf("type = %d, want %d", msg.Type, MsgSetEncodings)
	}
}

func TestParseIncomplete(t *testing.T) {
	// ServerCutText with only 4 bytes (need 8 for header)
	buf := []byte{MsgServerCutText, 0, 0, 0}
	_, _, err := ParseServerMessage(buf, 4)
	if err != ErrIncomplete {
		t.Fatalf("expected ErrIncomplete, got %v", err)
	}
}

// buildRawFBUpdate constructs a FramebufferUpdate with one Raw-encoded rect.
func buildRawFBUpdate(x, y, w, h uint16, pixels []byte) []byte {
	buf := make([]byte, 4+12+len(pixels))
	buf[0] = MsgFramebufferUpdate
	// buf[1] padding
	binary.BigEndian.PutUint16(buf[2:4], 1) // nRects
	binary.BigEndian.PutUint16(buf[4:6], x)
	binary.BigEndian.PutUint16(buf[6:8], y)
	binary.BigEndian.PutUint16(buf[8:10], w)
	binary.BigEndian.PutUint16(buf[10:12], h)
	binary.BigEndian.PutUint32(buf[12:16], uint32(EncRaw))
	copy(buf[16:], pixels)
	return buf
}

func TestParseFramebufferUpdateRaw(t *testing.T) {
	// 2x2 pixels, 4 bpp = 16 bytes of pixel data
	pixels := make([]byte, 2*2*4)
	for i := range pixels {
		pixels[i] = byte(i)
	}
	buf := buildRawFBUpdate(10, 20, 2, 2, pixels)

	msg, n, err := ParseServerMessage(buf, 4)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if n != len(buf) {
		t.Fatalf("consumed %d, want %d", n, len(buf))
	}
	if msg.Type != MsgFramebufferUpdate {
		t.Fatalf("type = %d, want %d", msg.Type, MsgFramebufferUpdate)
	}
	if len(msg.Rects) != 1 {
		t.Fatalf("rects = %d, want 1", len(msg.Rects))
	}
	r := msg.Rects[0]
	if r.X != 10 || r.Y != 20 || r.Width != 2 || r.Height != 2 {
		t.Fatalf("rect = (%d,%d,%d,%d), want (10,20,2,2)", r.X, r.Y, r.Width, r.Height)
	}
	if r.Encoding != EncRaw {
		t.Fatalf("encoding = %d, want %d", r.Encoding, EncRaw)
	}
	if r.DataLen != 16 {
		t.Fatalf("datalen = %d, want 16", r.DataLen)
	}
}

func TestParseFramebufferUpdateCopyRect(t *testing.T) {
	// CopyRect: 12-byte header + 4 bytes (src x,y)
	buf := make([]byte, 4+12+4)
	buf[0] = MsgFramebufferUpdate
	binary.BigEndian.PutUint16(buf[2:4], 1)
	binary.BigEndian.PutUint16(buf[4:6], 0)
	binary.BigEndian.PutUint16(buf[6:8], 0)
	binary.BigEndian.PutUint16(buf[8:10], 10)
	binary.BigEndian.PutUint16(buf[10:12], 10)
	binary.BigEndian.PutUint32(buf[12:16], uint32(EncCopyRect))
	// src position
	binary.BigEndian.PutUint16(buf[16:18], 5)
	binary.BigEndian.PutUint16(buf[18:20], 5)

	msg, n, err := ParseServerMessage(buf, 4)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if n != 20 {
		t.Fatalf("consumed %d, want 20", n)
	}
	if len(msg.Rects) != 1 {
		t.Fatalf("rects = %d, want 1", len(msg.Rects))
	}
	if msg.Rects[0].Encoding != EncCopyRect {
		t.Fatalf("encoding = %d, want CopyRect", msg.Rects[0].Encoding)
	}
	if msg.Rects[0].DataLen != 4 {
		t.Fatalf("datalen = %d, want 4", msg.Rects[0].DataLen)
	}
}

func TestParseMultipleMessages(t *testing.T) {
	// Two client messages back-to-back: KeyEvent + PointerEvent
	key := BuildKeyEvent(true, 0x61) // 'a'
	ptr := BuildPointerEvent(0, 50, 60)
	buf := append(key, ptr...)

	sp := NewStreamProcessor()
	msgs, rem, err := sp.ProcessClientData(buf)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(rem) != 0 {
		t.Fatalf("remainder = %d bytes, want 0", len(rem))
	}
	if len(msgs) != 2 {
		t.Fatalf("msgs = %d, want 2", len(msgs))
	}
	if msgs[0].Type != MsgKeyEvent {
		t.Fatalf("msg[0].type = %d, want KeyEvent", msgs[0].Type)
	}
	if msgs[1].Type != MsgPointerEvent {
		t.Fatalf("msg[1].type = %d, want PointerEvent", msgs[1].Type)
	}
}

func TestFramebufferUpdateRequest(t *testing.T) {
	buf := FramebufferUpdateRequest(true, 0, 100, 200, 4)
	if len(buf) != 10 {
		t.Fatalf("len = %d, want 10", len(buf))
	}
	if buf[0] != MsgFramebufferUpdateReq {
		t.Fatalf("type = %d, want %d", buf[0], MsgFramebufferUpdateReq)
	}
	if buf[1] != 1 {
		t.Fatalf("incremental = %d, want 1", buf[1])
	}
	y := binary.BigEndian.Uint16(buf[4:6])
	if y != 100 {
		t.Fatalf("y = %d, want 100", y)
	}
}

func TestSetPixelFormatUpdatesBPP(t *testing.T) {
	sp := NewStreamProcessor()
	if sp.BPP() != 4 {
		t.Fatalf("initial bpp = %d, want 4", sp.BPP())
	}

	// Build a SetPixelFormat with 16 bpp
	buf := make([]byte, 20)
	buf[0] = MsgSetPixelFormat
	buf[4] = 16 // bits-per-pixel

	msgs, _, err := sp.ProcessClientData(buf)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("msgs = %d, want 1", len(msgs))
	}
	if sp.BPP() != 2 {
		t.Fatalf("bpp after SetPixelFormat = %d, want 2", sp.BPP())
	}
}

// --- Integer-overflow length-walker hardening ---
//
// A compromised/MITM'd upstream can craft a FramebufferUpdate rect whose
// declared length overflows a 32-bit int on the shipped windows/386 build,
// wrapping negative; the old `len(data) < n` guard was bypassed and a later
// buf[:offset] slice panicked, crashing the WHOLE runtime process (every
// operator's session). The length math is now done in int64 and bounded
// against len(data), so these frames refuse gracefully with ErrIncomplete and
// NEVER panic. (On amd64 `int` is 64-bit so these already wrapped to a large
// positive value; the assertion below locks the graceful-refusal contract on
// every arch and would catch a regression to native-int math under 386 CI.)

func fbHeader(enc int32, w, h uint16, body []byte) []byte {
	buf := make([]byte, 0, 16+len(body))
	buf = append(buf, MsgFramebufferUpdate, 0)
	buf = binary.BigEndian.AppendUint16(buf, 1) // nRects
	buf = binary.BigEndian.AppendUint16(buf, 0) // x
	buf = binary.BigEndian.AppendUint16(buf, 0) // y
	buf = binary.BigEndian.AppendUint16(buf, w)
	buf = binary.BigEndian.AppendUint16(buf, h)
	buf = binary.BigEndian.AppendUint32(buf, uint32(enc))
	return append(buf, body...)
}

// Tight palette entries track the negotiated pixel format: 3-byte TPIXELs at
// 32bpp (depth-24 compression), but the FULL 2-byte pixel at 16bpp — TPIXEL
// only exists for 32bpp. The old hardcoded 3-byte entry over-consumed one byte
// per palette color at 16bpp, misframing every low-color palette rect (the
// dominant Tight filter for flat UI) and desyncing the proxied stream.
func TestParseTightPaletteEntrySizeTracksBPP(t *testing.T) {
	cases := []struct {
		name string
		bpp  int
		w, h uint16
		body []byte
	}{
		{
			// Regression: 2 colors at 32bpp = 3-byte TPIXEL entries; 8x1 rect
			// packs to 1 index byte (1bpp rows), uncompressed (<12 bytes).
			name: "32bpp_2color_packed",
			bpp:  4,
			w:    8, h: 1,
			body: []byte{0x40, 1, 1, 1, 2, 3, 4, 5, 6, 0xAA},
		},
		{
			// 2 colors at 16bpp = 2-byte entries. With the old 3-byte entry the
			// walker demanded 2 extra bytes and returned ErrIncomplete forever.
			name: "16bpp_2color_packed",
			bpp:  2,
			w:    8, h: 1,
			body: []byte{0x40, 1, 1, 1, 2, 3, 4, 0xAA},
		},
		{
			// >2 colors at 16bpp: 8bpp-indexed rows (2x2 rect = 4 index bytes).
			name: "16bpp_4color_indexed",
			bpp:  2,
			w:    2, h: 2,
			body: []byte{0x40, 1, 3, 1, 2, 3, 4, 5, 6, 7, 8, 0, 1, 2, 3},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			buf := fbHeader(EncTight, c.w, c.h, c.body)
			msg, n, err := ParseServerMessage(buf, c.bpp)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			// Exact framing: the buffer holds exactly one rect, so any over- or
			// under-consumption shows up as n != len(buf).
			if n != len(buf) {
				t.Fatalf("consumed %d, want %d", n, len(buf))
			}
			if len(msg.Rects) != 1 {
				t.Fatalf("rects = %d, want 1", len(msg.Rects))
			}
			if msg.Rects[0].DataLen != len(c.body) {
				t.Fatalf("datalen = %d, want %d", msg.Rects[0].DataLen, len(c.body))
			}
		})
	}
}

func TestParseFramebufferUpdateOverflowRefusesNotPanics(t *testing.T) {
	cases := []struct {
		name string
		buf  []byte
	}{
		{
			// RRE: nSubrects = 0x10000000; 4+bpp+nSubrects*(bpp+8) overflows int32.
			name: "rre_huge_nsubrects",
			buf:  fbHeader(EncRRE, 1, 1, []byte{0x10, 0x00, 0x00, 0x00}),
		},
		{
			// ZRLE: zLen high bit set reads negative as int on 32-bit.
			name: "zrle_high_bit_len",
			buf:  fbHeader(EncZRLE, 1, 1, []byte{0x80, 0x00, 0x00, 0x00}),
		},
		{
			// Tight basic/copy with w=h=65535: rowBytes*h overflows int32 and the
			// `<12` branch would have returned offset+negative. Control byte 0x00 =
			// basic compression, stream 0, no filter.
			name: "tight_huge_dims",
			buf:  fbHeader(EncTight, 65535, 65535, []byte{0x00}),
		},
		{
			// Raw with w=h=65535: w*h*bpp overflows int32.
			name: "raw_huge_dims",
			buf:  fbHeader(EncRaw, 65535, 65535, nil),
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// A panic here is the bug; the test body must complete normally.
			_, _, err := ParseServerMessage(c.buf, 4)
			if err != ErrIncomplete {
				t.Fatalf("err = %v, want ErrIncomplete (graceful refusal, no panic)", err)
			}
		})
	}
}
