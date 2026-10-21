package vdagent

import (
	"context"
	"encoding/binary"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"
)

// fakePort is a programmable io.ReadWriteCloser. Reads are served from a queue
// of scripted "responses": each response yields some bytes (or a timeout, or a
// real error) so tests can drive readChunk / readFrameBytes deterministically.
// Writes are captured for assertion.
type fakePort struct {
	mu      sync.Mutex
	reads   []readResp
	readIdx int
	readOff int
	writes  [][]byte
	closed  bool
}

// readResp is one scripted Read outcome.
type readResp struct {
	data    []byte // bytes to deliver across one-or-more Read calls
	timeout bool   // deliver a (0, nil) "no data this interval" timeout
	err     error  // deliver a hard error (0, err)
}

func (p *fakePort) Read(b []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for p.readIdx < len(p.reads) {
		r := p.reads[p.readIdx]
		if r.err != nil {
			p.readIdx++
			return 0, r.err
		}
		if r.timeout {
			p.readIdx++
			return 0, nil // Windows-style timeout: (0, nil)
		}
		if p.readOff >= len(r.data) {
			p.readIdx++
			p.readOff = 0
			continue
		}
		n := copy(b, r.data[p.readOff:])
		p.readOff += n
		if p.readOff >= len(r.data) {
			p.readIdx++
			p.readOff = 0
		}
		return n, nil
	}
	// Reads exhausted: behave as an idle frame boundary (timeout) so
	// readFrameBytes blocks rather than spinning. Tests that need the loop to
	// stop cancel the context.
	return 0, nil
}

func (p *fakePort) Write(b []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	cp := make([]byte, len(b))
	copy(cp, b)
	p.writes = append(p.writes, cp)
	return len(b), nil
}

func (p *fakePort) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.closed = true
	return nil
}

func (p *fakePort) writtenCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.writes)
}

// fakeClipboard records writes and serves a fixed read text.
type fakeClipboard struct {
	mu        sync.Mutex
	readText  string
	readErr   error
	readCalls int
	written   []string
	writeErr  error
	watchCh   chan struct{}
	// readHook, if set, runs (without c.mu held) each time ReadText is called —
	// lets a test inject concurrent activity DURING a read (e.g. a second
	// self-write) to exercise interleavings.
	readHook func()
}

func (c *fakeClipboard) ReadText(ctx context.Context) ([]byte, error) {
	c.mu.Lock()
	c.readCalls++
	hook := c.readHook
	err := c.readErr
	text := c.readText
	c.mu.Unlock()
	if hook != nil {
		hook()
	}
	if err != nil {
		return nil, err
	}
	return []byte(text), nil
}

func (c *fakeClipboard) readCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.readCalls
}

func (c *fakeClipboard) WriteText(ctx context.Context, text []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.writeErr != nil {
		return c.writeErr
	}
	c.written = append(c.written, string(text))
	return nil
}

func (c *fakeClipboard) Watch(ctx context.Context) <-chan struct{} {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.watchCh == nil {
		c.watchCh = make(chan struct{})
	}
	return c.watchCh
}

func (c *fakeClipboard) lastWritten() (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.written) == 0 {
		return "", false
	}
	return c.written[len(c.written)-1], true
}

// recordingResizer captures the dimensions passed to Resize.
type recordingResizer struct {
	mu    sync.Mutex
	calls [][2]int
	err   error
}

func (r *recordingResizer) Resize(w, h int) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, [2]int{w, h})
	return r.err
}

func (r *recordingResizer) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.calls)
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func newTestAgent(port io.ReadWriteCloser, clip ClipboardHandler) *Agent {
	a := New(port, clip, testLogger())
	return a
}

// chunk wraps payload bytes in a VDIChunkHeader for PortClient (host→guest).
func chunk(port uint32, payload []byte) []byte {
	hdr := make([]byte, ChunkHeaderSize)
	binary.LittleEndian.PutUint32(hdr[0:4], port)
	binary.LittleEndian.PutUint32(hdr[4:8], uint32(len(payload)))
	return append(hdr, payload...)
}

// msgBytes builds a full VDAgentMessage (header + body) without chunk framing.
func msgBytes(msgType uint32, body []byte) []byte {
	out := make([]byte, MessageHeaderSize+len(body))
	binary.LittleEndian.PutUint32(out[0:4], ProtocolVersion)
	binary.LittleEndian.PutUint32(out[4:8], msgType)
	binary.LittleEndian.PutUint32(out[16:20], uint32(len(body)))
	copy(out[20:], body)
	return out
}

// capsBody builds an ANNOUNCE_CAPABILITIES body advertising the given caps.
func capsBody(request bool, caps ...uint32) []byte {
	return BuildCapabilities(request, caps...)
}

// ---------------------------------------------------------------------------
// PROT-02: receive / framing path table tests.
// ---------------------------------------------------------------------------

// startTestDrain starts the serial drain goroutine for tests that drive the
// read path directly. Run() normally does this; readFrameBytes consumes the
// ring the drain fills, so without it the read path has no source.
func startTestDrain(t *testing.T, a *Agent) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	a.startDrain(ctx)
	t.Cleanup(cancel)
}

// TestReadChunk_SingleAndSplit covers readChunk reassembling a chunk whose
// header and body are delivered in one read, and one where the body straddles
// multiple Read() calls (readFrameBytes loop).
func TestReadChunk(t *testing.T) {
	payload := []byte("PAYLOAD-BYTES")
	full := chunk(PortClient, payload)

	tests := []struct {
		name  string
		reads []readResp
	}{
		{
			name:  "single read delivers whole chunk",
			reads: []readResp{{data: full}},
		},
		{
			name: "header and body split across reads",
			reads: []readResp{
				{data: full[:3]},
				{data: full[3:ChunkHeaderSize]},
				{data: full[ChunkHeaderSize : ChunkHeaderSize+5]},
				{data: full[ChunkHeaderSize+5:]},
			},
		},
		{
			name: "timeouts interleaved mid-frame are tolerated",
			reads: []readResp{
				{data: full[:ChunkHeaderSize]},
				{timeout: true},
				{data: full[ChunkHeaderSize:]},
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			port := &fakePort{reads: tc.reads}
			a := newTestAgent(port, &fakeClipboard{})
			startTestDrain(t, a)
			ch, err := a.readChunk()
			if err != nil {
				t.Fatalf("readChunk: %v", err)
			}
			if ch.Port != PortClient {
				t.Fatalf("port = %d, want %d", ch.Port, PortClient)
			}
			if string(ch.Data) != string(payload) {
				t.Fatalf("data = %q, want %q", ch.Data, payload)
			}
		})
	}
}

// TestReadFrameBytes_StallTimesOut pins the mid-frame stall path: once partial
// bytes arrive, a sustained no-data stretch must surface an error rather than
// block forever.
func TestReadFrameBytes_StallTimesOut(t *testing.T) {
	// Deliver 2 bytes, then only timeouts forever. readFrameBytes must abort
	// after frameStallTimeout instead of hanging.
	port := &fakePort{reads: []readResp{
		{data: []byte{0xAA, 0xBB}},
	}}
	a := newTestAgent(port, &fakeClipboard{})
	startTestDrain(t, a)

	buf := make([]byte, 8)
	done := make(chan error, 1)
	go func() { done <- a.readFrameBytes(buf) }()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected stall error, got nil")
		}
	case <-time.After(frameStallTimeout + 2*time.Second):
		t.Fatal("readFrameBytes did not abort on a sustained mid-frame stall")
	}
}

// TestReadFrameBytes_IdleBoundaryWaits verifies that timeouts at a frame
// boundary (got==0) are NOT treated as a stall: the call blocks until data
// arrives.
func TestReadFrameBytes_IdleBoundaryWaits(t *testing.T) {
	port := &fakePort{reads: []readResp{
		{timeout: true},
		{timeout: true},
		{data: []byte{1, 2, 3, 4}},
	}}
	a := newTestAgent(port, &fakeClipboard{})
	startTestDrain(t, a)
	buf := make([]byte, 4)
	if err := a.readFrameBytes(buf); err != nil {
		t.Fatalf("readFrameBytes at idle boundary: %v", err)
	}
}

// dispatchTracker counts handleMessage invocations by patching nothing — we
// instead route messages that decrement a counter via a recognizable type and
// inspect side effects (caps response / clipboard writes). For reassembly we
// use a CLIPBOARD data message and observe the guest write.

// TestHandleChunkData_Reassembly covers the message-reassembly matrix:
// single message in one chunk; message split across 2+ chunks; two messages in
// one chunk; HEADER straddling a chunk boundary (PROT-01); oversized rejected;
// reassembly timeout drop.
func TestHandleChunkData_Reassembly(t *testing.T) {
	ctx := context.Background()
	text := "shared-clip"

	makeAgent := func() (*Agent, *fakeClipboard) {
		clip := &fakeClipboard{}
		a := newTestAgent(&fakePort{}, clip)
		// Mark clipboard negotiated + no selection prefix so the CLIPBOARD
		// body layout is [type uint32][text].
		a.connected = true
		a.clipboardEnabled = true
		a.useSelection = false
		return a, clip
	}

	clipMsg := func(s string) []byte {
		body := make([]byte, 4+len(s))
		binary.LittleEndian.PutUint32(body[0:4], ClipboardUTF8Text)
		copy(body[4:], s)
		return msgBytes(MsgClipboard, body)
	}

	t.Run("single message in one chunk", func(t *testing.T) {
		a, clip := makeAgent()
		if err := a.handleChunkData(ctx, clipMsg(text)); err != nil {
			t.Fatalf("handleChunkData: %v", err)
		}
		if got, ok := clip.lastWritten(); !ok || got != text {
			t.Fatalf("guest clipboard = %q (ok=%v), want %q", got, ok, text)
		}
	})

	t.Run("body split across 3 chunks", func(t *testing.T) {
		a, clip := makeAgent()
		full := clipMsg(text)
		// Split the message bytes (already past chunk framing — these are the
		// chunk DATA payloads) into three pieces, header intact in the first.
		p1 := full[:MessageHeaderSize+2]
		p2 := full[MessageHeaderSize+2 : MessageHeaderSize+5]
		p3 := full[MessageHeaderSize+5:]
		for _, p := range [][]byte{p1, p2, p3} {
			if err := a.handleChunkData(ctx, p); err != nil {
				t.Fatalf("handleChunkData: %v", err)
			}
		}
		if got, ok := clip.lastWritten(); !ok || got != text {
			t.Fatalf("guest clipboard = %q (ok=%v), want %q", got, ok, text)
		}
		if a.reassemblyRemaining != 0 || len(a.reassembly) != 0 {
			t.Fatalf("reassembly state not cleared: remaining=%d buf=%d", a.reassemblyRemaining, len(a.reassembly))
		}
	})

	t.Run("two messages in one chunk", func(t *testing.T) {
		a, clip := makeAgent()
		combined := append(clipMsg("first"), clipMsg("second")...)
		if err := a.handleChunkData(ctx, combined); err != nil {
			t.Fatalf("handleChunkData: %v", err)
		}
		// Second write wins lastWritten; both should have been processed.
		clip.mu.Lock()
		n := len(clip.written)
		last := ""
		if n > 0 {
			last = clip.written[n-1]
		}
		clip.mu.Unlock()
		if n != 2 || last != "second" {
			t.Fatalf("writes=%d last=%q, want 2 / \"second\"", n, last)
		}
	})

	// PROT-01: the message HEADER itself straddles a chunk boundary. The first
	// chunk ends mid-header (fewer than MessageHeaderSize bytes), so msgSize is
	// not yet readable. The leftover must be stashed and reassembled, NOT
	// dropped with "chunk too small for message header".
	t.Run("header straddles chunk boundary (PROT-01)", func(t *testing.T) {
		a, clip := makeAgent()
		full := clipMsg(text)
		// Cut inside the 20-byte header (e.g. after 10 bytes), so msgSize at
		// offset 16..20 is NOT present in the first chunk.
		p1 := full[:10]
		p2 := full[10:]
		if err := a.handleChunkData(ctx, p1); err != nil {
			t.Fatalf("handleChunkData(p1): %v", err)
		}
		if len(a.reassembly) != 10 || a.reassemblyRemaining != 0 {
			t.Fatalf("expected stashed partial header (10 bytes, remaining=0), got buf=%d remaining=%d",
				len(a.reassembly), a.reassemblyRemaining)
		}
		if err := a.handleChunkData(ctx, p2); err != nil {
			t.Fatalf("handleChunkData(p2): %v", err)
		}
		if got, ok := clip.lastWritten(); !ok || got != text {
			t.Fatalf("guest clipboard = %q (ok=%v), want %q", got, ok, text)
		}
		if a.reassemblyRemaining != 0 || len(a.reassembly) != 0 {
			t.Fatalf("reassembly state not cleared: remaining=%d buf=%d", a.reassemblyRemaining, len(a.reassembly))
		}
	})

	// PROT-01 extreme: header straddles across THREE chunks, each delivering
	// only a few header bytes, then the body.
	t.Run("header straddles three chunks then body", func(t *testing.T) {
		a, clip := makeAgent()
		full := clipMsg(text)
		parts := [][]byte{full[:4], full[4:8], full[8:14], full[14:]}
		for i, p := range parts {
			if err := a.handleChunkData(ctx, p); err != nil {
				t.Fatalf("handleChunkData(part %d): %v", i, err)
			}
		}
		if got, ok := clip.lastWritten(); !ok || got != text {
			t.Fatalf("guest clipboard = %q (ok=%v), want %q", got, ok, text)
		}
	})

	t.Run("msgSize over MaxMessageSize rejected", func(t *testing.T) {
		a, _ := makeAgent()
		bad := make([]byte, MessageHeaderSize)
		binary.LittleEndian.PutUint32(bad[0:4], ProtocolVersion)
		binary.LittleEndian.PutUint32(bad[4:8], MsgClipboard)
		binary.LittleEndian.PutUint32(bad[16:20], MaxMessageSize+1)
		err := a.handleChunkData(ctx, bad)
		if err == nil {
			t.Fatal("expected error for oversized msgSize, got nil")
		}
	})

	// A large host->guest paste arrives as many chunks over a slow serial
	// link: as long as chunks keep making progress, the reassembly deadline
	// must slide forward, otherwise any message taking longer than
	// reassemblyTimeout end-to-end is deterministically dropped. Only a
	// genuinely stalled stream (no progress for the full timeout) may drop.
	t.Run("progress extends the reassembly deadline", func(t *testing.T) {
		a, clip := makeAgent()
		full := clipMsg(text)
		p1 := full[:MessageHeaderSize+2]
		p2 := full[MessageHeaderSize+2 : MessageHeaderSize+5]
		p3 := full[MessageHeaderSize+5:]
		if err := a.handleChunkData(ctx, p1); err != nil {
			t.Fatalf("handleChunkData(p1): %v", err)
		}
		// Simulate the deadline armed at message start being nearly
		// exhausted while chunks are still flowing.
		a.reassemblyDeadline = time.Now().Add(50 * time.Millisecond)
		if err := a.handleChunkData(ctx, p2); err != nil {
			t.Fatalf("handleChunkData(p2): %v", err)
		}
		// Let the original (un-extended) deadline expire between chunks.
		time.Sleep(150 * time.Millisecond)
		if err := a.handleChunkData(ctx, p3); err != nil {
			t.Fatalf("handleChunkData(p3): %v", err)
		}
		if got, ok := clip.lastWritten(); !ok || got != text {
			t.Fatalf("message dropped: deadline not extended on progress (got %q, ok=%v)", got, ok)
		}
	})

	t.Run("reassembly timeout drops partial body", func(t *testing.T) {
		a, clip := makeAgent()
		full := clipMsg(text)
		// First chunk has the full header but only part of the body.
		p1 := full[:MessageHeaderSize+2]
		if err := a.handleChunkData(ctx, p1); err != nil {
			t.Fatalf("handleChunkData(p1): %v", err)
		}
		if a.reassemblyRemaining == 0 {
			t.Fatal("expected body reassembly in progress")
		}
		// Force the deadline into the past.
		a.reassemblyDeadline = time.Now().Add(-time.Second)
		// The next chunk arrives too late: it must be dropped, not appended.
		if err := a.handleChunkData(ctx, full[MessageHeaderSize+2:]); err != nil {
			t.Fatalf("handleChunkData(late): %v", err)
		}
		if _, ok := clip.lastWritten(); ok {
			t.Fatal("expected no guest write after reassembly timeout")
		}
		if a.reassemblyRemaining != 0 || len(a.reassembly) != 0 {
			t.Fatalf("reassembly state not cleared after timeout: remaining=%d buf=%d", a.reassemblyRemaining, len(a.reassembly))
		}
	})

	t.Run("partial-header reassembly timeout drops stash", func(t *testing.T) {
		a, clip := makeAgent()
		full := clipMsg(text)
		if err := a.handleChunkData(ctx, full[:8]); err != nil {
			t.Fatalf("handleChunkData(p1): %v", err)
		}
		if len(a.reassembly) == 0 {
			t.Fatal("expected stashed partial header")
		}
		a.reassemblyDeadline = time.Now().Add(-time.Second)
		if err := a.handleChunkData(ctx, full[8:]); err != nil {
			t.Fatalf("handleChunkData(late): %v", err)
		}
		if _, ok := clip.lastWritten(); ok {
			t.Fatal("expected no guest write after partial-header timeout")
		}
		if len(a.reassembly) != 0 {
			t.Fatalf("partial-header stash not cleared after timeout: buf=%d", len(a.reassembly))
		}
	})
}

// ---------------------------------------------------------------------------
// PROT-06: outbound selection byte echoes the inbound selection.
// ---------------------------------------------------------------------------

// TestClipboardGrab_EchoesSelection verifies that when the selection prefix is
// negotiated, the REQUEST we emit in reply to a GRAB carries the host's inbound
// selection byte (e.g. SelectionPrimary), not a hard-coded SelectionClipboard.
func TestClipboardGrab_EchoesSelection(t *testing.T) {
	port := &fakePort{}
	a := newTestAgent(port, &fakeClipboard{})
	a.useSelection = true

	// GRAB payload: [selection=PRIMARY (4 bytes)][type=UTF8].
	grab := BuildClipboardGrab(SelectionPrimary, []uint32{ClipboardUTF8Text}, true)
	if err := a.handleClipboardGrab(context.Background(), grab); err != nil {
		t.Fatalf("handleClipboardGrab: %v", err)
	}
	if port.writtenCount() != 1 {
		t.Fatalf("expected 1 outbound REQUEST, got %d", port.writtenCount())
	}
	// Decode the written chunk -> message -> request body and check selection.
	sel := decodeSelectionByte(t, port, MsgClipboardRequest)
	if sel != SelectionPrimary {
		t.Fatalf("echoed selection = %d, want %d (SelectionPrimary)", sel, SelectionPrimary)
	}
}

// TestClipboardRequest_EchoesSelection verifies the data reply to a REQUEST
// echoes the inbound selection byte.
func TestClipboardRequest_EchoesSelection(t *testing.T) {
	port := &fakePort{}
	clip := &fakeClipboard{readText: "guest-copied"}
	a := newTestAgent(port, clip)
	a.useSelection = true

	req := BuildClipboardRequest(SelectionPrimary, ClipboardUTF8Text, true)
	if err := a.handleClipboardRequest(context.Background(), req); err != nil {
		t.Fatalf("handleClipboardRequest: %v", err)
	}
	if port.writtenCount() != 1 {
		t.Fatalf("expected 1 outbound CLIPBOARD data, got %d", port.writtenCount())
	}
	sel := decodeSelectionByte(t, port, MsgClipboard)
	if sel != SelectionPrimary {
		t.Fatalf("echoed selection = %d, want %d (SelectionPrimary)", sel, SelectionPrimary)
	}
}

// decodeSelectionByte unwraps the first write (chunk header + message header)
// and returns the leading selection byte of the message body, asserting the
// message type along the way.
func decodeSelectionByte(t *testing.T, port *fakePort, wantType uint32) uint8 {
	t.Helper()
	port.mu.Lock()
	raw := port.writes[0]
	port.mu.Unlock()
	if len(raw) < ChunkHeaderSize+MessageHeaderSize+1 {
		t.Fatalf("written frame too short: %d bytes", len(raw))
	}
	msgType := binary.LittleEndian.Uint32(raw[ChunkHeaderSize+4 : ChunkHeaderSize+8])
	if msgType != wantType {
		t.Fatalf("written msg type = %d, want %d", msgType, wantType)
	}
	body := raw[ChunkHeaderSize+MessageHeaderSize:]
	return body[0]
}

// ---------------------------------------------------------------------------
// Host->guest paste regression: QEMU 9.1 advertises CapClipboardGrabSerial in
// its host caps unconditionally, but since the agent never advertises it the
// host still sends LEGACY grabs (selection prefix + type list, no serial). The
// agent must HANDLE such a grab — request the data — rather than refuse it. An
// earlier guard refused on the cap alone, which silently killed host->guest
// clipboard while guest->host kept working.
// ---------------------------------------------------------------------------

func TestClipboardGrab_HandledWhenHostAdvertisesSerialCap(t *testing.T) {
	port := &fakePort{}
	a := newTestAgent(port, &fakeClipboard{})
	a.useSelection = true
	// Host advertises the grab-serial cap (as QEMU 9.1 does), but we do not.
	a.hostCaps = capWords(CapClipboardSelection, CapClipboardGrabSerial)

	// Legacy grab as actually seen on AHV/QEMU 9.1: [selection(4)][type UTF8(4)]
	// = 8 bytes, no serial (the serial only appears when mutually negotiated).
	body := make([]byte, 8)
	body[0] = SelectionClipboard
	binary.LittleEndian.PutUint32(body[4:8], ClipboardUTF8Text)

	if err := a.handleClipboardGrab(context.Background(), body); err != nil {
		t.Fatalf("handleClipboardGrab: %v", err)
	}
	// Must proceed to request the data (host->guest paste step 2), not refuse.
	if port.writtenCount() != 1 {
		t.Fatalf("expected 1 CLIPBOARD_REQUEST after grab, got %d", port.writtenCount())
	}
	if sel := decodeSelectionByte(t, port, MsgClipboardRequest); sel != SelectionClipboard {
		t.Fatalf("request selection = %d, want %d (SelectionClipboard)", sel, SelectionClipboard)
	}
}

// capWords builds a hostCaps slice with the given capability bits set.
func capWords(caps ...uint32) []uint32 {
	maxBit := uint32(0)
	for _, c := range caps {
		if c > maxBit {
			maxBit = c
		}
	}
	words := make([]uint32, maxBit/32+1)
	for _, c := range caps {
		words[c/32] |= 1 << (c % 32)
	}
	return words
}

// ---------------------------------------------------------------------------
// SECU-05: resize bounds.
// ---------------------------------------------------------------------------

func TestHandleResizeCommand_Bounds(t *testing.T) {
	tests := []struct {
		name    string
		cmd     string
		wantW   int
		wantH   int
		applied bool
	}{
		{"valid 1080p", "__AHVRC_RESIZE__1920x1080", 1920, 1080, true},
		{"valid 4k", "__AHVRC_RESIZE__3840x2160", 3840, 2160, true},
		{"valid 8k boundary", "__AHVRC_RESIZE__7680x4320", 7680, 4320, true},
		{"too small width", "__AHVRC_RESIZE__100x480", 0, 0, false},
		{"too small height", "__AHVRC_RESIZE__640x100", 0, 0, false},
		{"too large width (SECU-05)", "__AHVRC_RESIZE__7681x4320", 0, 0, false},
		{"too large height (SECU-05)", "__AHVRC_RESIZE__7680x4321", 0, 0, false},
		{"absurd width (SECU-05)", "__AHVRC_RESIZE__999999x1080", 0, 0, false},
		{"malformed", "__AHVRC_RESIZE__1920", 0, 0, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rz := &recordingResizer{}
			a := newTestAgent(&fakePort{}, &fakeClipboard{})
			a.SetDisplay(rz)
			if err := a.handleResizeCommand(tc.cmd); err != nil {
				t.Fatalf("handleResizeCommand: %v", err)
			}
			if tc.applied {
				if rz.count() != 1 {
					t.Fatalf("expected resize applied once, got %d calls", rz.count())
				}
				if rz.calls[0][0] != tc.wantW || rz.calls[0][1] != tc.wantH {
					t.Fatalf("resize = %dx%d, want %dx%d", rz.calls[0][0], rz.calls[0][1], tc.wantW, tc.wantH)
				}
			} else if rz.count() != 0 {
				t.Fatalf("expected resize rejected, got %d calls (%v)", rz.count(), rz.calls)
			}
		})
	}
}
