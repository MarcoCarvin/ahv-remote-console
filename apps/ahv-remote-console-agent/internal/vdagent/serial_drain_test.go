package vdagent

import (
	"bytes"
	"testing"
	"time"
)

// TestDrain_AbsorbsBurstWhileConsumerStalled is the regression guard for the
// "drop-while-busy" half of the measured ~4 KB host→guest wall.
//
// The agent used to read the tty on the SAME goroutine that blocks for seconds
// writing the guest clipboard (sanitizeAndWriteClipboard retries 3×3s under
// Windows OpenClipboard contention). While that goroutine was blocked nothing
// drained the port, so QEMU's inbound burst overflowed the OS receive buffer and
// was silently discarded. The dedicated drain goroutine must absorb the whole
// burst no matter what the consumer is doing.
func TestDrain_AbsorbsBurstWhileConsumerStalled(t *testing.T) {
	// Comfortably larger than the ~4 KB wall and than any OS receive buffer.
	burst := bytes.Repeat([]byte("PAYLOAD-"), 32*1024) // 256 KB
	port := &fakePort{reads: []readResp{{data: burst}}}
	a := newTestAgent(port, &fakeClipboard{})
	startTestDrain(t, a)

	// Stand in for the consumer being busy on a long clipboard write: read
	// nothing at all while the burst arrives.
	time.Sleep(300 * time.Millisecond)

	got := make([]byte, len(burst))
	if err := a.readFrameBytes(got); err != nil {
		t.Fatalf("readFrameBytes after a stalled consumer: %v", err)
	}
	if !bytes.Equal(got, burst) {
		t.Fatalf("burst was truncated or corrupted while the consumer was stalled")
	}
}

// TestDrain_RxCapacityFallsBackWhenPortCannotReport: a port that can't report a
// granted receive queue must be treated as the conservative ~4 KB default, never
// optimistically — advertising a ceiling we can't honour is how a guest promises
// capacity and then drops bytes past it.
func TestDrain_RxCapacityFallsBackWhenPortCannotReport(t *testing.T) {
	a := newTestAgent(&fakePort{}, &fakeClipboard{})
	if got := a.rxCapacity(); got != defaultRxCapacity {
		t.Fatalf("rxCapacity = %d, want the conservative default %d", got, defaultRxCapacity)
	}
	if _, ok := a.serialOverruns(); ok {
		t.Fatal("a port with no diagnostics must report no overrun visibility")
	}
}

// diagPort is a fakePort that reports a granted receive queue and overruns.
type diagPort struct {
	fakePort
	cap  int
	over uint64
}

func (d *diagPort) RxCapacity() int  { return d.cap }
func (d *diagPort) Overruns() uint64 { return d.over }

// TestDrain_RxCapacityClampedToRing: we never advertise more than we can
// actually buffer, however large a queue the driver granted.
func TestDrain_RxCapacityClampedToRing(t *testing.T) {
	a := newTestAgent(&diagPort{cap: 64 << 20}, &fakeClipboard{})
	if got := a.rxCapacity(); got != drainRingBytes {
		t.Fatalf("rxCapacity = %d, want clamp to ring %d", got, drainRingBytes)
	}

	a2 := newTestAgent(&diagPort{cap: 256 << 10, over: 7}, &fakeClipboard{})
	if got := a2.rxCapacity(); got != 256<<10 {
		t.Fatalf("rxCapacity = %d, want the granted 256KB", got)
	}
	n, ok := a2.serialOverruns()
	if !ok || n != 7 {
		t.Fatalf("overruns = %d,%v; want 7,true", n, ok)
	}
}
