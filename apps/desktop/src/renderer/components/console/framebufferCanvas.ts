/**
 * Framebuffer paint/composite helpers, extracted verbatim from ConsoleView
 * (FE-01 useFramebufferCanvas). Pure functions over canvas elements — no React
 * state, no closures — so they are safe to move out of the component and unit
 * test directly.
 *
 * ───────────────────────── HiDPI-crisp rendering ──────────────────────────
 *
 * The framebuffer is decoded at the guest's NATIVE resolution (e.g.
 * 1920×1080). We render in two stages so the result is pixel-crisp on any
 * monitor / DPI, and stays crisp after the window is dragged to a
 * different-scale display:
 *
 *   stage 1 (paintFull / paintRects):  RFB pixels → an OFFSCREEN canvas
 *       held at the framebuffer's native resolution. This canvas is the
 *       single source of truth for guest pixels; incremental rects and full
 *       repaints both land here. It is NEVER attached to the DOM, so its
 *       size is independent of CSS / DPI.
 *
 *   stage 2 (composite):  offscreen → the VISIBLE canvas whose BACKING STORE
 *       is sized to the display area in *physical device pixels*
 *       (cssW*dpr × cssH*dpr). Because the backing store then equals the
 *       on-screen pixel grid 1:1, the compositor performs NO resample of its
 *       own — every scale decision is ours, made with the right filter:
 *         • upscaling (fb smaller than the display area): nearest-neighbour
 *           (imageSmoothingEnabled = false) so pixels stay sharp instead of
 *           going soft, exactly like the native Prism console.
 *         • downscaling (fb larger than the display area): high-quality
 *           smoothing so text shrinks cleanly without aliasing.
 *
 * The previous implementation painted RFB pixels straight into a backing
 * store fixed at the framebuffer size and relied on `image-rendering:
 * pixelated` + the compositor to scale to the physical grid. On a HiDPI /
 * Retina monitor (dpr ≥ 2) that meant a 1× backing store upscaled by the
 * compositor → blur; and nothing re-sized on a per-monitor DPI change, so
 * the console blurred the moment it moved to a different-scale display.
 */

import type { CursorShape, Framebuffer, Rect, RFBClient } from '../../rfb/client';
import { BTN_LEFT, BTN_MIDDLE, BTN_RIGHT } from '../../rfb/protocol';

export function buttonForBrowser(b: number): number {
  switch (b) {
    case 0:
      return BTN_LEFT;
    case 1:
      return BTN_MIDDLE;
    case 2:
      return BTN_RIGHT;
    default:
      return 0;
  }
}

/**
 * Convert a parsed cursor shape (RGBA + hotspot) into a CSS cursor
 * value. The OS renders this at native display speed — zero JS
 * latency, zero rAF delay. Called once per ENC_CURSOR FBU (rare:
 * only when the guest switches cursor type).
 */
const _cursorCanvas = document.createElement('canvas');
export function cursorShapeToCss(shape: CursorShape | null): string | null {
  if (!shape || shape.width === 0 || shape.height === 0) return null;
  _cursorCanvas.width = shape.width;
  _cursorCanvas.height = shape.height;
  const ctx = _cursorCanvas.getContext('2d');
  if (!ctx) return null;
  const img = new ImageData(new Uint8ClampedArray(shape.rgba), shape.width, shape.height);
  ctx.putImageData(img, 0, 0);
  const url = _cursorCanvas.toDataURL('image/png');
  return `url(${url}) ${shape.hotspotX} ${shape.hotspotY}, auto`;
}

/**
 * Shared ImageData view over the framebuffer pixels. Reused across paint calls
 * to avoid per-frame allocation. Invalidated when the framebuffer dimensions
 * change (reconnect / resize).
 */
let _sharedImageData: ImageData | null = null;
let _sharedFbRef: Uint8ClampedArray | null = null;

function getSharedImageData(fb: Framebuffer): ImageData {
  if (
    _sharedImageData &&
    _sharedFbRef === fb.pixels &&
    _sharedImageData.width === fb.width &&
    _sharedImageData.height === fb.height
  ) {
    return _sharedImageData;
  }
  // The framebuffer's backing store is always a plain ArrayBuffer (never a
  // SharedArrayBuffer), but the Uint8ClampedArray<ArrayBufferLike> type can't
  // prove that to TS 6's narrowed ImageData overload — assert the buffer kind.
  _sharedImageData = new ImageData(
    fb.pixels as Uint8ClampedArray<ArrayBuffer>,
    fb.width,
    fb.height,
  );
  _sharedFbRef = fb.pixels;
  return _sharedImageData;
}

/**
 * Cached 2D rendering context. Avoids repeated getContext() calls in the hot
 * paint path. The context is configured with:
 *   - alpha: false    → compositor skips per-pixel alpha blending (opaque fb)
 *   - desynchronized: true → putImageData writes bypass the vsync-gated
 *     compositor pipeline, reducing display latency by up to 1 frame (~16ms).
 *     This is the single biggest latency win for VNC-style paint workloads.
 */
let _cachedCtx: CanvasRenderingContext2D | null = null;
let _cachedCanvas: HTMLCanvasElement | null = null;

function getCtx(canvas: HTMLCanvasElement): CanvasRenderingContext2D | null {
  if (_cachedCanvas === canvas && _cachedCtx) return _cachedCtx;
  _cachedCtx = canvas.getContext('2d', { alpha: false, desynchronized: true });
  _cachedCanvas = canvas;
  return _cachedCtx;
}

/** Paint the entire framebuffer into the canvas at native resolution. */
export function paintFull(canvas: HTMLCanvasElement | null, fb: Framebuffer) {
  if (!canvas) return;
  if (canvas.width !== fb.width || canvas.height !== fb.height) {
    // Resizing the canvas clears it AND invalidates the cached context.
    canvas.width = fb.width;
    canvas.height = fb.height;
    _cachedCtx = null;
    _cachedCanvas = null;
  }
  const ctx = getCtx(canvas);
  if (!ctx) return;
  ctx.putImageData(getSharedImageData(fb), 0, 0);
}

// paintRects blits dirty rects from the live framebuffer to the canvas.
// Uses a BOUNDING BOX over all dirty rects to issue a SINGLE putImageData
// call per paint frame — reducing hundreds of JS→native transitions to one.
// The shared ImageData wraps the entire framebuffer (zero-copy), so the bbox
// just controls how much the native side writes. For video content where
// rects cluster together, this is dramatically faster than per-rect calls.
//
// EXCEPT when the rects are few and scattered (cursor corner + clock corner):
// there the bbox union approaches the full frame (~8.3 MB upload at 1080p for
// a few hundred dirty pixels), so when the union would overpaint > 4× the
// actual dirty area and there are ≤ 16 rects, issue per-rect putImageData
// calls instead. Overlapping rects double-count area, biasing toward the
// union — conservative, fine.
export function paintRects(
  canvas: HTMLCanvasElement | null,
  client: RFBClient | null,
  rects: Rect[],
) {
  if (!canvas || !client) return;
  const fb = client.framebuffer();
  if (!fb) return;
  if (canvas.width !== fb.width || canvas.height !== fb.height) {
    canvas.width = fb.width;
    canvas.height = fb.height;
    _cachedCtx = null;
    _cachedCanvas = null;
  }
  const ctx = getCtx(canvas);
  if (!ctx) return;

  // Compute bounding box + total area of all valid dirty rects.
  let minX = fb.width,
    minY = fb.height,
    maxX = 0,
    maxY = 0;
  let totalRectArea = 0;
  let anyValid = false;
  for (const r of rects) {
    if (r.w <= 0 || r.h <= 0) continue;
    if (r.x < 0 || r.y < 0 || r.x + r.w > fb.width || r.y + r.h > fb.height) continue;
    if (r.x < minX) minX = r.x;
    if (r.y < minY) minY = r.y;
    if (r.x + r.w > maxX) maxX = r.x + r.w;
    if (r.y + r.h > maxY) maxY = r.y + r.h;
    totalRectArea += r.w * r.h;
    anyValid = true;
  }
  if (!anyValid) return;

  const img = getSharedImageData(fb);
  const bboxArea = (maxX - minX) * (maxY - minY);
  if (bboxArea > 4 * totalRectArea && rects.length <= 16) {
    // Scattered small rects: the union would overpaint badly — blit each
    // rect on its own (same shared ImageData, per-rect bounds re-checked).
    for (const r of rects) {
      if (r.w <= 0 || r.h <= 0) continue;
      if (r.x < 0 || r.y < 0 || r.x + r.w > fb.width || r.y + r.h > fb.height) continue;
      ctx.putImageData(img, 0, 0, r.x, r.y, r.w, r.h);
    }
    return;
  }

  // Single putImageData with the bounding box as dirty rect.
  // This paints the minimal region that covers all changed rects.
  ctx.putImageData(img, 0, 0, minX, minY, maxX - minX, maxY - minY);
}

/**
 * Paint a decoded ImageBitmap (from Tight JPEG) directly onto the visible
 * canvas, bypassing the fb.pixels → putImageData roundtrip. This eliminates
 * 3 intermediate buffer copies that previously made the JPEG path the
 * slowest part of the pipeline during video playback.
 */
export function paintBitmap(
  canvas: HTMLCanvasElement | null,
  bmp: ImageBitmap,
  x: number,
  y: number,
): void {
  if (!canvas) return;
  const ctx = getCtx(canvas);
  if (!ctx) return;
  ctx.drawImage(bmp, x, y);
}
