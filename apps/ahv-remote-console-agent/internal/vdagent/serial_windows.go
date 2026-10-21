//go:build windows

package vdagent

import (
	"fmt"
	"io"
	"sync/atomic"
	"syscall"
	"unsafe"
)

var (
	kernel32              = syscall.NewLazyDLL("kernel32.dll")
	procCreateFileW       = kernel32.NewProc("CreateFileW")
	procSetCommState      = kernel32.NewProc("SetCommState")
	procSetCommTimeouts   = kernel32.NewProc("SetCommTimeouts")
	procSetupComm         = kernel32.NewProc("SetupComm")
	procGetCommProperties = kernel32.NewProc("GetCommProperties")
	procClearCommError    = kernel32.NewProc("ClearCommError")
)

type dcb struct {
	DCBlength  uint32
	BaudRate   uint32
	Flags      uint32
	wReserved  uint16
	XonLim     uint16
	XoffLim    uint16
	ByteSize   byte
	Parity     byte
	StopBits   byte
	XonChar    byte
	XoffChar   byte
	ErrorChar  byte
	EofChar    byte
	EvtChar    byte
	wReserved1 uint16
}

type commTimeouts struct {
	ReadIntervalTimeout         uint32
	ReadTotalTimeoutMultiplier  uint32
	ReadTotalTimeoutConstant    uint32
	WriteTotalTimeoutMultiplier uint32
	WriteTotalTimeoutConstant   uint32
}

// commProp is Win32 COMMPROP. We only read dwCurrentRxQueue, but the layout
// must match exactly for the driver to fill it.
type commProp struct {
	wPacketLength       uint16
	wPacketVersion      uint16
	dwServiceMask       uint32
	dwReserved1         uint32
	dwMaxTxQueue        uint32
	dwMaxRxQueue        uint32
	dwMaxBaud           uint32
	dwProvSubType       uint32
	dwProvCapabilities  uint32
	dwSettableParams    uint32
	dwSettableBaud      uint32
	wSettableData       uint16
	wSettableStopParity uint16
	dwCurrentTxQueue    uint32
	dwCurrentRxQueue    uint32
	dwProvSpec1         uint32
	dwProvSpec2         uint32
	wcProvChar          [1]uint16
}

// comStat is Win32 COMSTAT, filled by ClearCommError alongside the error mask.
type comStat struct {
	Flags    uint32 // packed bitfields; unused
	cbInQue  uint32
	cbOutQue uint32
}

// Win32 comm error flags (winbase.h).
const (
	ceRxOver  = 0x0001 // receive QUEUE overflow — the driver dropped bytes
	ceOverrun = 0x0002 // UART character-buffer overrun
)

// rxQueueLadder is tried in order: ask for a big receive queue, settle for what
// the driver will grant. The Windows default is ~4 KB, which SILENTLY drops
// every byte of a larger burst — the measured host→guest ~4 KB wall.
var rxQueueLadder = []uint32{1 << 20, 256 << 10, 64 << 10}

// SerialPort wraps a Windows COM port handle as an io.ReadWriteCloser.
type SerialPort struct {
	handle     syscall.Handle
	rxCapacity int
	overruns   atomic.Uint64
}

// RxCapacity reports the receive-queue size the driver actually GRANTED (read
// back via GetCommProperties, never what we requested — a driver may silently
// clamp SetupComm, and advertising an ungranted number is how a guest promises
// a ceiling it will then drop bytes past).
func (s *SerialPort) RxCapacity() int { return s.rxCapacity }

// Overruns counts receive overflows observed since open. Non-zero means the
// driver dropped inbound bytes — data loss, always worth surfacing.
func (s *SerialPort) Overruns() uint64 { return s.overruns.Load() }

// OpenSerial opens a COM port (e.g., "COM3") configured for raw binary
// communication, with the receive queue enlarged as far as the driver allows.
func OpenSerial(name string) (io.ReadWriteCloser, error) {
	path := `\\.\` + name
	pathW, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}

	h, _, callErr := procCreateFileW.Call(
		uintptr(unsafe.Pointer(pathW)),
		syscall.GENERIC_READ|syscall.GENERIC_WRITE,
		0, // exclusive access — no other process can open this port
		0, // no security attrs
		syscall.OPEN_EXISTING,
		0, // no overlapped
		0, // no template
	)
	if h == uintptr(syscall.InvalidHandle) {
		return nil, fmt.Errorf("open %s: %w", name, callErr)
	}
	handle := syscall.Handle(h)

	// SetupComm BEFORE SetCommState: the queue must be sized first, then the
	// line configured on the resized queue (not the reverse). Ladder down until
	// the driver accepts one.
	requested := uint32(0)
	for _, want := range rxQueueLadder {
		if ret, _, _ := procSetupComm.Call(uintptr(handle), uintptr(want), uintptr(64<<10)); ret != 0 {
			requested = want
			break
		}
	}

	// Configure for raw binary: 115200 8N1, no flow control. Flow control stays
	// OFF deliberately — qemu-vdagent drives no modem lines, so RTS/CTS would
	// stall forever. Capacity + drain rate are the only levers.
	d := dcb{
		DCBlength: uint32(unsafe.Sizeof(dcb{})),
		BaudRate:  115200,
		ByteSize:  8,
		Parity:    0,    // NOPARITY
		StopBits:  0,    // ONESTOPBIT
		Flags:     0x01, // fBinary
	}
	ret, _, callErr := procSetCommState.Call(uintptr(handle), uintptr(unsafe.Pointer(&d)))
	if ret == 0 {
		_ = syscall.CloseHandle(handle)
		return nil, fmt.Errorf("SetCommState: %w", callErr)
	}

	// Set read timeouts: block up to 100ms per read.
	timeouts := commTimeouts{
		ReadIntervalTimeout:        50,
		ReadTotalTimeoutMultiplier: 10,
		ReadTotalTimeoutConstant:   100,
		WriteTotalTimeoutConstant:  1000,
	}
	ret, _, callErr = procSetCommTimeouts.Call(uintptr(handle), uintptr(unsafe.Pointer(&timeouts)))
	if ret == 0 {
		_ = syscall.CloseHandle(handle)
		return nil, fmt.Errorf("SetCommTimeouts: %w", callErr)
	}

	return &SerialPort{handle: handle, rxCapacity: grantedRxQueue(handle, requested)}, nil
}

// grantedRxQueue reads back the live receive-queue size. Falls back to the
// requested value, then to the conservative default, so a driver that refuses
// GetCommProperties never inflates what we advertise.
func grantedRxQueue(handle syscall.Handle, requested uint32) int {
	var props commProp
	props.wPacketLength = uint16(unsafe.Sizeof(props))
	if ret, _, _ := procGetCommProperties.Call(uintptr(handle), uintptr(unsafe.Pointer(&props))); ret != 0 {
		if props.dwCurrentRxQueue > 0 {
			return int(props.dwCurrentRxQueue)
		}
		// Some drivers report 0 to mean "no fixed limit"; trust our request then.
	}
	if requested > 0 {
		return int(requested)
	}
	return defaultRxCapacity
}

func (s *SerialPort) Read(buf []byte) (int, error) {
	var n uint32
	err := syscall.ReadFile(s.handle, buf, &n, nil)
	// Without ClearCommError a dropped burst is COMPLETELY invisible — which is
	// exactly how the host→guest wall went unseen for months. Check every read.
	s.checkOverrun()
	if err != nil {
		return int(n), err
	}
	return int(n), nil
}

// checkOverrun clears and records the driver's error mask. ClearCommError also
// re-arms the port after an overrun, so calling it is required for recovery,
// not just for reporting.
func (s *SerialPort) checkOverrun() {
	var errs uint32
	var st comStat
	ret, _, _ := procClearCommError.Call(
		uintptr(s.handle),
		uintptr(unsafe.Pointer(&errs)),
		uintptr(unsafe.Pointer(&st)),
	)
	if ret == 0 {
		return
	}
	if errs&(ceRxOver|ceOverrun) != 0 {
		s.overruns.Add(1)
	}
}

func (s *SerialPort) Write(buf []byte) (int, error) {
	var n uint32
	err := syscall.WriteFile(s.handle, buf, &n, nil)
	if err != nil {
		return int(n), err
	}
	return int(n), nil
}

func (s *SerialPort) Close() error {
	return syscall.CloseHandle(s.handle)
}
