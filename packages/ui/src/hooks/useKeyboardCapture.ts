import { useEffect } from 'react';

export interface KeyboardCaptureOptions {
  /** Only forward when this is true. Used to gate forwarding to the active console tab. */
  enabled: boolean;
  /** Called for every keydown that should be forwarded to the VM. */
  onKeyDown: (e: KeyboardEvent) => void;
  /** Called for every keyup. */
  onKeyUp: (e: KeyboardEvent) => void;
  /**
   * Modifier chord that releases capture (Ctrl+Alt by default). When pressed,
   * the hook will *not* forward the chord itself, but will not preventDefault
   * so the parent useMouseCapture can react.
   */
  releaseChord?: ('Control' | 'Alt' | 'Shift' | 'Meta')[];
}

/**
 * Pass-through keyboard hook used by the console view. When enabled, every
 * keystroke that arrives at the window is:
 *
 *   - preventDefault'd so the browser/Electron does not consume it.
 *   - stopPropagation'd so the global command-palette hotkey does not fire.
 *   - forwarded to the caller via onKeyDown/onKeyUp.
 *
 * This is what lets the operator type Cmd+Q (or Alt+F4, etc.) inside the
 * guest VM without quitting AHV Remote Console.
 *
 * Important caveat — OS-intercepted combos (Ctrl+Alt+Del on Windows,
 * Super+L on most desktops) cannot be captured by the browser. Those must be
 * sent via the explicit "Send Keys" menu, which synthesizes the matching RFB
 * events directly.
 */
export function useKeyboardCapture({
  enabled,
  onKeyDown,
  onKeyUp,
  releaseChord,
}: KeyboardCaptureOptions): void {
  useEffect(() => {
    if (!enabled) return;

    const chord = new Set<string>(releaseChord ?? ['Control', 'Alt']);
    const heldMods = new Set<string>();

    const isModifier = (k: string) =>
      k === 'Control' || k === 'Alt' || k === 'Shift' || k === 'Meta';

    const matchesChord = (): boolean => {
      if (heldMods.size < chord.size) return false;
      for (const m of chord) {
        if (!heldMods.has(m)) return false;
      }
      return true;
    };

    const keydown = (e: KeyboardEvent) => {
      if (isModifier(e.key)) heldMods.add(e.key);
      // Let the release-chord pass through cleanly.
      if (matchesChord()) return;
      e.preventDefault();
      e.stopPropagation();
      onKeyDown(e);
    };
    const keyup = (e: KeyboardEvent) => {
      if (isModifier(e.key)) heldMods.delete(e.key);
      e.preventDefault();
      e.stopPropagation();
      onKeyUp(e);
    };
    const blur = () => heldMods.clear();

    window.addEventListener('keydown', keydown, { capture: true });
    window.addEventListener('keyup', keyup, { capture: true });
    window.addEventListener('blur', blur);
    return () => {
      window.removeEventListener('keydown', keydown, { capture: true });
      window.removeEventListener('keyup', keyup, { capture: true });
      window.removeEventListener('blur', blur);
    };
  }, [enabled, onKeyDown, onKeyUp, releaseChord]);
}
