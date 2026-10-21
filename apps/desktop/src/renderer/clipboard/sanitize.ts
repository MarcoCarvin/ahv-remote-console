/**
 * sanitizeGuestText — strips dangerous invisible characters from guest
 * clipboard text before writing to the host macOS pasteboard.
 *
 * Stripped:
 *   C0 controls except \t \n \r \f, DEL, C1 controls (incl. NEL U+0085),
 *   bidi overrides/embeddings, zero-width chars, BOM, and the Unicode
 *   line/paragraph separators U+2028 / U+2029.
 *
 * Preserved: tabs, newlines, CR, form feed, all printable Unicode.
 *
 * Note: U+0085 (NEL), U+2028 (LINE SEPARATOR) and U+2029 (PARAGRAPH
 * SEPARATOR) are newline-EQUIVALENT control characters. A hostile guest
 * could otherwise smuggle them into the host clipboard to inject extra
 * command lines on paste into a host terminal (many shells/editors treat
 * them as line breaks), bypassing the trailing-newline strip that forces a
 * deliberate human Enter. They are stripped here, and the guest→host
 * trailing-break strip in ClipboardEngine also removes them when trailing.
 */

function buildDangerousPattern(): RegExp {
  const ranges: ([number, number] | [number])[] = [
    [0x0000, 0x0008], // C0: NUL through BS
    [0x000b], // C0: VT
    [0x000e, 0x001f], // C0: SO through US (includes ESC)
    [0x007f], // DEL
    [0x0080, 0x009f], // C1 controls (CSI, OSC, etc.) — includes NEL U+0085
    [0x200b, 0x200f], // zero-width space, ZWNJ, ZWJ, LRM, RLM
    [0x2028, 0x2029], // LINE SEPARATOR, PARAGRAPH SEPARATOR (newline-equivalent)
    [0x202a, 0x202e], // bidi: LRE, RLE, PDF, LRO, RLO
    [0x2066, 0x2069], // bidi isolates: LRI, RLI, FSI, PDI
    [0xfeff], // BOM / ZWNBS
  ];
  const cls = ranges
    .map(([lo, hi]) =>
      hi !== undefined
        ? String.fromCharCode(lo) + '-' + String.fromCharCode(hi)
        : String.fromCharCode(lo),
    )
    .join('');
  return new RegExp('[' + cls + ']', 'g');
}

const DANGEROUS = buildDangerousPattern();

export function sanitizeGuestText(text: string): { cleaned: string; stripped: number } {
  let stripped = 0;
  DANGEROUS.lastIndex = 0;
  const cleaned = text.replace(DANGEROUS, () => {
    stripped++;
    return '';
  });
  return { cleaned, stripped };
}
