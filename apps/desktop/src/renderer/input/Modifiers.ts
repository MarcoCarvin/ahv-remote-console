/**
 * Modifier state machine.
 *
 * Tracks, per logical modifier, the renderer's *belief* about what the
 * guest currently sees. "Belief" because we can't read back from the
 * guest — every authoritative decision is local. Whenever the canvas
 * blurs (Cmd+Tab, app-menu open, window minimize, modal popup) we
 * iterate this map and emit KeyUp for any modifier currently DOWN so
 * the guest never wakes up to a stuck Ctrl/Alt/Shift/Cmd.
 *
 * The map is keyed by *logical* modifier name, not by browser
 * `KeyboardEvent.key` / `.code`, because left vs. right side share
 * one keysym for our purposes; tracking both halves separately would
 * just create stale-down phantoms when an operator presses one side
 * and releases the other (e.g. left-Shift down → right-Shift up).
 */

import { XK_Alt_L, XK_Control_L, XK_Shift_L, XK_Super_L } from '../rfb/keysyms';

export type LogicalModifier = 'control' | 'shift' | 'alt' | 'meta';

export interface ModifierTracker {
  isDown(mod: LogicalModifier): boolean;
  /**
   * Mark a modifier as guest-believed DOWN. The caller is expected
   * to have already sent the corresponding KeyDown over the wire;
   * this only updates the bookkeeping.
   */
  press(mod: LogicalModifier): void;
  /** Symmetric to press. */
  release(mod: LogicalModifier): void;
  /**
   * Emit KeyUp for every modifier currently believed-DOWN and reset
   * the map. Returns the list of modifiers that were released, for
   * audit / telemetry. Idempotent — safe to call on blur even if
   * nothing was held.
   */
  releaseAll(emitKey: (keysym: number, down: boolean) => void): LogicalModifier[];
  /** Snapshot for debugging / tests. */
  snapshot(): Record<LogicalModifier, boolean>;
}

/**
 * Keysym used to release a logical modifier. We always emit the
 * left-side variant on release; the guest's modifier state is a set
 * (left OR right Shift is "Shift"), so releasing the left half
 * clears the OS-level Shift state even if the operator pressed the
 * right half. This is the same trick `xdotool` uses.
 */
export const RELEASE_KEYSYM: Record<LogicalModifier, number> = {
  control: XK_Control_L,
  shift: XK_Shift_L,
  alt: XK_Alt_L,
  meta: XK_Super_L,
};

export function createModifierTracker(): ModifierTracker {
  const down: Record<LogicalModifier, boolean> = {
    control: false,
    shift: false,
    alt: false,
    meta: false,
  };

  return {
    isDown(mod) {
      return down[mod];
    },
    press(mod) {
      down[mod] = true;
    },
    release(mod) {
      down[mod] = false;
    },
    releaseAll(emitKey) {
      const released: LogicalModifier[] = [];
      for (const mod of Object.keys(down) as LogicalModifier[]) {
        if (down[mod]) {
          emitKey(RELEASE_KEYSYM[mod], false);
          down[mod] = false;
          released.push(mod);
        }
      }
      return released;
    },
    snapshot() {
      return { ...down };
    },
  };
}

/**
 * Map a browser `KeyboardEvent.key` to its logical modifier, or
 * null if the event is not a modifier.
 *
 * `AltGraph` (Chromium's right-Alt on non-US layouts) is deliberately NOT a
 * modifier here. AltGr is a layout LAYER, not a command modifier: folding it
 * into `alt` made AltGr+key send `Alt_L` to the guest, so e.g. German AltGr+Q
 * landed as an Alt accelerator instead of '@'. AltGr-produced characters are
 * handled by the layout-combo escape in InputEngine, which sends the literal
 * character with no Alt/Ctrl attached.
 */
export function modifierFromKey(key: string): LogicalModifier | null {
  switch (key) {
    case 'Control':
      return 'control';
    case 'Shift':
      return 'shift';
    case 'Alt':
      return 'alt';
    case 'AltGraph':
      return null;
    case 'Meta':
    case 'OS':
      return 'meta';
    default:
      return null;
  }
}
