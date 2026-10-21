//go:build windows

// uplinkprobe — 5-variant uplink viability experiment.
//
// Background: as of 2026-05-24 the bridge's existing pixel-strip
// overlay uses WS_EX_LAYERED + UpdateLayeredWindow. Field evidence
// shows the renderer receives ZERO `[uplink.rx]` log lines over
// 90 seconds of bridge running — meaning QEMU's framebuffer-
// capture path is NOT seeing the bridge's overlay paint reach the
// guest's screen. Modern Windows compositors (DWM with hardware
// acceleration, MPO, DirectComposition) are known to bypass
// GDI-level screen-capture for some layered-window paths, so the
// hypothesis is that we're rendering into a surface QEMU's VNC
// server doesn't see.
//
// Before redesigning Plane 3 of the convergence architecture
// (docs/clipboard-convergence.md), we need to KNOW which Win32
// painting strategy QEMU actually captures. This probe answers
// that question deterministically by painting the bottom 32-pixel
// strip in five different ways, each for 10 seconds, each in a
// distinctive solid color. The operator watches the VM's display
// in the renderer and reports which colors appeared.
//
// Variant → color guide (also logged at probe start):
//
//   A  plain GDI window  (WS_POPUP | WS_EX_TOPMOST)            → RED
//   B  layered window    (current; WS_EX_LAYERED + Update…)    → GREEN
//   C  child of desktop  (parent = HWND_DESKTOP)                → BLUE
//   D  GDI + DWM excluded (DWMWA_EXCLUDED_FROM_PEEK = TRUE)     → YELLOW
//   E  direct screen DC  (no window; GetDC(NULL) + FillRect)    → MAGENTA
//
// Operator procedure:
//   1. Mount the ISO containing the new bridge binary.
//   2. Inside the guest, open a command prompt.
//   3. Run:  hcbridge.exe --uplink-probe
//   4. Watch the bottom of the VM's screen for ~50 seconds.
//   5. Note which colors appear. Order: RED → GREEN → BLUE →
//      YELLOW → MAGENTA, ~10 s each.
//   6. Report back. Each color that DOES NOT appear means the
//      corresponding Win32 variant is invisible to QEMU's capture.
//
// Decision rule:
//   * First color that appears cleanly → that variant becomes the
//     Plane 3 primitive. Replace the existing overlay.
//   * Variant E (MAGENTA) is the strongest fallback because it
//     paints directly to the screen DC; if even E doesn't appear,
//     QEMU isn't capturing the primary monitor's screen at all and
//     we have a fundamentally different problem.
//   * No color appears → Plane 3 has to move off the framebuffer
//     entirely (second rotating ISO pool for guest→host).
//
// The probe is intentionally minimalist: no diagnostics piped over
// the convergence channel (which itself isn't working yet), no
// integration with the renderer's RFB stream. The operator's eyes
// are the measurement instrument.

package uplinkprobe

import (
	"context"
	"fmt"
	"log/slog"
	"runtime"
	"syscall"
	"time"
	"unsafe"
)

// Wire constants — duplicated from internal/uplink/render_windows.go
// rather than imported so the probe can stand alone if we ever
// salvage it into a separate hcprobe.exe.
const (
	stripHeight             = 32
	variantDurationSeconds  = 10
	repaintIntervalMS       = 100

	// Colors are 0x00BBGGRR (Windows COLORREF). The mnemonic for
	// the operator is just "red green blue yellow magenta" in that
	// fixed order; the byte layout doesn't need to be memorable.
	colorVariantA uint32 = 0x000000FF // RED
	colorVariantB uint32 = 0x0000FF00 // GREEN
	colorVariantC uint32 = 0x00FF0000 // BLUE
	colorVariantD uint32 = 0x0000FFFF // YELLOW
	colorVariantE uint32 = 0x00FF00FF // MAGENTA

	// Window classes / styles
	wsPopup        uintptr = 0x80000000
	wsChild        uintptr = 0x40000000
	wsVisible      uintptr = 0x10000000

	wsExTopmost    uintptr = 0x00000008
	wsExLayered    uintptr = 0x00080000
	wsExTransparent uintptr = 0x00000020
	wsExToolwindow uintptr = 0x00000080

	swpNoActivate  uintptr = 0x0010
	swpShowWindow  uintptr = 0x0040

	hwndTopmost   uintptr = ^uintptr(0)         // (HWND)-1
	hwndDesktopP  uintptr = 0                   // CreateWindow parent=0 → desktop
	hwndMessage   uintptr = ^uintptr(2)         // (HWND)-3 — message-only

	swShow         uintptr = 5

	smCXScreen = 0
	smCYScreen = 1

	// DWM attributes
	dwmwaExcludedFromPeek = 12

	// Layered window
	ulwColorKey = 0x00000001
	ulwAlpha    = 0x00000002
	ulwOpaque   = 0x00000004

	wmPaint   uint32 = 0x000F
	wmDestroy uint32 = 0x0002
	wmTimer   uint32 = 0x0113
)

var (
	modUser32 = syscall.NewLazyDLL("user32.dll")
	modGDI32  = syscall.NewLazyDLL("gdi32.dll")
	modDWMApi = syscall.NewLazyDLL("dwmapi.dll")
	modKernel = syscall.NewLazyDLL("kernel32.dll")

	procGetSystemMetrics    = modUser32.NewProc("GetSystemMetrics")
	procRegisterClassExW    = modUser32.NewProc("RegisterClassExW")
	procUnregisterClassW    = modUser32.NewProc("UnregisterClassW")
	procCreateWindowExW     = modUser32.NewProc("CreateWindowExW")
	procDestroyWindow       = modUser32.NewProc("DestroyWindow")
	procDefWindowProcW      = modUser32.NewProc("DefWindowProcW")
	procShowWindow          = modUser32.NewProc("ShowWindow")
	procSetWindowPos        = modUser32.NewProc("SetWindowPos")
	procGetDC               = modUser32.NewProc("GetDC")
	procReleaseDC           = modUser32.NewProc("ReleaseDC")
	procBeginPaint          = modUser32.NewProc("BeginPaint")
	procEndPaint            = modUser32.NewProc("EndPaint")
	procFillRect            = modUser32.NewProc("FillRect")
	procInvalidateRect      = modUser32.NewProc("InvalidateRect")
	procGetMessageW         = modUser32.NewProc("GetMessageW")
	procTranslateMessage    = modUser32.NewProc("TranslateMessage")
	procDispatchMessageW    = modUser32.NewProc("DispatchMessageW")
	procPostQuitMessage     = modUser32.NewProc("PostQuitMessage")
	procSetTimer            = modUser32.NewProc("SetTimer")
	procKillTimer           = modUser32.NewProc("KillTimer")
	procUpdateLayeredWindow = modUser32.NewProc("UpdateLayeredWindow")

	procCreateSolidBrush  = modGDI32.NewProc("CreateSolidBrush")
	procDeleteObject      = modGDI32.NewProc("DeleteObject")
	procCreateCompatibleDC = modGDI32.NewProc("CreateCompatibleDC")
	procCreateDIBSection  = modGDI32.NewProc("CreateDIBSection")
	procSelectObject      = modGDI32.NewProc("SelectObject")
	procDeleteDC          = modGDI32.NewProc("DeleteDC")
	procBitBlt            = modGDI32.NewProc("BitBlt")

	procDwmSetWindowAttribute = modDWMApi.NewProc("DwmSetWindowAttribute")
	procGetModuleHandleW      = modKernel.NewProc("GetModuleHandleW")

	procPaintLog *slog.Logger
)

// rect / paintStruct / WNDCLASSEX / etc. — minimal struct
// declarations matching Win32 ABI on x64.

type rect struct {
	Left, Top, Right, Bottom int32
}

type paintStruct struct {
	HDC         uintptr
	FErase      int32
	RcPaint     rect
	FRestore    int32
	FIncUpdate  int32
	RgbReserved [32]byte
}

type wndclassexW struct {
	CbSize        uint32
	Style         uint32
	LpfnWndProc   uintptr
	CbClsExtra    int32
	CbWndExtra    int32
	HInstance     uintptr
	HIcon         uintptr
	HCursor       uintptr
	HbrBackground uintptr
	LpszMenuName  *uint16
	LpszClassName *uint16
	HIconSm       uintptr
}

type msgStruct struct {
	Hwnd    uintptr
	Message uint32
	WParam  uintptr
	LParam  uintptr
	Time    uint32
	Pt      [2]int32
}

type bitmapInfoHeader struct {
	BiSize          uint32
	BiWidth         int32
	BiHeight        int32
	BiPlanes        uint16
	BiBitCount      uint16
	BiCompression   uint32
	BiSizeImage     uint32
	BiXPelsPerMeter int32
	BiYPelsPerMeter int32
	BiClrUsed       uint32
	BiClrImportant  uint32
}

type bitmapInfo struct {
	BmiHeader bitmapInfoHeader
	BmiColors [4]byte // dummy palette space
}

type pointStruct struct {
	X, Y int32
}

type sizeStruct struct {
	Cx, Cy int32
}

type blendFunction struct {
	BlendOp             byte
	BlendFlags          byte
	SourceConstantAlpha byte
	AlphaFormat         byte
}

// Run executes the 5-variant probe. Each variant gets
// variantDurationSeconds (10s) to paint its strip. The function
// returns when all variants have finished or ctx is cancelled.
func Run(ctx context.Context, log *slog.Logger) error {
	if log == nil {
		log = slog.Default()
	}
	procPaintLog = log

	log.Info("uplinkprobe: starting 5-variant viability experiment")
	log.Info("uplinkprobe: variant→color guide",
		"A_plain_GDI", "RED",
		"B_layered", "GREEN",
		"C_child_of_desktop", "BLUE",
		"D_GDI_dwm_excluded", "YELLOW",
		"E_direct_screen_DC", "MAGENTA",
	)
	log.Info("uplinkprobe: each variant runs 10 seconds; total ~50 seconds")

	variants := []struct {
		name  string
		color uint32
		fn    func(context.Context, uint32) error
	}{
		{"A_plain_GDI", colorVariantA, runVariantPlainGDI},
		{"B_layered", colorVariantB, runVariantLayered},
		{"C_child_of_desktop", colorVariantC, runVariantChildOfDesktop},
		{"D_GDI_dwm_excluded", colorVariantD, runVariantDWMExcluded},
		{"E_direct_screen_DC", colorVariantE, runVariantScreenDC},
	}

	for _, v := range variants {
		if err := ctx.Err(); err != nil {
			return err
		}
		log.Info("uplinkprobe: variant START", "name", v.name)
		vctx, cancel := context.WithTimeout(ctx, variantDurationSeconds*time.Second)
		err := v.fn(vctx, v.color)
		cancel()
		if err != nil {
			log.Warn("uplinkprobe: variant returned with error", "name", v.name, "err", err)
		} else {
			log.Info("uplinkprobe: variant END", "name", v.name)
		}
	}

	log.Info("uplinkprobe: experiment complete — operator reports which colors appeared")
	return nil
}

func primaryScreenMetrics() (int32, int32) {
	w, _, _ := procGetSystemMetrics.Call(uintptr(smCXScreen))
	h, _, _ := procGetSystemMetrics.Call(uintptr(smCYScreen))
	if w == 0 || h == 0 {
		return 1920, 1080
	}
	return int32(w), int32(h)
}

// ───────────────────────── Variant A: plain GDI ────────────────────────

func runVariantPlainGDI(ctx context.Context, color uint32) error {
	return runWindowedVariant(ctx, color, windowedVariantOpts{
		className: "HCProbeA",
		exStyle:   wsExTopmost,
		style:     wsPopup | wsVisible,
		parent:    hwndDesktopP,
	})
}

// ───────────────────────── Variant B: layered (current overlay) ────────

func runVariantLayered(ctx context.Context, color uint32) error {
	return runLayeredVariant(ctx, color)
}

// ───────────────────────── Variant C: child of desktop ─────────────────

func runVariantChildOfDesktop(ctx context.Context, color uint32) error {
	// True "child of desktop" requires HWND_DESKTOP as parent. On
	// modern Win32 this is unusual but technically allowed; the
	// window inherits the desktop's compositing path which may
	// expose it to GDI-level capture differently from a popup.
	return runWindowedVariant(ctx, color, windowedVariantOpts{
		className: "HCProbeC",
		exStyle:   0, // no extended styles
		style:     wsChild | wsVisible,
		parent:    hwndDesktopP,
	})
}

// ───────────────────────── Variant D: GDI + DWMWA_EXCLUDED_FROM_PEEK ───

func runVariantDWMExcluded(ctx context.Context, color uint32) error {
	return runWindowedVariant(ctx, color, windowedVariantOpts{
		className:  "HCProbeD",
		exStyle:    wsExTopmost,
		style:      wsPopup | wsVisible,
		parent:     hwndDesktopP,
		dwmExclude: true,
	})
}

// ───────────────────────── Variant E: direct screen DC ─────────────────

func runVariantScreenDC(ctx context.Context, color uint32) error {
	screenW, screenH := primaryScreenMetrics()
	x := int32(0)
	y := screenH - stripHeight
	w := screenW
	h := int32(stripHeight)

	hDC, _, _ := procGetDC.Call(0)
	if hDC == 0 {
		return fmt.Errorf("uplinkprobe: GetDC(NULL) failed")
	}
	defer procReleaseDC.Call(0, hDC)

	brush, _, _ := procCreateSolidBrush.Call(uintptr(color))
	if brush == 0 {
		return fmt.Errorf("uplinkprobe: CreateSolidBrush failed")
	}
	defer procDeleteObject.Call(brush)

	r := rect{Left: x, Top: y, Right: x + w, Bottom: y + h}

	// Paint repeatedly to combat compositor overdraw. The screen DC
	// has no WM_PAINT to retrigger us, so we run a tight repaint
	// loop until ctx times out.
	ticker := time.NewTicker(repaintIntervalMS * time.Millisecond)
	defer ticker.Stop()

	// Initial paint immediately.
	procFillRect.Call(hDC, uintptr(unsafe.Pointer(&r)), brush)

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			procFillRect.Call(hDC, uintptr(unsafe.Pointer(&r)), brush)
		}
	}
}

// ─────────────────── shared windowed-variant infrastructure ────────────

type windowedVariantOpts struct {
	className  string
	exStyle    uintptr
	style      uintptr
	parent     uintptr
	dwmExclude bool
}

// runWindowedVariant covers variants A, C, D. Each owns its own
// OS thread (Win32 windows are bound to their creating thread),
// runs a tiny message pump for variantDurationSeconds, paints the
// strip on WM_PAINT, and tears down cleanly.
func runWindowedVariant(ctx context.Context, color uint32, opts windowedVariantOpts) error {
	done := make(chan error, 1)
	go func() {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		done <- runWindowedVariantOnThread(ctx, color, opts)
	}()
	return <-done
}

func runWindowedVariantOnThread(ctx context.Context, color uint32, opts windowedVariantOpts) error {
	hInstance, _, _ := procGetModuleHandleW.Call(0)
	className, _ := syscall.UTF16PtrFromString(opts.className)

	brush, _, _ := procCreateSolidBrush.Call(uintptr(color))
	if brush == 0 {
		return fmt.Errorf("uplinkprobe: CreateSolidBrush failed")
	}
	defer procDeleteObject.Call(brush)

	// Capture brush in the wndProc via a package-level — the
	// callback runs on the same thread, so a simple variable is OK.
	stripBrush.Store(brush)

	wc := wndclassexW{
		CbSize:        uint32(unsafe.Sizeof(wndclassexW{})),
		LpfnWndProc:   syscall.NewCallback(probeWndProc),
		HInstance:     hInstance,
		HbrBackground: brush,
		LpszClassName: className,
	}
	atom, _, callErr := procRegisterClassExW.Call(uintptr(unsafe.Pointer(&wc)))
	if atom == 0 {
		return fmt.Errorf("uplinkprobe: RegisterClassExW failed: %v", callErr)
	}
	defer procUnregisterClassW.Call(uintptr(unsafe.Pointer(className)), hInstance)

	screenW, screenH := primaryScreenMetrics()
	x := int32(0)
	y := screenH - stripHeight
	w := screenW
	h := int32(stripHeight)

	hwnd, _, callErr := procCreateWindowExW.Call(
		opts.exStyle,
		uintptr(unsafe.Pointer(className)),
		0, // window name
		opts.style,
		uintptr(x), uintptr(y), uintptr(w), uintptr(h),
		opts.parent,
		0, // menu
		hInstance,
		0,
	)
	if hwnd == 0 {
		return fmt.Errorf("uplinkprobe: CreateWindowExW failed: %v", callErr)
	}
	defer procDestroyWindow.Call(hwnd)

	if opts.dwmExclude {
		var excluded int32 = 1
		ret, _, _ := procDwmSetWindowAttribute.Call(
			hwnd,
			uintptr(dwmwaExcludedFromPeek),
			uintptr(unsafe.Pointer(&excluded)),
			uintptr(unsafe.Sizeof(excluded)),
		)
		if ret != 0 {
			procPaintLog.Warn("uplinkprobe: DwmSetWindowAttribute non-zero return", "ret", ret)
		}
	}

	procSetWindowPos.Call(
		hwnd, hwndTopmost,
		uintptr(x), uintptr(y), uintptr(w), uintptr(h),
		swpNoActivate|swpShowWindow,
	)
	procShowWindow.Call(hwnd, swShow)

	// Repaint timer — every 100ms invalidate so we overpaint any
	// compositor cycle that might draw over us.
	const repaintTimerID = 1
	procSetTimer.Call(hwnd, repaintTimerID, repaintIntervalMS, 0)
	defer procKillTimer.Call(hwnd, repaintTimerID)

	// Goroutine: when ctx is done, post WM_QUIT to break out of
	// the message pump. We can't share state across goroutines
	// here (the hwnd is thread-bound) but PostQuitMessage is safe
	// from any thread.
	go func() {
		<-ctx.Done()
		procPostQuitMessage.Call(0)
	}()

	var m msgStruct
	for {
		ret, _, _ := procGetMessageW.Call(uintptr(unsafe.Pointer(&m)), 0, 0, 0)
		if int32(ret) == 0 || int32(ret) == -1 {
			// WM_QUIT or error
			return nil
		}
		procTranslateMessage.Call(uintptr(unsafe.Pointer(&m)))
		procDispatchMessageW.Call(uintptr(unsafe.Pointer(&m)))
	}
}

// stripBrush is a stash for the WM_PAINT handler to fill the strip
// with the variant's color. A package-level atomic.Value is fine
// because runWindowedVariant runs ONE variant at a time (no
// concurrency).
var stripBrush atomicUintptr

func probeWndProc(hwnd uintptr, msg uint32, wparam, lparam uintptr) uintptr {
	switch msg {
	case wmPaint:
		var ps paintStruct
		hDC, _, _ := procBeginPaint.Call(hwnd, uintptr(unsafe.Pointer(&ps)))
		brush := stripBrush.Load()
		if hDC != 0 && brush != 0 {
			procFillRect.Call(hDC, uintptr(unsafe.Pointer(&ps.RcPaint)), brush)
		}
		procEndPaint.Call(hwnd, uintptr(unsafe.Pointer(&ps)))
		return 0
	case wmTimer:
		procInvalidateRect.Call(hwnd, 0, 0)
		return 0
	case wmDestroy:
		procPostQuitMessage.Call(0)
		return 0
	}
	ret, _, _ := procDefWindowProcW.Call(hwnd, uintptr(msg), wparam, lparam)
	return ret
}

// ─────────────────────── Variant B: layered window ─────────────────────

func runLayeredVariant(ctx context.Context, color uint32) error {
	done := make(chan error, 1)
	go func() {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		done <- runLayeredOnThread(ctx, color)
	}()
	return <-done
}

func runLayeredOnThread(ctx context.Context, color uint32) error {
	hInstance, _, _ := procGetModuleHandleW.Call(0)
	className, _ := syscall.UTF16PtrFromString("HCProbeB")

	wc := wndclassexW{
		CbSize:        uint32(unsafe.Sizeof(wndclassexW{})),
		LpfnWndProc:   syscall.NewCallback(probeWndProcLayered),
		HInstance:     hInstance,
		LpszClassName: className,
	}
	atom, _, callErr := procRegisterClassExW.Call(uintptr(unsafe.Pointer(&wc)))
	if atom == 0 {
		return fmt.Errorf("uplinkprobe: RegisterClassExW (B) failed: %v", callErr)
	}
	defer procUnregisterClassW.Call(uintptr(unsafe.Pointer(className)), hInstance)

	screenW, screenH := primaryScreenMetrics()
	x := int32(0)
	y := screenH - stripHeight
	w := screenW
	h := int32(stripHeight)

	hwnd, _, callErr := procCreateWindowExW.Call(
		wsExTopmost|wsExLayered|wsExTransparent|wsExToolwindow,
		uintptr(unsafe.Pointer(className)),
		0,
		wsPopup,
		uintptr(x), uintptr(y), uintptr(w), uintptr(h),
		0, 0, hInstance, 0,
	)
	if hwnd == 0 {
		return fmt.Errorf("uplinkprobe: CreateWindowExW (B) failed: %v", callErr)
	}
	defer procDestroyWindow.Call(hwnd)

	procSetWindowPos.Call(
		hwnd, hwndTopmost,
		uintptr(x), uintptr(y), uintptr(w), uintptr(h),
		swpNoActivate|swpShowWindow,
	)

	// Paint the layered window via UpdateLayeredWindow with a DIB
	// section. Fill the DIB with the variant color and call UPL.
	memDC, _, _ := procCreateCompatibleDC.Call(0)
	if memDC == 0 {
		return fmt.Errorf("uplinkprobe: CreateCompatibleDC failed")
	}
	defer procDeleteDC.Call(memDC)

	bi := bitmapInfo{
		BmiHeader: bitmapInfoHeader{
			BiSize:        40,
			BiWidth:       w,
			BiHeight:      -h, // top-down
			BiPlanes:      1,
			BiBitCount:    32,
			BiCompression: 0, // BI_RGB
		},
	}
	var pixels unsafe.Pointer
	hbmp, _, _ := procCreateDIBSection.Call(
		memDC,
		uintptr(unsafe.Pointer(&bi)),
		0, // DIB_RGB_COLORS
		uintptr(unsafe.Pointer(&pixels)),
		0, 0,
	)
	if hbmp == 0 || pixels == nil {
		return fmt.Errorf("uplinkprobe: CreateDIBSection failed")
	}
	defer procDeleteObject.Call(hbmp)
	procSelectObject.Call(memDC, hbmp)

	// Fill the DIB with the color. DIB is 32-bit BGRA top-down.
	pixelCount := int(w) * int(h)
	pixelSlice := unsafe.Slice((*uint32)(pixels), pixelCount)
	bgra := ((color & 0xFF) << 16) | (color & 0xFF00) | ((color >> 16) & 0xFF) | 0xFF000000
	for i := range pixelSlice {
		pixelSlice[i] = bgra
	}

	// Call UpdateLayeredWindow.
	srcPos := pointStruct{0, 0}
	dstPos := pointStruct{x, y}
	size := sizeStruct{w, h}
	blend := blendFunction{
		BlendOp:             0,
		BlendFlags:          0,
		SourceConstantAlpha: 255,
		AlphaFormat:         1, // AC_SRC_ALPHA
	}
	ret, _, callErr := procUpdateLayeredWindow.Call(
		hwnd,
		0,
		uintptr(unsafe.Pointer(&dstPos)),
		uintptr(unsafe.Pointer(&size)),
		memDC,
		uintptr(unsafe.Pointer(&srcPos)),
		0,
		uintptr(unsafe.Pointer(&blend)),
		ulwAlpha,
	)
	if ret == 0 {
		procPaintLog.Warn("uplinkprobe: UpdateLayeredWindow returned 0", "err", callErr.Error())
	}

	go func() {
		<-ctx.Done()
		procPostQuitMessage.Call(0)
	}()

	var m msgStruct
	for {
		ret, _, _ := procGetMessageW.Call(uintptr(unsafe.Pointer(&m)), 0, 0, 0)
		if int32(ret) == 0 || int32(ret) == -1 {
			return nil
		}
		procTranslateMessage.Call(uintptr(unsafe.Pointer(&m)))
		procDispatchMessageW.Call(uintptr(unsafe.Pointer(&m)))
	}
}

func probeWndProcLayered(hwnd uintptr, msg uint32, wparam, lparam uintptr) uintptr {
	if msg == wmDestroy {
		procPostQuitMessage.Call(0)
		return 0
	}
	ret, _, _ := procDefWindowProcW.Call(hwnd, uintptr(msg), wparam, lparam)
	return ret
}

// ────────────────────── tiny atomic.Uintptr (Go 1.18-compat) ───────────

// atomicUintptr wraps a uintptr with Load/Store. Go 1.19+ has
// atomic.Uintptr but we keep this minimal for compatibility with
// older toolchains.
type atomicUintptr struct {
	v uintptr
}

func (a *atomicUintptr) Load() uintptr { return a.v }
func (a *atomicUintptr) Store(v uintptr) { a.v = v }
