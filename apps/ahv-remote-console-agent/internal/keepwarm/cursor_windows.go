//go:build windows

// cursor_windows.go is the Win32 guest cursor takeover op: while a keep-warm
// lease is live (= a console is attached) every standard system cursor is
// replaced with an invisible cursor, so the guest composites no pointer into
// the framebuffer and the operator's instant local OS arrow is the only
// pointer. (AHV's std VGA has no hardware cursor, so the guest-drawn pointer
// otherwise trails the local arrow by a VNC round-trip — the dominant
// "VNC feel" artifact.) Close restores the user's own registry cursor scheme
// via SystemParametersInfo(SPI_SETCURSORS).
//
// Per-user-session only, like the daemon itself: SetSystemCursor changes the
// calling session's cursors, never the machine's.
//
// Same plain-syscall idiom as op_windows.go: lazy user32/kernel32 procs, no
// cgo (the agent cross-compiles with CGO_ENABLED=0).

package keepwarm

import (
	"errors"
	"syscall"
	"unsafe"
)

const (
	// The standard OCR_* system cursor ids the takeover replaces.
	ocrNormal      = 32512
	ocrIBeam       = 32513
	ocrWait        = 32514
	ocrCross       = 32515
	ocrUp          = 32516
	ocrSizeNWSE    = 32642
	ocrSizeNESW    = 32643
	ocrSizeWE      = 32644
	ocrSizeNS      = 32645
	ocrSizeAll     = 32646
	ocrNo          = 32648
	ocrHand        = 32649
	ocrAppStarting = 32650

	spiSetCursors  = 0x0057
	spifSendChange = 0x0002

	// Classic 32x32 monochrome cursor: each plane is 32*32/8 bytes.
	cursorSide       = 32
	cursorPlaneBytes = cursorSide * cursorSide / 8
)

var systemCursorIDs = [...]uintptr{
	ocrNormal, ocrIBeam, ocrWait, ocrCross, ocrUp,
	ocrSizeNWSE, ocrSizeNESW, ocrSizeWE, ocrSizeNS, ocrSizeAll,
	ocrNo, ocrHand, ocrAppStarting,
}

var (
	modKernel32 = syscall.NewLazyDLL("kernel32.dll")

	procGetModuleHandleW      = modKernel32.NewProc("GetModuleHandleW")
	procCreateCursor          = modUser32.NewProc("CreateCursor")
	procCopyIcon              = modUser32.NewProc("CopyIcon")
	procSetSystemCursor       = modUser32.NewProc("SetSystemCursor")
	procDestroyCursor         = modUser32.NewProc("DestroyCursor")
	procSystemParametersInfoW = modUser32.NewProc("SystemParametersInfoW")
)

// cursorOp marks an active takeover. All real state lives in the session
// (the replaced system cursors); the hide is level-triggered at creation and
// holds until Close restores the registry scheme, so Tick is a no-op. (No
// per-tick re-assert: re-replacing 13 system cursors 4x/s would fight any
// pointer-scheme change the user makes mid-lease; the next lease cycle
// re-applies the hide.)
type cursorOp struct{}

// newPlatformCursorOp builds the classic invisible cursor — 32x32, AND mask
// all 1s, XOR mask all 0s, i.e. "keep the screen pixel" everywhere — and
// installs it as every standard system cursor. SetSystemCursor DESTROYS the
// handle it is given, so each id gets a fresh CopyIcon of the template, and
// the template itself is destroyed before returning.
func newPlatformCursorOp() (op, error) {
	hinst, _, _ := procGetModuleHandleW.Call(0)
	var andPlane, xorPlane [cursorPlaneBytes]byte
	for i := range andPlane {
		andPlane[i] = 0xFF
	}
	tmpl, _, _ := procCreateCursor.Call(
		hinst, 0, 0, cursorSide, cursorSide,
		uintptr(unsafe.Pointer(&andPlane[0])), uintptr(unsafe.Pointer(&xorPlane[0])),
	)
	if tmpl == 0 {
		return nil, errors.New("keepwarm: CreateCursor failed")
	}
	hidden := 0
	for _, id := range systemCursorIDs {
		cp, _, _ := procCopyIcon.Call(tmpl)
		if cp == 0 {
			continue
		}
		if ret, _, _ := procSetSystemCursor.Call(cp, id); ret == 0 {
			// On failure the system did not take ownership of the copy.
			_, _, _ = procDestroyCursor.Call(cp)
			continue
		}
		hidden++
	}
	_, _, _ = procDestroyCursor.Call(tmpl)
	if hidden == 0 {
		// Nothing was replaced, so there is nothing to restore either.
		return nil, errors.New("keepwarm: SetSystemCursor failed for every cursor id")
	}
	return &cursorOp{}, nil
}

// Tick is a no-op — see the cursorOp doc.
func (c *cursorOp) Tick() error { return nil }

// Close restores the user's registry cursor scheme.
func (c *cursorOp) Close() {
	restoreDefaultCursors()
}

// restoreDefaultCursors reloads the per-user registry cursor scheme
// (SPI_SETCURSORS, with SPIF_SENDCHANGE so running apps refresh their
// cursors immediately), undoing any takeover. Harmless when the cursors are
// already the user's own — which is why New calls it unconditionally at
// daemon startup to heal a hide left behind by a daemon that died mid-lease.
func restoreDefaultCursors() {
	_, _, _ = procSystemParametersInfoW.Call(spiSetCursors, 0, 0, spifSendChange)
}
