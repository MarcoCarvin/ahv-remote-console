/**
 * Crisp-rendering geometry tests.
 *
 * These test the CSS layout computation for the VM console canvas. The canvas
 * backing store now matches the framebuffer exactly (NoVNC style), so these
 * geometry functions simply compute how to scale the canvas using CSS to fit
 * the container.
 */

import { describe, it, expect } from 'vitest';
import { computeCanvasGeometry } from '../canvasGeometry';

const fb1080 = { width: 1920, height: 1080 };

describe('computeCanvasGeometry — aspect ratio preserved in fit/resize-guest', () => {
  it('fit mode: width-bound tall container keeps the framebuffer aspect', () => {
    // Container is wider than the fb aspect → height-bound; width derived.
    const g = computeCanvasGeometry(fb1080, { width: 1280, height: 720 }, 'fit');
    expect(g.cssWidth).toBeCloseTo(1280);
    expect(g.cssHeight).toBeCloseTo(720);
  });

  it('width-bound: tall container keeps the framebuffer aspect', () => {
    const g = computeCanvasGeometry(fb1080, { width: 800, height: 2000 }, 'fit');
    expect(g.cssWidth).toBeCloseTo(800);
    expect(g.cssHeight).toBeCloseTo(800 / (1920 / 1080));
  });

  it('height-bound: wide container keeps the framebuffer aspect', () => {
    const g = computeCanvasGeometry(fb1080, { width: 4000, height: 600 }, 'fit');
    expect(g.cssHeight).toBeCloseTo(600);
    expect(g.cssWidth).toBeCloseTo(600 * (1920 / 1080));
  });

  it('resize-guest matches fit geometry exactly', () => {
    const container = { width: 1500, height: 1200 };
    const fit = computeCanvasGeometry(fb1080, container, 'fit');
    const rg = computeCanvasGeometry(fb1080, container, 'resize-guest');
    expect(rg).toEqual(fit);
  });
});

describe('computeCanvasGeometry — stretch', () => {
  it('fills the container, ignores aspect', () => {
    const g = computeCanvasGeometry(fb1080, { width: 1000, height: 1000 }, 'stretch');
    expect(g.cssWidth).toBe(1000);
    expect(g.cssHeight).toBe(1000);
  });
});

describe('computeCanvasGeometry — pixel (1:1 native guest pixels)', () => {
  it('CSS size equals the framebuffer exactly', () => {
    const g = computeCanvasGeometry(fb1080, { width: 800, height: 600 }, 'pixel');
    expect(g.cssWidth).toBe(1920);
    expect(g.cssHeight).toBe(1080);
  });
});
