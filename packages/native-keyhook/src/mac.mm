// macOS backend — DEFERRED STUB (Phase-2b).
//
// A real implementation is a CGEventTap at kCGSessionEventTap on a dedicated
// CFRunLoop thread, gated on Accessibility (TCC). It is intentionally NOT shipped
// yet because:
//   1. WindowServer reserves Cmd+Tab / Cmd+Space / Mission Control ABOVE a
//      session tap, so the grab buys little on macOS (even Apple's own
//      VZVirtualMachineView.capturesSystemKeys loses to WindowServer, and that
//      regressed on Sequoia 15.6) — Send-Keys stays the path for those anyway.
//   2. A TCC-prompting, hardened-runtime, notarized build needs a
//      Developer ID cert that does not yet exist on the build Mac (self-signed
//      only — see the mac-signing-pipeline note).
//
// Until then this returns "unavailable", so macOS transparently falls back to
// the Send-Keys menu with NO permission prompt and NO notarization risk.
#include "keyhook.h"

bool keyhook::PlatformAvailable() { return false; }

bool keyhook::PlatformArm() { return false; }

void keyhook::PlatformRelease() {}
