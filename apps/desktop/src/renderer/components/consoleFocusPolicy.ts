/**
 * Focus auto-capture policy for POP-OUT consoles (field feedback: the canvas
 * was never auto-focused, so typing went nowhere until the operator clicked
 * it). Pure decision core, in its own module so ConsoleView.tsx stays a
 * component-only file (Vite Fast Refresh requires that — a non-component export
 * there forces a full page reload on every edit). Same idiom as the status
 * helpers in ./console/status (FE-01). ConsoleView's effects only translate its
 * booleans into canvasRef.current.focus() calls.
 *
 * Decisions it owns:
 *   - a status transition INTO 'connected' focuses the canvas; a fresh
 *     (re)connect also re-arms auto-focus after a deliberate release;
 *   - when the error overlay is dismissed by a successful action (status
 *     leaves 'error' back to 'connecting') while the window has OS focus,
 *     focus returns to the console instead of staying on the overlay button;
 *   - a window 'focus' event (alt-tab back) re-focuses ONLY while
 *     'connected' — never while an overlay needs its buttons reachable by
 *     keyboard;
 *   - a deliberate Ctrl+Alt release is NEVER auto-undone: the policy stays
 *     inert until the operator clicks the canvas or the stream reconnects.
 *
 * A non-pop-out view never auto-focuses — an embedded console must not steal
 * the main window's focus.
 */

import type { ConsoleStatus } from './console/status';

export interface ConsoleFocusPolicy {
  /** Feed every status.kind change. Returns true when the canvas should be
   *  focused now; if it is not rendered yet (the proxy's 'connected' control
   *  frame precedes the first framebuffer), the request stays pending for
   *  canvasReady(). */
  statusChanged(
    kind: ConsoleStatus['kind'],
    ctx: { hasOsFocus: boolean; canvasAvailable: boolean },
  ): boolean;
  /** The canvas element just rendered (first framebuffer). Returns true when
   *  a pending connect-focus should be applied to it. One-shot. */
  canvasReady(): boolean;
  /** The OS window regained focus (alt-tab back). Returns true to re-focus. */
  windowFocused(): boolean;
  /** Ctrl+Alt release chord — a deliberate release. */
  releasedByChord(): void;
  /** Operator clicked the canvas — re-arms auto-focus. */
  pointerDown(): void;
}

export function createConsoleFocusPolicy(popout: boolean): ConsoleFocusPolicy {
  let kindNow: ConsoleStatus['kind'] = 'connecting';
  let released = false;
  let pending = false;
  return {
    statusChanged(kind, ctx) {
      const prev = kindNow;
      kindNow = kind;
      if (!popout || kind === prev) return false;
      if (kind === 'connected') {
        // A fresh stream re-arms auto-focus after a Ctrl+Alt release.
        released = false;
        pending = true;
      } else if (kind === 'error') {
        // The error overlay's buttons must stay keyboard-reachable — drop any
        // connect-focus still pending from before the fault.
        pending = false;
      } else if (prev === 'error' && kind === 'connecting' && ctx.hasOsFocus) {
        // Overlay dismissed by a successful action (Reconnect /
        // Re-authenticate) — put focus back on the console, not the button.
        pending = true;
      }
      if (pending && ctx.canvasAvailable) {
        pending = false;
        return true;
      }
      return false;
    },
    canvasReady() {
      if (!pending) return false;
      pending = false;
      return true;
    },
    windowFocused() {
      // 'connected' implies no overlay is showing, and a deliberate Ctrl+Alt
      // release stays released across alt-tab.
      return popout && kindNow === 'connected' && !released;
    },
    releasedByChord() {
      released = true;
    },
    pointerDown() {
      released = false;
    },
  };
}
