/**
 * InputEngine — the renderer's single source of truth for keyboard
 * and mouse events delivered to the guest VM.
 *
 * Replaces the scattered effect bodies that previously lived directly
 * in ConsoleView.tsx (modifier set, fakeCtrlActive flag, AltGr
 * coalescing, isReleaseChord predicate, mouse-move rAF coalescing).
 * Pulled out so that:
 *
 *   - state ownership is explicit. Everything lives behind
 *     `createInputEngine` and can be inspected via `snapshot()`.
 *   - the blur watchdog (window-level, not canvas-level) can call
 *     `releaseAllModifiers()` without poking into private fields.
 *   - the unit-test surface area is a pure event-in / wire-call-out
 *     class, with no DOM dependency beyond the event objects.
 *
 * The engine does NOT own DOM listener registration; the caller
 * (`ConsoleView` or pop-out `ConsoleWindowApp`) is responsible for
 * attaching/detaching keyboard + mouse listeners on the canvas, and
 * forwards events into the engine via `handleKeyDown` /
 * `handleKeyUp` / `handlePointer*`. That separation lets us swap the
 * canvas underneath (during pop-out / re-attach) without losing
 * engine state, and keeps the engine free of React-specific
 * lifecycle assumptions.
 *
 * MODEL (see docs/INTERACTION-ARCH.md §4): per key we send the X11
 * KEYSYM derived from `KeyboardEvent.key` (the character the host
 * layout already resolved) plus, in ext-key mode, the AT-set-1
 * scancode from `KeyboardEvent.code`. The GUEST owns layout
 * interpretation — there is deliberately NO host-keyboard-layout
 * remapping here (that path corrupts a mixed-locale fleet and can't
 * widen the achievable character set past the en-us keysym ceiling;
 * true non-ASCII input is the in-guest agent's job). This mirrors
 * noVNC, which is what the Prism web console uses.
 */

import type { RFBClient } from '../rfb/client';
import {
  eventToKeysym,
  unicodeKeysym,
  XK_Alt_L,
  XK_Alt_R,
  XK_BackSpace,
  XK_CapsLock,
  XK_Control_L,
  XK_Control_R,
  XK_Delete,
  XK_End,
  XK_Home,
  XK_ISO_Level3_Shift,
  XK_Left,
  XK_NumLock,
  XK_Right,
  XK_Shift_L,
  XK_Shift_R,
  XK_Super_L,
  XK_Super_R,
} from '../rfb/keysyms';
import { BTN_WHEEL_DOWN, BTN_WHEEL_UP } from '../rfb/protocol';
import { asGuestLayoutId, guestKeyFor, type GuestKey, type GuestLayoutId } from './GuestLayouts';
import { hostOS, type HostOS } from './HostOS';
import { eventCodeToKeycode } from './Keycodes';
import {
  createModifierTracker,
  modifierFromKey,
  RELEASE_KEYSYM,
  type LogicalModifier,
  type ModifierTracker,
} from './Modifiers';

// AT-set-1 scancode for the modifier/lock keysyms we emit SYNTHETICALLY (Cmd→Ctrl,
// the Windows AltGr coalesce, blur releases, lock-key sync). In QEMU
// Extended-Key (scancode) mode the wire frame's keycode field is read first;
// AHV's QEMU does not reliably fall back to the keysym when the keycode is 0, so
// a synthetic emission with keycode 0 is DROPPED — which silently broke Cmd+C/
// Cmd+V. We look the real scancode up here so synthetics carry one. Reuses the
// verified code→scancode table (host-independent for these physical keys).
const SYNTHETIC_KEYCODE: Readonly<Record<number, number>> = {
  [XK_Control_L]: eventCodeToKeycode('ControlLeft'),
  [XK_Control_R]: eventCodeToKeycode('ControlRight'),
  [XK_Shift_L]: eventCodeToKeycode('ShiftLeft'),
  [XK_Shift_R]: eventCodeToKeycode('ShiftRight'),
  [XK_Alt_L]: eventCodeToKeycode('AltLeft'),
  [XK_Alt_R]: eventCodeToKeycode('AltRight'),
  [XK_Super_L]: eventCodeToKeycode('MetaLeft'),
  [XK_Super_R]: eventCodeToKeycode('MetaRight'),
  [XK_CapsLock]: eventCodeToKeycode('CapsLock'),
  [XK_NumLock]: eventCodeToKeycode('NumLock'),
};

// Modifier keysyms — the keys whose key-UP macOS DOES deliver even while Cmd is
// held. KBD-7: when we drain downKeys on Cmd/Meta-up (to release the printables
// whose key-up macOS swallowed), we must skip these — a still-held Shift must
// not be force-released out from under the operator; it gets its own real keyup.
const MODIFIER_KEYSYMS: ReadonlySet<number> = new Set<number>([
  XK_Shift_L,
  XK_Shift_R,
  XK_Control_L,
  XK_Control_R,
  XK_Alt_L,
  XK_Alt_R,
  XK_Super_L,
  XK_Super_R,
  XK_CapsLock,
  XK_NumLock,
]);

export interface InputEngineOptions {
  /**
   * Lazily-resolved RFB client. The engine never caches the client
   * across event ticks — we always go through this accessor so the
   * caller can swap clients (pop-out / reconnect) without telling
   * the engine.
   */
  client: () => RFBClient | null;
  /**
   * Called when the operator presses the bare Ctrl+Alt release
   * chord. The caller is responsible for taking whatever action
   * "release input capture" should mean (blur the canvas, dismiss
   * pointer lock, etc.). The engine itself only stops forwarding
   * keys until the caller re-attaches focus.
   */
  onReleaseChord: () => void;
  /**
   * Optional override — production runs use the real host detection,
   * tests can pin to a specific platform.
   */
  hostOSOverride?: HostOS;
  /**
   * Force-drop ALL `KeyboardEvent.repeat === true` keydowns. Defaults to
   * FALSE. QEMU's emulated keyboard does NOT typematic-repeat on its own, so a
   * held key only auto-repeats in the guest if we forward each browser repeat as
   * another key-down (make) — the hardware-faithful pattern (repeated makes, one
   * break on release). The engine therefore forwards repeats for printable /
   * navigation / editing keys and drops them only for MODIFIER keys (a held
   * modifier is a single make; repeated makes are noise). Set this true to
   * suppress host-driven repeat entirely.
   */
  suppressRepeat?: boolean;
}

export interface InputEngineSnapshot {
  modifiers: Record<LogicalModifier, boolean>;
  fakeCtrlActive: boolean;
  host: HostOS;
}

export interface InputEngine {
  /** Forward a KeyboardEvent.keydown. */
  handleKeyDown(e: KeyboardEvent): void;
  /** Forward a KeyboardEvent.keyup. */
  handleKeyUp(e: KeyboardEvent): void;
  /**
   * Forward a CompositionEvent.compositionstart. Engine begins
   * suppressing keydown/keyup emission to the guest until the
   * matching compositionend fires; the host browser owns the
   * intermediate state and we don't want the guest to see
   * dead-key triggers / IME pre-edit keystrokes as raw input.
   */
  handleCompositionStart(): void;
  /**
   * Forward a CompositionEvent.compositionupdate — we ignore the
   * intermediate text and only act on compositionend. Stub kept
   * symmetric so callers can wire all three events.
   */
  handleCompositionUpdate(text: string): void;
  /**
   * Forward a CompositionEvent.compositionend. Engine emits the
   * final composed text as a sequence of keysym down/up pairs
   * (codepoint-as-keysym for printables; Return for newlines).
   * Resumes normal keyboard handling after.
   */
  handleCompositionEnd(text: string): void;
  /**
   * Force-release every modifier the engine believes the guest is
   * holding. Called by the blur watchdog (window blur, document
   * hidden) and on canvas blur. Idempotent.
   */
  releaseAllModifiers(): LogicalModifier[];
  /**
   * Declare the GUEST VM's keyboard layout (operator-chosen via the globe
   * picker, persisted per VM). Typed characters are translated into the key
   * sequences THAT layout uses — the engine never forces a specific layout;
   * unknown ids fall back to en-us. See GuestLayouts.ts.
   */
  setGuestLayout(id: string): void;
  /**
   * Send a pointer event. Coalescing for high-frequency `move`
   * events lives at the caller-supplied schedule layer; the engine
   * just unifies the wire format.
   */
  sendPointer(x: number, y: number, buttonMask: number): void;
  /**
   * Send a wheel event. Synchronous so click latency stays low;
   * scrolling is semantic at the per-tick level.
   */
  sendWheel(x: number, y: number, buttonMask: number, deltaY: number): void;
  /** Snapshot for debugging / tests. */
  snapshot(): InputEngineSnapshot;
  /** Dispose of any held state; idempotent. */
  dispose(): void;
}

export function createInputEngine(opts: InputEngineOptions): InputEngine {
  const host = opts.hostOSOverride ?? hostOS();
  const isMac = host === 'macos';
  const suppressRepeat = opts.suppressRepeat ?? false;

  const modifiers: ModifierTracker = createModifierTracker();
  // Mac→PC remap state: when Cmd is held on a Mac host we suppress the
  // Meta keysym (which would land as the Win key on a Windows guest)
  // and emit Control_L instead, so Cmd+C / Cmd+V / Cmd+X / Cmd+Z behave
  // the way a Mac operator expects in a PC guest. `fakeCtrl` tracks
  // whether the synthetic Control_L is currently held so we know to
  // release it when Cmd goes up.
  let fakeCtrl = false;

  // The GUEST VM's keyboard layout, as DECLARED by the operator (the globe
  // picker, persisted per VM — never auto-detected, never forced). Drives the
  // character → key-sequence translation; see GuestLayouts.ts. ConsoleView
  // pushes the picker value via setGuestLayout() on mount and on change.
  let guestLayout: GuestLayoutId = 'us';

  // Per-event raw scancode captured by handleKeyDown/Up so the Cmd→Ctrl and
  // AltGr paths can still go through `send()` while carrying the matching
  // AT-set-1 keycode. Sticky across nested send() calls during a single event —
  // cleared at the top of each handler. Synthetic modifier emissions
  // (activeKeycode 0) fall back to SYNTHETIC_KEYCODE.
  let activeKeycode = 0;

  // ── Windows AltGr coalescing state ─────────────────────────────────────────
  // Windows synthesizes a PHANTOM ControlLeft keydown immediately before
  // AltRight when AltGr is pressed. Forwarding that Ctrl makes AltGr+key arrive
  // as Ctrl+key on the guest (and the phantom Ctrl's key-up comes out-of-band,
  // so it sticks). We DEFER a fresh Windows ControlLeft and let the NEXT keydown
  // resolve it: while still armed (within the 100ms arm window) an AltRight ⇒
  // real AltGr (one ISO_Level3_Shift, no Ctrl); anything else ⇒ the Ctrl was
  // real, flush it. (We resolve AltRight as AltGr regardless of the precise gap,
  // not just <50ms: a slow phantom-Ctrl→AltRight delivery over an RDP/KVM input
  // pipeline must still coalesce, or the symbol lands as Ctrl+symbol and the
  // flushed Ctrl sticks.) Linux delivers a genuine AltGraph modifier, so this is
  // Windows-only.
  let altGrArmed = false;
  let altGrTimer: ReturnType<typeof setTimeout> | null = null;

  // KBD-6: the {keysym, keycode} actually sent on keydown, keyed by physical
  // `code`. keyup/blur release THESE rather than recomputing eventToKeysym(e):
  //  - if a modifier (Shift/AltGr) is released mid-press, e.key changes, so a
  //    recomputed keysym would differ and leave the original stuck DOWN;
  //  - scancode-only keys (keysym 0 but a valid scancode — NumpadClear,
  //    media, …) have no keysym to release by, so we MUST carry the keycode.
  // Cleared on blur / dispose.
  const downKeys = new Map<string, { keysym: number; keycode: number }>();

  // KBD-7: physical `code`s whose keydown emitted a SELF-CONTAINED chord (the
  // Mac→Windows nav translations and the lock-mode Cmd+Tab remap) — these
  // already sent a full sequence via sendKeySequence and owe NO raw key-up. We
  // record the code here on keydown and swallow the matching key-UP by code,
  // rather than recomputing the predicate on the up (its inputs — Cmd/Option/
  // Shift/e.key — can change between the down and the up). Cleared on blur /
  // dispose alongside downKeys.
  const literalDown = new Set<string>();

  // IME composition state. While composing we hold raw KeyboardEvents:
  // the browser fires keydown/keyup during composition as well as the
  // CompositionEvent stream, and forwarding the raw keys would let
  // the guest see dead-key triggers / IME pre-edit as input.
  // composedText accumulates the most recent compositionupdate so
  // browsers that fire compositionend with empty `data` (Safari,
  // some Chromium configurations) still have something to commit.
  let composing = false;
  let composedText = '';

  // Lock-key sync: macOS (and some Linux DEs) track CapsLock/NumLock
  // internally and may not fire key events reliably. On every
  // keystroke we compare getModifierState with the last state we
  // sent to the guest and synthesize a toggle if they diverge.
  let lastCapsLock: boolean | null = null;
  let lastNumLock: boolean | null = null;
  const syncLockKeys = (e: KeyboardEvent) => {
    const caps = e.getModifierState('CapsLock');
    if (lastCapsLock !== null && caps !== lastCapsLock) {
      const saved = activeKeycode;
      activeKeycode = 0;
      send(XK_CapsLock, true);
      send(XK_CapsLock, false);
      activeKeycode = saved;
    }
    lastCapsLock = caps;

    // macOS: getModifierState('NumLock') is unreliable (Mac keyboards have no
    // NumLock; it reads false), so a divergence check would spuriously toggle
    // the guest's NumLock. Skip NumLock sync on Mac entirely.
    if (!isMac) {
      const num = e.getModifierState('NumLock');
      if (lastNumLock !== null && num !== lastNumLock) {
        const saved = activeKeycode;
        activeKeycode = 0;
        send(XK_NumLock, true);
        send(XK_NumLock, false);
        activeKeycode = saved;
      }
      lastNumLock = num;
    }
  };

  const send = (keysym: number, down: boolean) => {
    const c = opts.client();
    if (!c) return;
    if (c.extendedKeyEnabled()) {
      // Carry a real scancode. For physical keys activeKeycode is already set;
      // for synthetic modifier/lock emissions (activeKeycode 0) fall back to
      // the keysym's AT-set-1 code so QEMU doesn't drop the frame.
      const keycode = activeKeycode !== 0 ? activeKeycode : (SYNTHETIC_KEYCODE[keysym] ?? 0);
      // ext-key: send when EITHER a keysym OR a real scancode is present — a
      // scancode-only frame (keysym 0, keycode≠0) is exactly what NumpadClear /
      // media keys need; only a 0/0 frame is a no-op.
      if (keysym === 0 && keycode === 0) return;
      c.sendExtendedKey(keysym, keycode, down);
    } else {
      // keysym-fallback mode has no scancode field, so a keysym-0 key has
      // nothing to send — preserve the original drop (no regression).
      if (keysym === 0) return;
      c.sendKey(keysym, down);
    }
  };

  // Type a LITERAL character (no physical key behind it) as a keysym via the
  // plain CLI_MSG_KEY_EVENT, so QEMU resolves it through its keymap. Used by IME
  // composition: a composed glyph like 'é' has no reliable guest scancode, and
  // an ext-key frame with keycode 0 would be dropped. Valid even when ext-key is
  // enabled — QEMU accepts both message types. (Glyphs outside the guest's
  // keymap, e.g. an accent on an en-US guest, still can't be produced this way —
  // that is the in-guest agent's job; over VNC it is best-effort.)
  //
  // US shifted-symbol keysym → its BASE-key keysym. The Prism/QEMU endpoint does
  // NOT auto-assert Shift for a shifted-symbol keysym, so a bare '@' (0x40) came
  // out as the base digit '2'. For these we send Shift + the base key, which the
  // endpoint composes correctly into the symbol.
  const US_SHIFTED_BASE: Record<number, number> = {
    0x21: 0x31, // ! → 1
    0x40: 0x32, // @ → 2
    0x23: 0x33, // # → 3
    0x24: 0x34, // $ → 4
    0x25: 0x35, // % → 5
    0x5e: 0x36, // ^ → 6
    0x26: 0x37, // & → 7
    0x2a: 0x38, // * → 8
    0x28: 0x39, // ( → 9
    0x29: 0x30, // ) → 0
    0x5f: 0x2d, // _ → -
    0x2b: 0x3d, // + → =
    0x7b: 0x5b, // { → [
    0x7d: 0x5d, // } → ]
    0x7c: 0x5c, // | → backslash
    0x3a: 0x3b, // : → ;
    0x22: 0x27, // " → '
    0x3c: 0x2c, // < → ,
    0x3e: 0x2e, // > → .
    0x3f: 0x2f, // ? → /
    0x7e: 0x60, // ~ → backquote
  };

  const typeLiteral = (keysym: number) => {
    if (keysym === 0) return;
    const c = opts.client();
    if (!c) return;
    const base = US_SHIFTED_BASE[keysym];
    if (base !== undefined) {
      // Shifted ASCII symbol — send Shift + the base key so the endpoint
      // composes it (a bare shifted keysym is not auto-shifted → '@' → '2').
      c.sendKey(XK_Shift_L, true);
      c.sendKey(base, true);
      c.sendKey(base, false);
      c.sendKey(XK_Shift_L, false);
      return;
    }
    // Codepoints ≥ 0x100 (CJK / accented / symbol glyphs from an IME commit) are
    // NOT valid bare keysyms — X.Org carries them as the direct-Unicode keysym
    // 0x01000000+cp. Route through unicodeKeysym (a no-op for Latin-1 < 0x100,
    // so ASCII symbols are unchanged). Whether the GUEST can render the glyph
    // still depends on its keymap, but the wire form is now correct.
    const ks = unicodeKeysym(keysym);
    c.sendKey(ks, true);
    c.sendKey(ks, false);
  };

  // The en-us BASE position a keysym lands on after QEMU's -k reverse-map:
  // uppercase letters land on their lowercase key, shifted symbols on their
  // base key, everything else on itself. Used to detect when the host's raw
  // keystroke is already wire-equivalent to the guest-layout sequence (so we
  // can send it verbatim and keep real key-down/up semantics).
  const enUsBaseOf = (keysym: number): number => {
    if (keysym >= 0x41 && keysym <= 0x5a) return keysym + 0x20; // A–Z → a–z
    return US_SHIFTED_BASE[keysym] ?? keysym;
  };

  // Type a character AS THE SELECTED GUEST LAYOUT TYPES IT: press the layout's
  // physical position (via that position's en-us base keysym) wrapped in the
  // AltGr/Shift state the layout needs, then restore the host's Shift. Sent as
  // a self-contained down+up via plain key events; the caller swallows the
  // matching key-up (literalDown).
  const typeGuestKey = (pos: GuestKey, hostShift: boolean) => {
    const c = opts.client();
    if (!c) return;
    const wantShift = !!pos.shift;
    if (pos.altgr) c.sendKey(XK_Alt_R, true); // the guest's AltGr
    if (wantShift !== hostShift) c.sendKey(XK_Shift_L, wantShift);
    c.sendKey(pos.base, true);
    c.sendKey(pos.base, false);
    if (wantShift !== hostShift) c.sendKey(XK_Shift_L, hostShift);
    if (pos.altgr) c.sendKey(XK_Alt_R, false);
  };

  // Whether the QEMU Extended Key Event (scancode) path is active. We read it to
  // decide whether a keysym-0 key (NumpadClear / media — no keysym, scancode
  // only) can be sent: in ext-key mode the scancode carries it; in keysym mode
  // there is no scancode field so it is dropped (no regression).
  const extKeyActive = (): boolean => opts.client()?.extendedKeyEnabled() ?? false;

  // Flush a deferred Windows AltGr ControlLeft as a real Ctrl press. Called when
  // the 100ms arm timer fires (the operator is genuinely holding Ctrl) or a
  // key-UP arrives while armed (a brief real Ctrl tap). Tracked in downKeys +
  // the modifier tracker so the matching ControlLeft key-up releases it.
  const interruptAltGr = () => {
    if (!altGrArmed) return;
    altGrArmed = false;
    if (altGrTimer !== null) {
      clearTimeout(altGrTimer);
      altGrTimer = null;
    }
    const kc = eventCodeToKeycode('ControlLeft', host);
    const saved = activeKeycode;
    activeKeycode = kc;
    send(XK_Control_L, true);
    activeKeycode = saved;
    modifiers.press('control');
    downKeys.set('ControlLeft', { keysym: XK_Control_L, keycode: kc });
  };

  // Release chord detection. Bare Ctrl+Alt (no third key) blurs.
  // Ctrl+Alt+anything else (Linux shortcut like Ctrl+Alt+T) passes
  // through. Two AltGr guards: (1) the AltGraph modifier-state flag, set when the
  // browser reports a DE/FR operator's AltGr; (2) an explicit e.code !==
  // 'AltRight', because some browsers report AltGr's right-Alt as key='Alt'
  // WITHOUT the AltGraph flag — AltRight is never a legitimate blur-chord key, so
  // a slow phantom-Ctrl→AltRight that resolved as a real Ctrl must not blur here.
  const isReleaseChord = (e: KeyboardEvent) =>
    e.ctrlKey &&
    e.altKey &&
    !e.getModifierState('AltGraph') &&
    e.code !== 'AltRight' &&
    (e.key === 'Alt' || e.key === 'Control');

  // Mac → Windows NAVIGATION translation. A Mac keyboard has no Home/End/PgUp/
  // PgDn keys; the muscle-memory idioms are Cmd+arrows (line/document) and
  // Option+arrows (by word). On a Windows guest those must become the Windows
  // nav keys, or text editing "feels broken" (the #1 cross-platform gripe).
  // Returns the keysym sequence to send (press-all-then-release-all), or null
  // if e is not a Mac nav idiom. Shift extends the selection (prepend Shift_L).
  // Only fires on the bare Cmd-or-Option + arrow (no other command modifier),
  // so it never shadows a real Ctrl/AltGr combo.
  const macNavChord = (e: KeyboardEvent): number[] | null => {
    if (!isMac) return null;
    const cmd = e.metaKey && !e.ctrlKey && !e.altKey;
    const opt = e.altKey && !e.metaKey && !e.ctrlKey;
    if (!cmd && !opt) return null;
    const shift = e.shiftKey ? [XK_Shift_L] : [];
    if (cmd) {
      switch (e.key) {
        case 'ArrowLeft':
          return [...shift, XK_Home]; // start of line
        case 'ArrowRight':
          return [...shift, XK_End]; // end of line
        case 'ArrowUp':
          return [...shift, XK_Control_L, XK_Home]; // top of document
        case 'ArrowDown':
          return [...shift, XK_Control_L, XK_End]; // end of document
      }
    } else {
      // Option+arrows = move/select by word → Ctrl+arrows on Windows.
      switch (e.key) {
        case 'ArrowLeft':
          return [...shift, XK_Control_L, XK_Left];
        case 'ArrowRight':
          return [...shift, XK_Control_L, XK_Right];
        // Option+Delete = delete by word. Mac's top-right "Delete" key reports
        // e.key 'Backspace' and erases the PREVIOUS word; Fn+Delete (forward,
        // e.key 'Delete') the NEXT. Windows = Ctrl+Backspace / Ctrl+Delete.
        // Shift does not extend a deletion, so it is intentionally ignored.
        case 'Backspace':
          return [XK_Control_L, XK_BackSpace];
        case 'Delete':
          return [XK_Control_L, XK_Delete];
      }
    }
    return null;
  };

  // Stuck-modifier reconciliation. Every KeyboardEvent carries the host's
  // CURRENT modifier state (shiftKey/ctrlKey/altKey/metaKey) — ground truth. If
  // we believe the guest still holds a modifier the host has ALREADY released,
  // drop it now so it can't corrupt every subsequent keystroke. Missed key-UPs
  // are common and were the cause of the "Alt/Shift gets stuck and everything
  // turns to symbols" reports: macOS swallows key-UPs while Cmd is held, a focus
  // change can eat one, and AltGr's phantom modifiers release out-of-band. We
  // ONLY release here (never press) and ONLY what the host is no longer holding,
  // so it can't fight a genuinely-held modifier (both flags true → no-op) or the
  // Ctrl+Alt release chord (handled earlier).
  const reconcileHeldModifiers = (e: KeyboardEvent) => {
    const hostHeld: Record<LogicalModifier, boolean> = {
      shift: e.shiftKey,
      control: e.ctrlKey,
      alt: e.altKey,
      meta: e.metaKey,
    };
    for (const m of ['shift', 'control', 'alt', 'meta'] as LogicalModifier[]) {
      if (modifiers.isDown(m) && !hostHeld[m]) {
        const saved = activeKeycode;
        activeKeycode = 0; // synthetic release — no physical key behind it
        send(RELEASE_KEYSYM[m], false);
        modifiers.release(m);
        activeKeycode = saved;
      }
    }
  };

  return {
    handleKeyDown(e) {
      // Suppress raw keystrokes during IME composition / dead-key
      // composition — the browser commits the final text via
      // compositionend and we type that atomically. KeyboardEvent
      // also fires keyCode=229 ("processed") during composition;
      // catch that defensively regardless of the composing flag.
      if (composing || e.keyCode === 229) {
        e.preventDefault();
        e.stopPropagation();
        return;
      }

      // Auto-repeat: QEMU's emulated keyboard does NOT typematic-repeat on its
      // own, so a held key repeats in the guest ONLY if we forward each browser
      // repeat as another key-down (make) — the hardware-faithful pattern
      // (repeated makes, one break on release). Drop repeats just for MODIFIER
      // keys (a held modifier is one make; repeats are noise); printable /
      // navigation / editing keys fall through to the normal handling below and
      // re-send. `suppressRepeat` (default false) force-drops everything.
      if (e.repeat && (suppressRepeat || modifierFromKey(e.key) !== null || e.key === 'AltGraph')) {
        e.preventDefault();
        e.stopPropagation();
        return;
      }

      // ── Windows AltGr coalescing (must run BEFORE isReleaseChord so an AltGr
      // sequence can't be mistaken for the Ctrl+Alt release chord) ──
      // ARM: a fresh Windows ControlLeft may be the phantom Ctrl that precedes
      // AltRight. Defer it (don't forward yet); the next keydown resolves it.
      if (
        host === 'windows' &&
        e.code === 'ControlLeft' &&
        !altGrArmed &&
        !downKeys.has('ControlLeft')
      ) {
        e.preventDefault();
        e.stopPropagation();
        altGrArmed = true;
        altGrTimer = setTimeout(interruptAltGr, 100);
        return;
      }
      // RESOLVE a deferred ControlLeft.
      if (altGrArmed) {
        altGrArmed = false;
        if (altGrTimer !== null) {
          clearTimeout(altGrTimer);
          altGrTimer = null;
        }
        if (e.code === 'AltRight') {
          // An armed ControlLeft immediately followed by AltRight IS AltGr (the
          // arm timer bounds this to the 100ms window). Emit a single
          // ISO_Level3_Shift, tracked by code so the AltRight key-up releases it;
          // bypass the release-chord/modifier path.
          e.preventDefault();
          e.stopPropagation();
          activeKeycode = eventCodeToKeycode('AltRight', host);
          downKeys.set('AltRight', { keysym: XK_ISO_Level3_Shift, keycode: activeKeycode });
          send(XK_ISO_Level3_Shift, true);
          return;
        }
        // The deferred ControlLeft was a real Ctrl press — flush it (tracked so
        // its key-up releases it).
        const kc = eventCodeToKeycode('ControlLeft', host);
        activeKeycode = kc;
        send(XK_Control_L, true);
        modifiers.press('control');
        downKeys.set('ControlLeft', { keysym: XK_Control_L, keycode: kc });
        // If the resolving key is ITSELF another ControlLeft (two Ctrl taps
        // inside the arm window), the flush already represents its down — don't
        // fall through and send a second Control_L. Otherwise let THIS key
        // proceed normally below (it lands with Ctrl held).
        if (e.code === 'ControlLeft') {
          e.preventDefault();
          e.stopPropagation();
          return;
        }
      }

      if (isReleaseChord(e)) {
        opts.onReleaseChord();
        return;
      }

      e.preventDefault();
      e.stopPropagation();
      syncLockKeys(e);

      // Capture the AT-set-1 keycode for this entire handler's send() calls.
      // The Cmd→Ctrl branch emits multiple wire frames; they all carry the same
      // physical-key keycode. The synthetic Cmd→Ctrl Control_L emission
      // temporarily resets it to 0 to avoid attaching the physical key's code to
      // a virtual modifier.
      activeKeycode = eventCodeToKeycode(e.code, host);

      // Drop any modifier the host has released but the guest still believes is
      // held (missed key-up) BEFORE processing this key — see the helper.
      reconcileHeldModifiers(e);

      // Mac → Windows navigation (Cmd/Option + arrows). Emit the translated
      // Windows nav chord as a self-contained sequence and swallow the matching
      // key-up by code (KBD-7). Runs before the bare-Cmd/Option suppression
      // below so the arrow idioms aren't dropped. Cmd+arrow's e.key.length>1
      // means it never collides with the Cmd+printable→Ctrl branch.
      const nav = macNavChord(e);
      if (nav) {
        activeKeycode = 0;
        if (fakeCtrl) {
          send(XK_Control_L, false);
          fakeCtrl = false;
        }
        literalDown.add(e.code);
        opts.client()?.sendKeySequence(nav);
        return;
      }

      if (isMac) {
        // Suppress bare Cmd — Win-key on a Windows guest is rarely
        // wanted and we have explicit Send-Keys options for it.
        if (e.key === 'Meta' || e.code === 'MetaLeft' || e.code === 'MetaRight') return;
        // Suppress bare Option/Alt. On a Mac, Option is the LAYOUT modifier (it
        // composes symbols the host resolves into e.key), NOT a command Alt.
        // Forwarding it as Alt put a Windows guest into accelerator/menu mode —
        // the File/Edit/View access-key overlay popped on every Option press.
        // The Send-Keys menu provides Alt+Tab/Alt+F4 for guest Alt combos.
        if (e.key === 'Alt' || e.code === 'AltLeft' || e.code === 'AltRight') {
          return;
        }
        // Cmd + printable: emit Control_L + same printable in the
        // guest. This makes Cmd+C / Cmd+V / Cmd+X / Cmd+Z behave the
        // way a Mac operator expects in a PC guest. The synthetic
        // Ctrl is NOT registered with the modifier tracker — it's
        // engine-private bookkeeping. That matters because if we
        // released it via releaseAllModifiers() on blur, we'd skip
        // the fakeCtrl flag and have a phantom Ctrl in the guest's
        // OS state.
        if (e.metaKey && !e.ctrlKey && e.key.length === 1) {
          if (!fakeCtrl) {
            // Synthetic Ctrl has no physical-key keycode.
            const saved = activeKeycode;
            activeKeycode = 0;
            send(XK_Control_L, true);
            activeKeycode = saved;
            fakeCtrl = true;
          }
          // KBD-5: respect Shift. Forcing lowercase made Cmd+Shift+Z send
          // Ctrl+Shift+'z' (lowercase), which the guest reads differently
          // from the intended Shift'd key. Match the actual case of e.key.
          const ch = e.shiftKey ? e.key.toUpperCase() : e.key.toLowerCase();
          const keysym = ch.codePointAt(0) ?? 0;
          if (keysym !== 0) {
            downKeys.set(e.code, { keysym, keycode: activeKeycode });
            send(keysym, true);
          }
          return;
        }
      }

      const mod = modifierFromKey(e.key);
      if (mod) modifiers.press(mod);

      const keysym = eventToKeysym(e);

      // ── Guest-layout translation ──────────────────────────────────────────
      // Type each character the way the DECLARED guest layout types it. The -k
      // en-us QEMU reverse-maps our keysym to an en-us POSITION and masks off
      // the keymap entry's own shift bit without synthesizing anything
      // (verified live on AHV: a bare '@' landed as '2', IT '/'=Shift+7 landed
      // as '?'). So the only wire controls are WHICH position is pressed (via
      // its en-us base keysym) and the Shift/AltGr state WE send around it —
      // exactly what guestKeyFor/typeGuestKey express, per the operator-chosen
      // layout (globe picker; never forced). Wire-equivalent keystrokes pass
      // through verbatim so real key-down/up (and guest auto-repeat) survive.
      // Characters the selected layout can't type fall through verbatim
      // (best-effort; producing those regardless of layout is the Tier-2
      // agent's job). NOT gated on extKeyActive(): the -258 ACK does not mean
      // scancode passthrough on AHV — -k makes the keysym authoritative in both
      // modes; the old !extKeyActive() gate kept this class of fix dead in
      // production. CapsLock exception: letters whose case comes from a HOST
      // CapsLock stay verbatim — syncLockKeys mirrors CapsLock to the guest,
      // and forcing Shift on top of an engaged guest CapsLock inverts the case.
      if (keysym !== 0 && e.key.length === 1) {
        const pos = guestKeyFor(keysym, guestLayout);
        if (pos) {
          const isLetter = (keysym >= 0x41 && keysym <= 0x5a) || (keysym >= 0x61 && keysym <= 0x7a);
          const wireEquivalent =
            !pos.altgr && !!pos.shift === e.shiftKey && enUsBaseOf(keysym) === pos.base;
          if (!wireEquivalent && !(isLetter && e.getModifierState('CapsLock'))) {
            activeKeycode = 0;
            literalDown.add(e.code); // self-contained — swallow the key-up by code
            typeGuestKey(pos, e.shiftKey);
            return;
          }
        }
      }

      // activeKeycode was set to eventCodeToKeycode(e.code, host) at the top.
      // Send when we have a keysym OR (ext-key mode) a real scancode even with
      // keysym 0 — the latter is the NumpadClear / media-key case that was
      // silently dropped. Cache {keysym, keycode} so keyup/blur release by the
      // real scancode (a keysym-0 key has no keysym to release by).
      if (keysym !== 0 || (extKeyActive() && activeKeycode !== 0)) {
        downKeys.set(e.code, { keysym, keycode: activeKeycode });
        send(keysym, true);
      }
    },

    handleKeyUp(e) {
      if (composing || e.keyCode === 229) {
        e.preventDefault();
        e.stopPropagation();
        return;
      }

      e.preventDefault();
      e.stopPropagation();
      syncLockKeys(e);

      activeKeycode = eventCodeToKeycode(e.code, host);

      // A pending Windows AltGr arm + an incoming key-UP ⇒ the deferred
      // ControlLeft was a real Ctrl tap (not an AltGr lead-in): flush it now so
      // the release below pairs with it (e.g. a ControlLeft tap → Ctrl down+up).
      if (altGrArmed) interruptAltGr();

      // KBD-7: keys whose keydown emitted a self-contained chord (the Mac→Windows
      // nav translations, the lock-mode Cmd+Tab remap) recorded their physical
      // `code` in literalDown and owe NO raw key-up. Swallow by code — NOT by
      // recomputing the predicate, whose inputs (Cmd/Option/Shift/e.key) can
      // change between the down and the up.
      if (literalDown.delete(e.code)) return;

      if (isMac) {
        if (e.key === 'Meta' || e.code === 'MetaLeft' || e.code === 'MetaRight') {
          if (fakeCtrl) {
            const saved = activeKeycode;
            activeKeycode = 0;
            send(XK_Control_L, false);
            activeKeycode = saved;
            fakeCtrl = false;
          }
          // KBD-7 (the "sssss" stuck key): macOS/WebKit/Chromium deliver NO
          // key-UP for a NON-modifier key while Cmd is held, so every printable
          // pressed during this Cmd hold is still in downKeys with no release
          // coming — the guest keeps it DOWN and autorepeats. Release those
          // orphaned keys now. Skip modifier keysyms (Shift etc.): macOS DOES
          // deliver their key-up, so they release via their own event; force-
          // releasing a still-held Shift here would corrupt the guest's state.
          for (const [code, { keysym, keycode }] of downKeys) {
            if (MODIFIER_KEYSYMS.has(keysym)) continue;
            activeKeycode = keycode;
            send(keysym, false);
            downKeys.delete(code);
          }
          activeKeycode = 0;
          return;
        }
        // Option/Alt was suppressed on keydown, so swallow its release too
        // (no stray Alt-up the guest never saw).
        if (e.key === 'Alt' || e.code === 'AltLeft' || e.code === 'AltRight') {
          return;
        }
        if (e.metaKey && e.key.length === 1) {
          // KBD-6: release the keysym we actually sent on keydown, not a
          // recomputed one (Shift may have changed e.key's case meanwhile).
          const cached = downKeys.get(e.code);
          const keysym = cached?.keysym ?? e.key.toLowerCase().codePointAt(0) ?? 0;
          if (cached) activeKeycode = cached.keycode;
          downKeys.delete(e.code);
          if (keysym !== 0) send(keysym, false);
          return;
        }
      }

      const mod = modifierFromKey(e.key);
      if (mod) modifiers.release(mod);

      // KBD-6 + scancode-only: release by the cached {keysym, keycode} from this
      // key's keydown (prefer the cached keycode so a scancode-only key releases
      // by the exact code it went down with, and a mid-press modifier release
      // can't leave the original stuck DOWN). Mirror the keydown gate.
      const cached = downKeys.get(e.code);
      const keysym = cached?.keysym ?? eventToKeysym(e);
      if (cached) activeKeycode = cached.keycode;
      downKeys.delete(e.code);
      if (keysym !== 0 || (extKeyActive() && activeKeycode !== 0)) send(keysym, false);
    },

    handleCompositionStart() {
      composing = true;
      composedText = '';
      // Drop the canvas's belief about any modifier the operator
      // was holding when composition began — the dead-key trigger
      // (Opt on Mac, AltGr on Win/Linux) was registered as DOWN
      // before the compositionstart fired. Releasing it on the
      // guest side prevents the composed character from arriving
      // with stray Alt/AltGr modifiers. Synthetic Ctrl is left
      // alone — Cmd+anything during IME is unusual but possible.
      activeKeycode = 0;
      modifiers.releaseAll((ks, down) => send(ks, down));
    },

    handleCompositionUpdate(text) {
      // Browsers vary in whether compositionend.data is populated;
      // we stash the latest update so we can fall back to it in
      // case compositionend's data is empty (older Safari, certain
      // Chromium IME bridges).
      composedText = text;
    },

    handleCompositionEnd(text) {
      composing = false;
      const final = text || composedText;
      composedText = '';
      if (!final) return;
      // Type each codepoint as a literal keysym (plain key event → QEMU
      // keymap), since composed text has no physical scancode. Newline / Tab are
      // sent as their X11 keysyms DIRECTLY — they are already keysyms, not
      // codepoints, so they must NOT go through typeLiteral (which routes
      // codepoints ≥ 0x100 through the direct-Unicode offset and would mangle
      // XK_Return 0xff0d into 0x0100ff0d). Printables go through typeLiteral.
      const c = opts.client();
      for (const ch of final) {
        const cp = ch.codePointAt(0);
        if (cp === undefined) continue;
        if (cp === 0x0a || cp === 0x0d) {
          c?.sendKey(0xff0d, true); // XK_Return
          c?.sendKey(0xff0d, false);
        } else if (cp === 0x09) {
          c?.sendKey(0xff09, true); // XK_Tab
          c?.sendKey(0xff09, false);
        } else if (cp >= 0x20) {
          typeLiteral(cp);
        }
        // other control chars (< 0x20) are dropped — no meaningful keysym
      }
    },

    releaseAllModifiers() {
      // Discard any pending Windows AltGr arm — the deferred Ctrl was never
      // sent, so there is nothing to flush here (a flushed Ctrl is already in
      // downKeys and gets released by the drain below). Flushing it now would
      // press a Ctrl into a guest that's about to lose focus.
      altGrArmed = false;
      if (altGrTimer !== null) {
        clearTimeout(altGrTimer);
        altGrTimer = null;
      }
      // Blur-driven release has no physical key — emit with keycode=0
      // so the guest treats the up as keysym-only (correct, since
      // the host already lost focus and there's no real release
      // event to attribute).
      activeKeycode = 0;
      const released = modifiers.releaseAll((ks, down) => send(ks, down));
      // Synthetic Ctrl is engine-private — release it here too so
      // the blur watchdog covers the full set, not just the tracker.
      if (fakeCtrl) {
        send(XK_Control_L, false);
        fakeCtrl = false;
      }
      // KBD-6: release any keys still believed DOWN — a blur mid-keypress
      // (Alt+Tab away) would otherwise leave a stuck key in the guest. Carry
      // each key's real scancode so a scancode-only key (keysym 0) releases by
      // its scancode rather than 0 and doesn't stick.
      for (const { keysym, keycode } of downKeys.values()) {
        activeKeycode = keycode;
        send(keysym, false);
      }
      activeKeycode = 0;
      downKeys.clear();
      literalDown.clear();
      // Forget the host lock-key cache so the next keydown re-syncs CapsLock /
      // NumLock from ground truth. Matters on a reconnect re-baseline (rfb_reset):
      // the fresh guest's lock LEDs are unknown, and a stale cached value would
      // suppress the corrective toggle and leave the locks diverged.
      lastCapsLock = null;
      lastNumLock = null;
      return released;
    },

    setGuestLayout(id) {
      guestLayout = asGuestLayoutId(id);
    },

    sendPointer(x, y, buttonMask) {
      opts.client()?.sendPointer(x, y, buttonMask);
    },

    sendWheel(x, y, buttonMask, deltaY) {
      const c = opts.client();
      if (!c) return;
      const dir = deltaY > 0 ? BTN_WHEEL_DOWN : BTN_WHEEL_UP;
      c.sendPointer(x, y, buttonMask | dir);
      // Release the wheel click on the next tick so the guest sees
      // a discrete click rather than a sticky button.
      setTimeout(() => c.sendPointer(x, y, buttonMask), 0);
    },

    snapshot() {
      return {
        modifiers: modifiers.snapshot(),
        fakeCtrlActive: fakeCtrl,
        host,
      };
    },

    dispose() {
      altGrArmed = false;
      if (altGrTimer !== null) {
        clearTimeout(altGrTimer);
        altGrTimer = null;
      }
      activeKeycode = 0;
      modifiers.releaseAll((ks, down) => send(ks, down));
      if (fakeCtrl) {
        send(XK_Control_L, false);
        fakeCtrl = false;
      }
      for (const { keysym, keycode } of downKeys.values()) {
        activeKeycode = keycode;
        send(keysym, false);
      }
      activeKeycode = 0;
      downKeys.clear();
      literalDown.clear();
    },
  };
}
