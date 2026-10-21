//go:build linux

package clipboard

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

// Linux clipboard via shell-out to the platform's standard CLI tools.
// We prefer Wayland's wl-copy/wl-paste when WAYLAND_DISPLAY is set,
// falling back to xclip then xsel for X11 / Xwayland. CGO is disabled
// in the build (the binary is static) so we cannot link libX11
// directly; shell-out is the portable alternative.
//
// Each Read/Write spawns a fresh subprocess so the credential bytes
// never live in the parent's heap longer than the syscall window. The
// subprocess inherits no environment variables we don't want it to;
// we sanitize.
//
// Caveat: if the guest has no clipboard CLI installed at all the
// agent logs the error and returns ErrUnsupported. Operators on
// minimal images can either install xclip (zero dependencies, ~30KB)
// or accept that clipboard sync is unavailable on that guest. The
// alternative (statically linking X11 / Wayland client libs) would
// blow the binary size from 1.8MB to 20MB+ and require CGO.
type linuxProvider struct {
	// Cached tool selection so we don't probe on every call.
	readCmd  []string
	writeCmd []string
}

func newProvider() (Provider, error) {
	// systemd --user environments frequently lack WAYLAND_DISPLAY / DISPLAY
	// because the compositor doesn't push them to dbus-update-activation-environment.
	// We discover the standard socket paths to ensure clipboard works outside terminals.
	uid := os.Getuid()
	runtimeDir := os.Getenv("XDG_RUNTIME_DIR")
	if runtimeDir == "" {
		runtimeDir = fmt.Sprintf("/run/user/%d", uid)
		os.Setenv("XDG_RUNTIME_DIR", runtimeDir)
	}
	if os.Getenv("WAYLAND_DISPLAY") == "" {
		for _, name := range []string{"wayland-0", "wayland-1"} {
			if _, err := os.Stat(filepath.Join(runtimeDir, name)); err == nil {
				os.Setenv("WAYLAND_DISPLAY", name)
				break
			}
		}
	}
	if os.Getenv("DISPLAY") == "" {
		for _, name := range []string{"0", "1"} {
			if _, err := os.Stat(fmt.Sprintf("/tmp/.X11-unix/X%s", name)); err == nil {
				os.Setenv("DISPLAY", ":"+name)
				break
			}
		}
	}

	wayland := os.Getenv("WAYLAND_DISPLAY") != ""
	display := os.Getenv("DISPLAY") != ""
	if !wayland && !display {
		return nil, errors.New("clipboard: no DISPLAY or WAYLAND_DISPLAY set (and discovery failed)")
	}

	// Probe in preference order. wl-copy/wl-paste on Wayland; xclip
	// then xsel on X11.
	candidates := []struct {
		need     []string
		readCmd  []string
		writeCmd []string
	}{
		{
			need:     []string{"wl-paste", "wl-copy"},
			readCmd:  []string{"wl-paste", "--no-newline", "--type", "text/plain"},
			writeCmd: []string{"wl-copy", "--type", "text/plain"},
		},
		{
			need:     []string{"xclip"},
			readCmd:  []string{"xclip", "-selection", "clipboard", "-o"},
			writeCmd: []string{"xclip", "-selection", "clipboard", "-i"},
		},
		{
			need:     []string{"xsel"},
			readCmd:  []string{"xsel", "--clipboard", "--output"},
			writeCmd: []string{"xsel", "--clipboard", "--input"},
		},
	}
	for _, c := range candidates {
		ok := true
		for _, prog := range c.need {
			if _, err := exec.LookPath(prog); err != nil {
				ok = false
				break
			}
		}
		if ok {
			return &linuxProvider{readCmd: c.readCmd, writeCmd: c.writeCmd}, nil
		}
	}
	return nil, fmt.Errorf("clipboard: no supported helper found (need wl-copy/wl-paste or xclip or xsel)")
}

func (p *linuxProvider) ReadText(ctx context.Context) ([]byte, error) {
	cmd := exec.CommandContext(ctx, p.readCmd[0], p.readCmd[1:]...)
	cmd.Env = sanitizedEnv()
	out, err := cmd.Output()
	if err != nil {
		// wl-paste exits non-zero when the clipboard is empty or
		// non-text. xclip / xsel similarly. We can't reliably
		// distinguish "empty" from "broken display" in the exit code
		// alone, so we treat all errors as ErrEmpty and let the
		// caller decide whether to log.
		return nil, fmt.Errorf("%w (%v)", ErrEmpty, err)
	}
	if len(out) == 0 {
		return nil, ErrEmpty
	}
	// Some CLIs append a trailing newline that wasn't on the
	// clipboard. Strip a single \n / \r\n.
	out = bytes.TrimRight(out, "\r\n")
	if len(out) == 0 {
		return nil, ErrEmpty
	}
	return out, nil
}

func (p *linuxProvider) WriteText(ctx context.Context, text []byte) error {
	cmd := exec.CommandContext(ctx, p.writeCmd[0], p.writeCmd[1:]...)
	cmd.Env = sanitizedEnv()
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	if _, err := stdin.Write(text); err != nil {
		_ = stdin.Close()
		_ = cmd.Wait()
		return err
	}
	if err := stdin.Close(); err != nil {
		_ = cmd.Wait()
		return err
	}
	return cmd.Wait()
}

func (p *linuxProvider) Close() error { return nil }

// sanitizedEnv returns the minimal environment the clipboard helper
// needs (DISPLAY, WAYLAND_DISPLAY, XAUTHORITY, HOME, PATH). We don't
// pass arbitrary parent env so the helper can't be coerced by an
// attacker-supplied variable like XCLIP_SELECTION to do something
// surprising.
func sanitizedEnv() []string {
	keep := []string{
		"DISPLAY",
		"WAYLAND_DISPLAY",
		"XAUTHORITY",
		"XDG_RUNTIME_DIR",
		"HOME",
		"PATH",
		"LANG",
		"LC_ALL",
	}
	out := make([]string, 0, len(keep))
	for _, k := range keep {
		if v, ok := os.LookupEnv(k); ok {
			out = append(out, k+"="+v)
		}
	}
	return out
}
