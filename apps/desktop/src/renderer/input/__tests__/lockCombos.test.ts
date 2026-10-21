import { describe, expect, it } from 'vitest';
import { LOCK_COMBO_KEYSYMS } from '../lockCombos';

/**
 * The native keyboard grab forwards a closed LockCombo enum; the renderer maps
 * it to a keysym chord injected via RFBClient.sendKeySequence. These chords MUST
 * stay byte-identical to the Send Keys menu entries in ConsoleView (the
 * guaranteed fallback) so a grabbed Alt+Tab and a Send-Keys Alt+Tab are
 * indistinguishable to the guest.
 */

// RFB keysyms (X11): the same legend ConsoleView's Send Keys menu uses.
const Alt_L = 0xffe9;
const Shift_L = 0xffe1;
const Tab = 0xff09;
const Escape = 0xff1b;
const Super_L = 0xffeb;

describe('LOCK_COMBO_KEYSYMS', () => {
  it('maps each combo to the exact Send-Keys keysym chord', () => {
    expect(LOCK_COMBO_KEYSYMS.AltTab).toEqual([Alt_L, Tab]);
    expect(LOCK_COMBO_KEYSYMS.AltShiftTab).toEqual([Alt_L, Shift_L, Tab]);
    expect(LOCK_COMBO_KEYSYMS.AltEsc).toEqual([Alt_L, Escape]);
    expect(LOCK_COMBO_KEYSYMS.Super).toEqual([Super_L]);
  });

  it('covers exactly the four grabbable combos', () => {
    expect(Object.keys(LOCK_COMBO_KEYSYMS).sort()).toEqual(
      ['AltEsc', 'AltShiftTab', 'AltTab', 'Super'].sort(),
    );
  });

  it('every Alt combo leads with Alt_L (so the guest sees Alt held first)', () => {
    for (const combo of ['AltTab', 'AltShiftTab', 'AltEsc'] as const) {
      expect(LOCK_COMBO_KEYSYMS[combo][0]).toBe(Alt_L);
    }
  });
});
