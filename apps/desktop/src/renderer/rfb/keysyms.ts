/**
 * X11 keysym mapping for RFB key events.
 *
 * RFB (RFC 6143 §7.5.4) carries keysyms as 32-bit values defined by X11.
 * The mapping is partial — we cover the keys a remote operator actually uses,
 * including the function keys, navigation keys, and the modifier+letter
 * combinations that the OS would normally intercept.
 *
 * For printable ASCII the keysym equals the Unicode code point. For
 * non-printables we map from `KeyboardEvent.code` (a layout-independent
 * physical-key identifier) to keep the binding stable across layouts.
 */

// KEYSYM_HC_FOCUS_GRAB (XK_Pause = 0xff13) removed in Phase 3.4
// cleanup (May 2026). Used to signal the bridge's LL keyboard hook
// that a v3 keystroke-session was about to start; the keystroke
// path has been fully retired in favour of inline + chunked-inline
// ANNOUNCEs on the pointer-PHY channel. Git history preserves the
// selection-history narrative (XF86 vendor extension → XK_F19 →
// XK_Pause) if needed. XK_Pause itself is still defined below
// (line 101) because Pause-key passthrough remains an operator
// input gesture.

/**
 * Base for X.Org's "direct Unicode" keysym scheme. Per the X11
 * keysym registry, keysyms in the range 0x01000100 .. 0x0110FFFF
 * directly encode Unicode codepoints (0x100 .. 0x10FFFF) by
 * subtracting this base. Used for the v3 data-plane text stream:
 * the renderer sends one keysym per codepoint, the guest-side
 * Windows keyboard layer translates it to WM_CHAR.
 *
 * For BMP ASCII (codepoints < 0x100) the legacy convention is to
 * send the codepoint as-is (e.g. 'a' = 0x61), which matches the
 * X11 "latin1" range; helpers below pick the right form.
 */
export const X11_UNICODE_KEYSYM_BASE = 0x01000000;

/**
 * Convert a Unicode codepoint to the X11 keysym the bridge's
 * TranslateMessage path will turn into the matching WM_CHAR.
 *
 *  * Codepoints < 0x100 (Latin-1 range) → keysym = codepoint.
 *    This matches X11's historic Latin-1 keysym block and is what
 *    every VNC viewer does for printable ASCII.
 *  * Codepoints ≥ 0x100 → keysym = 0x01000000 + codepoint.
 *    This is the X.Org "direct Unicode" extension, supported by
 *    QEMU's keymap layer.
 *  * Codepoints outside [0, 0x10FFFF] are invalid Unicode and
 *    rejected.
 */
export function unicodeKeysym(codepoint: number): number {
  if (!Number.isInteger(codepoint) || codepoint < 0 || codepoint > 0x10ffff) {
    throw new RangeError(`unicodeKeysym: codepoint out of Unicode range: ${codepoint}`);
  }
  return codepoint < 0x100 ? codepoint : X11_UNICODE_KEYSYM_BASE + codepoint;
}

export const XK_BackSpace = 0xff08;
export const XK_Tab = 0xff09;
export const XK_Return = 0xff0d;
export const XK_Escape = 0xff1b;
export const XK_Insert = 0xff63;
export const XK_Delete = 0xffff;
export const XK_Home = 0xff50;
export const XK_End = 0xff57;
export const XK_PageUp = 0xff55;
export const XK_PageDown = 0xff56;
export const XK_Left = 0xff51;
export const XK_Up = 0xff52;
export const XK_Right = 0xff53;
export const XK_Down = 0xff54;
export const XK_F1 = 0xffbe;
export const XK_Print = 0xff61;
export const XK_Pause = 0xff13;
export const XK_Menu = 0xff67;
export const XK_NumLock = 0xff7f;
export const XK_ScrollLock = 0xff14;
export const XK_CapsLock = 0xffe5;

// Modifiers (left/right pairs).
export const XK_Shift_L = 0xffe1;
export const XK_Shift_R = 0xffe2;
export const XK_Control_L = 0xffe3;
export const XK_Control_R = 0xffe4;
export const XK_Alt_L = 0xffe9;
export const XK_Alt_R = 0xffea;
export const XK_Super_L = 0xffeb;
export const XK_Super_R = 0xffec;

// AltGr (third-level shift). On Windows, AltGr is delivered as a phantom
// ControlLeft keydown immediately followed by AltRight; the InputEngine
// coalesces that pair into this single keysym so the guest sees real AltGr
// instead of a stuck Ctrl. (Linux delivers a genuine AltGraph modifier, so no
// coalescing is needed there.)
export const XK_ISO_Level3_Shift = 0xfe03;

// Sysreq / Compose / Multi-key — useful for kernel "magic SysRq" sequences.
export const XK_Sys_Req = 0xff15;

// Spacebar.
export const XK_space = 0x0020;

// Numeric keypad + Clear/Help/Lang keysyms (X11 keysymdef.h). The keypad
// keysyms give QEMU a correct fallback (and make these keys work in
// keysym-only mode); the scancode path carries the authoritative keycode.
export const XK_Clear = 0xff0b; // macOS keypad Clear ("cancel") key
export const XK_KP_0 = 0xffb0; // XK_KP_1..9 = XK_KP_0 + n
export const XK_KP_Decimal = 0xffae;
export const XK_KP_Add = 0xffab;
export const XK_KP_Subtract = 0xffad;
export const XK_KP_Multiply = 0xffaa;
export const XK_KP_Divide = 0xffaf;
export const XK_KP_Equal = 0xffbd;
export const XK_Help = 0xff6a;
export const XK_Hangul = 0xff31; // Lang1
export const XK_Hangul_Hanja = 0xff34; // Lang2

// X11 dead-key keysyms (X11/keysymdef.h). A dead/accent key on the host
// (´ ` ^ ~ ¨) arrives as KeyboardEvent.key === 'Dead'; we forward the
// matching dead keysym so the GUEST's keyboard layout composes the accent
// with the next character. Without this the dead key produced no keysym and
// was silently dropped — the "dead button" operators reported.
export const XK_dead_grave = 0xfe50;
export const XK_dead_acute = 0xfe51;
export const XK_dead_circumflex = 0xfe52;
export const XK_dead_tilde = 0xfe53;
export const XK_dead_diaeresis = 0xfe57;

// Dead-key resolution keyed on the layout-independent physical `code`. These
// placements match the common IT/DE/FR/ES/PT-BR/CH keyboards. Best-effort:
// the host layout determines which physical key is the dead key, so this is
// approximate, but every entry composes correctly on a matching guest layout.
const DEAD_BY_CODE: Record<string, number> = {
  Backquote: XK_dead_grave,
  Equal: XK_dead_acute,
  BracketLeft: XK_dead_circumflex,
  BracketRight: XK_dead_tilde,
  Quote: XK_dead_diaeresis,
};

/**
 * Map a KeyboardEvent → X11 keysym. Returns 0 on miss so the caller can skip.
 *
 * We prefer the `code` (physical) attribute for non-printables so that, e.g.,
 * Backspace works regardless of locale. For printables we fall back to
 * `key.charCodeAt(0)` (KeyboardEvent.key reflects the typed character).
 */
export function eventToKeysym(e: KeyboardEvent): number {
  // Physical keys we always know.
  switch (e.code) {
    case 'Backspace':
      return XK_BackSpace;
    case 'Tab':
      return XK_Tab;
    case 'Enter':
    case 'NumpadEnter':
      return XK_Return;
    case 'Escape':
      return XK_Escape;
    case 'Insert':
      return XK_Insert;
    case 'Delete':
      return XK_Delete;
    case 'Home':
      return XK_Home;
    case 'End':
      return XK_End;
    case 'PageUp':
      return XK_PageUp;
    case 'PageDown':
      return XK_PageDown;
    case 'ArrowLeft':
      return XK_Left;
    case 'ArrowUp':
      return XK_Up;
    case 'ArrowRight':
      return XK_Right;
    case 'ArrowDown':
      return XK_Down;
    case 'PrintScreen':
      return XK_Print;
    case 'Pause':
      return XK_Pause;
    case 'ContextMenu':
      return XK_Menu;
    case 'NumLock':
      return XK_NumLock;
    case 'ScrollLock':
      return XK_ScrollLock;
    case 'CapsLock':
      return XK_CapsLock;
    case 'ShiftLeft':
      return XK_Shift_L;
    case 'ShiftRight':
      return XK_Shift_R;
    case 'ControlLeft':
      return XK_Control_L;
    case 'ControlRight':
      return XK_Control_R;
    case 'AltLeft':
      return XK_Alt_L;
    case 'AltRight':
      return XK_Alt_R;
    case 'MetaLeft':
    case 'OSLeft':
      return XK_Super_L;
    case 'MetaRight':
    case 'OSRight':
      return XK_Super_R;
    case 'Space':
      return XK_space;
    // macOS keypad Clear ("cancel" key) + the rest of the numpad. e.key for
    // these is 'Clear' (len 5) or a length-1 digit when NumLock is on, so the
    // code switch must name them or they fall through to 0 / the wrong keysym.
    case 'NumpadClear':
      return XK_Clear;
    case 'Numpad0':
      return XK_KP_0;
    case 'Numpad1':
      return XK_KP_0 + 1;
    case 'Numpad2':
      return XK_KP_0 + 2;
    case 'Numpad3':
      return XK_KP_0 + 3;
    case 'Numpad4':
      return XK_KP_0 + 4;
    case 'Numpad5':
      return XK_KP_0 + 5;
    case 'Numpad6':
      return XK_KP_0 + 6;
    case 'Numpad7':
      return XK_KP_0 + 7;
    case 'Numpad8':
      return XK_KP_0 + 8;
    case 'Numpad9':
      return XK_KP_0 + 9;
    case 'NumpadDecimal':
      return XK_KP_Decimal;
    case 'NumpadAdd':
      return XK_KP_Add;
    case 'NumpadSubtract':
      return XK_KP_Subtract;
    case 'NumpadMultiply':
      return XK_KP_Multiply;
    case 'NumpadDivide':
      return XK_KP_Divide;
    case 'NumpadEqual':
      return XK_KP_Equal;
    case 'Help':
      return XK_Help;
    case 'Lang1':
      return XK_Hangul;
    case 'Lang2':
      return XK_Hangul_Hanja;
  }
  // Function keys F1–F24.
  if (e.code.startsWith('F') && /^F\d+$/.test(e.code)) {
    const n = parseInt(e.code.slice(1), 10);
    if (n >= 1 && n <= 24) return XK_F1 + (n - 1);
  }
  // Dead/accent keys arrive as key === 'Dead'. Forward the X11 dead keysym so
  // the guest composes the accent; previously these fell through to 0 (the
  // length===1 guard below) and were dropped.
  if (e.key === 'Dead') {
    return DEAD_BY_CODE[e.code] ?? XK_dead_grave;
  }
  // Printable: the typed character's Unicode code point IS the keysym for
  // Latin-1 / Basic Multilingual Plane chars.
  if (e.key.length === 1) {
    const cp = e.key.codePointAt(0);
    if (cp !== undefined) return cp;
  }
  return 0;
}
