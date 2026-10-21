// Command ahv-remote-console-agent is the in-guest agent for AHV Remote
// Console — "AHV Remote Console Guest Agent". It is ONE binary that plays
// several roles depending on how it is invoked:
//
//	(no args, Windows)      # graphical installer (WebView2) — what the
//	                        # operator double-clicks on the ISO; installs
//	                        # this binary as the resident agent
//	--daemon                # resident agent: bridges the guest clipboard to
//	                        # the operator over the ISA serial / vdagent
//	                        # channel and forces the running display to
//	                        # 1920x1080. Launched at login via HKCU\Run.
//	--install / --uninstall # register / remove the per-user HKCU\Run autostart
//	--apply-display         # (elevated, one-shot) write the OVMF boot
//	                        # resolution; run by the installer
//	--version               # print build info and exit
//
// Delivered to the guest as a CD-ROM/ISO: the operator mounts it and runs the
// binary, whose installer mode copies it to %LOCALAPPDATA% and registers
// HKCU\Run (with --daemon) so it starts at every login. Off Windows there is
// no GUI; the bare invocation runs as the daemon.

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/MarcoCarvin/ahv-remote-console/apps/ahv-remote-console-agent/internal/build"
	"github.com/MarcoCarvin/ahv-remote-console/apps/ahv-remote-console-agent/internal/clipboard"
	"github.com/MarcoCarvin/ahv-remote-console/apps/ahv-remote-console-agent/internal/display"
	"github.com/MarcoCarvin/ahv-remote-console/apps/ahv-remote-console-agent/internal/uefivar"
	"github.com/MarcoCarvin/ahv-remote-console/apps/ahv-remote-console-agent/internal/uplinkprobe"
	"github.com/MarcoCarvin/ahv-remote-console/apps/ahv-remote-console-agent/internal/vdagent"
)

type flags struct {
	install           bool
	uninstall         bool
	setup             bool
	version           bool
	daemon            bool
	applyDisplay      bool
	logLevel          string
	uplinkProbe       bool
	defaultResolution string
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "ahv-remote-console-agent:", err)
		os.Exit(1)
	}
}

func run() error {
	// On Windows the binary is linked as the GUI subsystem (-H windowsgui) so the
	// auto-started daemon never flashes a console window. The cost is that any
	// fmt output (a CLI mode's result, flag.Parse's usage on a bad flag, our own
	// --default-resolution validation error below) has NO console when run from
	// cmd.exe and vanishes, making the command look like a silent no-op. Attach
	// the parent console FIRST — before parseFlags — so even a flag-parse or
	// validation failure is visible. No-op off Windows and when there is no
	// parent console (the double-clicked GUI, the detached/HKCU\Run daemon), so
	// it is safe to call unconditionally for every mode.
	attachParentConsole()

	f, err := parseFlags()
	if err != nil {
		return err
	}
	if f.version {
		fmt.Println(build.Info())
		return nil
	}
	if f.install {
		return doInstall(f.defaultResolution)
	}
	if f.uninstall {
		return doUninstall()
	}
	if f.setup {
		// Dialog-driven --install for Linux desktop guests; what the packaged
		// "Agent Setup" launcher (GNOME Software's post-install Launch button)
		// runs. See setup_linux.go.
		return runSetupMode(f)
	}
	if f.applyDisplay {
		// Elevated one-shot launched by the installer GUI: write the OVMF boot
		// resolution and exit. Honor --default-resolution as the single source of
		// truth for the boot + runtime "standard resolution". "none"/"" leaves the
		// firmware varstore untouched; "max"/"largest" resolves to the biggest
		// mode the display actually offers (so we never pin an unavailable target
		// like 1920x1080 on a VM whose virtual GPU can't do it). Best-effort — a
		// read-only varstore (or an unresolvable target) is non-fatal.
		target, ok := resolveTargetMode(f.defaultResolution)
		if !ok {
			return nil
		}
		// #nosec G115 -- display dimensions are small positive values from a parsed resolution
		_, err := uefivar.SetBootResolution(uint32(target.Width), uint32(target.Height))
		return err
	}

	logger := buildLogger(f.logLevel)
	// Route package-level slog calls (e.g. internal/clipboard) through the same
	// handler, so deeper-package slog.Debug isn't silently dropped at info.
	slog.SetDefault(logger)

	if f.uplinkProbe {
		// One-shot diagnostic: run the 5-variant Win32 painting experiment and
		// exit. Bypasses the daemon lifecycle (no single-instance guard, no
		// clipboard). Operator runs it once and reports which colors appear.
		logger.Info("agent: starting uplink-probe (one-shot, ~50s)",
			"version", build.Version, "commit", build.Commit)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		go waitForSignal(cancel)
		return uplinkprobe.Run(ctx, logger)
	}

	if f.daemon {
		return runDaemon(logger, f)
	}

	// No mode flag → platform default: the GUI installer on Windows (what the
	// operator double-clicks on the ISO); the resident daemon off Windows.
	return runDefault(logger, f)
}

// runDaemon is the resident agent: the SPICE vdagent clipboard bridge plus the
// startup display default. Launched at login via the HKCU\Run entry the
// installer writes (which passes --daemon).
func runDaemon(logger *slog.Logger, f flags) error {
	logger.Info("agent starting", "version", build.Version, "commit", build.Commit)

	// Per-user single-instance guard with retry. After --install
	// kills the old process, the mutex may take a moment to be
	// released by the OS. Retry for up to 5 seconds.
	var instance *singleInstance
	for attempt := 1; attempt <= 10; attempt++ {
		var err error
		instance, err = acquireSingleInstance()
		if err == nil {
			break
		}
		if !errors.Is(err, ErrBridgeAlreadyRunning) {
			// Log via the file sink before returning: main() prints the
			// returned error to stderr only, which is lost on guests whose
			// journald has no per-user storage. The logfile is the canonical
			// sink operators can `cat`, so a fatal startup error must land there.
			logger.Error("agent: fatal startup error", "err", err)
			return fmt.Errorf("single-instance guard: %w", err)
		}
		if attempt == 10 {
			logger.Info("agent: another instance is already running for this user, exiting")
			return nil
		}
		time.Sleep(500 * time.Millisecond)
	}
	defer instance.release()

	// Default the running guest display to the standard console resolution so
	// the whole stack is 1920x1080, not just at boot: the OVMF write governs
	// the firmware / Basic-Display resolution, but once a guest display driver
	// loads it can pick something else. Setting your own display mode is a
	// non-elevated, user-level call, so the resident daemon can do it.
	// Best-effort and off the startup path (goroutine): never blocks or fails
	// the clipboard. Pass --default-resolution=none to disable.
	//
	// Windows drives Win32 ChangeDisplaySettingsExW; Linux drives the running
	// display server (xrandr on X11, the compositor's tool on Wayland —
	// display_linux.go). On Linux this applies LIVE with no reboot, unlike the
	// Windows boot pin. Other platforms keep the no-op stub, so we gate to the
	// two real backends to avoid a spurious "not available" warning each login.
	if runtime.GOOS == "windows" || runtime.GOOS == "linux" {
		go applyDefaultResolution(logger, f.defaultResolution)
	}

	// Guest clipboard helper. Its absence is NOT fatal: the agent must run
	// regardless (it still pins the display resolution and stays resident so
	// the clipboard can be enabled later by installing a helper + restarting).
	// Only the guest<->host clipboard bridge depends on the helper, so a
	// missing one disables that bridge alone rather than killing the daemon —
	// the same "degrade, don't die" posture the serial-port loop below uses.
	clip, err := clipboard.New()
	if err != nil {
		logger.Warn("clipboard: no guest clipboard helper found — clipboard sync disabled. "+
			"Install wl-clipboard (Wayland) or xclip/xsel (X11), then restart the agent. "+
			"The display-resolution pin is unaffected.", "err", err)
		clip = nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go waitForSignal(cancel)

	// SPICE vdagent clipboard over ISA serial port — only started when a guest
	// clipboard helper exists to bridge to (otherwise there is nothing to read
	// from / write into on the guest side).
	//
	// The AHV libvirt hook swaps the serial backend to qemu-vdagent
	// ~3 seconds after QEMU starts. The port becomes a native QEMU
	// clipboard channel — the sole clipboard path. The bridge speaks
	// the SPICE agent protocol and bridges to the guest OS clipboard.
	//
	// Platform defaults: COM3 on Windows, /dev/ttyS2 on Linux.
	vdagentDone := make(chan struct{})
	if clip != nil {
		defer clip.Close()
		go func() {
			defer close(vdagentDone)
			port := defaultSerialPort
			var serialPort io.ReadWriteCloser
			for attempt := 1; attempt <= 10; attempt++ {
				var err error
				serialPort, err = vdagent.OpenSerial(port)
				if err == nil {
					break
				}
				if attempt == 1 {
					logger.Info("vdagent: serial port not ready, retrying...", "port", port, "err", err)
				}
				select {
				case <-ctx.Done():
					return
				case <-time.After(1 * time.Second):
				}
			}
			if serialPort == nil {
				logger.Info("vdagent: serial port not available, clipboard disabled", "port", port)
				return
			}
			defer serialPort.Close()
			logger.Info("vdagent: serial port opened, waiting for vdagent handshake", "port", port)
			agent := vdagent.New(serialPort, clipboardAdapter{p: clip}, logger)
			agent.SetDisplay(displayAdapter{log: logger})
			if err := agent.Run(ctx); err != nil && err != context.Canceled {
				logger.Warn("vdagent: agent exited", "err", err)
			}
		}()
	}

	logger.Info("agent running")
	<-ctx.Done()
	logger.Info("agent: shutting down")
	if clip != nil {
		// Bounded wait for the vdagent goroutine: Agent.Run's shutdown path
		// blocks in keepwarm.Stop until a live lease has restored the guest
		// cursors — exiting before that lands would leave the pointer
		// invisible until the next daemon start.
		select {
		case <-vdagentDone:
		case <-time.After(3 * time.Second):
			logger.Warn("agent: vdagent shutdown timed out")
		}
	}
	return nil
}

// clipboardAdapter bridges clipboard.Provider's []byte API to the
// convergence reconciler's OSClipboard interface (which is
// string-based). Wipes the temporary byte buffer in ReadText so the
// guest clipboard cleartext isn't lingering on the heap longer than
// it needs to be.
type clipboardAdapter struct {
	p clipboard.Provider
}

func (a clipboardAdapter) ReadText(ctx context.Context) ([]byte, error) {
	b, err := a.p.ReadText(ctx)
	if err != nil {
		// Empty / non-text clipboards are normal — the reconciler's
		// Watch semantics just swallow them without logging an error,
		// because the guest OS is just informing us that the clipboard
		// changed to a bitmap/file-list/etc.
		if errors.Is(err, clipboard.ErrEmpty) {
			return nil, nil
		}
		return nil, err
	}
	// Copy to a new slice before returning so we can zero the original.
	// Actually, returning the byte slice directly is fine because the
	// consumer (agent) doesn't leak it, but let's be careful.
	out := make([]byte, len(b))
	copy(out, b)
	clipboard.Wipe(b)
	return out, nil
}

func (a clipboardAdapter) WriteText(ctx context.Context, text []byte) error {
	buf := make([]byte, len(text))
	copy(buf, text)
	defer clipboard.Wipe(buf)
	return a.p.WriteText(ctx, buf)
}

func (a clipboardAdapter) Watch(ctx context.Context) <-chan struct{} {
	return a.p.Watch(ctx)
}

// displayAdapter adapts the display package's package-level ApplyMode
// function to the vdagent.DisplayResizer interface.
type displayAdapter struct {
	log *slog.Logger
}

func (d displayAdapter) Resize(width, height int) error {
	requested := display.Mode{Width: width, Height: height}
	applied, err := display.ApplyMode(requested)
	if err != nil {
		modes, enumErr := display.EnumerateModes()
		if enumErr == nil {
			modeStrs := make([]string, len(modes))
			for i, m := range modes {
				modeStrs[i] = fmt.Sprintf("%dx%d", m.Width, m.Height)
			}
			d.log.Warn("display: available modes", "modes", modeStrs)
		}
		return err
	}
	d.log.Info("display: mode applied",
		"requested", fmt.Sprintf("%dx%d", width, height),
		"applied", fmt.Sprintf("%dx%d", applied.Width, applied.Height))
	return nil
}

func parseFlags() (flags, error) {
	var f flags
	flag.BoolVar(&f.install, "install", false,
		"copy self to %LOCALAPPDATA%/AHV Remote Console and register HKCU\\Run, then exit")
	flag.BoolVar(&f.uninstall, "uninstall", false,
		"reverse of --install")
	flag.BoolVar(&f.setup, "setup", false,
		"(Linux) interactive per-user install: confirmation + resolution dialogs "+
			"(zenity/kdialog, terminal fallback), then the same install as --install")
	flag.BoolVar(&f.version, "version", false, "print version and exit")
	flag.BoolVar(&f.daemon, "daemon", false,
		"run as the resident agent (the HKCU\\Run autostart entry uses this)")
	flag.BoolVar(&f.applyDisplay, "apply-display", false,
		"(elevated, one-shot) write the OVMF boot resolution from --default-resolution and exit")
	flag.StringVar(&f.logLevel, "log-level", "info", "log level: debug|info|warn|error")
	flag.BoolVar(&f.uplinkProbe, "uplink-probe", false,
		"run the 5-variant uplink viability experiment (~50s) and exit.")
	// Platform-specific default: Windows VMs pin to 1920x1080 (safe,
	// known-good on AHV std-VGA; mode is always advertised by the driver)
	// so the console resolution is consistent without operator intervention.
	// Linux uses 'max' because xrandr / gdbus apply the change live and the
	// available modes are reliably enumerated after the graphical session
	// loads. Other platforms keep 'none' (no-op stub).
	defaultResolutionDefault := "none"
	switch runtime.GOOS {
	case "windows":
		defaultResolutionDefault = "1920x1080"
	case "linux":
		defaultResolutionDefault = "max"
	}
	flag.StringVar(&f.defaultResolution, "default-resolution", defaultResolutionDefault,
		"on startup, set the running guest display (best-effort, non-elevated): "+
			"'none' = leave unchanged, an explicit WxH the operator has "+
			"verified is safe (e.g. 1920x1080), or 'max'/'largest' for the biggest "+
			"offered mode. Default: 1920x1080 on Windows, max on Linux.")
	flag.Parse()
	if f.install && f.uninstall {
		return f, fmt.Errorf("--install and --uninstall are mutually exclusive")
	}
	// Validate --default-resolution at the trunk so the raw operator string can
	// never reach the spawn sinks (systemd ExecStart, the HKCU\Run value, the
	// elevated ShellExecute parameter) as anything other than a bare "WxH" token
	// or the "none"/"" opt-out. Without this a value carrying a space or a flag
	// token is re-tokenized by the OS on the next login — e.g.
	// `--default-resolution="1920x1080 --uninstall"` would bake an autostart entry
	// that uninstalls the agent — and trailing garbage ("1920x1080oops") or a
	// wrong separator ("1920X1080") would be silently mis-handled downstream.
	// Reject loudly here instead.
	if r := strings.TrimSpace(f.defaultResolution); r != "" &&
		!strings.EqualFold(r, "none") && !strings.EqualFold(r, "max") && !strings.EqualFold(r, "largest") {
		if _, _, ok := parseResolution(r); !ok {
			return f, fmt.Errorf("invalid --default-resolution %q: want WxH (e.g. 1920x1080), 'max', or 'none'", f.defaultResolution)
		}
	}
	return f, nil
}

// resolveTargetMode turns a --default-resolution value into the concrete mode
// the agent should pin: an explicit WxH, or the largest mode the display offers
// for "max"/"largest". ok is false for "none"/"" (operator opt-out) or when
// "max" cannot be resolved (no enumerable modes). Shared by the runtime pin
// (applyDefaultResolution), the elevated boot pin (--apply-display), and the
// Windows reboot-needed check, so all three agree on the target.
func resolveTargetMode(res string) (display.Mode, bool) {
	t := strings.TrimSpace(res)
	if t == "" || strings.EqualFold(t, "none") {
		return display.Mode{}, false
	}
	if strings.EqualFold(t, "max") || strings.EqualFold(t, "largest") {
		m, err := display.LargestMode()
		if err != nil {
			return display.Mode{}, false
		}
		return m, true
	}
	if w, h, ok := parseResolution(t); ok {
		return display.Mode{Width: w, Height: h}, true
	}
	return display.Mode{}, false
}

// applyDefaultResolution sets the running guest display to the configured
// target on every daemon start (i.e. every login, via HKCU\Run / the systemd
// user unit), so the standard layout is re-asserted automatically. The target
// is resolved by resolveTargetMode: an explicit WxH is applied exactly (no
// nearest-mode snap); "max"/"largest" picks the biggest mode the display
// actually offers. Default targets: 1920x1080 on Windows, max on Linux.
// Best-effort: an unresolvable/unsupported target is logged (with the available
// modes) and otherwise ignored — the clipboard daemon must run regardless.
// "none"/"" leaves the resolution alone.
func applyDefaultResolution(logger *slog.Logger, res string) {
	if t := strings.TrimSpace(res); t == "" || strings.EqualFold(t, "none") {
		return
	}
	// The daemon autostarts with the login and can beat the graphical session
	// (and its env import) by several seconds — until then the display backend
	// is simply unreachable and "max" resolves to nothing. Wait for mode
	// enumeration to come up before resolving the target, exactly like the
	// serial loop waits for its port; give up quietly after ~2 minutes (a
	// headless session has nothing to resize). On Windows enumeration works
	// immediately, so this loop exits on the first attempt.
	for attempt := 1; ; attempt++ {
		if _, err := display.EnumerateModes(); err == nil {
			break
		}
		if attempt >= 24 {
			logger.Warn("display: no display session reachable; leaving display unchanged", "requested", res)
			return
		}
		time.Sleep(5 * time.Second)
	}
	target, ok := resolveTargetMode(res)
	if !ok {
		// Enumeration works but the target still didn't resolve (e.g. an
		// invalid token parseFlags should have rejected): nothing to apply.
		logger.Warn("display: default resolution unresolvable; leaving display unchanged", "requested", res)
		return
	}
	applied, err := display.ApplyExact(target)
	if errors.Is(err, display.ErrRestartRequired) {
		// Windows accepted the mode and wrote it to the registry, but the
		// current session cannot display it without a reboot. Schedule one
		// now (5-second countdown) so the resolution takes effect — the
		// daemon will restart at login into the correct mode.
		logger.Info("display: default resolution requires restart; scheduling reboot",
			"requested", res,
			"written", fmt.Sprintf("%dx%d", applied.Width, applied.Height))
		scheduleReboot("AHV Remote Console — applying display resolution")
		return
	}
	if err != nil {
		// Not available on this adapter — leave the display untouched and log
		// the modes that ARE offered (useful when a requested WxH isn't one).
		if modes, enumErr := display.EnumerateModes(); enumErr == nil {
			strs := make([]string, len(modes))
			for i, m := range modes {
				strs[i] = fmt.Sprintf("%dx%d", m.Width, m.Height)
			}
			logger.Warn("display: default resolution not available; leaving display unchanged", "requested", res, "available", strs, "err", err)
		} else {
			logger.Warn("display: default resolution not available; leaving display unchanged", "requested", res, "err", err)
		}
		return
	}
	logger.Info("display: default resolution applied", "requested", res,
		"applied", fmt.Sprintf("%dx%d", applied.Width, applied.Height))
}

// parseResolution parses a "WxH" string into positive dimensions. ok is false
// for anything that isn't EXACTLY two positive base-10 integers joined by a
// single lowercase 'x' — so "none", "", "1920X1080" (capital X), "1920x1080oops"
// (trailing garbage), "1920x1080x720" and "1920 1080" all return ok=false,
// letting callers choose their own fallback. (fmt.Sscanf("%dx%d") was too
// loose: it does not require end-of-input, so trailing garbage parsed as ok.)
func parseResolution(res string) (w, h int, ok bool) {
	a, b, found := strings.Cut(strings.TrimSpace(res), "x")
	if !found {
		return 0, 0, false
	}
	w, errW := strconv.Atoi(a)
	h, errH := strconv.Atoi(b)
	if errW != nil || errH != nil || w <= 0 || h <= 0 {
		return 0, 0, false
	}
	return w, h, true
}

func buildLogger(level string) *slog.Logger {
	// DIAG_TODO: default to debug while we diagnose handshake /
	// clipboard sync issues. Flip back to info before shipping.
	lvl := slog.LevelDebug
	switch level {
	case "debug":
		lvl = slog.LevelDebug
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	}
	// Log to a file alongside stderr. When the bridge is started
	// detached via HKCU\Run there's no console attached, so stderr
	// is lost — that left us blind to handshake / decoder failures.
	// File path mirrors the install directory: %LOCALAPPDATA%/AHV
	// Remote Console/hcbridge.log. Operators (and us) can `type` it
	// from the guest to diagnose envelope rejects, decoder hiccups,
	// AEAD failures, clipboard write errors.
	//
	// File APPENDS across daemon starts: a restart must never destroy the
	// previous run's evidence (a start-time truncate erased the forensic
	// trail of a field incident — the periodic truncator below already
	// bounds growth at 5 MiB, so truncating at start bought nothing).
	// HC_BRIDGE_LOG_APPEND is retained for back-compat; it now matches
	// the default.
	out := io.Writer(os.Stderr)
	if path := logFilePath(); path != "" {
		flags := os.O_CREATE | os.O_WRONLY | os.O_APPEND
		if f, err := os.OpenFile(path, flags, 0o600); err == nil {
			// The log FILE is the canonical sink — operators read hcbridge.log
			// to debug clipboard. stderr is only a best-effort dev echo. On
			// Windows the agent is GUI-subsystem (-H windowsgui) so it has NO
			// console and os.Stderr is an invalid handle; wrap it so a failed
			// stderr write can NEVER abort the MultiWriter and starve the file
			// (io.MultiWriter stops at the first writer error). File is FIRST.
			//
			// Both the slog handler's writes AND the periodic truncator touch
			// the same *os.File offset. The JSONHandler serializes its own
			// writes with an internal mutex, but the truncator's Truncate+Seek
			// would bypass that mutex and race the file offset, interleaving /
			// garbling JSON exactly when an operator is reading the log. Route
			// BOTH through one syncFileWriter so they share a single lock.
			sf := &syncFileWriter{f: f}
			out = io.MultiWriter(sf, bestEffortWriter{os.Stderr})
			go truncateLogPeriodically(sf, 5*1024*1024)
		}
	}
	return slog.New(slog.NewJSONHandler(out, &slog.HandlerOptions{Level: lvl}))
}

// bestEffortWriter forwards to w but always reports full success, so it can
// sit inside an io.MultiWriter without a broken underlying writer (e.g. a
// non-existent console on a GUI-subsystem Windows build, where os.Stderr is
// an invalid handle) aborting writes to the other sinks.
type bestEffortWriter struct{ w io.Writer }

func (b bestEffortWriter) Write(p []byte) (int, error) {
	_, _ = b.w.Write(p)
	return len(p), nil
}

// syncFileWriter serializes every operation that touches the underlying
// *os.File's offset behind a single mutex. The slog JSONHandler writes
// through it (so its writes take the lock), and truncateLogPeriodically
// truncates through it (so Truncate+Seek take the SAME lock). Without this
// shared lock the truncator's Seek(0,0) could land between a handler write's
// implicit append and the next, garbling the JSON an operator is reading.
type syncFileWriter struct {
	mu sync.Mutex
	f  *os.File
}

func (w *syncFileWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.f.Write(p)
}

// truncate empties the file and rewinds the offset under the same lock the
// writes use, so it can never interleave with an in-flight handler write.
func (w *syncFileWriter) truncate() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := w.f.Truncate(0); err != nil {
		return err
	}
	_, err := w.f.Seek(0, 0)
	return err
}

func (w *syncFileWriter) size() (int64, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	info, err := w.f.Stat()
	if err != nil {
		return 0, err
	}
	return info.Size(), nil
}

func truncateLogPeriodically(w *syncFileWriter, maxBytes int64) {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()
	for range ticker.C {
		size, err := w.size()
		if err != nil || size < maxBytes {
			continue
		}
		_ = w.truncate()
	}
}

func waitForSignal(cancel context.CancelFunc) {
	c := make(chan os.Signal, 1)
	signal.Notify(c, syscall.SIGINT, syscall.SIGTERM)
	<-c
	cancel()
}
