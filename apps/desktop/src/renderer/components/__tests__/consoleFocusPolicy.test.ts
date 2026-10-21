/**
 * Tests for the pop-out focus auto-capture policy (createConsoleFocusPolicy,
 * exported from ConsoleView). Field feedback: the canvas was never
 * auto-focused, so typing went nowhere until the operator clicked it.
 *
 * The policy is the pure decision core — ConsoleView's effects only translate
 * its booleans into canvasRef.current.focus() calls — so these node-env tests
 * cover the behavior the same way consoleStatus.test.ts covers the status
 * helpers (FE-01 idiom):
 *   - pop-out connect focuses the canvas (incl. the connected-control-frame-
 *     before-first-framebuffer case);
 *   - a main-window (non-pop-out) view never auto-focuses;
 *   - window focus (alt-tab back) re-focuses while connected;
 *   - window focus does NOT steal focus while the error overlay is showing;
 *   - a deliberate Ctrl+Alt release is not auto-undone by window focus.
 */

import { describe, expect, it } from 'vitest';

// ConsoleView's import graph touches DOM globals at module scope (the ui
// barrel's toast portal, framebufferCanvas's scratch canvas). Provide minimal
// stand-ins so the module loads under the node test environment — same
// pattern as framebufferCanvas.test.ts.
class StubImageData {
  constructor(
    public data: Uint8ClampedArray,
    public width: number,
    public height: number,
  ) {}
}
(globalThis as Record<string, unknown>).ImageData ??= StubImageData;
(globalThis as Record<string, unknown>).document ??= {
  createElement: () => ({ getContext: () => null, style: {}, setAttribute: () => undefined }),
  getElementById: () => null,
  head: { appendChild: () => undefined },
  body: { appendChild: () => undefined },
  addEventListener: () => undefined,
  removeEventListener: () => undefined,
};
(globalThis as Record<string, unknown>).window ??= {
  addEventListener: () => undefined,
  removeEventListener: () => undefined,
};
(globalThis as Record<string, unknown>).navigator ??= {};

const { createConsoleFocusPolicy } = await import('../consoleFocusPolicy');

// A connected console with the canvas rendered and the OS window focused —
// the common steady state most scenarios start from.
const ready = { hasOsFocus: true, canvasAvailable: true };

function connectedPopout() {
  const p = createConsoleFocusPolicy(true);
  p.statusChanged('connected', ready);
  return p;
}

describe('pop-out connect focus', () => {
  it('focuses the canvas when status transitions to connected', () => {
    const p = createConsoleFocusPolicy(true);
    expect(p.statusChanged('connected', ready)).toBe(true);
  });

  it('defers focus to canvasReady when the connected frame precedes the first framebuffer', () => {
    const p = createConsoleFocusPolicy(true);
    // The proxy's 'connected' control frame arrives before ServerInit, so the
    // canvas (gated on fbSize) is not rendered yet.
    expect(p.statusChanged('connected', { hasOsFocus: true, canvasAvailable: false })).toBe(false);
    expect(p.canvasReady()).toBe(true);
    // One-shot: the request is served, not re-applied on later canvas churn.
    expect(p.canvasReady()).toBe(false);
  });

  it('does not re-request focus while staying connected (heartbeat churn)', () => {
    const p = connectedPopout();
    expect(p.statusChanged('connected', ready)).toBe(false);
    expect(p.canvasReady()).toBe(false);
  });

  it('re-focuses when the error overlay is dismissed by a successful action', () => {
    const p = connectedPopout();
    p.statusChanged('error', ready);
    // Reconnect / Re-authenticate succeeded: status leaves 'error' back to
    // 'connecting' while the window has OS focus — focus must leave the
    // overlay button and return to the console.
    expect(p.statusChanged('connecting', ready)).toBe(true);
  });

  it('does not grab focus on error → connecting when the window lacks OS focus', () => {
    const p = connectedPopout();
    p.statusChanged('error', ready);
    expect(p.statusChanged('connecting', { hasOsFocus: false, canvasAvailable: true })).toBe(false);
    // The eventual reconnect still focuses.
    expect(p.statusChanged('connected', ready)).toBe(true);
  });

  it('drops a pending connect-focus if the session faults before the canvas appears', () => {
    const p = createConsoleFocusPolicy(true);
    p.statusChanged('connected', { hasOsFocus: true, canvasAvailable: false });
    p.statusChanged('error', { hasOsFocus: true, canvasAvailable: false });
    // The error overlay's buttons must stay keyboard-reachable — a stale
    // connect-focus must not fire underneath it.
    expect(p.canvasReady()).toBe(false);
  });
});

describe('main-window (non-pop-out) view', () => {
  it('never auto-focuses on connect, canvas-ready, or window focus', () => {
    const p = createConsoleFocusPolicy(false);
    expect(p.statusChanged('connected', ready)).toBe(false);
    expect(p.canvasReady()).toBe(false);
    expect(p.windowFocused()).toBe(false);
    p.statusChanged('error', ready);
    expect(p.statusChanged('connecting', ready)).toBe(false);
  });
});

describe('window focus (alt-tab back)', () => {
  it('re-focuses the canvas while connected', () => {
    const p = connectedPopout();
    expect(p.windowFocused()).toBe(true);
  });

  it('does not steal focus while the error overlay is showing', () => {
    const p = connectedPopout();
    p.statusChanged('error', ready);
    // The overlay's Reconnect / Re-authenticate buttons must stay reachable
    // by keyboard (and ConsoleWindowApp's one-shot session_expired focus
    // listener owns the error case).
    expect(p.windowFocused()).toBe(false);
  });

  it('stays inert during connecting / reconnecting / poweredOff / starting', () => {
    for (const kind of ['connecting', 'reconnecting', 'poweredOff', 'starting'] as const) {
      const p = connectedPopout();
      p.statusChanged(kind, ready);
      expect(p.windowFocused()).toBe(false);
    }
  });
});

describe('deliberate Ctrl+Alt release', () => {
  it('window focus does not auto-undo the release', () => {
    const p = connectedPopout();
    p.releasedByChord();
    expect(p.windowFocused()).toBe(false);
  });

  it('clicking the canvas re-arms auto-focus', () => {
    const p = connectedPopout();
    p.releasedByChord();
    p.pointerDown();
    expect(p.windowFocused()).toBe(true);
  });

  it('a stream reconnect re-arms auto-focus and focuses', () => {
    const p = connectedPopout();
    p.releasedByChord();
    p.statusChanged('reconnecting', ready);
    expect(p.windowFocused()).toBe(false);
    expect(p.statusChanged('connected', ready)).toBe(true);
    expect(p.windowFocused()).toBe(true);
  });
});
