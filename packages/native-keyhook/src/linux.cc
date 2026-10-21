// Linux backend — DEFERRED STUB.
//
// The obvious approach — XGrabKeyboard on the root window from this addon's own
// X connection — is WRONG for our model: an active keyboard grab redirects ALL
// key events to the grabbing *client* (this addon's Display connection), which
// is a different X client from Electron. While armed, Electron's renderer would
// stop receiving keydown events entirely, so normal typing into the guest (which
// rides the renderer's DOM keydown → InputEngine → RFB path) would break — only
// the few forwarded combos would work.
//
// The correct primitive is a set of PASSIVE per-combo grabs (XGrabKey for
// Alt+Tab / Alt+Shift+Tab / Alt+Esc / Super, across the lock/numlock modifier
// permutations), so ONLY those specific combos are redirected here and every
// other key still reaches Electron normally. That is intricate (bare-modifier
// Super grab, modifier-mask permutations) and untestable off a real X server, so
// it is deferred. Wayland has no core active grab at all.
//
// Until then this returns "unavailable", so Linux transparently falls back to
// the Send-Keys menu.
#include "keyhook.h"

bool keyhook::PlatformAvailable() { return false; }

bool keyhook::PlatformArm() { return false; }

void keyhook::PlatformRelease() {}
