//go:build windows

// uefivar_windows.go is the Win32 implementation: it enables
// SeSystemEnvironmentPrivilege, confirms the VM booted UEFI, then uses
// Get/SetFirmwareEnvironmentVariableExW to read, write and verify OVMF's
// PlatformConfig variable. Raw syscall.NewLazyDLL (matching display_windows.go)
// so the agent has no extra deps.

package uefivar

import (
	"fmt"
	"syscall"
	"unsafe"
)

var (
	modKernel32 = syscall.NewLazyDLL("kernel32.dll")
	modAdvapi32 = syscall.NewLazyDLL("advapi32.dll")

	procGetFirmwareType   = modKernel32.NewProc("GetFirmwareType")
	procGetFirmwareEnvVar = modKernel32.NewProc("GetFirmwareEnvironmentVariableExW")
	procSetFirmwareEnvVar = modKernel32.NewProc("SetFirmwareEnvironmentVariableExW")
	procGetCurrentProcess = modKernel32.NewProc("GetCurrentProcess")
	procOpenProcessToken  = modAdvapi32.NewProc("OpenProcessToken")
	procLookupPrivilege   = modAdvapi32.NewProc("LookupPrivilegeValueW")
	procAdjustTokenPrivs  = modAdvapi32.NewProc("AdjustTokenPrivileges")
)

const (
	firmwareTypeUefi      = 2      // FIRMWARE_TYPE.FirmwareTypeUefi
	tokenAdjustPrivileges = 0x0020 // TOKEN_ADJUST_PRIVILEGES
	tokenQuery            = 0x0008 // TOKEN_QUERY
	sePrivilegeEnabled    = 0x0002 // SE_PRIVILEGE_ENABLED
	errNotAllAssigned     = 1300   // ERROR_NOT_ALL_ASSIGNED
	errEnvVarNotFound     = 203    // ERROR_ENVVAR_NOT_FOUND
)

type luid struct {
	lowPart  uint32
	highPart int32
}
type luidAndAttributes struct {
	luid       luid
	attributes uint32
}
type tokenPrivileges struct {
	privilegeCount uint32
	privileges     [1]luidAndAttributes
}

// setSystemEnvironmentPrivilege enables (or disables) SeSystemEnvironmentPrivilege
// on the current process token. Scoping the privilege to the single SetVariable
// call (enable → write → disable) keeps this rare, high-signal privilege live
// for the minimum window — defense-in-depth and lower EDR/behavioral footprint
// than holding it for the whole process. On enable it returns a clear error if
// the process is not elevated (the token doesn't hold the privilege).
func setSystemEnvironmentPrivilege(enable bool) error {
	cur, _, _ := procGetCurrentProcess.Call()
	var tok syscall.Handle
	if r1, _, e := procOpenProcessToken.Call(cur, uintptr(tokenAdjustPrivileges|tokenQuery), uintptr(unsafe.Pointer(&tok))); r1 == 0 {
		return fmt.Errorf("OpenProcessToken: %v", e)
	}
	defer syscall.CloseHandle(tok)

	name, err := syscall.UTF16PtrFromString("SeSystemEnvironmentPrivilege")
	if err != nil {
		return err
	}
	var lu luid
	if r1, _, e := procLookupPrivilege.Call(0, uintptr(unsafe.Pointer(name)), uintptr(unsafe.Pointer(&lu))); r1 == 0 {
		return fmt.Errorf("LookupPrivilegeValue: %v", e)
	}
	var attr uint32 // 0 = disable; SE_PRIVILEGE_ENABLED = enable
	if enable {
		attr = sePrivilegeEnabled
	}
	tp := tokenPrivileges{privilegeCount: 1}
	tp.privileges[0] = luidAndAttributes{luid: lu, attributes: attr}
	r1, _, e := procAdjustTokenPrivs.Call(uintptr(tok), 0, uintptr(unsafe.Pointer(&tp)), 0, 0, 0)
	if r1 == 0 {
		return fmt.Errorf("AdjustTokenPrivileges: %v", e)
	}
	// AdjustTokenPrivileges returns TRUE even on PARTIAL failure — the real
	// signal is GetLastError()==ERROR_NOT_ALL_ASSIGNED, meaning the token
	// doesn't hold the privilege (process not elevated).
	if enable {
		if errno, ok := e.(syscall.Errno); ok && errno == errNotAllAssigned {
			return fmt.Errorf("SeSystemEnvironmentPrivilege not held — run this elevated (Administrator/SYSTEM)")
		}
	}
	return nil
}

func readVar() (w, h uint32, found bool, err error) {
	name, err := syscall.UTF16PtrFromString(VarName)
	if err != nil {
		return 0, 0, false, err
	}
	guid, err := syscall.UTF16PtrFromString(VarGUID)
	if err != nil {
		return 0, 0, false, err
	}
	buf := make([]byte, 8)
	var attrs uint32
	r1, _, e := procGetFirmwareEnvVar.Call(
		uintptr(unsafe.Pointer(name)), uintptr(unsafe.Pointer(guid)),
		uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)), uintptr(unsafe.Pointer(&attrs)))
	if n := uint32(r1); n == 0 {
		if errno, ok := e.(syscall.Errno); ok && errno == errEnvVarNotFound {
			return 0, 0, false, nil // not set yet — expected on a fresh VM
		}
		return 0, 0, false, nil // unreadable; the write below is the real test
	}
	w, h, ok := parse(buf)
	return w, h, ok, nil
}

func writeVar(w, h uint32) error {
	// Hard guard: this primitive may write ONLY the non-authenticated OVMF
	// display-config variable. It must never become a path to a boot/Secure-Boot
	// variable, even if a future flag exposes the name/GUID — so we refuse
	// anything that is not exactly PlatformConfig / its GUID / attrs 0x7 / 8 bytes.
	if VarName != "PlatformConfig" || VarGUID != "{7235c51c-0c80-4cab-87ac-3b084a6304b1}" || attrNVBSRT != 0x7 {
		return fmt.Errorf("refusing firmware write: target is not the expected PlatformConfig display variable")
	}
	if len(payload(w, h)) != 8 {
		return fmt.Errorf("refusing firmware write: unexpected payload length")
	}
	name, err := syscall.UTF16PtrFromString(VarName)
	if err != nil {
		return err
	}
	guid, err := syscall.UTF16PtrFromString(VarGUID)
	if err != nil {
		return err
	}
	buf := payload(w, h)
	r1, _, e := procSetFirmwareEnvVar.Call(
		uintptr(unsafe.Pointer(name)), uintptr(unsafe.Pointer(guid)),
		uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)), uintptr(attrNVBSRT))
	if r1 == 0 {
		return fmt.Errorf("SetFirmwareEnvironmentVariableExW: %v", e)
	}
	return nil
}

// SetBootResolution writes {w,h} to OVMF's PlatformConfig and verifies the
// read-back. It does NOT reboot. See package doc for the mechanism.
func SetBootResolution(w, h uint32) (Result, error) {
	var res Result

	var ft uint32
	if r1, _, e := procGetFirmwareType.Call(uintptr(unsafe.Pointer(&ft))); r1 == 0 {
		return res, fmt.Errorf("GetFirmwareType failed (%v) — likely a legacy-BIOS VM, where the OVMF preferred-resolution variable does not exist", e)
	}
	res.FirmwareUEFI = ft == firmwareTypeUefi
	if !res.FirmwareUEFI {
		return res, fmt.Errorf("VM firmware is not UEFI (type %d) — the OVMF preferred-resolution variable does not apply", ft)
	}

	// Scope SeSystemEnvironmentPrivilege to the firmware I/O below and drop it
	// immediately after — minimum window for this rare, high-signal privilege.
	if err := setSystemEnvironmentPrivilege(true); err != nil {
		return res, err
	}
	res.PrivilegeHeld = true
	defer func() { _ = setSystemEnvironmentPrivilege(false) }()

	res.BeforeW, res.BeforeH, res.BeforeFound, _ = readVar()

	if err := writeVar(w, h); err != nil {
		return res, err
	}
	res.Wrote = true

	aw, ah, afound, _ := readVar()
	res.AfterW, res.AfterH = aw, ah
	res.Persisted = afound && aw == w && ah == h
	if res.Persisted {
		res.Note = "ok — reboot the VM to apply the new boot resolution"
	} else {
		res.Note = "write was accepted but the read-back did not match — OVMF's varstore is likely READ-ONLY on this AHV build (the F2 menu would also fail to persist here)"
		// Don't leave a read-only/odd varstore half-changed: restore the prior value.
		if res.BeforeFound {
			if err := writeVar(res.BeforeW, res.BeforeH); err == nil {
				res.Note += fmt.Sprintf(" — reverted to prior %dx%d", res.BeforeW, res.BeforeH)
			}
		}
	}
	return res, nil
}
