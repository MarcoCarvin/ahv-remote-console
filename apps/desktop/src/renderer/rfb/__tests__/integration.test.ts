/**
 * End-to-end RFB integration tests.
 *
 * These exercise the protocol loop against a fake WebSocket from
 * handshake → FramebufferUpdate → onUpdate fan-out → pixel inspection.
 *
 * The goal is to catch wiring-layer regressions that the unit tests
 * for {client, encodings, reader} can't see individually:
 *
 *   - Cross-rect state in a multi-rect FBU (Hextile state persisted
 *     correctly between calls)
 *   - Per-rect FramebufferUpdate subscribers fire in order
 *   - Cursor pseudo-encoding round-trips and exposes a CursorShape
 *   - DesktopSize / ExtendedDesktopSize resize the framebuffer
 *   - The flushUpdate request pump fires an incremental update after
 *     a complete FBU
 */

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { RFBClient } from '../client';
import {
  CLI_MSG_FRAMEBUFFER_UPDATE_REQUEST,
  SRV_MSG_FRAMEBUFFER_UPDATE,
  SRV_MSG_BELL,
  SRV_MSG_SERVER_CUT_TEXT,
  ENC_RAW,
  ENC_COPY_RECT,
  ENC_DESKTOP_SIZE,
  ENC_EXTENDED_DESKTOP_SIZE,
  ENC_CURSOR,
} from '../protocol';

// ─── FakeWebSocket (reuses pattern from client.test.ts) ────────────────

class FakeWebSocket {
  static readonly CONNECTING = 0;
  static readonly OPEN = 1;
  static readonly CLOSING = 2;
  static readonly CLOSED = 3;

  binaryType = 'arraybuffer';
  readyState = 1;
  bufferedAmount = 0;
  readonly sent: Uint8Array[] = [];

  private readonly listeners: { type: string; handler: (e: unknown) => void }[] = [];

  addEventListener(type: string, handler: (e: unknown) => void): void {
    this.listeners.push({ type, handler });
  }
  removeEventListener(type: string, handler: (e: unknown) => void): void {
    const i = this.listeners.findIndex((l) => l.type === type && l.handler === handler);
    if (i >= 0) this.listeners.splice(i, 1);
  }
  send(buf: ArrayBuffer | Uint8Array): void {
    const v = buf instanceof Uint8Array ? new Uint8Array(buf) : new Uint8Array(buf);
    this.sent.push(v);
  }
  close(): void {
    this.readyState = FakeWebSocket.CLOSED;
  }
  feed(bytes: Uint8Array): void {
    const data = bytes.buffer.slice(bytes.byteOffset, bytes.byteOffset + bytes.byteLength);
    for (const l of this.listeners) if (l.type === 'message') l.handler({ data });
  }
}

beforeEach(() => {
  (globalThis as { WebSocket?: unknown }).WebSocket = FakeWebSocket;
});
afterEach(() => {
  delete (globalThis as { WebSocket?: unknown }).WebSocket;
});

// ─── Wire-format helpers ───────────────────────────────────────────────

function serverVersion(): Uint8Array {
  return new TextEncoder().encode('RFB 003.008\n');
}
function secList(): Uint8Array {
  return new Uint8Array([1, 1]);
}
function secOK(): Uint8Array {
  return new Uint8Array([0, 0, 0, 0]);
}
function serverInit(w: number, h: number): Uint8Array {
  const name = new TextEncoder().encode('VM');
  const buf = new Uint8Array(24 + name.length);
  new DataView(buf.buffer).setUint16(0, w);
  new DataView(buf.buffer).setUint16(2, h);
  new DataView(buf.buffer).setUint32(20, name.length);
  buf.set(name, 24);
  return buf;
}

function handshake(ws: FakeWebSocket, w = 64, h = 32): void {
  ws.feed(serverVersion());
  ws.feed(secList());
  ws.feed(secOK());
  ws.feed(serverInit(w, h));
}

/** Build a FramebufferUpdate header: msgtype(1) + pad(1) + nrects(u16 BE). */
function fbuHeader(nrects: number): Uint8Array {
  return new Uint8Array([SRV_MSG_FRAMEBUFFER_UPDATE, 0, (nrects >> 8) & 0xff, nrects & 0xff]);
}

/** Build a rect header: x, y, w, h (u16 BE), encoding (s32 BE). */
function rectHeader(x: number, y: number, w: number, h: number, enc: number): Uint8Array {
  const buf = new Uint8Array(12);
  const dv = new DataView(buf.buffer);
  dv.setUint16(0, x);
  dv.setUint16(2, y);
  dv.setUint16(4, w);
  dv.setUint16(6, h);
  dv.setInt32(8, enc);
  return buf;
}

/** Build a Raw rect's payload (BGRX little-endian pixels). */
function rawPayload(
  w: number,
  h: number,
  fn: (x: number, y: number) => [number, number, number],
): Uint8Array {
  const buf = new Uint8Array(w * h * 4);
  for (let y = 0; y < h; y++) {
    for (let x = 0; x < w; x++) {
      const [r, g, b] = fn(x, y);
      const o = (y * w + x) * 4;
      buf[o] = b;
      buf[o + 1] = g;
      buf[o + 2] = r;
      buf[o + 3] = 0;
    }
  }
  return buf;
}

function concat(...parts: Uint8Array[]): Uint8Array {
  const total = parts.reduce((s, p) => s + p.length, 0);
  const out = new Uint8Array(total);
  let off = 0;
  for (const p of parts) {
    out.set(p, off);
    off += p.length;
  }
  return out;
}

// ─── Suite ─────────────────────────────────────────────────────────────

describe('RFBClient integration', () => {
  it('handshake → 1-rect Raw FBU → onUpdate fires with the rect and pixels are painted', () => {
    const ws = new FakeWebSocket();
    const onUpdate = vi.fn();
    const client = new RFBClient({
      ws: ws as unknown as WebSocket,
      events: { onUpdate },
    });
    handshake(ws, 8, 8);

    // Server sends a FBU: 1 rect at (0,0) 4×4 Raw, all red.
    const fbu = concat(
      fbuHeader(1),
      rectHeader(0, 0, 4, 4, ENC_RAW),
      rawPayload(4, 4, () => [200, 50, 25]),
    );
    ws.feed(fbu);

    expect(onUpdate).toHaveBeenCalledTimes(1);
    const rects = onUpdate.mock.calls[0]![0] as {
      x: number;
      y: number;
      w: number;
      h: number;
      encoding: number;
    }[];
    expect(rects.length).toBe(1);
    expect(rects[0]).toMatchObject({ x: 0, y: 0, w: 4, h: 4, encoding: ENC_RAW });

    const fb = client.framebuffer()!;
    expect(fb.pixels[0]).toBe(200);
    expect(fb.pixels[1]).toBe(50);
    expect(fb.pixels[2]).toBe(25);
    expect(fb.pixels[3]).toBe(255);
  });

  it('readRGB returns the painted pixels in RGB row-major order', () => {
    const ws = new FakeWebSocket();
    const client = new RFBClient({ ws: ws as unknown as WebSocket, events: {} });
    handshake(ws, 4, 4);
    ws.feed(
      concat(
        fbuHeader(1),
        rectHeader(0, 0, 4, 4, ENC_RAW),
        rawPayload(4, 4, () => [10, 20, 30]),
      ),
    );
    const out = client.readRGB(0, 0, 4, 4);
    expect(out.length).toBe(4 * 4 * 3);
    for (let i = 0; i < 16; i++) {
      expect(out[i * 3]).toBe(10);
      expect(out[i * 3 + 1]).toBe(20);
      expect(out[i * 3 + 2]).toBe(30);
    }
  });

  it('readRGB clamps out-of-range coordinates rather than reading OOB', () => {
    const ws = new FakeWebSocket();
    const client = new RFBClient({ ws: ws as unknown as WebSocket, events: {} });
    handshake(ws, 4, 4);
    // No pixels painted yet, but read out-of-range — should clamp.
    const tooBig = client.readRGB(0, 0, 100, 100);
    expect(tooBig.length).toBe(4 * 4 * 3); // clamped to FB bounds
    // Negative origin clamps to 0 too.
    const negOrigin = client.readRGB(-5, -5, 4, 4);
    expect(negOrigin.length).toBe(4 * 4 * 3);
  });

  it('multi-rect FBU: all rects landed and onUpdate fires once with all of them', () => {
    const ws = new FakeWebSocket();
    const onUpdate = vi.fn();
    const client = new RFBClient({
      ws: ws as unknown as WebSocket,
      events: { onUpdate },
    });
    handshake(ws, 8, 8);
    const fbu = concat(
      fbuHeader(2),
      rectHeader(0, 0, 4, 4, ENC_RAW),
      rawPayload(4, 4, () => [100, 0, 0]),
      rectHeader(4, 4, 4, 4, ENC_RAW),
      rawPayload(4, 4, () => [0, 100, 0]),
    );
    ws.feed(fbu);
    expect(onUpdate).toHaveBeenCalledTimes(1);
    const rects = onUpdate.mock.calls[0]![0] as { encoding: number }[];
    expect(rects.length).toBe(2);
    const fb = client.framebuffer()!;
    // Top-left rect: red. Bottom-right: green.
    const tl = (y: number, x: number) => fb.pixels[(y * 8 + x) * 4]!;
    expect(tl(0, 0)).toBe(100);
    expect(fb.pixels[(4 * 8 + 4) * 4 + 1]).toBe(100); // green at (4,4)
  });

  it('per-rect subscribers fire in encounter order and survive subscriber crashes', () => {
    const ws = new FakeWebSocket();
    const client = new RFBClient({ ws: ws as unknown as WebSocket, events: {} });
    handshake(ws, 8, 8);

    const seen: number[] = [];
    const crashy = vi.fn(() => {
      throw new Error('subscriber crash — protocol must survive');
    });
    const unsub = client.subscribeFramebufferUpdate((rect) => {
      seen.push(rect.x);
    });
    client.subscribeFramebufferUpdate(crashy);

    const fbu = concat(
      fbuHeader(2),
      rectHeader(0, 0, 4, 4, ENC_RAW),
      rawPayload(4, 4, () => [10, 10, 10]),
      rectHeader(4, 0, 4, 4, ENC_RAW),
      rawPayload(4, 4, () => [20, 20, 20]),
    );
    ws.feed(fbu);

    expect(seen).toEqual([0, 4]);
    expect(crashy).toHaveBeenCalledTimes(2);

    // Unsubscribe works.
    unsub();
    ws.feed(
      concat(
        fbuHeader(1),
        rectHeader(0, 0, 4, 4, ENC_RAW),
        rawPayload(4, 4, () => [30, 30, 30]),
      ),
    );
    expect(seen).toEqual([0, 4]); // unchanged
  });

  it('FBU split across many small WebSocket chunks reassembles correctly', () => {
    const ws = new FakeWebSocket();
    const onUpdate = vi.fn();
    const client = new RFBClient({
      ws: ws as unknown as WebSocket,
      events: { onUpdate },
    });
    handshake(ws, 4, 4);
    const fbu = concat(
      fbuHeader(1),
      rectHeader(0, 0, 4, 4, ENC_RAW),
      rawPayload(4, 4, (x, y) => [(x + y * 4) * 16, 0, 0]),
    );
    // Feed the entire FBU one byte at a time.
    for (let i = 0; i < fbu.length; i++) {
      ws.feed(fbu.subarray(i, i + 1));
    }
    expect(onUpdate).toHaveBeenCalledTimes(1);
    const fb = client.framebuffer()!;
    expect(fb.pixels[0]).toBe(0);
    expect(fb.pixels[4]).toBe(16);
    expect(fb.pixels[(3 * 4 + 3) * 4]).toBe(15 * 16);
  });

  it('flushUpdate pump: client emits incremental FBU request after each complete FBU', () => {
    const ws = new FakeWebSocket();
    const client = new RFBClient({ ws: ws as unknown as WebSocket, events: {} });
    handshake(ws, 4, 4);
    // Count CLI_MSG_FRAMEBUFFER_UPDATE_REQUEST messages on the wire.
    const beforeFBUs = ws.sent.filter((b) => b[0] === CLI_MSG_FRAMEBUFFER_UPDATE_REQUEST).length;
    expect(beforeFBUs).toBeGreaterThanOrEqual(1); // bootstrap non-incremental
    // Feed a complete FBU.
    ws.feed(
      concat(
        fbuHeader(1),
        rectHeader(0, 0, 4, 4, ENC_RAW),
        rawPayload(4, 4, () => [0, 0, 0]),
      ),
    );
    const afterFBUs = ws.sent.filter((b) => b[0] === CLI_MSG_FRAMEBUFFER_UPDATE_REQUEST).length;
    expect(afterFBUs).toBe(beforeFBUs + 1);
    // The newest one should be incremental (byte 1 == 1).
    const last = ws.sent.filter((b) => b[0] === CLI_MSG_FRAMEBUFFER_UPDATE_REQUEST).pop()!;
    expect(last[1]).toBe(1);
    void client;
  });

  it('Bell message fires onBell', () => {
    const ws = new FakeWebSocket();
    const onBell = vi.fn();
    const client = new RFBClient({ ws: ws as unknown as WebSocket, events: { onBell } });
    handshake(ws);
    ws.feed(new Uint8Array([SRV_MSG_BELL]));
    expect(onBell).toHaveBeenCalledTimes(1);
    void client;
  });

  it('ServerCutText (plain-text, positive length) fires onClipboard with the decoded string', () => {
    const ws = new FakeWebSocket();
    const log = vi.spyOn(console, 'log').mockImplementation(() => {
      /* silence */
    });
    const onClipboard = vi.fn();
    const client = new RFBClient({
      ws: ws as unknown as WebSocket,
      events: { onClipboard },
    });
    handshake(ws);
    // ServerCutText: type=3, 3 pad bytes, u32 length, N bytes
    const payload = new TextEncoder().encode('hello world');
    const msg = new Uint8Array(8 + payload.length);
    msg[0] = SRV_MSG_SERVER_CUT_TEXT;
    new DataView(msg.buffer).setUint32(4, payload.length);
    msg.set(payload, 8);
    ws.feed(msg);
    expect(onClipboard).toHaveBeenCalledWith('hello world');
    void client;
    log.mockRestore();
  });

  it('CopyRect end-to-end: paint a source block, then copy via the wire-side decoder', () => {
    const ws = new FakeWebSocket();
    const client = new RFBClient({ ws: ws as unknown as WebSocket, events: {} });
    handshake(ws, 16, 16);
    // First FBU: Raw red 2×2 at (0, 0).
    ws.feed(
      concat(
        fbuHeader(1),
        rectHeader(0, 0, 2, 2, ENC_RAW),
        rawPayload(2, 2, () => [255, 0, 0]),
      ),
    );
    // Second FBU: CopyRect (0, 0) → (8, 8), size 2×2.
    const cr = new Uint8Array(4);
    new DataView(cr.buffer).setUint16(0, 0);
    new DataView(cr.buffer).setUint16(2, 0);
    ws.feed(concat(fbuHeader(1), rectHeader(8, 8, 2, 2, ENC_COPY_RECT), cr));
    const fb = client.framebuffer()!;
    const at = (x: number, y: number, c: number) => fb.pixels[(y * 16 + x) * 4 + c]!;
    expect(at(8, 8, 0)).toBe(255); // R
    expect(at(9, 9, 0)).toBe(255);
  });

  it('DesktopSize pseudo-encoding resizes the framebuffer and fires onResize', () => {
    const ws = new FakeWebSocket();
    const onResize = vi.fn();
    const onFB = vi.fn();
    const client = new RFBClient({
      ws: ws as unknown as WebSocket,
      events: { onResize, onFramebufferReady: onFB },
    });
    handshake(ws, 64, 32);
    onResize.mockClear();
    onFB.mockClear();
    // FBU: DesktopSize rect to 128×64.
    ws.feed(concat(fbuHeader(1), rectHeader(0, 0, 128, 64, ENC_DESKTOP_SIZE)));
    expect(onResize).toHaveBeenCalledWith(128, 64);
    expect(onFB).toHaveBeenCalledTimes(1);
    expect(client.framebuffer()!.width).toBe(128);
    expect(client.framebuffer()!.height).toBe(64);
  });

  it('ExtendedDesktopSize: consumes the per-screen body and resizes', () => {
    const ws = new FakeWebSocket();
    const onResize = vi.fn();
    const client = new RFBClient({
      ws: ws as unknown as WebSocket,
      events: { onResize },
    });
    handshake(ws, 64, 32);
    onResize.mockClear();
    // Body: 1 screen → 4 header bytes (screens=1, pad×3) + 1×16 bytes screen.
    const body = new Uint8Array(4 + 16);
    body[0] = 1; // screens
    ws.feed(concat(fbuHeader(1), rectHeader(0, 0, 200, 100, ENC_EXTENDED_DESKTOP_SIZE), body));
    expect(onResize).toHaveBeenCalledWith(200, 100);
    expect(client.framebuffer()!.width).toBe(200);
  });

  it('Cursor pseudo-encoding: decodes pixels + mask and exposes a CursorShape', () => {
    const ws = new FakeWebSocket();
    const onCursor = vi.fn();
    const client = new RFBClient({
      ws: ws as unknown as WebSocket,
      events: { onCursorChange: onCursor },
    });
    handshake(ws, 32, 32);
    // 2×2 cursor with hotspot (1, 1):
    //   pixel data 4×4 BGRX (16 bytes total — 2×2×4)
    //   mask: ceil(2/8)*2 = 2 bytes
    // mask bits MSB-first: top row 0b11000000 → pixels (0,0) and (1,0) opaque
    //                     bot row 0b10000000 → only (0,1) opaque
    const pixels = new Uint8Array([
      // (0,0): BGRX (255, 0, 0, 0) — pure blue when reordered → R=0 G=0 B=255
      255, 0, 0, 0,
      // (1,0): BGRX (0, 255, 0, 0) → green
      0, 255, 0, 0,
      // (0,1): BGRX (0, 0, 255, 0) → red
      0, 0, 255, 0,
      // (1,1): BGRX (50, 50, 50, 0) → grey
      50, 50, 50, 0,
    ]);
    const mask = new Uint8Array([0xc0, 0x80]); // row 0: 11000000, row 1: 10000000
    ws.feed(
      concat(
        fbuHeader(1),
        rectHeader(1, 1, 2, 2, ENC_CURSOR), // hotspot at (1, 1)
        pixels,
        mask,
      ),
    );
    expect(onCursor).toHaveBeenCalledTimes(1);
    const shape = client.getCursorShape()!;
    expect(shape).not.toBeNull();
    expect(shape.width).toBe(2);
    expect(shape.height).toBe(2);
    expect(shape.hotspotX).toBe(1);
    expect(shape.hotspotY).toBe(1);
    // Check pixel (0, 0) — opaque blue.
    expect(shape.rgba[0]).toBe(0); // R
    expect(shape.rgba[1]).toBe(0); // G
    expect(shape.rgba[2]).toBe(255); // B
    expect(shape.rgba[3]).toBe(255); // alpha (mask bit was 1)
    // Pixel (1, 0): opaque green.
    expect(shape.rgba[7]).toBe(255);
    // Pixel (1, 1): mask bit was 0 → fully transparent.
    expect(shape.rgba[(1 * 2 + 1) * 4 + 3]).toBe(0);
    // Alpha array matches.
    expect(shape.alpha[0]).toBe(255);
    expect(shape.alpha[3]).toBe(0);
  });

  it('Zero-sized cursor (cursor-hide) delivers an empty CursorShape', () => {
    const ws = new FakeWebSocket();
    const onCursor = vi.fn();
    const client = new RFBClient({
      ws: ws as unknown as WebSocket,
      events: { onCursorChange: onCursor },
    });
    handshake(ws);
    ws.feed(
      concat(
        fbuHeader(1),
        rectHeader(0, 0, 0, 0, ENC_CURSOR),
        // no body — w=0, h=0 → total=0
      ),
    );
    expect(onCursor).toHaveBeenCalledTimes(1);
    const shape = client.getCursorShape()!;
    expect(shape.width).toBe(0);
    expect(shape.height).toBe(0);
    expect(shape.rgba.length).toBe(0);
    expect(shape.alpha.length).toBe(0);
  });

  it('unknown encoding fires onError and does not crash the loop', () => {
    const ws = new FakeWebSocket();
    const onError = vi.fn();
    const client = new RFBClient({
      ws: ws as unknown as WebSocket,
      events: { onError },
    });
    handshake(ws, 16, 16);
    // Send a FBU with an unsupported encoding number.
    ws.feed(concat(fbuHeader(1), rectHeader(0, 0, 4, 4, 0x4242)));
    expect(onError).toHaveBeenCalled();
    expect((onError.mock.calls[0]![0] as Error).message).toMatch(/Unsupported RFB encoding/);
    void client;
  });
});
