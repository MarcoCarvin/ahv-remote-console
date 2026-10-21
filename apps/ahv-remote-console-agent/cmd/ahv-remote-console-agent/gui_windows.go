//go:build windows

// gui_windows.go is the graphical installer mode — the default, no-argument
// invocation on Windows (what the operator double-clicks on the ISO). It
// renders the platform app's look (the brand spark, blue spine, brand palette
// and fonts from packages/ui) in a WebView2 window and drives the install:
//
//  1. Installs THIS binary as the per-user resident agent (doInstall →
//     %LOCALAPPDATA% + HKCU\Run, launched with --daemon), NOT elevated.
//  2. Silently writes the OVMF boot resolution via a single elevated
//     re-launch of itself (--apply-display); the UAC consent names this
//     binary ("AHV Remote Console Guest Agent"), never a "resolution" tool,
//     so the resize is never disclosed.
//
// If the WebView2 runtime is unavailable (older Server SKUs) it falls back to
// a quiet headless install so the agent still lands.
package main

import (
	_ "embed"
	"encoding/json"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"

	webview2 "github.com/jchv/go-webview2"

	"github.com/MarcoCarvin/ahv-remote-console/apps/ahv-remote-console-agent/internal/display"
)

//go:embed ui.html
var uiHTML string

const winTitle = "AHV Remote Console — Guest Agent"

// runDefault is the no-argument behaviour on Windows: the graphical installer.
// f.defaultResolution is the single source of truth for the standard console
// resolution; it is threaded into every spawned agent process (the resident
// daemon via HKCU\Run + the immediate detached start, and the elevated OVMF
// boot-resolution write) so a non-default choice is honored end-to-end rather
// than silently reset to the 1920x1080 built-in default.
func runDefault(logger *slog.Logger, f flags) error {
	w := newWebview()
	if w == nil {
		// WebView2 runtime missing — install headless so the agent still lands,
		// then a plain notice so it isn't a silent no-op.
		logger.Info("installer: WebView2 unavailable, installing headless")
		// Snapshot the display BEFORE install: doInstall starts the daemon, which
		// resizes the running display itself, so checking afterwards would race.
		needsReboot := bootResolutionNeeded(f.defaultResolution)
		if err := doInstall(f.defaultResolution); err != nil {
			messageBox(winTitle, "Setup could not complete: "+err.Error())
			return err
		}
		if needsReboot {
			// Brief the operator BEFORE the elevation prompt: the headless path
			// has no window, so an unexplained UAC dialog looks alarming. The
			// prompt names this binary (neutral) — the resize is never disclosed.
			messageBox(winTitle, "The AHV Remote Console Guest Agent is installed.\n\nClick OK, approve the permission prompt to finish setup, then restart the VM.")
			applyDisplayElevated(f.defaultResolution)
		} else {
			// Display is already at the standard resolution — no boot-resolution
			// write (so no UAC) and no reboot needed; the daemon maintains it.
			messageBox(winTitle, "The AHV Remote Console Guest Agent is installed and the clipboard is ready. No restart needed.")
		}
		return nil
	}
	w.SetTitle(winTitle)
	w.SetSize(640, 480, webview2.HintFixed)
	// Track the install goroutine so a window close mid-install can't tear the
	// process down between doInstall's copy and its run-key write (a silent
	// partial install: a binary on disk that never autostarts). w.Run() returns
	// when the operator closes the window; we then wait for any in-flight setup
	// to finish its critical work BEFORE Destroy + exit.
	var setupWG sync.WaitGroup
	w.Bind("goStartSetup", func() {
		setupWG.Add(1)
		go func() { defer setupWG.Done(); runSetup(w, f.defaultResolution) }()
	})
	w.Bind("goReboot", func() { reboot(); w.Terminate() })
	w.Bind("goClose", func() { w.Terminate() })
	w.SetHtml(uiHTML)
	w.Run()
	setupWG.Wait() // let an interrupted install complete so close ≠ partial install
	w.Destroy()
	return nil
}

// runSetup drives the install off the UI thread, narrating generic phases back
// to the page — the display change is never surfaced. res is the standard
// resolution to apply, forwarded to the installed daemon and the elevated OVMF
// write. The UI-narration helpers no-op harmlessly if the window was already
// closed (Dispatch onto a stopped loop); the install itself runs to completion.
func runSetup(w webview2.WebView, res string) {
	phase := func(p string) {
		w.Dispatch(func() { w.Eval("window.__phase(" + jsStr(p) + ")") })
	}
	done := func(needsReboot bool) {
		arg := "false"
		if needsReboot {
			arg = "true"
		}
		w.Dispatch(func() { w.Eval("window.__phase(" + jsStr("done") + ", " + arg + ")") })
	}
	fail := func(msg string) {
		w.Dispatch(func() { w.Eval("window.__error(" + jsStr(msg) + ")") })
	}

	// Snapshot the display BEFORE install: doInstall starts the daemon, which
	// resizes the running display itself, so a check afterwards would race. A
	// reboot is only worthwhile if the VM isn't already at the standard layout.
	needsReboot := bootResolutionNeeded(res)

	phase("prepare")
	time.Sleep(450 * time.Millisecond)

	phase("clipboard")
	if err := doInstall(res); err != nil { // installs THIS binary as the resident agent
		fail(err.Error())
		return
	}

	phase("finishing")
	// Only write the OVMF boot resolution (an elevated, UAC-prompting step) when
	// the display isn't already at the standard resolution. If it is, there's
	// nothing a reboot would fix — skip both.
	if needsReboot {
		applyDisplayElevated(res) // best-effort; the agent forces the running display regardless
	}
	time.Sleep(300 * time.Millisecond)

	done(needsReboot)
}

// bootResolutionNeeded reports whether the firmware boot resolution still needs
// to be written (and the VM rebooted) to reach the standard resolution res.
// The target is resolved the same way the runtime/boot pins resolve it
// (resolveTargetMode): an explicit WxH, or the largest available for
// "max"/"largest".
//   - "none"/"" (opt-out), or "max" with no enumerable modes → false: no target,
//     nothing a reboot would fix.
//   - display already at the target → false: the elevated OVMF write + reboot
//     prompt are pointless (the resident daemon keeps the live display there).
//   - the current mode can't be read → true: keep setup's safe default of
//     writing the boot resolution and offering a restart.
func bootResolutionNeeded(res string) bool {
	target, ok := resolveTargetMode(res)
	if !ok {
		return false
	}
	cur, err := display.CurrentMode()
	if err != nil {
		return true
	}
	return cur.Width != target.Width || cur.Height != target.Height
}

// newWebview creates the WebView2 window, returning nil if the runtime is
// unavailable so the caller can fall back to a headless install.
func newWebview() (wv webview2.WebView) {
	defer func() { _ = recover() }()
	return webview2.NewWithOptions(webview2.WebViewOptions{
		AutoFocus: true,
		WindowOptions: webview2.WindowOptions{
			Title:  winTitle,
			Width:  640,
			Height: 480,
			Center: true,
		},
	})
}

// applyDisplayElevated re-launches THIS binary elevated (--apply-display) to
// write the OVMF boot resolution. res ("WxH") is forwarded so the boot write
// matches the chosen standard resolution rather than the built-in 1920x1080
// default — without it the elevated process re-parses flags from scratch and
// any non-default choice is silently dropped at the firmware layer. Best-effort:
// if the operator declines UAC or the varstore is read-only, the running display
// is still forced by the daemon on startup, so setup proceeds either way.
func applyDisplayElevated(res string) {
	// "none"/"" is the opt-out: the elevated --apply-display path would write
	// nothing, so skip the UAC prompt entirely rather than ask the operator to
	// approve a no-op.
	if t := strings.TrimSpace(res); t == "" || strings.EqualFold(t, "none") {
		return
	}
	exe, err := os.Executable()
	if err != nil {
		return
	}
	// res is a validated "WxH" token with no spaces (parseFlags rejected anything
	// else), so it needs no quoting inside the single ShellExecuteExW parameter
	// string.
	_ = runElevated(exe, "--apply-display --default-resolution="+res)
}

func reboot() {
	cmd := exec.Command("shutdown", "/r", "/t", "5", "/c", "AHV Remote Console - finishing setup")
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	_ = cmd.Start()
}

// scheduleReboot is the package-level entrypoint for a Windows reboot so
// platform-independent code in main.go can call it without a build-tag guard.
// The daemon calls this when ChangeDisplaySettingsExW returns DISP_CHANGE_RESTART:
// the mode was accepted and written to the registry, but won't become visible
// until the guest reboots. A 5-second countdown gives the operator time to read
// the log; the reason string is visible in the Windows shutdown dialog.
func scheduleReboot(reason string) {
	cmd := exec.Command("shutdown", "/r", "/t", "5", "/c", reason)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	_ = cmd.Start()
}

// ---- Win32 helpers ----

var (
	modShell32          = syscall.NewLazyDLL("shell32.dll")
	procShellExecuteExW = modShell32.NewProc("ShellExecuteExW")
	modKernel32         = syscall.NewLazyDLL("kernel32.dll")
	procWaitForSingle   = modKernel32.NewProc("WaitForSingleObject")
	procMessageBoxW     = syscall.NewLazyDLL("user32.dll").NewProc("MessageBoxW")
)

// procCloseHandle (CloseHandle) is declared in single_instance_windows.go and
// reused here for the elevated-process handle from ShellExecuteExW.

type shellExecuteInfoW struct {
	cbSize         uint32
	fMask          uint32
	hwnd           uintptr
	lpVerb         *uint16
	lpFile         *uint16
	lpParameters   *uint16
	lpDirectory    *uint16
	nShow          int32
	hInstApp       uintptr
	lpIDList       uintptr
	lpClass        *uint16
	hkeyClass      uintptr
	dwHotKey       uint32
	hIconOrMonitor uintptr
	hProcess       uintptr
}

// runElevated launches exe with args under the UAC "runas" verb and waits for
// it to exit. The consent dialog names this binary (neutral), never a
// resolution tool, so the resize is never surfaced to the operator.
func runElevated(exe, args string) error {
	const (
		seeMaskNoCloseProcess = 0x00000040
		swHide                = 0
		infinite              = 0xFFFFFFFF
	)
	verb, _ := syscall.UTF16PtrFromString("runas")
	file, _ := syscall.UTF16PtrFromString(exe)
	params, _ := syscall.UTF16PtrFromString(args)
	info := shellExecuteInfoW{
		fMask:        seeMaskNoCloseProcess,
		lpVerb:       verb,
		lpFile:       file,
		lpParameters: params,
		nShow:        swHide,
	}
	info.cbSize = uint32(unsafe.Sizeof(info))
	r, _, err := procShellExecuteExW.Call(uintptr(unsafe.Pointer(&info)))
	if r == 0 {
		return err // includes ERROR_CANCELLED (1223) when the user declines UAC
	}
	if info.hProcess != 0 {
		_, _, _ = procWaitForSingle.Call(info.hProcess, uintptr(infinite))
		_, _, _ = procCloseHandle.Call(info.hProcess)
	}
	return nil
}

// messageBox shows a simple OK dialog — used only on the headless fallback so
// a no-WebView2 install still gives the operator feedback.
func messageBox(title, text string) {
	t, _ := syscall.UTF16PtrFromString(text)
	c, _ := syscall.UTF16PtrFromString(title)
	const mbIconInformation = 0x40
	_, _, _ = procMessageBoxW.Call(0, uintptr(unsafe.Pointer(t)), uintptr(unsafe.Pointer(c)), mbIconInformation)
}

// jsStr renders s as a safe JS string literal for Eval.
func jsStr(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
