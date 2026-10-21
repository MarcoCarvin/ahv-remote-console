//go:build windows

// op_windows.go is the Win32 dirty op: a 1x1 layered window in the screen's
// far (bottom-right) corner whose single pixel toggles its color by 1 LSB on
// every tick. A REAL pixel op is required — power/timer tricks like
// SetThreadExecutionState do not change the framebuffer, so QEMU's dirty-page
// tracking never sees them and vnc_refresh still backs off. The window is
// WS_EX_LAYERED|WS_EX_TRANSPARENT|WS_EX_NOACTIVATE|WS_EX_TOOLWINDOW and
// topmost: it never takes focus, never hit-tests, never appears in the
// taskbar or Alt-Tab, and the 1-LSB toggle is invisible in practice.
//
// Same plain-syscall idiom as display_windows.go: lazy user32/gdi32 procs, no
// cgo (the agent cross-compiles with CGO_ENABLED=0).

package keepwarm

import (
	"errors"
	"syscall"
	"unsafe"
)

const (
	wsExTopmost     = 0x00000008
	wsExTransparent = 0x00000020
	wsExToolWindow  = 0x00000080
	wsExLayered     = 0x00080000
	wsExNoActivate  = 0x08000000

	wsPopup   = 0x80000000
	wsVisible = 0x10000000

	lwaAlpha = 0x00000002

	smCXScreen = 0
	smCYScreen = 1

	pmRemove = 0x0001
)

var (
	modUser32 = syscall.NewLazyDLL("user32.dll")
	modGdi32  = syscall.NewLazyDLL("gdi32.dll")

	procCreateWindowExW            = modUser32.NewProc("CreateWindowExW")
	procDestroyWindow              = modUser32.NewProc("DestroyWindow")
	procSetLayeredWindowAttributes = modUser32.NewProc("SetLayeredWindowAttributes")
	procGetDC                      = modUser32.NewProc("GetDC")
	procReleaseDC                  = modUser32.NewProc("ReleaseDC")
	procGetSystemMetrics           = modUser32.NewProc("GetSystemMetrics")
	procPeekMessageW               = modUser32.NewProc("PeekMessageW")
	procTranslateMessage           = modUser32.NewProc("TranslateMessage")
	procDispatchMessageW           = modUser32.NewProc("DispatchMessageW")
	procSetPixelV                  = modGdi32.NewProc("SetPixelV")
)

// msgW mirrors Win32 MSG (hwnd, message, wParam, lParam, time, POINT pt).
type msgW struct {
	Hwnd    uintptr
	Message uint32
	WParam  uintptr
	LParam  uintptr
	Time    uint32
	PtX     int32
	PtY     int32
}

// winOp owns the 1x1 layered window. Created and driven on the lease loop's
// locked OS thread (Win32 windows are thread-affine).
type winOp struct {
	hwnd  uintptr
	color uintptr // COLORREF 0x00BBGGRR; LSB = red channel
}

// newPlatformOp creates the 1x1 layered window. Uses the predefined STATIC
// window class so no class registration / wndproc is needed; the window never
// receives input (WS_EX_TRANSPARENT + WS_EX_NOACTIVATE).
func newPlatformOp() (op, error) {
	cx, _, _ := procGetSystemMetrics.Call(smCXScreen)
	cy, _, _ := procGetSystemMetrics.Call(smCYScreen)
	x := uintptr(0)
	y := uintptr(0)
	if int32(cx) > 0 && int32(cy) > 0 {
		x = uintptr(int32(cx) - 1)
		y = uintptr(int32(cy) - 1)
	}

	className, err := syscall.UTF16PtrFromString("STATIC")
	if err != nil {
		return nil, err
	}
	hwnd, _, _ := procCreateWindowExW.Call(
		wsExLayered|wsExTransparent|wsExNoActivate|wsExToolWindow|wsExTopmost,
		uintptr(unsafe.Pointer(className)),
		0, // no window name
		wsPopup|wsVisible,
		x, y, 1, 1, // 1x1 in the far corner
		0, 0, 0, 0, // no parent / menu / instance / param
	)
	if hwnd == 0 {
		return nil, errors.New("keepwarm: CreateWindowExW failed")
	}
	// Layered "attributes" mode: the window paints via plain GDI and the
	// compositor applies the (fully opaque) alpha — required for a layered
	// window's surface to be displayed at all.
	ret, _, _ := procSetLayeredWindowAttributes.Call(hwnd, 0, 255, lwaAlpha)
	if ret == 0 {
		_, _, _ = procDestroyWindow.Call(hwnd)
		return nil, errors.New("keepwarm: SetLayeredWindowAttributes failed")
	}
	w := &winOp{hwnd: hwnd}
	w.pump()
	return w, nil
}

// pump drains the thread's message queue (window creation and DWM chatter
// post a few messages even to an input-transparent window) so the queue can
// never fill across a long-lived lease.
func (w *winOp) pump() {
	var m msgW
	for {
		ret, _, _ := procPeekMessageW.Call(uintptr(unsafe.Pointer(&m)), w.hwnd, 0, 0, pmRemove)
		if ret == 0 {
			return
		}
		_, _, _ = procTranslateMessage.Call(uintptr(unsafe.Pointer(&m)))
		_, _, _ = procDispatchMessageW.Call(uintptr(unsafe.Pointer(&m)))
	}
}

// Tick toggles the window's single pixel by 1 LSB — the minimal real
// framebuffer change that keeps QEMU's dirty tracking (and so vnc_refresh)
// warm.
func (w *winOp) Tick() error {
	w.pump()
	w.color ^= 1
	dc, _, _ := procGetDC.Call(w.hwnd)
	if dc == 0 {
		return errors.New("keepwarm: GetDC failed")
	}
	defer func() { _, _, _ = procReleaseDC.Call(w.hwnd, dc) }()
	if ret, _, _ := procSetPixelV.Call(dc, 0, 0, w.color); ret == 0 {
		return errors.New("keepwarm: SetPixelV failed")
	}
	return nil
}

// Close destroys the window and drains its final messages.
func (w *winOp) Close() {
	_, _, _ = procDestroyWindow.Call(w.hwnd)
	w.pump()
	w.hwnd = 0
}
