/**
 * StreamReader tests — partial-message buffering for the RFB client.
 *
 * The reader is the foundation of every RFB decoder: every byte we
 * consume from the WebSocket comes through it. A bug here corrupts
 * the entire wire stream silently. Pay particular attention to:
 *
 *   - chunk boundary edge cases (especially HEADER bytes split across
 *     chunks — the nastiest case)
 *   - `available()` invariants — `len` must shrink monotonically as
 *     bytes are consumed
 *   - peek operations must not move the cursor
 *
 * Test data note: we use Uint8Array directly when convenient and
 * ArrayBuffer when verifying the push() overload accepts both.
 */

import { describe, expect, it } from 'vitest';
import { StreamReader } from '../reader';

/** Build a Uint8Array from a varargs list of byte literals. */
function bytes(...vals: number[]): Uint8Array {
  return new Uint8Array(vals);
}

describe('StreamReader', () => {
  describe('basic single-chunk reads', () => {
    it('reads u8 in order', () => {
      const r = new StreamReader();
      r.push(bytes(0x01, 0x02, 0x03));
      expect(r.readU8()).toBe(0x01);
      expect(r.readU8()).toBe(0x02);
      expect(r.readU8()).toBe(0x03);
    });

    it('reads big-endian u16', () => {
      const r = new StreamReader();
      r.push(bytes(0x12, 0x34));
      expect(r.readU16()).toBe(0x1234);
    });

    it('reads big-endian u32', () => {
      const r = new StreamReader();
      r.push(bytes(0x12, 0x34, 0x56, 0x78));
      expect(r.readU32()).toBe(0x12345678);
    });

    it('reads u32 at the unsigned 32-bit max without sign extension', () => {
      const r = new StreamReader();
      r.push(bytes(0xff, 0xff, 0xff, 0xff));
      // Must NOT come back as -1; must be the full 32-bit unsigned max.
      expect(r.readU32()).toBe(0xffffffff);
    });

    it('reads signed i16 (negative values)', () => {
      const r = new StreamReader();
      r.push(bytes(0xff, 0xff));
      expect(r.readI16()).toBe(-1);
    });

    it('reads signed i16 (positive values)', () => {
      const r = new StreamReader();
      r.push(bytes(0x7f, 0xff));
      expect(r.readI16()).toBe(0x7fff);
    });

    it('reads signed i32 (negative values)', () => {
      const r = new StreamReader();
      r.push(bytes(0xff, 0xff, 0xff, 0xfe));
      expect(r.readI32()).toBe(-2);
    });

    it('readBytes returns the requested number of bytes', () => {
      const r = new StreamReader();
      r.push(bytes(1, 2, 3, 4, 5));
      const out = r.readBytes(3);
      expect(out.length).toBe(3);
      expect(Array.from(out)).toEqual([1, 2, 3]);
    });

    it('readBytes returns a COPY (mutating the result does not affect future reads)', () => {
      const r = new StreamReader();
      r.push(bytes(10, 20, 30, 40));
      const a = r.readBytes(2);
      a[0] = 99;
      const b = r.readBytes(2);
      expect(Array.from(b)).toEqual([30, 40]);
    });
  });

  describe('readBytesView (zero-copy view contract)', () => {
    it('returns the requested bytes and advances the cursor like readBytes', () => {
      const r = new StreamReader();
      r.push(bytes(1, 2, 3, 4, 5));
      expect(Array.from(r.readBytesView(3))).toEqual([1, 2, 3]);
      expect(Array.from(r.readBytesView(2))).toEqual([4, 5]);
      expect(r.available()).toBe(0);
    });

    it('ALIASES the pushed buffer when the read fits the head chunk (zero-copy)', () => {
      const r = new StreamReader();
      const src = bytes(10, 20, 30, 40);
      r.push(src);
      const v = r.readBytesView(2);
      // Same backing store, not a copy.
      expect(v.buffer).toBe(src.buffer);
      // Mutation through the source is visible through the view (aliasing),
      // in contrast with readBytes' copy contract above.
      src[0] = 99;
      expect(v[0]).toBe(99);
    });

    it('mutating the view writes through to the backing buffer; later reads are unaffected', () => {
      const r = new StreamReader();
      const src = bytes(10, 20, 30, 40);
      r.push(src);
      const v = r.readBytesView(2);
      v[0] = 77;
      expect(src[0]).toBe(77);
      // The cursor advanced past the view, so subsequent reads see the
      // untouched remainder.
      expect(Array.from(r.readBytes(2))).toEqual([30, 40]);
    });

    it('falls back to a fresh copy when the read spans chunks', () => {
      const r = new StreamReader();
      const c1 = bytes(1, 2);
      r.push(c1);
      r.push(bytes(3, 4));
      const v = r.readBytesView(3);
      expect(Array.from(v)).toEqual([1, 2, 3]);
      expect(v.buffer).not.toBe(c1.buffer);
      expect(r.readU8()).toBe(4);
    });
  });

  describe('availability checks', () => {
    it('available() returns the total pending byte count', () => {
      const r = new StreamReader();
      expect(r.available()).toBe(0);
      r.push(bytes(1, 2, 3));
      expect(r.available()).toBe(3);
      r.readU8();
      expect(r.available()).toBe(2);
    });

    it('has() returns false when there are not enough bytes', () => {
      const r = new StreamReader();
      r.push(bytes(1, 2));
      expect(r.has(3)).toBe(false);
      expect(r.has(2)).toBe(true);
      expect(r.has(1)).toBe(true);
      expect(r.has(0)).toBe(true);
    });

    it('available() decrements monotonically as bytes are consumed across chunks', () => {
      // Regression: the comment in reader.ts:21-26 calls out that `len` must
      // represent pending bytes, not absolute consumption. If anyone "fixes"
      // it to subtract offset, this test will catch the double-counting.
      const r = new StreamReader();
      r.push(bytes(1, 2, 3));
      r.push(bytes(4, 5, 6));
      const seen: number[] = [r.available()];
      for (let i = 0; i < 6; i++) {
        r.readU8();
        seen.push(r.available());
      }
      expect(seen).toEqual([6, 5, 4, 3, 2, 1, 0]);
    });
  });

  describe('multi-chunk reassembly', () => {
    it('joins a u32 split across two chunks (the nastiest case: header bytes)', () => {
      const r = new StreamReader();
      r.push(bytes(0x12, 0x34));
      r.push(bytes(0x56, 0x78));
      expect(r.readU32()).toBe(0x12345678);
    });

    it('joins a u32 split across four 1-byte chunks', () => {
      const r = new StreamReader();
      r.push(bytes(0xaa));
      r.push(bytes(0xbb));
      r.push(bytes(0xcc));
      r.push(bytes(0xdd));
      expect(r.readU32()).toBe(0xaabbccdd);
    });

    it('readBytes reassembles a payload split across many chunks', () => {
      const r = new StreamReader();
      r.push(bytes(0x01, 0x02));
      r.push(bytes(0x03));
      r.push(bytes(0x04, 0x05, 0x06));
      r.push(bytes(0x07, 0x08));
      const out = r.readBytes(8);
      expect(Array.from(out)).toEqual([1, 2, 3, 4, 5, 6, 7, 8]);
    });

    it('handles a long sequence with mixed chunk sizes and primitive reads', () => {
      // Simulates a small RFB FramebufferUpdate header arriving piecemeal:
      // type(1) + pad(1) + nrects(2) + rect{x(2), y(2), w(2), h(2), enc(4)}
      const r = new StreamReader();
      r.push(bytes(0x00)); // type
      r.push(bytes(0x00, 0x00, 0x01)); // pad + first nrects byte starts here
      r.push(bytes(0x00, 0x10, 0x00, 0x20)); // ... rest of header + rect.x + rect.y prefix
      r.push(bytes(0x00, 0x40, 0x00, 0x80, 0xff, 0xff, 0xff, 0xff));
      expect(r.readU8()).toBe(0); // type
      expect(r.readU8()).toBe(0); // pad
      expect(r.readU16()).toBe(1); // nrects
      expect(r.readU16()).toBe(0x0010);
      expect(r.readU16()).toBe(0x0020);
      expect(r.readU16()).toBe(0x0040);
      expect(r.readU16()).toBe(0x0080);
      expect(r.readI32()).toBe(-1);
      expect(r.available()).toBe(0);
    });

    it('multiple complete messages in one push are read serially', () => {
      const r = new StreamReader();
      // Three u32s back-to-back.
      r.push(bytes(0, 0, 0, 1, 0, 0, 0, 2, 0, 0, 0, 3));
      expect(r.readU32()).toBe(1);
      expect(r.readU32()).toBe(2);
      expect(r.readU32()).toBe(3);
    });
  });

  describe('ArrayBuffer / Uint8Array overload', () => {
    it('push() accepts a plain ArrayBuffer', () => {
      const r = new StreamReader();
      const ab = new ArrayBuffer(4);
      new Uint8Array(ab).set([10, 20, 30, 40]);
      r.push(ab);
      expect(r.readU8()).toBe(10);
      expect(Array.from(r.readBytes(3))).toEqual([20, 30, 40]);
    });

    it('push() accepts a Uint8Array view that does not start at offset 0', () => {
      const r = new StreamReader();
      const ab = new ArrayBuffer(8);
      const view = new Uint8Array(ab, 2, 4); // window at offset 2..5
      view.set([0xaa, 0xbb, 0xcc, 0xdd]);
      r.push(view);
      expect(r.readU32()).toBe(0xaabbccdd);
    });
  });

  describe('peek operations', () => {
    it('peekU8 returns the next byte without consuming it', () => {
      const r = new StreamReader();
      r.push(bytes(0x42, 0x43));
      expect(r.peekU8()).toBe(0x42);
      expect(r.peekU8()).toBe(0x42); // still there
      expect(r.available()).toBe(2);
      expect(r.readU8()).toBe(0x42);
    });

    it('peekU8 throws when the reader is empty', () => {
      const r = new StreamReader();
      expect(() => r.peekU8()).toThrow(/empty/);
    });

    it('peekAt reads ahead without consuming', () => {
      const r = new StreamReader();
      r.push(bytes(0x10, 0x20, 0x30, 0x40, 0x50));
      const out = r.peekAt(2, 2);
      expect(Array.from(out)).toEqual([0x30, 0x40]);
      // Cursor untouched.
      expect(r.available()).toBe(5);
      expect(r.readU8()).toBe(0x10);
    });

    it('peekAt spans across chunks', () => {
      const r = new StreamReader();
      r.push(bytes(0x10, 0x20));
      r.push(bytes(0x30, 0x40));
      r.push(bytes(0x50));
      const out = r.peekAt(1, 3);
      expect(Array.from(out)).toEqual([0x20, 0x30, 0x40]);
      expect(r.available()).toBe(5);
    });

    it('peekAt throws when insufficient bytes', () => {
      const r = new StreamReader();
      r.push(bytes(1, 2));
      expect(() => r.peekAt(0, 5)).toThrow(/insufficient/);
      expect(() => r.peekAt(2, 1)).toThrow(/insufficient/);
    });

    it('peekAt returns a fresh copy', () => {
      const r = new StreamReader();
      r.push(bytes(0xaa, 0xbb, 0xcc));
      const out = r.peekAt(0, 3);
      out[0] = 0x00;
      // Reader's buffer is unchanged.
      expect(r.peekU8()).toBe(0xaa);
    });
  });

  describe('error handling', () => {
    it('readU8 throws when no data is available', () => {
      const r = new StreamReader();
      expect(() => r.readU8()).toThrow(/insufficient/);
    });

    it('readBytes throws on insufficient data', () => {
      const r = new StreamReader();
      r.push(bytes(1, 2, 3));
      expect(() => r.readBytes(10)).toThrow(/insufficient/);
    });

    it('readBytes(negative) throws an explicit "invalid length" error', () => {
      // The reader.ts:36-43 docstring calls this out as a desync-bug guard.
      // Validate that the guard is in place.
      const r = new StreamReader();
      r.push(bytes(1, 2));
      expect(() => r.readBytes(-1)).toThrow(/invalid length/);
    });

    it('readBytes(non-integer) throws', () => {
      const r = new StreamReader();
      r.push(bytes(1, 2));
      expect(() => r.readBytes(1.5)).toThrow(/invalid length/);
    });
  });

  describe('zero-byte edge cases', () => {
    it('readBytes(0) returns an empty array without consuming', () => {
      const r = new StreamReader();
      r.push(bytes(1, 2, 3));
      const out = r.readBytes(0);
      expect(out.length).toBe(0);
      expect(r.available()).toBe(3);
    });

    it('push(empty Uint8Array) is a no-op', () => {
      const r = new StreamReader();
      r.push(new Uint8Array(0));
      expect(r.available()).toBe(0);
    });
  });

  describe('chunk-drop invariant', () => {
    it('drops fully-consumed leading chunks (no unbounded growth)', () => {
      const r = new StreamReader();
      // Push and consume 1000 single-byte chunks. If chunks weren't being
      // dropped, the internal array would grow unboundedly. We can't peek
      // at internals, so we exercise the path and verify `available()`
      // stays correct.
      for (let i = 0; i < 1000; i++) {
        r.push(bytes(i & 0xff));
      }
      expect(r.available()).toBe(1000);
      for (let i = 0; i < 1000; i++) {
        expect(r.readU8()).toBe(i & 0xff);
      }
      expect(r.available()).toBe(0);

      // Push again — reader should be reusable cleanly.
      r.push(bytes(0xff));
      expect(r.available()).toBe(1);
      expect(r.readU8()).toBe(0xff);
    });

    it('interleaved push/read pattern (network simulation)', () => {
      // Simulates the realistic case: pushes arrive, consumer reads less
      // than is available each round, so leftover accumulates across
      // chunk boundaries. Verifies bytes come out in FIFO order.
      const r = new StreamReader();
      let pushed = 0;
      let consumed = 0;
      for (let round = 0; round < 100; round++) {
        // Push 5 bytes
        const buf = new Uint8Array(5);
        for (let i = 0; i < 5; i++) buf[i] = (pushed + i) & 0xff;
        r.push(buf);
        pushed += 5;

        // Read 4 bytes. The first byte we read should equal `consumed`,
        // i.e. the FIFO byte we haven't seen yet.
        for (let i = 0; i < 4; i++) {
          expect(r.readU8()).toBe((consumed + i) & 0xff);
        }
        consumed += 4;
      }
      // After 100 rounds we've pushed 500 bytes and consumed 400, so 100 left.
      expect(r.available()).toBe(100);
    });
  });
});
