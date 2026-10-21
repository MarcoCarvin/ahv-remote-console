import { describe, expect, it, beforeEach, vi } from 'vitest';

/**
 * ELEC-04 / SEC-02 / ELEC-03: the trusted-window registry + sender guard.
 *
 * We mock `electron` so the registry's BrowserWindow/session interactions run
 * in plain Node. The mock models just enough: BrowserWindow.fromWebContents
 * resolves a fake window off the sender, session.defaultSession is a sentinel,
 * and fake windows expose isDestroyed()/webContents.session/once('closed').
 */

// vi.mock is hoisted above module init, so the sentinel default session must
// be created inside a vi.hoisted block to exist when the factory runs.
const { defaultSession } = vi.hoisted(() => ({ defaultSession: { id: 'default' } }));

vi.mock('electron', () => ({
  session: { defaultSession },
  BrowserWindow: {
    // The fake sender carries a back-pointer to its owning window.
    fromWebContents: (wc: { win?: FakeWindow } | null) => wc?.win ?? null,
  },
}));

interface FakeWindow {
  destroyed: boolean;
  closedCb?: () => void;
  isDestroyed: () => boolean;
  once: (ev: string, cb: () => void) => void;
  webContents: { session: unknown };
}

function makeWindow(opts: { session?: unknown } = {}): FakeWindow {
  const w: FakeWindow = {
    destroyed: false,
    isDestroyed: () => w.destroyed,
    once: (ev, cb) => {
      if (ev === 'closed') w.closedCb = cb;
    },
    webContents: { session: opts.session ?? defaultSession },
  };
  return w;
}

import {
  registerTrustedWindow,
  unregisterTrustedWindow,
  isTrustedWindow,
  assertTrustedSender,
  trustedBroadcastTargets,
} from '../trustedSenders';
import type { BrowserWindow, IpcMainInvokeEvent } from 'electron';

// Helper: invoke assertTrustedSender with a fake event whose sender points at a
// (possibly null) window.
function assertFrom(win: FakeWindow | null) {
  const event = { sender: win ? { win } : null } as unknown as IpcMainInvokeEvent;
  assertTrustedSender(event);
}

describe('trustedSenders registry', () => {
  // The registry is module-global; drain it between tests by unregistering.
  let created: FakeWindow[] = [];
  beforeEach(() => {
    for (const w of created) unregisterTrustedWindow(w as unknown as BrowserWindow);
    created = [];
  });
  const track = (w: FakeWindow) => {
    created.push(w);
    return w;
  };

  it('isTrustedWindow is false for an unregistered window', () => {
    const w = track(makeWindow());
    expect(isTrustedWindow(w as unknown as BrowserWindow)).toBe(false);
  });

  it('isTrustedWindow is true after register, false after unregister', () => {
    const w = track(makeWindow());
    registerTrustedWindow(w as unknown as BrowserWindow);
    expect(isTrustedWindow(w as unknown as BrowserWindow)).toBe(true);
    unregisterTrustedWindow(w as unknown as BrowserWindow);
    expect(isTrustedWindow(w as unknown as BrowserWindow)).toBe(false);
  });

  it('auto-prunes on the window closed event', () => {
    const w = track(makeWindow());
    registerTrustedWindow(w as unknown as BrowserWindow);
    expect(isTrustedWindow(w as unknown as BrowserWindow)).toBe(true);
    w.closedCb?.();
    expect(isTrustedWindow(w as unknown as BrowserWindow)).toBe(false);
  });

  it('isTrustedWindow is false for a destroyed (but still registered) window', () => {
    const w = track(makeWindow());
    registerTrustedWindow(w as unknown as BrowserWindow);
    w.destroyed = true;
    expect(isTrustedWindow(w as unknown as BrowserWindow)).toBe(false);
  });

  it('isTrustedWindow is false for null', () => {
    expect(isTrustedWindow(null)).toBe(false);
  });
});

describe('assertTrustedSender', () => {
  let created: FakeWindow[] = [];
  beforeEach(() => {
    for (const w of created) unregisterTrustedWindow(w as unknown as BrowserWindow);
    created = [];
  });
  const track = (w: FakeWindow) => {
    created.push(w);
    return w;
  };

  it('passes for a registered trusted window', () => {
    const w = track(makeWindow());
    registerTrustedWindow(w as unknown as BrowserWindow);
    expect(() => assertFrom(w)).not.toThrow();
  });

  it('throws for an unregistered window (e.g. the SSO login window)', () => {
    const sso = track(makeWindow({ session: { id: 'persist:sso-ep1' } }));
    expect(() => assertFrom(sso)).toThrow(/not a trusted window/);
  });

  it('throws when fromWebContents cannot resolve a window', () => {
    expect(() => assertFrom(null)).toThrow(/not a trusted window/);
  });
});

describe('trustedBroadcastTargets', () => {
  let created: FakeWindow[] = [];
  beforeEach(() => {
    for (const w of created) unregisterTrustedWindow(w as unknown as BrowserWindow);
    created = [];
  });
  const track = (w: FakeWindow) => {
    created.push(w);
    return w;
  };

  it('returns registered default-session windows', () => {
    const a = track(makeWindow());
    const b = track(makeWindow());
    registerTrustedWindow(a as unknown as BrowserWindow);
    registerTrustedWindow(b as unknown as BrowserWindow);
    const targets = trustedBroadcastTargets();
    expect(targets).toContain(a as unknown as BrowserWindow);
    expect(targets).toContain(b as unknown as BrowserWindow);
  });

  it('excludes a window on a non-default (SSO) session even if registered', () => {
    const app = track(makeWindow());
    const sso = track(makeWindow({ session: { id: 'persist:sso-ep1' } }));
    registerTrustedWindow(app as unknown as BrowserWindow);
    registerTrustedWindow(sso as unknown as BrowserWindow);
    const targets = trustedBroadcastTargets();
    expect(targets).toContain(app as unknown as BrowserWindow);
    expect(targets).not.toContain(sso as unknown as BrowserWindow);
  });

  it('prunes destroyed windows', () => {
    const a = track(makeWindow());
    registerTrustedWindow(a as unknown as BrowserWindow);
    a.destroyed = true;
    expect(trustedBroadcastTargets()).not.toContain(a as unknown as BrowserWindow);
  });
});
