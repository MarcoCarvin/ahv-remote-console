// Shared contract between the N-API glue (keyhook.cc) and the per-OS hook
// backends (win.cc / mac.mm / linux.cc).
#pragma once

// The FIXED, closed set of host-reserved combos the hook can suppress + forward.
// Order is load-bearing: index.js maps these ordinals back to LockCombo strings.
enum LockCombo {
  COMBO_ALT_TAB = 0,
  COMBO_ALT_SHIFT_TAB = 1,
  COMBO_ALT_ESC = 2,
  COMBO_SUPER = 3,
};

namespace keyhook {

// Implemented in keyhook.cc. A per-OS hook thread calls this for each suppressed
// combo; it marshals to the JS thread via a Napi::ThreadSafeFunction. Safe to
// call from any thread; a no-op once released. Carries only the closed combo
// ordinal — never a raw scancode/keysym.
void DispatchCombo(int combo);

// Implemented per-OS. Probe whether an active grab is possible WITHOUT installing
// the hook (X11 present vs Wayland; macOS Accessibility/TCC granted).
bool PlatformAvailable();

// Implemented per-OS. Install the keyboard hook on its own thread. Returns false
// if the grab cannot be established. Must not be called twice without a release.
bool PlatformArm();

// Implemented per-OS. Remove the hook and stop its thread. Idempotent.
void PlatformRelease();

}  // namespace keyhook
