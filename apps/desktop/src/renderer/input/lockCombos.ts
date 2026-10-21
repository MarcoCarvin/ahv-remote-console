/**
 * Keyboard LOCK mode (VMware-style windowed auto-grab) — the renderer-side map
 * from a native-grab combo (the closed enum that crosses the signed IPC bridge)
 * to the RFB keysym chord injected into the guest.
 *
 * The chords are byte-identical to the matching Send Keys menu entries, so a
 * grabbed Alt+Tab and a Send-Keys Alt+Tab produce the same wire frames. The
 * renderer owns this table; raw keysyms never cross IPC — only the LockCombo
 * enum does (main → renderer over `ahv-remote-console:lock-combo`).
 *
 *   Alt_L 0xffe9 · Shift_L 0xffe1 · Tab 0xff09 · Escape 0xff1b · Super_L 0xffeb
 */
import type { LockCombo } from '../../preload/preload';

export type { LockCombo };

export const LOCK_COMBO_KEYSYMS: Record<LockCombo, number[]> = {
  AltTab: [0xffe9, 0xff09],
  AltShiftTab: [0xffe9, 0xffe1, 0xff09],
  AltEsc: [0xffe9, 0xff1b],
  Super: [0xffeb],
};
