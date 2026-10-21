/**
 * KeyboardEvent.code → AT/PC set-1 scancode for the QEMU Extended Key Event
 * extension.
 *
 * Why this exists: VNC's plain CLI_MSG_KEY_EVENT only carries an X11 keysym.
 * That loses the physical key the operator pressed, so the guest OS can't apply
 * its own layout, can't see scan-code-only keys (Pause/SysRq variants), and
 * pre-OS firmware (UEFI/BIOS menus) — which reads raw scancodes before any
 * layout loads — gets nothing. QEMU's `-258` pseudo-encoding adds a raw keycode
 * field; this map fills it from `KeyboardEvent.code`.
 *
 * WIRE FORMAT — read this before "correcting" a value:
 *
 *   The keycode field QEMU expects is an **AT / PC scan-code set 1** value
 *   ("XT scancode" / QEMU "qnum"), NOT a Linux evdev keycode. QEMU's
 *   ui/vnc.c `do_key_event` feeds the field straight into
 *   `qemu_input_key_number_to_qcode()`, which is the qnum (AT set-1) space.
 *   A two-byte "grey"/extended scancode `0xE0 0xNN` is folded into ONE byte by
 *   SETTING THE HIGH BIT of the second byte (`0xNN | 0x80`) — e.g. ArrowLeft
 *   `0xE0 0x4B` → `0xCB`. This matches noVNC core/input/xtscancodes.js +
 *   getRFBkeycode and the RFB QEMU-Extended-Key-Event spec.
 *
 *   For the base typewriter / F1–F12 / non-grey-numpad keys the AT set-1 value
 *   happens to EQUAL the Linux evdev keycode (1..88), which is why an earlier
 *   evdev table "worked" for letters and digits — but every extended key
 *   (arrows, nav island, right Ctrl/Alt, Meta, Menu, KP-Enter, KP-/, Pause,
 *   PrintScreen) and the JIS Yen/Ro keys diverge, and evdev there indexes a
 *   garbage qcode. The values below are the FINAL folded wire bytes (no further
 *   folding happens in client.ts sendExtendedKey — this table is authoritative).
 *
 *   NOTE: on AHV today the upstream QEMU is launched with `-k`/keyboard-layout
 *   (en-us), so `ext_key_event()` reverse-maps the KEYSYM and DISCARDS this
 *   scancode — the field is correctness insurance for firmware screens, a future
 *   `-k` drop, or another VNC backend, not a live behavior change there. We send
 *   it correct regardless.
 *
 * Source of truth: noVNC core/input/xtscancodes.js (auto-generated from
 * keycodemapdb `keymaps.csv … atset1`). We mirror the keys on standard
 * 104/105-key boards plus the Apple/JIS extras a Mac host emits; rarer keys
 * round-trip via the keysym path (keycode 0 → keysym-only).
 */

import { hostOS, type HostOS } from './HostOS';

const KEY_RESERVED = 0;

/**
 * Base lookup table — AT set-1 folded wire bytes keyed by W3C UI Events
 * `KeyboardEvent.code` (UI Events Level 3 §10.4). Each entry's trailing comment
 * gives the unfolded AT set-1 value where it differs (0xE0-prefixed keys), so a
 * reviewer can check the fold without a separate table.
 */
const codeToKeycode: Readonly<Record<string, number>> = Object.freeze({
  // Letters (AT set-1 == evdev here)
  KeyA: 0x1e,
  KeyB: 0x30,
  KeyC: 0x2e,
  KeyD: 0x20,
  KeyE: 0x12,
  KeyF: 0x21,
  KeyG: 0x22,
  KeyH: 0x23,
  KeyI: 0x17,
  KeyJ: 0x24,
  KeyK: 0x25,
  KeyL: 0x26,
  KeyM: 0x32,
  KeyN: 0x31,
  KeyO: 0x18,
  KeyP: 0x19,
  KeyQ: 0x10,
  KeyR: 0x13,
  KeyS: 0x1f,
  KeyT: 0x14,
  KeyU: 0x16,
  KeyV: 0x2f,
  KeyW: 0x11,
  KeyX: 0x2d,
  KeyY: 0x15,
  KeyZ: 0x2c,

  // Digit row
  Digit1: 0x02,
  Digit2: 0x03,
  Digit3: 0x04,
  Digit4: 0x05,
  Digit5: 0x06,
  Digit6: 0x07,
  Digit7: 0x08,
  Digit8: 0x09,
  Digit9: 0x0a,
  Digit0: 0x0b,

  // Function row F1–F12
  F1: 0x3b,
  F2: 0x3c,
  F3: 0x3d,
  F4: 0x3e,
  F5: 0x3f,
  F6: 0x40,
  F7: 0x41,
  F8: 0x42,
  F9: 0x43,
  F10: 0x44,
  F11: 0x57,
  F12: 0x58,
  // Extended function row F13–F24 (Apple Extended / PC 122-key). Host-independent.
  // F17/F18/F19/F22 are grey (folded); the rest are base AT set-1.
  F13: 0x5d,
  F14: 0x5e,
  F15: 0x5f,
  F16: 0x55,
  F17: 0x83, // 0xe003 -> |0x80
  F18: 0xf7, // 0xe077 -> |0x80
  F19: 0x84, // 0xe004 -> |0x80
  F20: 0x5a,
  F21: 0x74,
  F22: 0xf9, // 0xe079 -> |0x80
  F23: 0x6d,
  F24: 0x6f,

  // Symbol keys (AT set-1 == evdev here)
  Minus: 0x0c,
  Equal: 0x0d,
  BracketLeft: 0x1a,
  BracketRight: 0x1b,
  Semicolon: 0x27,
  Quote: 0x28,
  Backquote: 0x29,
  Backslash: 0x2b,
  Comma: 0x33,
  Period: 0x34,
  Slash: 0x35,
  IntlBackslash: 0x56, // <> key on ISO layouts (KEY_102ND)
  // JIS physical keys — AT set-1 DIFFERS from evdev (124/89). Present on any
  // host with a JIS keyboard, so they live in the base table.
  IntlRo: 0x73, // JIS ろ/_  (evdev KEY_RO 89)
  IntlYen: 0x7d, // JIS ¥/|  (evdev KEY_YEN 124)

  // Control row
  Escape: 0x01,
  Backspace: 0x0e,
  Tab: 0x0f,
  Enter: 0x1c,
  ControlLeft: 0x1d,
  ShiftLeft: 0x2a,
  ShiftRight: 0x36,
  AltLeft: 0x38,
  Space: 0x39,
  CapsLock: 0x3a,
  NumLock: 0x45,
  ScrollLock: 0x46,
  // Grey/extended (0xE0-prefixed → folded)
  ControlRight: 0x9d, // 0xe01d
  AltRight: 0xb8, // 0xe038
  MetaLeft: 0xdb, // 0xe05b
  OSLeft: 0xdb, // Firefox legacy alias of MetaLeft
  MetaRight: 0xdc, // 0xe05c
  OSRight: 0xdc,
  ContextMenu: 0xdd, // 0xe05d (KEY_COMPOSE / Menu)

  // Numeric keypad (base AT set-1, except the two grey keys)
  Numpad0: 0x52,
  Numpad1: 0x4f,
  Numpad2: 0x50,
  Numpad3: 0x51,
  Numpad4: 0x4b,
  Numpad5: 0x4c,
  Numpad6: 0x4d,
  Numpad7: 0x47,
  Numpad8: 0x48,
  Numpad9: 0x49,
  NumpadDecimal: 0x53,
  NumpadAdd: 0x4e,
  NumpadSubtract: 0x4a,
  NumpadMultiply: 0x37,
  NumpadEqual: 0x59,
  NumpadEnter: 0x9c, // 0xe01c (grey)
  NumpadDivide: 0xb5, // 0xe035 (grey)

  // Navigation island (all grey/extended → folded)
  Insert: 0xd2, // 0xe052
  Home: 0xc7, // 0xe047
  PageUp: 0xc9, // 0xe049
  Delete: 0xd3, // 0xe053
  End: 0xcf, // 0xe04f
  PageDown: 0xd1, // 0xe051
  ArrowUp: 0xc8, // 0xe048
  ArrowLeft: 0xcb, // 0xe04b
  ArrowRight: 0xcd, // 0xe04d
  ArrowDown: 0xd0, // 0xe050

  // Misc
  PrintScreen: 0x54, // base AT set-1 (KEY_SYSRQ), NOT grey
  Pause: 0xc6, // 0xe046 (grey)
});

/**
 * macOS-only code→scancode overrides. Consulted ONLY when host==='macos',
 * because these DOM codes are predominantly emitted by Apple keyboards (the
 * keypad Clear, Help, Eject, Power, JIS Lang1/Lang2, the function-row media
 * keys). Gating them behind the host keeps the base table aligned with the W3C
 * code values and avoids surprising a PC operator whose external keyboard emits
 * one of these.
 *
 * `Fn` is DELIBERATELY ABSENT: Chromium fires no KeyboardEvent for it and there
 * is no scancode. NO Backquote↔IntlBackslash swap: modern Chromium reports
 * standard W3C codes on Mac ISO keyboards (matching the base table).
 */
const macCodeToKeycode: Readonly<Record<string, number>> = Object.freeze({
  NumpadClear: 0x45, // Mac keypad Clear/Cancel — sits in the PC NumLock position
  Help: 0xf5, // 0xe075 (Apple Extended Help, PC Insert position)
  Eject: 0xfd, // 0xe07d
  Power: 0xde, // 0xe05e
  Lang1: 0x72, // JIS かな / Korean Hangul (base AT set-1)
  Lang2: 0x71, // JIS 英数 / Korean Hanja (base AT set-1)
  AudioVolumeMute: 0xa0, // 0xe020
  AudioVolumeDown: 0xae, // 0xe02e
  AudioVolumeUp: 0xb0, // 0xe030
  MediaPlayPause: 0xa2, // 0xe022
  MediaStop: 0xa4, // 0xe024
  MediaTrackPrevious: 0x90, // 0xe010
  MediaTrackNext: 0x99, // 0xe019
});

/**
 * Translate `KeyboardEvent.code` to its AT set-1 (folded) scancode for the QEMU
 * Extended Key Event message. Returns 0 (KEY_RESERVED) when we have no mapping;
 * the wire frame treats 0 as "no raw code available" and the server falls back
 * to the keysym-only interpretation.
 *
 * `host` defaults to the detected host so existing single-arg callers are
 * unchanged; the InputEngine passes its resolved host (honoring hostOSOverride)
 * so behavior is deterministic in tests.
 */
export function eventCodeToKeycode(code: string, host: HostOS = hostOS()): number {
  if (host === 'macos') {
    const mac = macCodeToKeycode[code];
    if (mac !== undefined) return mac;
  }
  return codeToKeycode[code] ?? KEY_RESERVED;
}

/**
 * Boolean form for callers that just want to know whether a mapping exists.
 */
export function hasMapping(code: string, host: HostOS = hostOS()): boolean {
  if (host === 'macos' && code in macCodeToKeycode) return true;
  return code in codeToKeycode;
}
