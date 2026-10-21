//go:build !windows && !linux

// display_other.go is the no-op stub for builds that are neither Windows
// (display_windows.go drives Win32) nor Linux (display_linux.go drives
// xrandr / the Wayland compositor tools). It exists so `go build ./...`
// is clean on developer machines (e.g. macOS) and so unit tests for
// downstream packages can compile without platform gating.
//
// Every entry point returns ErrNoMatchingMode so the bridge
// reconciler can transparently treat "not implemented" as "no fit
// available" — the resize ACK path then sends the documented
// `size=65535` sentinel to the renderer, which surfaces it as a
// "resolution not supported" indicator. That's the correct
// user-facing behaviour on a non-Windows dev machine.
//
// Mode + ErrNoMatchingMode are defined in display.go so the picker
// is testable from any platform.

package display

// EnumerateModes is a no-op on non-Windows.
func EnumerateModes() ([]Mode, error) {
	return nil, ErrNoMatchingMode
}

// ApplyMode is a no-op on non-Windows; always returns
// ErrNoMatchingMode.
func ApplyMode(_ Mode) (Mode, error) {
	return Mode{}, ErrNoMatchingMode
}

// ApplyExact is a no-op on non-Windows; always returns ErrNoMatchingMode.
func ApplyExact(_ Mode) (Mode, error) {
	return Mode{}, ErrNoMatchingMode
}

// CurrentMode is a no-op on non-Windows; always returns ErrNoMatchingMode.
func CurrentMode() (Mode, error) {
	return Mode{}, ErrNoMatchingMode
}
