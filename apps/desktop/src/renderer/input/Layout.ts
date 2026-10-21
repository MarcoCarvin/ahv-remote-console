/**
 * Per-host keyboard layout introspection.
 *
 * The static `Keycodes.ts` table assumes US-QWERTY physical
 * placements (KeyA = Linux KEY_A regardless of what the operator's
 * actual layout maps it to). For RFB / QEMU paths that's fine —
 * the guest's keyboard driver does its own layout. For the agent
 * uinput path it's also fine (same reason).
 *
 * Where layout introspection helps is the rare case where the
 * host's layout has moved physical keys (Japanese 106-key has
 * extra keys with codes 91, 93, 94, 145 that aren't on a US
 * keyboard, and Apple aluminum keyboards reorder \) so the
 * static table doesn't have an evdev code for them. The Keyboard
 * API exposes `navigator.keyboard.getLayoutMap()` which returns
 * `Map<code, char>` for the active layout; we use it to detect
 * additional `KeyboardEvent.code` values that ship characters,
 * and warn at startup so an operator with a non-US layout has
 * visibility into what fell through.
 *
 * Browser support: Chrome / Edge / Electron yes; Firefox / Safari
 * no (return null and we silently no-op).
 *
 * See docs/INTERACTION-ARCH.md §3 Phase 3 item 5.
 */

import { hasMapping } from './Keycodes';

export interface LayoutSnapshot {
  /** Active layout's `code` → `key` mapping (codepoints only). */
  entries: Map<string, string>;
  /**
   * Codes the active layout reports characters for that our static
   * `Keycodes.ts` table does NOT have. For these keys, RFB falls
   * back to the keysym path (which is fine), but the agent uinput
   * path will have keycode=0 — and the guest's input subsystem
   * loses the physical-key information.
   */
  unmapped: string[];
}

interface NavigatorWithKeyboard extends Navigator {
  keyboard?: {
    getLayoutMap?: () => Promise<Map<string, string>>;
  };
}

/**
 * Probe the browser's Keyboard API for the active layout. Returns
 * null when unavailable (Firefox / Safari / older browsers).
 *
 * Called once at ConsoleView mount. Result cached so the InputEngine
 * can consult it without re-issuing the API call per keystroke.
 */
export async function probeLayout(): Promise<LayoutSnapshot | null> {
  const nav = navigator as NavigatorWithKeyboard;
  const keyboard = nav.keyboard;
  if (!keyboard?.getLayoutMap) return null;
  try {
    const layoutMap = await keyboard.getLayoutMap();
    const unmapped: string[] = [];
    for (const code of layoutMap.keys()) {
      if (!hasMapping(code)) unmapped.push(code);
    }
    return { entries: layoutMap, unmapped };
  } catch {
    // Permission denied (mismatched user activation in some
    // Chromium policies) or transient error — degrade silently.
    return null;
  }
}

/**
 * Singleton cache populated by the first ConsoleView mount. Subsequent
 * mounts (multi-tab, pop-out) re-use the cached snapshot — the active
 * keyboard layout doesn't change during an app session in practice;
 * if it does, the operator can restart the app.
 */
let cached: Promise<LayoutSnapshot | null> | null = null;
export function layoutSnapshot(): Promise<LayoutSnapshot | null> {
  cached ??= probeLayout();
  return cached;
}
