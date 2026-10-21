// Package uefivar sets the VM's OVMF "preferred" boot resolution by writing
// the firmware's PlatformConfig UEFI variable directly from the running guest
// OS — the deterministic, menu-free alternative to the F2/OVMF "Change
// Preferred Resolution" ritual.
//
// Background: Nutanix AHV runs VMs on QEMU + OVMF (edk2). A Windows guest with
// only the Microsoft Basic Display Adapter inherits the OVMF GOP "preferred
// resolution", which OVMF reads at boot from a single non-volatile UEFI
// variable that OvmfPkg/PlatformDxe writes with attributes
// NON_VOLATILE | BOOTSERVICE_ACCESS | RUNTIME_ACCESS (0x7), non-authenticated.
// RUNTIME_ACCESS is what lets a running OS write it — so we set those 8 bytes
// and a reboot applies the new resolution to the GOP / Basic Display Adapter,
// with no firmware menu, no F2 timing race, and no hypervisor-host access.
//
// The actual SetVariable/GetVariable calls are Windows-only (uefivar_windows.go);
// every other platform gets a stub so the agent still cross-compiles.
package uefivar

import "encoding/binary"

const (
	// VarName / VarGUID identify OVMF's PlatformConfig variable
	// (OvmfPkg/PlatformDxe; gOvmfPlatformConfigGuid). The GUID string uses
	// the curly-brace + hyphen form Windows' firmware-variable API requires.
	VarName = "PlatformConfig"
	VarGUID = "{7235c51c-0c80-4cab-87ac-3b084a6304b1}"

	// attrNVBSRT = EFI_VARIABLE_NON_VOLATILE | _BOOTSERVICE_ACCESS |
	// _RUNTIME_ACCESS — the exact (non-authenticated) attribute set OVMF uses.
	// We MUST write with the same attributes the variable already has, or the
	// firmware rejects the change (EFI_INVALID_PARAMETER).
	attrNVBSRT = 0x7
)

// Result reports every step so the caller (and the operator log) can see
// exactly what happened — including the two outcomes that are NOT bugs:
// PrivilegeHeld=false (the process wasn't elevated) and Persisted=false (the
// OVMF varstore is read-only on this build, where the F2 menu would also fail).
type Result struct {
	FirmwareUEFI  bool   // GetFirmwareType == UEFI
	PrivilegeHeld bool   // SeSystemEnvironmentPrivilege enabled
	BeforeFound   bool   // PlatformConfig existed before the write
	BeforeW       uint32 // resolution read before the write
	BeforeH       uint32
	Wrote         bool   // SetVariable succeeded
	AfterW        uint32 // resolution read back after the write
	AfterH        uint32
	Persisted     bool // read-back matched what we wrote (varstore is writable)
	Note          string
}

// SetBootResolution writes {w,h} to PlatformConfig and reads it back to confirm
// the varstore persisted it. It does NOT reboot — the caller decides when. The
// per-platform implementations live in uefivar_windows.go / uefivar_other.go.

// payload encodes the PlatformConfig data: packed { UINT32 H; UINT32 V } LE.
func payload(w, h uint32) []byte {
	b := make([]byte, 8)
	binary.LittleEndian.PutUint32(b[0:4], w)
	binary.LittleEndian.PutUint32(b[4:8], h)
	return b
}

// parse decodes 8 PlatformConfig bytes back into (width, height).
func parse(b []byte) (w, h uint32, ok bool) {
	if len(b) < 8 {
		return 0, 0, false
	}
	return binary.LittleEndian.Uint32(b[0:4]), binary.LittleEndian.Uint32(b[4:8]), true
}
