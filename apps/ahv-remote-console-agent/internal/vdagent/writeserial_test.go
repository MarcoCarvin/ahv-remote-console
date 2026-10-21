package vdagent

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
)

// scriptedWritePort is an io.ReadWriteCloser whose Write behavior is scripted
// per call. Each entry caps how many bytes are accepted — a cap of 0 with a
// nil error models the Windows timed-out-yet-"successful" WriteFile that
// silently truncates — or returns a hard error. Once the script is exhausted,
// writes accept everything offered. Reads behave as an idle serial port.
type scriptedWritePort struct {
	mu       sync.Mutex
	script   []writeResp
	idx      int
	got      []byte // concatenation of all accepted bytes
	calls    int
	maxOffer int    // largest len(buf) seen in a single Write call
	onWrite  func() // optional hook, runs before each write is applied
}

type writeResp struct {
	max int   // accept at most this many bytes
	err error // return (0, err) instead
}

func (p *scriptedWritePort) Read(b []byte) (int, error) { return 0, nil }

func (p *scriptedWritePort) Write(b []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.onWrite != nil {
		p.onWrite()
	}
	p.calls++
	if len(b) > p.maxOffer {
		p.maxOffer = len(b)
	}
	n := len(b)
	if p.idx < len(p.script) {
		r := p.script[p.idx]
		p.idx++
		if r.err != nil {
			return 0, r.err
		}
		if r.max < n {
			n = r.max
		}
	}
	p.got = append(p.got, b[:n]...)
	return n, nil
}

func (p *scriptedWritePort) Close() error { return nil }

func (p *scriptedWritePort) received() []byte {
	p.mu.Lock()
	defer p.mu.Unlock()
	cp := make([]byte, len(p.got))
	copy(cp, p.got)
	return cp
}

func pattern(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i % 251)
	}
	return b
}

// TestWriteSerial_ShortWritesDeliverFully pins the Windows-truncation fix: a
// port that accepts only part of each buffer (as a timed-out synchronous
// WriteFile does) must still receive every byte, in order, because the wire
// framing has no resync marker and a short write desyncs the host parser.
func TestWriteSerial_ShortWritesDeliverFully(t *testing.T) {
	data := pattern(100)
	port := &scriptedWritePort{script: []writeResp{
		{max: 7}, {max: 1}, {max: 13}, {max: 0}, {max: 30},
	}}
	a := newTestAgent(port, &fakeClipboard{})

	n, err := a.writeSerial(context.Background(), data)
	if err != nil {
		t.Fatalf("writeSerial: %v", err)
	}
	if n != len(data) {
		t.Fatalf("n = %d, want %d", n, len(data))
	}
	if got := port.received(); string(got) != string(data) {
		t.Fatalf("port received %d bytes, want %d, or bytes out of order", len(got), len(data))
	}
}

// TestWriteSerial_SlicesCappedAt4KB verifies no single port.Write is offered
// more than serialWriteSlice bytes, keeping each Windows WriteFile under the
// 1s WriteTotalTimeoutConstant at any drain rate >= 4KB/s.
func TestWriteSerial_SlicesCappedAt4KB(t *testing.T) {
	data := pattern(10 * 1024)
	port := &scriptedWritePort{}
	a := newTestAgent(port, &fakeClipboard{})

	n, err := a.writeSerial(context.Background(), data)
	if err != nil || n != len(data) {
		t.Fatalf("writeSerial = (%d, %v), want (%d, nil)", n, err, len(data))
	}
	if port.maxOffer > serialWriteSlice {
		t.Fatalf("largest single write = %d bytes, want <= %d", port.maxOffer, serialWriteSlice)
	}
	if got := port.received(); string(got) != string(data) {
		t.Fatalf("port received %d bytes, want %d", len(got), len(data))
	}
}

// TestWriteSerial_ZeroProgressFails verifies the stall guard: two consecutive
// zero-byte writes mean the port stopped draining — fail bounded, don't spin.
func TestWriteSerial_ZeroProgressFails(t *testing.T) {
	port := &scriptedWritePort{script: []writeResp{
		{max: 0}, {max: 0}, {max: 0}, {max: 0}, {max: 0},
	}}
	a := newTestAgent(port, &fakeClipboard{})

	n, err := a.writeSerial(context.Background(), pattern(64))
	if err == nil {
		t.Fatal("expected stall error after consecutive zero-byte writes, got nil")
	}
	if n != 0 {
		t.Fatalf("n = %d, want 0", n)
	}
	if port.calls > 2 {
		t.Fatalf("port.Write called %d times, want 2 (fail on second consecutive zero)", port.calls)
	}
}

// TestWriteSerial_SingleZeroThenProgressRecovers verifies one zero-byte write
// (a transient full buffer) does not abort as long as progress resumes.
func TestWriteSerial_SingleZeroThenProgressRecovers(t *testing.T) {
	data := pattern(64)
	port := &scriptedWritePort{script: []writeResp{
		{max: 10}, {max: 0}, {max: 10}, {max: 0}, {max: 10},
	}}
	a := newTestAgent(port, &fakeClipboard{})

	n, err := a.writeSerial(context.Background(), data)
	if err != nil || n != len(data) {
		t.Fatalf("writeSerial = (%d, %v), want (%d, nil)", n, err, len(data))
	}
	if got := port.received(); string(got) != string(data) {
		t.Fatalf("port received %d bytes, want %d", len(got), len(data))
	}
}

// TestWriteSerial_ContextCancelStopsRetry verifies a cancelled context stops
// the retry loop between slices instead of grinding on a dying port.
func TestWriteSerial_ContextCancelStopsRetry(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	port := &scriptedWritePort{script: []writeResp{{max: 10}}}
	port.onWrite = cancel
	a := newTestAgent(port, &fakeClipboard{})

	n, err := a.writeSerial(ctx, pattern(64))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if n != 10 {
		t.Fatalf("n = %d, want 10 (the bytes written before cancellation)", n)
	}
}

// TestWriteSerial_HardErrorPropagates verifies a real port error (e.g. closed
// on shutdown) is returned immediately, not retried.
func TestWriteSerial_HardErrorPropagates(t *testing.T) {
	boom := errors.New("port closed")
	port := &scriptedWritePort{script: []writeResp{{max: 10}, {err: boom}}}
	a := newTestAgent(port, &fakeClipboard{})

	n, err := a.writeSerial(context.Background(), pattern(64))
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want %v", err, boom)
	}
	if n != 10 {
		t.Fatalf("n = %d, want 10", n)
	}
	if port.calls != 2 {
		t.Fatalf("port.Write called %d times, want 2 (no retry after hard error)", port.calls)
	}
}

// TestClipboardRequest_OutboundCap verifies handleClipboardRequest refuses to
// stream an oversized guest clipboard: nothing is written to the port, no
// error bubbles (the session stays alive), and at the boundary the data still
// flows.
func TestClipboardRequest_OutboundCap(t *testing.T) {
	t.Run("over the cap is dropped", func(t *testing.T) {
		port := &fakePort{}
		clip := &fakeClipboard{readText: strings.Repeat("x", maxOutboundClipboard+1)}
		a := newTestAgent(port, clip)

		req := BuildClipboardRequest(SelectionClipboard, ClipboardUTF8Text, false)
		if err := a.handleClipboardRequest(context.Background(), req); err != nil {
			t.Fatalf("handleClipboardRequest: %v", err)
		}
		if port.writtenCount() != 0 {
			t.Fatalf("expected no outbound writes for oversized clipboard, got %d", port.writtenCount())
		}
	})

	t.Run("at the cap is sent", func(t *testing.T) {
		port := &fakePort{}
		clip := &fakeClipboard{readText: strings.Repeat("x", maxOutboundClipboard)}
		a := newTestAgent(port, clip)

		req := BuildClipboardRequest(SelectionClipboard, ClipboardUTF8Text, false)
		if err := a.handleClipboardRequest(context.Background(), req); err != nil {
			t.Fatalf("handleClipboardRequest: %v", err)
		}
		if port.writtenCount() == 0 {
			t.Fatal("expected clipboard data at the cap boundary to be sent")
		}
	})
}
