//go:build !windows

package main

import "log/slog"

// runDefault is the no-argument behaviour off Windows. There is no graphical
// installer (the WebView2 GUI is Windows-only), so the bare invocation runs as
// the resident daemon. Linux guests install via the documented manual path
// (see docs/operations/install.md) or --install.
func runDefault(logger *slog.Logger, f flags) error {
	return runDaemon(logger, f)
}

// scheduleReboot is a no-op on non-Windows: the daemon only triggers a reboot
// when ChangeDisplaySettingsExW returns DISP_CHANGE_RESTART, which only
// happens on Windows. On Linux xrandr/gdbus apply the mode change live.
func scheduleReboot(_ string) {}
