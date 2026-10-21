//go:build !windows

// op_other.go is the documented no-op for every non-Windows GOOS.
//
// Linux: the agent is a static CGO_ENABLED=0 binary and the repo's only X11
// path is shelling out to xrandr (display_linux.go) — there is no CGO-free
// in-process X client to borrow for a 1x1 override-redirect window, and
// spawning a helper process at the 25ms duty cycle (or adding a pure-Go X11
// protocol dependency just for one pixel) is not worth it. On Wayland a
// foreign client can't draw an unmanaged surface at all. So Linux guests get
// no keep-warm: the lease arms, newPlatformOp fails, and the Lease latches
// itself unsupported — the console works exactly as before, just without the
// cadence floor.
//
// Other GOOS (darwin dev builds): same no-op; AHV guests are Windows/Linux.

package keepwarm

import "errors"

// newPlatformOp reports that no display dirty-op exists on this platform.
func newPlatformOp() (op, error) {
	return nil, errors.ErrUnsupported
}
