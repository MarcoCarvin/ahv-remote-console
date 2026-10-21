package vdagent

import (
	"context"
	"encoding/binary"
	"fmt"
	"strings"
	"testing"
)

// countGrabWrites counts the writes that carry a CLIPBOARD_GRAB message (each is
// a single small serial write).
func countGrabWrites(p *fakePort) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := 0
	for _, raw := range p.writes {
		if len(raw) >= ChunkHeaderSize+8 &&
			binary.LittleEndian.Uint32(raw[ChunkHeaderSize+4:ChunkHeaderSize+8]) == MsgClipboardGrab {
			n++
		}
	}
	return n
}

// totalWritten sums the bytes of every Write the fake port received (a large
// message is split across many writeSerial slices).
func totalWritten(p *fakePort) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := 0
	for _, w := range p.writes {
		n += len(w)
	}
	return n
}

// TestReverseBlob_RoundTrip drives the guest→host sender end to end: startReverseBlob
// spins the goroutine, and this test plays the host — serving each frame from the
// grab cache (handleClipboardRequest), reassembling it with the reference receiver
// (blobRx), and acking back through handleClipboardData. The reassembled text must
// equal the original, proving the sender's framing is wire-compatible with the
// receiver in both languages.
func TestReverseBlob_RoundTrip(t *testing.T) {
	port := &fakePort{}
	clip := &fakeClipboard{}
	a := newTestAgent(port, clip)
	a.connected = true
	a.clipboardEnabled = true
	a.blobTx.chunkBytes = 256 // force a multi-frame transfer

	var sb strings.Builder
	for i := 0; i < 2000; i++ {
		fmt.Fprintf(&sb, "log line %d: the quick brown fox %x\n", i, i*2654435761)
	}
	text := sb.String()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a.startReverseBlob(ctx, []byte(text))

	var recv blobRx
	var out string
	req := BuildClipboardRequest(SelectionClipboard, ClipboardUTF8Text, false)
	for k := 1; k <= 500; k++ {
		// GRAB k lands at write index 2k-2, so the count reaches 2k-1.
		waitFor(t, fmt.Sprintf("grab %d", k), func() bool { return port.writtenCount() >= 2*k-1 })

		if err := a.handleClipboardRequest(ctx, req); err != nil {
			t.Fatalf("serve frame %d: %v", k, err)
		}
		frame := writtenClipText(t, port, 2*k-1)
		if !strings.HasPrefix(frame, blobFramePrefix) {
			t.Fatalf("frame %d not a blob frame: %.40q", k, frame)
		}
		res := recv.ingest(strings.TrimPrefix(frame, blobFramePrefix), MaxClipboardSize)
		if res.Ack == "" {
			t.Fatalf("frame %d produced no ack", k)
		}
		if err := a.handleClipboardData(ctx,
			BuildClipboardData(SelectionClipboard, ClipboardUTF8Text, []byte(res.Ack), false)); err != nil {
			t.Fatalf("deliver ack %d: %v", k, err)
		}
		if res.Complete {
			out = res.Text
			break
		}
	}
	if out != text {
		t.Fatalf("reassembled %d bytes, want %d", len(out), len(text))
	}
	if got := len(recv.buf); got != 0 {
		t.Fatalf("receiver state not reset after completion")
	}
}

// TestReverseBlob_OnAckRouting: onAck delivers a status only to the matching
// in-flight (xferId, seq) waiter; mismatches are ignored; a status containing a
// colon (err:crc) is rejoined intact.
func TestReverseBlob_OnAckRouting(t *testing.T) {
	a := newTestAgent(&fakePort{}, &fakeClipboard{})
	a.blobTx.activeID = "abcd1234"

	ch := a.armFrame("abcd1234", 3, []byte("frame"))
	if ch == nil {
		t.Fatal("armFrame returned nil for the active transfer")
	}

	// Wrong seq — ignored.
	a.blobTx.onAck(blobAck("abcd1234", 2, "ok"))
	// Wrong xferId — ignored.
	a.blobTx.onAck(blobAck("ffffffff", 3, "ok"))
	select {
	case s := <-ch:
		t.Fatalf("waiter resolved by a non-matching ack: %q", s)
	default:
	}

	// Matching ack with a colon in the status.
	a.blobTx.onAck(blobAck("abcd1234", 3, "err:crc"))
	select {
	case s := <-ch:
		if s != "err:crc" {
			t.Fatalf("status = %q, want err:crc", s)
		}
	default:
		t.Fatal("matching ack did not resolve the waiter")
	}

	// A superseded transfer refuses to arm.
	a.blobTx.cancel()
	if a.armFrame("abcd1234", 4, []byte("frame")) != nil {
		t.Fatal("armFrame succeeded after cancel")
	}
}

// TestReverseBlob_CancelUnblocks: a frame blocked on its ack is released with a
// "cancelled" status when a newer copy supersedes it.
func TestReverseBlob_CancelUnblocks(t *testing.T) {
	a := newTestAgent(&fakePort{}, &fakeClipboard{})
	a.blobTx.activeID = "deadbeef"
	ch := a.armFrame("deadbeef", 1, []byte("frame"))
	a.blobTx.cancel()
	select {
	case s := <-ch:
		if s != "cancelled" {
			t.Fatalf("status = %q, want cancelled", s)
		}
	default:
		t.Fatal("cancel did not release the blocked waiter")
	}
}

// TestReleaseTransfer_OwnerGuardsCache: a finishing/superseded reverse goroutine
// must only wipe its OWN residual frame. If another producer (keep-warm ack,
// operator copy, a newer transfer) took the grab-cache slot in between,
// releaseTransfer must leave it untouched — the two-lock TOCTOU the review found.
func TestReleaseTransfer_OwnerGuardsCache(t *testing.T) {
	a := newTestAgent(&fakePort{}, &fakeClipboard{})

	// Transfer A is active and has stamped its frame into the cache.
	a.blobTx.activeID = "aaaaaaaa"
	if a.armFrame("aaaaaaaa", 1, []byte("frameA")) == nil {
		t.Fatal("armFrame A returned nil")
	}
	// While A is still active, a keep-warm ack takes the slot (owner cleared).
	a.setGrabCache([]byte("keepwarm-ack"), true)

	// A's goroutine finishes and releases — it must NOT wipe the keep-warm ack.
	a.releaseTransfer("aaaaaaaa")
	if got := string(grabCacheState(a)); got != "keepwarm-ack" {
		t.Fatalf("releaseTransfer(A) wiped a non-owned cache entry: got %q", got)
	}

	// And a superseded transfer (activeID moved on) also leaves the cache alone.
	a.blobTx.activeID = "bbbbbbbb"
	a.releaseTransfer("aaaaaaaa")
	if got := string(grabCacheState(a)); got != "keepwarm-ack" {
		t.Fatalf("stale releaseTransfer disturbed the cache: got %q", got)
	}
}

// TestReverseBlob_RestartRewinds: a reconnect resets the host receiver
// mid-transfer; it acks err:restart, and the agent sender must rewind to seq 1
// and complete rather than hang out the ack timeout and drop the copy.
func TestReverseBlob_RestartRewinds(t *testing.T) {
	port := &fakePort{}
	clip := &fakeClipboard{}
	a := newTestAgent(port, clip)
	a.connected = true
	a.clipboardEnabled = true
	a.blobTx.chunkBytes = 256

	var sb strings.Builder
	for i := 0; i < 1500; i++ {
		fmt.Fprintf(&sb, "row %d value %x\n", i, i*40503)
	}
	text := sb.String()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a.startReverseBlob(ctx, []byte(text))

	var recv blobRx
	var out string
	req := BuildClipboardRequest(SelectionClipboard, ClipboardUTF8Text, false)
	injected := false
	served := 0
	for iter := 0; iter < 2000 && out == ""; iter++ {
		waitFor(t, "next grab", func() bool { return countGrabWrites(port) > served })
		if err := a.handleClipboardRequest(ctx, req); err != nil {
			t.Fatalf("serve: %v", err)
		}
		served++
		frame := writtenClipText(t, port, port.writtenCount()-1)
		body := strings.TrimPrefix(frame, blobFramePrefix)
		fields := strings.SplitN(body, ":", 6)
		xferID, seq := fields[0], fields[1]

		// On the first frame, simulate a receiver that reset (reconnect): ask the
		// sender to rewind instead of acking normally.
		if seq == "1" && !injected {
			injected = true
			restart := blobAckPrefix + xferID + ":1:err:restart"
			if err := a.handleClipboardData(ctx,
				BuildClipboardData(SelectionClipboard, ClipboardUTF8Text, []byte(restart), false)); err != nil {
				t.Fatalf("deliver restart: %v", err)
			}
			continue
		}
		res := recv.ingest(body, MaxClipboardSize)
		if res.Ack != "" {
			if err := a.handleClipboardData(ctx,
				BuildClipboardData(SelectionClipboard, ClipboardUTF8Text, []byte(res.Ack), false)); err != nil {
				t.Fatalf("deliver ack: %v", err)
			}
		}
		if res.Complete {
			out = res.Text
		}
	}
	if !injected {
		t.Fatal("restart path was never exercised (transfer was single-frame?)")
	}
	if out != text {
		t.Fatalf("transfer did not recover after restart: got %d bytes, want %d", len(out), len(text))
	}
}

// TestHandleClipboardRequest_SideChannelExempt: a reverse-BCP frame exceeds the
// direct-copy cap by design (it IS the chunking) and must be served, while a
// same-size ordinary guest copy is still dropped.
func TestHandleClipboardRequest_SideChannelExempt(t *testing.T) {
	req := BuildClipboardRequest(SelectionClipboard, ClipboardUTF8Text, false)

	// A frame larger than the direct cap, tagged as protocol side-channel.
	// (writeSerial splits a large message across many Write calls, so assert on
	// total bytes emitted, not the write count.)
	big := blobFramePrefix + strings.Repeat("A", maxOutboundClipboard+1024)
	port := &fakePort{}
	a := newTestAgent(port, &fakeClipboard{})
	a.setGrabCache([]byte(big), true)
	if err := a.handleClipboardRequest(context.Background(), req); err != nil {
		t.Fatalf("serve oversized frame: %v", err)
	}
	if got := totalWritten(port); got < len(big) {
		t.Fatalf("oversized __AHVRC_ frame not fully served: %d bytes out, want >= %d", got, len(big))
	}

	// The same size WITHOUT the side-channel prefix is still dropped.
	port2 := &fakePort{}
	a2 := newTestAgent(port2, &fakeClipboard{})
	a2.setGrabCache([]byte(strings.Repeat("A", maxOutboundClipboard+1024)), false)
	if err := a2.handleClipboardRequest(context.Background(), req); err != nil {
		t.Fatalf("oversized operator copy: %v", err)
	}
	if port2.writtenCount() != 0 {
		t.Fatalf("oversized operator copy was sent: %d writes, want 0", port2.writtenCount())
	}
}

// TestGzipReverseRoundTrip: the sender's compressor and the receiver's
// decompressor agree (gzipBytes → gunzipText).
func TestGzipReverseRoundTrip(t *testing.T) {
	text := strings.Repeat("kind: ConfigMap\ndata:\n  k: v\n", 500)
	z, err := gzipBytes([]byte(text))
	if err != nil {
		t.Fatalf("gzipBytes: %v", err)
	}
	out, err := gunzipText(z, len(text))
	if err != nil {
		t.Fatalf("gunzipText: %v", err)
	}
	if out != text {
		t.Fatal("gzip round-trip mismatch")
	}
}

// TestPollClipboard_ProtocolCacheSurvivesEcho is the ack-wipe regression guard:
// after handleBlobFrame writes the host's paste into the guest clipboard, that
// write echoes back as a watch event. pollClipboard must NOT wipe the pending
// PROTOCOL grab cache (the done-ACK the host is about to REQUEST) on that echo —
// wiping it stranded the final ACK and stalled every large paste ~10s.
func TestPollClipboard_ProtocolCacheSurvivesEcho(t *testing.T) {
	echoed := "the large paste that we just wrote to the guest"
	clip := &fakeClipboard{readText: echoed}
	clip.watchCh = make(chan struct{})
	port := &fakePort{}
	a := newTestAgent(port, clip)
	a.connected = true
	a.clipboardEnabled = true

	// A protocol done-ACK is cached, awaiting the host's REQUEST — exactly the
	// state sendBlobAck leaves after writing the guest clipboard.
	ackText := blobAck("cafef00d", 7, "done")
	a.setGrabCache([]byte(ackText), true)
	// The echo the guest clipboard will replay matches our just-written text.
	// Record it the way the production write path does (markSelfWriteLocked),
	// which arms the single-consume echo marker pollClipboard checks.
	a.mu.Lock()
	a.markSelfWriteLocked(hashText(canonicalClip(echoed)))
	a.mu.Unlock()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { a.pollClipboard(ctx); close(done) }()
	defer func() { cancel(); <-done }()

	// Fire the echo watch event and let pollClipboard process it.
	clip.watchCh <- struct{}{}
	// The event is echo-suppressed (no new grab), and the protocol cache must
	// still be intact for the host's REQUEST.
	waitFor(t, "protocol cache preserved", func() bool {
		return string(grabCacheState(a)) == ackText
	})
	// No grab should have been sent for the echo.
	if port.writtenCount() != 0 {
		t.Fatalf("echo produced %d writes, want 0 (suppressed)", port.writtenCount())
	}
}
