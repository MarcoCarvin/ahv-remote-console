/**
 * Pure geometry helpers for RFB FramebufferUpdate rects.
 *
 * Extracted verbatim from RFBClient.processServerMessage so the
 * inbound-rect bounds guard can be unit-tested in isolation without
 * standing up a full RFB handshake. No I/O, no `this` — purely a
 * function of the rect header and the framebuffer dimensions.
 */
import type { Rect } from './encodings';

/**
 * True if a real pixel-drawing rect (encoding >= 0) would overrun the
 * framebuffer. The pseudo-encodings (DesktopSize / ExtendedDesktopSize /
 * Cursor) are all negative and carry their own geometry, so they are
 * exempt — the bounds check only applies to encoding >= 0.
 *
 * This is the predicate the decode-amplification desync guard trips on:
 * a header advertising e.g. 65535×65535 from a handful of wire bytes
 * means a PRIOR rect left the stream misaligned. fb dims are themselves
 * clamped to 7680×4320 in applyResize, so a passing rect bounds its
 * per-rect decode work to the screen.
 */
export function rectExceedsFramebuffer(rect: Rect, fbWidth: number, fbHeight: number): boolean {
  return rect.encoding >= 0 && (rect.x + rect.w > fbWidth || rect.y + rect.h > fbHeight);
}
