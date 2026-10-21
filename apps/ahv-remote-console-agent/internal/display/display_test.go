// display_test.go exercises the platform-independent best-fit
// picker. PickBestMode is the pure function the Windows ApplyMode
// path uses to choose which driver-supported mode to commit to; the
// rest of ApplyMode is Win32 plumbing that can only run on a real
// Windows desktop.
//
// Test cases follow the rules documented on PickBestMode:
//
//  1. Exact match wins.
//  2. Among modes ≤ requested on both axes, smallest Manhattan
//     distance wins.
//  3. If no mode is ≤ on both axes, smallest mode ≥ requested wins.
//  4. Empty mode list → ErrNoMatchingMode.
//  5. Equal Manhattan distance → larger area wins (more screen real
//     estate is the user-visible preference).
//
// Tests live in display_test.go (no _windows suffix, no build tag)
// because they don't touch Win32; the picker is package-private to
// display but exported as PickBestMode for testability.

package display

import (
	"errors"
	"testing"
)

func TestPickBestMode_exactMatch(t *testing.T) {
	modes := []Mode{
		{Width: 1024, Height: 768},
		{Width: 1280, Height: 720},
		{Width: 1920, Height: 1080},
	}
	got, err := PickBestMode(modes, Mode{Width: 1280, Height: 720})
	if err != nil {
		t.Fatalf("PickBestMode: %v", err)
	}
	if got.Width != 1280 || got.Height != 720 {
		t.Errorf("got %dx%d; want 1280x720", got.Width, got.Height)
	}
}

func TestPickBestMode_belowOrEqualPreferred(t *testing.T) {
	// Requested 1440x900. Below-or-equal candidates: 1366x768, 1280x720.
	// Above candidate: 1600x900. We must pick 1366x768 (smallest
	// Manhattan distance among the "below" cohort, 74+132 = 206) even
	// though 1600x900 is closer in absolute terms (160+0 = 160).
	modes := []Mode{
		{Width: 800, Height: 600},
		{Width: 1280, Height: 720},
		{Width: 1366, Height: 768},
		{Width: 1600, Height: 900},
	}
	got, err := PickBestMode(modes, Mode{Width: 1440, Height: 900})
	if err != nil {
		t.Fatalf("PickBestMode: %v", err)
	}
	if got.Width != 1366 || got.Height != 768 {
		t.Errorf("got %dx%d; want 1366x768 (below-or-equal preferred)", got.Width, got.Height)
	}
}

func TestPickBestMode_smallestAboveWhenNoBelow(t *testing.T) {
	// Requested 800x600. Every supported mode is strictly larger on
	// both axes — picker must fall back to the smallest "above" mode.
	modes := []Mode{
		{Width: 1920, Height: 1080},
		{Width: 1280, Height: 720},
		{Width: 1366, Height: 768},
	}
	got, err := PickBestMode(modes, Mode{Width: 800, Height: 600})
	if err != nil {
		t.Fatalf("PickBestMode: %v", err)
	}
	// Among (1280x720, 1366x768, 1920x1080), 1280x720 has the smallest
	// Manhattan distance to 800x600 (480 + 120 = 600).
	if got.Width != 1280 || got.Height != 720 {
		t.Errorf("got %dx%d; want 1280x720 (smallest above)", got.Width, got.Height)
	}
}

func TestPickBestMode_emptyModes(t *testing.T) {
	if _, err := PickBestMode(nil, Mode{Width: 1024, Height: 768}); !errors.Is(err, ErrNoMatchingMode) {
		t.Errorf("PickBestMode(nil): err = %v; want ErrNoMatchingMode", err)
	}
	if _, err := PickBestMode([]Mode{}, Mode{Width: 1024, Height: 768}); !errors.Is(err, ErrNoMatchingMode) {
		t.Errorf("PickBestMode([]): err = %v; want ErrNoMatchingMode", err)
	}
}

func TestPickBestMode_invalidRequested(t *testing.T) {
	modes := []Mode{{Width: 1024, Height: 768}}
	if _, err := PickBestMode(modes, Mode{Width: 0, Height: 768}); !errors.Is(err, ErrNoMatchingMode) {
		t.Errorf("PickBestMode(width=0): err = %v; want ErrNoMatchingMode", err)
	}
	if _, err := PickBestMode(modes, Mode{Width: 1024, Height: 0}); !errors.Is(err, ErrNoMatchingMode) {
		t.Errorf("PickBestMode(height=0): err = %v; want ErrNoMatchingMode", err)
	}
	if _, err := PickBestMode(modes, Mode{Width: -1, Height: -1}); !errors.Is(err, ErrNoMatchingMode) {
		t.Errorf("PickBestMode(negative): err = %v; want ErrNoMatchingMode", err)
	}
}

// TestPickBestMode_areaTieBreaker — when two below-or-equal modes have
// identical Manhattan distance, the larger-area mode wins. This
// reflects the user-visible preference: prefer the mode that uses
// more of the operator's window.
func TestPickBestMode_areaTieBreaker(t *testing.T) {
	// Requested 1200x900. Candidates:
	//   1000x900 → distance 200, area 900000
	//   1200x700 → distance 200, area 840000
	// Both below-or-equal, equal distance; picker must choose
	// 1000x900 (larger area).
	modes := []Mode{
		{Width: 1000, Height: 900},
		{Width: 1200, Height: 700},
	}
	got, err := PickBestMode(modes, Mode{Width: 1200, Height: 900})
	if err != nil {
		t.Fatalf("PickBestMode: %v", err)
	}
	if got.Width != 1000 || got.Height != 900 {
		t.Errorf("got %dx%d; want 1000x900 (area tie-breaker prefers larger area)", got.Width, got.Height)
	}
}

// TestPickBestMode_mixedHeightWidthDoesntPreventBelowMatch — a mode
// that's larger on one axis and smaller on the other belongs to
// neither the "below" nor the "above" cohort; it should be ignored
// in favour of a below-or-equal match if one exists.
func TestPickBestMode_mixedDimensionsIgnoredWhenBelowExists(t *testing.T) {
	// Requested 1280x1024. Modes:
	//   1024x768 → below, distance 512
	//   1600x768 → mixed (wider, shorter) → not in either cohort
	//   1280x800 → below, distance 224 ← winner
	modes := []Mode{
		{Width: 1024, Height: 768},
		{Width: 1600, Height: 768},
		{Width: 1280, Height: 800},
	}
	got, err := PickBestMode(modes, Mode{Width: 1280, Height: 1024})
	if err != nil {
		t.Fatalf("PickBestMode: %v", err)
	}
	if got.Width != 1280 || got.Height != 800 {
		t.Errorf("got %dx%d; want 1280x800", got.Width, got.Height)
	}
}

// TestPickBestMode_mixedFallsBackToAbove — same mixed-dimensions
// scenario, but with no below-or-equal candidates: above cohort
// wins, mixed candidates are still skipped.
func TestPickBestMode_mixedDimensionsSkippedInAboveFallback(t *testing.T) {
	// Requested 1280x1024. Modes:
	//   1600x768  → mixed, skipped
	//   1366x1080 → above (1366>=1280, 1080>=1024), distance 86+56=142
	//   1920x1200 → above, distance 640+176=816
	modes := []Mode{
		{Width: 1600, Height: 768},
		{Width: 1366, Height: 1080},
		{Width: 1920, Height: 1200},
	}
	got, err := PickBestMode(modes, Mode{Width: 1280, Height: 1024})
	if err != nil {
		t.Fatalf("PickBestMode: %v", err)
	}
	if got.Width != 1366 || got.Height != 1080 {
		t.Errorf("got %dx%d; want 1366x1080 (smallest above; mixed skipped)", got.Width, got.Height)
	}
}

// TestPickBestMode_onlyMixedReturnsErrNoMatch — if every supported
// mode is mixed (none satisfies both ≤ on both axes nor ≥ on both
// axes), there's no valid pick.
func TestPickBestMode_onlyMixedReturnsErrNoMatch(t *testing.T) {
	modes := []Mode{
		{Width: 1600, Height: 600}, // wider, shorter
		{Width: 800, Height: 1200}, // narrower, taller
	}
	_, err := PickBestMode(modes, Mode{Width: 1280, Height: 1024})
	if !errors.Is(err, ErrNoMatchingMode) {
		t.Errorf("PickBestMode(only mixed): err = %v; want ErrNoMatchingMode", err)
	}
}
