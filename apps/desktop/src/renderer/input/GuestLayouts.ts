/**
 * Guest keyboard-layout tables — character → physical key sequence.
 *
 * The OPERATOR declares which layout the GUEST VM is set to (the globe picker in
 * ConsoleView, persisted per VM). The InputEngine then translates every typed
 * character into the key sequence that produces it ON THAT GUEST LAYOUT,
 * expressed on the wire as en-us keysyms + Shift/AltGr wrappers.
 *
 * Why this works (the wire model, verified live on AHV):
 *   keysym → QEMU `-k en-us` reverse-map → en-us-position scancode → GUEST
 *   layout interprets the position.
 * QEMU masks off the keymap entry's own shift bit and synthesizes nothing, so
 * the only things we control are (a) WHICH physical position is pressed — via
 * the en-us keysym of that position — and (b) the modifier state riding it —
 * via XK_Shift_L / XK_Alt_R we send ourselves. A character is therefore
 * expressed as: the en-us BASE keysym of the guest-layout position that bears
 * it, plus that position's level (none / shift / altgr / shift+altgr).
 *
 * Example, Italian guest: '@' lives on AltGr+ò, and ò sits on the en-us ';'
 * position → emit Alt_R down, ';' keysym (→ 0x27 scancode), Alt_R up. '£' is
 * Shift+3 → '3' keysym with Shift held. 'è' is the bare en-us '[' position.
 *
 * NO host-layout guessing happens here — `KeyboardEvent.key` already tells us
 * WHAT the operator typed; these tables only answer HOW the chosen guest
 * layout types it. Characters absent from the selected layout's table fall
 * back to the verbatim keysym (best-effort; on most guests they drop — that is
 * the Tier-2 agent's job, see docs/keyboard-multilang-tier2.md).
 *
 * Tables describe the standard WINDOWS layout variants (the guests are
 * predominantly Windows). 'us'/'uk'/'it'/'de' are complete for their printable
 * set; 'fr'/'es' cover the well-established positions and are flagged
 * best-effort pending live validation. Dead keys (FR/ES/DE accents composed via
 * dead keys) are intentionally absent — a dead-key round trip cannot be driven
 * blindly from one keysym.
 */

export type GuestLayoutId = 'us' | 'uk' | 'it' | 'de' | 'fr' | 'es';

export interface GuestKey {
  /** en-us keysym of the physical position (its UNSHIFTED en-us glyph). */
  base: number;
  /** The guest layout needs Shift held for this character. */
  shift?: boolean;
  /** The guest layout needs AltGr (right Alt) held for this character. */
  altgr?: boolean;
}

/**
 * Row format: [positionBase, unshifted, shifted, altgr?, shiftAltgr?]
 * `positionBase` is the en-us UNSHIFTED character of the physical key (which
 * doubles as the keysym we send for it). Each level entry is the character the
 * GUEST layout produces there, or '' when none/dead.
 */
type Row = readonly [string, string, string, string?, string?];

const cp = (ch: string): number => ch.codePointAt(0)!;

function buildLayout(
  rows: readonly Row[],
  letterSwaps?: Record<string, string>,
): Map<number, GuestKey> {
  const map = new Map<number, GuestKey>();
  // Letters first: identity positions unless swapped (DE y/z, FR azerty).
  for (let i = 0; i < 26; i++) {
    const lower = String.fromCharCode(0x61 + i);
    const pos = letterSwaps?.[lower] ?? lower;
    map.set(cp(lower), { base: cp(pos) });
    map.set(cp(lower.toUpperCase()), { base: cp(pos), shift: true });
  }
  // Symbol/digit rows override or extend.
  for (const [pos, plain, shifted, altgr, shiftAltgr] of rows) {
    const base = cp(pos);
    if (plain) map.set(cp(plain), { base });
    if (shifted) map.set(cp(shifted), { base, shift: true });
    if (altgr) map.set(cp(altgr), { base, altgr: true });
    if (shiftAltgr) map.set(cp(shiftAltgr), { base, shift: true, altgr: true });
  }
  return map;
}

// The en-us keysym for the ISO 102nd key (IntlBackslash). The QEMU en-us keymap
// carries `less 0x56` for it, so the 'less' keysym reaches that position when
// Shift isn't held (QEMU disambiguates the two 'less' entries by modifier
// state). The '>' shifted case is ambiguous server-side — best-effort.
const INTL = '<';

const US_ROWS: readonly Row[] = [
  ['`', '`', '~'],
  ['1', '1', '!'],
  ['2', '2', '@'],
  ['3', '3', '#'],
  ['4', '4', '$'],
  ['5', '5', '%'],
  ['6', '6', '^'],
  ['7', '7', '&'],
  ['8', '8', '*'],
  ['9', '9', '('],
  ['0', '0', ')'],
  ['-', '-', '_'],
  ['=', '=', '+'],
  ['[', '[', '{'],
  [']', ']', '}'],
  [';', ';', ':'],
  ["'", "'", '"'],
  ['\\', '\\', '|'],
  [',', ',', '<'],
  ['.', '.', '>'],
  ['/', '/', '?'],
];

const UK_ROWS: readonly Row[] = [
  ['`', '`', '¬', '¦'],
  ['1', '1', '!'],
  ['2', '2', '"'],
  ['3', '3', '£'],
  ['4', '4', '$', '€'],
  ['5', '5', '%'],
  ['6', '6', '^'],
  ['7', '7', '&'],
  ['8', '8', '*'],
  ['9', '9', '('],
  ['0', '0', ')'],
  ['-', '-', '_'],
  ['=', '=', '+'],
  ['[', '[', '{'],
  [']', ']', '}'],
  [';', ';', ':'],
  ["'", "'", '@'],
  ['\\', '#', '~'], // UK: the en-us backslash position bears # ~
  [INTL, '\\', '|'], // UK: backslash/pipe live on the 102nd key
  [',', ',', '<'],
  ['.', '.', '>'],
  ['/', '/', '?'],
];

const IT_ROWS: readonly Row[] = [
  ['`', '\\', '|'], // IT: top-left key IS backslash/pipe
  ['1', '1', '!'],
  ['2', '2', '"'],
  ['3', '3', '£'],
  ['4', '4', '$'],
  ['5', '5', '%'],
  ['6', '6', '&'],
  ['7', '7', '/'],
  ['8', '8', '('],
  ['9', '9', ')'],
  ['0', '0', '='],
  ['-', "'", '?'],
  ['=', 'ì', '^'],
  ['e', '', '', '€'], // AltGr+E = €
  ['[', 'è', 'é', '[', '{'],
  [']', '+', '*', ']', '}'],
  [';', 'ò', 'ç', '@'],
  ["'", 'à', '°', '#'],
  ['\\', 'ù', '§'],
  [INTL, '<', '>'],
  [',', ',', ';'],
  ['.', '.', ':'],
  ['/', '-', '_'],
];

const DE_ROWS: readonly Row[] = [
  ['`', '^', '°'],
  ['1', '1', '!'],
  ['2', '2', '"', '²'],
  ['3', '3', '§', '³'],
  ['4', '4', '$'],
  ['5', '5', '%'],
  ['6', '6', '&'],
  ['7', '7', '/', '{'],
  ['8', '8', '(', '['],
  ['9', '9', ')', ']'],
  ['0', '0', '=', '}'],
  ['-', 'ß', '?', '\\'],
  // '=' position is the dead acute/grave key — intentionally absent.
  ['q', '', '', '@'],
  ['e', '', '', '€'],
  ['[', 'ü', 'Ü'],
  [']', '+', '*', '~'],
  [';', 'ö', 'Ö'],
  ["'", 'ä', 'Ä'],
  ['\\', '#', "'"],
  [INTL, '<', '>', '|'],
  [',', ',', ';'],
  ['.', '.', ':'],
  ['/', '-', '_'],
  ['m', '', '', 'µ'],
];

// BEST-EFFORT pending live validation (dead keys omitted).
const FR_ROWS: readonly Row[] = [
  ['`', '²', ''],
  ['1', '&', '1'],
  ['2', 'é', '2'],
  ['3', '"', '3', '#'],
  ['4', "'", '4', '{'],
  ['5', '(', '5', '['],
  ['6', '-', '6', '|'],
  ['7', 'è', '7'],
  ['8', '_', '8', '\\'],
  ['9', 'ç', '9'],
  ['0', 'à', '0', '@'],
  ['-', ')', '°', ']'],
  ['=', '=', '+', '}'],
  ['e', '', '', '€'],
  [']', '$', '£', '¤'],
  [';', 'm', 'M'],
  ["'", 'ù', '%'],
  ['\\', '*', 'µ'],
  [INTL, '<', '>'],
  ['m', ',', '?'],
  [',', ';', '.'],
  ['.', ':', '/'],
  ['/', '!', '§'],
];

// BEST-EFFORT pending live validation (dead keys omitted).
const ES_ROWS: readonly Row[] = [
  ['`', 'º', 'ª', '\\'],
  ['1', '1', '!', '|'],
  ['2', '2', '"', '@'],
  ['3', '3', '·', '#'],
  ['4', '4', '$', '~'],
  ['5', '5', '%'],
  ['6', '6', '&', '¬'],
  ['7', '7', '/'],
  ['8', '8', '('],
  ['9', '9', ')'],
  ['0', '0', '='],
  ['-', "'", '?'],
  ['=', '¡', '¿'],
  ['e', '', '', '€'],
  [']', '+', '*', ']'],
  [';', 'ñ', 'Ñ'],
  ['\\', 'ç', 'Ç', '}'],
  ['[', '', '', '['],
  [INTL, '<', '>'],
  [',', ',', ';'],
  ['.', '.', ':'],
  ['/', '-', '_'],
];

const LAYOUTS: Record<GuestLayoutId, Map<number, GuestKey>> = {
  us: buildLayout(US_ROWS),
  uk: buildLayout(UK_ROWS),
  it: buildLayout(IT_ROWS),
  de: buildLayout(DE_ROWS, { y: 'z', z: 'y' }),
  fr: buildLayout(FR_ROWS, { a: 'q', q: 'a', z: 'w', w: 'z', m: ';' }),
  es: buildLayout(ES_ROWS),
};

/** Coerce an arbitrary picker id to a known layout (unknown → en-us). */
export function asGuestLayoutId(id: string): GuestLayoutId {
  return id in LAYOUTS ? (id as GuestLayoutId) : 'us';
}

/**
 * How the given GUEST layout types this character, or null when the layout
 * can't produce it directly (caller falls back to the verbatim keysym).
 */
export function guestKeyFor(codepoint: number, layout: GuestLayoutId): GuestKey | null {
  return LAYOUTS[layout].get(codepoint) ?? null;
}
