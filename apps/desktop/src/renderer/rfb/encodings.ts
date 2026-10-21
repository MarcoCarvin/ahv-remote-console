/**
 * RFB pixel-encoding decoders. Operates on the StreamReader and a target
 * ImageData/canvas. Each decoder is a generator-like function: the caller
 * resumes it on every onmessage; if the function returns `false` it needs
 * more bytes.
 *
 * Encodings supported:
 *   - Raw (RFC 6143 §7.7.1)
 *   - CopyRect (§7.7.2)
 *   - Hextile (§7.7.4)
 *   - Tight (§7.7.5) — the preferred content-adaptive encoding (Fill / Basic
 *     with Copy/Palette/Gradient filters + 4 zlib streams / JPEG)
 *   - ZRLE (§7.7.6)
 */

import { Unzlib } from 'fflate';
import type { StreamReader } from './reader';

export interface PixelFormat {
  bitsPerPixel: number;
  depth: number;
  bigEndian: boolean;
  trueColor: boolean;
  redMax: number;
  greenMax: number;
  blueMax: number;
  redShift: number;
  greenShift: number;
  blueShift: number;
}

export interface Framebuffer {
  width: number;
  height: number;
  /** RGBA8888 backing store, in canvas-native byte order. */
  pixels: Uint8ClampedArray;
  /** Cached Uint32Array view over `pixels.buffer` for the bulk blit loops.
   *  Created with `pixels` and discarded with it (only set in newFramebuffer),
   *  so it can never go stale relative to `pixels`. */
  pixels32: Uint32Array;
}

/**
 * Allocate a fresh framebuffer.
 */
export function newFramebuffer(width: number, height: number): Framebuffer {
  const pixels = new Uint8ClampedArray(width * height * 4);
  return { width, height, pixels, pixels32: new Uint32Array(pixels.buffer) };
}

/**
 * Write a tile of BGRX little-endian pixels (Prism's default) into the
 * canvas-native RGBA buffer. Optimised with Uint32Array to reduce memory
 * accesses from 8 per pixel (4 read + 4 write) to 2 (1 read + 1 write).
 */
function blitBGRX(
  fb: Framebuffer,
  x: number,
  y: number,
  w: number,
  h: number,
  src: Uint8Array,
): void {
  const fbWidth = fb.width;
  // Use Uint32Array views for bulk pixel writes when possible.
  // BGRX → RGBA on little-endian (which Electron always is):
  //   src bytes: B G R X  →  dst bytes: R G B 0xFF
  //   src u32:   0xXRGB   →  dst u32:   0xFF_BB_GG_RR (LE memory layout)
  const dst32 = fb.pixels32;
  for (let row = 0; row < h; row++) {
    let dstIdx = (y + row) * fbWidth + x;
    let srcOff = row * w * 4;
    for (let col = 0; col < w; col++) {
      // Read BGRX bytes individually (src may not be 4-byte aligned)
      // and pack into canvas-native RGBA as a single u32 write.
      dst32[dstIdx++] =
        src[srcOff + 2]! | // R
        (src[srcOff + 1]! << 8) | // G
        (src[srcOff]! << 16) | // B
        0xff000000; // A = 255
      srcOff += 4;
    }
  }
}

// ---------- RGB565 (low-color mode) ----------
//
// When the client negotiates the 16bpp RGB565 pixel format (lowColor — the
// collapsed-link demotion lever), every wire pixel is a little-endian u16:
// rrrrrggg gggbbbbb. The framebuffer stays RGBA8888, so the hot decode loops
// convert at blit time through a precomputed 65536-entry table — one index
// per pixel instead of three shift/mask/widen chains. 256 KiB, built lazily
// on the first low-color connect so 32bpp sessions never pay for it.
// Component widening replicates the high bits into the low bits
// ((r5 << 3) | (r5 >> 2)) — the standard expansion that maps 0→0 and max→255.
let lut565: Uint32Array | null = null;

function rgb565Lut(): Uint32Array {
  if (lut565) return lut565;
  const lut = new Uint32Array(65536);
  for (let v = 0; v < 65536; v++) {
    const r5 = (v >> 11) & 0x1f;
    const g6 = (v >> 5) & 0x3f;
    const b5 = v & 0x1f;
    const r = (r5 << 3) | (r5 >> 2);
    const g = (g6 << 2) | (g6 >> 4);
    const b = (b5 << 3) | (b5 >> 2);
    // Canvas-native RGBA packed as a little-endian u32 (0xAABBGGRR memory
    // layout) — same convention as blitBGRX/fillRect.
    lut[v] = r | (g << 8) | (b << 16) | 0xff000000;
  }
  lut565 = lut;
  return lut;
}

/** Expand one little-endian RGB565 wire pixel to an [R, G, B] tuple.
 *  Exported for the client's cursor pseudo-encoding decoder, which reads
 *  pixel-format pixels outside the hot blit loops. */
export function read565Pixel(buf: Uint8Array, off: number): [number, number, number] {
  const px = rgb565Lut()[buf[off]! | (buf[off + 1]! << 8)]!;
  return [px & 0xff, (px >>> 8) & 0xff, (px >>> 16) & 0xff];
}

/** Write a tile of RGB565 little-endian pixels into the RGBA8888 buffer. */
function blit565(
  fb: Framebuffer,
  x: number,
  y: number,
  w: number,
  h: number,
  src: Uint8Array,
): void {
  const lut = rgb565Lut();
  const dst32 = fb.pixels32;
  const fbWidth = fb.width;
  for (let row = 0; row < h; row++) {
    let dstIdx = (y + row) * fbWidth + x;
    let srcOff = row * w * 2;
    for (let col = 0; col < w; col++) {
      dst32[dstIdx++] = lut[src[srcOff]! | (src[srcOff + 1]! << 8)]!;
      srcOff += 2;
    }
  }
}

function fillRect(
  fb: Framebuffer,
  x: number,
  y: number,
  w: number,
  h: number,
  rgb: [number, number, number],
): void {
  const dst32 = fb.pixels32;
  const fbWidth = fb.width;
  // Pack the fill color as a single u32 (little-endian: 0xAABBGGRR).
  const pixel = rgb[0] | (rgb[1] << 8) | (rgb[2] << 16) | 0xff000000;
  for (let row = 0; row < h; row++) {
    const off = (y + row) * fbWidth + x;
    dst32.fill(pixel, off, off + w);
  }
}

function copyRect(
  fb: Framebuffer,
  sx: number,
  sy: number,
  dx: number,
  dy: number,
  w: number,
  h: number,
): void {
  // Worst case is overlapping copies; do a row-aware sweep.
  const stride = fb.width * 4;
  const lineBytes = w * 4;
  if (sy < dy) {
    // Copy bottom-up to avoid overwriting source.
    for (let row = h - 1; row >= 0; row--) {
      const srcOff = (sy + row) * stride + sx * 4;
      const dstOff = (dy + row) * stride + dx * 4;
      fb.pixels.copyWithin(dstOff, srcOff, srcOff + lineBytes);
    }
  } else {
    for (let row = 0; row < h; row++) {
      const srcOff = (sy + row) * stride + sx * 4;
      const dstOff = (dy + row) * stride + dx * 4;
      fb.pixels.copyWithin(dstOff, srcOff, srcOff + lineBytes);
    }
  }
}

function readBGRXPixel(reader: StreamReader): [number, number, number] {
  const b = reader.readU8();
  const g = reader.readU8();
  const r = reader.readU8();
  reader.readU8(); // X / alpha placeholder
  return [r, g, b];
}

// ---------- Encoding decoders ----------

export function decodeRaw(
  reader: StreamReader,
  fb: Framebuffer,
  rect: Rect,
  lowColor = false,
): DecoderResult {
  const bytes = rect.w * rect.h * (lowColor ? 2 : 4);
  if (!reader.has(bytes)) return DecoderResult.NeedMore;
  const raw = reader.readBytesView(bytes);
  if (lowColor) blit565(fb, rect.x, rect.y, rect.w, rect.h, raw);
  else blitBGRX(fb, rect.x, rect.y, rect.w, rect.h, raw);
  return DecoderResult.Done;
}

export function decodeCopyRect(reader: StreamReader, fb: Framebuffer, rect: Rect): DecoderResult {
  if (!reader.has(4)) return DecoderResult.NeedMore;
  const sx = reader.readU16();
  const sy = reader.readU16();
  copyRect(fb, sx, sy, rect.x, rect.y, rect.w, rect.h);
  return DecoderResult.Done;
}

/**
 * Hextile: the rect is divided into 16×16 tiles, each carrying a
 * subEncodingMask byte and the corresponding optional fields.
 *
 * Bits in subEncodingMask:
 *   0x01  Raw
 *   0x02  BackgroundSpecified
 *   0x04  ForegroundSpecified
 *   0x08  AnySubrects
 *   0x10  SubrectsColoured
 */
export function decodeHextile(reader: StreamReader, fb: Framebuffer, rect: Rect): DecoderResult {
  // Resume from saved state inside `rect` if present.
  const state =
    (rect as Rect & { _hex?: HextileState })._hex ??
    ((rect as Rect & { _hex?: HextileState })._hex = {
      bg: [0, 0, 0],
      fg: [0, 0, 0],
      tileX: rect.x,
      tileY: rect.y,
    });

  while (state.tileY < rect.y + rect.h) {
    const tileH = Math.min(16, rect.y + rect.h - state.tileY);
    while (state.tileX < rect.x + rect.w) {
      const tileW = Math.min(16, rect.x + rect.w - state.tileX);
      if (!reader.has(1)) return DecoderResult.NeedMore;
      const mask = reader.peekU8();

      // Raw subencoding short-circuit.
      if ((mask & 0x01) !== 0) {
        const needed = 1 + tileW * tileH * 4;
        if (!reader.has(needed)) return DecoderResult.NeedMore;
        reader.readU8(); // consume mask
        const raw = reader.readBytes(tileW * tileH * 4);
        blitBGRX(fb, state.tileX, state.tileY, tileW, tileH, raw);
        state.tileX += 16;
        continue;
      }

      // Compute payload size to know if we can proceed.
      let needed = 1;
      if ((mask & 0x02) !== 0) needed += 4;
      if ((mask & 0x04) !== 0) needed += 4;
      let nSubrects: number;
      const colouredSubrects = (mask & 0x10) !== 0;
      if ((mask & 0x08) !== 0) {
        if (!reader.has(needed + 1)) return DecoderResult.NeedMore;
        // Peek nSubrects without consuming the header bytes.
        nSubrects = reader.peekAt(needed, 1)[0]!;
        const perSubrect = colouredSubrects ? 6 : 2;
        needed += 1 + nSubrects * perSubrect;
      }
      if (!reader.has(needed)) return DecoderResult.NeedMore;

      // OK to consume now.
      reader.readU8(); // mask
      if ((mask & 0x02) !== 0) state.bg = readBGRXPixel(reader);
      if ((mask & 0x04) !== 0) state.fg = readBGRXPixel(reader);

      // Background fill first.
      fillRect(fb, state.tileX, state.tileY, tileW, tileH, state.bg);

      if ((mask & 0x08) !== 0) {
        const count = reader.readU8();
        for (let i = 0; i < count; i++) {
          let color = state.fg;
          if (colouredSubrects) color = readBGRXPixel(reader);
          const xy = reader.readU8();
          const wh = reader.readU8();
          const sx = (xy >> 4) & 0x0f;
          const sy = xy & 0x0f;
          const sw = ((wh >> 4) & 0x0f) + 1;
          const sh = (wh & 0x0f) + 1;
          fillRect(fb, state.tileX + sx, state.tileY + sy, sw, sh, color);
        }
      }

      state.tileX += 16;
    }
    state.tileY += 16;
    state.tileX = rect.x;
  }
  return DecoderResult.Done;
}

// ---------- ZRLE (RFC 6143 §7.7.6) ─────────────────────────────────────

const ZRLE_TILE = 64;
const CPIXEL_SIZE = 3; // 3-byte compact pixel for depth ≤ 24, shifts multiples of 8

// ZRLE CPIXEL: the 3 least-significant bytes of the negotiated little-endian
// BGRX PIXEL, byte order honored — so [B, G, R]. QEMU's ZRLE path writes these
// via the generic pixel writer, which respects the format's byte order.
function readCPixel(buf: Uint8Array, off: number): [number, number, number] {
  return [buf[off + 2]!, buf[off + 1]!, buf[off]!]; // B,G,R → R,G,B
}

// Tight TPIXEL: looks identical on the wire (3 bytes) but is NOT the same byte
// order as the ZRLE CPIXEL above. QEMU's Tight encoder (tight_pack24) packs the
// 24-bit pixel as red, green, blue extracted via the pixel-format shifts — i.e.
// ALWAYS [R, G, B], independent of the negotiated byte order. Reading it with
// the byte-order convention ([B,G,R]) renders every Tight rect — flat UI and
// JPEG alike — with red and blue swapped. Read straight RGB.
function readTPixel(buf: Uint8Array, off: number): [number, number, number] {
  return [buf[off]!, buf[off + 1]!, buf[off + 2]!]; // R,G,B
}

/**
 * Persistent zlib inflate context for ZRLE. The zlib stream carries
 * state (dictionary) across rects within the same RFB session — a
 * fresh Inflate per rect would produce corrupt output.
 *
 * fflate's Unzlib streams chunks back through an ondata callback
 * synchronously inside push(). A push() typically emits one ondata call
 * with the newly-decoded bytes for that chunk (zero calls when the input
 * is too short to decode anything yet — e.g. a sub-6-byte first chunk
 * that hasn't completed the zlib header), and the internal 32 KiB
 * sliding window keeps the LZ77 dictionary alive across rects.
 */
// InflateBudget bounds the TOTAL decompressed output across every zlib stream
// (the ZRLE inflater plus Tight's four streams) within ONE FramebufferUpdate.
// The per-call MAX_INFLATED_BYTES cap bounds a single inflate(), but a hostile
// upstream can split an 8 MiB frame across many sub-cap rects to drive
// cumulative gigabytes of inflation (zlib's ~1000:1 ratio). The client resets
// this at each FBU header so legitimate full-frame refreshes pass while the
// aggregate bomb is capped. Shared by reference across all inflaters of one
// session.
export class InflateBudget {
  // Two full 8K BGRA frames (≈254 MiB) of headroom per FBU — far above any
  // legitimate single update (one full-screen rect ≈ 127 MiB) yet caps a
  // multi-rect bomb to a survivable transient instead of multi-GiB.
  private static readonly MAX_PER_FBU = 512 * 1024 * 1024;
  private used = 0;
  reset(): void {
    this.used = 0;
  }
  charge(n: number): void {
    this.used += n;
    if (this.used > InflateBudget.MAX_PER_FBU) {
      throw new Error(
        `RFB inflate exceeded ${InflateBudget.MAX_PER_FBU} bytes within one FramebufferUpdate — refusing (possible decompression bomb)`,
      );
    }
  }
}

export class ZRLEInflater {
  private inf = this.createStream();
  private collected: Uint8Array[] = [];

  // Cap on decompressed output per inflate() call. A single full-frame ZRLE
  // rect is bounded by the framebuffer: 8K BGRA = 7680×4320×4 ≈ 127 MiB, so
  // 256 MiB clears the largest realistic frame with slack while still defusing
  // a decompression bomb where a few KiB of attacker-influenced compressed
  // data (the RFB stream is only as trusted as the possibly-MITM'd Prism path)
  // inflates to gigabytes and OOMs the renderer.
  private static readonly MAX_INFLATED_BYTES = 256 * 1024 * 1024;
  private inflatedThisCall = 0;

  // Optional per-FBU budget, shared with sibling streams; charged on every
  // decoded chunk so chained rects can't bypass the per-call cap in aggregate.
  constructor(private readonly budget?: InflateBudget) {}

  private createStream(): Unzlib {
    const stream = new Unzlib();
    stream.ondata = (chunk: Uint8Array) => {
      this.inflatedThisCall += chunk.length;
      if (this.inflatedThisCall > ZRLEInflater.MAX_INFLATED_BYTES) {
        throw new Error(
          `ZRLE decompressed output exceeded ${ZRLEInflater.MAX_INFLATED_BYTES} bytes — refusing (possible decompression bomb)`,
        );
      }
      this.budget?.charge(chunk.length);
      // fflate 0.8.x hands ondata a fresh caller-owned allocation (it copies
      // out of its sliding window before dispatch), so the chunk is safe to
      // keep as-is — no defensive copy needed.
      this.collected.push(chunk);
    };
    return stream;
  }

  inflate(data: Uint8Array): Uint8Array {
    this.collected = [];
    this.inflatedThisCall = 0;
    this.inf.push(data, false);
    if (this.collected.length === 1) return this.collected[0]!;
    const totalLen = this.collected.reduce((sum, c) => sum + c.length, 0);
    const out = new Uint8Array(totalLen);
    let off = 0;
    for (const c of this.collected) {
      out.set(c, off);
      off += c.length;
    }
    return out;
  }

  reset(): void {
    this.inf = this.createStream();
    this.collected = [];
  }
}

export function decodeZRLE(
  reader: StreamReader,
  fb: Framebuffer,
  rect: Rect,
  inflater: ZRLEInflater,
): DecoderResult {
  if (!reader.has(4)) return DecoderResult.NeedMore;
  const lenBytes = reader.peekAt(0, 4);
  const compressedLen =
    (lenBytes[0]! * 0x1000000 + (lenBytes[1]! << 16) + (lenBytes[2]! << 8) + lenBytes[3]!) >>> 0;
  if (!reader.has(4 + compressedLen)) return DecoderResult.NeedMore;
  reader.readBytesView(4); // consume length prefix
  const compressed = reader.readBytesView(compressedLen);
  const data = inflater.inflate(compressed);

  let off = 0;

  function u8(): number {
    return data[off++]!;
  }

  function cpixel(): [number, number, number] {
    const c = readCPixel(data, off);
    off += CPIXEL_SIZE;
    return c;
  }

  function rleLength(): number {
    let len = 1;
    let b: number;
    do {
      b = u8();
      len += b;
    } while (b === 255);
    return len;
  }

  for (let tileY = rect.y; tileY < rect.y + rect.h; tileY += ZRLE_TILE) {
    const tileH = Math.min(ZRLE_TILE, rect.y + rect.h - tileY);
    for (let tileX = rect.x; tileX < rect.x + rect.w; tileX += ZRLE_TILE) {
      const tileW = Math.min(ZRLE_TILE, rect.x + rect.w - tileX);
      const subenc = u8();

      if (subenc === 0) {
        // Raw CPIXEL data
        for (let row = 0; row < tileH; row++) {
          for (let col = 0; col < tileW; col++) {
            const rgb = cpixel();
            const dst = fb.pixels;
            const idx = ((tileY + row) * fb.width + (tileX + col)) * 4;
            dst[idx] = rgb[0];
            dst[idx + 1] = rgb[1];
            dst[idx + 2] = rgb[2];
            dst[idx + 3] = 255;
          }
        }
      } else if (subenc === 1) {
        // Solid
        const rgb = cpixel();
        fillRect(fb, tileX, tileY, tileW, tileH, rgb);
      } else if (subenc >= 2 && subenc <= 16) {
        // Packed palette
        const paletteSize = subenc;
        const palette: [number, number, number][] = [];
        for (let i = 0; i < paletteSize; i++) palette.push(cpixel());
        const bitsPerIndex = paletteSize <= 2 ? 1 : paletteSize <= 4 ? 2 : 4;
        const mask = (1 << bitsPerIndex) - 1;
        for (let row = 0; row < tileH; row++) {
          let byte = 0;
          let bitsLeft = 0;
          for (let col = 0; col < tileW; col++) {
            if (bitsLeft === 0) {
              byte = u8();
              bitsLeft = 8;
            }
            bitsLeft -= bitsPerIndex;
            const idx = (byte >> bitsLeft) & mask;
            const rgb = palette[idx]!;
            const dstIdx = ((tileY + row) * fb.width + (tileX + col)) * 4;
            fb.pixels[dstIdx] = rgb[0];
            fb.pixels[dstIdx + 1] = rgb[1];
            fb.pixels[dstIdx + 2] = rgb[2];
            fb.pixels[dstIdx + 3] = 255;
          }
        }
      } else if (subenc === 128) {
        // Plain RLE
        let pixels = tileW * tileH;
        let px = 0;
        while (pixels > 0) {
          const rgb = cpixel();
          const runLen = rleLength();
          for (let i = 0; i < runLen; i++) {
            const row = Math.floor(px / tileW);
            const col = px % tileW;
            const dstIdx = ((tileY + row) * fb.width + (tileX + col)) * 4;
            fb.pixels[dstIdx] = rgb[0];
            fb.pixels[dstIdx + 1] = rgb[1];
            fb.pixels[dstIdx + 2] = rgb[2];
            fb.pixels[dstIdx + 3] = 255;
            px++;
          }
          pixels -= runLen;
        }
      } else if (subenc >= 130) {
        // Palette RLE
        const paletteSize = subenc - 128;
        const palette: [number, number, number][] = [];
        for (let i = 0; i < paletteSize; i++) palette.push(cpixel());
        let pixels = tileW * tileH;
        let px = 0;
        while (pixels > 0) {
          const indexByte = u8();
          const palIdx = indexByte & 0x7f;
          const rgb = palette[palIdx]!;
          let runLen: number;
          if ((indexByte & 0x80) !== 0) {
            runLen = rleLength();
          } else {
            runLen = 1;
          }
          for (let i = 0; i < runLen; i++) {
            const row = Math.floor(px / tileW);
            const col = px % tileW;
            const dstIdx = ((tileY + row) * fb.width + (tileX + col)) * 4;
            fb.pixels[dstIdx] = rgb[0];
            fb.pixels[dstIdx + 1] = rgb[1];
            fb.pixels[dstIdx + 2] = rgb[2];
            fb.pixels[dstIdx + 3] = 255;
            px++;
          }
          pixels -= runLen;
        }
      } else {
        throw new Error(`ZRLE: unsupported subencoding ${subenc}`);
      }
    }
  }

  return DecoderResult.Done;
}

// ---------- Tight (RFC 6143 §7.7.5 / TightVNC) ----------
//
// Tight is the content-adaptive encoding production VNC clients (noVNC, the AHV
// web console) use: solid -> Fill, low-color UI -> indexed palette + zlib,
// photographic/video -> JPEG. It replaces uncompressed Raw and is what makes
// the same QEMU backend feel "instant" instead of laggy.
//
// We decode ALL-OR-NOTHING per rect: peek the entire header (control byte,
// optional filter, palette, compact length) to compute the exact total byte
// count, and only consume + decode once the whole rect is buffered. A partial
// read is therefore impossible, which structurally rules out the stream
// misalignment that disabled ZRLE/Hextile.

const TIGHT_MIN_TO_COMPRESS = 12; // payloads smaller than this are sent uncompressed
const TIGHT_FILTER_COPY = 0;
const TIGHT_FILTER_PALETTE = 1;
const TIGHT_FILTER_GRADIENT = 2;

// TightInflater owns Tight's four independent zlib streams (each keeps its LZ77
// dictionary across rects; the control byte's low 4 bits reset specific streams).
export class TightInflater {
  private streams: ZRLEInflater[];
  constructor(budget?: InflateBudget) {
    // All four streams share the session's per-FBU budget so their combined
    // output is bounded across a single FramebufferUpdate.
    this.streams = [
      new ZRLEInflater(budget),
      new ZRLEInflater(budget),
      new ZRLEInflater(budget),
      new ZRLEInflater(budget),
    ];
  }
  applyResets(ctl: number): void {
    for (let i = 0; i < 4; i++) {
      if ((ctl >> i) & 1) this.streams[i]!.reset();
    }
  }
  inflate(streamId: number, data: Uint8Array): Uint8Array {
    return this.streams[streamId]!.inflate(data);
  }
  reset(): void {
    for (const s of this.streams) s.reset();
  }
}

// A JPEG rect is decoded asynchronously by the browser; the decoder consumes
// the JPEG bytes (keeping the stream aligned) and hands them to this callback,
// which blits the decoded image into the framebuffer and requests a repaint.
// `jpeg` is a transient view into the receive buffer (readBytesView): the
// handler must copy synchronously (the client's Blob construction does)
// before any async use.
export type TightImageHandler = (rect: Rect, jpeg: Uint8Array) => void;

// peekCompactLen reads Tight's 1-3 byte compact length at `skip` WITHOUT
// consuming, returning {len, bytesRead} or null if not enough is buffered.
function peekCompactLen(
  reader: StreamReader,
  skip: number,
): { len: number; bytesRead: number } | null {
  if (!reader.has(skip + 1)) return null;
  let b = reader.peekAt(skip, 1)[0]!;
  let len = b & 0x7f;
  if ((b & 0x80) === 0) return { len, bytesRead: 1 };
  if (!reader.has(skip + 2)) return null;
  b = reader.peekAt(skip + 1, 1)[0]!;
  len |= (b & 0x7f) << 7;
  if ((b & 0x80) === 0) return { len, bytesRead: 2 };
  if (!reader.has(skip + 3)) return null;
  b = reader.peekAt(skip + 2, 1)[0]!;
  len |= b << 14;
  return { len, bytesRead: 3 };
}

export function decodeTight(
  reader: StreamReader,
  fb: Framebuffer,
  rect: Rect,
  inflater: TightInflater,
  onImage: TightImageHandler,
  lowColor = false,
): DecoderResult {
  if (!reader.has(1)) return DecoderResult.NeedMore;
  const ctl = reader.peekU8();
  const mode = ctl >> 4;

  // Tight pixel size: the 3-byte TPIXEL compression exists ONLY for the
  // 32bpp depth-24 format — at 16bpp every Tight pixel (fill color, palette
  // entry, full-color payload) is the full 2-byte RGB565 wire pixel.
  const pixSize = lowColor ? 2 : CPIXEL_SIZE;

  // Fill: control byte + one pixel.
  if (mode === 0x08) {
    const total = 1 + pixSize;
    if (!reader.has(total)) return DecoderResult.NeedMore;
    reader.readU8();
    inflater.applyResets(ctl);
    const pbytes = reader.readBytes(pixSize);
    fillRect(
      fb,
      rect.x,
      rect.y,
      rect.w,
      rect.h,
      lowColor ? read565Pixel(pbytes, 0) : readTPixel(pbytes, 0),
    );
    return DecoderResult.Done;
  }

  // JPEG: control byte + compact-length + JPEG bytes (decoded async by client).
  if (mode === 0x09) {
    const lr = peekCompactLen(reader, 1);
    if (lr === null) return DecoderResult.NeedMore;
    const total = 1 + lr.bytesRead + lr.len;
    if (!reader.has(total)) return DecoderResult.NeedMore;
    reader.readU8();
    inflater.applyResets(ctl);
    reader.readBytes(lr.bytesRead);
    onImage(rect, reader.readBytesView(lr.len));
    return DecoderResult.Done;
  }

  // Basic compression (mode bit 3 == 0): stream id + optional filter.
  if ((mode & 0x08) === 0) {
    const streamId = mode & 0x03;
    const filterPresent = (mode & 0x04) !== 0;
    let hdr = 1; // control byte
    let filter = TIGHT_FILTER_COPY;
    if (filterPresent) {
      if (!reader.has(2)) return DecoderResult.NeedMore;
      filter = reader.peekAt(1, 1)[0]!;
      hdr = 2;
    }
    const w = rect.w;
    const h = rect.h;

    // Palette: count byte + numColors raw pixels precede the indexed payload.
    let numColors = 0;
    if (filter === TIGHT_FILTER_PALETTE) {
      if (!reader.has(hdr + 1)) return DecoderResult.NeedMore;
      numColors = reader.peekAt(hdr, 1)[0]! + 1;
    }
    const rawHdr = hdr + (filter === TIGHT_FILTER_PALETTE ? 1 + numColors * pixSize : 0);
    if (!reader.has(rawHdr)) return DecoderResult.NeedMore;

    let uncompressedSize: number;
    if (filter === TIGHT_FILTER_PALETTE) {
      const rowSize = numColors <= 2 ? Math.ceil(w / 8) : w;
      uncompressedSize = rowSize * h;
    } else if (filter === TIGHT_FILTER_COPY || filter === TIGHT_FILTER_GRADIENT) {
      if (filter === TIGHT_FILTER_GRADIENT && lowColor) {
        // Gradient predicts per 8-bit component on the depth-24 TPIXEL
        // layout; AHV's QEMU never emits it (it's gated with the lossy
        // paths that are compiled out), and a 565 gradient would need its
        // own component-width prediction. Refuse → protocolReset realigns.
        throw new Error('Tight: gradient filter unsupported at 16bpp');
      }
      uncompressedSize = w * h * pixSize;
    } else {
      throw new Error(`Tight: illegal filter ${filter}`);
    }

    let compressed: boolean;
    let lenPrefix = 0;
    let dataLen: number;
    if (uncompressedSize < TIGHT_MIN_TO_COMPRESS) {
      compressed = false;
      dataLen = uncompressedSize;
      if (!reader.has(rawHdr + dataLen)) return DecoderResult.NeedMore;
    } else {
      const lr = peekCompactLen(reader, rawHdr);
      if (lr === null) return DecoderResult.NeedMore;
      compressed = true;
      lenPrefix = lr.bytesRead;
      dataLen = lr.len;
      if (!reader.has(rawHdr + lenPrefix + dataLen)) return DecoderResult.NeedMore;
    }

    // Fully buffered — consume in exact order.
    reader.readU8(); // control byte
    inflater.applyResets(ctl);
    if (filterPresent) reader.readU8(); // filter byte
    const palette: [number, number, number][] = [];
    if (filter === TIGHT_FILTER_PALETTE) {
      reader.readU8(); // numColors - 1
      const pbytes = reader.readBytesView(numColors * pixSize);
      for (let i = 0; i < numColors; i++) {
        palette.push(
          lowColor ? read565Pixel(pbytes, i * pixSize) : readTPixel(pbytes, i * pixSize),
        );
      }
    }
    let data: Uint8Array;
    if (compressed) {
      reader.readBytes(lenPrefix);
      data = inflater.inflate(streamId, reader.readBytesView(dataLen));
    } else {
      data = reader.readBytesView(dataLen);
    }

    if (filter === TIGHT_FILTER_COPY) {
      if (lowColor) tightCopy565(fb, rect, data);
      else tightCopy(fb, rect, data);
    } else if (filter === TIGHT_FILTER_PALETTE) {
      tightPalette(fb, rect, data, palette, numColors);
    } else {
      tightGradient(fb, rect, data);
    }
    return DecoderResult.Done;
  }

  // 0x0A = tight-png (never advertised) or an illegal mode.
  throw new Error(
    `Tight: unsupported compression mode 0x${mode.toString(16)} (ctl 0x${ctl.toString(16)})`,
  );
}

function tightCopy(fb: Framebuffer, rect: Rect, data: Uint8Array): void {
  const dst32 = fb.pixels32;
  const fbWidth = fb.width;
  let si = 0;
  for (let row = 0; row < rect.h; row++) {
    let di = (rect.y + row) * fbWidth + rect.x;
    for (let col = 0; col < rect.w; col++) {
      // Tight TPIXEL is [R,G,B]. Pack directly to canvas-native RGBA u32.
      dst32[di++] = data[si]! | (data[si + 1]! << 8) | (data[si + 2]! << 16) | 0xff000000;
      si += 3;
    }
  }
}

// Full-color (copy filter) at 16bpp: 2-byte little-endian RGB565 pixels,
// widened through the 565 lookup — one table index per pixel in the hot loop.
function tightCopy565(fb: Framebuffer, rect: Rect, data: Uint8Array): void {
  const lut = rgb565Lut();
  const dst32 = fb.pixels32;
  const fbWidth = fb.width;
  let si = 0;
  for (let row = 0; row < rect.h; row++) {
    let di = (rect.y + row) * fbWidth + rect.x;
    for (let col = 0; col < rect.w; col++) {
      dst32[di++] = lut[data[si]! | (data[si + 1]! << 8)]!;
      si += 2;
    }
  }
}

function tightPalette(
  fb: Framebuffer,
  rect: Rect,
  data: Uint8Array,
  palette: [number, number, number][],
  numColors: number,
): void {
  const dst32 = fb.pixels32;
  const fbWidth = fb.width;
  // Pre-pack palette as u32 values — avoids per-pixel tuple lookup + 4 byte writes.
  const pal32 = new Uint32Array(numColors);
  for (let i = 0; i < numColors; i++) {
    const c = palette[i] ?? [0, 0, 0];
    pal32[i] = c[0] | (c[1] << 8) | (c[2] << 16) | 0xff000000;
  }
  if (numColors <= 2) {
    // 1 bit per pixel, MSB-first, rows padded to a byte boundary.
    const rowBytes = Math.ceil(rect.w / 8);
    for (let row = 0; row < rect.h; row++) {
      let di = (rect.y + row) * fbWidth + rect.x;
      const rowOff = row * rowBytes;
      for (let col = 0; col < rect.w; col++) {
        const bit = (data[rowOff + (col >> 3)]! >> (7 - (col & 7))) & 1;
        dst32[di++] = pal32[bit]!;
      }
    }
  } else {
    // 1 byte per pixel index.
    let si = 0;
    for (let row = 0; row < rect.h; row++) {
      let di = (rect.y + row) * fbWidth + rect.x;
      for (let col = 0; col < rect.w; col++) {
        dst32[di++] = pal32[data[si++]!]!;
      }
    }
  }
}

// Gradient filter: PNG-style prediction on the raw TPIXEL bytes ([R,G,B] —
// QEMU's Tight 24-bit pixel order; see readTPixel). prediction = clamp(left +
// up - upperLeft), plus the transmitted per-byte delta (mod 256).
function tightGradient(fb: Framebuffer, rect: Rect, data: Uint8Array): void {
  const dst32 = fb.pixels32;
  const w = rect.w;
  const fbWidth = fb.width;
  const prev = new Int32Array(w * 3); // row above, [R,G,B] per pixel
  const cur = new Int32Array(w * 3);
  let si = 0;
  for (let row = 0; row < rect.h; row++) {
    let di = (rect.y + row) * fbWidth + rect.x;
    for (let col = 0; col < w; col++) {
      for (let c = 0; c < 3; c++) {
        const left = col > 0 ? cur[(col - 1) * 3 + c]! : 0;
        const up = prev[col * 3 + c]!;
        const upLeft = col > 0 ? prev[(col - 1) * 3 + c]! : 0;
        let pred = left + up - upLeft;
        if (pred < 0) pred = 0;
        else if (pred > 255) pred = 255;
        cur[col * 3 + c] = (pred + data[si++]!) & 0xff;
      }
      dst32[di++] =
        cur[col * 3]! | (cur[col * 3 + 1]! << 8) | (cur[col * 3 + 2]! << 16) | 0xff000000;
    }
    prev.set(cur);
  }
}

// ---------- Types ----------

export enum DecoderResult {
  Done,
  NeedMore,
}

export interface Rect {
  x: number;
  y: number;
  w: number;
  h: number;
  encoding: number;
}

interface HextileState {
  bg: [number, number, number];
  fg: [number, number, number];
  tileX: number;
  tileY: number;
}
