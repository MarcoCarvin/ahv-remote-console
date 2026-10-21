/**
 * paintRects dispatch tests: per-rect putImageData for scattered small rects
 * versus the single bbox-union putImageData for clustered (video) rects.
 *
 * The decision rule under test: paint per-rect when the bbox union would
 * overpaint badly (bboxArea > 4 × total rect area) AND the rect count is
 * small (≤ 16); otherwise keep the single-union path.
 */

import { describe, expect, it } from 'vitest';
import type { Framebuffer, Rect, RFBClient } from '../../rfb/client';

// framebufferCanvas.ts touches DOM globals at module scope (a scratch canvas
// for cursor rendering) and constructs ImageData in the paint path. Provide
// minimal stand-ins so the module loads under the node test environment.
class StubImageData {
  constructor(
    public data: Uint8ClampedArray,
    public width: number,
    public height: number,
  ) {}
}
(globalThis as Record<string, unknown>).ImageData ??= StubImageData;
(globalThis as Record<string, unknown>).document ??= {
  createElement: () => ({ getContext: () => null }),
};

const { paintRects } = await import('./framebufferCanvas');

interface PutCall {
  dirtyX: number;
  dirtyY: number;
  dirtyW: number;
  dirtyH: number;
}

function makeCanvas(): { canvas: HTMLCanvasElement; calls: PutCall[] } {
  const calls: PutCall[] = [];
  const ctx = {
    putImageData: (
      _img: unknown,
      _dx: number,
      _dy: number,
      dirtyX: number,
      dirtyY: number,
      dirtyW: number,
      dirtyH: number,
    ) => {
      calls.push({ dirtyX, dirtyY, dirtyW, dirtyH });
    },
  };
  const canvas = { width: 0, height: 0, getContext: () => ctx };
  return { canvas: canvas as unknown as HTMLCanvasElement, calls };
}

function makeClient(width: number, height: number): RFBClient {
  const pixels = new Uint8ClampedArray(width * height * 4);
  const fb: Framebuffer = {
    width,
    height,
    pixels,
    pixels32: new Uint32Array(pixels.buffer),
  };
  return { framebuffer: () => fb } as unknown as RFBClient;
}

function r(x: number, y: number, w: number, h: number): Rect {
  return { x, y, w, h, encoding: 0 };
}

describe('paintRects split/union decision', () => {
  it('scattered small rects (bbox > 4× total area) paint per-rect', () => {
    const { canvas, calls } = makeCanvas();
    const client = makeClient(1000, 1000);
    // Cursor corner + clock corner: 200px dirty, bbox 1,000,000px.
    paintRects(canvas, client, [r(0, 0, 10, 10), r(990, 990, 10, 10)]);
    expect(calls).toEqual([
      { dirtyX: 0, dirtyY: 0, dirtyW: 10, dirtyH: 10 },
      { dirtyX: 990, dirtyY: 990, dirtyW: 10, dirtyH: 10 },
    ]);
  });

  it('clustered rects (bbox ≤ 4× total area) paint a single union', () => {
    const { canvas, calls } = makeCanvas();
    const client = makeClient(1000, 1000);
    // Adjacent video tiles: bbox exactly covers the rects (no overpaint).
    paintRects(canvas, client, [r(0, 0, 100, 100), r(100, 0, 100, 100)]);
    expect(calls).toEqual([{ dirtyX: 0, dirtyY: 0, dirtyW: 200, dirtyH: 100 }]);
  });

  it('bbox exactly 4× total area stays on the union path (strict >)', () => {
    const { canvas, calls } = makeCanvas();
    const client = makeClient(1000, 1000);
    // total = 200, bbox = 80×10 = 800 = 4 × 200 → union.
    paintRects(canvas, client, [r(0, 0, 10, 10), r(70, 0, 10, 10)]);
    expect(calls).toEqual([{ dirtyX: 0, dirtyY: 0, dirtyW: 80, dirtyH: 10 }]);
  });

  it('bbox just over 4× total area splits per-rect', () => {
    const { canvas, calls } = makeCanvas();
    const client = makeClient(1000, 1000);
    // total = 200, bbox = 81×10 = 810 > 800 → split.
    paintRects(canvas, client, [r(0, 0, 10, 10), r(71, 0, 10, 10)]);
    expect(calls).toEqual([
      { dirtyX: 0, dirtyY: 0, dirtyW: 10, dirtyH: 10 },
      { dirtyX: 71, dirtyY: 0, dirtyW: 10, dirtyH: 10 },
    ]);
  });

  it('16 scattered rects still split per-rect (boundary of the count cap)', () => {
    const { canvas, calls } = makeCanvas();
    const client = makeClient(1000, 1000);
    const rects: Rect[] = [];
    for (let i = 0; i < 16; i++) rects.push(r(i * 60, i * 60, 1, 1));
    paintRects(canvas, client, rects);
    expect(calls.length).toBe(16);
    expect(calls[15]).toEqual({ dirtyX: 900, dirtyY: 900, dirtyW: 1, dirtyH: 1 });
  });

  it('more than 16 rects always use the union, even when scattered', () => {
    const { canvas, calls } = makeCanvas();
    const client = makeClient(1000, 1000);
    const rects: Rect[] = [];
    for (let i = 0; i < 17; i++) rects.push(r(i * 50, i * 50, 1, 1));
    paintRects(canvas, client, rects);
    expect(calls).toEqual([{ dirtyX: 0, dirtyY: 0, dirtyW: 801, dirtyH: 801 }]);
  });

  it('invalid rects are excluded from both the decision and the per-rect paint', () => {
    const { canvas, calls } = makeCanvas();
    const client = makeClient(1000, 1000);
    paintRects(canvas, client, [
      r(0, 0, 10, 10),
      r(5, 5, 0, 10), // zero width — invalid
      r(995, 995, 10, 10), // overruns the framebuffer — invalid
      r(990, 990, 10, 10),
    ]);
    expect(calls).toEqual([
      { dirtyX: 0, dirtyY: 0, dirtyW: 10, dirtyH: 10 },
      { dirtyX: 990, dirtyY: 990, dirtyW: 10, dirtyH: 10 },
    ]);
  });

  it('paints nothing when no rect is valid', () => {
    const { canvas, calls } = makeCanvas();
    const client = makeClient(100, 100);
    paintRects(canvas, client, [r(0, 0, 0, 0), r(99, 99, 10, 10)]);
    expect(calls).toEqual([]);
  });
});
