//go:build linux

// uefivar_linux.go writes OVMF's PlatformConfig UEFI variable from a Linux
// guest via efivarfs — the exact analog of the Windows SetFirmwareEnvironment
// path (uefivar_windows.go). Same variable name, same GUID, same packed
// { UINT32 W; UINT32 H } LE payload (shared payload()/parse()), same 0x7
// NV|BS|RT attributes — so the boot resolution a Linux guest pins is
// byte-for-byte what a Windows guest pins.
//
// efivarfs contract: each variable file is [4-byte attributes LE][data], the
// attributes+data MUST be written in a single write() call, and non-standard
// variables are created immutable — so we clear the immutable inode flag
// first. Requires root (the daemon never calls this; only the elevated
// one-shot `--apply-display` does, e.g. via sudo/pkexec, mirroring Windows).
package uefivar

import (
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

// fsImmutableFL is FS_IMMUTABLE_FL from <linux/fs.h> — a stable kernel ABI
// constant that x/sys/unix exposes the FS_IOC_*FLAGS ioctls for but not the
// flag value itself.
const fsImmutableFL = 0x00000010

// efivarFilePath is the efivarfs file for PlatformConfig: "<Name>-<guid>",
// where the GUID is the bare (no-brace) lowercase form. VarGUID is stored in
// the Windows "{...}" form, so strip the braces.
func efivarFilePath() string {
	guid := strings.Trim(VarGUID, "{}")
	return filepath.Join("/sys/firmware/efi/efivars", VarName+"-"+guid)
}

// SetBootResolution writes {w,h} to PlatformConfig via efivarfs and reads it
// back to confirm the varstore persisted it. It does NOT reboot. Best-effort:
// a non-UEFI guest or a read-only varstore is reported in Result, not an error
// — matching the Windows path's PrivilegeHeld/Persisted semantics.
func SetBootResolution(w, h uint32) (Result, error) {
	// Refuse anything that isn't exactly the PlatformConfig display variable —
	// same guard as the Windows writer, so a future flag can't redirect this at
	// an arbitrary firmware variable.
	if VarName != "PlatformConfig" || VarGUID != "{7235c51c-0c80-4cab-87ac-3b084a6304b1}" || attrNVBSRT != 0x7 {
		return Result{}, fmt.Errorf("refusing firmware write: target is not the expected PlatformConfig display variable")
	}
	if len(payload(w, h)) != 8 {
		return Result{}, fmt.Errorf("refusing firmware write: unexpected payload length")
	}

	res := Result{PrivilegeHeld: os.Geteuid() == 0}

	if _, err := os.Stat("/sys/firmware/efi"); err != nil {
		res.Note = "not a UEFI guest (/sys/firmware/efi absent); boot-resolution pin skipped"
		return res, nil
	}
	res.FirmwareUEFI = true

	path := efivarFilePath()

	// Read the existing value (if any) for the before/after report.
	if b, err := os.ReadFile(path); err == nil && len(b) >= 4+8 {
		if bw, bh, ok := parse(b[4:]); ok {
			res.BeforeFound = true
			res.BeforeW, res.BeforeH = bw, bh
		}
	}

	// efivarfs payload is [attributes LE][data], written in ONE write().
	buf := make([]byte, 4+8)
	binary.LittleEndian.PutUint32(buf[0:4], uint32(attrNVBSRT))
	copy(buf[4:], payload(w, h))

	// Existing firmware variables are immutable; clear the flag before writing.
	clearImmutable(path)

	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE, 0o644)
	if err != nil {
		res.Note = "open efivar (need root / writable efivarfs): " + err.Error()
		return res, err
	}
	_, werr := f.Write(buf) // single write of attrs+data, per the efivarfs contract
	cerr := f.Close()
	if werr != nil {
		res.Note = "write efivar (read-only varstore or insufficient privilege): " + werr.Error()
		return res, werr
	}
	if cerr != nil {
		res.Note = "close efivar: " + cerr.Error()
		return res, cerr
	}
	res.Wrote = true

	// Read back to confirm the varstore actually persisted the value.
	if b, err := os.ReadFile(path); err == nil && len(b) >= 4+8 {
		if aw, ah, ok := parse(b[4:]); ok {
			res.AfterW, res.AfterH = aw, ah
			res.Persisted = aw == w && ah == h
		}
	}
	return res, nil
}

// clearImmutable drops the FS_IMMUTABLE_FL inode flag efivarfs sets on
// existing variables. Best-effort: a missing file or a kernel that rejects the
// ioctl just means the subsequent write decides the outcome.
func clearImmutable(path string) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC, 0)
	if err != nil {
		return
	}
	defer unix.Close(fd)
	flags, err := unix.IoctlGetInt(fd, unix.FS_IOC_GETFLAGS)
	if err != nil || flags&fsImmutableFL == 0 {
		return
	}
	_ = unix.IoctlSetPointerInt(fd, unix.FS_IOC_SETFLAGS, flags&^fsImmutableFL)
}
