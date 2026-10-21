/**
 * Log-forging defense for renderer-controlled log lines (RED-04 / ELEC-06).
 *
 * The `hc-log` IPC writes a `[tag] message` line, both renderer-supplied, to
 * the shared electron-log file. Without sanitization a compromised/buggy
 * renderer can inject CR/LF to forge additional log records (splitting one
 * entry into several, faking timestamps/levels) or smuggle terminal control
 * sequences (C0/C1 escapes) that corrupt a `tail -f`/`cat` of the log.
 *
 * This module is intentionally pure (no Electron / no electron-log imports)
 * so it can be unit-tested in isolation.
 */

/** Upper bound on a single sanitized log field. */
export const MAX_LOG_FIELD = 2048;

/**
 * Replace every control character with a single space:
 *   - C0 controls  U+0000..U+001F (includes CR, LF, TAB)
 *   - DEL          U+007F
 *   - C1 controls  U+0080..U+009F (some terminals treat these as escapes)
 *
 * Built from codepoint ranges (not a regex literal) so the SOURCE stays
 * ASCII-clean — no literal control bytes embedded in this file.
 */
function stripControl(s: string): string {
  let out = '';
  for (let i = 0; i < s.length; i++) {
    const c = s.charCodeAt(i);
    const isControl = c <= 0x1f || c === 0x7f || (c >= 0x80 && c <= 0x9f);
    out += isControl ? ' ' : s[i];
  }
  return out;
}

/**
 * Make a renderer-supplied string safe to write into a line-oriented log file:
 *
 *   1. Replace control chars (CR/LF/TAB/DEL/C0/C1) with a space — this is what
 *      closes the log-forging CR/LF injection.
 *   2. Cap the result at `max` chars so an oversized renderer line cannot
 *      bloat the log or push earlier records out of a tail view.
 *
 * Returns a single-line string with no control characters.
 */
export function sanitizeLogField(s: string, max = MAX_LOG_FIELD): string {
  const stripped = stripControl(s);
  return stripped.length > max ? stripped.slice(0, max) : stripped;
}
