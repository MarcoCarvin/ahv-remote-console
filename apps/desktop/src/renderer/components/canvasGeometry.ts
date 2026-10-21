/**
 * Crisp-rendering geometry for the VM console canvas.
 *
 * The framebuffer is decoded at the guest's NATIVE resolution. To render it
 * pixel-crisp on any monitor / DPI we size the VISIBLE canvas BACKING STORE
 * to the display area in *physical device pixels* (cssSize × devicePixelRatio)
 * and draw the framebuffer into it with the right scaling filter. This module
 * holds only the pure math — no DOM — so it can be unit-tested directly. The
 * drawing itself lives in ConsoleView's `composite()`.
 *
 * Why this matters (the blur this fixes):
 *   - On a HiDPI / Retina monitor (dpr ≥ 2) a backing store fixed at the
 *     framebuffer size is upscaled by the compositor → soft blur. Matching the
 *     backing store to physical device pixels removes the compositor resample.
 *   - When the window moves to a monitor with a different scale, dpr changes;
 *     recomputing geometry at the new dpr re-sizes the backing store so the
 *     console stays crisp on the second display.
 */

export type FitMode = 'fit' | 'pixel' | 'stretch' | 'resize-guest';

export interface Size {
  width: number;
  height: number;
}

export interface CanvasGeometry {
  /** CSS px the canvas occupies on screen (what the user sees). */
  cssWidth: number;
  cssHeight: number;
}

/**
 * Given a framebuffer size, the container's CSS size, and the fit mode,
 * compute the canvas CSS display size.
 */
export function computeCanvasGeometry(fb: Size, container: Size, fitMode: FitMode): CanvasGeometry {
  let cssWidth: number;
  let cssHeight: number;

  switch (fitMode) {
    case 'pixel': {
      // 1:1 native guest pixels (no CSS scale).
      cssWidth = fb.width;
      cssHeight = fb.height;
      break;
    }
    case 'stretch': {
      cssWidth = container.width;
      cssHeight = container.height;
      break;
    }
    case 'fit':
    case 'resize-guest':
    default: {
      const aspect = fb.width / fb.height;
      cssWidth = container.width;
      cssHeight = cssWidth / aspect;
      if (cssHeight > container.height) {
        cssHeight = container.height;
        cssWidth = cssHeight * aspect;
      }
      break;
    }
  }

  return { cssWidth, cssHeight };
}
