/**
 * Big-Clipboard Protocol (BCP) — host→guest sender.
 *
 * Why: a single large clipboard message cannot survive the path. Two distinct
 * limits shape this design; do not conflate them.
 *
 * 1. QEMU's real caps, which we cannot change but which rarely bite: the VNC
 *    server DISCONNECTS the console for any ClientCutText over 1 MiB (ui/vnc.c
 *    "exceeds our limit of 1MB"), and qemu-vdagent's 1 MiB outbuf toward the
 *    guest serial drops whole messages when full.
 *
 * 2. The GUEST's serial receive capacity, which is what actually binds — and is
 *    ours. Host→guest was capped near 4 KB for months (a hard wall at 4096 over
 *    11.5k real transfers). That was NOT a QEMU limit: the agent never called
 *    SetupComm, so the Windows driver's receive queue sat at its ~4 KB default;
 *    Linux's line discipline buffers 4096; neither OS has flow control here; and
 *    the agent read the tty on the same goroutine that blocks for seconds writing
 *    the guest clipboard. Bytes past the buffer were silently discarded, and
 *    nothing called ClearCommError/TIOCGICOUNT, so the drops were invisible.
 *    Believing that wall was "upstream of us" is why the earlier 24 KB and 256 KB
 *    frame sizes were reverted. See the agent's serial_windows.go / serial_drain.go.
 *
 * So frames are sized to the capacity the GUEST reports it can absorb — measured,
 * not declared (see hello.go and frameChunkBytes below) — and the payload is
 * re-shaped to fit: gzip(text) split into base64 chunks, each sent as an ordinary
 * clipboard text frame, stop-and-wait ACK-paced so QEMU's outbuf never holds more
 * than one frame.
 *
 * Stop-and-wait is REQUIRED, not an optimization: QEMU's clipboard is
 * last-writer-wins (no queue), so a second in-flight frame would replace the
 * first before the guest agent requested its data.
 *
 * Wire grammar (keep in lockstep with
 * apps/ahv-remote-console-agent/internal/vdagent/bigclip.go):
 *
 *   frame: __AHVRC_BLOB__v1:<xferId>:<seq>:<total>:<rawLen>:<crc32>:<b64>
 *   ack:   __AHVRC_BLOBACK__v1:<xferId>:<seq>:<status>
 *   hello: __AHVRC_HELLO__v1:  →  __AHVRC_HELLOACK__v1:<ver>:<os>:<rxCapacity>
 *
 * seq is 1-based; rawLen is the UNCOMPRESSED UTF-8 byte length; crc32 is
 * IEEE lowercase hex over the b64 field's bytes; each b64 field is a
 * standalone base64 (std alphabet) of one slice of the gzip stream. status:
 * ok | done | err:crc (retryable) | err:too_large | err:decode | err:write
 * (fatal) | err:restart (rewind to seq 1). Frames/acks never touch a real
 * clipboard on either side.
 */

export const BLOB_FRAME_PREFIX = '__AHVRC_BLOB__v1:';
export const BLOB_ACK_PREFIX = '__AHVRC_BLOBACK__v1:';
export const HELLO_PREFIX = '__AHVRC_HELLO__v1:';
export const HELLOACK_PREFIX = '__AHVRC_HELLOACK__v1:';

/**
 * Conservative frame size used until the guest reports its capacity (and the
 * floor we demote to if big frames turn out not to land). Chosen to fit under
 * the historical ~4 KB receive ceiling on ANY guest, including one whose agent
 * predates the negotiation entirely.
 */
export const MIN_CHUNK_BYTES = 2 * 1024;

/**
 * Beyond this the round-trip saving is noise, and QEMU's real 1 MiB caps
 * (cut-text session-kill, vdagent outbuf) start to loom.
 */
export const MAX_CHUNK_BYTES = 256 * 1024;

/**
 * Compressed bytes per frame, derived from the guest's MEASURED receive
 * capacity (see hello.go — Windows reports the queue the driver granted, Linux
 * its drain ring).
 *
 * The frame that must fit is the base64 text plus our prefix, carried inside a
 * VDAgentMessage that QEMU splits into 1024-byte VDI chunks — so budget for all
 * three before converting back through base64's ×4/3 expansion. A guest that
 * reports nothing (old ISO) or a clamped queue lands on MIN_CHUNK_BYTES, which
 * is exactly today's shipping behaviour.
 */
export function frameChunkBytes(rxCapacity: number): number {
  if (!Number.isFinite(rxCapacity) || rxCapacity <= 0) return MIN_CHUNK_BYTES;
  const wire = Math.floor(rxCapacity * 0.9); // headroom for the guest's own bookkeeping
  const vdiOverhead = 8 * Math.ceil(wire / 1024); // one VDI chunk header per 1024 B
  const budget = wire - 32 /* VDAgentMessage + selection + type */ - vdiOverhead - 64; /* prefix */
  const chunk = Math.floor((budget * 3) / 4); // base64 expands ×4/3
  return Math.max(MIN_CHUNK_BYTES, Math.min(MAX_CHUNK_BYTES, chunk));
}

export interface BcpResult {
  ok: boolean;
  /** Uncompressed UTF-8 byte length of the submitted text. */
  rawLen: number;
  /** Failure cause when !ok (ack error token, 'timeout', or a cancel reason). */
  error?: string;
  /** True when a newer submit/cancel displaced this transfer — not a failure. */
  superseded?: boolean;
}

/** IEEE CRC-32 (same polynomial/table as Go's hash/crc32 ChecksumIEEE). */
const CRC_TABLE = (() => {
  const t = new Uint32Array(256);
  for (let n = 0; n < 256; n++) {
    let c = n;
    for (let k = 0; k < 8; k++) c = c & 1 ? 0xedb88320 ^ (c >>> 1) : c >>> 1;
    t[n] = c >>> 0;
  }
  return t;
})();

export function crc32Hex(bytes: Uint8Array): string {
  let c = 0xffffffff;
  for (const b of bytes) {
    c = CRC_TABLE[(c ^ b) & 0xff]! ^ (c >>> 8);
  }
  return ((c ^ 0xffffffff) >>> 0).toString(16).padStart(8, '0');
}

async function gzipBytes(data: Uint8Array): Promise<Uint8Array> {
  const stream = new Blob([data as BlobPart]).stream().pipeThrough(new CompressionStream('gzip'));
  return new Uint8Array(await new Response(stream).arrayBuffer());
}

function base64FromBytes(bytes: Uint8Array): string {
  // btoa takes a binary string; build it in slices to keep the argument list
  // bounded (String.fromCharCode(...huge) blows the call stack).
  let bin = '';
  const SLICE = 0x8000;
  for (let i = 0; i < bytes.length; i += SLICE) {
    bin += String.fromCharCode(...bytes.subarray(i, i + SLICE));
  }
  return btoa(bin);
}

function randHex8(): string {
  const b = new Uint8Array(4);
  crypto.getRandomValues(b);
  return Array.from(b, (x) => x.toString(16).padStart(2, '0')).join('');
}

interface ActiveTransfer {
  xferId: string;
  /** Set by cancel()/supersede — checked between sends, and pending waits resolve. */
  cancelReason: string | null;
  waiter: { seq: number; resolve: (status: string) => void } | null;
}

export class BigClipboardSender {
  private active: ActiveTransfer | null = null;
  private readonly ackTimeoutMs: number;
  private readonly maxRetries: number;
  private readonly chunkBytes: () => number;

  constructor(
    /** Sends one frame as an RFB ClientCutText (the direct-path sink). */
    private readonly sink: (text: string) => void,
    opts: { ackTimeoutMs?: number; maxRetries?: number; chunkBytes?: () => number } = {},
  ) {
    // 10 s ≈ 5× the ~2 s serial drain of one frame — tolerates keep-warm
    // interleaving and a busy guest without stalling real failures for long.
    this.ackTimeoutMs = opts.ackTimeoutMs ?? 10_000;
    this.maxRetries = opts.maxRetries ?? 4;
    // Read per-transfer, not per-construction: the guest's capacity arrives via
    // the HELLO handshake AFTER the engine (and this sender) already exist, and
    // can be demoted mid-session if big frames stop landing.
    this.chunkBytes = opts.chunkBytes ?? (() => MIN_CHUNK_BYTES);
  }

  /**
   * Transfer `text` to the guest clipboard. Any in-flight transfer is
   * superseded (latest copy wins — the same semantics as the clipboard
   * itself). Resolves when the guest acked the final frame (`ok:true` means
   * the agent decoded AND wrote the guest clipboard), or with the failure.
   */
  async submit(text: string): Promise<BcpResult> {
    this.cancel('superseded');
    const state: ActiveTransfer = { xferId: randHex8(), cancelReason: null, waiter: null };
    this.active = state;

    const raw = new TextEncoder().encode(text);
    const compressed = await gzipBytes(raw);
    if (this.active !== state || state.cancelReason) {
      return { ok: false, rawLen: raw.byteLength, superseded: true, error: 'superseded' };
    }
    // Snapshot once per transfer so every frame of THIS payload is framed
    // identically — the receiver validates total/rawLen against frame 1.
    const chunkSize = Math.max(1, Math.floor(this.chunkBytes()));
    const total = Math.max(1, Math.ceil(compressed.byteLength / chunkSize));

    for (let seq = 1; seq <= total; seq++) {
      const chunk = compressed.subarray(
        (seq - 1) * chunkSize,
        Math.min(seq * chunkSize, compressed.byteLength),
      );
      const b64 = base64FromBytes(chunk);
      const frame = `${BLOB_FRAME_PREFIX}${state.xferId}:${seq}:${total}:${raw.byteLength}:${crc32Hex(new TextEncoder().encode(b64))}:${b64}`;

      let status = 'timeout';
      for (let attempt = 0; attempt <= this.maxRetries; attempt++) {
        if (state.cancelReason) {
          this.finish(state);
          return {
            ok: false,
            rawLen: raw.byteLength,
            superseded: state.cancelReason === 'superseded',
            error: state.cancelReason,
          };
        }
        this.sink(frame);
        status = await this.waitAck(state, seq);
        // Retryable: lost frame/ack (timeout) or corrupt in transit (err:crc).
        if (status !== 'timeout' && status !== 'err:crc') break;
      }

      const wantFinal = seq === total;
      if ((wantFinal && status === 'done') || (!wantFinal && status === 'ok')) continue;
      this.finish(state);
      return {
        ok: false,
        rawLen: raw.byteLength,
        superseded: status === 'cancelled:superseded',
        error: status,
      };
    }
    this.finish(state);
    return { ok: true, rawLen: raw.byteLength };
  }

  /**
   * Route a guest→host text that carries our ack prefix. Returns true when
   * consumed (callers must then NOT treat it as clipboard data). Unknown
   * xferId/seq acks (a sibling console's transfer, or stale retransmits) are
   * consumed silently — they are never operator data.
   */
  onAckText(text: string): boolean {
    if (!text.startsWith(BLOB_ACK_PREFIX)) return false;
    const parts = text.slice(BLOB_ACK_PREFIX.length).split(':');
    // status may itself contain ':' (err:crc) — rejoin the tail.
    if (parts.length < 3) return true;
    const [xferId, seqStr, ...statusParts] = parts;
    const seq = Number(seqStr);
    const status = statusParts.join(':');
    const w = this.active?.waiter;
    if (this.active && w && this.active.xferId === xferId && w.seq === seq) {
      this.active.waiter = null;
      w.resolve(status);
    }
    return true;
  }

  /** Abort the in-flight transfer (reconnect, console close, supersede). */
  cancel(reason: string): void {
    const state = this.active;
    if (!state) return;
    state.cancelReason = reason;
    const w = state.waiter;
    if (w) {
      state.waiter = null;
      w.resolve(`cancelled:${reason}`);
    }
  }

  private finish(state: ActiveTransfer): void {
    if (this.active === state) this.active = null;
  }

  private waitAck(state: ActiveTransfer, seq: number): Promise<string> {
    return new Promise<string>((resolve) => {
      const timer = setTimeout(() => {
        if (state.waiter?.seq === seq) state.waiter = null;
        resolve('timeout');
      }, this.ackTimeoutMs);
      state.waiter = {
        seq,
        resolve: (status) => {
          clearTimeout(timer);
          resolve(status);
        },
      };
    });
  }
}

// ── Reverse direction (guest→host) ────────────────────────────────────────
// A large GUEST copy hits the mirror-image wall: QEMU's VNC server silently
// drops any guest→host clipboard PROVIDE that deflates over 1 MiB. So the guest
// agent (bigclip_tx.go) chunks it with the SAME wire grammar, and this receiver
// reassembles — the exact mirror of the agent's blobRx in bigclip.go.

/** Go-style SplitN: at most `n` fields; the last keeps any remaining separators.
 *  (A valid frame's fields never contain ':', but stay robust anyway.) */
function splitN(s: string, sep: string, n: number): string[] {
  const out: string[] = [];
  let start = 0;
  for (let i = 0; i < n - 1; i++) {
    const idx = s.indexOf(sep, start);
    if (idx === -1) {
      out.push(s.slice(start));
      return out;
    }
    out.push(s.slice(start, idx));
    start = idx + sep.length;
  }
  out.push(s.slice(start));
  return out;
}

function bytesFromBase64(b64: string): Uint8Array {
  const bin = atob(b64); // throws on invalid input — caller treats as corruption
  const out = new Uint8Array(bin.length);
  for (let i = 0; i < bin.length; i++) out[i] = bin.charCodeAt(i);
  return out;
}

function concatBytes(chunks: Uint8Array[], totalLen: number): Uint8Array {
  const out = new Uint8Array(totalLen);
  let off = 0;
  for (const c of chunks) {
    out.set(c, off);
    off += c.length;
  }
  return out;
}

/** Inflate gzip bytes, bounded by the declared length (a zip-bomb guard — one
 *  byte past rawLen is an error, and gzip's own trailing CRC covers integrity). */
async function gunzipToText(compressed: Uint8Array, rawLen: number): Promise<string> {
  const stream = new Blob([compressed as BlobPart])
    .stream()
    .pipeThrough(new DecompressionStream('gzip'));
  const reader = stream.getReader();
  const parts: Uint8Array[] = [];
  let total = 0;
  for (;;) {
    const { done, value } = await reader.read();
    if (done) break;
    total += value.byteLength;
    if (total > rawLen) {
      await reader.cancel();
      throw new Error('blob length overflow');
    }
    parts.push(value);
  }
  if (total !== rawLen) throw new Error(`blob length mismatch: ${total} != ${rawLen}`);
  return new TextDecoder().decode(concatBytes(parts, total));
}

function mkAck(xferId: string, seq: number, status: string): string {
  return `${BLOB_ACK_PREFIX}${xferId}:${seq}:${status}`;
}

/**
 * Reassembles a large guest→host transfer. State-machine mirror of the agent's
 * blobRx (bigclip.go): accepts frames in order under stop-and-wait, ACKs each,
 * and on the final frame inflates and delivers to the host clipboard.
 *
 * Serialization: the agent is stop-and-wait, so at most one frame is in flight
 * and ingest() calls never overlap — no internal locking needed.
 */
export class BigClipboardReceiver {
  private xferId = '';
  private total = 0;
  private rawLen = 0;
  private next = 1; // next expected 1-based seq
  private buf: Uint8Array[] = [];
  private bufLen = 0;
  // Last completed transfer: a late duplicate of its final frame (our done ACK
  // was lost, the agent retransmitted) must be re-ACKed done, not misread as a
  // new transfer.
  private doneId = '';

  constructor(
    /** Decompressed-payload cap (bytes) — matches the agent's MaxClipboardSize. */
    private readonly maxBytes: number,
    /** Deliver the completed text to the host clipboard (shared with the direct path). */
    private readonly deliver: (text: string) => Promise<void>,
    /** Send one ACK as an ordinary host→guest ClientCutText (raw side-channel sink). */
    private readonly ack: (ackText: string) => void,
  ) {}

  /** Abort in-progress reassembly (reconnect/rfb_reset). doneId is kept so a
   *  straggler final-frame retransmit from before the reset still re-ACKs done. */
  reset(): void {
    this.xferId = '';
    this.buf = [];
    this.bufLen = 0;
    this.next = 1;
  }

  async ingest(frameText: string): Promise<void> {
    const parts = splitN(frameText.slice(BLOB_FRAME_PREFIX.length), ':', 6);
    if (parts.length !== 6) return;
    // length === 6 guarantees all six are present; defaults only satisfy the
    // indexed-access type (they can never actually apply). Empty values are
    // still rejected by the validation below.
    const [xferId = '', seqStr = '', totalStr = '', rawLenStr = '', wantCrc = '', b64 = ''] = parts;
    const seq = Number(seqStr);
    const total = Number(totalStr);
    const rawLen = Number(rawLenStr);
    if (
      !xferId ||
      !Number.isInteger(seq) ||
      !Number.isInteger(total) ||
      !Number.isInteger(rawLen) ||
      seq < 1 ||
      total < 1 ||
      seq > total ||
      rawLen < 1
    ) {
      return;
    }

    // Late duplicate of a completed transfer's final frame: our done ACK was lost.
    if (xferId === this.doneId) {
      this.ack(mkAck(xferId, seq, 'done'));
      return;
    }
    if (rawLen > this.maxBytes) {
      this.ack(mkAck(xferId, seq, 'err:too_large'));
      return;
    }

    // New transfer supersedes any in-progress one (latest copy wins).
    if (xferId !== this.xferId) {
      // A fresh transfer must start at seq 1. Seeing seq > 1 for an unknown
      // xferId means we joined mid-stream — our state was reset (a reconnect,
      // reset()) while the agent kept sending. We can't reassemble from the
      // middle, so tell the agent to rewind to the first frame rather than
      // silently dropping (which would stall it for its full ack timeout).
      if (seq !== 1) {
        this.ack(mkAck(xferId, seq, 'err:restart'));
        return;
      }
      this.xferId = xferId;
      this.total = total;
      this.rawLen = rawLen;
      this.next = 1;
      this.buf = [];
      this.bufLen = 0;
    }
    if (total !== this.total || rawLen !== this.rawLen) {
      // Header drifted mid-transfer — unrecoverable; drop state, let the sender time out.
      this.reset();
      return;
    }
    if (seq === this.next - 1) {
      // Duplicate of the frame we already consumed: our ok ACK was lost.
      this.ack(mkAck(xferId, seq, 'ok'));
      return;
    }
    if (seq !== this.next) return; // out-of-order under stop-and-wait → drop; sender retries

    if (crc32Hex(new TextEncoder().encode(b64)) !== wantCrc) {
      this.ack(mkAck(xferId, seq, 'err:crc'));
      return;
    }
    let chunk: Uint8Array;
    try {
      chunk = bytesFromBase64(b64);
    } catch {
      this.ack(mkAck(xferId, seq, 'err:crc')); // corrupt in transit, exactly like a CRC miss
      return;
    }
    if (this.bufLen + chunk.length > this.maxBytes + 64 * 1024) {
      this.reset();
      this.ack(mkAck(xferId, seq, 'err:too_large'));
      return;
    }
    this.buf.push(chunk);
    this.bufLen += chunk.length;
    this.next++;
    if (seq < total) {
      this.ack(mkAck(xferId, seq, 'ok'));
      return;
    }

    // Final frame: inflate and deliver. State is consumed either way.
    const compressed = concatBytes(this.buf, this.bufLen);
    this.reset();
    let text: string;
    try {
      text = await gunzipToText(compressed, rawLen);
    } catch {
      this.ack(mkAck(xferId, seq, 'err:decode'));
      return;
    }
    try {
      await this.deliver(text);
    } catch {
      // The host clipboard write failed — never ACK done for a paste that
      // didn't land (the sender treats err:write as terminal, like the agent).
      this.ack(mkAck(xferId, seq, 'err:write'));
      return;
    }
    // Record done only AFTER a confirmed delivery, so a retried final frame
    // after a write failure re-attempts the write rather than falsely acking.
    this.doneId = xferId;
    this.ack(mkAck(xferId, seq, 'done'));
  }
}
