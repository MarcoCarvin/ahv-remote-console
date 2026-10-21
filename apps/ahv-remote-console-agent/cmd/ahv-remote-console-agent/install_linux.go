//go:build linux

// Self-installation for Linux guests.
//
// When invoked with --install, the bridge:
//
//  1. Copies its own executable to ~/.local/bin/hcbridge.
//  2. Writes a systemd user unit at
//     ~/.config/systemd/user/hcbridge.service and enables it with
//     systemctl --user enable --now, so the bridge starts every time
//     the user logs in.
//
// Per-user install means:
//
//   - No root privileges required.
//   - Only the installing user's clipboard is bridged.
//   - Uninstall is systemctl disable + file removal.

package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"time"
)

const (
	installDirName  = ".local/bin"
	installFileName = "ahv-remote-console-agent"
	unitName        = "ahv-remote-console-agent.service"

	// Pre-rename ("hcbridge") artifacts we migrate away from on upgrade. The
	// per-user flock name is unchanged across the rename, so a still-enabled
	// old unit would keep the lock and make the new daemon exit as a no-op.
	legacyInstallFileName = "hcbridge"
	legacyUnitName        = "hcbridge.service"
)

// doInstall copies this binary to ~/.local/bin and installs a systemd user
// unit that autostarts it. res ("WxH"/"none") is the standard guest resolution
// chosen at install time; it is baked into the unit's ExecStart so the resident
// daemon re-asserts it on every login rather than the built-in default.
func doInstall(res string) error {
	src, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locate self: %w", err)
	}
	dst, err := installPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return fmt.Errorf("mkdir: %w", err)
	}

	// Idempotent upgrade path, mirroring the Windows installer: stop any
	// running agent FIRST so a re-run of the installer always takes over.
	// Without this, `enable --now` below is a no-op on an already-active
	// unit (the OLD daemon keeps running the OLD binary), and overwriting
	// an executing binary in place fails with ETXTBSY. Best-effort — a
	// stop failure surfaces as a copy/restart error with the real cause.
	stopRunningAgents()

	srcAbs, _ := filepath.Abs(src)
	dstAbs, _ := filepath.Abs(dst)
	if srcAbs != dstAbs {
		// Unlink before copying: removing the path sidesteps ETXTBSY if
		// anything is still executing the old binary — the old process keeps
		// its inode, the copy gets a fresh one.
		if err := os.Remove(dst); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove old binary: %w", err)
		}
		if err := copyFile(src, dst); err != nil {
			return fmt.Errorf("copy: %w", err)
		}
	}

	unitDir, err := unitDirPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(unitDir, 0o700); err != nil {
		return fmt.Errorf("mkdir unit dir: %w", err)
	}

	unitContent := fmt.Sprintf(`[Unit]
Description=AHV Remote Console Guest Agent
After=graphical-session.target

[Service]
Type=simple
ExecStart=%s --daemon --default-resolution=%s
Restart=on-failure
RestartSec=3

[Install]
WantedBy=default.target
`, dst, res)

	unitPath := filepath.Join(unitDir, unitName)
	if err := os.WriteFile(unitPath, []byte(unitContent), 0o644); err != nil {
		return fmt.Errorf("write unit: %w", err)
	}

	// Upgrade hygiene: stop + remove the pre-rename "hcbridge" unit and binary
	// BEFORE the reload below, so it picks up the removed unit and the new
	// daemon can take the (shared) per-user flock. Best-effort.
	removeLegacyInstall()

	// Reload so systemd picks up the new unit, then enable + RESTART (not
	// `enable --now`: --now is a start, which is a no-op when the unit is
	// already active — the freshly-copied binary would never take over).
	if out, err := exec.Command("systemctl", "--user", "daemon-reload").CombinedOutput(); err != nil {
		return fmt.Errorf("daemon-reload: %w (%s)", err, out)
	}
	if out, err := exec.Command("systemctl", "--user", "enable", unitName).CombinedOutput(); err != nil {
		return fmt.Errorf("enable: %w (%s)", err, out)
	}
	if out, err := exec.Command("systemctl", "--user", "restart", unitName).CombinedOutput(); err != nil {
		return fmt.Errorf("restart: %w (%s)", err, out)
	}

	fmt.Println("AHV Remote Console bridge installed to", dst)
	return nil
}

// stopRunningAgents terminates any resident agent running for this user,
// EXCEPT this process — the Linux mirror of the Windows installer's
// stopOtherInstances, so re-running the installer "Just Works" with no manual
// `systemctl --user stop` dance. It stops the unit-managed daemons (current +
// legacy hcbridge names) and then SIGTERMs any stray same-uid process whose
// argv[0] basename matches either binary name (a foreground --daemon run, an
// old manual install). The self-exclusion matters: the packaged --setup path
// runs doInstall from a binary with exactly this name. All best-effort — the
// new daemon's single-instance guard retries the shared flock for 5s anyway,
// so this only needs to free the lock in the common case.
func stopRunningAgents() {
	_ = exec.Command("systemctl", "--user", "stop", unitName).Run()
	_ = exec.Command("systemctl", "--user", "stop", legacyUnitName).Run()

	self := os.Getpid()
	// #nosec G115 -- os.Getuid() returns the process's real uid, a small
	// non-negative value that never overflows uint32.
	uid := uint32(os.Getuid())
	names := map[string]bool{installFileName: true, legacyInstallFileName: true}
	procs, err := os.ReadDir("/proc")
	if err != nil {
		return
	}
	var signalled []int
	for _, e := range procs {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid == self {
			continue
		}
		fi, err := os.Stat("/proc/" + e.Name())
		if err != nil {
			continue
		}
		st, ok := fi.Sys().(*syscall.Stat_t)
		if !ok || st.Uid != uid {
			continue
		}
		cmdline, err := os.ReadFile("/proc/" + e.Name() + "/cmdline")
		if err != nil || len(cmdline) == 0 {
			continue
		}
		argv0 := string(bytes.SplitN(cmdline, []byte{0}, 2)[0])
		if !names[filepath.Base(argv0)] {
			continue
		}
		if err := syscall.Kill(pid, syscall.SIGTERM); err == nil {
			signalled = append(signalled, pid)
		}
	}
	// Wait briefly for the signalled processes to exit and release the
	// per-user flock; kill(pid, 0) probes liveness without sending a signal.
	for i := 0; i < 20 && len(signalled) > 0; i++ {
		time.Sleep(100 * time.Millisecond)
		remaining := signalled[:0]
		for _, pid := range signalled {
			if syscall.Kill(pid, 0) == nil {
				remaining = append(remaining, pid)
			}
		}
		signalled = remaining
	}
}

func doUninstall() error {
	// Stop and disable the service. Best-effort — the unit may not exist.
	_ = exec.Command("systemctl", "--user", "disable", "--now", unitName).Run()
	// Also clear any leftover pre-rename "hcbridge" unit + binary.
	removeLegacyInstall()
	_ = exec.Command("systemctl", "--user", "daemon-reload").Run()

	unitDir, err := unitDirPath()
	if err == nil {
		unitPath := filepath.Join(unitDir, unitName)
		if err := os.Remove(unitPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			fmt.Fprintln(os.Stderr, "warning: remove unit:", err)
		}
	}

	dst, err := installPath()
	if err != nil {
		return err
	}
	if err := os.Remove(dst); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove binary: %w", err)
	}

	fmt.Println("AHV Remote Console bridge uninstalled")
	return nil
}

// removeLegacyInstall disables and deletes a pre-rename "hcbridge" install so an
// in-place upgrade doesn't leave two enabled user units contending for the
// shared per-user flock (the lock file name is unchanged across the rename, so
// a live old daemon would otherwise make the new one exit as a no-op). All
// best-effort: a machine that never ran hcbridge has nothing to remove.
func removeLegacyInstall() {
	_ = exec.Command("systemctl", "--user", "disable", "--now", legacyUnitName).Run()
	if unitDir, err := unitDirPath(); err == nil {
		_ = os.Remove(filepath.Join(unitDir, legacyUnitName))
	}
	if home := os.Getenv("HOME"); home != "" {
		_ = os.Remove(filepath.Join(home, installDirName, legacyInstallFileName))
	}
}

func installPath() (string, error) {
	home := os.Getenv("HOME")
	if home == "" {
		return "", errors.New("HOME environment variable empty")
	}
	return filepath.Join(home, installDirName, installFileName), nil
}

func unitDirPath() (string, error) {
	// XDG_CONFIG_HOME, defaulting to ~/.config per the XDG spec.
	dir := os.Getenv("XDG_CONFIG_HOME")
	if dir == "" {
		home := os.Getenv("HOME")
		if home == "" {
			return "", errors.New("HOME environment variable empty")
		}
		dir = filepath.Join(home, ".config")
	}
	return filepath.Join(dir, "systemd", "user"), nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o700)
	if err != nil {
		return err
	}
	defer out.Close()
	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	return nil
}
