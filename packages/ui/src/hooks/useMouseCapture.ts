import { useCallback, useEffect, useState, type RefObject } from 'react';

export type ModifierKey = 'Control' | 'Alt' | 'Shift' | 'Meta';

export interface MouseCaptureOptions {
  /**
   * Ref to the element that should own pointer lock (e.g. the console canvas).
   */
  target: RefObject<HTMLElement>;
  /**
   * Modifier-chord that releases mouse capture, e.g. ['Control', 'Alt'] for
   * the classic VMware Workstation behavior. The chord must be held — every
   * key in the array must be down simultaneously.
   */
  releaseChord?: ModifierKey[];
  /** Called whenever capture state changes. */
  onChange?: (captured: boolean) => void;
  /** Disable click-to-capture (useful when the tab is not active). */
  disabled?: boolean;
}

export interface MouseCaptureAPI {
  /** True if the element currently has pointer lock. */
  captured: boolean;
  /** Manually request capture (e.g. from a button). */
  capture: () => void;
  /** Manually release capture. */
  release: () => void;
}

/**
 * Hook that wraps the Pointer Lock API with VMRC-style ergonomics:
 *
 *   - clicking the target element captures the pointer.
 *   - pressing a configurable modifier chord (default Ctrl+Alt) releases it.
 *   - browser-initiated lock loss (e.g. Esc on Windows) also triggers onChange.
 *
 * The mouse exit chord intentionally uses *only* modifier keys so it never
 * collides with anything the guest VM cares about — RFB key events for the
 * modifiers themselves are still forwarded; only the *combination* triggers
 * the local exit.
 */
export function useMouseCapture(opts: MouseCaptureOptions): MouseCaptureAPI {
  const { target, onChange, disabled } = opts;
  const releaseChord = opts.releaseChord ?? ['Control', 'Alt'];
  const [captured, setCaptured] = useState(false);

  const capture = useCallback(() => {
    const el = target.current;
    if (!el || disabled) return;
    // Pointer lock is best-effort: in Electron renderers loaded via custom
    // protocols Chromium rejects requestPointerLock() with "The root
    // document of this element is not valid for pointer lock." We don't
    // need it — keyboard capture (gated on `captured`) and absolute mouse
    // coordinates both work without it. Try anyway so users on browsers
    // where it does work get the OS-level cursor hide.
    try {
      void el.requestPointerLock?.();
    } catch {
      /* ignored — pointer lock is optional */
    }
    setCaptured(true);
    onChange?.(true);
  }, [target, disabled, onChange]);

  const release = useCallback(() => {
    if (document.pointerLockElement) {
      try {
        document.exitPointerLock();
      } catch {
        /* ignored */
      }
    }
    setCaptured(false);
    onChange?.(false);
  }, [onChange]);

  // No pointerlockchange listener: when requestPointerLock() rejects (as
  // it does in this Electron renderer), Chromium fires pointerlockchange
  // with document.pointerLockElement === null. Listening to that event
  // would immediately undo the captured=true we just set synchronously
  // in capture(). Capture is driven entirely by explicit click /
  // release-chord here — pointer lock is decorative.

  useEffect(() => {
    if (disabled) return;
    const el = target.current;
    if (!el) return;
    const click = (e: MouseEvent) => {
      if (captured) return;
      e.preventDefault();
      capture();
    };
    el.addEventListener('mousedown', click);
    return () => el.removeEventListener('mousedown', click);
  }, [capture, target, disabled, captured]);

  useEffect(() => {
    if (!captured) return;
    const heldMods = new Set<string>();
    const isModifier = (k: string): k is ModifierKey =>
      k === 'Control' || k === 'Alt' || k === 'Shift' || k === 'Meta';
    const matchChord = () => {
      for (const k of releaseChord) {
        if (!heldMods.has(k)) return false;
      }
      return true;
    };
    const onKeyDown = (e: KeyboardEvent) => {
      if (isModifier(e.key)) heldMods.add(e.key);
      if (matchChord()) release();
    };
    const onKeyUp = (e: KeyboardEvent) => {
      if (isModifier(e.key)) heldMods.delete(e.key);
    };
    const onBlur = () => heldMods.clear();
    window.addEventListener('keydown', onKeyDown, { capture: true });
    window.addEventListener('keyup', onKeyUp, { capture: true });
    window.addEventListener('blur', onBlur);
    return () => {
      window.removeEventListener('keydown', onKeyDown, { capture: true });
      window.removeEventListener('keyup', onKeyUp, { capture: true });
      window.removeEventListener('blur', onBlur);
    };
  }, [captured, releaseChord, release]);

  return { captured, capture, release };
}
