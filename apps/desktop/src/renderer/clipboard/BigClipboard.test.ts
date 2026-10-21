import { describe, it, expect } from 'vitest';

import {
  BigClipboardSender,
  BigClipboardReceiver,
  BLOB_FRAME_PREFIX,
  BLOB_ACK_PREFIX,
  MIN_CHUNK_BYTES,
  MAX_CHUNK_BYTES,
  crc32Hex,
  frameChunkBytes,
} from './BigClipboard';

interface ParsedFrame {
  xferId: string;
  seq: number;
  total: number;
  rawLen: number;
  crc: string;
  b64: string;
}

function parseFrame(text: string): ParsedFrame {
  expect(text.startsWith(BLOB_FRAME_PREFIX)).toBe(true);
  const [xferId, seq, total, rawLen, crc, b64] = text
    .slice(BLOB_FRAME_PREFIX.length)
    .split(':', 6) as [string, string, string, string, string, string];
  return { xferId, seq: +seq, total: +total, rawLen: +rawLen, crc, b64 };
}

const ack = (f: ParsedFrame, status: string) => `${BLOB_ACK_PREFIX}${f.xferId}:${f.seq}:${status}`;

/** A well-behaved fake agent: acks every frame, collects the chunks. */
function autoAcker(collect: ParsedFrame[]) {
  const sender: BigClipboardSender = new BigClipboardSender(
    (text) => {
      const f = parseFrame(text);
      collect.push(f);
      // Ack asynchronously, like the real serial round-trip.
      queueMicrotask(() => sender.onAckText(ack(f, f.seq === f.total ? 'done' : 'ok')));
    },
    { ackTimeoutMs: 250, maxRetries: 2 },
  );
  return sender;
}

async function gunzip(bytes: Uint8Array): Promise<string> {
  const stream = new Blob([bytes as BlobPart])
    .stream()
    .pipeThrough(new DecompressionStream('gzip'));
  return await new Response(stream).text();
}

describe('frameChunkBytes (negotiated sizing)', () => {
  it('falls back to the safe floor when the guest reports nothing usable', () => {
    // An agent too old to answer HELLO, or a nonsense value, must never widen
    // frames past what every guest is known to absorb.
    for (const bad of [0, -1, NaN, Infinity]) {
      expect(frameChunkBytes(bad)).toBe(MIN_CHUNK_BYTES);
    }
  });

  it('never produces a frame larger than the capacity the guest reported', () => {
    // THE regression guard for the ~4 KB wall: whatever the guest says it can
    // absorb, the frame text we actually emit (prefix + base64) must fit inside
    // it. A frame past this is silently dropped and the paste never lands.
    for (const rx of [4096, 8192, 64 * 1024, 256 * 1024, 1024 * 1024]) {
      const chunk = frameChunkBytes(rx);
      const frameText = Math.ceil(chunk / 3) * 4 + 64; // base64 ×4/3 + prefix
      expect(frameText).toBeLessThanOrEqual(rx);
    }
  });

  it('grows with capacity, clamped between the floor and the ceiling', () => {
    // The historical wall: still a real improvement over the 2 KB floor.
    expect(frameChunkBytes(4096)).toBeGreaterThan(MIN_CHUNK_BYTES);
    // A guest whose driver granted a big queue gets big frames, but clamped.
    expect(frameChunkBytes(64 * 1024 * 1024)).toBe(MAX_CHUNK_BYTES);
    expect(frameChunkBytes(64 * 1024)).toBeGreaterThan(frameChunkBytes(8 * 1024));
  });
});

describe('BigClipboard sender', () => {
  it('crc32 matches the IEEE reference vector (Go hash/crc32 compatible)', () => {
    expect(crc32Hex(new TextEncoder().encode('123456789'))).toBe('cbf43926');
  });

  it('honours a negotiated frame size, re-read per transfer', () => {
    const frames: ParsedFrame[] = [];
    let negotiated = MIN_CHUNK_BYTES;
    const sender: BigClipboardSender = new BigClipboardSender(
      (text) => {
        const f = parseFrame(text);
        frames.push(f);
        queueMicrotask(() => sender.onAckText(ack(f, f.seq === f.total ? 'done' : 'ok')));
      },
      { ackTimeoutMs: 250, maxRetries: 1, chunkBytes: () => negotiated },
    );
    // Same payload, bigger negotiated frame → strictly fewer frames (fewer RTTs).
    // Must be poorly compressible: a repeated string gzips to a single frame at
    // any size and would make this assertion vacuous.
    return (async () => {
      const text = Array.from({ length: 60_000 }, (_, i) =>
        ((i * 2654435761) >>> 0).toString(36),
      ).join(' ');
      await sender.submit(text);
      const atFloor = frames.length;
      frames.length = 0;
      negotiated = 64 * 1024;
      await sender.submit(text);
      expect(frames.length).toBeLessThan(atFloor);
    })();
  });

  it('round-trips a large payload through frames (gzip + per-chunk base64)', async () => {
    const text = 'kind: Deployment\nmetadata:\n  name: web\n'.repeat(30_000); // ~1.2 MB
    const frames: ParsedFrame[] = [];
    const sender = autoAcker(frames);

    const res = await sender.submit(text);
    expect(res.ok).toBe(true);
    expect(frames.length).toBeGreaterThan(0);

    // Frames are self-consistent: sequential, single transfer, valid CRCs.
    const enc = new TextEncoder();
    frames.forEach((f, i) => {
      expect(f.seq).toBe(i + 1);
      expect(f.total).toBe(frames.length);
      expect(f.xferId).toBe(frames[0]!.xferId);
      expect(f.rawLen).toBe(enc.encode(text).byteLength);
      expect(crc32Hex(enc.encode(f.b64))).toBe(f.crc);
    });

    // Reassemble exactly as the agent does: per-frame base64 → concat → gunzip.
    const chunks = frames.map((f) => Uint8Array.from(atob(f.b64), (c) => c.charCodeAt(0)));
    const compressed = new Uint8Array(chunks.reduce((n, c) => n + c.length, 0));
    let off = 0;
    for (const c of chunks) {
      compressed.set(c, off);
      off += c.length;
    }
    expect(await gunzip(compressed)).toBe(text);
  });

  it('retries on err:crc, then succeeds', async () => {
    const seen: number[] = [];
    let corruptedOnce = false;
    const sender: BigClipboardSender = new BigClipboardSender(
      (text) => {
        const f = parseFrame(text);
        seen.push(f.seq);
        const status =
          f.seq === 1 && !corruptedOnce
            ? ((corruptedOnce = true), 'err:crc')
            : f.seq === f.total
              ? 'done'
              : 'ok';
        queueMicrotask(() => sender.onAckText(ack(f, status)));
      },
      { ackTimeoutMs: 250, maxRetries: 2 },
    );
    const res = await sender.submit('z'.repeat(200_000));
    expect(res.ok).toBe(true);
    // Frame 1 was sent twice (initial + retry after err:crc).
    expect(seen.filter((s) => s === 1).length).toBe(2);
  });

  it('retransmits on ack timeout and fails after retries run out', async () => {
    let sends = 0;
    const sender = new BigClipboardSender(
      () => {
        sends++; // never ack
      },
      { ackTimeoutMs: 20, maxRetries: 2 },
    );
    const res = await sender.submit('payload');
    expect(res.ok).toBe(false);
    expect(res.error).toBe('timeout');
    expect(sends).toBe(3); // initial + 2 retries
  });

  it('a fatal agent error aborts without retrying', async () => {
    let sends = 0;
    const sender: BigClipboardSender = new BigClipboardSender(
      (text) => {
        sends++;
        const f = parseFrame(text);
        queueMicrotask(() => sender.onAckText(ack(f, 'err:too_large')));
      },
      { ackTimeoutMs: 250, maxRetries: 3 },
    );
    const res = await sender.submit('q'.repeat(100_000));
    expect(res.ok).toBe(false);
    expect(res.error).toBe('err:too_large');
    expect(sends).toBe(1);
  });

  it('a second submit supersedes the first (latest copy wins)', async () => {
    let release: (() => void) | null = null;
    const sender: BigClipboardSender = new BigClipboardSender(
      (text) => {
        const f = parseFrame(text);
        // Hold the first transfer's ack until the second submit lands.
        if (!release) {
          release = () => sender.onAckText(ack(f, f.seq === f.total ? 'done' : 'ok'));
          return;
        }
        queueMicrotask(() => sender.onAckText(ack(f, f.seq === f.total ? 'done' : 'ok')));
      },
      { ackTimeoutMs: 500, maxRetries: 1 },
    );
    const first = sender.submit('first payload');
    const second = sender.submit('second payload');
    const [r1, r2] = await Promise.all([first, second]);
    expect(r1.ok).toBe(false);
    expect(r1.superseded).toBe(true);
    expect(r2.ok).toBe(true);
  });

  it('consumes ack-prefixed text and ignores foreign/stale acks', () => {
    const sender = new BigClipboardSender(() => undefined);
    expect(sender.onAckText(`${BLOB_ACK_PREFIX}deadbeef:3:ok`)).toBe(true);
    expect(sender.onAckText('not an ack')).toBe(false);
  });
});

// ── Reverse direction: the guest→host receiver ────────────────────────────
async function gzipBytes(data: Uint8Array): Promise<Uint8Array> {
  const s = new Blob([data as BlobPart]).stream().pipeThrough(new CompressionStream('gzip'));
  return new Uint8Array(await new Response(s).arrayBuffer());
}

function b64FromBytes(bytes: Uint8Array): string {
  let bin = '';
  const SLICE = 0x8000;
  for (let i = 0; i < bytes.length; i += SLICE) {
    bin += String.fromCharCode(...bytes.subarray(i, i + SLICE));
  }
  return btoa(bin);
}

/** Build frames exactly as the agent's reverse sender (bigclip_tx.go) does. */
async function buildAgentFrames(
  xferId: string,
  text: string,
  chunkSize: number,
): Promise<string[]> {
  const raw = new TextEncoder().encode(text);
  const compressed = await gzipBytes(raw);
  const total = Math.max(1, Math.ceil(compressed.byteLength / chunkSize));
  const frames: string[] = [];
  for (let seq = 1; seq <= total; seq++) {
    const b64 = b64FromBytes(
      compressed.subarray((seq - 1) * chunkSize, Math.min(seq * chunkSize, compressed.byteLength)),
    );
    const crc = crc32Hex(new TextEncoder().encode(b64));
    frames.push(`${BLOB_FRAME_PREFIX}${xferId}:${seq}:${total}:${raw.byteLength}:${crc}:${b64}`);
  }
  return frames;
}

function ackStatus(ackText: string): { xferId: string; seq: number; status: string } {
  const [xferId, seq, ...rest] = ackText.slice(BLOB_ACK_PREFIX.length).split(':');
  return { xferId: xferId!, seq: +seq!, status: rest.join(':') };
}

function makeReceiver(maxBytes = 10 * 1024 * 1024) {
  const acks: string[] = [];
  const state = { delivered: null as string | null, deliverCalls: 0, failNext: false };
  const r = new BigClipboardReceiver(
    maxBytes,
    (text: string): Promise<void> => {
      state.deliverCalls++;
      if (state.failNext) {
        state.failNext = false;
        return Promise.reject(new Error('host write failed'));
      }
      state.delivered = text;
      return Promise.resolve();
    },
    (a: string) => acks.push(a),
  );
  return { r, acks, state };
}

describe('BigClipboard receiver (guest→host)', () => {
  it('reassembles a multi-frame transfer and acks ok…ok,done', async () => {
    const text = Array.from(
      { length: 3000 },
      (_, i) => `log ${i}: ${(i * 2654435761) & 0xffff}`,
    ).join('\n');
    const frames = await buildAgentFrames('aabbccdd', text, 256);
    expect(frames.length).toBeGreaterThan(2);

    const { r, acks, state } = makeReceiver();
    for (const f of frames) await r.ingest(f);

    expect(state.delivered).toBe(text);
    expect(state.deliverCalls).toBe(1);
    expect(acks.map((a) => ackStatus(a).status)).toEqual([
      ...frames.slice(0, -1).map(() => 'ok'),
      'done',
    ]);
  });

  it('re-acks done for a duplicate final frame without re-delivering', async () => {
    const frames = await buildAgentFrames('beefbeef', 'small guest copy', 4096);
    expect(frames.length).toBe(1);
    const { r, acks, state } = makeReceiver();

    await r.ingest(frames[0]!);
    expect(state.deliverCalls).toBe(1);
    // The done ack was lost; the agent retransmits — re-ack done, no re-deliver.
    await r.ingest(frames[0]!);
    expect(state.deliverCalls).toBe(1);
    expect(acks.map((a) => ackStatus(a).status)).toEqual(['done', 'done']);
  });

  it('re-acks a duplicate consumed frame and drops out-of-order frames', async () => {
    const text = 'x'.repeat(30_000);
    const frames = await buildAgentFrames('00112233', text, 16);
    expect(frames.length).toBeGreaterThan(3);
    const { r, acks, state } = makeReceiver();

    await r.ingest(frames[0]!);
    await r.ingest(frames[0]!); // dup of consumed → re-ack ok
    expect(acks.map((a) => ackStatus(a).status)).toEqual(['ok', 'ok']);
    const before = acks.length;
    await r.ingest(frames[3]!); // out of order → silent drop, no ack
    expect(acks.length).toBe(before);

    for (let i = 1; i < frames.length; i++) await r.ingest(frames[i]!);
    expect(state.delivered).toBe(text);
  });

  it('acks err:crc on corruption, then recovers on retransmit', async () => {
    const frames = await buildAgentFrames('0a0b0c0d', 'y'.repeat(5000), 16);
    const { r, acks, state } = makeReceiver();

    const corrupt = frames[0]!.slice(0, -2) + 'zz'; // same declared crc → mismatch
    await r.ingest(corrupt);
    expect(ackStatus(acks[acks.length - 1]!).status).toBe('err:crc');

    for (const f of frames) await r.ingest(f);
    expect(state.delivered).toBe('y'.repeat(5000));
  });

  it('rejects a payload whose declared rawLen exceeds the cap', async () => {
    const frames = await buildAgentFrames('cafebabe', 'a'.repeat(500), 4096);
    const { r, acks } = makeReceiver(100); // cap below rawLen (500)
    await r.ingest(frames[0]!);
    expect(ackStatus(acks[acks.length - 1]!).status).toBe('err:too_large');
  });

  it('acks err:decode when the declared length disagrees with the payload', async () => {
    const [frame] = await buildAgentFrames('d00dd00d', 'consistent text', 4096);
    // Bump rawLen only; crc is over b64 so it still validates → decode length check trips.
    const parts = frame!.slice(BLOB_FRAME_PREFIX.length).split(':');
    parts[3] = String(Number(parts[3]) + 5);
    const { r, acks } = makeReceiver();
    await r.ingest(BLOB_FRAME_PREFIX + parts.join(':'));
    expect(ackStatus(acks[acks.length - 1]!).status).toBe('err:decode');
  });

  it('acks err:write when the host clipboard write fails', async () => {
    const [frame] = await buildAgentFrames('feedface', 'lands nowhere', 4096);
    const { r, acks, state } = makeReceiver();
    state.failNext = true;
    await r.ingest(frame!);
    expect(ackStatus(acks[acks.length - 1]!).status).toBe('err:write');
    expect(state.delivered).toBeNull();
  });

  it('asks the sender to restart when it joins a transfer mid-stream (post-reconnect)', async () => {
    const text = 'y'.repeat(20_000);
    const frames = await buildAgentFrames('ab12cd34', text, 16);
    expect(frames.length).toBeGreaterThan(3);
    const { r, acks, state } = makeReceiver();

    // A reconnect reset our state; the agent is mid-transfer at seq 3.
    await r.ingest(frames[2]!);
    expect(ackStatus(acks[acks.length - 1]!)).toMatchObject({ seq: 3, status: 'err:restart' });
    expect(state.deliverCalls).toBe(0);

    // The agent rewinds to seq 1; the transfer now reassembles cleanly.
    for (const f of frames) await r.ingest(f);
    expect(state.delivered).toBe(text);
  });
});
