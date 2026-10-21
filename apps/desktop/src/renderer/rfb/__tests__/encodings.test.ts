/**
 * RFB encoding decoder tests.
 *
 * The codebase decodes the encodings exercised below: Raw, CopyRect,
 * Hextile, ZRLE, plus the pseudo-encodings (Cursor, DesktopSize,
 * ExtendedDesktopSize) which are handled in client.ts.
 *
 * Pixel format conversion: the server sends BGRX little-endian (what
 * setPreferredPixelFormat asks for); the framebuffer stores canvas-
 * native RGBA. The decoders are responsible for the swap. Tests verify
 * the swap is correct end-to-end by pushing known BGRX bytes onto a
 * StreamReader and asserting the resulting RGBA pixels.
 */

import { Zlib, zlibSync } from 'fflate';
import { describe, expect, it } from 'vitest';
import {
  decodeRaw,
  decodeCopyRect,
  decodeHextile,
  decodeZRLE,
  DecoderResult,
  newFramebuffer,
  ZRLEInflater,
  type Framebuffer,
  type Rect,
} from '../encodings';
import { StreamReader } from '../reader';
import { ENC_RAW, ENC_COPY_RECT, ENC_HEXTILE, ENC_ZRLE } from '../protocol';

// ─── Helpers ────────────────────────────────────────────────────────────

/** Push raw bytes onto a fresh reader. */
function readerFrom(...chunks: Uint8Array[]): StreamReader {
  const r = new StreamReader();
  for (const c of chunks) r.push(c);
  return r;
}

/** Build a BGRX pixel buffer of width × height with a per-pixel callback
 *  that returns the desired (R, G, B) triple. Returns the bytes as
 *  they'd arrive on the wire from a server that negotiated BGRX little-
 *  endian (decodeRaw / Hextile-Raw / Cursor all expect this byte order).
 */
function bgrxPattern(
  w: number,
  h: number,
  pixelFn: (x: number, y: number) => [number, number, number],
): Uint8Array {
  const out = new Uint8Array(w * h * 4);
  for (let y = 0; y < h; y++) {
    for (let x = 0; x < w; x++) {
      const [r, g, b] = pixelFn(x, y);
      const off = (y * w + x) * 4;
      out[off] = b;
      out[off + 1] = g;
      out[off + 2] = r;
      out[off + 3] = 0; // X / alpha placeholder
    }
  }
  return out;
}

/** Read the RGBA pixel at (x, y) from a framebuffer. */
function rgbaAt(fb: Framebuffer, x: number, y: number): [number, number, number, number] {
  const o = (y * fb.width + x) * 4;
  return [fb.pixels[o]!, fb.pixels[o + 1]!, fb.pixels[o + 2]!, fb.pixels[o + 3]!];
}

/** Make a Rect, defaulting encoding to whatever's appropriate. */
function rect(x: number, y: number, w: number, h: number, encoding: number): Rect {
  return { x, y, w, h, encoding };
}

// ─── Raw ────────────────────────────────────────────────────────────────

describe('decodeRaw', () => {
  it('decodes a 2×2 BGRX → RGBA block and reorders bytes', () => {
    const fb = newFramebuffer(2, 2);
    // Pixel (0,0) = pure red, (1,0) = pure green, (0,1) = pure blue, (1,1) = grey
    const src = bgrxPattern(2, 2, (x, y) => {
      if (x === 0 && y === 0) return [255, 0, 0];
      if (x === 1 && y === 0) return [0, 255, 0];
      if (x === 0 && y === 1) return [0, 0, 255];
      return [128, 128, 128];
    });
    const reader = readerFrom(src);
    const result = decodeRaw(reader, fb, rect(0, 0, 2, 2, ENC_RAW));
    expect(result).toBe(DecoderResult.Done);
    expect(rgbaAt(fb, 0, 0)).toEqual([255, 0, 0, 255]);
    expect(rgbaAt(fb, 1, 0)).toEqual([0, 255, 0, 255]);
    expect(rgbaAt(fb, 0, 1)).toEqual([0, 0, 255, 255]);
    expect(rgbaAt(fb, 1, 1)).toEqual([128, 128, 128, 255]);
  });

  it('places the rect at the correct (x, y) offset in the framebuffer', () => {
    const fb = newFramebuffer(8, 8);
    // 2×2 red block at (3, 4).
    const src = bgrxPattern(2, 2, () => [200, 100, 50]);
    decodeRaw(readerFrom(src), fb, rect(3, 4, 2, 2, ENC_RAW));
    expect(rgbaAt(fb, 3, 4)).toEqual([200, 100, 50, 255]);
    expect(rgbaAt(fb, 4, 4)).toEqual([200, 100, 50, 255]);
    expect(rgbaAt(fb, 3, 5)).toEqual([200, 100, 50, 255]);
    expect(rgbaAt(fb, 4, 5)).toEqual([200, 100, 50, 255]);
    // Outside the rect: still zeros.
    expect(rgbaAt(fb, 0, 0)).toEqual([0, 0, 0, 0]);
    expect(rgbaAt(fb, 5, 4)).toEqual([0, 0, 0, 0]);
  });

  it('returns NeedMore and does not consume the reader on partial data', () => {
    const fb = newFramebuffer(4, 4);
    // Only push 5 bytes instead of the required 64 (4×4×4).
    const partial = new Uint8Array([1, 2, 3, 4, 5]);
    const reader = readerFrom(partial);
    const result = decodeRaw(reader, fb, rect(0, 0, 4, 4, ENC_RAW));
    expect(result).toBe(DecoderResult.NeedMore);
    expect(reader.available()).toBe(5); // bytes still pending
  });

  it('handles a 1×1 rect', () => {
    const fb = newFramebuffer(4, 4);
    const src = bgrxPattern(1, 1, () => [10, 20, 30]);
    const result = decodeRaw(readerFrom(src), fb, rect(2, 2, 1, 1, ENC_RAW));
    expect(result).toBe(DecoderResult.Done);
    expect(rgbaAt(fb, 2, 2)).toEqual([10, 20, 30, 255]);
  });

  it('handles a large 64×64 rect', () => {
    const w = 64;
    const fb = newFramebuffer(w, w);
    const src = bgrxPattern(w, w, (x, y) => [x & 0xff, y & 0xff, (x ^ y) & 0xff]);
    const result = decodeRaw(readerFrom(src), fb, rect(0, 0, w, w, ENC_RAW));
    expect(result).toBe(DecoderResult.Done);
    // Spot-check a handful of pixels.
    expect(rgbaAt(fb, 0, 0)).toEqual([0, 0, 0, 255]);
    expect(rgbaAt(fb, 63, 0)).toEqual([63, 0, 63, 255]);
    expect(rgbaAt(fb, 0, 63)).toEqual([0, 63, 63, 255]);
    expect(rgbaAt(fb, 63, 63)).toEqual([63, 63, 0, 255]);
    expect(rgbaAt(fb, 17, 31)).toEqual([17, 31, 17 ^ 31, 255]);
  });

  it('resumes correctly after NeedMore once data arrives', () => {
    const fb = newFramebuffer(2, 2);
    const src = bgrxPattern(2, 2, () => [50, 100, 150]);
    const reader = new StreamReader();
    const r = rect(0, 0, 2, 2, ENC_RAW);
    // First push only half — decoder must return NeedMore.
    reader.push(src.subarray(0, 8));
    expect(decodeRaw(reader, fb, r)).toBe(DecoderResult.NeedMore);
    // Now push the rest.
    reader.push(src.subarray(8));
    expect(decodeRaw(reader, fb, r)).toBe(DecoderResult.Done);
    expect(rgbaAt(fb, 0, 0)).toEqual([50, 100, 150, 255]);
    expect(rgbaAt(fb, 1, 1)).toEqual([50, 100, 150, 255]);
  });
});

// ─── Raw at 16bpp (RGB565 low-color mode) ───────────────────────────────
//
// Wire pixels are little-endian u16 rrrrrggg gggbbbbb; the decoder widens to
// RGBA8888 by bit replication ((r5 << 3) | (r5 >> 2)). Golden vectors are
// hand-computed: 0xF800 → [255,0,0], 0x07E0 → [0,255,0], 0x001F → [0,0,255],
// and 0x5285 (r5=10, g6=20, b5=5) → [82, 81, 41].
describe('decodeRaw (lowColor / RGB565)', () => {
  it('decodes a 2×2 RGB565 block against hand-computed RGBA8888 values', () => {
    const fb = newFramebuffer(2, 2);
    const src = new Uint8Array([
      0x00,
      0xf8, // (0,0) 0xF800 pure red
      0xe0,
      0x07, // (1,0) 0x07E0 pure green
      0x1f,
      0x00, // (0,1) 0x001F pure blue
      0x85,
      0x52, // (1,1) 0x5285 mid color
    ]);
    const reader = readerFrom(src);
    expect(decodeRaw(reader, fb, rect(0, 0, 2, 2, ENC_RAW), true)).toBe(DecoderResult.Done);
    expect(rgbaAt(fb, 0, 0)).toEqual([255, 0, 0, 255]);
    expect(rgbaAt(fb, 1, 0)).toEqual([0, 255, 0, 255]);
    expect(rgbaAt(fb, 0, 1)).toEqual([0, 0, 255, 255]);
    expect(rgbaAt(fb, 1, 1)).toEqual([82, 81, 41, 255]);
    expect(reader.available()).toBe(0); // exactly w*h*2 consumed
  });

  it('consumes exactly 2 bytes per pixel — a trailing byte survives', () => {
    const fb = newFramebuffer(4, 4);
    const reader = readerFrom(new Uint8Array([0xff, 0xff, 0xab]));
    expect(decodeRaw(reader, fb, rect(1, 1, 1, 1, ENC_RAW), true)).toBe(DecoderResult.Done);
    expect(rgbaAt(fb, 1, 1)).toEqual([255, 255, 255, 255]); // 0xFFFF = white
    expect(reader.available()).toBe(1);
  });

  it('returns NeedMore at the 16bpp byte count, not the 32bpp one', () => {
    const fb = newFramebuffer(2, 2);
    // 2×2 at 16bpp needs 8 bytes; 7 are present → NeedMore, nothing consumed.
    const reader = readerFrom(new Uint8Array(7));
    expect(decodeRaw(reader, fb, rect(0, 0, 2, 2, ENC_RAW), true)).toBe(DecoderResult.NeedMore);
    expect(reader.available()).toBe(7);
  });
});

// ─── CopyRect ───────────────────────────────────────────────────────────

describe('decodeCopyRect', () => {
  it('reads src x/y as big-endian u16s and copies the rect', () => {
    const fb = newFramebuffer(8, 8);
    // Plant a red 2×2 block at (0, 0).
    for (let y = 0; y < 2; y++) {
      for (let x = 0; x < 2; x++) {
        const o = (y * 8 + x) * 4;
        fb.pixels[o] = 255;
        fb.pixels[o + 1] = 0;
        fb.pixels[o + 2] = 0;
        fb.pixels[o + 3] = 255;
      }
    }
    // Tell decoder to copy from (0, 0) to (4, 4). Wire format: u16 sx, u16 sy.
    const reader = readerFrom(new Uint8Array([0, 0, 0, 0]));
    const result = decodeCopyRect(reader, fb, rect(4, 4, 2, 2, ENC_COPY_RECT));
    expect(result).toBe(DecoderResult.Done);
    expect(rgbaAt(fb, 4, 4)).toEqual([255, 0, 0, 255]);
    expect(rgbaAt(fb, 5, 5)).toEqual([255, 0, 0, 255]);
  });

  it('correctly handles overlapping copies when sy < dy (bottom-up sweep)', () => {
    // Scrolling-down case: copying source upward into dest below it. The
    // decoder must walk rows bottom-up so it doesn't overwrite source
    // before reading it.
    const fb = newFramebuffer(4, 8);
    // Fill rows 0..3 with row-index colors.
    for (let y = 0; y < 4; y++) {
      for (let x = 0; x < 4; x++) {
        const o = (y * 4 + x) * 4;
        fb.pixels[o] = y * 60; // R = row index × 60
        fb.pixels[o + 1] = 0;
        fb.pixels[o + 2] = 0;
        fb.pixels[o + 3] = 255;
      }
    }
    // Copy rows 0..3 to rows 2..5 (overlapping).
    const reader = readerFrom(new Uint8Array([0, 0, 0, 0])); // src (0, 0)
    decodeCopyRect(reader, fb, rect(0, 2, 4, 4, ENC_COPY_RECT));
    // After: rows 2..5 should contain the original row 0..3 colors.
    expect(rgbaAt(fb, 0, 2)[0]).toBe(0);
    expect(rgbaAt(fb, 0, 3)[0]).toBe(60);
    expect(rgbaAt(fb, 0, 4)[0]).toBe(120);
    expect(rgbaAt(fb, 0, 5)[0]).toBe(180);
  });

  it('correctly handles overlapping copies when sy > dy (top-down sweep)', () => {
    // Scrolling-up case: source below, dest above. Top-down sweep is correct.
    const fb = newFramebuffer(4, 8);
    // Plant a row-index gradient in rows 4..7.
    for (let y = 0; y < 4; y++) {
      for (let x = 0; x < 4; x++) {
        const o = ((y + 4) * 4 + x) * 4;
        fb.pixels[o + 1] = y * 60; // G = row-within-source × 60
        fb.pixels[o + 3] = 255;
      }
    }
    // Wire bytes: sx=0, sy=4 → big-endian u16s → [0,0, 0,4].
    const reader = readerFrom(new Uint8Array([0, 0, 0, 4]));
    decodeCopyRect(reader, fb, rect(0, 2, 4, 4, ENC_COPY_RECT));
    expect(rgbaAt(fb, 0, 2)[1]).toBe(0);
    expect(rgbaAt(fb, 0, 3)[1]).toBe(60);
    expect(rgbaAt(fb, 0, 4)[1]).toBe(120);
    expect(rgbaAt(fb, 0, 5)[1]).toBe(180);
  });

  it('returns NeedMore when fewer than 4 wire bytes are available', () => {
    const fb = newFramebuffer(4, 4);
    const reader = readerFrom(new Uint8Array([0, 0])); // only 2 bytes
    const result = decodeCopyRect(reader, fb, rect(0, 0, 2, 2, ENC_COPY_RECT));
    expect(result).toBe(DecoderResult.NeedMore);
    expect(reader.available()).toBe(2);
  });

  it('handles a 1×1 copy', () => {
    const fb = newFramebuffer(4, 4);
    // Plant a single yellow pixel at (0, 0).
    fb.pixels[0] = 255;
    fb.pixels[1] = 255;
    fb.pixels[2] = 0;
    fb.pixels[3] = 255;
    const reader = readerFrom(new Uint8Array([0, 0, 0, 0]));
    decodeCopyRect(reader, fb, rect(3, 3, 1, 1, ENC_COPY_RECT));
    expect(rgbaAt(fb, 3, 3)).toEqual([255, 255, 0, 255]);
  });
});

// ─── Hextile ────────────────────────────────────────────────────────────

describe('decodeHextile', () => {
  // Encode a single tile with the Raw sub-encoding (mask = 0x01).
  function encodeRawTile(
    tileW: number,
    tileH: number,
    color: [number, number, number],
  ): Uint8Array {
    const out = new Uint8Array(1 + tileW * tileH * 4);
    out[0] = 0x01; // Raw sub-encoding
    for (let i = 0; i < tileW * tileH; i++) {
      out[1 + i * 4] = color[2]; // B
      out[1 + i * 4 + 1] = color[1]; // G
      out[1 + i * 4 + 2] = color[0]; // R
      out[1 + i * 4 + 3] = 0;
    }
    return out;
  }

  it('decodes a single 16×16 Raw tile (entire 16x16 rect)', () => {
    const fb = newFramebuffer(16, 16);
    const payload = encodeRawTile(16, 16, [200, 100, 50]);
    const reader = readerFrom(payload);
    const result = decodeHextile(reader, fb, rect(0, 0, 16, 16, ENC_HEXTILE));
    expect(result).toBe(DecoderResult.Done);
    expect(rgbaAt(fb, 0, 0)).toEqual([200, 100, 50, 255]);
    expect(rgbaAt(fb, 15, 15)).toEqual([200, 100, 50, 255]);
  });

  it('decodes a tile with Background-only (mask = 0x02)', () => {
    const fb = newFramebuffer(16, 16);
    // mask=0x02: just BG color (BGRX 4 bytes).
    const payload = new Uint8Array([0x02, 50, 100, 150, 0]);
    const result = decodeHextile(readerFrom(payload), fb, rect(0, 0, 16, 16, ENC_HEXTILE));
    expect(result).toBe(DecoderResult.Done);
    // BG = R=150, G=100, B=50.
    expect(rgbaAt(fb, 0, 0)).toEqual([150, 100, 50, 255]);
    expect(rgbaAt(fb, 8, 8)).toEqual([150, 100, 50, 255]);
  });

  it('decodes a tile with BG + uncolored subrects using FG (mask = 0x0E)', () => {
    const fb = newFramebuffer(16, 16);
    // mask = BG | FG | AnySubrects = 0x02 | 0x04 | 0x08 = 0x0E
    // BG = red (BGRX: 0, 0, 200, 0)
    // FG = green (BGRX: 0, 200, 0, 0)
    // 1 subrect at (4,4) sized 3×3 filled with FG.
    //   subrect: xy-byte = (4 << 4) | 4 = 0x44
    //            wh-byte = ((3-1) << 4) | (3-1) = 0x22
    const payload = new Uint8Array([
      0x0e, // mask: BG + FG + AnySubrects
      0,
      0,
      200,
      0, // BG: BGRX, R=200
      0,
      200,
      0,
      0, // FG: BGRX, G=200
      1, // 1 subrect
      0x44, // x=4, y=4
      0x22, // w=3, h=3
    ]);
    const result = decodeHextile(readerFrom(payload), fb, rect(0, 0, 16, 16, ENC_HEXTILE));
    expect(result).toBe(DecoderResult.Done);
    // BG everywhere except the subrect.
    expect(rgbaAt(fb, 0, 0)).toEqual([200, 0, 0, 255]);
    expect(rgbaAt(fb, 3, 3)).toEqual([200, 0, 0, 255]);
    // Subrect: (4,4) to (6,6) inclusive (w=3, h=3 from wh-byte).
    expect(rgbaAt(fb, 4, 4)).toEqual([0, 200, 0, 255]);
    expect(rgbaAt(fb, 6, 6)).toEqual([0, 200, 0, 255]);
    expect(rgbaAt(fb, 7, 7)).toEqual([200, 0, 0, 255]); // back to BG
  });

  it('decodes a tile with coloured subrects (mask = 0x1A)', () => {
    const fb = newFramebuffer(16, 16);
    // mask = BG | AnySubrects | SubrectsColoured = 0x02 | 0x08 | 0x10 = 0x1A
    // 2 subrects, each carrying its own BGRX color.
    const payload = new Uint8Array([
      0x1a,
      0,
      0,
      0,
      0, // BG = black
      2, // 2 subrects
      // subrect 0: red 1×1 at (1, 1)
      0,
      0,
      250,
      0, // BGRX color
      0x11, // x=1, y=1
      0x00, // w=1, h=1
      // subrect 1: blue 2×2 at (5, 5)
      250,
      0,
      0,
      0,
      0x55,
      0x11,
    ]);
    decodeHextile(readerFrom(payload), fb, rect(0, 0, 16, 16, ENC_HEXTILE));
    expect(rgbaAt(fb, 0, 0)).toEqual([0, 0, 0, 255]);
    expect(rgbaAt(fb, 1, 1)).toEqual([250, 0, 0, 255]);
    expect(rgbaAt(fb, 5, 5)).toEqual([0, 0, 250, 255]);
    expect(rgbaAt(fb, 6, 6)).toEqual([0, 0, 250, 255]);
    expect(rgbaAt(fb, 7, 7)).toEqual([0, 0, 0, 255]);
  });

  it('decodes a multi-tile rect (3×2 tiles = 48×32 pixels)', () => {
    const fb = newFramebuffer(48, 32);
    // Each tile is BG-only with a unique color. Tile order is row-major:
    // (0,0), (16,0), (32,0), (0,16), (16,16), (32,16).
    const tile = (r: number, g: number, b: number) => new Uint8Array([0x02, b, g, r, 0]);
    const payload = new Uint8Array([
      ...tile(10, 0, 0),
      ...tile(0, 20, 0),
      ...tile(0, 0, 30),
      ...tile(40, 40, 0),
      ...tile(0, 50, 50),
      ...tile(60, 0, 60),
    ]);
    const result = decodeHextile(readerFrom(payload), fb, rect(0, 0, 48, 32, ENC_HEXTILE));
    expect(result).toBe(DecoderResult.Done);
    expect(rgbaAt(fb, 0, 0)[0]).toBe(10);
    expect(rgbaAt(fb, 16, 0)[1]).toBe(20);
    expect(rgbaAt(fb, 32, 0)[2]).toBe(30);
    expect(rgbaAt(fb, 0, 16)[0]).toBe(40);
    expect(rgbaAt(fb, 16, 16)[1]).toBe(50);
    expect(rgbaAt(fb, 32, 16)[2]).toBe(60);
  });

  it('handles non-16-aligned trailing tile (rect not a multiple of 16)', () => {
    // 20×20 rect: row 0 = tiles (16×16) + (4×16), row 1 = (16×4) + (4×4).
    // mask=0x02 means BG only (4 BGRX bytes follow); the actual tile size
    // is implicit (computed from the rect bounds in the decoder).
    const fb = newFramebuffer(20, 20);
    const tileBG = (b: number, g: number, r: number) => new Uint8Array([0x02, b, g, r, 0]);
    const payload = new Uint8Array([
      ...tileBG(0, 0, 10),
      ...tileBG(0, 0, 20),
      ...tileBG(0, 0, 30),
      ...tileBG(0, 0, 40),
    ]);
    decodeHextile(readerFrom(payload), fb, rect(0, 0, 20, 20, ENC_HEXTILE));
    expect(rgbaAt(fb, 0, 0)[0]).toBe(10);
    expect(rgbaAt(fb, 16, 0)[0]).toBe(20); // right strip
    expect(rgbaAt(fb, 0, 16)[0]).toBe(30); // bottom strip
    expect(rgbaAt(fb, 16, 16)[0]).toBe(40); // bottom-right corner
    expect(rgbaAt(fb, 19, 19)[0]).toBe(40);
  });

  it('returns NeedMore on partial tile data', () => {
    const fb = newFramebuffer(16, 16);
    // Send only the mask byte for a Raw tile — decoder needs 1+1024 bytes.
    const partial = new Uint8Array([0x01, 0, 0, 0]);
    const reader = readerFrom(partial);
    const result = decodeHextile(reader, fb, rect(0, 0, 16, 16, ENC_HEXTILE));
    expect(result).toBe(DecoderResult.NeedMore);
  });

  it('decodes a 0×0 / empty rect immediately (no payload required)', () => {
    const fb = newFramebuffer(16, 16);
    // Empty rect: 0 tiles — the outer while loops never execute.
    const reader = readerFrom(new Uint8Array(0));
    const result = decodeHextile(reader, fb, rect(0, 0, 0, 0, ENC_HEXTILE));
    expect(result).toBe(DecoderResult.Done);
  });

  it('persists tile state across NeedMore returns (resume after partial data)', () => {
    // Two tiles. Push the first complete, the second only partially.
    // Decoder should: (a) decode tile 0, (b) return NeedMore at tile 1.
    // After more bytes arrive, decoder resumes mid-rect via the _hex state.
    const fb = newFramebuffer(32, 16);
    const tile0 = new Uint8Array([0x02, 0, 0, 100, 0]); // BG=red
    const tile1 = new Uint8Array([0x02, 100, 0, 0, 0]); // BG=blue
    const reader = new StreamReader();
    reader.push(tile0);
    reader.push(tile1.subarray(0, 3)); // only first 3 bytes of tile 1
    const r = rect(0, 0, 32, 16, ENC_HEXTILE);
    const first = decodeHextile(reader, fb, r);
    expect(first).toBe(DecoderResult.NeedMore);
    // Tile 0 painted; tile 1 region still zero.
    expect(rgbaAt(fb, 0, 0)).toEqual([100, 0, 0, 255]);
    expect(rgbaAt(fb, 16, 0)).toEqual([0, 0, 0, 0]);
    // Push the rest of tile 1.
    reader.push(tile1.subarray(3));
    const second = decodeHextile(reader, fb, r);
    expect(second).toBe(DecoderResult.Done);
    expect(rgbaAt(fb, 16, 0)).toEqual([0, 0, 100, 255]);
  });
});

// ─── ZRLE ──────────────────────────────────────────────────────────────

/**
 * Build a ZRLE wire message: 4-byte big-endian length + zlib-compressed payload.
 * Uses fflate.zlibSync (full zlib, not raw deflate) to match what RFB servers emit.
 */
function zrleWireBlock(tilePayload: Uint8Array): Uint8Array {
  const compressed = zlibSync(tilePayload);
  const wire = new Uint8Array(4 + compressed.length);
  wire[0] = (compressed.length >> 24) & 0xff;
  wire[1] = (compressed.length >> 16) & 0xff;
  wire[2] = (compressed.length >> 8) & 0xff;
  wire[3] = compressed.length & 0xff;
  wire.set(compressed, 4);
  return wire;
}

/** Build a CPIXEL triplet (B, G, R on wire) from an RGB triple. */
function cpixelBGR(r: number, g: number, b: number): Uint8Array {
  return new Uint8Array([b, g, r]);
}

describe('decodeZRLE', () => {
  it('decodes a solid tile (subencoding 1)', () => {
    const fb = newFramebuffer(4, 4);
    // Tile payload: subencoding=1, CPIXEL(B=50,G=100,R=200)
    const payload = new Uint8Array([1, ...cpixelBGR(200, 100, 50)]);
    const wire = zrleWireBlock(payload);
    const reader = readerFrom(wire);
    const inflater = new ZRLEInflater();
    const result = decodeZRLE(reader, fb, rect(0, 0, 4, 4, ENC_ZRLE), inflater);
    expect(result).toBe(DecoderResult.Done);
    expect(rgbaAt(fb, 0, 0)).toEqual([200, 100, 50, 255]);
    expect(rgbaAt(fb, 3, 3)).toEqual([200, 100, 50, 255]);
  });

  it('decodes a raw tile (subencoding 0)', () => {
    const fb = newFramebuffer(2, 2);
    // 2x2 tile: subencoding=0 + 4 CPIXELs (3 bytes each)
    const pixels = new Uint8Array([
      0, // subencoding = Raw
      ...cpixelBGR(255, 0, 0), // (0,0) red
      ...cpixelBGR(0, 255, 0), // (1,0) green
      ...cpixelBGR(0, 0, 255), // (0,1) blue
      ...cpixelBGR(128, 128, 0), // (1,1) yellow-ish
    ]);
    const wire = zrleWireBlock(pixels);
    const reader = readerFrom(wire);
    const inflater = new ZRLEInflater();
    const result = decodeZRLE(reader, fb, rect(0, 0, 2, 2, ENC_ZRLE), inflater);
    expect(result).toBe(DecoderResult.Done);
    expect(rgbaAt(fb, 0, 0)).toEqual([255, 0, 0, 255]);
    expect(rgbaAt(fb, 1, 0)).toEqual([0, 255, 0, 255]);
    expect(rgbaAt(fb, 0, 1)).toEqual([0, 0, 255, 255]);
    expect(rgbaAt(fb, 1, 1)).toEqual([128, 128, 0, 255]);
  });

  it('decodes a packed palette tile (subencoding 2 = 2-color palette, 1 bit/pixel)', () => {
    const fb = newFramebuffer(4, 2);
    // 4x2 tile with 2-color palette: red and blue
    // 1 bit per pixel, MSB-first, row-padded to byte boundary
    // Row 0: indices [0, 1, 0, 1] = 0b01010000 = 0x50
    // Row 1: indices [1, 0, 1, 0] = 0b10100000 = 0xA0
    const payload = new Uint8Array([
      2, // subencoding = 2-color packed palette
      ...cpixelBGR(255, 0, 0), // palette[0] = red
      ...cpixelBGR(0, 0, 255), // palette[1] = blue
      0x50, // row 0: 0,1,0,1
      0xa0, // row 1: 1,0,1,0
    ]);
    const wire = zrleWireBlock(payload);
    const reader = readerFrom(wire);
    const inflater = new ZRLEInflater();
    const result = decodeZRLE(reader, fb, rect(0, 0, 4, 2, ENC_ZRLE), inflater);
    expect(result).toBe(DecoderResult.Done);
    expect(rgbaAt(fb, 0, 0)).toEqual([255, 0, 0, 255]); // palette[0]
    expect(rgbaAt(fb, 1, 0)).toEqual([0, 0, 255, 255]); // palette[1]
    expect(rgbaAt(fb, 2, 0)).toEqual([255, 0, 0, 255]); // palette[0]
    expect(rgbaAt(fb, 3, 0)).toEqual([0, 0, 255, 255]); // palette[1]
    expect(rgbaAt(fb, 0, 1)).toEqual([0, 0, 255, 255]); // palette[1]
    expect(rgbaAt(fb, 1, 1)).toEqual([255, 0, 0, 255]); // palette[0]
  });

  it('decodes a packed palette tile with 4 colors (2 bits/pixel)', () => {
    const fb = newFramebuffer(4, 1);
    // 4x1 tile with 4-color palette
    // 2 bits/pixel: indices [0, 1, 2, 3] = 0b00011011 = 0x1B
    const payload = new Uint8Array([
      4, // subencoding = 4-color packed palette
      ...cpixelBGR(10, 0, 0), // palette[0]
      ...cpixelBGR(0, 20, 0), // palette[1]
      ...cpixelBGR(0, 0, 30), // palette[2]
      ...cpixelBGR(40, 40, 40), // palette[3]
      0x1b, // 00 01 10 11
    ]);
    const wire = zrleWireBlock(payload);
    const reader = readerFrom(wire);
    const inflater = new ZRLEInflater();
    const result = decodeZRLE(reader, fb, rect(0, 0, 4, 1, ENC_ZRLE), inflater);
    expect(result).toBe(DecoderResult.Done);
    expect(rgbaAt(fb, 0, 0)).toEqual([10, 0, 0, 255]);
    expect(rgbaAt(fb, 1, 0)).toEqual([0, 20, 0, 255]);
    expect(rgbaAt(fb, 2, 0)).toEqual([0, 0, 30, 255]);
    expect(rgbaAt(fb, 3, 0)).toEqual([40, 40, 40, 255]);
  });

  it('decodes plain RLE (subencoding 128)', () => {
    const fb = newFramebuffer(4, 2);
    // 4x2 = 8 pixels. Two runs: 5 red, 3 blue.
    // Run 1: CPIXEL(red), length=5 → byte 4 (5-1=4, since <255 it terminates)
    // Run 2: CPIXEL(blue), length=3 → byte 2
    const payload = new Uint8Array([
      128, // subencoding = plain RLE
      ...cpixelBGR(255, 0, 0),
      4, // 5 red pixels
      ...cpixelBGR(0, 0, 255),
      2, // 3 blue pixels
    ]);
    const wire = zrleWireBlock(payload);
    const reader = readerFrom(wire);
    const inflater = new ZRLEInflater();
    const result = decodeZRLE(reader, fb, rect(0, 0, 4, 2, ENC_ZRLE), inflater);
    expect(result).toBe(DecoderResult.Done);
    // First 5 pixels: red
    expect(rgbaAt(fb, 0, 0)).toEqual([255, 0, 0, 255]);
    expect(rgbaAt(fb, 3, 0)).toEqual([255, 0, 0, 255]);
    expect(rgbaAt(fb, 0, 1)).toEqual([255, 0, 0, 255]); // 5th pixel
    // Last 3 pixels: blue
    expect(rgbaAt(fb, 1, 1)).toEqual([0, 0, 255, 255]);
    expect(rgbaAt(fb, 3, 1)).toEqual([0, 0, 255, 255]);
  });

  it('decodes palette RLE (subencoding 130+)', () => {
    const fb = newFramebuffer(4, 1);
    // 4x1 = 4 pixels. Palette of 2 colors, then RLE runs using palette indices.
    // Palette: red, green
    // Run 1: index 0 (red), MSB clear → single pixel
    // Run 2: index 1 | 0x80 (green, MSB set) → run length follows: byte 2 → len 3
    const payload = new Uint8Array([
      130, // subencoding = 128 + 2 palette entries
      ...cpixelBGR(255, 0, 0), // palette[0] = red
      ...cpixelBGR(0, 255, 0), // palette[1] = green
      0x00, // index 0, MSB clear → 1 red pixel
      0x81, // index 1, MSB set → run length follows
      2, // run length = 2 + 1 = 3 green pixels
    ]);
    const wire = zrleWireBlock(payload);
    const reader = readerFrom(wire);
    const inflater = new ZRLEInflater();
    const result = decodeZRLE(reader, fb, rect(0, 0, 4, 1, ENC_ZRLE), inflater);
    expect(result).toBe(DecoderResult.Done);
    expect(rgbaAt(fb, 0, 0)).toEqual([255, 0, 0, 255]);
    expect(rgbaAt(fb, 1, 0)).toEqual([0, 255, 0, 255]);
    expect(rgbaAt(fb, 2, 0)).toEqual([0, 255, 0, 255]);
    expect(rgbaAt(fb, 3, 0)).toEqual([0, 255, 0, 255]);
  });

  it('decodes a multi-tile rect (tile grid iteration)', () => {
    // 80x80 rect: 2x2 grid of 64x64 tiles, but last col/row = 16px wide/tall.
    // Tiles: (0,0)=64x64, (64,0)=16x64, (0,64)=64x16, (64,64)=16x16
    // Use solid tiles with distinct colors.
    const fb = newFramebuffer(80, 80);
    const payload = new Uint8Array([
      1,
      ...cpixelBGR(10, 0, 0), // tile (0,0): solid red-ish
      1,
      ...cpixelBGR(0, 20, 0), // tile (64,0): solid green-ish
      1,
      ...cpixelBGR(0, 0, 30), // tile (0,64): solid blue-ish
      1,
      ...cpixelBGR(40, 40, 40), // tile (64,64): solid grey
    ]);
    const wire = zrleWireBlock(payload);
    const reader = readerFrom(wire);
    const inflater = new ZRLEInflater();
    const result = decodeZRLE(reader, fb, rect(0, 0, 80, 80, ENC_ZRLE), inflater);
    expect(result).toBe(DecoderResult.Done);
    // Tile (0,0)
    expect(rgbaAt(fb, 0, 0)).toEqual([10, 0, 0, 255]);
    expect(rgbaAt(fb, 63, 63)).toEqual([10, 0, 0, 255]);
    // Tile (64,0) — 16px wide
    expect(rgbaAt(fb, 64, 0)).toEqual([0, 20, 0, 255]);
    expect(rgbaAt(fb, 79, 63)).toEqual([0, 20, 0, 255]);
    // Tile (0,64) — 16px tall
    expect(rgbaAt(fb, 0, 64)).toEqual([0, 0, 30, 255]);
    expect(rgbaAt(fb, 63, 79)).toEqual([0, 0, 30, 255]);
    // Tile (64,64) — 16x16 corner
    expect(rgbaAt(fb, 64, 64)).toEqual([40, 40, 40, 255]);
    expect(rgbaAt(fb, 79, 79)).toEqual([40, 40, 40, 255]);
  });

  it('returns NeedMore when length prefix is incomplete', () => {
    const fb = newFramebuffer(4, 4);
    const reader = readerFrom(new Uint8Array([0, 0])); // only 2 of 4 length bytes
    const inflater = new ZRLEInflater();
    const result = decodeZRLE(reader, fb, rect(0, 0, 4, 4, ENC_ZRLE), inflater);
    expect(result).toBe(DecoderResult.NeedMore);
    expect(reader.available()).toBe(2);
  });

  it('returns NeedMore when compressed data is incomplete', () => {
    const fb = newFramebuffer(4, 4);
    const payload = new Uint8Array([1, ...cpixelBGR(100, 100, 100)]);
    const compressed = zlibSync(payload);
    // Write the full length prefix but only half the compressed data.
    const wire = new Uint8Array(4 + Math.floor(compressed.length / 2));
    wire[0] = (compressed.length >> 24) & 0xff;
    wire[1] = (compressed.length >> 16) & 0xff;
    wire[2] = (compressed.length >> 8) & 0xff;
    wire[3] = compressed.length & 0xff;
    wire.set(compressed.subarray(0, Math.floor(compressed.length / 2)), 4);
    const reader = readerFrom(wire);
    const inflater = new ZRLEInflater();
    const result = decodeZRLE(reader, fb, rect(0, 0, 4, 4, ENC_ZRLE), inflater);
    expect(result).toBe(DecoderResult.NeedMore);
  });

  it('persists zlib state across multiple rects (shared inflater)', () => {
    // Produce two compressed chunks from a SINGLE continuous zlib stream,
    // simulating how a real server emits ZRLE rects: one Zlib instance
    // that flushes (sync-flush) per rect without resetting.
    const compressed1Chunks: Uint8Array[] = [];
    const compressed2Chunks: Uint8Array[] = [];
    let active = compressed1Chunks;
    const def = new Zlib();
    def.ondata = (chunk: Uint8Array) => {
      active.push(new Uint8Array(chunk));
    };
    const tile1 = new Uint8Array([1, ...cpixelBGR(200, 0, 0)]);
    const tile2 = new Uint8Array([1, ...cpixelBGR(0, 0, 200)]);
    def.push(tile1, false);
    def.flush(true); // sync flush so tile1 bytes are immediately decompressible
    active = compressed2Chunks;
    def.push(tile2, false);
    def.flush(true);

    function concat(chunks: Uint8Array[]): Uint8Array {
      const len = chunks.reduce((s, c) => s + c.length, 0);
      const out = new Uint8Array(len);
      let off = 0;
      for (const c of chunks) {
        out.set(c, off);
        off += c.length;
      }
      return out;
    }
    const c1 = concat(compressed1Chunks);
    const c2 = concat(compressed2Chunks);

    function wireFromCompressed(compressed: Uint8Array): Uint8Array {
      const wire = new Uint8Array(4 + compressed.length);
      wire[0] = (compressed.length >> 24) & 0xff;
      wire[1] = (compressed.length >> 16) & 0xff;
      wire[2] = (compressed.length >> 8) & 0xff;
      wire[3] = compressed.length & 0xff;
      wire.set(compressed, 4);
      return wire;
    }

    const fb = newFramebuffer(4, 4);
    const inflater = new ZRLEInflater();
    const r1 = decodeZRLE(
      readerFrom(wireFromCompressed(c1)),
      fb,
      rect(0, 0, 2, 2, ENC_ZRLE),
      inflater,
    );
    expect(r1).toBe(DecoderResult.Done);
    expect(rgbaAt(fb, 0, 0)).toEqual([200, 0, 0, 255]);
    const r2 = decodeZRLE(
      readerFrom(wireFromCompressed(c2)),
      fb,
      rect(2, 2, 2, 2, ENC_ZRLE),
      inflater,
    );
    expect(r2).toBe(DecoderResult.Done);
    expect(rgbaAt(fb, 2, 2)).toEqual([0, 0, 200, 255]);
    expect(rgbaAt(fb, 0, 0)).toEqual([200, 0, 0, 255]);
  });
});

// ─── ZRLEInflater chunk-ownership contract ──────────────────────────────

describe('ZRLEInflater', () => {
  it('tolerates a sub-6-byte first push (zero ondata calls) and completes on the next push', () => {
    // Unzlib can emit NOTHING for a first chunk too short to clear the zlib
    // header — collected is then empty and inflate() must return an empty
    // array, not throw, with the remainder completing on the next call.
    const payload = new Uint8Array(64);
    for (let i = 0; i < payload.length; i++) payload[i] = (i * 7) & 0xff;
    const compressed = zlibSync(payload);
    const inflater = new ZRLEInflater();
    const first = inflater.inflate(compressed.subarray(0, 4));
    expect(first.length).toBeLessThan(payload.length);
    const rest = inflater.inflate(compressed.subarray(4));
    const out = new Uint8Array(first.length + rest.length);
    out.set(first, 0);
    out.set(rest, first.length);
    expect(Array.from(out)).toEqual(Array.from(payload));
  });

  it('output from an earlier inflate() is not corrupted by a later push (caller-owned chunks)', () => {
    // ondata no longer copies the chunk fflate hands over; this pins the
    // assumption that makes that safe — fflate allocates fresh output per
    // chunk rather than recycling its sliding-window buffer.
    const raw1 = new Uint8Array(256);
    const raw2 = new Uint8Array(256);
    for (let i = 0; i < 256; i++) {
      raw1[i] = i;
      raw2[i] = 255 - i;
    }
    const chunks1: Uint8Array[] = [];
    const chunks2: Uint8Array[] = [];
    let active = chunks1;
    const def = new Zlib();
    def.ondata = (c: Uint8Array) => {
      active.push(new Uint8Array(c));
    };
    def.push(raw1, false);
    def.flush(true);
    active = chunks2;
    def.push(raw2, false);
    def.flush(true);
    const concat = (chunks: Uint8Array[]) => {
      const out = new Uint8Array(chunks.reduce((s, c) => s + c.length, 0));
      let off = 0;
      for (const c of chunks) {
        out.set(c, off);
        off += c.length;
      }
      return out;
    };

    const inflater = new ZRLEInflater();
    const out1 = inflater.inflate(concat(chunks1));
    const snapshot = Array.from(out1);
    const out2 = inflater.inflate(concat(chunks2));
    expect(Array.from(out1)).toEqual(snapshot);
    expect(Array.from(out1)).toEqual(Array.from(raw1));
    expect(Array.from(out2)).toEqual(Array.from(raw2));
  });
});

// ─── Framebuffer construction ───────────────────────────────────────────

describe('newFramebuffer', () => {
  it('allocates width × height × 4 bytes initialized to zero', () => {
    const fb = newFramebuffer(10, 20);
    expect(fb.width).toBe(10);
    expect(fb.height).toBe(20);
    expect(fb.pixels.length).toBe(10 * 20 * 4);
    for (const px of fb.pixels) expect(px).toBe(0);
  });

  it('uses Uint8ClampedArray (canvas-compatible)', () => {
    const fb = newFramebuffer(2, 2);
    expect(fb.pixels).toBeInstanceOf(Uint8ClampedArray);
  });
});
