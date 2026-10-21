//go:build !windows

// cursor_other.go is the documented no-op for the guest cursor takeover on
// every non-Windows GOOS.
//
// Linux: hiding the pointer would have to go through the running display
// server (XFixes hide-cursor on X11, a compositor protocol on Wayland), and
// as with the dirty op (op_other.go) there is no CGO-free in-process client
// to borrow — so Linux guests keep their visible pointer and the lease runs
// whatever ops the platform does support. Other GOOS (darwin dev builds):
// same no-op; AHV guests are Windows/Linux.

package keepwarm

import "errors"

// newPlatformCursorOp reports that no cursor takeover exists on this
// platform.
func newPlatformCursorOp() (op, error) {
	return nil, errors.ErrUnsupported
}

// restoreDefaultCursors is the startup heal for a stale Windows takeover;
// no other platform hides cursors, so there is nothing to restore.
func restoreDefaultCursors() {}
