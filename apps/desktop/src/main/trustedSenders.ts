/**
 * Trusted-window registry + privileged-IPC sender guard (ELEC-04, SEC-02/ELEC-03).
 *
 * The main process owns two kinds of BrowserWindow:
 *   - TRUSTED app windows: the main window and console pop-outs. These load
 *     our bundled renderer and are the only windows allowed to call privileged
 *     IPC handlers (sign-request, clipboard, open-console-window, …) and the
 *     only windows we fan host-side broadcasts (clipboard-changed, runtime-state,
 *     update-*) to.
 *   - The embedded Prism SSO login window (sso-login.ts), which renders
 *     UNTRUSTED third-party content on a `persist:sso-*` partition. It must
 *     never receive our broadcasts nor be able to invoke privileged IPC.
 *
 * Previously broadcasts used `BrowserWindow.getAllWindows()` (which includes
 * the SSO window) and privileged handlers trusted any `event.sender`. This
 * module centralises a tracked set of trusted windows so both gaps close in
 * one place.
 */

import { BrowserWindow, session } from 'electron';
import type { IpcMainInvokeEvent } from 'electron';

/**
 * The set of trusted windows. Populated by the window factories in main.ts
 * (registerTrustedWindow on create, auto-pruned on 'closed'). Module-scoped so
 * the IPC handlers and broadcast fan-out share one source of truth.
 */
const trustedWindows = new Set<BrowserWindow>();

/**
 * Register a window as trusted. Idempotent. Auto-unregisters on 'closed' so the
 * set never holds a destroyed window. Call this in the main + console-window
 * factories immediately after construction.
 */
export function registerTrustedWindow(win: BrowserWindow): void {
  trustedWindows.add(win);
  win.once('closed', () => {
    trustedWindows.delete(win);
  });
}

/** Remove a window from the trusted set (defensive; 'closed' also handles this). */
export function unregisterTrustedWindow(win: BrowserWindow): void {
  trustedWindows.delete(win);
}

/**
 * The live trusted windows, with any destroyed entries pruned. Used as the
 * broadcast target for clipboard-changed / runtime-state / update-* so the
 * untrusted SSO window is structurally excluded (it is never registered, and
 * its `persist:sso-*` partition is asserted as a belt-and-braces check).
 */
export function trustedBroadcastTargets(): BrowserWindow[] {
  const live: BrowserWindow[] = [];
  for (const win of trustedWindows) {
    if (win.isDestroyed()) {
      trustedWindows.delete(win);
      continue;
    }
    // Belt-and-braces: never fan a broadcast to an SSO-partition window even
    // if one were ever (wrongly) registered. Trusted app windows (main +
    // console pop-outs) all run on the DEFAULT session; the embedded Prism
    // login window runs on an isolated `persist:sso-<endpointId>` partition
    // (see sso-login.ts), so any window whose session is not the default
    // session is excluded here.
    try {
      if (win.webContents.session !== session.defaultSession) continue;
    } catch {
      continue; // destroyed mid-iteration
    }
    live.push(win);
  }
  return live;
}

/**
 * The single FOCUSED trusted window, or null if focus is outside the app (a host
 * app like KeePass) or on the untrusted SSO window. Used to SCOPE host→guest
 * clipboard forwarding to the console the operator is actually looking at, rather
 * than fanning a host copy into every open guest at once (a cross-guest secret
 * spray). Applies the same default-session / not-destroyed guards as
 * trustedBroadcastTargets so the SSO-partition window is never a target.
 */
export function focusedBroadcastTarget(): BrowserWindow | null {
  const win = BrowserWindow.getFocusedWindow();
  if (!isTrustedWindow(win)) return null;
  try {
    if (win!.isDestroyed()) return null;
    if (win!.webContents.session !== session.defaultSession) return null;
  } catch {
    return null;
  }
  return win;
}

/**
 * Pure membership check, extracted so it can be unit-tested without Electron.
 * Returns true iff `win` is a non-destroyed member of `set`.
 */
export function isTrustedWindow(
  win: BrowserWindow | null,
  set: ReadonlySet<BrowserWindow> = trustedWindows,
): boolean {
  if (!win) return false;
  if (!set.has(win)) return false;
  try {
    return !win.isDestroyed();
  } catch {
    return false;
  }
}

/**
 * Assert that a privileged IPC event came from a trusted window. Resolve the
 * BrowserWindow that owns `event.sender` and require it to be in the trusted
 * set. Throws otherwise — call this FIRST in every privileged ipcMain.handle
 * handler so a compromised/untrusted webContents (e.g. the SSO window, or a
 * subframe) cannot reach the runtime-signing or clipboard surface.
 */
export function assertTrustedSender(event: IpcMainInvokeEvent): void {
  const win = BrowserWindow.fromWebContents(event.sender);
  if (!isTrustedWindow(win)) {
    throw new Error('ipc: sender is not a trusted window');
  }
}
