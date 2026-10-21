// Serial receive path — split so that NOTHING the agent does can stall the tty.
//
// A dedicated goroutine does nothing but Read() the port into a bounded ring;
// the read loop consumes the ring. This matters because handling a message can
// block for SECONDS (sanitizeAndWriteClipboard retries an OS clipboard write up
// to 3×3s under Windows OpenClipboard contention), and while the reader is
// blocked the kernel/driver receive buffer keeps filling from QEMU and silently
// DROPS whatever it cannot hold. That is the "drop-while-busy" half of the
// measured ~4 KB host→guest wall; the other half is the receive queue's default
// size, which serial_windows.go now enlarges via SetupComm.
//
// Neither OS gives us flow control here (qemu-vdagent drives no modem lines), so
// capacity and drain rate are the only two levers — this file is the drain rate.

package vdagent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"time"
)

const (
	// drainReadChunk is one port.Read. Large reads amortize the per-syscall and
	// (on the emulated UART) per-VM-exit cost that made the old 1024-byte,
	// two-syscalls-per-chunk read path expensive.
	drainReadChunk = 64 * 1024
	// drainRingBytes bounds how much inbound data is buffered while the consumer
	// is busy. Sized to cover a worst-case clipboard-write stall at the serial's
	// ceiling with headroom, so a stall costs LATENCY, never bytes.
	drainRingBytes = 4 * 1024 * 1024
	// defaultRxCapacity is the conservative burst assumption when the platform
	// cannot report one — the historical ~4 KB wall.
	defaultRxCapacity = 4096
	// drainPollInterval matches the platform read timeout: how long the consumer
	// waits for more bytes before re-evaluating the stall deadline.
	drainPollInterval = 100 * time.Millisecond
	// drainIdleBackoff yields between empty reads. Real ports block internally
	// (Windows read timeouts, Linux VMIN=0/VTIME=1), so this never fires in the
	// field; it stops a backend that returns (0, nil) immediately from spinning
	// this goroutine hot.
	drainIdleBackoff = 2 * time.Millisecond
)

// serialDiag is implemented by serial ports that can report the receive capacity
// the OS actually GRANTED and any drops it observed. A port that cannot report
// either is treated as defaultRxCapacity with no visibility.
type serialDiag interface {
	// RxCapacity is how large an inbound burst this guest can absorb, in bytes.
	RxCapacity() int
	// Overruns counts receive drops since open. Non-zero means lost bytes.
	Overruns() uint64
}

// rxCapacity reports the negotiated-capacity claim for this port, clamped to
// what we can actually buffer. Never reports more than the ring can hold.
func (a *Agent) rxCapacity() int {
	n := defaultRxCapacity
	if d, ok := a.port.(serialDiag); ok {
		if c := d.RxCapacity(); c > 0 {
			n = c
		}
	}
	if n > drainRingBytes {
		n = drainRingBytes
	}
	return n
}

// serialOverruns reports receive drops observed by the platform, if it can see
// them at all. Non-zero is always data loss and always worth surfacing.
func (a *Agent) serialOverruns() (uint64, bool) {
	d, ok := a.port.(serialDiag)
	if !ok {
		return 0, false
	}
	return d.Overruns(), true
}

// logSerialDrops surfaces any NEW receive overrun. A non-zero count means the
// OS threw inbound bytes away — silent data loss. The absence of this signal is
// precisely how the host→guest wall hid for months: nothing ever called
// ClearCommError/TIOCGICOUNT, so the drops were invisible. Logged once per
// increase (never spammed), at ERROR because it is always real data loss.
func (a *Agent) logSerialDrops() {
	n, ok := a.serialOverruns()
	if !ok || n == 0 || n == a.lastOverruns {
		return
	}
	a.lastOverruns = n
	a.log.Error("vdagent: SERIAL RECEIVE OVERRUN — the OS dropped inbound bytes",
		"overruns", n, "rx_capacity", a.rxCapacity())
}

// startDrain launches the drain goroutine. Must be called before readLoop.
func (a *Agent) startDrain(ctx context.Context) {
	a.drainCh = make(chan []byte, drainRingBytes/drainReadChunk)
	go a.drainLoop(ctx)
}

// drainLoop reads the port as fast as it will yield and hands the bytes to the
// consumer. It does NOTHING else — no parsing, no clipboard, no locks — so the
// tty is drained continuously regardless of what the rest of the agent is doing.
func (a *Agent) drainLoop(ctx context.Context) {
	defer close(a.drainCh)
	buf := make([]byte, drainReadChunk)
	for {
		if ctx.Err() != nil {
			return
		}
		n, err := a.port.Read(buf)
		if n > 0 {
			b := make([]byte, n)
			copy(b, buf[:n])
			select {
			case a.drainCh <- b:
			case <-ctx.Done():
				return
			}
		}
		if err != nil && !isReadTimeout(err) {
			// A real error (port closed on shutdown, backend swap). Hand it to
			// the consumer rather than logging here — readLoop owns recovery.
			a.drainErr.Store(&err)
			return
		}
		if n == 0 {
			select {
			case <-ctx.Done():
				return
			case <-time.After(drainIdleBackoff):
			}
		}
	}
}

// isReadTimeout classifies a (0, err) read as the platform's "no data this
// interval" rather than a failure: Windows ReadFile timeout returns (0, nil);
// Linux VMIN=0/VTIME=1 returns (0, io.EOF).
func isReadTimeout(err error) bool {
	return err == nil || errors.Is(err, io.EOF) || errors.Is(err, os.ErrDeadlineExceeded)
}

// readFrameBytes fills buf from the drain ring.
//
//   - With NO bytes yet (a frame boundary), idle is the normal state — wait
//     indefinitely.
//   - Once partial bytes have arrived, a sustained no-data stretch
//     (> frameStallTimeout) means the sender vanished mid-frame (chardev swap /
//     guest reboot); return an error so readLoop resets reassembly and recovers
//     instead of blocking forever on a tail that will never come.
//   - A closed ring means the drain goroutine exited: surface its error.
func (a *Agent) readFrameBytes(buf []byte) error {
	got := 0
	var stallSince time.Time
	for got < len(buf) {
		if len(a.drainLeft) > 0 {
			n := copy(buf[got:], a.drainLeft)
			a.drainLeft = a.drainLeft[n:]
			got += n
			stallSince = time.Time{} // progress resets the stall timer
			continue
		}
		select {
		case b, ok := <-a.drainCh:
			if !ok {
				if p := a.drainErr.Load(); p != nil {
					return *p
				}
				return io.EOF
			}
			a.drainLeft = b
		case <-time.After(drainPollInterval):
			if got == 0 {
				continue // frame boundary — idle is normal, keep waiting
			}
			if stallSince.IsZero() {
				stallSince = time.Now()
			} else if time.Since(stallSince) > frameStallTimeout {
				return fmt.Errorf("serial frame stalled after %d/%d bytes (backend swap?)", got, len(buf))
			}
		}
	}
	return nil
}
