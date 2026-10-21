//go:build !windows && !linux

// uefivar_other.go is the stub for builds that are neither Windows
// (uefivar_windows.go drives SetFirmwareEnvironmentVariableExW) nor Linux
// (uefivar_linux.go writes efivarfs). It keeps the agent cross-compilable
// on dev machines (e.g. macOS).

package uefivar

import "errors"

// SetBootResolution is unsupported on this platform.
func SetBootResolution(_, _ uint32) (Result, error) {
	// Reference payload/parse so the shared file's helpers aren't flagged
	// unused on non-Windows builds, and the encoding stays exercised.
	_ = payload
	_ = parse
	return Result{}, errors.New("uefivar: setting the OVMF boot resolution is only supported on Windows guests")
}
