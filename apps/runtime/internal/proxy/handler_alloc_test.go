package proxy

import (
	"testing"

	"github.com/MarcoCarvin/ahv-remote-console/apps/runtime/internal/plugins"
	"github.com/MarcoCarvin/ahv-remote-console/apps/runtime/internal/rfb"
)

// fbuHotFrame builds a representative raw FramebufferUpdate — the dominant frame
// type on the upstream→client hot path at ~30fps motion.
func fbuHotFrame() []byte {
	px := make([]byte, 64*64*4)
	for i := range px {
		px[i] = byte(i*7 + 1)
	}
	return buildRawFBUpdate(0, 0, 64, 64, px)
}

// BenchmarkHandleServerBinaryFBU measures the per-frame cost of the
// upstream→client hot path for a FramebufferUpdate. The raw-passthrough fast
// path must stay allocation-free; `go test -bench . -benchmem` surfaces a
// regression that reintroduces a per-frame copy or the once-reverted PROT-07
// full reparse. There are no benchmarks elsewhere in the repo, so this is the
// first guardrail over the deliberately-landed proxy hot-path optimizations.
func BenchmarkHandleServerBinaryFBU(b *testing.B) {
	p := &proxy{sess: &plugins.Session{}, stream: rfb.NewStreamProcessor()}
	frame := fbuHotFrame()
	b.ReportAllocs()
	b.SetBytes(int64(len(frame)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = p.handleServerBinary(frame)
	}
}

// TestHandleServerBinaryFBUZeroAlloc is the GUARD behind the benchmark and the
// part that actually gates CI: the FramebufferUpdate fast path must forward the
// frame with ZERO allocations (it returns the input slice unchanged). A change
// that copies, re-slices, or reparses FBU frames — e.g. resurrecting PROT-07's
// full reparse, which regressed every Tight frame into a desync loop — makes
// this fail before it can ship.
func TestHandleServerBinaryFBUZeroAlloc(t *testing.T) {
	p := &proxy{sess: &plugins.Session{}, stream: rfb.NewStreamProcessor()}
	frame := fbuHotFrame()
	if allocs := testing.AllocsPerRun(200, func() {
		_ = p.handleServerBinary(frame)
	}); allocs != 0 {
		t.Fatalf("FBU raw passthrough must be allocation-free, got %.0f allocs/op", allocs)
	}
}
