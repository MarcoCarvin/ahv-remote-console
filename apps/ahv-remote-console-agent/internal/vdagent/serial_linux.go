//go:build linux

package vdagent

import (
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"unsafe"

	"golang.org/x/sys/unix"
)

// serialICounter is linux/serial.h serial_icounter_struct. Only the overrun
// counters are read, but the layout must match for the ioctl to fill it.
type serialICounter struct {
	CTS        int32
	DSR        int32
	RNG        int32
	DCD        int32
	RX         int32
	TX         int32
	Frame      int32
	Overrun    int32 // UART FIFO overrun — bytes lost in hardware
	Parity     int32
	Brk        int32
	BufOverrun int32 // line-discipline buffer overflow — bytes lost in the kernel
	Reserved   [9]int32
}

// SerialPort wraps a Linux tty file descriptor as an io.ReadWriteCloser.
type SerialPort struct {
	f *os.File
	// Overruns are counted as a DELTA from the value at open, so a port that
	// was already dirty doesn't read as our data loss.
	baseline int32
	haveBase bool
	overruns atomic.Uint64
}

// OpenSerial opens a serial port configured for raw binary communication
// at 115200 8N1. Accepts Windows-style names ("COM3" → /dev/ttyS2) or
// Linux device paths ("/dev/ttyS2") directly.
func OpenSerial(name string) (io.ReadWriteCloser, error) {
	path := translatePort(name)

	f, err := os.OpenFile(path, unix.O_RDWR|unix.O_NOCTTY|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}

	fd := int(f.Fd())

	// Clear NONBLOCK now that we have the fd — we want blocking I/O
	// with VTIME-based read timeout, not poll/epoll.
	if err := unix.SetNonblock(fd, false); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("clear O_NONBLOCK: %w", err)
	}

	// Exclusive access — mirrors the Windows OPEN_EXISTING with
	// sharing=0. Prevents two bridges from racing on the same port.
	if err := unix.IoctlSetInt(fd, unix.TIOCEXCL, 0); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("TIOCEXCL: %w", err)
	}

	t, err := unix.IoctlGetTermios(fd, unix.TCGETS)
	if err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("TCGETS: %w", err)
	}

	// Raw mode: no echo, no canonical, no signal chars, no input
	// processing, no output processing.
	t.Iflag &^= unix.IGNBRK | unix.BRKINT | unix.PARMRK | unix.ISTRIP |
		unix.INLCR | unix.IGNCR | unix.ICRNL | unix.IXON | unix.IXOFF | unix.IXANY
	t.Oflag &^= unix.OPOST
	t.Lflag &^= unix.ECHO | unix.ECHONL | unix.ICANON | unix.ISIG | unix.IEXTEN
	t.Cflag &^= unix.CSIZE | unix.PARENB | unix.CSTOPB | unix.CRTSCTS

	// 8 data bits, enable receiver, local mode (no modem control).
	//
	// Flow control stays OFF deliberately: qemu-vdagent drives no modem lines,
	// so CRTSCTS would stall forever, and IXON/IXOFF would corrupt binary data.
	// Unlike Windows there is no SetupComm equivalent — the line-discipline
	// buffer (N_TTY_BUF_SIZE) is a kernel compile-time constant — so on Linux
	// DRAIN RATE is the only lever, which is exactly what the agent's dedicated
	// drain goroutine provides. TIOCGICOUNT below makes any residual loss loud.
	t.Cflag |= unix.CS8 | unix.CREAD | unix.CLOCAL

	// 115200 baud.
	t.Ispeed = unix.B115200
	t.Ospeed = unix.B115200

	// VMIN=0, VTIME=1 → read returns after 100ms with whatever bytes
	// are available (or immediately if at least 1 byte arrived).
	// Matches the Windows read-timeout behaviour.
	t.Cc[unix.VMIN] = 0
	t.Cc[unix.VTIME] = 1

	if err := unix.IoctlSetTermios(fd, unix.TCSETS, t); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("TCSETS: %w", err)
	}

	s := &SerialPort{f: f}
	if n, ok := s.readOverrunCounter(); ok {
		s.baseline, s.haveBase = n, true
	}
	return s, nil
}

// RxCapacity reports how large a host→guest burst this guest can absorb. On
// Linux the kernel's line-discipline buffer is a fixed 4096 and cannot be
// resized, so the real capacity comes from streaming: the drain goroutine
// empties the tty continuously into a userspace ring, and the ring is what
// bounds a burst. Reported as a claim — the host verifies it with CLIPACK
// before trusting it, and any residual kernel-side loss shows up in Overruns.
func (s *SerialPort) RxCapacity() int { return drainRingBytes }

// Overruns reports receive drops observed since open (UART FIFO overrun +
// line-discipline buffer overflow), as a delta from the baseline at open.
// Non-zero means the kernel threw inbound bytes away.
func (s *SerialPort) Overruns() uint64 {
	s.pollOverrun()
	return s.overruns.Load()
}

func (s *SerialPort) readOverrunCounter() (int32, bool) {
	var ic serialICounter
	_, _, errno := unix.Syscall(
		unix.SYS_IOCTL,
		s.f.Fd(),
		uintptr(unix.TIOCGICOUNT),
		uintptr(unsafe.Pointer(&ic)),
	)
	if errno != 0 {
		return 0, false // not all backends implement TIOCGICOUNT
	}
	return ic.Overrun + ic.BufOverrun, true
}

func (s *SerialPort) pollOverrun() {
	n, ok := s.readOverrunCounter()
	if !ok || !s.haveBase {
		return
	}
	if d := n - s.baseline; d > 0 {
		s.overruns.Store(uint64(d))
	}
}

func (s *SerialPort) Read(buf []byte) (int, error) {
	n, err := s.f.Read(buf)
	if n > 0 {
		s.pollOverrun()
	}
	return n, err
}

func (s *SerialPort) Write(buf []byte) (int, error) { return s.f.Write(buf) }
func (s *SerialPort) Close() error                  { return s.f.Close() }

// translatePort converts a Windows COM name to a Linux tty path.
// "COM3" → "/dev/ttyS2" (COM{N} → /dev/ttyS{N-1}).
// Paths starting with "/" are returned unchanged.
func translatePort(name string) string {
	if strings.HasPrefix(name, "/") {
		return name
	}
	upper := strings.ToUpper(name)
	if strings.HasPrefix(upper, "COM") {
		if n, err := strconv.Atoi(upper[3:]); err == nil && n >= 1 {
			return fmt.Sprintf("/dev/ttyS%d", n-1)
		}
	}
	return "/dev/" + name
}
