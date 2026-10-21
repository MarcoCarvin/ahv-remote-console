//go:build !linux

package main

import "errors"

// runSetupMode is Linux-only: it drives the per-user systemd install through
// zenity/kdialog dialogs (see setup_linux.go). On Windows the no-argument
// invocation already IS the graphical installer.
func runSetupMode(_ flags) error {
	return errors.New("--setup is only available on Linux guests; on Windows run the agent with no arguments")
}
