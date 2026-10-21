import { describe, it, expect } from 'vitest';
import { rectExceedsFramebuffer } from '../rectGeometry';
import type { Rect } from '../encodings';

const r = (x: number, y: number, w: number, h: number, encoding: number): Rect => ({
  x,
  y,
  w,
  h,
  encoding,
});

describe('rectExceedsFramebuffer', () => {
  const FB_W = 1024;
  const FB_H = 768;

  it('accepts a real rect that fits exactly to the bottom-right corner', () => {
    expect(rectExceedsFramebuffer(r(0, 0, FB_W, FB_H, 0), FB_W, FB_H)).toBe(false);
    expect(rectExceedsFramebuffer(r(FB_W - 1, FB_H - 1, 1, 1, 7), FB_W, FB_H)).toBe(false);
  });

  it('rejects a real rect that overruns horizontally', () => {
    expect(rectExceedsFramebuffer(r(0, 0, FB_W + 1, FB_H, 0), FB_W, FB_H)).toBe(true);
    expect(rectExceedsFramebuffer(r(1, 0, FB_W, FB_H, 0), FB_W, FB_H)).toBe(true);
  });

  it('rejects a real rect that overruns vertically', () => {
    expect(rectExceedsFramebuffer(r(0, 0, FB_W, FB_H + 1, 0), FB_W, FB_H)).toBe(true);
    expect(rectExceedsFramebuffer(r(0, 1, FB_W, FB_H, 0), FB_W, FB_H)).toBe(true);
  });

  it('rejects the decompression-bomb header from a few wire bytes', () => {
    // 65535x65535 advertised from a handful of bytes — the canonical desync.
    expect(rectExceedsFramebuffer(r(0, 0, 65535, 65535, 0), FB_W, FB_H)).toBe(true);
  });

  it('exempts pseudo-encodings (negative encoding) regardless of geometry', () => {
    // DesktopSize / ExtendedDesktopSize / Cursor carry their own geometry and
    // are negative — they must NOT trip the bounds guard even when (w,h)
    // exceed the current framebuffer (a resize-up announce does exactly this).
    expect(rectExceedsFramebuffer(r(0, 0, 65535, 65535, -223), FB_W, FB_H)).toBe(false); // ExtendedDesktopSize
    expect(rectExceedsFramebuffer(r(0, 0, FB_W * 2, FB_H * 2, -239), FB_W, FB_H)).toBe(false); // Cursor
    expect(rectExceedsFramebuffer(r(0, 0, 9000, 9000, -1), FB_W, FB_H)).toBe(false);
  });

  it('treats encoding 0 (Raw) as a real rect subject to the bounds check', () => {
    expect(rectExceedsFramebuffer(r(0, 0, FB_W + 1, FB_H, 0), FB_W, FB_H)).toBe(true);
  });
});
