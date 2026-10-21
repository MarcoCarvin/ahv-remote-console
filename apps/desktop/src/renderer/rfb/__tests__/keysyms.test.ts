import { describe, it, expect } from 'vitest';
import {
  eventToKeysym,
  XK_Clear,
  XK_dead_acute,
  XK_dead_grave,
  XK_Help,
  XK_KP_0,
  XK_KP_Add,
  XK_Left,
} from '../keysyms';

// vitest runs in the node environment (no DOM), so construct event-like plain
// objects and cast — eventToKeysym only reads .code and .key.
const ev = (code: string, key: string): KeyboardEvent =>
  ({ code, key }) as unknown as KeyboardEvent;

describe('eventToKeysym', () => {
  it('resolves dead/accent keys to X11 dead keysyms (KBD-1)', () => {
    expect(eventToKeysym(ev('Equal', 'Dead'))).toBe(XK_dead_acute);
    expect(eventToKeysym(ev('Backquote', 'Dead'))).toBe(XK_dead_grave);
    // Unknown physical code for a dead key falls back to grave rather than 0.
    expect(eventToKeysym(ev('KeyZ', 'Dead'))).toBe(XK_dead_grave);
  });

  it('maps printable keys to their codepoint, preserving case', () => {
    expect(eventToKeysym(ev('KeyA', 'a'))).toBe(0x61);
    expect(eventToKeysym(ev('KeyA', 'A'))).toBe(0x41);
    expect(eventToKeysym(ev('Digit1', '1'))).toBe(0x31);
    expect(eventToKeysym(ev('KeyQ', '@'))).toBe(0x40);
  });

  it('maps non-printable navigation keys via the physical code', () => {
    expect(eventToKeysym(ev('ArrowLeft', 'ArrowLeft'))).toBe(XK_Left);
  });

  it('returns 0 for keys it cannot resolve', () => {
    expect(eventToKeysym(ev('Unidentified', 'Unidentified'))).toBe(0);
  });

  it('maps the macOS keypad Clear ("cancel") key + the numpad', () => {
    expect(eventToKeysym(ev('NumpadClear', 'Clear'))).toBe(XK_Clear);
    expect(eventToKeysym(ev('Numpad0', '0'))).toBe(XK_KP_0);
    expect(eventToKeysym(ev('Numpad5', '5'))).toBe(XK_KP_0 + 5);
    expect(eventToKeysym(ev('NumpadAdd', '+'))).toBe(XK_KP_Add);
    expect(eventToKeysym(ev('Help', 'Help'))).toBe(XK_Help);
  });

  it('leaves media/volume keys keysym-less (they ride the scancode path)', () => {
    expect(eventToKeysym(ev('AudioVolumeUp', 'AudioVolumeUp'))).toBe(0);
    expect(eventToKeysym(ev('MediaPlayPause', 'MediaPlayPause'))).toBe(0);
  });
});
