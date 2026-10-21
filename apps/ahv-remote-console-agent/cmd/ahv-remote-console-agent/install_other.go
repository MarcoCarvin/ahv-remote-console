//go:build !windows && !linux

// Stub install/uninstall for platforms we don't auto-install on
// yet. Linux equivalent will use a systemd user unit at
// ~/.config/systemd/user/hcbridge.service — landing in a follow-up
// once the Linux keystroke decoder ships. For now, return a clear
// error so an operator running --install on Linux knows why.

package main

import "errors"

func doInstall(_ string) error {
	return errors.New("--install not supported on this platform yet (Windows-only in v1)")
}

func doUninstall() error {
	return errors.New("--uninstall not supported on this platform yet (Windows-only in v1)")
}
