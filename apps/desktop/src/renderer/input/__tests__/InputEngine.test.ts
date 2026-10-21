import { describe, it, expect, vi } from 'vitest';
import { createInputEngine } from '../InputEngine';
import { eventCodeToKeycode } from '../Keycodes';
import {
  XK_Alt_R,
  XK_BackSpace,
  XK_Clear,
  XK_Control_L,
  XK_Delete,
  XK_End,
  XK_Home,
  XK_ISO_Level3_Shift,
  XK_Left,
  XK_Print,
  XK_Right,
  XK_Shift_L,
} from '../../rfb/keysyms';
import type { HostOS } from '../HostOS';

interface KeyCall {
  keysym: number;
  keycode: number;
  down: boolean;
  ext: boolean;
}

function makeClient(extEnabled: boolean) {
  const calls: KeyCall[] = [];
  // sendKeySequence is the press-all-then-release-all path the engine uses for
  // pre-composed chords (Mac→Windows nav translations, lock-mode Cmd+Tab). We
  // capture the raw keysym list per call so tests assert the translation without
  // unrolling the up/down expansion.
  const sequences: number[][] = [];
  const client = {
    extendedKeyEnabled: () => extEnabled,
    sendKey: (keysym: number, down: boolean) =>
      calls.push({ keysym, keycode: 0, down, ext: false }),
    sendExtendedKey: (keysym: number, keycode: number, down: boolean) =>
      calls.push({ keysym, keycode, down, ext: true }),
    sendKeySequence: (keysyms: number[]) => sequences.push(keysyms),
    sendPointer: () => undefined,
  };
  return { client, calls, sequences };
}

function engineFor(host: HostOS, extEnabled = false) {
  const { client, calls, sequences } = makeClient(extEnabled);
  const engine = createInputEngine({
    client: () => client as never,
    onReleaseChord: () => undefined,
    hostOSOverride: host,
  });
  return { engine, calls, sequences };
}

// Event-like plain object; the engine only reads these fields. getModifierState
// answers AltGraph/CapsLock/NumLock from the flags and false for anything else.
function kev(
  code: string,
  key: string,
  opts: {
    shift?: boolean;
    meta?: boolean;
    ctrl?: boolean;
    alt?: boolean;
    altGraph?: boolean;
    caps?: boolean;
    num?: boolean;
    ts?: number;
    keyCode?: number;
    repeat?: boolean;
  } = {},
): KeyboardEvent {
  return {
    code,
    key,
    repeat: !!opts.repeat,
    keyCode: opts.keyCode ?? 0,
    timeStamp: opts.ts ?? 0,
    shiftKey: !!opts.shift,
    metaKey: !!opts.meta,
    ctrlKey: !!opts.ctrl,
    altKey: !!opts.alt || !!opts.altGraph,
    getModifierState: (m: string) =>
      m === 'AltGraph'
        ? !!opts.altGraph
        : m === 'CapsLock'
          ? !!opts.caps
          : m === 'NumLock'
            ? !!opts.num
            : false,
    preventDefault: () => undefined,
    stopPropagation: () => undefined,
  } as unknown as KeyboardEvent;
}

describe('InputEngine — keysym path (extended key OFF)', () => {
  it('KBD-6: releases the keysym sent on keydown even if Shift changed by keyup', () => {
    const { engine, calls } = engineFor('windows');
    engine.handleKeyDown(kev('KeyA', 'A', { shift: true })); // -> 0x41 down
    engine.handleKeyUp(kev('KeyA', 'a', { shift: false })); // Shift released; e.key now 'a'
    const a = calls.filter((c) => c.keysym === 0x41);
    const lower = calls.filter((c) => c.keysym === 0x61);
    expect(a.map((c) => c.down)).toEqual([true, false]); // 0x41 down then up
    expect(lower).toHaveLength(0); // never releases 0x61 — no stuck key
  });

  it('blur releases printable keys still held (KBD-6 cleanup)', () => {
    const { engine, calls } = engineFor('windows');
    engine.handleKeyDown(kev('KeyA', 'a'));
    calls.length = 0;
    engine.releaseAllModifiers();
    expect(calls).toContainEqual({ keysym: 0x61, keycode: 0, down: false, ext: false });
  });
});

describe('InputEngine — key auto-repeat (a held key repeats in the guest)', () => {
  // QEMU's emulated keyboard does not typematic-repeat, so the engine forwards
  // each browser repeat keydown as another make; a held key thus repeats in the
  // guest (the "sssss" the operator expects), with one break on release. Drops
  // repeats only for modifier keys.
  it('a held printable forwards each repeat as another key-down (sssss)', () => {
    const { engine, calls } = engineFor('windows');
    engine.handleKeyDown(kev('KeyS', 's')); // initial make
    engine.handleKeyDown(kev('KeyS', 's', { repeat: true })); // auto-repeat
    engine.handleKeyDown(kev('KeyS', 's', { repeat: true }));
    engine.handleKeyUp(kev('KeyS', 's'));
    const downs = calls.filter((c) => c.keysym === 0x73 && c.down);
    const ups = calls.filter((c) => c.keysym === 0x73 && !c.down);
    expect(downs.length).toBe(3); // three makes -> "sss"
    expect(ups.length).toBe(1); // single break on release
  });

  it('a held MODIFIER does not flood repeats (a single make)', () => {
    const { engine, calls } = engineFor('windows');
    engine.handleKeyDown(kev('ShiftLeft', 'Shift', { shift: true }));
    engine.handleKeyDown(kev('ShiftLeft', 'Shift', { shift: true, repeat: true }));
    engine.handleKeyDown(kev('ShiftLeft', 'Shift', { shift: true, repeat: true }));
    expect(calls.filter((c) => c.keysym === XK_Shift_L && c.down).length).toBe(1);
  });

  it('suppressRepeat:true restores the drop-all-repeats behavior', () => {
    const { client, calls } = makeClient(false);
    const engine = createInputEngine({
      client: () => client as never,
      onReleaseChord: () => undefined,
      hostOSOverride: 'windows',
      suppressRepeat: true,
    });
    engine.handleKeyDown(kev('KeyS', 's'));
    engine.handleKeyDown(kev('KeyS', 's', { repeat: true }));
    expect(calls.filter((c) => c.keysym === 0x73 && c.down).length).toBe(1);
  });
});

describe('InputEngine — no HOST-layout guessing (translation follows the DECLARED guest layout only)', () => {
  // e.key already tells us WHAT the operator typed; the only layout input is
  // the operator's guest-layout pick (setGuestLayout, default en-us). There is
  // no locale sniffing and no forced pattern.
  it('on a US guest, "ù" is untranslatable and rides verbatim (no Italian-host guessing)', () => {
    const { engine, calls } = engineFor('windows');
    engine.handleKeyDown(kev('Backslash', 'ù'));
    expect(calls.some((c) => c.keysym === 0x00f9 && c.down)).toBe(true); // ù verbatim
    expect(calls.some((c) => c.keysym === 0x5c)).toBe(false); // no host-side "recovery"
    expect(calls.some((c) => c.keysym === XK_Shift_L)).toBe(false);
  });

  it('a plain digit is sent as the digit, not a guest shift-level symbol', () => {
    const { engine, calls } = engineFor('windows');
    engine.handleKeyDown(kev('Digit1', '1'));
    expect(calls.some((c) => c.keysym === 0x31 && c.down)).toBe(true);
    expect(calls.some((c) => c.keysym === 0x21)).toBe(false); // never '!'
  });

  it('createInputEngine has no host-layout API (no setHostLayout / layout options)', () => {
    const { engine } = engineFor('windows');
    expect('setHostLayout' in engine).toBe(false);
  });
});

describe('InputEngine — guest-layout translation (operator-declared via the picker)', () => {
  const SHIFT_OFF = { k: XK_Shift_L, d: false };
  const SHIFT_ON = { k: XK_Shift_L, d: true };
  const slim = (calls: KeyCall[]) => calls.map((c) => ({ k: c.keysym, d: c.down }));

  it('US guest: "@" with no host Shift composes Shift+2 (QEMU masks the entry shift bit)', () => {
    const { engine, calls } = engineFor('windows'); // default guest layout: us
    engine.handleKeyDown(kev('Semicolon', '@', { altGraph: true })); // IT host AltGr+ò
    expect(slim(calls)).toEqual([SHIFT_ON, { k: 0x32, d: true }, { k: 0x32, d: false }, SHIFT_OFF]);
  });

  it('US guest: IT-host "/" (Shift+7) lifts Shift around the slash key — never "?"', () => {
    const { engine, calls } = engineFor('windows');
    engine.handleKeyDown(kev('Digit7', '/', { shift: true }));
    expect(slim(calls)).toEqual([SHIFT_OFF, { k: 0x2f, d: true }, { k: 0x2f, d: false }, SHIFT_ON]);
    expect(calls.some((c) => c.keysym === 0x3f)).toBe(false); // never '?'
  });

  it('US guest: "?" with host Shift held is wire-equivalent — passes verbatim', () => {
    const { engine, calls } = engineFor('windows');
    engine.handleKeyDown(kev('Slash', '?', { shift: true }));
    expect(calls.some((c) => c.keysym === 0x3f && c.down)).toBe(true); // raw '?' rides host Shift
    expect(calls.some((c) => c.keysym === XK_Shift_L)).toBe(false); // no synthetic frames
  });

  it('Italian guest: "£" rides the host Shift on the 3 key', () => {
    const { engine, calls } = engineFor('windows');
    engine.setGuestLayout('it');
    engine.handleKeyDown(kev('Digit3', '£', { shift: true })); // IT host Shift+3
    expect(slim(calls)).toEqual([
      { k: 0x33, d: true },
      { k: 0x33, d: false },
    ]); // '3' under host Shift
  });

  it('Italian guest: "@" becomes AltGr + the ò key (en-us ";" position)', () => {
    const { engine, calls } = engineFor('windows');
    engine.setGuestLayout('it');
    engine.handleKeyDown(kev('Semicolon', '@', { altGraph: true }));
    expect(slim(calls)).toEqual([
      { k: XK_Alt_R, d: true },
      { k: 0x3b, d: true },
      { k: 0x3b, d: false },
      { k: XK_Alt_R, d: false },
    ]);
  });

  it('Italian guest: "è" is the bare [-position key; "é" rides the host Shift there', () => {
    const eGrave = engineFor('windows');
    eGrave.engine.setGuestLayout('it');
    eGrave.engine.handleKeyDown(kev('BracketLeft', 'è'));
    expect(slim(eGrave.calls)).toEqual([
      { k: 0x5b, d: true },
      { k: 0x5b, d: false },
    ]);
    const eAcute = engineFor('windows');
    eAcute.engine.setGuestLayout('it');
    eAcute.engine.handleKeyDown(kev('BracketLeft', 'é', { shift: true }));
    expect(slim(eAcute.calls)).toEqual([
      { k: 0x5b, d: true },
      { k: 0x5b, d: false },
    ]);
  });

  it('Italian guest: "ù" maps to the en-us backslash position', () => {
    const { engine, calls } = engineFor('windows');
    engine.setGuestLayout('it');
    engine.handleKeyDown(kev('Backslash', 'ù'));
    expect(slim(calls)).toEqual([
      { k: 0x5c, d: true },
      { k: 0x5c, d: false },
    ]);
  });

  it('Italian guest: plain letters stay verbatim (real key-down/up, auto-repeat preserved)', () => {
    const { engine, calls } = engineFor('windows');
    engine.setGuestLayout('it');
    engine.handleKeyDown(kev('KeyA', 'a'));
    engine.handleKeyUp(kev('KeyA', 'a'));
    expect(slim(calls)).toEqual([
      { k: 0x61, d: true },
      { k: 0x61, d: false },
    ]);
  });

  it('the translated key-up is swallowed (no stray release)', () => {
    const { engine, calls } = engineFor('windows');
    engine.setGuestLayout('it');
    engine.handleKeyDown(kev('Digit3', '£', { shift: true }));
    calls.length = 0;
    engine.handleKeyUp(kev('Digit3', '£', { shift: true }));
    expect(calls).toHaveLength(0);
  });

  it('characters the declared layout cannot type fall through verbatim (Tier-2 territory)', () => {
    const { engine, calls } = engineFor('windows');
    engine.setGuestLayout('it');
    engine.handleKeyDown(kev('KeyV', '√'));
    expect(calls.some((c) => c.keysym === 0x221a && c.down)).toBe(true); // best-effort verbatim
    expect(calls.some((c) => c.keysym === XK_Shift_L || c.keysym === XK_Alt_R)).toBe(false);
  });

  it('CapsLock letters are NOT shift-wrapped (guest CapsLock is mirrored instead)', () => {
    const { engine, calls } = engineFor('windows');
    engine.handleKeyDown(kev('KeyA', 'A', { caps: true })); // case from CapsLock, no Shift
    expect(calls.some((c) => c.keysym === 0x41 && c.down)).toBe(true); // verbatim 'A'
    expect(calls.some((c) => c.keysym === XK_Shift_L)).toBe(false);
  });

  it('unknown picker ids fall back to en-us', () => {
    const { engine, calls } = engineFor('windows');
    engine.setGuestLayout('xx-nonexistent');
    engine.handleKeyDown(kev('Digit2', '@', {}));
    expect(calls.some((c) => c.keysym === 0x32 && c.down)).toBe(true); // Shift+2 composition
    expect(calls.some((c) => c.keysym === XK_Shift_L && c.down)).toBe(true);
  });
});

describe('InputEngine — scancode path (extended key ON) carries AT-set-1, not evdev', () => {
  it('AHV keysym authority: an ext frame carries BOTH the keysym and the scancode', () => {
    const { engine, calls } = engineFor('windows', true);
    engine.handleKeyDown(kev('KeyA', 'a'));
    const down = calls.find((c) => c.down)!;
    expect(down.ext).toBe(true);
    expect(down.keysym).toBe(0x61); // host-resolved 'a' (authoritative on -k AHV)
    expect(down.keycode).toBe(0x1e); // KeyA AT-set-1 (== evdev here)
  });

  it('ArrowLeft carries folded AT-set-1 0xcb, NOT evdev 105', () => {
    const { engine, calls } = engineFor('windows', true);
    engine.handleKeyDown(kev('ArrowLeft', 'ArrowLeft'));
    const down = calls.find((c) => c.down)!;
    expect(down.keycode).toBe(0xcb);
    expect(down.keysym).toBe(XK_Left);
    expect(eventCodeToKeycode('ArrowLeft')).toBe(0xcb);
  });

  it('every extended key carries its folded AT-set-1 wire byte', () => {
    const { engine, calls } = engineFor('windows', true);
    const cases: [string, number][] = [
      ['ControlRight', 0x9d],
      ['AltRight', 0xb8],
      ['Home', 0xc7],
      ['End', 0xcf],
      ['PageUp', 0xc9],
      ['PageDown', 0xd1],
      ['Insert', 0xd2],
      ['Delete', 0xd3],
      ['ArrowUp', 0xc8],
      ['ArrowDown', 0xd0],
      ['ArrowRight', 0xcd],
      ['ContextMenu', 0xdd],
      ['MetaLeft', 0xdb],
      ['MetaRight', 0xdc],
      ['NumpadEnter', 0x9c], // 0xe01c -> 0x9c
      ['NumpadDivide', 0xb5],
      ['Pause', 0xc6],
    ];
    for (const [code, want] of cases) {
      expect(eventCodeToKeycode(code)).toBe(want);
    }
    // PrintScreen is the base 0x54 special-case (no 0xe0 prefix, not folded).
    engine.handleKeyDown(kev('PrintScreen', 'PrintScreen'));
    const ps = calls.find((c) => c.down)!;
    expect(ps.keycode).toBe(0x54);
    expect(ps.keysym).toBe(XK_Print);
  });

  it('JIS keys diverge from evdev: IntlYen 0x7d (not 124), IntlRo 0x73 (not 89)', () => {
    expect(eventCodeToKeycode('IntlYen')).toBe(0x7d);
    expect(eventCodeToKeycode('IntlRo')).toBe(0x73);
  });

  it('base keys are unchanged (AT-set-1 == evdev there)', () => {
    expect(eventCodeToKeycode('KeyA')).toBe(0x1e);
    expect(eventCodeToKeycode('Digit1')).toBe(0x02);
    expect(eventCodeToKeycode('F5')).toBe(0x3f);
    expect(eventCodeToKeycode('Space')).toBe(0x39);
    expect(eventCodeToKeycode('Backslash')).toBe(0x2b);
    expect(eventCodeToKeycode('IntlBackslash')).toBe(0x56);
  });

  it('Mac Cmd+C synthetic Ctrl carries a real scancode (AHV drops keycode 0)', () => {
    const { engine, calls } = engineFor('macos', true);
    engine.handleKeyDown(kev('KeyC', 'c', { meta: true }));
    const ctrl = calls.filter((c) => c.keysym === XK_Control_L);
    expect(ctrl.length).toBeGreaterThan(0);
    expect(ctrl.every((c) => c.keycode === eventCodeToKeycode('ControlLeft'))).toBe(true);
    expect(ctrl.every((c) => c.keycode === 0x1d)).toBe(true); // real scancode, never 0
  });

  it('the macOS keypad Clear ("cancel") key reaches the guest down AND up', () => {
    const { engine, calls } = engineFor('macos', true);
    engine.handleKeyDown(kev('NumpadClear', 'Clear'));
    engine.handleKeyUp(kev('NumpadClear', 'Clear'));
    const clear = calls.filter((c) => c.keysym === XK_Clear);
    expect(clear.map((c) => c.down)).toEqual([true, false]);
    expect(clear.every((c) => c.ext === true && c.keycode === 0x45)).toBe(true); // NumLock slot
  });

  it('scancode-only keys (AudioVolumeUp) are sent + released by AT-set-1 scancode', () => {
    const { engine, calls } = engineFor('macos', true);
    engine.handleKeyDown(kev('AudioVolumeUp', 'AudioVolumeUp')); // keysym 0, keycode 0xb0
    engine.handleKeyUp(kev('AudioVolumeUp', 'AudioVolumeUp'));
    const vol = calls.filter((c) => c.keycode === 0xb0);
    expect(vol.map((c) => c.down)).toEqual([true, false]);
    expect(vol.every((c) => c.ext === true && c.keysym === 0)).toBe(true);
  });

  it('blur releases a held scancode-only key by its scancode (no stuck key)', () => {
    const { engine, calls } = engineFor('macos', true);
    engine.handleKeyDown(kev('AudioVolumeUp', 'AudioVolumeUp'));
    calls.length = 0;
    engine.releaseAllModifiers();
    expect(calls).toContainEqual({ keysym: 0, keycode: 0xb0, down: false, ext: true });
  });
});

describe('InputEngine — keysym-fallback path (extended key OFF) — no regression', () => {
  it('keysym-less keys are still dropped (no scancode field to carry them)', () => {
    const { engine, calls } = engineFor('macos', false);
    engine.handleKeyDown(kev('AudioVolumeUp', 'AudioVolumeUp'));
    engine.handleKeyUp(kev('AudioVolumeUp', 'AudioVolumeUp'));
    expect(calls).toHaveLength(0); // nothing to send without a scancode field
  });

  it('NumpadClear still works in keysym mode (it has a real keysym)', () => {
    const { engine, calls } = engineFor('macos', false);
    engine.handleKeyDown(kev('NumpadClear', 'Clear'));
    const clear = calls.filter((c) => c.keysym === XK_Clear);
    expect(clear.length).toBe(1);
    expect(clear[0]!.ext).toBe(false); // plain key event
  });
});

describe('InputEngine — Windows AltGr coalescing (phantom Ctrl → single AltGr)', () => {
  it('ControlLeft then AltRight within 50ms yields ISO_Level3_Shift and NO Control_L', () => {
    const { engine, calls } = engineFor('windows');
    engine.handleKeyDown(kev('ControlLeft', 'Control', { ctrl: true, ts: 0 })); // arm (deferred)
    expect(calls).toHaveLength(0); // the phantom Ctrl is NOT forwarded yet
    engine.handleKeyDown(kev('AltRight', 'AltGraph', { ctrl: true, altGraph: true, ts: 30 }));
    expect(calls.some((c) => c.keysym === XK_ISO_Level3_Shift && c.down)).toBe(true);
    expect(calls.some((c) => c.keysym === XK_Control_L)).toBe(false); // no leaked Ctrl
  });

  it('an AltGr sequence is NOT mistaken for the Ctrl+Alt release chord', () => {
    let blurred = false;
    const { client } = makeClient(false);
    const engine = createInputEngine({
      client: () => client as never,
      onReleaseChord: () => {
        blurred = true;
      },
      hostOSOverride: 'windows',
    });
    engine.handleKeyDown(kev('ControlLeft', 'Control', { ctrl: true, ts: 0 }));
    engine.handleKeyDown(kev('AltRight', 'Alt', { ctrl: true, alt: true, ts: 20 }));
    expect(blurred).toBe(false);
  });

  it('a SLOW AltGr (AltRight 50-100ms after the phantom Ctrl, key="Alt") still coalesces and never blurs', () => {
    // Regression for the review must-fix: a >50ms phantom-Ctrl→AltRight gap (RDP/
    // KVM input pipelines) on a browser that labels right-Alt as key="Alt" with
    // NO AltGraph flag used to fall through to isReleaseChord → blur the canvas
    // mid-typing AND leak a stuck Control_L. Any armed AltRight is now AltGr.
    let blurred = false;
    const { client, calls } = makeClient(false);
    const engine = createInputEngine({
      client: () => client as never,
      onReleaseChord: () => {
        blurred = true;
      },
      hostOSOverride: 'windows',
    });
    engine.handleKeyDown(kev('ControlLeft', 'Control', { ctrl: true, ts: 0 }));
    engine.handleKeyDown(kev('AltRight', 'Alt', { ctrl: true, alt: true, ts: 60 }));
    expect(blurred).toBe(false); // does NOT blur
    expect(calls.some((c) => c.keysym === XK_ISO_Level3_Shift && c.down)).toBe(true);
    expect(calls.some((c) => c.keysym === XK_Control_L)).toBe(false); // no stuck Ctrl leaked
  });

  it('holding a lone ControlLeft past 100ms flushes the real Control_L (timer interrupt)', () => {
    vi.useFakeTimers();
    try {
      const { engine, calls } = engineFor('windows');
      engine.handleKeyDown(kev('ControlLeft', 'Control', { ctrl: true, ts: 0 })); // arm
      expect(calls).toHaveLength(0); // deferred — not forwarded yet
      vi.advanceTimersByTime(100); // arm timer fires → interruptAltGr flushes
      expect(calls.some((c) => c.keysym === XK_Control_L && c.down)).toBe(true);
      engine.handleKeyUp(kev('ControlLeft', 'Control', { ts: 200 }));
      const ctrl = calls.filter((c) => c.keysym === XK_Control_L);
      expect(ctrl.map((c) => c.down)).toEqual([true, false]); // down (timer) then up (keyup)
    } finally {
      vi.useRealTimers();
    }
  });

  it('AltGr symbol leaks no stuck Ctrl/Alt; AltRight key-up releases the AltGr', () => {
    const { engine, calls } = engineFor('windows');
    engine.handleKeyDown(kev('ControlLeft', 'Control', { ctrl: true, ts: 0 }));
    engine.handleKeyDown(kev('AltRight', 'AltGraph', { ctrl: true, altGraph: true, ts: 20 }));
    engine.handleKeyDown(kev('KeyQ', '@', { ctrl: true, altGraph: true, ts: 30 })); // DE AltGr+Q
    engine.handleKeyUp(kev('KeyQ', '@', { ctrl: true, altGraph: true, ts: 40 }));
    engine.handleKeyUp(kev('AltRight', 'AltGraph', { ctrl: true, altGraph: true, ts: 50 }));
    // '@' is delivered as the (default) US guest's Shift+2 composition — the
    // -k QEMU masks the '@' entry's own shift bit, so the engine spells it.
    expect(calls.some((c) => c.keysym === 0x32 && c.down)).toBe(true);
    expect(calls.filter((c) => c.keysym === XK_Control_L).length).toBe(0); // Ctrl never leaked
    const iso = calls.filter((c) => c.keysym === XK_ISO_Level3_Shift);
    expect(iso.map((c) => c.down)).toEqual([true, false]); // AltGr down then released
  });

  it('a lone ControlLeft followed by a non-AltRight key flushes the real Control_L', () => {
    const { engine, calls } = engineFor('windows');
    engine.handleKeyDown(kev('ControlLeft', 'Control', { ctrl: true, ts: 0 })); // arm
    engine.handleKeyDown(kev('KeyC', 'c', { ctrl: true, ts: 80 })); // not AltRight → flush
    const ctrlDown = calls.findIndex((c) => c.keysym === XK_Control_L && c.down);
    const cDown = calls.findIndex((c) => c.keysym === 0x63 && c.down);
    expect(ctrlDown).toBeGreaterThanOrEqual(0); // real Ctrl flushed
    expect(cDown).toBeGreaterThan(ctrlDown); // ...before 'c'
  });

  it('a ControlLeft key-UP while armed flushes then releases (a real Ctrl tap)', () => {
    const { engine, calls } = engineFor('windows');
    engine.handleKeyDown(kev('ControlLeft', 'Control', { ctrl: true, ts: 0 })); // arm
    engine.handleKeyUp(kev('ControlLeft', 'Control', { ts: 5 })); // tap released
    const ctrl = calls.filter((c) => c.keysym === XK_Control_L);
    expect(ctrl.map((c) => c.down)).toEqual([true, false]); // down (flush) then up
  });

  it('a real held Ctrl suppresses arming (no spurious AltGr conversion)', () => {
    const { engine, calls } = engineFor('windows');
    engine.handleKeyDown(kev('ControlLeft', 'Control', { ctrl: true, ts: 0 })); // arm
    engine.handleKeyDown(kev('KeyX', 'x', { ctrl: true, ts: 10 })); // flush → Ctrl now tracked
    calls.length = 0;
    engine.handleKeyDown(kev('ControlLeft', 'Control', { ctrl: true, ts: 20 })); // already held → no arm
    engine.handleKeyDown(kev('AltRight', 'AltGraph', { ctrl: true, altGraph: true, ts: 25 }));
    expect(calls.some((c) => c.keysym === XK_ISO_Level3_Shift)).toBe(false);
  });

  it('blur while AltGr-armed strands no Control_L (armed state discarded)', () => {
    const { engine, calls } = engineFor('windows');
    engine.handleKeyDown(kev('ControlLeft', 'Control', { ctrl: true, ts: 0 })); // arm
    calls.length = 0;
    engine.releaseAllModifiers();
    expect(calls.some((c) => c.keysym === XK_Control_L)).toBe(false); // nothing flushed
  });

  it('non-Windows hosts do not coalesce — Linux AltGr rides the genuine modifier', () => {
    const { engine, calls } = engineFor('linux');
    engine.handleKeyDown(kev('ControlLeft', 'Control', { ctrl: true, ts: 0 }));
    // On Linux the ControlLeft is a real Ctrl, forwarded immediately (no arming).
    expect(calls.some((c) => c.keysym === XK_Control_L && c.down)).toBe(true);
  });
});

describe('InputEngine — Linux AltGr (no coalesce, no fake-Ctrl)', () => {
  it('AltGr+Q="@" composes Shift+2 for the en-us wire with no fake-Ctrl dance', () => {
    const { engine, calls } = engineFor('linux');
    engine.handleKeyDown(kev('KeyQ', '@', { altGraph: true }));
    // '@' needs Shift on the -k en-us wire (QEMU masks the entry's own shift
    // bit), and AltGr doesn't hold Shift — so the engine composes Shift+'2'.
    expect(calls.some((c) => c.keysym === XK_Shift_L && c.down)).toBe(true);
    const two = calls.filter((c) => c.keysym === 0x32);
    expect(two.map((c) => c.down)).toEqual([true, false]);
    expect(calls.some((c) => c.keysym === XK_Control_L)).toBe(false);
  });

  it('release chord excludes the third-key case (Ctrl+Alt+T passes through)', () => {
    let blurred = false;
    const { client, calls } = makeClient(false);
    const engine = createInputEngine({
      client: () => client as never,
      onReleaseChord: () => {
        blurred = true;
      },
      hostOSOverride: 'linux',
    });
    engine.handleKeyDown(kev('KeyT', 't', { ctrl: true, alt: true }));
    expect(blurred).toBe(false);
    expect(calls.some((c) => c.keysym === 0x74 && c.down)).toBe(true); // 't' forwarded
  });
});

describe('InputEngine — release chord (bare Ctrl+Alt)', () => {
  it('bare Ctrl+Alt blurs; an AltGr key does NOT', () => {
    let chordCount = 0;
    const { client } = makeClient(false);
    const engine = createInputEngine({
      client: () => client as never,
      onReleaseChord: () => {
        chordCount++;
      },
      hostOSOverride: 'linux',
    });
    engine.handleKeyDown(kev('AltLeft', 'Alt', { ctrl: true, alt: true }));
    expect(chordCount).toBe(1);
    engine.handleKeyDown(kev('KeyQ', '@', { altGraph: true }));
    expect(chordCount).toBe(1); // AltGr did not blur
  });
});

describe('InputEngine — Mac→Windows navigation translations', () => {
  it('Cmd+Left → Home, Cmd+Right → End (line motion)', () => {
    const { engine, sequences } = engineFor('macos');
    engine.handleKeyDown(kev('ArrowLeft', 'ArrowLeft', { meta: true }));
    engine.handleKeyDown(kev('ArrowRight', 'ArrowRight', { meta: true }));
    expect(sequences).toContainEqual([XK_Home]);
    expect(sequences).toContainEqual([XK_End]);
  });

  it('Cmd+Up → Ctrl+Home, Cmd+Down → Ctrl+End (document motion)', () => {
    const { engine, sequences } = engineFor('macos');
    engine.handleKeyDown(kev('ArrowUp', 'ArrowUp', { meta: true }));
    engine.handleKeyDown(kev('ArrowDown', 'ArrowDown', { meta: true }));
    expect(sequences).toContainEqual([XK_Control_L, XK_Home]);
    expect(sequences).toContainEqual([XK_Control_L, XK_End]);
  });

  it('Shift extends the selection: Cmd+Shift+Left → Shift+Home', () => {
    const { engine, sequences } = engineFor('macos');
    engine.handleKeyDown(kev('ArrowLeft', 'ArrowLeft', { meta: true, shift: true }));
    expect(sequences).toContainEqual([XK_Shift_L, XK_Home]);
  });

  it('Option+Left/Right → Ctrl+Left/Right (word motion)', () => {
    const { engine, sequences } = engineFor('macos');
    engine.handleKeyDown(kev('ArrowLeft', 'ArrowLeft', { alt: true }));
    engine.handleKeyDown(kev('ArrowRight', 'ArrowRight', { alt: true }));
    expect(sequences).toContainEqual([XK_Control_L, XK_Left]);
    expect(sequences).toContainEqual([XK_Control_L, XK_Right]);
  });

  it('the translated arrow key-up is swallowed (no stray arrow release)', () => {
    const { engine, calls, sequences } = engineFor('macos');
    engine.handleKeyDown(kev('ArrowLeft', 'ArrowLeft', { meta: true }));
    const seqCount = sequences.length;
    calls.length = 0;
    engine.handleKeyUp(kev('ArrowLeft', 'ArrowLeft', {})); // Cmd already lifted
    expect(calls).toHaveLength(0);
    expect(sequences.length).toBe(seqCount);
  });

  it('a bare arrow (no Cmd/Option) is NOT translated', () => {
    const { engine, calls, sequences } = engineFor('macos');
    engine.handleKeyDown(kev('ArrowLeft', 'ArrowLeft', {}));
    expect(sequences).toHaveLength(0);
    expect(calls.some((c) => c.keysym === XK_Left && c.down)).toBe(true);
  });
});

describe('InputEngine — Mac word-delete idioms (Option+Delete)', () => {
  it('Option+Delete (e.key Backspace) → guest Ctrl+Backspace', () => {
    const { engine, sequences } = engineFor('macos');
    engine.handleKeyDown(kev('Backspace', 'Backspace', { alt: true }));
    expect(sequences).toContainEqual([XK_Control_L, XK_BackSpace]);
  });

  it('Option+Fn+Delete (e.key Delete) → guest Ctrl+Delete', () => {
    const { engine, sequences } = engineFor('macos');
    engine.handleKeyDown(kev('Delete', 'Delete', { alt: true }));
    expect(sequences).toContainEqual([XK_Control_L, XK_Delete]);
  });
});

describe('InputEngine — Mac Cmd+<printable> → Ctrl+<printable>', () => {
  it('KBD-5: Cmd+Shift+Z sends Ctrl + uppercase Z, not lowercase', () => {
    const { engine, calls } = engineFor('macos');
    engine.handleKeyDown(kev('KeyZ', 'Z', { meta: true, shift: true }));
    expect(calls.some((c) => c.keysym === XK_Control_L && c.down)).toBe(true);
    expect(calls.some((c) => c.keysym === 0x5a)).toBe(true); // 'Z'
    expect(calls.some((c) => c.keysym === 0x7a)).toBe(false); // not 'z'
  });

  it('KBD-7 "sssss": Cmd-up drains the printable whose key-up macOS swallowed', () => {
    const { engine, calls } = engineFor('macos');
    engine.handleKeyDown(kev('KeyS', 's', { meta: true })); // Ctrl_L down + 's' down
    engine.handleKeyUp(kev('MetaLeft', 'Meta', {})); // only the Meta-up arrives
    const s = calls.filter((c) => c.keysym === 0x73);
    expect(s.map((c) => c.down)).toEqual([true, false]); // 's' down then RELEASED
    const ctrl = calls.filter((c) => c.keysym === XK_Control_L);
    expect(ctrl.map((c) => c.down)).toEqual([true, false]); // synthetic Ctrl down then up
  });

  it('Cmd-up drain releases the printable but NOT a still-held Shift', () => {
    const { engine, calls } = engineFor('macos');
    engine.handleKeyDown(kev('ShiftLeft', 'Shift', { meta: true, shift: true }));
    engine.handleKeyDown(kev('KeyS', 'S', { meta: true, shift: true }));
    calls.length = 0;
    engine.handleKeyUp(kev('MetaLeft', 'Meta', { shift: true })); // Cmd up, Shift still held
    expect(calls.some((c) => c.keysym === 0x53 && !c.down)).toBe(true); // 'S' released
    expect(calls.some((c) => c.keysym === XK_Shift_L && !c.down)).toBe(false); // Shift NOT force-released
  });

  it('Cmd+<non-printable> (Cmd+Enter) is drained on Meta-up — no stuck Return, not remapped to Ctrl', () => {
    const { engine, calls } = engineFor('macos');
    engine.handleKeyDown(kev('Enter', 'Enter', { meta: true })); // bare XK_Return down, cached
    // macOS swallows the Enter key-up under Cmd; only the Meta-up arrives.
    engine.handleKeyUp(kev('MetaLeft', 'Meta', {}));
    const ret = calls.filter((c) => c.keysym === 0xff0d);
    expect(ret.map((c) => c.down)).toEqual([true, false]); // Return down then drained
    expect(calls.some((c) => c.keysym === XK_Control_L)).toBe(false); // Cmd+Enter is NOT Cmd→Ctrl
  });
});

describe('InputEngine — stuck-modifier reconciliation', () => {
  it('releases a tracked modifier the host no longer holds, before the next key', () => {
    const { engine, calls } = engineFor('macos');
    engine.handleKeyDown(kev('ControlLeft', 'Control', { ctrl: true })); // Ctrl down (tracked)
    calls.length = 0;
    engine.handleKeyDown(kev('Digit1', '1', {})); // host no longer holds Ctrl
    expect(calls.some((c) => c.keysym === XK_Control_L && !c.down)).toBe(true); // Ctrl released
    expect(calls.some((c) => c.keysym === 0x31 && c.down)).toBe(true); // digit clean
  });

  it('does NOT release a modifier the host is still holding', () => {
    const { engine, calls } = engineFor('macos');
    engine.handleKeyDown(kev('ControlLeft', 'Control', { ctrl: true }));
    calls.length = 0;
    engine.handleKeyDown(kev('Tab', 'Tab', { ctrl: true })); // Ctrl still held
    expect(calls.some((c) => c.keysym === XK_Control_L && !c.down)).toBe(false);
  });
});

describe('InputEngine — macOS lock-key sync', () => {
  it('CapsLock divergence still synthesizes a toggle on Mac', () => {
    const { engine, calls } = engineFor('macos');
    engine.handleKeyDown(kev('KeyA', 'a', { caps: false })); // baseline lastCapsLock=false
    calls.length = 0;
    engine.handleKeyDown(kev('KeyB', 'b', { caps: true })); // CapsLock now engaged
    expect(calls.some((c) => c.keysym === 0xffe5)).toBe(true); // XK_CapsLock toggle
  });

  it('NumLock is NOT synced on Mac (getModifierState is unreliable there)', () => {
    const { engine, calls } = engineFor('macos');
    engine.handleKeyDown(kev('KeyA', 'a', { num: false }));
    engine.handleKeyDown(kev('KeyB', 'b', { num: true })); // would diverge — but Mac skips it
    expect(calls.some((c) => c.keysym === 0xff7f)).toBe(false); // no XK_NumLock toggle
  });

  it('NumLock IS synced on Windows (the skip is Mac-only)', () => {
    const { engine, calls } = engineFor('windows');
    engine.handleKeyDown(kev('KeyA', 'a', { num: false })); // baseline lastNumLock=false
    calls.length = 0;
    engine.handleKeyDown(kev('KeyB', 'b', { num: true })); // diverge → toggle
    expect(calls.some((c) => c.keysym === 0xff7f)).toBe(true); // XK_NumLock toggle
  });
});

describe('InputEngine — IME composition', () => {
  it('raw keydown is suppressed while composing', () => {
    const { engine, calls } = engineFor('macos');
    engine.handleCompositionStart();
    engine.handleKeyDown(kev('KeyE', 'e', {})); // would be raw — suppressed
    expect(calls).toHaveLength(0);
  });

  it('a keyCode 229 ("processed") keydown is suppressed even outside composition', () => {
    const { engine, calls } = engineFor('macos');
    engine.handleKeyDown(kev('KeyA', 'a', { keyCode: 229 }));
    expect(calls).toHaveLength(0);
  });

  it('compositionEnd types the final text; newline → Return', () => {
    const { engine, calls } = engineFor('macos');
    engine.handleCompositionStart();
    engine.handleCompositionEnd('ab\n');
    expect(calls.some((c) => c.keysym === 0x61)).toBe(true); // a
    expect(calls.some((c) => c.keysym === 0x62)).toBe(true); // b
    expect(calls.some((c) => c.keysym === 0xff0d)).toBe(true); // Return
  });

  it('compositionEnd sends Tab/Return as their keysyms directly — never the 0x0100ff_ mangle', () => {
    // Regression guard for the typeLiteral→unicodeKeysym mangle: XK_Tab (0xff09)
    // and XK_Return (0xff0d) are keysyms, not codepoints, so they must NOT get
    // the direct-Unicode 0x01000000 offset added.
    const { engine, calls } = engineFor('macos');
    engine.handleCompositionStart();
    engine.handleCompositionEnd('a\tb\n');
    expect(calls.some((c) => c.keysym === 0xff09)).toBe(true); // Tab
    expect(calls.some((c) => c.keysym === 0xff0d)).toBe(true); // Return
    expect(calls.some((c) => c.keysym === 0x01000000 + 0xff09)).toBe(false); // not mangled
    expect(calls.some((c) => c.keysym === 0x01000000 + 0xff0d)).toBe(false);
  });

  it('compositionEnd emits the correct wire keysym for non-ASCII (wire-form only, not guest arrival)', () => {
    // Latin-1 'é' rides as the bare codepoint 0x00e9; a BMP CJK glyph rides as the
    // X.Org direct-Unicode keysym 0x01000000+cp. This asserts the EMITTED keysym
    // only — whether an en-us guest renders it is the Tier-2 agent's job.
    const { engine, calls } = engineFor('macos');
    engine.handleCompositionStart();
    engine.handleCompositionEnd('é中');
    expect(calls.some((c) => c.keysym === 0x00e9)).toBe(true); // é, no offset
    expect(calls.some((c) => c.keysym === 0x01000000 + 0x4e2d)).toBe(true); // 中, direct-Unicode
  });
});
