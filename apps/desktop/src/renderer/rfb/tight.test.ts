import { describe, it, expect } from 'vitest';
import { Zlib, zlibSync } from 'fflate';
import { StreamReader } from './reader';
import {
  newFramebuffer,
  decodeTight,
  TightInflater,
  DecoderResult,
  type Framebuffer,
} from './encodings';
import { ENC_TIGHT } from './protocol';
import { readJpegDimensions } from './client';

function pixel(fb: Framebuffer, x: number, y: number): number[] {
  const i = (y * fb.width + x) * 4;
  return [fb.pixels[i]!, fb.pixels[i + 1]!, fb.pixels[i + 2]!, fb.pixels[i + 3]!];
}
function feed(bytes: number[]): StreamReader {
  const r = new StreamReader();
  r.push(new Uint8Array(bytes));
  return r;
}
// Encode a length the way Tight's compact 1-3 byte length does.
function compactLen(n: number): number[] {
  const out: number[] = [];
  out.push((n & 0x7f) | (n > 0x7f ? 0x80 : 0));
  if (n > 0x7f) {
    n >>= 7;
    out.push((n & 0x7f) | (n > 0x7f ? 0x80 : 0));
    if (n > 0x7f) out.push((n >> 7) & 0xff);
  }
  return out;
}
const rect = (w: number, h: number) => ({ x: 0, y: 0, w, h, encoding: ENC_TIGHT });
const noImage = () => {
  /* noop */
};

describe('Tight decoder — byte-exact alignment + pixels', () => {
  it('Fill: fills the rect with a TPIXEL and consumes exactly control+3 bytes', () => {
    const fb = newFramebuffer(4, 4);
    // 0x80 = Fill; TPIXEL [R,G,B] = [10,20,30]; one trailing byte must survive.
    const r = feed([0x80, 10, 20, 30, 0xab]);
    expect(decodeTight(r, fb, rect(2, 2), new TightInflater(), noImage)).toBe(DecoderResult.Done);
    expect(pixel(fb, 0, 0)).toEqual([10, 20, 30, 255]); // R,G,B,A
    expect(pixel(fb, 1, 1)).toEqual([10, 20, 30, 255]);
    expect(r.available()).toBe(1); // exact consumption
  });

  it('Basic Copy, uncompressed (payload < 12 bytes): no length prefix', () => {
    const fb = newFramebuffer(4, 4);
    // ctl 0x00 = basic, stream 0, no filter. 1x2 rect = 6 bytes raw [R,G,B] x2.
    const r = feed([0x00, 1, 2, 3, 4, 5, 6, 0x99]);
    expect(decodeTight(r, fb, rect(1, 2), new TightInflater(), noImage)).toBe(DecoderResult.Done);
    expect(pixel(fb, 0, 0)).toEqual([1, 2, 3, 255]);
    expect(pixel(fb, 0, 1)).toEqual([4, 5, 6, 255]);
    expect(r.available()).toBe(1);
  });

  it('Basic Copy, zlib-compressed (payload >= 12): compact length + zlib stream', () => {
    const fb = newFramebuffer(4, 4);
    const raw = new Uint8Array(16 * 3);
    for (let i = 0; i < 16; i++) {
      raw[i * 3] = i; // R
      raw[i * 3 + 1] = i + 50; // G
      raw[i * 3 + 2] = i + 100; // B
    }
    const comp = zlibSync(raw);
    const r = feed([0x00, ...compactLen(comp.length), ...comp]);
    expect(decodeTight(r, fb, rect(4, 4), new TightInflater(), noImage)).toBe(DecoderResult.Done);
    expect(pixel(fb, 0, 0)).toEqual([0, 50, 100, 255]); // pixel 0
    expect(pixel(fb, 3, 3)).toEqual([15, 65, 115, 255]); // pixel 15
    expect(r.available()).toBe(0);
  });

  it('Basic Palette (2-color, 1 bit/pixel)', () => {
    const fb = newFramebuffer(8, 4);
    // ctl 0x40 = basic + filter-present; filter 1 = palette; numColors-1 = 1 (2 colors).
    // palette: [0,0,0] and [255,255,255]; 8x1 -> 1 index byte 0b10101010.
    const r = feed([0x40, 1, 1, 0, 0, 0, 255, 255, 255, 0b10101010, 0x77]);
    expect(decodeTight(r, fb, rect(8, 1), new TightInflater(), noImage)).toBe(DecoderResult.Done);
    expect(pixel(fb, 0, 0)).toEqual([255, 255, 255, 255]); // bit7 = 1
    expect(pixel(fb, 1, 0)).toEqual([0, 0, 0, 255]); // bit6 = 0
    expect(r.available()).toBe(1);
  });

  it('JPEG: consumes control+length+data and hands raw bytes to onImage', () => {
    const fb = newFramebuffer(4, 4);
    const jpeg = new Uint8Array([0xff, 0xd8, 1, 2, 3, 4, 5, 0xff, 0xd9]);
    let got: Uint8Array | null = null;
    const r = feed([0x90, ...compactLen(jpeg.length), ...jpeg, 0xcc]);
    expect(
      decodeTight(r, fb, rect(2, 2), new TightInflater(), (_rect, j) => {
        got = j;
      }),
    ).toBe(DecoderResult.Done);
    expect(got).not.toBeNull();
    expect(Array.from(got!)).toEqual(Array.from(jpeg));
    expect(r.available()).toBe(1);
  });

  it('two consecutive Basic Copy rects on ONE zlib stream decode byte-exact', () => {
    // A real server compresses every Basic rect of a stream through a single
    // zlib instance with a sync flush per rect — the second rect's output
    // depends on the first rect's LZ77 window. Byte-compares both rects, so
    // any corruption of the first inflated chunk by the second push (the
    // hazard the old ondata defensive copy guarded against) is caught here.
    const fb = newFramebuffer(4, 8);
    const raw1 = new Uint8Array(16 * 3);
    const raw2 = new Uint8Array(16 * 3);
    for (let i = 0; i < 16; i++) {
      raw1[i * 3] = i;
      raw1[i * 3 + 1] = i + 40;
      raw1[i * 3 + 2] = i + 80;
      raw2[i * 3] = 255 - i;
      raw2[i * 3 + 1] = 200 - i;
      raw2[i * 3 + 2] = 150 - i;
    }
    const chunks1: Uint8Array[] = [];
    const chunks2: Uint8Array[] = [];
    let active = chunks1;
    const def = new Zlib();
    def.ondata = (c: Uint8Array) => {
      active.push(new Uint8Array(c));
    };
    def.push(raw1, false);
    def.flush(true); // sync flush: raw1 bytes immediately decompressible
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
    const c1 = concat(chunks1);
    const c2 = concat(chunks2);

    const inflater = new TightInflater();
    const r1 = feed([0x00, ...compactLen(c1.length), ...c1]);
    expect(
      decodeTight(r1, fb, { x: 0, y: 0, w: 4, h: 4, encoding: ENC_TIGHT }, inflater, noImage),
    ).toBe(DecoderResult.Done);
    const r2 = feed([0x00, ...compactLen(c2.length), ...c2]);
    expect(
      decodeTight(r2, fb, { x: 0, y: 4, w: 4, h: 4, encoding: ENC_TIGHT }, inflater, noImage),
    ).toBe(DecoderResult.Done);

    for (let i = 0; i < 16; i++) {
      const x = i % 4;
      expect(pixel(fb, x, Math.floor(i / 4))).toEqual([i, i + 40, i + 80, 255]);
      expect(pixel(fb, x, 4 + Math.floor(i / 4))).toEqual([255 - i, 200 - i, 150 - i, 255]);
    }
  });

  it('NeedMore: returns without consuming when the rect is incomplete', () => {
    const fb = newFramebuffer(4, 4);
    const r = feed([0x80, 10, 20]); // Fill needs 4 bytes; only 3 present
    const before = r.available();
    expect(decodeTight(r, fb, rect(2, 2), new TightInflater(), noImage)).toBe(
      DecoderResult.NeedMore,
    );
    expect(r.available()).toBe(before); // nothing consumed
  });
});

// ─── Tight at 16bpp (RGB565 low-color mode) ─────────────────────────────
//
// At 16bpp there is NO 3-byte TPIXEL compression — every Tight pixel (fill
// color, palette entry, full-color payload) is the full 2-byte little-endian
// RGB565 wire pixel. Golden vectors hand-computed via bit replication:
//   0xF800 → [255,0,0]   0x07E0 → [0,255,0]   0x001F → [0,0,255]
//   0xFFFF → [255,255,255]   0x5285 (r5=10,g6=20,b5=5) → [82,81,41]
describe('Tight decoder at 16bpp — 2-byte pixels, byte-exact alignment', () => {
  const LOW = true;

  it('Fill: one 2-byte RGB565 pixel, consumes exactly control+2 bytes', () => {
    const fb = newFramebuffer(4, 4);
    const r = feed([0x80, 0x85, 0x52, 0xab]); // Fill; 0x5285 LE; one trailing byte
    expect(decodeTight(r, fb, rect(2, 2), new TightInflater(), noImage, LOW)).toBe(
      DecoderResult.Done,
    );
    expect(pixel(fb, 0, 0)).toEqual([82, 81, 41, 255]);
    expect(pixel(fb, 1, 1)).toEqual([82, 81, 41, 255]);
    expect(r.available()).toBe(1); // exact consumption
  });

  it('Fill: NeedMore when only 1 of the 2 pixel bytes arrived', () => {
    const fb = newFramebuffer(4, 4);
    const r = feed([0x80, 0x85]);
    const before = r.available();
    expect(decodeTight(r, fb, rect(2, 2), new TightInflater(), noImage, LOW)).toBe(
      DecoderResult.NeedMore,
    );
    expect(r.available()).toBe(before);
  });

  it('Basic Copy, uncompressed (payload < 12 bytes): 2 bytes per pixel', () => {
    const fb = newFramebuffer(4, 4);
    // ctl 0x00 = basic, stream 0, no filter. 1x2 rect = 4 bytes of pixels.
    const r = feed([0x00, 0x00, 0xf8, 0x85, 0x52, 0x99]);
    expect(decodeTight(r, fb, rect(1, 2), new TightInflater(), noImage, LOW)).toBe(
      DecoderResult.Done,
    );
    expect(pixel(fb, 0, 0)).toEqual([255, 0, 0, 255]); // 0xF800
    expect(pixel(fb, 0, 1)).toEqual([82, 81, 41, 255]); // 0x5285
    expect(r.available()).toBe(1);
  });

  it('Basic Copy, zlib-compressed (payload >= 12): 565 pixels through the stream', () => {
    const fb = newFramebuffer(4, 4);
    // 4x4 = 32 bytes uncompressed (>= 12 → compressed path). Pixel i carries
    // the 565 value (i<<11 | i<<5 | i).
    const raw = new Uint8Array(16 * 2);
    for (let i = 0; i < 16; i++) {
      const v = (i << 11) | (i << 5) | i;
      raw[i * 2] = v & 0xff;
      raw[i * 2 + 1] = v >> 8;
    }
    const comp = zlibSync(raw);
    const r = feed([0x00, ...compactLen(comp.length), ...comp]);
    expect(decodeTight(r, fb, rect(4, 4), new TightInflater(), noImage, LOW)).toBe(
      DecoderResult.Done,
    );
    // Hand-computed: i=0 → black; i=8 → r=(8<<3)|2=66, g=8<<2=32, b=66;
    // i=15 → r=(15<<3)|3=123, g=15<<2=60, b=123.
    expect(pixel(fb, 0, 0)).toEqual([0, 0, 0, 255]);
    expect(pixel(fb, 0, 2)).toEqual([66, 32, 66, 255]);
    expect(pixel(fb, 3, 3)).toEqual([123, 60, 123, 255]);
    expect(r.available()).toBe(0);
  });

  it('Basic Palette (2-color, 1 bit/pixel): 2-byte palette entries', () => {
    const fb = newFramebuffer(8, 4);
    // ctl 0x40 = basic + filter-present; filter 1 = palette; numColors-1 = 1.
    // palette: 0xF800 (red), 0x07E0 (green); 8x1 → 1 index byte 0b10101010.
    const r = feed([0x40, 1, 1, 0x00, 0xf8, 0xe0, 0x07, 0b10101010, 0x77]);
    expect(decodeTight(r, fb, rect(8, 1), new TightInflater(), noImage, LOW)).toBe(
      DecoderResult.Done,
    );
    expect(pixel(fb, 0, 0)).toEqual([0, 255, 0, 255]); // bit7 = 1 → palette[1]
    expect(pixel(fb, 1, 0)).toEqual([255, 0, 0, 255]); // bit6 = 0 → palette[0]
    expect(r.available()).toBe(1);
  });

  it('Basic Palette (4-color, 1 byte/pixel index): 2-byte palette entries', () => {
    const fb = newFramebuffer(2, 2);
    // 4 colors → 8 palette bytes; 2x2 indexed rows = 4 bytes, uncompressed.
    const r = feed([0x40, 1, 3, 0x00, 0xf8, 0xe0, 0x07, 0x1f, 0x00, 0xff, 0xff, 0, 1, 2, 3, 0xcd]);
    expect(decodeTight(r, fb, rect(2, 2), new TightInflater(), noImage, LOW)).toBe(
      DecoderResult.Done,
    );
    expect(pixel(fb, 0, 0)).toEqual([255, 0, 0, 255]);
    expect(pixel(fb, 1, 0)).toEqual([0, 255, 0, 255]);
    expect(pixel(fb, 0, 1)).toEqual([0, 0, 255, 255]);
    expect(pixel(fb, 1, 1)).toEqual([255, 255, 255, 255]);
    expect(r.available()).toBe(1);
  });

  it('Gradient filter is refused at 16bpp (cannot occur on AHV; desync-safe)', () => {
    const fb = newFramebuffer(4, 4);
    // ctl 0x40 = basic + filter-present; filter 2 = gradient.
    const r = feed([0x40, 2, 0, 0, 0, 0]);
    expect(() => decodeTight(r, fb, rect(2, 2), new TightInflater(), noImage, LOW)).toThrow(
      /gradient/i,
    );
  });
});

// The JPEG decompression-bomb defense reads intrinsic SOF
// dimensions WITHOUT decoding, so the client can reject a tiny payload that
// declares enormous dimensions before createImageBitmap allocates gigabytes.
describe('readJpegDimensions — SOF dimension probe', () => {
  // Minimal baseline-JPEG byte stream: SOI, an APP0 segment, a SOF0 frame
  // header carrying (height, width), then SOS. Only the SOF needs to be exact.
  function jpegWith(width: number, height: number): Uint8Array {
    const sof = [
      0xff,
      0xc0, // SOF0
      0x00,
      0x11, // segment length (17)
      0x08, // sample precision
      (height >> 8) & 0xff,
      height & 0xff,
      (width >> 8) & 0xff,
      width & 0xff,
      0x03, // 3 components
      0x01,
      0x22,
      0x00,
      0x02,
      0x11,
      0x01,
      0x03,
      0x11,
      0x01,
    ];
    return new Uint8Array([
      0xff,
      0xd8, // SOI
      0xff,
      0xe0,
      0x00,
      0x04,
      0x00,
      0x00, // APP0 (len 4, 2 payload bytes) — must be skipped
      ...sof,
      0xff,
      0xda,
      0x00,
      0x02, // SOS start (we never read past SOF)
    ]);
  }

  it('reads dimensions past a preceding APP0 segment', () => {
    expect(readJpegDimensions(jpegWith(64, 48))).toEqual({ w: 64, h: 48 });
  });

  it('surfaces a bomb SOF (30000×30000) so the caller can reject it', () => {
    // The few-KB payload claims 30000² ≈ 3.6 GB decoded — the caller compares
    // this against the (framebuffer-bounded) rect dims and drops the rect.
    expect(readJpegDimensions(jpegWith(30000, 30000))).toEqual({ w: 30000, h: 30000 });
  });

  it('returns null for a non-JPEG / missing SOI', () => {
    expect(readJpegDimensions(new Uint8Array([0x00, 0x01, 0x02]))).toBeNull();
  });

  it('returns null when the stream ends before any SOF', () => {
    expect(readJpegDimensions(new Uint8Array([0xff, 0xd8]))).toBeNull(); // SOI only
  });
});
