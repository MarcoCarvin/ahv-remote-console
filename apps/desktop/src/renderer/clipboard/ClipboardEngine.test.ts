import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';

import { createClipboardEngine, type ClipboardEngine } from './ClipboardEngine';
import { HELLO_PREFIX, HELLOACK_PREFIX, BLOB_FRAME_PREFIX, BLOB_ACK_PREFIX } from './BigClipboard';

// These tests exercise the opt-in reliable large-paste path (reprobeCapacity):
// a paste too big for the direct path first probes the guest's receive capacity
// (HELLO handshake) so the sender uses a FEW BIG frames instead of hundreds of
// tiny ones. The small/direct path must be completely untouched.

interface ParsedFrame {
  xferId: string;
  seq: number;
  total: number;
}
function parseFrame(text: string): ParsedFrame {
  const [xferId, seq, total] = text.slice(BLOB_FRAME_PREFIX.length).split(':', 6);
  return { xferId: xferId!, seq: Number(seq), total: Number(total) };
}
const mkAck = (f: ParsedFrame, status: string) =>
  `${BLOB_ACK_PREFIX}${f.xferId}:${f.seq}:${status}`;

// Poorly compressible payload so the frame COUNT reflects the frame SIZE — a
// repeated string gzips to a single frame at any size and makes the count
// comparison vacuous (same trick as BigClipboard.test.ts).
function incompressible(words: number): string {
  return Array.from({ length: words }, (_, i) => ((i * 2654435761) >>> 0).toString(36)).join(' ');
}

describe('ClipboardEngine — large-paste capacity probe (reprobeCapacity)', () => {
  let sent: string[];
  let changeCb: ((e: { text: string; seq: number }) => void) | null;
  // When armed, the fake guest answers a HELLO with a 1 MB capacity. Left OFF
  // for the startup probe so the sender starts at the floor — reproducing the
  // field bug where the startup HELLOACK is lost and only the lazy pre-paste
  // probe rescues the frame size.
  let answerHello: boolean;

  beforeEach(() => {
    sent = [];
    changeCb = null;
    answerHello = false;
    vi.stubGlobal('window', {
      ahvRemoteConsole: {
        clipboardRead: vi.fn().mockResolvedValue({ seq: 0, text: '' }),
        onClipboardChanged: (cb: (e: { text: string; seq: number }) => void) => {
          changeCb = cb;
          return vi.fn(); // unsubscribe (unused)
        },
        clipboardWrite: vi.fn().mockResolvedValue({ ok: true }),
        hcLog: vi.fn(),
      },
    });
  });
  afterEach(() => {
    vi.unstubAllGlobals();
  });

  function makeEngine(reprobe: boolean): ClipboardEngine {
    const e = createClipboardEngine({
      sessionId: () => 's',
      reprobeCapacity: () => reprobe,
      forwardHostToVM: (text: string) => {
        sent.push(text);
        if (text === HELLO_PREFIX) {
          if (answerHello) {
            queueMicrotask(
              () => void e.handleGuestClipboard(`${HELLOACK_PREFIX}1.0:linux:${1024 * 1024}`),
            );
          }
          return;
        }
        if (text.startsWith(BLOB_FRAME_PREFIX)) {
          const f = parseFrame(text);
          queueMicrotask(
            () => void e.handleGuestClipboard(mkAck(f, f.seq === f.total ? 'done' : 'ok')),
          );
        }
      },
    });
    return e;
  }

  // Drive the fire-and-forget forwardHostToVM chain to completion. It awaits
  // sha256 + gzip (which resolve on MACROTASKS, so microtask flushing alone is
  // not enough) and then stop-and-wait frames (acked via queueMicrotask). Each
  // setTimeout(0) turn drains all microtasks and advances one macrotask; loop
  // until output stops growing. The probe's 2 s / BCP's 10 s timers never fire
  // here because the HELLOACK and frame ACKs are fed first.
  async function settle(): Promise<void> {
    // Fixed macrotask turns (not early-exit-on-stable): the probe→gzip gap leaves
    // `sent` briefly stable mid-transfer, so a stability check bails too early.
    for (let i = 0; i < 40; i++) await new Promise((r) => setTimeout(r, 0));
  }

  async function paste(text: string, seq: number): Promise<void> {
    if (!changeCb) throw new Error('startHostPoll was not called');
    changeCb({ text, seq });
    await settle();
  }

  const helloCount = () => sent.filter((t) => t === HELLO_PREFIX).length;
  const frames = () => sent.filter((t) => t.startsWith(BLOB_FRAME_PREFIX)).map(parseFrame);

  it('flag ON: probes capacity before a large paste, then uses big frames (fewer)', async () => {
    const e = makeEngine(true);
    e.startHostPoll();
    await settle();
    expect(helloCount()).toBe(1); // startup probe only, and it went unanswered

    answerHello = true; // the guest will now answer the lazy pre-paste probe
    const text = incompressible(15_000);
    await paste(text, 1);

    // The paste fired a fresh HELLO probe (beyond the startup one)...
    expect(helloCount()).toBeGreaterThanOrEqual(2);
    const bigFrames = frames();
    expect(bigFrames.length).toBeGreaterThan(0);
    const bigTotal = bigFrames[0]!.total;

    // ...and the same payload with the flag OFF stays at the floor → many frames.
    sent = [];
    answerHello = false;
    const off = makeEngine(false);
    off.startHostPoll();
    await settle();
    await paste(text, 2);
    expect(helloCount()).toBe(1); // startup only — NO pre-paste probe
    const floorFrames = frames();
    expect(floorFrames.length).toBeGreaterThan(0);
    const floorTotal = floorFrames[0]!.total;

    // Big frames ⇒ strictly fewer frames than the floor for the SAME payload.
    expect(bigTotal).toBeLessThan(floorTotal);
  });

  it('flag OFF: a large paste never sends a probe HELLO', async () => {
    const e = makeEngine(false);
    e.startHostPoll();
    await settle();
    const before = helloCount(); // 1 (startup)
    await paste(incompressible(8_000), 1);
    expect(helloCount()).toBe(before); // no extra HELLO from the paste
    expect(frames().length).toBeGreaterThan(0); // it did go through BCP
  });

  it('small pastes never probe and never chunk (untouched direct path)', async () => {
    const e = makeEngine(true);
    answerHello = true;
    e.startHostPoll();
    await settle();
    const before = sent.length;
    await paste('a short snippet', 1); // < DIRECT_MAX_BYTES
    expect(sent.slice(before)).toEqual(['a short snippet']); // one direct ClientCutText
  });

  it('probes at most once: a later large paste after the ack sends no new HELLO', async () => {
    const e = makeEngine(true);
    answerHello = true; // startup probe is answered → capacity known up front
    e.startHostPoll();
    await settle();
    const afterStartup = helloCount();
    await paste(incompressible(9_000), 1);
    await paste(incompressible(11_000), 2); // distinct payload (not loop-suppressed)
    expect(helloCount()).toBe(afterStartup); // no further probes once acked
  });

  it('never-answering guest: probes at most once — no recurring 2s stall', async () => {
    const e = makeEngine(true);
    answerHello = false; // legacy guest / dead ACK rail: HELLO is never answered
    e.startHostPoll();
    await settle();
    const afterStartup = helloCount(); // 1 (unanswered startup HELLO)

    // First large paste probes once, waits out the 2 s HELLO timeout, then
    // submits at the floor. (The only intentionally slow test — it exercises the
    // real timeout path so the probe-once latch is genuinely verified.)
    changeCb!({ text: incompressible(9_000), seq: 1 });
    await new Promise((r) => setTimeout(r, 2200)); // > HELLO_PROBE_WAIT_MS (2000)
    await settle();
    expect(helloCount()).toBe(afterStartup + 1); // exactly one probe HELLO
    expect(frames().length).toBeGreaterThan(0); // and it still went through BCP

    // A later large paste must NOT re-probe (probePromise is cached) — this is
    // the fix for the recurring-stall regression.
    await paste(incompressible(11_000), 2);
    expect(helloCount()).toBe(afterStartup + 1); // still just the one probe
  }, 8000);
});
