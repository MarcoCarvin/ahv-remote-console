package vdagent

import (
	"context"
	"encoding/binary"
	"sync"
	"testing"
	"time"
)

// fakeKeepwarmLease records Arm/Disarm calls from the agent's keep-warm
// side-channel intercept.
type fakeKeepwarmLease struct {
	mu      sync.Mutex
	arms    []time.Duration
	disarms int
}

func (f *fakeKeepwarmLease) Arm(ttl time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.arms = append(f.arms, ttl)
}

func (f *fakeKeepwarmLease) Disarm() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.disarms++
}

// Stop counts as a disarm for assertions — the synchronous wait is the real
// Lease's concern, exercised by the keepwarm package's own tests.
func (f *fakeKeepwarmLease) Stop() { f.Disarm() }

func (f *fakeKeepwarmLease) state() ([]time.Duration, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]time.Duration(nil), f.arms...), f.disarms
}

// kwClipPayload builds a CLIPBOARD message body ([type uint32][text],
// useSelection=false) for feeding handleClipboardData directly.
func kwClipPayload(text string) []byte {
	body := make([]byte, 4+len(text))
	binary.LittleEndian.PutUint32(body[0:4], ClipboardUTF8Text)
	copy(body[4:], text)
	return body
}

// TestKeepwarm_LeaseArmsAndNeverTouchesClipboard: a "__AHVRC_KEEPWARM__<ttl>"
// token arms the lease, records the echo hash exactly like resize does, and
// is NEVER written to the guest clipboard.
func TestKeepwarm_LeaseArmsAndNeverTouchesClipboard(t *testing.T) {
	port := &fakePort{}
	clip := &fakeClipboard{}
	a := newTestAgent(port, clip)
	kw := &fakeKeepwarmLease{}
	a.keepwarm = kw

	token := keepwarmPrefix + "45"
	if err := a.handleClipboardData(context.Background(), kwClipPayload(token)); err != nil {
		t.Fatalf("handleClipboardData: %v", err)
	}

	arms, disarms := kw.state()
	if len(arms) != 1 || arms[0] != 45*time.Second {
		t.Fatalf("arms = %v, want one 45s arm", arms)
	}
	if disarms != 0 {
		t.Fatalf("disarms = %d, want 0", disarms)
	}
	if got, ok := clip.lastWritten(); ok {
		t.Fatalf("keepwarm token reached the guest clipboard: %q", got)
	}
	a.mu.Lock()
	gotHash := a.lastClipHash
	a.mu.Unlock()
	if gotHash != hashText(token) {
		t.Fatal("echo hash not recorded for keepwarm token")
	}
}

// TestKeepwarm_CancelDisarms: ttl=0 disarms immediately, no clipboard write.
func TestKeepwarm_CancelDisarms(t *testing.T) {
	port := &fakePort{}
	clip := &fakeClipboard{}
	a := newTestAgent(port, clip)
	kw := &fakeKeepwarmLease{}
	a.keepwarm = kw

	if err := a.handleClipboardData(context.Background(), kwClipPayload(keepwarmPrefix+"0")); err != nil {
		t.Fatalf("handleClipboardData: %v", err)
	}
	arms, disarms := kw.state()
	if len(arms) != 0 || disarms != 1 {
		t.Fatalf("arms=%v disarms=%d, want 0 arms / 1 disarm", arms, disarms)
	}
	if _, ok := clip.lastWritten(); ok {
		t.Fatal("cancel token reached the guest clipboard")
	}
}

// TestKeepwarm_MalformedDropped: unparseable / out-of-range TTLs are dropped —
// no lease change AND no guest clipboard write.
func TestKeepwarm_MalformedDropped(t *testing.T) {
	for _, suffix := range []string{"", "abc", "-5", "301", "45x"} {
		t.Run("suffix_"+suffix, func(t *testing.T) {
			port := &fakePort{}
			clip := &fakeClipboard{}
			a := newTestAgent(port, clip)
			kw := &fakeKeepwarmLease{}
			a.keepwarm = kw

			if err := a.handleClipboardData(context.Background(), kwClipPayload(keepwarmPrefix+suffix)); err != nil {
				t.Fatalf("handleClipboardData: %v", err)
			}
			arms, disarms := kw.state()
			if len(arms) != 0 || disarms != 0 {
				t.Fatalf("arms=%v disarms=%d, want no lease change", arms, disarms)
			}
			if got, ok := clip.lastWritten(); ok {
				t.Fatalf("malformed keepwarm token reached the guest clipboard: %q", got)
			}
		})
	}
}

// TestKeepwarm_ProbeAcksWithoutSideEffects: the resize-namespace probe sends
// the ack grab, serves keepwarmAckText to the host's CLIPBOARD_REQUEST from
// the grab cache (no OS clipboard read), and never touches the resizer, the
// lease, or the guest clipboard.
func TestKeepwarm_ProbeAcksWithoutSideEffects(t *testing.T) {
	port := &fakePort{}
	clip := &fakeClipboard{readText: "real-guest-clipboard"}
	a := newTestAgent(port, clip)
	kw := &fakeKeepwarmLease{}
	a.keepwarm = kw
	rz := &recordingResizer{}
	a.SetDisplay(rz)

	if err := a.handleClipboardData(context.Background(), kwClipPayload(keepwarmProbeText)); err != nil {
		t.Fatalf("handleClipboardData(probe): %v", err)
	}

	// One frame written: the ack GRAB.
	if n := port.writtenCount(); n != 1 {
		t.Fatalf("frames written = %d, want 1 (the ack grab)", n)
	}
	port.mu.Lock()
	grabType := binary.LittleEndian.Uint32(port.writes[0][ChunkHeaderSize+4 : ChunkHeaderSize+8])
	port.mu.Unlock()
	if grabType != MsgClipboardGrab {
		t.Fatalf("written msg type = %d, want %d (MsgClipboardGrab)", grabType, MsgClipboardGrab)
	}

	// The host's REQUEST is served the ack token from the grab cache.
	req := BuildClipboardRequest(SelectionClipboard, ClipboardUTF8Text, false)
	if err := a.handleClipboardRequest(context.Background(), req); err != nil {
		t.Fatalf("handleClipboardRequest: %v", err)
	}
	if got := writtenClipText(t, port, 1); got != keepwarmAckText {
		t.Fatalf("served %q to the host, want the keepwarm ack", got)
	}
	if n := clip.readCount(); n != 0 {
		t.Fatalf("OS clipboard read %d times for the ack, want 0", n)
	}

	if rz.count() != 0 {
		t.Fatal("probe reached the display resizer")
	}
	arms, disarms := kw.state()
	if len(arms) != 0 || disarms != 0 {
		t.Fatalf("probe changed the lease: arms=%v disarms=%d", arms, disarms)
	}
	if _, ok := clip.lastWritten(); ok {
		t.Fatal("probe reached the guest clipboard")
	}
}

// TestKeepwarm_ResizeStillRoutes: guard that the probe check inside the
// __AHVRC_RESIZE__ branch did not break real resize commands.
func TestKeepwarm_ResizeStillRoutes(t *testing.T) {
	port := &fakePort{}
	clip := &fakeClipboard{}
	a := newTestAgent(port, clip)
	kw := &fakeKeepwarmLease{}
	a.keepwarm = kw
	rz := &recordingResizer{}
	a.SetDisplay(rz)

	if err := a.handleClipboardData(context.Background(), kwClipPayload("__AHVRC_RESIZE__1920x1080")); err != nil {
		t.Fatalf("handleClipboardData(resize): %v", err)
	}
	if rz.count() != 1 {
		t.Fatalf("resizer calls = %d, want 1", rz.count())
	}
	arms, disarms := kw.state()
	if len(arms) != 0 || disarms != 0 {
		t.Fatalf("resize changed the lease: arms=%v disarms=%d", arms, disarms)
	}
	if _, ok := clip.lastWritten(); ok {
		t.Fatal("resize token reached the guest clipboard")
	}
}

// TestKeepwarm_ProbeDeferredWhileRealGrabPending: a probe arriving while a
// REAL guest copy is mid-flight (grab cache armed, host REQUEST not served
// yet) must NOT clobber the cache with the ack — the operator's copy would be
// silently lost and swallowed by the proxy. The probe is skipped (the proxy
// re-probes on its renewal cadence); a STALE cache (expired TTL, REQUEST
// never arrived) must not defer the ack forever.
func TestKeepwarm_ProbeDeferredWhileRealGrabPending(t *testing.T) {
	port := &fakePort{}
	clip := &fakeClipboard{}
	a := newTestAgent(port, clip)

	a.setGrabCache([]byte("operator copy"), false)

	if err := a.handleClipboardData(context.Background(), kwClipPayload(keepwarmProbeText)); err != nil {
		t.Fatalf("handleClipboardData(probe): %v", err)
	}
	if n := port.writtenCount(); n != 0 {
		t.Fatalf("frames written = %d, want 0 (probe deferred while grab pending)", n)
	}

	// The pending REAL copy still serves the host's REQUEST.
	req := BuildClipboardRequest(SelectionClipboard, ClipboardUTF8Text, false)
	if err := a.handleClipboardRequest(context.Background(), req); err != nil {
		t.Fatalf("handleClipboardRequest: %v", err)
	}
	if got := writtenClipText(t, port, 0); got != "operator copy" {
		t.Fatalf("served %q to the host, want the pending operator copy", got)
	}

	// Stale cache: REQUEST never arrived and the TTL lapsed — the next probe
	// must ack normally instead of deferring forever.
	a.setGrabCache([]byte("abandoned copy"), false)
	a.mu.Lock()
	a.grabCacheTime = time.Now().Add(-grabCacheTTL - time.Second)
	a.mu.Unlock()
	if err := a.handleClipboardData(context.Background(), kwClipPayload(keepwarmProbeText)); err != nil {
		t.Fatalf("handleClipboardData(probe, stale cache): %v", err)
	}
	if n := port.writtenCount(); n != 2 {
		t.Fatalf("frames written = %d, want 2 (ack grab after stale cache)", n)
	}
}
