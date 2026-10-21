import { describe, it, expect } from 'vitest';
import { eventCodeToKeycode, hasMapping } from '../Keycodes';

describe('eventCodeToKeycode — host-aware Mac overrides (AT set-1 scancodes)', () => {
  it('maps macOS-only keys to their AT-set-1 codes ONLY on macOS', () => {
    // The reported "cancel key": Mac keypad Clear → PC NumLock position (0x45).
    expect(eventCodeToKeycode('NumpadClear', 'macos')).toBe(0x45);
    expect(eventCodeToKeycode('Help', 'macos')).toBe(0xf5); // 0xe075 folded
    expect(eventCodeToKeycode('Eject', 'macos')).toBe(0xfd); // 0xe07d folded
    expect(eventCodeToKeycode('Power', 'macos')).toBe(0xde); // 0xe05e folded
    expect(eventCodeToKeycode('Lang1', 'macos')).toBe(0x72);
    expect(eventCodeToKeycode('Lang2', 'macos')).toBe(0x71);
    expect(eventCodeToKeycode('AudioVolumeMute', 'macos')).toBe(0xa0); // 0xe020 folded
    expect(eventCodeToKeycode('AudioVolumeDown', 'macos')).toBe(0xae); // 0xe02e folded
    expect(eventCodeToKeycode('AudioVolumeUp', 'macos')).toBe(0xb0); // 0xe030 folded
    expect(eventCodeToKeycode('MediaPlayPause', 'macos')).toBe(0xa2); // 0xe022 folded
    expect(eventCodeToKeycode('MediaTrackNext', 'macos')).toBe(0x99); // 0xe019 folded
    expect(eventCodeToKeycode('MediaTrackPrevious', 'macos')).toBe(0x90); // 0xe010 folded
  });

  it('does NOT leak the Mac overrides to other hosts', () => {
    expect(eventCodeToKeycode('NumpadClear', 'windows')).toBe(0);
    expect(eventCodeToKeycode('Help', 'linux')).toBe(0);
    expect(eventCodeToKeycode('MediaPlayPause', 'windows')).toBe(0);
  });

  it('keeps F13–F24 host-independent (base table, AT set-1)', () => {
    for (const host of ['windows', 'linux', 'macos'] as const) {
      expect(eventCodeToKeycode('F13', host)).toBe(0x5d);
      expect(eventCodeToKeycode('F24', host)).toBe(0x6f);
    }
  });

  it('base typewriter keys are unchanged; JIS Yen / Numpad= are now correct AT set-1', () => {
    for (const host of ['windows', 'linux', 'macos'] as const) {
      expect(eventCodeToKeycode('KeyA', host)).toBe(0x1e); // == evdev 30, unchanged
      expect(eventCodeToKeycode('IntlYen', host)).toBe(0x7d); // was evdev 124, now AT set-1 0x7d
      expect(eventCodeToKeycode('NumpadEqual', host)).toBe(0x59); // was evdev 117, now AT set-1 0x59
    }
  });

  it('hasMapping reflects the Mac override layer', () => {
    expect(hasMapping('NumpadClear', 'macos')).toBe(true);
    expect(hasMapping('NumpadClear', 'windows')).toBe(false);
    expect(hasMapping('KeyA', 'windows')).toBe(true);
    expect(hasMapping('F13', 'macos')).toBe(true);
  });
});
