import { useEffect } from 'react';

export interface KeyboardShortcutOptions {
  /** "Mod" maps to Cmd on macOS, Ctrl elsewhere. */
  combo: string;
  handler: (e: KeyboardEvent) => void;
  /** Disable the listener without unmounting. */
  enabled?: boolean;
  /** Prevent default browser behavior on match. */
  preventDefault?: boolean;
}

export function useKeyboardShortcut({
  combo,
  handler,
  enabled = true,
  preventDefault = true,
}: KeyboardShortcutOptions): void {
  useEffect(() => {
    if (!enabled) return;
    const parts = combo
      .toLowerCase()
      .split('+')
      .map((p) => p.trim());
    const wantsMod = parts.includes('mod');
    const wantsShift = parts.includes('shift');
    const wantsAlt = parts.includes('alt') || parts.includes('option');
    const key = parts.find(
      (p) => !['mod', 'shift', 'alt', 'option', 'ctrl', 'cmd', 'meta'].includes(p),
    );
    if (!key) return;

    const isMac = typeof navigator !== 'undefined' && /mac/i.test(navigator.platform);

    const onKey = (e: KeyboardEvent) => {
      const modPressed = isMac ? e.metaKey : e.ctrlKey;
      if (wantsMod !== modPressed) return;
      if (wantsShift !== e.shiftKey) return;
      if (wantsAlt !== e.altKey) return;
      if (e.key.toLowerCase() !== key) return;
      if (preventDefault) e.preventDefault();
      handler(e);
    };

    window.addEventListener('keydown', onKey);
    return () => window.removeEventListener('keydown', onKey);
  }, [combo, handler, enabled, preventDefault]);
}
