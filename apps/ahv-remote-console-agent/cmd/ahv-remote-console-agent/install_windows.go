//go:build windows

// Self-installation for Windows guests.
//
// When invoked with --install, the bridge:
//
//   1. Copies its own executable to
//      %LOCALAPPDATA%\AHV Remote Console\hcbridge.exe.
//   2. Writes a HKCU\Software\Microsoft\Windows\CurrentVersion\Run
//      registry value pointing at the copied binary, so the bridge
//      starts every time the user logs in.
//   3. Optionally spawns the freshly-installed bridge detached so
//      the operator's current console session has clipboard sync
//      immediately (rather than waiting for next login).
//
// HKCU (current user) install means:
//
//   * No admin privileges required.
//   * Only the installing user's clipboard is bridged — which is
//     what we want (security: a hostile second user logged into
//     the same VM doesn't intercept the operator's clipboard).
//   * Uninstall is a single registry-delete + file-delete.

package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"unsafe"
)

const (
	hkcuRunSubKey   = `Software\Microsoft\Windows\CurrentVersion\Run`
	registryValName = "AHV Remote Console Guest Agent"
	installSubDir   = "AHV Remote Console"
	installFileName = "ahv-remote-console-agent.exe"

	// Pre-rename ("hcbridge") artifacts we migrate away from on upgrade. The
	// install dir is unchanged across the rename, so the old binary sits next
	// to the new one; the HKCU\Run value name changed, so the old autostart
	// entry would otherwise linger forever. See removeLegacyInstall.
	legacyRegistryValName = "AHVRemoteConsoleBridge"
	legacyInstallFileName = "hcbridge.exe"
)

// doInstall copies this binary to the per-user install dir and registers it for
// autostart. res ("WxH", or "none") is the standard guest resolution chosen at
// install time; it is baked into both the HKCU\Run autostart command and the
// immediate detached start so every future daemon launch re-asserts the SAME
// resolution as the boot-time OVMF write — one source of truth, not the
// built-in default.
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
	// Idempotent upgrade path. If a previous agent daemon is
	// running, it holds the destination file open and a plain
	// os.OpenFile(dst, O_CREATE|O_WRONLY|O_TRUNC) blows up with
	// ERROR_SHARING_VIOLATION. Stop any sibling instances FIRST,
	// then copy. This is what makes re-running the installer
	// (operator re-mounts the ISO and runs the agent a second
	// time) Just Work — VMware-Tools-style "double-click, done"
	// with no manual Task Manager dance.
	if !samePath(src, dst) {
		if err := stopOtherInstances(); err != nil {
			fmt.Fprintln(os.Stderr, "warning: could not stop existing agent instance:", err)
		}
		if err := copyFile(src, dst); err != nil {
			return fmt.Errorf("copy: %w", err)
		}
	}
	if err := registerHKCURun(dst, res); err != nil {
		return fmt.Errorf("register run-key: %w", err)
	}
	// Upgrade hygiene: drop the pre-rename "hcbridge" autostart entry + binary
	// so login doesn't fire a now-stale second daemon. Best-effort.
	removeLegacyInstall()
	// Start the bridge immediately so the operator's current session
	// has clipboard sync without waiting for next login. Detached so
	// closing the installer terminal doesn't kill the bridge.
	if err := startDetached(dst, res); err != nil {
		// Non-fatal — install succeeded; bridge will start on next login.
		fmt.Fprintln(os.Stderr, "warning: failed to start bridge immediately:", err)
	}
	fmt.Println("AHV Remote Console bridge installed to", dst)
	return nil
}

// stopOtherInstances terminates any resident agent processes running
// under the current Windows session, EXCEPT this one. It matches both
// the current image name (ahv-remote-console-agent) AND the legacy
// pre-rename one (hcbridge): the per-user mutex name is shared across
// the rename, so a still-running old daemon would otherwise keep the
// guard and make the freshly-installed --daemon exit as a silent
// no-op. Used by --install so the destination file isn't held open
// during the copy, and so the upgrade actually takes effect.
//
// PowerShell is the cleanest Win32 path for this — Get-Process +
// Stop-Process is one-shot and matches by image name. We exclude
// the current PID so the installer doesn't terminate itself while
// running from the CD-ROM. taskkill would also work but PowerShell
// gives us the PID-exclude predicate in a single command.
//
// Best-effort: errors are logged but not propagated. If a stop
// fails (e.g. AV blocked it), the subsequent copy still tries
// — it'll fail loudly with the actual sharing-violation error.
func stopOtherInstances() error {
	myPID := os.Getpid()
	cmd := exec.Command("powershell", "-NoProfile", "-NonInteractive", "-Command",
		fmt.Sprintf(
			`$ErrorActionPreference='Stop';`+
				`$p = Get-Process -Name ahv-remote-console-agent,hcbridge -ErrorAction SilentlyContinue | Where-Object {$_.Id -ne %d};`+
				`if ($p) { $p | Stop-Process -Force; Start-Sleep -Milliseconds 600 }`,
			myPID,
		))
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("powershell stop-process: %w (%s)", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func doUninstall() error {
	dst, err := installPath()
	if err != nil {
		return err
	}
	if err := unregisterHKCURun(); err != nil {
		fmt.Fprintln(os.Stderr, "warning: failed to remove run-key:", err)
	}
	if err := os.Remove(dst); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove binary: %w", err)
	}
	// Also clear any leftover pre-rename "hcbridge" autostart entry + binary so
	// uninstall leaves nothing behind, even on a machine that was upgraded.
	removeLegacyInstall()
	fmt.Println("AHV Remote Console bridge uninstalled")
	return nil
}

func installPath() (string, error) {
	base := os.Getenv("LOCALAPPDATA")
	if base == "" {
		return "", errors.New("LOCALAPPDATA environment variable empty")
	}
	return filepath.Join(base, installSubDir, installFileName), nil
}

func samePath(a, b string) bool {
	aa, _ := filepath.Abs(a)
	bb, _ := filepath.Abs(b)
	return aa == bb
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

// startDetached spawns the bridge so it survives the parent
// process exiting. On Windows we use CreateProcess with
// DETACHED_PROCESS so no console window flashes for the operator.
// res ("WxH"/"none") is forwarded so this immediate start applies the same
// standard resolution the HKCU\Run autostart entry will on later logins.
func startDetached(bin, res string) error {
	cmd := exec.Command(bin, "--daemon", "--default-resolution="+res)
	cmd.SysProcAttr = &syscall.SysProcAttr{
		HideWindow:    true,
		CreationFlags: 0x00000008 | 0x00000200, // DETACHED_PROCESS | CREATE_NEW_PROCESS_GROUP
	}
	return cmd.Start()
}

// Registry helpers — we use advapi32 directly to avoid pulling in
// golang.org/x/sys/windows/registry as a dependency for two calls.
var (
	modAdvapi32        = syscall.NewLazyDLL("advapi32.dll")
	procRegCreateKeyEx = modAdvapi32.NewProc("RegCreateKeyExW")
	procRegSetValueEx  = modAdvapi32.NewProc("RegSetValueExW")
	procRegDeleteValue = modAdvapi32.NewProc("RegDeleteValueW")
	procRegCloseKey    = modAdvapi32.NewProc("RegCloseKey")
	procRegOpenKeyEx   = modAdvapi32.NewProc("RegOpenKeyExW")
)

const (
	hkeyCurrentUser   = 0x80000001
	keyAllAccess      = 0x000F003F
	keySetValue       = 0x00000002
	regOpenedExisting = 0
	regCreatedNewKey  = 1
	regSz             = 1
)

func registerHKCURun(exePath, res string) error {
	subKeyPtr, err := syscall.UTF16PtrFromString(hkcuRunSubKey)
	if err != nil {
		return err
	}
	var hKey syscall.Handle
	var disposition uint32
	ret, _, _ := procRegCreateKeyEx.Call(
		uintptr(hkeyCurrentUser),
		uintptr(unsafe.Pointer(subKeyPtr)),
		0,
		0,
		0,
		uintptr(keyAllAccess),
		0,
		uintptr(unsafe.Pointer(&hKey)),
		uintptr(unsafe.Pointer(&disposition)),
	)
	if ret != 0 {
		return fmt.Errorf("RegCreateKeyExW: %d", ret)
	}
	defer procRegCloseKey.Call(uintptr(hKey))

	namePtr, err := syscall.UTF16PtrFromString(registryValName)
	if err != nil {
		return err
	}
	// Launch the resident agent mode at login (the bare binary's no-arg mode is
	// the GUI installer, so the Run entry must pass --daemon). Carry the chosen
	// standard resolution so every login re-asserts it (res is a "WxH"/"none"
	// token with no spaces, so it needs no extra quoting inside the value).
	runCmd := `"` + exePath + `" --daemon --default-resolution=` + res
	// UTF16FromString returns the encoded code units INCLUDING the terminating
	// NUL, so len(units)*2 is the exact cbData (in BYTES) RegSetValueExW wants.
	// len(runCmd) is the UTF-8 byte count — for a non-ASCII install path a
	// multi-byte UTF-8 rune is a single UTF-16 unit, so that would over-report
	// cbData and read past the buffer.
	runUTF16, err := syscall.UTF16FromString(runCmd)
	if err != nil {
		return err
	}
	ret, _, _ = procRegSetValueEx.Call(
		uintptr(hKey),
		uintptr(unsafe.Pointer(namePtr)),
		0,
		uintptr(regSz),
		uintptr(unsafe.Pointer(&runUTF16[0])),
		uintptr(len(runUTF16)*2),
	)
	if ret != 0 {
		return fmt.Errorf("RegSetValueExW: %d", ret)
	}
	return nil
}

// removeLegacyInstall clears artifacts from pre-rename "hcbridge" builds so an
// in-place upgrade doesn't leave a second HKCU\Run entry pointing at a stale
// binary (and so uninstall leaves nothing behind). The shared mutex means the
// old daemon is already stopped by stopOtherInstances; here we drop its run-key
// value and its binary in the install dir. All best-effort — a clean machine
// that never ran hcbridge finds nothing and stays quiet.
func removeLegacyInstall() {
	if err := deleteRunValue(legacyRegistryValName); err != nil {
		fmt.Fprintln(os.Stderr, "warning: remove legacy run-key:", err)
	}
	if dst, err := installPath(); err == nil {
		legacy := filepath.Join(filepath.Dir(dst), legacyInstallFileName)
		if err := os.Remove(legacy); err != nil && !errors.Is(err, os.ErrNotExist) {
			fmt.Fprintln(os.Stderr, "warning: remove legacy binary:", err)
		}
	}
}

func unregisterHKCURun() error {
	return deleteRunValue(registryValName)
}

// deleteRunValue removes a single value from HKCU\...\Run. A missing value is
// not an error (ERROR_FILE_NOT_FOUND is ignored), so it's safe both for
// uninstall and the best-effort legacy-name cleanup.
func deleteRunValue(name string) error {
	subKeyPtr, err := syscall.UTF16PtrFromString(hkcuRunSubKey)
	if err != nil {
		return err
	}
	var hKey syscall.Handle
	ret, _, _ := procRegOpenKeyEx.Call(
		uintptr(hkeyCurrentUser),
		uintptr(unsafe.Pointer(subKeyPtr)),
		0,
		uintptr(keySetValue),
		uintptr(unsafe.Pointer(&hKey)),
	)
	if ret != 0 {
		return fmt.Errorf("RegOpenKeyExW: %d", ret)
	}
	defer procRegCloseKey.Call(uintptr(hKey))

	namePtr, err := syscall.UTF16PtrFromString(name)
	if err != nil {
		return err
	}
	ret, _, _ = procRegDeleteValue.Call(uintptr(hKey), uintptr(unsafe.Pointer(namePtr)))
	if ret != 0 && ret != 2 /* ERROR_FILE_NOT_FOUND */ {
		return fmt.Errorf("RegDeleteValueW: %d", ret)
	}
	return nil
}
