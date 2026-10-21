package vdagent

import (
	"context"
	"encoding/binary"
	"testing"
	"time"
)

// grabCacheState snapshots the agent's pending grab cache under lock.
func grabCacheState(a *Agent) []byte {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.grabCache
}

func allZero(b []byte) bool {
	for _, c := range b {
		if c != 0 {
			return false
		}
	}
	return true
}

// writtenClipText decodes the idx-th written frame as a CLIPBOARD data
// message (no selection prefix) and returns its text payload.
func writtenClipText(t *testing.T, port *fakePort, idx int) string {
	t.Helper()
	port.mu.Lock()
	defer port.mu.Unlock()
	if len(port.writes) <= idx {
		t.Fatalf("only %d frames written, want index %d", len(port.writes), idx)
	}
	raw := port.writes[idx]
	if len(raw) < ChunkHeaderSize+MessageHeaderSize+4 {
		t.Fatalf("written frame too short: %d bytes", len(raw))
	}
	msgType := binary.LittleEndian.Uint32(raw[ChunkHeaderSize+4 : ChunkHeaderSize+8])
	if msgType != MsgClipboard {
		t.Fatalf("written msg type = %d, want %d (MsgClipboard)", msgType, MsgClipboard)
	}
	body := raw[ChunkHeaderSize+MessageHeaderSize:]
	return string(body[4:]) // [type uint32][text] (useSelection=false)
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// TestGrabCache_ServeAndWipeOnUse: a CLIPBOARD_REQUEST is served from the
// text captured at GRAB time — the OS clipboard must NOT be read a second
// time (under Windows contention that read can stall 2s or transiently come
// back empty, losing the guest copy). The cache is single-use: its bytes are
// zeroed when consumed, and the next REQUEST falls back to a live read.
func TestGrabCache_ServeAndWipeOnUse(t *testing.T) {
	port := &fakePort{}
	clip := &fakeClipboard{readText: "live-read"}
	a := newTestAgent(port, clip)

	a.setGrabCache([]byte("grab-time-text"), false)
	captured := grabCacheState(a)

	req := BuildClipboardRequest(SelectionClipboard, ClipboardUTF8Text, false)
	if err := a.handleClipboardRequest(context.Background(), req); err != nil {
		t.Fatalf("handleClipboardRequest: %v", err)
	}
	if got := writtenClipText(t, port, 0); got != "grab-time-text" {
		t.Fatalf("served %q, want the grab-time cache %q", got, "grab-time-text")
	}
	if n := clip.readCount(); n != 0 {
		t.Fatalf("OS clipboard read %d times, want 0 (cache must be served)", n)
	}
	if !allZero(captured) {
		t.Fatal("cached cleartext not zeroed after use")
	}
	if grabCacheState(a) != nil {
		t.Fatal("grab cache still set after use")
	}

	// A second REQUEST (retry) must fall back to a live read.
	if err := a.handleClipboardRequest(context.Background(), req); err != nil {
		t.Fatalf("handleClipboardRequest(retry): %v", err)
	}
	if got := writtenClipText(t, port, 1); got != "live-read" {
		t.Fatalf("retry served %q, want live read %q", got, "live-read")
	}
	if n := clip.readCount(); n != 1 {
		t.Fatalf("OS clipboard read %d times after retry, want 1", n)
	}
}

// TestGrabCache_FallbackOnEmpty: with no pending cache, a REQUEST is served
// by a live clipboard read (covers REQUEST retries and pre-grab requests).
func TestGrabCache_FallbackOnEmpty(t *testing.T) {
	port := &fakePort{}
	clip := &fakeClipboard{readText: "live-read"}
	a := newTestAgent(port, clip)

	req := BuildClipboardRequest(SelectionClipboard, ClipboardUTF8Text, false)
	if err := a.handleClipboardRequest(context.Background(), req); err != nil {
		t.Fatalf("handleClipboardRequest: %v", err)
	}
	if got := writtenClipText(t, port, 0); got != "live-read" {
		t.Fatalf("served %q, want %q", got, "live-read")
	}
	if n := clip.readCount(); n != 1 {
		t.Fatalf("OS clipboard read %d times, want 1", n)
	}
}

// TestGrabCache_TTLExpiry: a cache whose REQUEST never arrived within the TTL
// is not served — the stale bytes are zeroed and a live read takes over.
func TestGrabCache_TTLExpiry(t *testing.T) {
	port := &fakePort{}
	clip := &fakeClipboard{readText: "live-read"}
	a := newTestAgent(port, clip)

	a.setGrabCache([]byte("stale-grab"), false)
	captured := grabCacheState(a)
	a.mu.Lock()
	a.grabCacheTime = time.Now().Add(-grabCacheTTL - time.Second)
	a.mu.Unlock()

	req := BuildClipboardRequest(SelectionClipboard, ClipboardUTF8Text, false)
	if err := a.handleClipboardRequest(context.Background(), req); err != nil {
		t.Fatalf("handleClipboardRequest: %v", err)
	}
	if got := writtenClipText(t, port, 0); got != "live-read" {
		t.Fatalf("served %q, want live read after TTL expiry", got)
	}
	if !allZero(captured) {
		t.Fatal("expired cache cleartext not zeroed")
	}
}

// TestGrabCache_InvalidatedOnNewWatchEvent drives pollClipboard end to end:
// each guest clipboard change wipes any pending cache (the old text is stale
// the moment the clipboard changes), and a new grab caches the new text —
// including events that dedupe out and send no grab at all.
func TestGrabCache_InvalidatedOnNewWatchEvent(t *testing.T) {
	port := &fakePort{}
	clip := &fakeClipboard{readText: "first"}
	clip.watchCh = make(chan struct{})
	a := newTestAgent(port, clip)
	a.connected = true
	a.clipboardEnabled = true

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		a.pollClipboard(ctx)
		close(done)
	}()
	defer func() {
		cancel()
		<-done
	}()

	// Event 1: guest copies "first" -> grab sent, cache holds "first".
	clip.watchCh <- struct{}{}
	waitFor(t, "first grab written", func() bool { return port.writtenCount() == 1 })
	cache1 := grabCacheState(a)
	if string(cache1) != "first" {
		t.Fatalf("cache after first grab = %q, want %q", cache1, "first")
	}

	// Event 2: guest copies "second" -> old cache wiped, new one cached.
	clip.mu.Lock()
	clip.readText = "second"
	clip.mu.Unlock()
	clip.watchCh <- struct{}{}
	waitFor(t, "second grab written", func() bool { return port.writtenCount() == 2 })
	if !allZero(cache1) {
		t.Fatal("previous cache cleartext not zeroed on new watch event")
	}
	cache2 := grabCacheState(a)
	if string(cache2) != "second" {
		t.Fatalf("cache after second grab = %q, want %q", cache2, "second")
	}

	// Event 3: same text again -> dedupes (no grab), but the pending cache is
	// still invalidated and wiped.
	clip.watchCh <- struct{}{}
	waitFor(t, "cache wiped on deduped event", func() bool { return grabCacheState(a) == nil })
	if !allZero(cache2) {
		t.Fatal("cache cleartext not zeroed on deduped watch event")
	}
	if port.writtenCount() != 2 {
		t.Fatalf("deduped event sent a grab: %d writes, want 2", port.writtenCount())
	}
}
