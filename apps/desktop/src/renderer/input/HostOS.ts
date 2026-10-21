/**
 * Authoritative host-OS detection.
 *
 * `navigator.platform` is deprecated and on some configurations
 * returns an empty or misleading value (e.g. a stale "MacIntel" on
 * ARM Macs running in Rosetta-emulated browsers). Single source of
 * truth is the contextBridge-exposed `window.ahvRemoteConsole.platform`,
 * which is set from `process.platform` in the Electron main process.
 *
 * In a future browser-only build there's no contextBridge; we then
 * fall back to `navigator.userAgentData.platform` (modern Chromium
 * exposes this) and only last to `navigator.platform`. The fallback
 * order matters: `navigator.platform` lies the most often, so it's
 * last.
 */

export type HostOS = 'macos' | 'windows' | 'linux' | 'unknown';

export function detectHostOS(): HostOS {
  // Preferred: Electron main says so.
  try {
    const p = window.ahvRemoteConsole?.platform;
    if (p === 'darwin') return 'macos';
    if (p === 'win32') return 'windows';
    if (p === 'linux') return 'linux';
  } catch {
    /* contextBridge not available */
  }

  // Browser modern API.
  const uaData = (
    navigator as Navigator & {
      userAgentData?: { platform?: string };
    }
  ).userAgentData;
  if (uaData?.platform) {
    const platform = uaData.platform.toLowerCase();
    if (platform.includes('mac')) return 'macos';
    if (platform.includes('win')) return 'windows';
    if (platform.includes('linux')) return 'linux';
  }

  // Last resort.
  const legacy = (navigator.platform || '').toLowerCase();
  if (legacy.includes('mac')) return 'macos';
  if (legacy.startsWith('win')) return 'windows';
  if (legacy.includes('linux')) return 'linux';

  return 'unknown';
}

/**
 * Singleton snapshot. Host OS doesn't change at runtime so caching
 * is safe; the lookup itself is cheap but `detectHostOS` is called
 * from hot paths so still worth caching.
 */
let cached: HostOS | null = null;
export function hostOS(): HostOS {
  cached ??= detectHostOS();
  return cached;
}

/**
 * The host PHYSICAL keyboard layout we assume when resolving printable
 * symbol keys against the guest. The guest QEMU VNC keymap is fixed at
 * `en-us`, so a non-US host layout cross-maps: e.g. the physical US-`\|`
 * key (`KeyboardEvent.code === 'Backslash'`) is labelled `ù` on an Italian
 * keyboard, and the browser reports `e.key === 'ù'`. Forwarding that glyph
 * verbatim makes the en-us guest emit `ù` (or nothing) instead of `\`.
 *
 * We DEFAULT to Italian ('it') because that was the original deployment's
 * operator population. The engine uses this layout to recover the
 * ASCII symbol the operator actually intends from the physical `code`
 * (see `recoverLayoutKeysym` in keysyms.ts), so `\` reliably reaches the
 * en-us guest as a backslash regardless of the Italian relabelling.
 */
export type HostKeyboardLayout = 'it' | 'us';

/**
 * Best-effort host-layout signal. The Italian symbol-recovery table is applied
 * ONLY to an explicitly Italian (`it…`) locale; every other locale — English,
 * any other non-English layout, or undetectable — uses the en-us passthrough
 * (the operator's host-resolved keysym is sent as-is).
 *
 * Why this narrowing: the recovery (recoverLayoutKeysym) only un-mangles the
 * NON-ASCII glyphs an *Italian* keyboard puts on relabelled symbol keys
 * (à è é ì ò ù ç ° §). German/French/Spanish/… layouts emit DIFFERENT non-ASCII
 * glyphs from those same physical positions (e.g. German 'ä' on `Quote`), and
 * the earlier "anything non-English → Italian" default rewrote those to the
 * wrong en-us symbol. Restricting recovery to the Italian locale stops that
 * collateral damage; non-IT operators get the host-resolved keysym untouched.
 *
 * Signal: `navigator.language` / `navigator.languages`. `en…` → US passthrough;
 * `it…` → Italian recovery; everything else (and undetectable) → US passthrough.
 *
 * Note: intentionally coarse. The browser exposes no reliable synchronous
 * physical-layout API (navigator.keyboard.getLayoutMap is async + Chromium-only);
 * resolving the real per-host layout via that API is the proper fix and would
 * replace this locale heuristic entirely.
 */
export function detectHostKeyboardLayout(): HostKeyboardLayout {
  try {
    // navigator.language may be an empty string on some configs; fall back to
    // the first entry of navigator.languages, then to '' (→ us passthrough).
    const lang = navigator.language?.length ? navigator.language : navigator.languages?.[0];
    const primary = (lang ?? '').toLowerCase();
    // Explicitly English locale → operator is presumably on a US/UK keyboard
    // whose symbol keys already emit ASCII; pass them through unchanged.
    if (primary.startsWith('en')) return 'us';
    // Only an explicitly Italian locale gets the Italian symbol-recovery table.
    // Previously ANY non-English locale fell through to Italian, which MANGLED
    // German/French/Spanish/… Mac symbol keys: those physical positions emit
    // different non-ASCII glyphs (e.g. German 'ä' on the Quote key) and the
    // Italian table rewrote them to the wrong en-us symbol. Such operators are
    // far better served by the en-us passthrough (send the host-resolved keysym
    // as-is) until real per-host layout resolution (navigator.keyboard
    // .getLayoutMap) replaces this locale heuristic.
    if (primary.startsWith('it')) return 'it';
    return 'us';
  } catch {
    /* navigator unavailable (non-browser test env) */
  }
  // Undetectable (no navigator) → en-us passthrough, the safe default.
  return 'us';
}

let cachedLayout: HostKeyboardLayout | null = null;
export function hostKeyboardLayout(): HostKeyboardLayout {
  cachedLayout ??= detectHostKeyboardLayout();
  return cachedLayout;
}
