// Package display implements display-mode enumeration and dynamic
// resolution changes on the guest (agent) side. The Windows build
// drives Win32 EnumDisplaySettingsExW / ChangeDisplaySettingsExW
// against the primary monitor; the non-Windows build is a no-op stub
// so the agent still compiles cross-platform.
//
// This file holds the platform-independent surface: the Mode type,
// the ErrNoMatchingMode sentinel, and the pure-function best-fit
// picker. Splitting the picker out means it's exercisable on any
// developer machine via display_test.go without dragging in Win32.

package display

import "errors"

// Mode is a single display mode. Width/Height are the only fields
// the picker compares; Bits / Hz are informational so the caller can
// log what was actually applied.
type Mode struct {
	Width  int
	Height int
	Bits   int // bits per pixel; informational
	Hz     int // refresh rate; informational
}

// ErrNoMatchingMode is returned by PickBestMode (and surfaced via
// ApplyMode on Windows / the no-op stubs elsewhere) when no supported
// mode fits the requested size on either axis. On non-Windows builds
// every entry point returns this so the bridge reconciler can treat
// "not implemented" the same as "no fit available" and ACK the
// renderer with the documented `size=65535` failed-sentinel.
var ErrNoMatchingMode = errors.New("display: no supported mode for the requested size")

// ErrRestartRequired is returned by ApplyExact (Windows only) when
// ChangeDisplaySettingsExW returns DISP_CHANGE_RESTART (1): the driver
// accepted the new resolution and wrote it to the registry, but it
// cannot take effect until the guest reboots. Callers that manage the
// daemon startup pin (applyDefaultResolution) must trigger a reboot
// when they receive this error.
var ErrRestartRequired = errors.New("display: mode change accepted but requires a restart to take effect")

// PickBestMode picks the supported mode that best fits the requested
// (width, height). Exposed so the bridge tests can drive it with a
// synthetic mode list — production code reaches it through ApplyMode.
//
// Rules (in priority order):
//
//  1. Exact (width, height) match wins.
//  2. Otherwise, among modes ≤ requested on BOTH axes, pick the one
//     with the smallest Manhattan distance |dw| + |dh|. Ties on
//     distance: prefer the larger area (higher pixel count) so the
//     operator gets the most screen real estate that still fits.
//  3. If no mode is ≤ on both axes, fall back to the smallest mode
//     that is ≥ requested on BOTH axes — by Manhattan distance, with
//     the same tie-breaker.
//  4. If neither cohort has any member, return ErrNoMatchingMode.
//
// Width and height of the requested mode must both be positive; a
// non-positive request short-circuits to ErrNoMatchingMode (no point
// trying to fit a zero-size window).
func PickBestMode(modes []Mode, requested Mode) (Mode, error) {
	if len(modes) == 0 {
		return Mode{}, ErrNoMatchingMode
	}
	if requested.Width <= 0 || requested.Height <= 0 {
		return Mode{}, ErrNoMatchingMode
	}

	// Rule 1: exact match.
	for _, m := range modes {
		if m.Width == requested.Width && m.Height == requested.Height {
			return m, nil
		}
	}

	abs := func(x int) int {
		if x < 0 {
			return -x
		}
		return x
	}

	type scored struct {
		mode Mode
		dist int // |dw| + |dh|
		area int // w*h, used as tie-breaker
	}

	var below []scored // modes ≤ requested on BOTH axes
	var above []scored // modes ≥ requested on BOTH axes
	for _, m := range modes {
		dw := requested.Width - m.Width
		dh := requested.Height - m.Height
		s := scored{
			mode: m,
			dist: abs(dw) + abs(dh),
			area: m.Width * m.Height,
		}
		switch {
		case m.Width <= requested.Width && m.Height <= requested.Height:
			below = append(below, s)
		case m.Width >= requested.Width && m.Height >= requested.Height:
			above = append(above, s)
		}
	}

	// pickBest returns the entry with the smallest distance, breaking
	// ties by larger area (more screen real estate).
	pickBest := func(in []scored) Mode {
		best := in[0]
		for _, s := range in[1:] {
			if s.dist < best.dist {
				best = s
				continue
			}
			if s.dist == best.dist && s.area > best.area {
				best = s
			}
		}
		return best.mode
	}

	if len(below) > 0 {
		return pickBest(below), nil
	}
	if len(above) > 0 {
		return pickBest(above), nil
	}
	return Mode{}, ErrNoMatchingMode
}

// pickLargest selects the mode with the greatest pixel count (area), preferring
// the wider mode on an area tie. Pure and platform-independent, so it is unit-
// tested directly. "Largest" = most pixels: on a VM whose virtual GPU caps the
// advertised modes (e.g. no 1920x1080), this picks the biggest mode that IS
// offered rather than forcing an unavailable target.
func pickLargest(modes []Mode) (Mode, error) {
	best := Mode{}
	found := false
	for _, m := range modes {
		if m.Width <= 0 || m.Height <= 0 {
			continue
		}
		if !found {
			best, found = m, true
			continue
		}
		a, ba := m.Width*m.Height, best.Width*best.Height
		if a > ba || (a == ba && m.Width > best.Width) {
			best = m
		}
	}
	if !found {
		return Mode{}, ErrNoMatchingMode
	}
	return best, nil
}

// LargestMode returns the largest supported mode (see pickLargest). It composes
// the platform EnumerateModes, so it behaves identically on Windows (Win32
// EnumDisplaySettings) and Linux (xrandr / the Wayland compositor). Returns
// ErrNoMatchingMode when nothing is enumerable (e.g. the no-op stub platform).
func LargestMode() (Mode, error) {
	modes, err := EnumerateModes()
	if err != nil {
		return Mode{}, err
	}
	return pickLargest(modes)
}
