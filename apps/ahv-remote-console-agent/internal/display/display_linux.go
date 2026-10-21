//go:build linux

// display_linux.go is the Linux counterpart of display_windows.go's
// Win32 ChangeDisplaySettingsExW path. On Nutanix/AHV the guest's QEMU
// std-VGA is driven by the bochs-drm KMS driver, and QEMU ignores the RFB
// SetDesktopSize request for that adapter — so, exactly as on the Windows
// Basic-Display sibling, the resize must be driven from INSIDE the guest.
//
// We do that by driving the running display server, never by a raw DRM/KMS
// modeset: a non-root systemd --user daemon can never hold DRM master while
// a compositor owns /dev/dri/card0, so drmModeSetCrtc would only ever return
// EACCES. Instead we shell out (the agent is a static CGO_ENABLED=0 binary,
// so this mirrors the clipboard package's shell-out convention) to:
//
//   - X11:           xrandr (PRIMARY; can synthesize a missing mode via cvt)
//   - Wayland-GNOME: gdctl   (Mutter)
//   - Wayland-KDE:   kscreen-doctor
//   - Wayland-wlroots: wlr-randr
//
// Every apply is confirmed by READ-BACK (xrandr/compositor exit codes are
// unreliable — xrandr prints "Configure crtc failed" to stderr yet may exit
// 0). When no usable server/tool is present we return ErrNoMatchingMode, the
// same graceful "not applicable" the no-op stub returns elsewhere, so the
// bridge reconciler ACKs the documented size=65535 sentinel.
package display

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// execTimeout bounds every helper invocation so a hung tool (e.g. an X
// server that accepted the socket but never answers) can't wedge the
// best-effort resize path.
const execTimeout = 5 * time.Second

// EnumerateModes lists the modes the running display server advertises.
func EnumerateModes() ([]Mode, error) {
	b, err := pickBackend()
	if err != nil {
		return nil, err
	}
	modes, _, err := b.enumerate()
	if err != nil {
		return nil, err
	}
	if len(modes) == 0 {
		return nil, ErrNoMatchingMode
	}
	return modes, nil
}

// CurrentMode returns the active mode of the running display server.
func CurrentMode() (Mode, error) {
	b, err := pickBackend()
	if err != nil {
		return Mode{}, err
	}
	_, cur, err := b.enumerate()
	if err != nil {
		return Mode{}, err
	}
	if cur.Width == 0 {
		return Mode{}, ErrNoMatchingMode
	}
	return cur, nil
}

// ApplyMode picks the best-fitting supported mode (PickBestMode, shared with
// the Windows path) and commits it, mirroring ApplyMode's contract.
func ApplyMode(requested Mode) (Mode, error) {
	b, err := pickBackend()
	if err != nil {
		return Mode{}, err
	}
	modes, _, err := b.enumerate()
	if err != nil {
		return Mode{}, err
	}
	best, err := PickBestMode(modes, requested)
	if err != nil {
		return Mode{}, err
	}
	return applyAndVerify(b, best.Width, best.Height)
}

// ApplyExact sets exactly requested.WxH or leaves the display untouched —
// the same exact-or-nothing contract the Windows ApplyExact honors (used by
// the daemon's startup --default-resolution pin).
func ApplyExact(requested Mode) (Mode, error) {
	if requested.Width <= 0 || requested.Height <= 0 {
		return Mode{}, ErrNoMatchingMode
	}
	b, err := pickBackend()
	if err != nil {
		return Mode{}, err
	}
	return applyAndVerify(b, requested.Width, requested.Height)
}

// applyAndVerify asks the backend to set w×h, then confirms via a fresh
// read-back — exit codes are not trustworthy, the active mode is.
func applyAndVerify(b backend, w, h int) (Mode, error) {
	if err := b.applyExact(w, h); err != nil {
		return Mode{}, err
	}
	if _, cur, err := b.enumerate(); err == nil && cur.Width == w && cur.Height == h {
		return cur, nil
	}
	// Name the backend that couldn't deliver the mode so the daemon's
	// best-effort warn log says whether it was x11/xrandr vs a Wayland
	// compositor tool; still unwraps to ErrNoMatchingMode for callers.
	return Mode{}, fmt.Errorf("%s: %w", b.label(), ErrNoMatchingMode)
}

// backend is one display-server driver (X11 or a specific Wayland compositor).
type backend interface {
	// enumerate returns the advertised modes and the active mode (zero Mode
	// if it can't be determined).
	enumerate() ([]Mode, Mode, error)
	// applyExact attempts to set exactly w×h. A nil return means the command
	// pipeline ran; the caller still verifies the result via read-back.
	applyExact(w, h int) error
	label() string
}

// pickBackend detects the session type and the available tool, preferring
// Wayland detection first: under Wayland, Xwayland still exports a DISPLAY,
// but xrandr against it only resizes a dummy output, never the real panel.
func pickBackend() (backend, error) {
	env := sessionEnv()
	if isWayland(env) {
		switch {
		case isGNOME(env) && hasExec("gdbus"):
			// GNOME/Mutter via its DisplayConfig DBus API. gdbus ships with
			// GLib, so it is present on every GNOME guest — unlike gdctl
			// (GNOME 48+) or gnome-randr, which are usually absent.
			return &gnomeBackend{env: env}, nil
		case hasExec("kscreen-doctor"):
			return &kdeBackend{env: env}, nil
		case hasExec("wlr-randr"):
			return &wlrBackend{env: env}, nil
		case hasExec("gdbus"):
			// GNOME-like session without the desktop hint — try Mutter anyway;
			// getState() fails gracefully if it is not actually Mutter.
			return &gnomeBackend{env: env}, nil
		default:
			return nil, ErrNoMatchingMode // Wayland, but no tool we can drive
		}
	}
	if getenv(env, "DISPLAY") != "" && hasExec("xrandr") {
		return &x11Backend{env: env}, nil
	}
	return nil, ErrNoMatchingMode // no GUI session reachable from this daemon
}

// isGNOME reports whether the session is GNOME/Mutter, from the desktop hints.
func isGNOME(env []string) bool {
	d := strings.ToLower(getenv(env, "XDG_CURRENT_DESKTOP") + ":" + getenv(env, "XDG_SESSION_DESKTOP"))
	return strings.Contains(d, "gnome")
}

// ---- session environment discovery ----------------------------------------

// sessionEnv builds the minimal, sanitized environment the display tools need.
// A systemd --user daemon usually inherits DISPLAY/WAYLAND_DISPLAY (the
// compositor runs `systemctl --user import-environment`), but it can start
// before that import — the #1 real-world failure — so when both are missing we
// harvest them from the compositor's /proc/<pid>/environ (same uid, no root).
func sessionEnv() []string {
	keep := []string{
		"DISPLAY", "WAYLAND_DISPLAY", "XAUTHORITY", "XDG_RUNTIME_DIR",
		"DBUS_SESSION_BUS_ADDRESS", "XDG_SESSION_TYPE", "XDG_CURRENT_DESKTOP",
		"XDG_SESSION_DESKTOP", "HOME", "PATH", "LANG", "LC_ALL",
	}
	vals := map[string]string{}
	for _, k := range keep {
		if v, ok := os.LookupEnv(k); ok {
			vals[k] = v
		}
	}
	if vals["DISPLAY"] == "" && vals["WAYLAND_DISPLAY"] == "" {
		for k, v := range harvestSessionEnv() {
			if vals[k] == "" {
				vals[k] = v
			}
		}
	}
	// Sensible defaults for the bus/runtime dir so the Wayland tools connect.
	if vals["XDG_RUNTIME_DIR"] == "" {
		vals["XDG_RUNTIME_DIR"] = "/run/user/" + strconv.Itoa(os.Getuid())
	}
	if vals["DBUS_SESSION_BUS_ADDRESS"] == "" {
		vals["DBUS_SESSION_BUS_ADDRESS"] = "unix:path=" + filepath.Join(vals["XDG_RUNTIME_DIR"], "bus")
	}
	out := make([]string, 0, len(vals))
	for k, v := range vals {
		if v != "" {
			out = append(out, k+"="+v)
		}
	}
	return out
}

// harvestSessionEnv recovers the graphical-session variables by reading the
// NUL-separated environ of a same-uid compositor / X server process.
func harvestSessionEnv() map[string]string {
	want := map[string]bool{
		"DISPLAY": true, "WAYLAND_DISPLAY": true, "XAUTHORITY": true,
		"XDG_RUNTIME_DIR": true, "DBUS_SESSION_BUS_ADDRESS": true,
		"XDG_SESSION_TYPE": true, "XDG_CURRENT_DESKTOP": true,
		"XDG_SESSION_DESKTOP": true,
	}
	leaders := map[string]bool{
		"gnome-shell": true, "kwin_wayland": true, "sway": true, "labwc": true,
		"Hyprland": true, "hyprland": true, "Xorg": true, "Xwayland": true,
		"weston": true, "wayfire": true,
	}
	// #nosec G115 -- os.Getuid() returns the process's real uid, a small
	// non-negative value that never overflows uint32.
	uid := uint32(os.Getuid())
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		pid := e.Name()
		if pid[0] < '0' || pid[0] > '9' {
			continue
		}
		comm, err := os.ReadFile("/proc/" + pid + "/comm")
		if err != nil || !leaders[strings.TrimSpace(string(comm))] {
			continue
		}
		fi, err := os.Stat("/proc/" + pid)
		if err != nil || !ownedBy(fi, uid) {
			continue
		}
		raw, err := os.ReadFile("/proc/" + pid + "/environ")
		if err != nil {
			continue
		}
		got := map[string]string{}
		for _, kv := range bytes.Split(raw, []byte{0}) {
			i := bytes.IndexByte(kv, '=')
			if i <= 0 {
				continue
			}
			k := string(kv[:i])
			if want[k] {
				got[k] = string(kv[i+1:])
			}
		}
		if got["DISPLAY"] != "" || got["WAYLAND_DISPLAY"] != "" {
			return got
		}
	}
	return nil
}

// ownedBy reports whether fi (a /proc/<pid> dir) is owned by uid — so we only
// ever read the environ of the operator's own processes, never another user's.
func ownedBy(fi os.FileInfo, uid uint32) bool {
	st, ok := fi.Sys().(*syscall.Stat_t)
	return ok && st.Uid == uid
}

func isWayland(env []string) bool {
	return getenv(env, "WAYLAND_DISPLAY") != "" ||
		strings.EqualFold(getenv(env, "XDG_SESSION_TYPE"), "wayland")
}

func getenv(env []string, key string) string {
	pre := key + "="
	for _, kv := range env {
		if strings.HasPrefix(kv, pre) {
			return kv[len(pre):]
		}
	}
	return ""
}

func hasExec(name string) bool {
	_, err := exec.LookPath(name)
	return err == nil
}

// run executes name with the given env (combined stdout+stderr captured for
// parsing) under the shared timeout.
func run(env []string, name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), execTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = env
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err := cmd.Run()
	return buf.String(), err
}

// ---- X11 / xrandr (PRIMARY) ------------------------------------------------

type x11Backend struct{ env []string }

func (b *x11Backend) label() string { return "x11/xrandr" }

func (b *x11Backend) enumerate() ([]Mode, Mode, error) {
	out, err := run(b.env, "xrandr", "--query")
	if err != nil {
		return nil, Mode{}, fmt.Errorf("xrandr --query: %w", err)
	}
	_, modes, cur := parseXrandrQuery(out)
	return modes, cur, nil
}

func (b *x11Backend) applyExact(w, h int) error {
	out, err := run(b.env, "xrandr", "--query")
	if err != nil {
		return fmt.Errorf("xrandr --query: %w", err)
	}
	output, modes, _ := parseXrandrQuery(out)
	if output == "" {
		return ErrNoMatchingMode
	}
	name := fmt.Sprintf("%dx%d", w, h)
	listed := false
	for _, m := range modes {
		if m.Width == w && m.Height == h {
			listed = true
			break
		}
	}
	if !listed {
		// Synthesize the mode: cvt → --newmode → --addmode. A BadMatch on
		// --addmode means the driver/screen rejects it (VRAM or max screen
		// size); abort without touching the current resolution.
		synth, err := b.addCvtMode(output, w, h)
		if err != nil {
			return err
		}
		name = synth
	}
	// Exit status is unreliable here — applyAndVerify confirms via read-back.
	_, _ = run(b.env, "xrandr", "--output", output, "--mode", name)
	return nil
}

func (b *x11Backend) addCvtMode(output string, w, h int) (string, error) {
	cvtOut, err := run(b.env, "cvt", strconv.Itoa(w), strconv.Itoa(h), "60")
	if err != nil {
		return "", ErrNoMatchingMode // cvt not installed → can't synthesize
	}
	name, args, ok := parseCvtModeline(cvtOut)
	if !ok {
		return "", ErrNoMatchingMode
	}
	// --newmode fails with BadName if the mode already exists — tolerate that.
	_, _ = run(b.env, "xrandr", append([]string{"--newmode", name}, args...)...)
	if _, err := run(b.env, "xrandr", "--addmode", output, name); err != nil {
		return "", ErrNoMatchingMode // BadMatch: driver/screen rejected the mode
	}
	return name, nil
}

// ---- Wayland: wlroots / wlr-randr ------------------------------------------

type wlrBackend struct{ env []string }

func (b *wlrBackend) label() string { return "wayland/wlr-randr" }

type wlrOutput struct {
	Name  string `json:"name"`
	Modes []struct {
		Width   int     `json:"width"`
		Height  int     `json:"height"`
		Refresh float64 `json:"refresh"`
		Current bool    `json:"current"`
	} `json:"modes"`
}

func (b *wlrBackend) outputs() ([]wlrOutput, error) {
	out, err := run(b.env, "wlr-randr", "--json")
	if err != nil {
		return nil, fmt.Errorf("wlr-randr --json: %w", err)
	}
	var outs []wlrOutput
	if err := json.Unmarshal([]byte(out), &outs); err != nil {
		return nil, err
	}
	return outs, nil
}

func (b *wlrBackend) enumerate() ([]Mode, Mode, error) {
	outs, err := b.outputs()
	if err != nil || len(outs) == 0 {
		return nil, Mode{}, err
	}
	o := outs[0]
	var modes []Mode
	var cur Mode
	for _, m := range o.Modes {
		modes = append(modes, Mode{Width: m.Width, Height: m.Height, Hz: int(m.Refresh + 0.5)})
		if m.Current {
			cur = Mode{Width: m.Width, Height: m.Height, Hz: int(m.Refresh + 0.5)}
		}
	}
	return modes, cur, nil
}

func (b *wlrBackend) applyExact(w, h int) error {
	outs, err := b.outputs()
	if err != nil || len(outs) == 0 {
		return ErrNoMatchingMode
	}
	name := outs[0].Name
	listed := false
	for _, m := range outs[0].Modes {
		if m.Width == w && m.Height == h {
			listed = true
			break
		}
	}
	if !listed {
		return ErrNoMatchingMode // no modeline injection on Wayland; defer to boot-pin
	}
	_, _ = run(b.env, "wlr-randr", "--output", name, "--mode", fmt.Sprintf("%dx%d", w, h))
	return nil
}

// ---- Wayland: KDE / kscreen-doctor -----------------------------------------

type kdeBackend struct{ env []string }

func (b *kdeBackend) label() string { return "wayland/kscreen-doctor" }

type kscreenState struct {
	Outputs []struct {
		Name          string `json:"name"`
		CurrentModeID string `json:"currentModeId"`
		Modes         []struct {
			ID   string `json:"id"`
			Size struct {
				Width  int `json:"width"`
				Height int `json:"height"`
			} `json:"size"`
			RefreshRate float64 `json:"refreshRate"`
		} `json:"modes"`
	} `json:"outputs"`
}

func (b *kdeBackend) state() (*kscreenState, error) {
	out, err := run(b.env, "kscreen-doctor", "--json")
	if err != nil {
		return nil, fmt.Errorf("kscreen-doctor --json: %w", err)
	}
	var st kscreenState
	if err := json.Unmarshal([]byte(out), &st); err != nil {
		return nil, err
	}
	return &st, nil
}

func (b *kdeBackend) enumerate() ([]Mode, Mode, error) {
	st, err := b.state()
	if err != nil || len(st.Outputs) == 0 {
		return nil, Mode{}, err
	}
	o := st.Outputs[0]
	var modes []Mode
	var cur Mode
	for _, m := range o.Modes {
		md := Mode{Width: m.Size.Width, Height: m.Size.Height, Hz: int(m.RefreshRate + 0.5)}
		modes = append(modes, md)
		if m.ID == o.CurrentModeID {
			cur = md
		}
	}
	return modes, cur, nil
}

func (b *kdeBackend) applyExact(w, h int) error {
	st, err := b.state()
	if err != nil || len(st.Outputs) == 0 {
		return ErrNoMatchingMode
	}
	o := st.Outputs[0]
	for _, m := range o.Modes {
		if m.Size.Width == w && m.Size.Height == h {
			_, _ = run(b.env, "kscreen-doctor", fmt.Sprintf("output.%s.mode.%s", o.Name, m.ID))
			return nil
		}
	}
	return ErrNoMatchingMode
}

// ---- Wayland: GNOME / Mutter via gdbus -------------------------------------

const mutterDisplayConfig = "org.gnome.Mutter.DisplayConfig"

type gnomeBackend struct{ env []string }

func (b *gnomeBackend) label() string { return "wayland/gdbus(mutter)" }

// state calls Mutter's GetCurrentState and parses the serial, connector, and
// advertised modes. gdbus is used (not the pure-Go DBus client) to match the
// repo's shell-out convention and avoid a new dependency.
func (b *gnomeBackend) state() (serial, connector string, modes []mutterMode, err error) {
	out, err := run(b.env, "gdbus", "call", "--session",
		"--dest", mutterDisplayConfig,
		"--object-path", "/org/gnome/Mutter/DisplayConfig",
		"--method", mutterDisplayConfig+".GetCurrentState")
	if err != nil {
		return "", "", nil, fmt.Errorf("gdbus GetCurrentState: %w", err)
	}
	s, c, m := parseMutterCurrentState(out)
	return s, c, m, nil
}

func (b *gnomeBackend) enumerate() ([]Mode, Mode, error) {
	_, _, mm, err := b.state()
	if err != nil {
		return nil, Mode{}, err
	}
	var modes []Mode
	var cur Mode
	for _, m := range mm {
		modes = append(modes, Mode{Width: m.w, Height: m.h})
		if m.current {
			cur = Mode{Width: m.w, Height: m.h}
		}
	}
	return modes, cur, nil
}

func (b *gnomeBackend) applyExact(w, h int) error {
	serial, connector, mm, err := b.state()
	if err != nil || connector == "" || serial == "" {
		return ErrNoMatchingMode
	}
	var id string
	for _, m := range mm {
		if m.w == w && m.h == h {
			id = m.id
			break
		}
	}
	if id == "" {
		return ErrNoMatchingMode // mode not advertised (EDID/VRAM ceiling) — defer to boot-pin
	}
	// ApplyMonitorsConfig(serial, method=1 temporary, logical_monitors, props).
	// One logical monitor at (0,0), scale 1.0, transform 0 (uint32), primary,
	// driving our single connector at the chosen mode id. Temporary (not
	// persistent) mirrors the Windows runtime apply: the daemon re-asserts it
	// each login rather than rewriting the operator's monitors.xml.
	logical := fmt.Sprintf("[(0, 0, 1.0, uint32 0, true, [('%s', '%s', {})])]", connector, id)
	_, _ = run(b.env, "gdbus", "call", "--session",
		"--dest", mutterDisplayConfig,
		"--object-path", "/org/gnome/Mutter/DisplayConfig",
		"--method", mutterDisplayConfig+".ApplyMonitorsConfig",
		serial, "1", logical, "{}")
	return nil // verified by applyAndVerify's read-back
}
