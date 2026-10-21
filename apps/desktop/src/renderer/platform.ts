/**
 * Platform helpers for the renderer. The desktop shell is multi-platform
 * (macOS / Windows / Linux); window chrome differs per OS, so layout that sits
 * under the title bar must adapt rather than assume macOS.
 *
 * See apps/desktop/src/main/main.ts: the main + console windows use
 * `titleBarStyle: 'hiddenInset'` on macOS (traffic-light controls overlay the
 * content's top-left) and a native frame (`titleBarStyle: 'default'`) on
 * Windows/Linux (the OS draws its own title bar above the webview — no
 * overlay).
 */

export const IS_MAC =
  typeof window !== 'undefined' && window.ahvRemoteConsole?.platform === 'darwin';

/**
 * Left padding for an in-window header strip. On macOS we reserve 78px so the
 * content clears the overlaid traffic lights; on Windows/Linux there is no
 * overlay, so the header keeps its normal `base` padding.
 */
export function titleBarLeft(base: number): number {
  return IS_MAC ? 78 : base;
}
