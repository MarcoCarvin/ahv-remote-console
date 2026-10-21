/**
 * RFBClient state-machine + handshake tests.
 *
 * The client speaks RFB 3.8 with security type "None". The handshake
 * is the highest-risk path in the file — getting the byte offsets
 * wrong here corrupts everything that follows. We exercise:
 *
 *   - Happy-path handshake: version → security → security_result →
 *     ServerInit → normal
 *   - Stress: server bytes arrive 1-byte-at-a-time
 *   - State guards: sendKey / sendPointer / requestUpdate are dropped
 *     before state === 'normal'
 *   - Error paths: nTypes=0, no 'None' offered, security_result != 0
 *   - Encoding-list emission (with and without the extended-key flag)
 *   - SetDesktopSize wire layout
 *
 * The WebSocket is a hand-rolled fake (FakeWebSocket below) — we don't
 * need a real WS implementation. Real-WS tests would require a server
 * and CI doesn't have one.
 */

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { RFBClient } from '../client';
import type { Framebuffer } from '../encodings';
import {
  CLI_MSG_SET_PIXEL_FORMAT,
  CLI_MSG_SET_ENCODINGS,
  CLI_MSG_FRAMEBUFFER_UPDATE_REQUEST,
  CLI_MSG_KEY_EVENT,
  CLI_MSG_POINTER_EVENT,
  CLI_MSG_SET_DESKTOP_SIZE,
  CLI_MSG_QEMU,
  ENC_COPY_RECT,
  ENC_TIGHT,
  ENC_RAW,
  ENC_DESKTOP_SIZE,
  ENC_CURSOR,
  ENC_EXTENDED_DESKTOP_SIZE,
  ENC_QEMU_EXT_KEY_EVENT,
  ENC_QUALITY_BASE,
  ENC_COMPRESS_BASE,
  SRV_MSG_SET_COLOUR_MAP_ENTRIES,
  SRV_MSG_SERVER_CUT_TEXT,
} from '../protocol';

// ─── FakeWebSocket ──────────────────────────────────────────────────────

interface WSListener {
  type: 'message' | 'close' | 'error';
  handler: (evt: unknown) => void;
}

class FakeWebSocket {
  static readonly CONNECTING = 0;
  static readonly OPEN = 1;
  static readonly CLOSING = 2;
  static readonly CLOSED = 3;

  binaryType = 'arraybuffer';
  readyState = 1; // OPEN
  bufferedAmount = 0;

  /** Bytes the client called `.send()` with, in order. */
  readonly sent: Uint8Array[] = [];

  private readonly listeners: WSListener[] = [];

  addEventListener(type: 'message' | 'close' | 'error', handler: (evt: unknown) => void): void {
    this.listeners.push({ type, handler });
  }

  removeEventListener(type: 'message' | 'close' | 'error', handler: (evt: unknown) => void): void {
    const idx = this.listeners.findIndex((l) => l.type === type && l.handler === handler);
    if (idx >= 0) this.listeners.splice(idx, 1);
  }

  send(buf: ArrayBuffer | Uint8Array): void {
    const view = buf instanceof Uint8Array ? new Uint8Array(buf) : new Uint8Array(buf);
    this.sent.push(view);
  }

  close(): void {
    this.readyState = FakeWebSocket.CLOSED;
  }

  /** Test helper: deliver a chunk of bytes to the client as a 'message' event. */
  feed(bytes: Uint8Array | ArrayBuffer): void {
    const data =
      bytes instanceof Uint8Array
        ? bytes.buffer.slice(bytes.byteOffset, bytes.byteOffset + bytes.byteLength)
        : bytes;
    for (const l of this.listeners) {
      if (l.type === 'message') l.handler({ data });
    }
  }

  /** Test helper: simulate a server-initiated close. */
  triggerClose(): void {
    for (const l of this.listeners) {
      if (l.type === 'close') l.handler({});
    }
  }

  /** Test helper: simulate a WS error. */
  triggerError(): void {
    for (const l of this.listeners) {
      if (l.type === 'error') l.handler({});
    }
  }
}

// Every client a test creates must be disposed in afterEach: a client that
// completes the handshake starts the 50ms re-prime pump (_prpTimer), and a
// leaked pump fires after this file's afterEach removes the fake global
// WebSocket — an unhandled ReferenceError that fails the whole vitest run.
const liveClients: RFBClient[] = [];

function makeClient(opts: ConstructorParameters<typeof RFBClient>[0]): RFBClient {
  const client = new RFBClient(opts);
  liveClients.push(client);
  return client;
}

// Provide a global WebSocket constant with OPEN === 1 so client.ts's
// `this.ws.readyState !== WebSocket.OPEN` check works in Node. RFBClient
// uses the global `WebSocket` identifier.
beforeEach(() => {
  (globalThis as { WebSocket?: unknown }).WebSocket = FakeWebSocket;
});
afterEach(() => {
  for (const client of liveClients.splice(0)) client.dispose();
  delete (globalThis as { WebSocket?: unknown }).WebSocket;
});

// ─── Handshake helpers ──────────────────────────────────────────────────

/** Build the 12-byte server ProtocolVersion greeting. */
function serverVersionBytes(): Uint8Array {
  return new TextEncoder().encode('RFB 003.008\n');
}

/** SecurityType list announcing only None (1). */
function securityListNone(): Uint8Array {
  return new Uint8Array([1, 1]); // count=1, [None]
}

/** SecurityResult = 0 (success). */
function securityResultOK(): Uint8Array {
  return new Uint8Array([0, 0, 0, 0]);
}

/**
 * ServerInit message:
 *   u16 width, u16 height,
 *   16 bytes server pixel format,
 *   u32 nameLen, N bytes name
 */
function serverInit(width: number, height: number, name: string): Uint8Array {
  const nameBytes = new TextEncoder().encode(name);
  const buf = new Uint8Array(24 + nameBytes.length);
  const dv = new DataView(buf.buffer);
  dv.setUint16(0, width);
  dv.setUint16(2, height);
  // 16 bytes pixel format — fill with zeros (server's preference; we override).
  dv.setUint32(20, nameBytes.length);
  buf.set(nameBytes, 24);
  return buf;
}

/**
 * A FramebufferUpdate carrying ONLY QEMU's bodyless Extended-Key-Event ack rect
 * (encoding -258, full-framebuffer geometry, no pixel data) — what QEMU sends to
 * confirm it accepted our advertised -258 pseudo-encoding.
 */
function extKeyAckFbu(width: number, height: number): Uint8Array {
  const buf = new Uint8Array(4 + 12);
  const dv = new DataView(buf.buffer);
  dv.setUint8(0, 0); // message-type: FramebufferUpdate
  dv.setUint8(1, 0); // padding
  dv.setUint16(2, 1); // 1 rectangle
  dv.setUint16(4, 0); // x
  dv.setUint16(6, 0); // y
  dv.setUint16(8, width); // w
  dv.setUint16(10, height); // h
  dv.setInt32(12, ENC_QEMU_EXT_KEY_EVENT); // encoding -258
  return buf;
}

/** Drive a fresh client through the full handshake to the 'normal' state. */
function handshake(
  client: RFBClient,
  ws: FakeWebSocket,
  opts: { width?: number; height?: number; name?: string } = {},
): void {
  const w = opts.width ?? 800;
  const h = opts.height ?? 600;
  const name = opts.name ?? 'AHV-Test-VM';
  ws.feed(serverVersionBytes());
  ws.feed(securityListNone());
  ws.feed(securityResultOK());
  ws.feed(serverInit(w, h, name));
  // The handshake also fires onFramebufferReady/onResize/setEncodings/requestUpdate.
  void client;
}

// ─── Suite ──────────────────────────────────────────────────────────────

describe('RFBClient isReady', () => {
  it('goes false when the socket closes — no fake-ready over a dead socket', () => {
    const ws = new FakeWebSocket();
    const client = makeClient({ ws: ws as unknown as WebSocket, events: {} });
    handshake(client, ws);
    expect(client.isReady).toBe(true);

    // The loopback drops: a real WebSocket flips readyState to CLOSED and fires
    // its close event. isReady MUST go false so ConsoleView's readiness poll
    // cannot re-mark a dropped console 'connected' over a corpse socket — that
    // disarmed the give-up watchdog and froze the console on a fake 'connected'.
    ws.readyState = FakeWebSocket.CLOSED;
    ws.triggerClose();
    expect(client.isReady).toBe(false);
  });

  it('disposes the request timer on close — no "send BLOCKED" spam over a dead socket', async () => {
    // Recovery regression: when the loopback dies, the dead client must stop its
    // ~50ms framebuffer-request timer. Before the fix the timer kept firing
    // requestUpdate()->send() into the closed socket, spamming
    // "[rfb.tx] send BLOCKED — ws not OPEN" forever and signalling that nothing
    // disposed the client or reopened the stream.
    const ws = new FakeWebSocket();
    const warn = vi.spyOn(console, 'warn').mockImplementation(() => undefined);
    const client = makeClient({ ws: ws as unknown as WebSocket, events: {} });
    handshake(client, ws); // completes the handshake → arms the request timer
    ws.readyState = FakeWebSocket.CLOSED;
    ws.triggerClose(); // closeHandler disposes → clears the timer
    await new Promise((r) => setTimeout(r, 120)); // > 2 timer intervals
    const blocked = warn.mock.calls.filter((c) => String(c[0]).includes('send BLOCKED'));
    expect(blocked).toHaveLength(0);
    warn.mockRestore();
  });
});

describe('RFBClient handshake', () => {
  it('walks ProtocolVersion → Security → SecurityResult → ServerInit and ends in normal state', () => {
    const ws = new FakeWebSocket();
    const onFB = vi.fn();
    const onResize = vi.fn();
    const onError = vi.fn();
    const client = makeClient({
      ws: ws as unknown as WebSocket,
      events: {
        onFramebufferReady: onFB,
        onResize,
        onError,
      },
    });

    // 1. Server greets.
    ws.feed(serverVersionBytes());
    // Client should have replied with its own ProtocolVersion.
    expect(ws.sent.length).toBe(1);
    expect(new TextDecoder().decode(ws.sent[0])).toBe('RFB 003.008\n');

    // 2. Server offers security types.
    ws.feed(securityListNone());
    // Client selects type 1 (None).
    expect(ws.sent.length).toBe(2);
    expect(Array.from(ws.sent[1]!)).toEqual([1]);

    // 3. Server reports security OK.
    ws.feed(securityResultOK());
    // Client sends ClientInit (shared=1).
    expect(ws.sent.length).toBe(3);
    expect(Array.from(ws.sent[2]!)).toEqual([1]);

    // 4. Server sends ServerInit. After this the client emits:
    //    - SetPixelFormat (20 bytes)
    //    - SetEncodings (variable)
    //    - FramebufferUpdateRequest (10 bytes, non-incremental)
    ws.feed(serverInit(1024, 768, 'AHV-Test-VM'));
    expect(onFB).toHaveBeenCalledTimes(1);
    expect(onResize).toHaveBeenCalledWith(1024, 768);
    expect(onError).not.toHaveBeenCalled();
    expect(client.framebuffer()).not.toBeNull();
    expect(client.framebuffer()!.width).toBe(1024);
    expect(client.framebuffer()!.height).toBe(768);
    expect(client.framebufferSize).toEqual({ width: 1024, height: 768 });

    // Three more frames from the post-init bootstrap.
    expect(ws.sent.length).toBe(6);
  });

  it('parses a unicode server name correctly', () => {
    const ws = new FakeWebSocket();
    const client = makeClient({ ws: ws as unknown as WebSocket, events: {} });
    handshake(client, ws, { name: 'AHV-VM-日本' });
    // Server name is consumed but not exposed publicly; the test confirms the
    // handshake completes without throwing despite the multi-byte UTF-8 name.
    expect(client.framebuffer()).not.toBeNull();
  });

  it('handles handshake bytes arriving 1-byte-at-a-time (stress)', () => {
    const ws = new FakeWebSocket();
    const onFB = vi.fn();
    const client = makeClient({
      ws: ws as unknown as WebSocket,
      events: { onFramebufferReady: onFB },
    });

    const allBytes = new Uint8Array([
      ...serverVersionBytes(),
      ...securityListNone(),
      ...securityResultOK(),
      ...serverInit(640, 480, 'X'),
    ]);
    for (const byte of allBytes) {
      ws.feed(new Uint8Array([byte]));
    }
    expect(onFB).toHaveBeenCalledTimes(1);
    expect(client.framebuffer()!.width).toBe(640);
    expect(client.framebuffer()!.height).toBe(480);
  });

  it('handles handshake bytes arriving as one giant blob', () => {
    const ws = new FakeWebSocket();
    const onFB = vi.fn();
    const client = makeClient({
      ws: ws as unknown as WebSocket,
      events: { onFramebufferReady: onFB },
    });
    const allBytes = new Uint8Array([
      ...serverVersionBytes(),
      ...securityListNone(),
      ...securityResultOK(),
      ...serverInit(320, 200, 'tiny'),
    ]);
    ws.feed(allBytes);
    expect(onFB).toHaveBeenCalledTimes(1);
    expect(client.framebuffer()!.width).toBe(320);
  });
});

describe('RFBClient pixel format negotiation', () => {
  it('sends SetPixelFormat with 32bpp BGRX little-endian after ServerInit', () => {
    const ws = new FakeWebSocket();
    const client = makeClient({ ws: ws as unknown as WebSocket, events: {} });
    handshake(client, ws);
    // Find the SetPixelFormat message — first byte after handshake.
    // Send order after ServerInit is: SetPixelFormat, SetEncodings, requestUpdate.
    const spf = ws.sent.find((b) => b[0] === CLI_MSG_SET_PIXEL_FORMAT && b.length === 20);
    expect(spf).toBeDefined();
    expect(spf![0]).toBe(CLI_MSG_SET_PIXEL_FORMAT);
    expect(spf![4]).toBe(32); // bpp
    expect(spf![5]).toBe(24); // depth
    expect(spf![6]).toBe(0); // big-endian = false
    expect(spf![7]).toBe(1); // truecolor = true
    // redMax, greenMax, blueMax — all 255 (u16 BE)
    const dv = new DataView(spf!.buffer, spf!.byteOffset);
    expect(dv.getUint16(8)).toBe(255);
    expect(dv.getUint16(10)).toBe(255);
    expect(dv.getUint16(12)).toBe(255);
    expect(spf![14]).toBe(16); // red shift
    expect(spf![15]).toBe(8); // green shift
    expect(spf![16]).toBe(0); // blue shift
  });
});

describe('RFBClient low-color (RGB565) negotiation', () => {
  it('sends SetPixelFormat with 16bpp RGB565 little-endian when lowColor is set', () => {
    const ws = new FakeWebSocket();
    const client = makeClient({ ws: ws as unknown as WebSocket, events: {}, lowColor: true });
    handshake(client, ws);
    const spf = ws.sent.find((b) => b[0] === CLI_MSG_SET_PIXEL_FORMAT && b.length === 20);
    expect(spf).toBeDefined();
    expect(spf![4]).toBe(16); // bpp
    expect(spf![5]).toBe(16); // depth
    expect(spf![6]).toBe(0); // big-endian = false
    expect(spf![7]).toBe(1); // truecolor = true
    const dv = new DataView(spf!.buffer, spf!.byteOffset);
    expect(dv.getUint16(8)).toBe(31); // red max
    expect(dv.getUint16(10)).toBe(63); // green max
    expect(dv.getUint16(12)).toBe(31); // blue max
    expect(spf![14]).toBe(11); // red shift
    expect(spf![15]).toBe(5); // green shift
    expect(spf![16]).toBe(0); // blue shift
  });

  it('decodes a Raw FBU at 2 bytes/pixel end-to-end (565 → RGBA8888)', () => {
    const ws = new FakeWebSocket();
    let fb: Framebuffer | undefined;
    const client = makeClient({
      ws: ws as unknown as WebSocket,
      events: {
        onFramebufferReady: (f) => {
          fb = f;
        },
      },
      lowColor: true,
    });
    handshake(client, ws, { width: 800, height: 600 });
    // FBU: one 1×1 Raw rect, payload 0xF800 (pure red) little-endian.
    const buf = new Uint8Array(4 + 12 + 2);
    const dv = new DataView(buf.buffer);
    dv.setUint8(0, 0); // FramebufferUpdate
    dv.setUint16(2, 1); // nRects
    dv.setUint16(8, 1); // w
    dv.setUint16(10, 1); // h
    dv.setInt32(12, ENC_RAW);
    buf[16] = 0x00;
    buf[17] = 0xf8;
    ws.feed(buf);
    expect(Array.from(fb!.pixels.slice(0, 4))).toEqual([255, 0, 0, 255]);
  });

  it('frames the cursor pseudo-encoding at 2 bytes/pixel (stream stays aligned)', () => {
    const ws = new FakeWebSocket();
    const onCursorChange = vi.fn();
    const onProtocolReset = vi.fn();
    let fb: Framebuffer | undefined;
    const client = makeClient({
      ws: ws as unknown as WebSocket,
      events: {
        onCursorChange,
        onProtocolReset,
        onFramebufferReady: (f) => {
          fb = f;
        },
      },
      lowColor: true,
    });
    handshake(client, ws, { width: 800, height: 600 });
    // FBU: a 1×1 cursor rect (2 pixel bytes = 0x07E0 green + 1 mask byte),
    // then a trailing 1×1 Raw rect proving the cursor consumed EXACTLY
    // 2+1 bytes — a 4-byte read (the 32bpp assumption) would desync here.
    const buf = new Uint8Array(4 + 12 + 3 + 12 + 2);
    const dv = new DataView(buf.buffer);
    dv.setUint8(0, 0); // FramebufferUpdate
    dv.setUint16(2, 2); // nRects
    // rect 0: cursor, hotspot (0,0), 1×1
    dv.setUint16(8, 1); // w
    dv.setUint16(10, 1); // h
    dv.setInt32(12, ENC_CURSOR);
    buf[16] = 0xe0; // pixel lo
    buf[17] = 0x07; // pixel hi → 0x07E0 green
    buf[18] = 0x80; // mask: bit 7 set → opaque
    // rect 1: Raw 1×1 @0,0, pixel 0xF800 red
    dv.setUint16(23, 1); // w
    dv.setUint16(25, 1); // h
    dv.setInt32(27, ENC_RAW);
    buf[31] = 0x00;
    buf[32] = 0xf8;
    ws.feed(buf);
    expect(onProtocolReset).not.toHaveBeenCalled();
    expect(onCursorChange).toHaveBeenCalledTimes(1);
    const shape = onCursorChange.mock.calls[0]![0] as { rgba: Uint8ClampedArray };
    expect(Array.from(shape.rgba)).toEqual([0, 255, 0, 255]);
    // The trailing Raw rect decoded → alignment held through the cursor rect.
    expect(Array.from(fb!.pixels.slice(0, 4))).toEqual([255, 0, 0, 255]);
  });
});

describe('RFBClient SetEncodings', () => {
  it('emits SetEncodings with the default encoding list', () => {
    const ws = new FakeWebSocket();
    const client = makeClient({ ws: ws as unknown as WebSocket, events: {} });
    handshake(client, ws);
    const setEnc = ws.sent.find((b) => b[0] === CLI_MSG_SET_ENCODINGS);
    expect(setEnc).toBeDefined();
    const dv = new DataView(setEnc!.buffer, setEnc!.byteOffset);
    const count = dv.getUint16(2);
    const encs: number[] = [];
    for (let i = 0; i < count; i++) {
      encs.push(dv.getInt32(4 + i * 4));
    }
    // Tight is the preferred content-adaptive pixel encoding (listed before
    // Raw, which stays as the guaranteed fallback). ZRLE + Hextile remain
    // disabled (Tight subsumes them).
    // ENC_QEMU_EXT_KEY_EVENT (-258) is announced UNCONDITIONALLY now: it's a
    // harmless no-op on servers that don't support it, and announcing it is what
    // lets us send scancodes (the only reliable pre-OS / firmware-menu input).
    expect(encs).toEqual([
      ENC_COPY_RECT,
      ENC_TIGHT,
      ENC_RAW,
      ENC_DESKTOP_SIZE,
      ENC_CURSOR,
      ENC_EXTENDED_DESKTOP_SIZE,
      ENC_QEMU_EXT_KEY_EVENT,
    ]);
  });

  it('includes ENC_QEMU_EXT_KEY_EVENT when enableExtendedKey is true', () => {
    const ws = new FakeWebSocket();
    const client = makeClient({
      ws: ws as unknown as WebSocket,
      events: {},
      enableExtendedKey: true,
    });
    handshake(client, ws);
    const setEnc = ws.sent.find((b) => b[0] === CLI_MSG_SET_ENCODINGS);
    const dv = new DataView(setEnc!.buffer, setEnc!.byteOffset);
    const count = dv.getUint16(2);
    const encs: number[] = [];
    for (let i = 0; i < count; i++) encs.push(dv.getInt32(4 + i * 4));
    expect(encs).toContain(ENC_QEMU_EXT_KEY_EVENT);
    expect(client.extendedKeyEnabled()).toBe(true);
  });

  it('extendedKeyEnabled() defaults to false', () => {
    const ws = new FakeWebSocket();
    const client = makeClient({ ws: ws as unknown as WebSocket, events: {} });
    expect(client.extendedKeyEnabled()).toBe(false);
  });
});

describe('RFBClient QEMU Extended Key Event ack (-258)', () => {
  it("handles QEMU's bodyless -258 ack pseudo-rect as a no-op, not a desync", () => {
    const ws = new FakeWebSocket();
    const onProtocolReset = vi.fn();
    const onError = vi.fn();
    let fb: Framebuffer | undefined;
    const client = makeClient({
      ws: ws as unknown as WebSocket,
      events: {
        onProtocolReset,
        onError,
        onFramebufferReady: (f) => {
          fb = f;
        },
      },
    });
    handshake(client, ws, { width: 1920, height: 1080 });

    // QEMU's send_ext_key_event_ack (ui/vnc.c) replies to our advertised
    // ENC_QEMU_EXT_KEY_EVENT (-258) with a FramebufferUpdate whose ONLY rect is
    // a BODYLESS pseudo-rect at 0,0,client_width,client_height, encoding -258.
    // We append a 1×1 Raw rect to prove the stream stays byte-aligned: if the
    // ack were mis-consumed (or routed to the unsupported-encoding reset path,
    // the bug), this Raw rect would never decode.
    ws.feed(
      new Uint8Array([
        0,
        0,
        0,
        2, // FramebufferUpdate, pad, nRects=2
        // rect 0 — the -258 ack, full-framebuffer geometry, NO body
        0,
        0,
        0,
        0,
        0x07,
        0x80,
        0x04,
        0x38,
        0xff,
        0xff,
        0xfe,
        0xfe,
        // rect 1 — Raw 1×1 @0,0, BGRX bytes 11 22 33 44
        0,
        0,
        0,
        0,
        0,
        1,
        0,
        1,
        0,
        0,
        0,
        0,
        0x11,
        0x22,
        0x33,
        0x44,
      ]),
    );

    // The fix: the -258 ack must NOT trip protocolReset / onError.
    expect(onProtocolReset).not.toHaveBeenCalled();
    expect(onError).not.toHaveBeenCalled();
    // Stream stayed aligned past the ack: the trailing Raw rect decoded
    // (BGRX 11 22 33 44 → canvas RGBA 33 22 11 255).
    expect(fb).toBeDefined();
    expect(Array.from(fb!.pixels.slice(0, 4))).toEqual([0x33, 0x22, 0x11, 255]);
  });

  it('auto-enables scancode passthrough once QEMU ACKs the -258 pseudo-encoding', () => {
    const ws = new FakeWebSocket();
    const client = makeClient({ ws: ws as unknown as WebSocket, events: {} });
    handshake(client, ws, { width: 1920, height: 1080 });
    // Keysym path until the server confirms it accepts ext-key frames...
    expect(client.extendedKeyEnabled()).toBe(false);
    ws.feed(extKeyAckFbu(1920, 1080));
    // ...scancode passthrough from the ack on. The input engine reads this per
    // keystroke, so the guest then decodes physical scancodes with its OWN
    // layout — no en-us keysym translation to corrupt a non-en-us guest.
    expect(client.extendedKeyEnabled()).toBe(true);
  });
});

describe('RFBClient state guards (state !== normal)', () => {
  it('sendKey before handshake is dropped (no bytes on the wire)', () => {
    const ws = new FakeWebSocket();
    const warn = vi.spyOn(console, 'warn').mockImplementation(() => {
      /* silence */
    });
    const client = makeClient({ ws: ws as unknown as WebSocket, events: {} });
    // Note: still in 'version' state.
    client.sendKey(0x61, true);
    // No KeyEvent should have been emitted. (The constructor itself
    // doesn't send anything either — the first send is in response
    // to the server's version greeting.)
    expect(ws.sent.length).toBe(0);
    warn.mockRestore();
  });

  it('sendKey after handshake produces a CLI_MSG_KEY_EVENT (8 bytes)', () => {
    const ws = new FakeWebSocket();
    const client = makeClient({ ws: ws as unknown as WebSocket, events: {} });
    handshake(client, ws);
    const beforeCount = ws.sent.length;
    client.sendKey(0x61, true); // 'a' down
    expect(ws.sent.length).toBe(beforeCount + 1);
    const last = ws.sent[ws.sent.length - 1]!;
    expect(last[0]).toBe(CLI_MSG_KEY_EVENT);
    expect(last[1]).toBe(1); // down
    expect(last.length).toBe(8);
    const dv = new DataView(last.buffer, last.byteOffset);
    expect(dv.getUint32(4)).toBe(0x61);
  });

  it('sendPointer before handshake is dropped (state !== normal)', () => {
    const ws = new FakeWebSocket();
    const client = makeClient({ ws: ws as unknown as WebSocket, events: {} });
    client.sendPointer(100, 200, 0);
    // Dropped because the socket is not in 'normal' state yet, and nothing is
    // logged (the old console.warn leaked pointer coordinates). The drop is
    // counted on the rfbDiag counter (exposed as window.__rfbDiag in a browser).
    expect(ws.sent.length).toBe(0);
  });

  it('sendPointer after handshake produces a CLI_MSG_POINTER_EVENT (6 bytes)', () => {
    const ws = new FakeWebSocket();
    const client = makeClient({ ws: ws as unknown as WebSocket, events: {} });
    handshake(client, ws);
    const before = ws.sent.length;
    client.sendPointer(123, 456, 0x05); // mask = left + right
    expect(ws.sent.length).toBe(before + 1);
    const last = ws.sent[ws.sent.length - 1]!;
    expect(last[0]).toBe(CLI_MSG_POINTER_EVENT);
    expect(last[1]).toBe(0x05);
    const dv = new DataView(last.buffer, last.byteOffset);
    expect(dv.getUint16(2)).toBe(123);
    expect(dv.getUint16(4)).toBe(456);
    expect(last.length).toBe(6);
  });

  it('primeOnInput sends one incremental FramebufferUpdateRequest, debounced', () => {
    const ws = new FakeWebSocket();
    const client = makeClient({ ws: ws as unknown as WebSocket, events: {} });
    handshake(client, ws);
    const before = ws.sent.length;

    client.primeOnInput();
    expect(ws.sent.length).toBe(before + 1);
    const sent = ws.sent[ws.sent.length - 1]!;
    expect(sent[0]).toBe(CLI_MSG_FRAMEBUFFER_UPDATE_REQUEST);
    expect(sent[1]).toBe(1); // incremental — must NOT force a full-frame request
    expect(sent.length).toBe(10);

    // Leading-edge debounced: a second prime within the window sends nothing.
    client.primeOnInput();
    expect(ws.sent.length).toBe(before + 1);
  });

  it('sendPointer clamps x and y to the 0..65535 range (16-bit RFB field width)', () => {
    const ws = new FakeWebSocket();
    const client = makeClient({ ws: ws as unknown as WebSocket, events: {} });
    handshake(client, ws);
    client.sendPointer(-100, 999999, 0);
    const last = ws.sent[ws.sent.length - 1]!;
    const dv = new DataView(last.buffer, last.byteOffset);
    expect(dv.getUint16(2)).toBe(0);
    expect(dv.getUint16(4)).toBe(0xffff);
  });

  it('sendPointer({ synthetic: true }) does NOT update lastOperatorPointer', () => {
    const ws = new FakeWebSocket();
    const client = makeClient({ ws: ws as unknown as WebSocket, events: {} });
    handshake(client, ws);
    // Real operator move first.
    client.sendPointer(50, 60, 1);
    expect(client.getLastOperatorPointer()).toEqual({ x: 50, y: 60, buttonMask: 1 });
    // Then a synthetic transport burst — must NOT overwrite.
    client.sendPointer(65535, 65535, 0, { synthetic: true });
    expect(client.getLastOperatorPointer()).toEqual({ x: 50, y: 60, buttonMask: 1 });
  });

  it('sendKeyEvents before handshake is dropped', () => {
    const ws = new FakeWebSocket();
    const warn = vi.spyOn(console, 'warn').mockImplementation(() => {
      /* silence */
    });
    const client = makeClient({ ws: ws as unknown as WebSocket, events: {} });
    client.sendKeyEvents([
      [0x61, true],
      [0x61, false],
    ]);
    expect(ws.sent.length).toBe(0);
    warn.mockRestore();
  });

  it('sendKeyEvents single-chunk fast path: emits ONE wire frame with N*8 bytes', () => {
    const ws = new FakeWebSocket();
    const log = vi.spyOn(console, 'log').mockImplementation(() => {
      /* silence */
    });
    const client = makeClient({ ws: ws as unknown as WebSocket, events: {} });
    handshake(client, ws);
    const before = ws.sent.length;
    const events: readonly (readonly [number, boolean])[] = [
      [0x61, true],
      [0x61, false],
      [0x62, true],
      [0x62, false],
    ];
    client.sendKeyEvents(events);
    expect(ws.sent.length).toBe(before + 1);
    const last = ws.sent[ws.sent.length - 1]!;
    expect(last.length).toBe(4 * 8);
    expect(last[0]).toBe(CLI_MSG_KEY_EVENT);
    log.mockRestore();
  });

  it('sendKeyEvents fires onChunk callback in the single-chunk path', () => {
    const ws = new FakeWebSocket();
    const log = vi.spyOn(console, 'log').mockImplementation(() => {
      /* silence */
    });
    const client = makeClient({ ws: ws as unknown as WebSocket, events: {} });
    handshake(client, ws);
    const onChunk = vi.fn();
    client.sendKeyEvents(
      [
        [0x61, true],
        [0x61, false],
      ],
      { onChunk },
    );
    expect(onChunk).toHaveBeenCalledWith(2, 2);
    log.mockRestore();
  });

  it('sendExtendedKey transmits regardless of enableExtendedKey (pseudo-encoding always announced)', () => {
    const ws = new FakeWebSocket();
    const client = makeClient({ ws: ws as unknown as WebSocket, events: {} });
    handshake(client, ws);
    const before = ws.sent.length;
    client.sendExtendedKey(0x61, 30, true);
    // The -258 pseudo-encoding is now announced unconditionally, so the frame is
    // valid to send. sendExtendedKey no longer self-gates on the flag — callers
    // (InputEngine) decide when to use it; the OVMF wizard uses it directly.
    expect(ws.sent.length).toBe(before + 1);
  });

  it('sendExtendedKey emits CLI_MSG_QEMU when enableExtendedKey is true', () => {
    const ws = new FakeWebSocket();
    const client = makeClient({
      ws: ws as unknown as WebSocket,
      events: {},
      enableExtendedKey: true,
    });
    handshake(client, ws);
    const before = ws.sent.length;
    client.sendExtendedKey(0x61, 30, true);
    expect(ws.sent.length).toBe(before + 1);
    const last = ws.sent[ws.sent.length - 1]!;
    expect(last[0]).toBe(CLI_MSG_QEMU);
    expect(last[1]).toBe(0); // QEMU_SUBMSG_EXT_KEY
    const dv = new DataView(last.buffer, last.byteOffset);
    expect(dv.getUint16(2)).toBe(1); // down=1
    expect(dv.getUint32(4)).toBe(0x61); // keysym
    expect(dv.getUint32(8)).toBe(30); // keycode
    expect(last.length).toBe(12);
  });

  it('requestUpdate before handshake is a no-op', () => {
    const ws = new FakeWebSocket();
    const client = makeClient({ ws: ws as unknown as WebSocket, events: {} });
    const before = ws.sent.length;
    client.requestUpdate();
    expect(ws.sent.length).toBe(before);
  });

  it('requestRectUpdate validates bounds and drops out-of-range requests', () => {
    const ws = new FakeWebSocket();
    const client = makeClient({ ws: ws as unknown as WebSocket, events: {} });
    handshake(client, ws, { width: 100, height: 100 });
    const before = ws.sent.length;
    // Negative coords → drop.
    client.requestRectUpdate(-1, 0, 10, 10);
    // Past FB edge → drop.
    client.requestRectUpdate(95, 95, 10, 10);
    // Zero w/h → drop.
    client.requestRectUpdate(0, 0, 0, 10);
    client.requestRectUpdate(0, 0, 10, 0);
    expect(ws.sent.length).toBe(before);
    // Valid request lands on the wire.
    client.requestRectUpdate(10, 10, 20, 20, true);
    expect(ws.sent.length).toBe(before + 1);
    const last = ws.sent[ws.sent.length - 1]!;
    expect(last[0]).toBe(CLI_MSG_FRAMEBUFFER_UPDATE_REQUEST);
    expect(last[1]).toBe(1); // incremental=true
    expect(last.length).toBe(10);
  });
});

describe('RFBClient SetDesktopSize wire format (RFC 6143 §7.5.5)', () => {
  it('emits a 24-byte message with the documented layout', () => {
    const ws = new FakeWebSocket();
    const client = makeClient({ ws: ws as unknown as WebSocket, events: {} });
    handshake(client, ws);
    const before = ws.sent.length;
    client.sendSetDesktopSize(1920, 1080);
    expect(ws.sent.length).toBe(before + 1);
    const buf = ws.sent[ws.sent.length - 1]!;
    expect(buf.length).toBe(24);
    expect(buf[0]).toBe(CLI_MSG_SET_DESKTOP_SIZE);
    expect(buf[1]).toBe(0); // padding
    const dv = new DataView(buf.buffer, buf.byteOffset);
    expect(dv.getUint16(2)).toBe(1920);
    expect(dv.getUint16(4)).toBe(1080);
    expect(buf[6]).toBe(1); // screens
    expect(buf[7]).toBe(0); // padding
    expect(dv.getUint32(8)).toBe(0); // screen id
    expect(dv.getUint16(12)).toBe(0); // x_pos
    expect(dv.getUint16(14)).toBe(0); // y_pos
    expect(dv.getUint16(16)).toBe(1920); // screen width
    expect(dv.getUint16(18)).toBe(1080); // screen height
    expect(dv.getUint32(20)).toBe(0); // flags
  });

  it('clamps width/height to u16 range', () => {
    const ws = new FakeWebSocket();
    const client = makeClient({ ws: ws as unknown as WebSocket, events: {} });
    handshake(client, ws);
    client.sendSetDesktopSize(-1, 9999999);
    const last = ws.sent[ws.sent.length - 1]!;
    const dv = new DataView(last.buffer, last.byteOffset);
    expect(dv.getUint16(2)).toBe(0);
    expect(dv.getUint16(4)).toBe(0xffff);
  });

  it('sendSetDesktopSize before handshake is a no-op', () => {
    const ws = new FakeWebSocket();
    const warn = vi.spyOn(console, 'warn').mockImplementation(() => {
      /* silence */
    });
    const client = makeClient({ ws: ws as unknown as WebSocket, events: {} });
    const before = ws.sent.length;
    client.sendSetDesktopSize(800, 600);
    expect(ws.sent.length).toBe(before);
    warn.mockRestore();
  });
});

describe('RFBClient handshake failure modes', () => {
  it('reports an error when the server rejects on security with nTypes=0', () => {
    const ws = new FakeWebSocket();
    const onError = vi.fn();
    const client = makeClient({
      ws: ws as unknown as WebSocket,
      events: { onError },
    });
    ws.feed(serverVersionBytes());
    // Server says "no auth types": [0, 0, 0, 0, len=11, "AccessDenied"]
    const reason = new TextEncoder().encode('AccessDenied');
    const buf = new Uint8Array(5 + reason.length);
    buf[0] = 0;
    const dv = new DataView(buf.buffer);
    dv.setUint32(1, reason.length);
    buf.set(reason, 5);
    ws.feed(buf);
    expect(onError).toHaveBeenCalledTimes(1);
    expect((onError.mock.calls[0]![0] as Error).message).toMatch(/Security handshake failed/);
    void client;
  });

  it('reports an error when the security list lacks SEC_NONE', () => {
    const ws = new FakeWebSocket();
    const onError = vi.fn();
    const client = makeClient({
      ws: ws as unknown as WebSocket,
      events: { onError },
    });
    ws.feed(serverVersionBytes());
    // Server offers only VNC-Auth (2). We can't speak that → error.
    ws.feed(new Uint8Array([1, 2]));
    expect(onError).toHaveBeenCalled();
    const err = onError.mock.calls[0]![0] as Error;
    expect(err.message).toMatch(/authentication/);
    void client;
  });

  it('reports an error when SecurityResult != 0', () => {
    const ws = new FakeWebSocket();
    const onError = vi.fn();
    const client = makeClient({
      ws: ws as unknown as WebSocket,
      events: { onError },
    });
    ws.feed(serverVersionBytes());
    ws.feed(securityListNone());
    // Failure (1).
    ws.feed(new Uint8Array([0, 0, 0, 1]));
    expect(onError).toHaveBeenCalled();
    expect((onError.mock.calls[0]![0] as Error).message).toMatch(/Security result: failure/);
    void client;
  });

  it('a handshake-phase failure routes to protocolReset and keeps the loopback OPEN (recover in place, do not strand the proxy re-handshake)', () => {
    // During the proxy's IN-PLACE re-handshake a byte-misalignment makes the
    // recreated client mis-read the security phase; closing the loopback there
    // stranded the proxy ("handshake aborted — client ws closed") and the console
    // never recovered. With onProtocolReset wired (as ConsoleView does) the
    // handshake-phase failure must keep the ws OPEN so the proxy can re-drive.
    const ws = new FakeWebSocket();
    const onProtocolReset = vi.fn();
    const onError = vi.fn();
    const client = makeClient({
      ws: ws as unknown as WebSocket,
      events: { onProtocolReset, onError },
    });
    ws.feed(serverVersionBytes());
    ws.feed(new Uint8Array([1, 2])); // offers only VNC-Auth (no SEC_NONE) — a desync here
    expect(onProtocolReset).toHaveBeenCalledTimes(1);
    expect(ws.readyState).not.toBe(FakeWebSocket.CLOSED); // loopback stays OPEN
    expect(onError).not.toHaveBeenCalled(); // recoverable, not terminal
    void client;
  });
});

describe('RFBClient lifecycle', () => {
  it('dispose() removes listeners but does NOT close the WebSocket', () => {
    const ws = new FakeWebSocket();
    const onClose = vi.fn();
    const client = makeClient({
      ws: ws as unknown as WebSocket,
      events: { onClose },
    });
    handshake(client, ws);
    client.dispose();
    // A subsequent server-side close should NOT invoke our onClose because
    // we've already unsubscribed.
    ws.triggerClose();
    expect(onClose).not.toHaveBeenCalled();
    // WS should still be OPEN (not actively closed).
    expect(ws.readyState).toBe(FakeWebSocket.OPEN);
  });

  it('close() disposes AND closes the WebSocket', () => {
    const ws = new FakeWebSocket();
    const client = makeClient({ ws: ws as unknown as WebSocket, events: {} });
    handshake(client, ws);
    client.close();
    expect(ws.readyState).toBe(FakeWebSocket.CLOSED);
  });

  it('onClose handler is invoked when the WebSocket fires its close event', () => {
    const ws = new FakeWebSocket();
    const onClose = vi.fn();
    const client = makeClient({
      ws: ws as unknown as WebSocket,
      events: { onClose },
    });
    void client;
    ws.triggerClose();
    expect(onClose).toHaveBeenCalledTimes(1);
  });

  it('onError handler is invoked when the WebSocket fires its error event', () => {
    const ws = new FakeWebSocket();
    const onError = vi.fn();
    const client = makeClient({
      ws: ws as unknown as WebSocket,
      events: { onError },
    });
    void client;
    ws.triggerError();
    expect(onError).toHaveBeenCalledTimes(1);
    expect((onError.mock.calls[0]![0] as Error).message).toMatch(/WebSocket error/);
  });

  it('onError is SUPPRESSED when the error fires on a CLOSING/CLOSED socket', () => {
    // The browser dispatches 'error' immediately before 'close' on an abnormal
    // close / deliberate close-in-flight. Firing onError there surfaced a
    // spurious "WebSocket error" on console close — guard it on readyState.
    const ws = new FakeWebSocket();
    const onError = vi.fn();
    const client = makeClient({ ws: ws as unknown as WebSocket, events: { onError } });
    void client;
    ws.readyState = FakeWebSocket.CLOSING;
    ws.triggerError();
    ws.readyState = FakeWebSocket.CLOSED;
    ws.triggerError();
    expect(onError).not.toHaveBeenCalled();
  });

  it('onError is SUPPRESSED when the error fires on a CONNECTING (aborted) socket', () => {
    // Fast open-then-close / sequential open→close→open aborts a CONNECTING
    // socket, which dispatches 'error' then 'close'. That is a close artifact,
    // not a fault — the accompanying close drives recovery via onClose.
    const ws = new FakeWebSocket();
    const onError = vi.fn();
    const client = makeClient({ ws: ws as unknown as WebSocket, events: { onError } });
    void client;
    ws.readyState = FakeWebSocket.CONNECTING;
    ws.triggerError();
    expect(onError).not.toHaveBeenCalled();
  });

  it('setEvents() swaps event handlers without resetting protocol state', () => {
    const ws = new FakeWebSocket();
    const a = vi.fn();
    const b = vi.fn();
    const client = makeClient({
      ws: ws as unknown as WebSocket,
      events: { onFramebufferReady: a },
    });
    // Swap before handshake.
    client.setEvents({ onFramebufferReady: b });
    handshake(client, ws);
    expect(a).not.toHaveBeenCalled();
    expect(b).toHaveBeenCalledTimes(1);
  });
});

// Per-rect bounds guard. A compromised/MITM'd upstream can send
// a FramebufferUpdate rect whose dimensions exceed the framebuffer; the decode
// loop would otherwise run billions of synchronous main-thread iterations from
// a handful of wire bytes (Tight Fill ≈ 4 bytes; ZRLE/COPY read past the short
// payload), freezing the renderer. The client must refuse and close.
describe('RFBClient inbound rect bounds', () => {
  // 4-byte FBU header (msg=0, pad, nRects=1) + one 12-byte rect header.
  function fbUpdate(x: number, y: number, w: number, h: number, enc: number): Uint8Array {
    const buf = new Uint8Array(4 + 12);
    const dv = new DataView(buf.buffer);
    dv.setUint8(0, 0); // SRV_MSG_FRAMEBUFFER_UPDATE
    dv.setUint16(2, 1); // nRects
    dv.setUint16(4, x);
    dv.setUint16(6, y);
    dv.setUint16(8, w);
    dv.setUint16(10, h);
    dv.setInt32(12, enc);
    return buf;
  }

  it('refuses + closes a real-pixel rect that exceeds the framebuffer', () => {
    const ws = new FakeWebSocket();
    const onError = vi.fn();
    const client = makeClient({ ws: ws as unknown as WebSocket, events: { onError } });
    handshake(client, ws, { width: 800, height: 600 });
    onError.mockClear();

    // 65535×65535 Tight rect against an 800×600 framebuffer.
    ws.feed(fbUpdate(0, 0, 65535, 65535, ENC_TIGHT));

    expect(onError).toHaveBeenCalledTimes(1);
    expect((onError.mock.calls[0]![0] as Error).message).toMatch(/exceeds framebuffer/);
    expect(ws.readyState).toBe(FakeWebSocket.CLOSED);
  });

  it('accepts an in-bounds rect (no spurious rejection)', () => {
    const ws = new FakeWebSocket();
    const onError = vi.fn();
    const client = makeClient({ ws: ws as unknown as WebSocket, events: { onError } });
    handshake(client, ws, { width: 800, height: 600 });
    onError.mockClear();

    // A 16×16 rect at the origin is well within 800×600; the decoder will
    // return NeedMore (no body yet) but the bounds guard must NOT fire.
    ws.feed(fbUpdate(0, 0, 16, 16, ENC_TIGHT));

    expect(onError).not.toHaveBeenCalled();
    expect(ws.readyState).not.toBe(FakeWebSocket.CLOSED);
  });

  it('does not bounds-check the negative ExtendedDesktopSize pseudo-encoding', () => {
    const ws = new FakeWebSocket();
    const onError = vi.fn();
    const client = makeClient({ ws: ws as unknown as WebSocket, events: { onError } });
    handshake(client, ws, { width: 800, height: 600 });
    onError.mockClear();

    // ExtendedDesktopSize legitimately carries NEW (larger) dimensions in its
    // header; it must not be rejected by the framebuffer-bounds guard. Body
    // (screen count = 0) follows so the decoder completes the resize.
    const hdr = fbUpdate(0, 0, 1024, 768, ENC_EXTENDED_DESKTOP_SIZE);
    const body = new Uint8Array(4); // number-of-screens=0 + 3 padding
    const frame = new Uint8Array(hdr.length + body.length);
    frame.set(hdr);
    frame.set(body, hdr.length);
    ws.feed(frame);

    expect(onError).not.toHaveBeenCalled();
    expect(ws.readyState).not.toBe(FakeWebSocket.CLOSED);
  });

  // A desync is RECOVERABLE (a fresh handshake realigns the stream), so when
  // the host wires onProtocolReset the client must route there — NOT to the
  // terminal onError. onError set a permanent 'error' status that the
  // subsequent onClose could no longer downgrade to 'reconnecting', which is
  // why a single desync read "Lost connection to the VM" with no self-heal.
  it('fires onProtocolReset (recoverable), not terminal onError, on an out-of-bounds rect', () => {
    const ws = new FakeWebSocket();
    const onError = vi.fn();
    const onProtocolReset = vi.fn();
    const client = makeClient({
      ws: ws as unknown as WebSocket,
      events: { onError, onProtocolReset },
    });
    handshake(client, ws, { width: 800, height: 600 });
    onError.mockClear();

    ws.feed(fbUpdate(0, 0, 65535, 65535, ENC_TIGHT));

    expect(onProtocolReset).toHaveBeenCalledTimes(1);
    expect(onProtocolReset.mock.calls[0]![0] as string).toMatch(/desync/);
    expect(onError).not.toHaveBeenCalled();
    // protocolReset now KEEPS the ws open: the proxy recovers a desync/upstream
    // drop by re-handshaking on the SAME socket (rfb_reset → recreate). Closing
    // here raced ahead of that and broke reset recovery. A fallback timer closes
    // it only if no recreate arrives; dispose() clears that timer.
    expect(ws.readyState).not.toBe(FakeWebSocket.CLOSED);
    client.dispose();
  });

  // An unknown encoding on an IN-BOUNDS rect is the other desync signature (a
  // prior rect's byte miscount makes pixel bytes parse as an encoding number);
  // it must be recoverable too, not a terminal error.
  it('treats an unknown encoding as a recoverable reset', () => {
    const ws = new FakeWebSocket();
    const onError = vi.fn();
    const onProtocolReset = vi.fn();
    const client = makeClient({
      ws: ws as unknown as WebSocket,
      events: { onError, onProtocolReset },
    });
    handshake(client, ws, { width: 800, height: 600 });
    onError.mockClear();

    ws.feed(fbUpdate(0, 0, 16, 16, 99)); // enc 99 was never advertised

    expect(onProtocolReset).toHaveBeenCalledTimes(1);
    expect(onError).not.toHaveBeenCalled();
    // ws stays open for the same-socket re-handshake (see the out-of-bounds
    // test above); the fallback close timer is cleared by dispose().
    expect(ws.readyState).not.toBe(FakeWebSocket.CLOSED);
    client.dispose();
  });

  // After a reset the remaining buffered bytes are garbage; the halt flag must
  // stop draining so we fire exactly ONE reset, not one per trailing rect.
  it('does not cascade into repeated resets on bytes trailing a desync', () => {
    const ws = new FakeWebSocket();
    const onProtocolReset = vi.fn();
    const client = makeClient({
      ws: ws as unknown as WebSocket,
      events: { onProtocolReset },
    });
    handshake(client, ws, { width: 800, height: 600 });

    // FBU declaring 2 rects: rect 1 is an unknown encoding (→ reset, returns
    // Done so the drain loop would otherwise continue), rect 2 is 12 bytes of
    // garbage that, unhalted, would parse as another bad rect → a 2nd reset.
    const frame = new Uint8Array(4 + 12 + 12);
    const dv = new DataView(frame.buffer);
    dv.setUint8(0, 0); // FramebufferUpdate
    dv.setUint16(2, 2); // nRects = 2
    dv.setUint16(8, 16); // rect 1: w=16
    dv.setUint16(10, 16); // rect 1: h=16
    dv.setInt32(12, 99); // rect 1: unknown encoding
    frame.fill(0xff, 16); // rect 2: garbage header
    ws.feed(frame);

    expect(onProtocolReset).toHaveBeenCalledTimes(1);
  });
});

// The adaptive-quality machine tracks the link tier (hysteresis + the
// onQualityChange UI event) but deliberately never emits Tight
// quality/compression pseudo-encodings: against AHV's QEMU, quality hints are
// gated behind `-vnc lossy=on` (always off) and compression hints are
// redundant (no hint = QEMU's reset default of 9, the best-bytes setting) or
// harmful (lower levels also shrink tight_conf's max-subrect limits). The
// hint-less SetEncodings is the contract — a regression that reintroduces
// hints can pin a session at 1.5-3x the bytes after one demotion round-trip.
describe('RFBClient adaptive quality (tier tracking, no wire hints)', () => {
  function lastSetEncodings(ws: FakeWebSocket): number[] | null {
    for (let i = ws.sent.length - 1; i >= 0; i--) {
      const f = ws.sent[i]!;
      if (f.length >= 4 && f[0] === CLI_MSG_SET_ENCODINGS) {
        const dv = new DataView(f.buffer, f.byteOffset, f.byteLength);
        const count = dv.getUint16(2);
        const encs: number[] = [];
        for (let j = 0; j < count; j++) encs.push(dv.getInt32(4 + j * 4));
        return encs;
      }
    }
    return null;
  }

  function expectNoTierHints(encs: number[] | null): void {
    expect(encs).not.toBeNull();
    for (let level = 0; level <= 9; level++) {
      expect(encs).not.toContain(ENC_QUALITY_BASE + level); // -32..-23
      expect(encs).not.toContain(ENC_COMPRESS_BASE + level); // -256..-247
    }
  }

  it('commits the low tier on a high-RTT link without renegotiating', () => {
    const ws = new FakeWebSocket();
    const client = makeClient({ ws: ws as unknown as WebSocket, events: {} });
    handshake(client, ws, { width: 800, height: 600 });
    const sendsAfterHandshake = ws.sent.length;

    // RTT > 150 ms → 'low', committed after QUALITY_HYSTERESIS (3) evaluations.
    client.feedHeartbeat(200);
    client.feedHeartbeat(200);
    client.feedHeartbeat(200);

    expect(client.getQualityLevel()).toBe('low');
    // The tier change must not re-send SetEncodings (or anything else).
    expect(ws.sent.length).toBe(sendsAfterHandshake);
    expectNoTierHints(lastSetEncodings(ws));
  });

  it('returns to the high tier on a fast link, still hint-less', () => {
    const ws = new FakeWebSocket();
    const client = makeClient({ ws: ws as unknown as WebSocket, events: {} });
    handshake(client, ws, { width: 800, height: 600 });

    // Force low first, then a fast link → climbs back to 'high'.
    client.feedHeartbeat(200);
    client.feedHeartbeat(200);
    client.feedHeartbeat(200);
    client.feedHeartbeat(10);
    client.feedHeartbeat(10);
    client.feedHeartbeat(10);

    expect(client.getQualityLevel()).toBe('high');
    expectNoTierHints(lastSetEncodings(ws));
  });

  it('initialQuality seeds the machine so recovery re-fires onQualityChange', () => {
    const ws = new FakeWebSocket();
    const onQualityChange = vi.fn();
    // ConsoleView persists the tier across client recreations and seeds it
    // back in; a fast link must then commit (and EMIT) the climb to 'high' —
    // a client seeded at the default 'high' would stay silent forever and
    // the host's persisted tier could never recover from 'low'.
    const client = makeClient({
      ws: ws as unknown as WebSocket,
      events: { onQualityChange },
      initialQuality: 'low',
    });
    handshake(client, ws, { width: 800, height: 600 });
    expect(client.getQualityLevel()).toBe('low');
    client.feedHeartbeat(10);
    client.feedHeartbeat(10);
    expect(client.getQualityLevel()).toBe('high');
    expect(onQualityChange).toHaveBeenCalledWith('high');
  });
});

// The FBU service-time signal: RTT alone is stale (heartbeat cadence) and
// blind to bufferbloat — a collapsed link can keep a low RTT while large
// FramebufferUpdates take hundreds of ms to arrive. Each FBU is timed from
// header to flushUpdate; the rolling p50 of LARGE FBUs (>= the 16 KiB floor)
// demotes one tier through the same QUALITY_HYSTERESIS machine, and slow
// evidence in the window blocks RTT-driven promotion.
describe('RFBClient FBU service-time demotion', () => {
  let now = 0;

  beforeEach(() => {
    now = 0;
    vi.spyOn(performance, 'now').mockImplementation(() => now);
  });
  afterEach(() => {
    vi.restoreAllMocks();
  });

  /** 4-byte FBU header declaring one rect. */
  function fbuHeader(): Uint8Array {
    const hdr = new Uint8Array(4);
    hdr[0] = 0; // SRV_MSG_FRAMEBUFFER_UPDATE
    new DataView(hdr.buffer).setUint16(2, 1);
    return hdr;
  }

  /** Raw rect of w×h at (0,0): 12-byte header + w*h*4 payload (32bpp). */
  function rawRect(w: number, h: number): Uint8Array {
    const buf = new Uint8Array(12 + w * h * 4);
    const dv = new DataView(buf.buffer);
    dv.setUint16(4, w);
    dv.setUint16(6, h);
    dv.setInt32(8, ENC_RAW);
    return buf;
  }

  /** Deliver one FBU whose body lands `serviceMs` after its header. */
  function feedFbu(ws: FakeWebSocket, w: number, h: number, serviceMs: number): void {
    ws.feed(fbuHeader()); // stamps the service-time start
    now += serviceMs;
    ws.feed(rawRect(w, h)); // completes the FBU → closes the sample
  }

  it('demotes ONE tier per hysteresis round when the p50 exceeds 250ms', () => {
    const ws = new FakeWebSocket();
    const onQualityChange = vi.fn();
    const client = makeClient({ ws: ws as unknown as WebSocket, events: { onQualityChange } });
    handshake(client, ws, { width: 800, height: 600 });

    // 64×64 Raw = 16 KiB payload — at the byte floor, so the sample counts.
    feedFbu(ws, 64, 64, 400);
    expect(client.getQualityLevel()).toBe('high'); // hysteresis: 1 of 2
    feedFbu(ws, 64, 64, 400);
    expect(client.getQualityLevel()).toBe('medium'); // one tier, not a jump
    expect(onQualityChange).toHaveBeenLastCalledWith('medium');
    feedFbu(ws, 64, 64, 400);
    feedFbu(ws, 64, 64, 400);
    expect(client.getQualityLevel()).toBe('low'); // next round, one more tier
    expect(onQualityChange).toHaveBeenLastCalledWith('low');
  });

  it('ignores FBUs below the byte floor — slow idle frames cannot demote', () => {
    const ws = new FakeWebSocket();
    const client = makeClient({ ws: ws as unknown as WebSocket, events: {} });
    handshake(client, ws, { width: 800, height: 600 });

    // 1×1 frames (the idle-pump class) taking 400ms each: excluded by the
    // floor, so the tier must not move.
    for (let i = 0; i < 6; i++) feedFbu(ws, 1, 1, 400);
    expect(client.getQualityLevel()).toBe('high');
  });

  it('blocks RTT-driven promotion while slow evidence sits in the window', () => {
    const ws = new FakeWebSocket();
    const client = makeClient({ ws: ws as unknown as WebSocket, events: {} });
    handshake(client, ws, { width: 800, height: 600 });

    // Collapse: four slow large FBUs → low (two hysteresis rounds).
    for (let i = 0; i < 4; i++) feedFbu(ws, 64, 64, 400);
    expect(client.getQualityLevel()).toBe('low');

    // RTT looks great (the bufferbloat signature) — but the slow samples are
    // still inside the 3s window, so promotion is vetoed.
    client.feedHeartbeat(10);
    client.feedHeartbeat(10);
    client.feedHeartbeat(10);
    expect(client.getQualityLevel()).toBe('low');

    // Window drains (idle): with no service-time evidence left, the RTT
    // signal is back in sole charge and promotion proceeds.
    now += 4000;
    client.feedHeartbeat(10);
    client.feedHeartbeat(10);
    expect(client.getQualityLevel()).toBe('high');
  });

  it('promotes when RTT is good AND recent large FBUs are demonstrably fast', () => {
    const ws = new FakeWebSocket();
    const client = makeClient({ ws: ws as unknown as WebSocket, events: {} });
    handshake(client, ws, { width: 800, height: 600 });

    for (let i = 0; i < 4; i++) feedFbu(ws, 64, 64, 400);
    expect(client.getQualityLevel()).toBe('low');

    // The link recovers: large FBUs now serviced in 10ms. Once the slow
    // samples age out, the fast ones are positive evidence — promotion
    // commits via the same hysteresis without waiting for a heartbeat.
    now += 4000;
    feedFbu(ws, 64, 64, 10);
    feedFbu(ws, 64, 64, 10);
    expect(client.getQualityLevel()).toBe('high');
  });
});

// RFB-02: SetColourMapEntries (server msg type 1) is a VARIABLE-length message.
// We negotiate truecolor so the map is meaningless to us, but a server may still
// emit one — and the old default case read a single byte and "hoped for resync,"
// silently desyncing the stream (the next msgType lands mid-colour-map). The
// client must consume the full 6-byte header + nColours*6 body and stay aligned.
describe('RFBClient SetColourMapEntries (server msg type 1)', () => {
  /**
   * SetColourMapEntries (RFC 6143 §7.6.2):
   *   u8  type = 1
   *   u8  padding
   *   u16 first-colour
   *   u16 number-of-colours
   *   then number-of-colours × (u16 red, u16 green, u16 blue)
   */
  function colourMap(firstColour: number, nColours: number): Uint8Array {
    const buf = new Uint8Array(6 + nColours * 6);
    const dv = new DataView(buf.buffer);
    dv.setUint8(0, SRV_MSG_SET_COLOUR_MAP_ENTRIES);
    dv.setUint16(2, firstColour);
    dv.setUint16(4, nColours);
    // Body bytes left as zeros — the client discards them.
    return buf;
  }

  it('consumes the full header+body and stays byte-aligned (no desync)', () => {
    const ws = new FakeWebSocket();
    const onError = vi.fn();
    const onBell = vi.fn();
    const client = makeClient({
      ws: ws as unknown as WebSocket,
      events: { onError, onBell },
    });
    handshake(client, ws, { width: 800, height: 600 });
    onError.mockClear();

    // A 256-entry colour map (6 + 256*6 = 1542 bytes) followed immediately by a
    // Bell (type 2). If the colour map weren't fully consumed, the trailing
    // Bell byte would be misread and onBell would not fire (or the parser would
    // desync). The Bell firing proves the stream re-aligned exactly after the map.
    const map = colourMap(0, 256);
    const bell = new Uint8Array([2]);
    const frame = new Uint8Array(map.length + bell.length);
    frame.set(map);
    frame.set(bell, map.length);
    ws.feed(frame);

    expect(onError).not.toHaveBeenCalled();
    expect(onBell).toHaveBeenCalledTimes(1);
    expect(ws.readyState).not.toBe(FakeWebSocket.CLOSED);
  });

  it('waits for the full body (NeedMore) when it arrives split across frames', () => {
    const ws = new FakeWebSocket();
    const onBell = vi.fn();
    const client = makeClient({
      ws: ws as unknown as WebSocket,
      events: { onBell },
    });
    handshake(client, ws, { width: 800, height: 600 });

    const map = colourMap(10, 4); // 6 + 4*6 = 30 bytes
    // Deliver just the header + 1 body byte first — the client must NOT consume
    // it yet (would desync), then the remainder + a trailing Bell.
    ws.feed(map.subarray(0, 7));
    expect(onBell).not.toHaveBeenCalled();
    const rest = new Uint8Array(map.length - 7 + 1);
    rest.set(map.subarray(7));
    rest[rest.length - 1] = 2; // trailing Bell
    ws.feed(rest);
    expect(onBell).toHaveBeenCalledTimes(1);
  });
});

// RFB-02 / PROT-08: the extended ServerCutText length is the negation of the
// signed length field. A huge-negative rawLen (e.g. INT32_MIN) yields a
// ~2 GB payloadLen that `has(8 + payloadLen)` can never satisfy — the old code
// sat in NeedMore forever (a silent hang on a malformed/hostile frame). The
// client must reject anything above the proxy's 16 MB cap with onError + close.
describe('RFBClient ServerCutText (msg type 3)', () => {
  /** Legacy positive-length ServerCutText carrying `text` as UTF-8. */
  function serverCutText(text: string): Uint8Array {
    const payload = new TextEncoder().encode(text);
    const buf = new Uint8Array(8 + payload.length);
    const dv = new DataView(buf.buffer);
    dv.setUint8(0, SRV_MSG_SERVER_CUT_TEXT);
    dv.setUint32(4, payload.length);
    buf.set(payload, 8);
    return buf;
  }

  /** Extended ServerCutText with a raw (signed) length field. */
  function extendedCutText(rawLen: number): Uint8Array {
    const buf = new Uint8Array(8);
    const dv = new DataView(buf.buffer);
    dv.setUint8(0, SRV_MSG_SERVER_CUT_TEXT);
    dv.setInt32(4, rawLen);
    return buf;
  }

  it('delivers a normal positive-length payload to onClipboard', () => {
    const ws = new FakeWebSocket();
    const onClipboard = vi.fn();
    const client = makeClient({
      ws: ws as unknown as WebSocket,
      events: { onClipboard },
    });
    handshake(client, ws, { width: 800, height: 600 });

    ws.feed(serverCutText('hello clipboard'));
    expect(onClipboard).toHaveBeenCalledTimes(1);
    expect(onClipboard.mock.calls[0]![0]).toBe('hello clipboard');
    expect(ws.readyState).not.toBe(FakeWebSocket.CLOSED);
  });

  it('rejects a huge-negative extended length with onError + close (no silent hang)', () => {
    const ws = new FakeWebSocket();
    const onError = vi.fn();
    const onClipboard = vi.fn();
    const client = makeClient({
      ws: ws as unknown as WebSocket,
      events: { onError, onClipboard },
    });
    handshake(client, ws, { width: 800, height: 600 });
    onError.mockClear();

    // INT32_MIN → payloadLen ≈ 2 GB, far above the 16 MB cap. The old code
    // would loop in NeedMore forever; the fix tears the session down instead.
    ws.feed(extendedCutText(-0x80000000));

    expect(onError).toHaveBeenCalledTimes(1);
    expect((onError.mock.calls[0]![0] as Error).message).toMatch(/exceeds .* cap/);
    expect(onClipboard).not.toHaveBeenCalled();
    expect(ws.readyState).toBe(FakeWebSocket.CLOSED);
  });

  it('still NeedMore-waits for a small extended payload (valid extended path)', () => {
    const ws = new FakeWebSocket();
    const onError = vi.fn();
    const client = makeClient({
      ws: ws as unknown as WebSocket,
      events: { onError },
    });
    handshake(client, ws, { width: 800, height: 600 });
    onError.mockClear();

    // rawLen = -8 → payloadLen 8, well under the cap. The header alone (8 bytes
    // with no body yet) must NOT error or close — the parser waits (NeedMore).
    ws.feed(extendedCutText(-8));
    expect(onError).not.toHaveBeenCalled();
    expect(ws.readyState).not.toBe(FakeWebSocket.CLOSED);
    // Deliver the 8-byte extended body; still no error, no clipboard event
    // (extended payloads are handled by the proxy, ignored here), stream stays open.
    ws.feed(new Uint8Array(8));
    expect(onError).not.toHaveBeenCalled();
    expect(ws.readyState).not.toBe(FakeWebSocket.CLOSED);
  });
});

// Protocol flow control (backpressure): the next FramebufferUpdateRequest must be
// issued ONLY after all rects in the current frame are fully decoded. Pipelining
// at the header causes upstream socket bufferbloat and massive input latency on
// constrained links.
describe('RFBClient frame pump (flow control / backpressure)', () => {
  it('requests the next frame only after decoding all rects, not at the FBU header', () => {
    const ws = new FakeWebSocket();
    const client = makeClient({ ws: ws as unknown as WebSocket, events: {} });
    handshake(client, ws, { width: 800, height: 600 });
    ws.sent.length = 0; // drop the post-init bootstrap frames

    // Feed ONLY a FramebufferUpdate header declaring 1 rect — no rect body yet,
    // so the FBU cannot complete.
    const hdr = new Uint8Array(4);
    hdr[0] = 0; // SRV_MSG_FRAMEBUFFER_UPDATE
    new DataView(hdr.buffer).setUint16(2, 1); // nRects = 1
    ws.feed(hdr);

    // Because the rect isn't decoded yet, no request should be in flight.
    const reqsBefore = ws.sent.filter(
      (f) => f.length >= 2 && f[0] === CLI_MSG_FRAMEBUFFER_UPDATE_REQUEST,
    );
    expect(reqsBefore.length).toBe(0);

    // Feed a complete RAW rect (12 byte rect header + 4 byte body for a 1x1 pixel).
    // This completes the FBU, which should trigger the pump.
    const rect = new Uint8Array(16);
    const dv = new DataView(rect.buffer);
    dv.setUint16(0, 0); // x
    dv.setUint16(2, 0); // y
    dv.setUint16(4, 1); // w
    dv.setUint16(6, 1); // h
    dv.setInt32(8, 0); // ENC_RAW
    rect[12] = 255;
    rect[13] = 0;
    rect[14] = 0;
    rect[15] = 255;
    ws.feed(rect);

    const reqsAfter = ws.sent.filter(
      (f) => f.length >= 2 && f[0] === CLI_MSG_FRAMEBUFFER_UPDATE_REQUEST,
    );
    expect(reqsAfter.length).toBe(1);
    expect(reqsAfter[0]![1]).toBe(1); // incremental
  });

  it('a zero-rect FBU immediately primes the pump since decode is synchronous', () => {
    const ws = new FakeWebSocket();
    const client = makeClient({ ws: ws as unknown as WebSocket, events: {} });
    handshake(client, ws, { width: 800, height: 600 });
    ws.sent.length = 0;
    const hdr = new Uint8Array(4); // nRects = 0
    hdr[0] = 0;
    ws.feed(hdr);
    expect(ws.sent.some((f) => f.length >= 1 && f[0] === CLI_MSG_FRAMEBUFFER_UPDATE_REQUEST)).toBe(
      true,
    );
  });
});

// The drain loop yields between server messages when Chromium's
// navigator.scheduling.isInputPending reports queued operator input, and the
// continuation rides a reusable MessageChannel instead of setTimeout(0).
// onUpdate (fired per completed FBU carrying rects) is the observable here:
// unlike update-request counting it cannot be polluted by the 50ms PRP pump.
describe('RFBClient drain yielding (input-aware MessageChannel continuation)', () => {
  /** FBU with one complete 1×1 RAW rect — completes synchronously, fires onUpdate. */
  function rawRectFbu(): Uint8Array {
    const buf = new Uint8Array(4 + 12 + 4);
    const dv = new DataView(buf.buffer);
    dv.setUint8(0, 0); // FramebufferUpdate
    dv.setUint16(2, 1); // nRects
    dv.setUint16(8, 1); // w
    dv.setUint16(10, 1); // h
    dv.setInt32(12, ENC_RAW);
    buf[16] = 255;
    buf[19] = 255;
    return buf;
  }

  /**
   * FBU with one in-bounds Tight rect whose control byte declares the illegal
   * mode 0x0A — decodeTight THROWS on it, exercising the continuation's
   * try/catch → protocolReset routing (unknown-encoding rects reset without
   * throwing, so they wouldn't cover that path).
   */
  function throwingRectFbu(): Uint8Array {
    const buf = new Uint8Array(4 + 12 + 1);
    const dv = new DataView(buf.buffer);
    dv.setUint8(0, 0);
    dv.setUint16(2, 1);
    dv.setUint16(8, 16);
    dv.setUint16(10, 16);
    dv.setInt32(12, ENC_TIGHT);
    buf[16] = 0xa0; // Tight ctl: mode 0x0A = tight-png / illegal → throw
    return buf;
  }

  function concat(...parts: Uint8Array[]): Uint8Array {
    const out = new Uint8Array(parts.reduce((s, p) => s + p.length, 0));
    let off = 0;
    for (const p of parts) {
      out.set(p, off);
      off += p.length;
    }
    return out;
  }

  const sleep = (ms: number) => new Promise((r) => setTimeout(r, ms));

  beforeEach(() => {
    vi.stubGlobal('navigator', { scheduling: { isInputPending: () => true } });
  });
  afterEach(() => {
    vi.unstubAllGlobals();
    vi.restoreAllMocks();
  });

  it('yields on pending input mid-buffer and finishes decoding on the continuation', async () => {
    const ws = new FakeWebSocket();
    const onUpdate = vi.fn();
    const client = makeClient({ ws: ws as unknown as WebSocket, events: { onUpdate } });
    handshake(client, ws);

    // Two complete FBUs in ONE WebSocket frame. With input pending, the
    // drain yields after every processed message, so nothing completes
    // synchronously — both FBUs must land via scheduled continuations.
    ws.feed(concat(rawRectFbu(), rawRectFbu()));
    expect(onUpdate).toHaveBeenCalledTimes(0);

    await sleep(20);
    expect(onUpdate).toHaveBeenCalledTimes(2);
  });

  it('behaves exactly as before when the isInputPending API is absent', () => {
    vi.stubGlobal('navigator', {}); // no scheduling API
    const ws = new FakeWebSocket();
    const onUpdate = vi.fn();
    const client = makeClient({ ws: ws as unknown as WebSocket, events: { onUpdate } });
    handshake(client, ws);

    // Same double-FBU frame decodes in a single synchronous pass.
    ws.feed(concat(rawRectFbu(), rawRectFbu()));
    expect(onUpdate).toHaveBeenCalledTimes(2);
  });

  it('routes a desync inside the continuation to protocolReset and halts further draining', async () => {
    const ws = new FakeWebSocket();
    const onUpdate = vi.fn();
    const onError = vi.fn();
    const onProtocolReset = vi.fn();
    const client = makeClient({
      ws: ws as unknown as WebSocket,
      events: { onUpdate, onError, onProtocolReset },
    });
    handshake(client, ws);

    // Good FBU, then a throwing rect, then a trailing good FBU that must
    // never be parsed once the reset halts the client.
    ws.feed(concat(rawRectFbu(), throwingRectFbu(), rawRectFbu()));
    await sleep(30);

    expect(onUpdate).toHaveBeenCalledTimes(1);
    expect(onProtocolReset).toHaveBeenCalledTimes(1);
    expect(onError).not.toHaveBeenCalled();
  });

  it('dispose() closes both MessageChannel ports and drops the pending continuation', async () => {
    const closeSpy = vi.spyOn(MessagePort.prototype, 'close');
    const ws = new FakeWebSocket();
    const onUpdate = vi.fn();
    const client = makeClient({ ws: ws as unknown as WebSocket, events: { onUpdate } });
    handshake(client, ws);

    ws.feed(concat(rawRectFbu(), rawRectFbu()));
    expect(onUpdate).toHaveBeenCalledTimes(0); // continuation pending

    client.dispose();
    expect(closeSpy.mock.calls.length).toBeGreaterThanOrEqual(2); // both ports

    await sleep(20);
    expect(onUpdate).toHaveBeenCalledTimes(0); // continuation never delivered
  });
});
