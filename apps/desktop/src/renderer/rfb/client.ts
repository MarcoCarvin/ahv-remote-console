/**
 * RFB / VNC client for AHV Remote Console.
 *
 * The client speaks RFB 3.8 with security type "None" (the Go runtime has
 * already authenticated upstream; the WS endpoint we talk to is the
 * runtime's loopback proxy). It negotiates Tight + CopyRect + Raw
 * (Hextile/ZRLE disabled; Tight subsumes them) and the
 * ExtendedDesktopSize + ExtendedClipboard + ContinuousUpdates pseudo
 * encodings.
 *
 * The client is purely event-driven; the caller hands it a WebSocket and
 * subscribes to update / resize / clipboard events.
 */

// DIAG_TODO: process-wide diagnostic counters. Driven by sendKey /
// send / heartbeat. Lets us correlate "renderer claims to have
// sent N bytes" against "bridge claims to have received M F-key
// events" without going through Wireshark. Remove with the rest
// of DIAG_TODO before shipping.
const rfbDiag = {
  sendKeyCount: 0,
  sendBytes: 0,
  rxBytes: 0,
  rxMessages: 0,
  // PointerEvents that sendPointer dropped because state !== 'normal'.
  // Surfaced in the heartbeat so the operator can spot it without
  // staring at every event log entry.
  droppedPointerEvents: 0,
};
// Diagnostic counters exposed on `window.__rfbDiag` for DevTools
// inspection. No periodic console.log in production.
if (typeof window !== 'undefined') {
  (window as unknown as Record<string, unknown>).__rfbDiag = rfbDiag;
}

import {
  CLI_MSG_CLIENT_CUT_TEXT,
  CLI_MSG_FRAMEBUFFER_UPDATE_REQUEST,
  CLI_MSG_KEY_EVENT,
  CLI_MSG_POINTER_EVENT,
  CLI_MSG_QEMU,
  CLI_MSG_SET_DESKTOP_SIZE,
  CLI_MSG_SET_ENCODINGS,
  CLI_MSG_SET_PIXEL_FORMAT,
  ENC_COPY_RECT,
  ENC_CURSOR,
  ENC_DESKTOP_SIZE,
  ENC_EXTENDED_DESKTOP_SIZE,
  ENC_HEXTILE,
  ENC_QEMU_EXT_KEY_EVENT,
  ENC_RAW,
  ENC_TIGHT,
  ENC_ZRLE,
  QEMU_SUBMSG_EXT_KEY,
  SRV_MSG_BELL,
  SRV_MSG_FRAMEBUFFER_UPDATE,
  SRV_MSG_SERVER_CUT_TEXT,
  SRV_MSG_SET_COLOUR_MAP_ENTRIES,
} from './protocol';
import { StreamReader } from './reader';
import {
  decodeCopyRect,
  decodeHextile,
  decodeRaw,
  decodeTight,
  decodeZRLE,
  DecoderResult,
  InflateBudget,
  newFramebuffer,
  read565Pixel,
  TightInflater,
  ZRLEInflater,
  type Framebuffer,
  type Rect,
} from './encodings';
import { unicodeKeysym } from './keysyms';
import { rectExceedsFramebuffer } from './rectGeometry';

/**
 * Pixel reader handed to per-rect FramebufferUpdate subscribers.
 * Returns RGB bytes (3 per pixel) in row-major order. Used by the
 * sbtransport UplinkScanner to read the bottom-of-screen strip
 * after each FBU rect — see
 * `apps/desktop/src/renderer/sbtransport/UplinkScanner.ts`. The
 * reader is bound to the framebuffer the rect was just painted
 * against; calling it after the next FBU may read different bytes
 * (the FB is mutated in-place between rects).
 */
export type RFBPixelReader = (x: number, y: number, w: number, h: number) => Uint8Array;

/**
 * Handler for the per-rect FramebufferUpdate subscription. Receives
 * the rect that just finished decoding and a pixel-reader bound to
 * the current framebuffer. Subscribers (notably the sbtransport
 * UplinkScanner) can sample arbitrary regions without having to
 * plumb the framebuffer reference themselves.
 */
export type RFBFramebufferUpdateHandler = (rect: Rect, pixelReader: RFBPixelReader) => void;

/**
 * Cursor shape parsed from the server's ENC_CURSOR pseudo-encoding
 * (RFC 6143 §7.7.2). Rendered as a transparent overlay above the
 * framebuffer canvas in ConsoleView so the operator sees the
 * guest's cursor shape WITHOUT waiting for an RFB round-trip — the
 * client paints it locally at the live pointer position the moment
 * a pointermove fires, then the actual position update arrives at
 * the guest a few ms later (VMware-Tools-style smoothness).
 *
 * The framebuffer-baked cursor blob (which the host could otherwise
 * paint via the SetCursorPosition extension) is intentionally NOT
 * a substitute — it's still subject to one RFB round-trip per
 * pointer motion and visibly lags fast moves.
 */
export interface CursorShape {
  width: number;
  height: number;
  /** Hotspot x-offset relative to top-left of the shape. */
  hotspotX: number;
  /** Hotspot y-offset relative to top-left of the shape. */
  hotspotY: number;
  /**
   * width * height * 4 bytes, canvas-native RGBA order (R at
   * offset 0). Alpha channel is pre-baked from the RFC 6143 mask:
   * mask bit = 1 → alpha = 255 (opaque), mask bit = 0 → alpha = 0
   * (transparent). This makes overlay compositing a single
   * putImageData call with no per-pixel mask test.
   */
  rgba: Uint8ClampedArray;
  /**
   * width * height bytes, expanded from the bitmask. 0 =
   * transparent, 255 = opaque. Kept alongside `rgba` even though
   * it duplicates the alpha channel because some renderers
   * (e.g. operator-prefer-OS-cursor mode) want the binary
   * silhouette without the color data — and the matching Go
   * bridge code uses the same field shape so both ends are
   * symmetrical at debug time.
   */
  alpha: Uint8Array;
}

export type QualityLevel = 'high' | 'medium' | 'low';

export interface RFBEvents {
  onFramebufferReady: (fb: Framebuffer) => void;
  onUpdate: (rects: Rect[]) => void;
  onResize: (width: number, height: number) => void;
  onClipboard: (text: string) => void;
  onBell: () => void;
  onError: (err: Error) => void;
  onClose: () => void;
  /**
   * The RFB byte stream lost integrity mid-session — a rect whose geometry
   * overruns the framebuffer, an unknown encoding, or a decoder that threw.
   * These all mean a PRIOR rect left the stream byte-misaligned, and RFB has
   * no resync marker, so the only recovery is a fresh handshake (which the
   * host's self-heal performs, re-fetching a full framebuffer). This is a
   * RECOVERABLE condition — distinct from onError (terminal, e.g. a failed
   * security handshake). The client closes the WS after firing so the host's
   * close-driven reconnect realigns the stream; `reason` carries the desync
   * detail for the reconnecting notice. Hosts that don't wire this fall back
   * to onError (terminal) for backward compatibility.
   */
  onProtocolReset?: (reason: string) => void;
  /**
   * Server announced a new cursor pixmap via ENC_CURSOR. The
   * shape's coordinates are in cursor-local space; the
   * ConsoleView overlay translates by `(hotspotX, hotspotY)` to
   * align with the operator's live pointer position. Null when
   * the cursor encoding hasn't been received yet or the session
   * was closed — callers should fall back to the OS cursor.
   */
  onCursorChange: (shape: CursorShape) => void;
  /**
   * Fired immediately after each rect in a FramebufferUpdate is
   * fully decoded and written into the framebuffer. Enables
   * progressive rendering — the UI can paint each rect as it
   * arrives instead of waiting for the entire FBU to complete.
   * The batch `onUpdate` callback still fires at the end of the
   * FBU for any final cleanup.
   */
  onRectDecoded: (rect: Rect) => void;
  /**
   * Adaptive quality level changed. Fired when the RTT-based
   * state machine transitions between quality tiers. The UI can
   * display the current quality level in the status bar.
   */
  onQualityChange: (level: QualityLevel) => void;
  /**
   * A Tight JPEG rect was decoded into an ImageBitmap. Paint it directly
   * onto the visible canvas at (x, y) via drawImage — bypassing the
   * fb.pixels → putImageData roundtrip (3 fewer buffer copies). The
   * caller must close the bitmap after painting.
   */
  onDirectImage?: (bmp: ImageBitmap, x: number, y: number) => void;
}

export interface RFBClientOptions {
  ws: WebSocket;
  events: Partial<RFBEvents>;
  /**
   * Feature flag: announce the QEMU Extended Key Event pseudo-encoding
   * (-258) in SetEncodings and use CLI_MSG_QEMU for key events instead
   * of the plain CLI_MSG_KEY_EVENT. The extension carries both the
   * keysym AND the raw evdev keycode, which lets the guest's keyboard
   * driver apply its own layout (fixes AltGr composition, dead-keys,
   * Pause/SysRq variants).
   *
   * Default: false — but this is now a FORCE-ON override, not the only gate.
   * The client also auto-enables scancode passthrough when the SERVER ACKs the
   * -258 pseudo-encoding (extendedKeyEnabled() OR's the two). AHV's QEMU does
   * send that ack (the decoder has to handle it — see ENC_QEMU_EXT_KEY_EVENT),
   * so the extension self-activates safely: no ack → stays on the keysym path,
   * never sending CLI_MSG_QEMU at a server that can't parse it. Leave false.
   */
  enableExtendedKey?: boolean;
  /**
   * Negotiate the 16bpp RGB565 pixel format instead of 32bpp BGRX — the
   * collapsed-link bandwidth lever (halves Raw bytes, cuts Tight payloads
   * ~25-35%; with JPEG compiled out of AHV's QEMU, pixel depth is the only
   * server-side lever left). FIXED for the lifetime of this client: a
   * SetPixelFormat mid-stream would garbage-decode any in-flight FBU encoded
   * in the old format, so the only safe switch point is the re-handshake on
   * reconnect/rfb_reset, where ConsoleView constructs a fresh client.
   * Default: false.
   */
  lowColor?: boolean;
  /**
   * Seed for the adaptive-quality machine. ConsoleView persists the last
   * committed tier across client recreations and seeds it back in here so
   * the machine resumes from the persisted level — a client seeded at the
   * default 'high' would never re-emit onQualityChange('high') on a
   * recovered link, leaving the host's persisted tier stale-low forever.
   * Default: 'high'.
   */
  initialQuality?: QualityLevel;
}

const RFB_PROTOCOL_VERSION = 'RFB 003.008\n';

// How long protocolReset keeps the ws open waiting for the proxy's rfb_reset to
// recreate this client (same-socket re-handshake) before falling back to a
// close-driven reopen. Must comfortably exceed a real guest REBOOT — the VM's
// VNC can take ~30s+ to serve again after a hard reset (observed 31s for the
// wizard's apply-reset), and the proxy holds this same socket and re-handshakes
// the moment upstream returns. Too short (was 6s) reverts to the broken close
// path mid-reboot. The proxy closes the socket itself if it truly gives up, so
// this is just the backstop for a pure client-side desync with a live upstream.
const PROTOCOL_RESET_FALLBACK_MS = 3000;

// Parse a JPEG's frame header (SOFn marker) to recover its intrinsic pixel
// dimensions WITHOUT decoding the image. Tight's JPEG subencoding always
// encodes exactly the rectangle, so the client requires (w,h) to equal the
// rect's; this lets us reject a few-KB JPEG that declares e.g. 30000×30000 in
// its SOF BEFORE createImageBitmap would allocate the multi-GB bitmap (a
// decompression bomb the ZRLE inflate cap doesn't cover, because the JPEG path
// bypasses the inflater). Returns null on a malformed/unrecognized stream.
export function readJpegDimensions(b: Uint8Array): { w: number; h: number } | null {
  if (b.length < 2 || b[0] !== 0xff || b[1] !== 0xd8) return null; // SOI
  let i = 2;
  while (i + 1 < b.length) {
    if (b[i] !== 0xff) return null; // not aligned to a marker — malformed
    // Skip any 0xFF fill bytes preceding the marker code.
    let marker = b[i + 1]!;
    while (marker === 0xff && i + 2 < b.length) {
      i++;
      marker = b[i + 1]!;
    }
    i += 2;
    // Standalone markers (SOI/EOI/RSTn/TEM) carry no length-prefixed segment.
    if (marker === 0xd8 || marker === 0xd9 || (marker >= 0xd0 && marker <= 0xd7) || marker === 0x01)
      continue;
    if (i + 1 >= b.length) return null;
    const segLen = (b[i]! << 8) | b[i + 1]!;
    if (segLen < 2) return null;
    // SOF0..SOF15 carry the frame dimensions, except DHT(C4)/JPG(C8)/DAC(CC).
    const isSOF =
      marker >= 0xc0 && marker <= 0xcf && marker !== 0xc4 && marker !== 0xc8 && marker !== 0xcc;
    if (isSOF) {
      // SOF payload: [precision:1][height:2][width:2][...]; height at +3 from
      // the segment-length field (which itself sits at offset i).
      if (i + 6 >= b.length) return null;
      const h = (b[i + 3]! << 8) | b[i + 4]!;
      const w = (b[i + 5]! << 8) | b[i + 6]!;
      return { w, h };
    }
    i += segLen; // skip this marker segment's payload
  }
  return null;
}

export class RFBClient {
  private readonly ws: WebSocket;
  private events: Partial<RFBEvents>;
  private readonly reader = new StreamReader();
  private state: 'version' | 'security' | 'security_result' | 'init' | 'normal' = 'version';
  private fb: Framebuffer | null = null;
  private pendingRect: (Rect & { _hex?: unknown }) | null = null;
  private remainingRects = 0;
  private rectAccumulator: Rect[] = [];
  /**
   * Ring buffer of the last few successfully-decoded rects (encoding +
   * geometry + payload bytes consumed). Purely diagnostic: when the
   * decode-amplification bounds guard trips on a garbage rect header, this is
   * dumped so the desync can be traced to the *preceding* decoder — a rect
   * that consumed the wrong byte count leaves the stream misaligned, and the
   * next header reads garbage. `bytes` is the synchronous payload consumption
   * (-1 for rects that spanned multiple WebSocket frames, where a single delta
   * isn't meaningful).
   */
  private readonly recentRects: {
    enc: number;
    x: number;
    y: number;
    w: number;
    h: number;
    bytes: number;
  }[] = [];
  private recordRect(rect: Rect, bytes: number): void {
    this.recentRects.push({
      enc: rect.encoding,
      x: rect.x,
      y: rect.y,
      w: rect.w,
      h: rect.h,
      bytes,
    });
    if (this.recentRects.length > 8) this.recentRects.shift();
  }
  private readonly enableExtendedKey: boolean;
  // 16bpp RGB565 mode, fixed at construction — see RFBClientOptions.lowColor.
  private readonly lowColor: boolean;
  // Set once the server ACKs our advertised QEMU Extended Key Event pseudo-
  // encoding (-258) with its bodyless confirmation rect (see the decoder). The
  // ack proves QEMU accepts CLI_MSG_QEMU ext-key frames, so from then on we can
  // use scancode passthrough — the guest applies its OWN keymap to the physical
  // scancode. That's the only correct model when the guest layout is operator-
  // chosen (not the fixed en-us the keysym path assumes), so it auto-fixes
  // non-US guests without any per-keystroke layout translation.
  private extKeyAcked = false;
  /**
   * Latest cursor pixmap from ENC_CURSOR. Null before the server
   * sends one or after dispose. Mutated in-place when a new shape
   * arrives so the React overlay (which calls cursorShape() each
   * frame) sees the freshest data without an extra setState hop.
   */
  private cursorShape: CursorShape | null = null;
  /**
   * Last OPERATOR-initiated pointer position (framebuffer-pixel
   * coords). Updated by every sendPointer() call EXCEPT those the
   * convergence Reconciler makes for v3 transport bursts — those
   * skip this tracking via the `synthetic` opt because the burst's
   * SYNC/data/END coordinates are transport noise, not real
   * intent. The Reconciler reads this on burst completion to
   * append a "cursor restore" event so the guest cursor doesn't
   * end up parked at (65535, 65535) after a clipboard sync.
   */
  private lastOperatorPointer: { x: number; y: number; buttonMask: number } | null = null;
  /**
   * Per-rect FramebufferUpdate subscribers. Parallel to the singular
   * `events.onUpdate` callback but fires per-rect (so the
   * sbtransport UplinkScanner can react before the rect-batch flush)
   * and supports multiple independent subscribers without forcing
   * each one through setEvents. Populated via
   * `subscribeFramebufferUpdate`; cleared on dispose().
   */
  private readonly fbuSubscribers = new Set<RFBFramebufferUpdateHandler>();
  // Shared per-FramebufferUpdate decompression budget — reset at each FBU
  // header so the ZRLE inflater and Tight's four streams can't be chained
  // across rects to amplify a small frame into multi-GiB of inflation.
  private readonly inflateBudget = new InflateBudget();
  private readonly zrleInflater = new ZRLEInflater(this.inflateBudget);
  private readonly tightInflater = new TightInflater(this.inflateBudget);
  // Reused scratch canvas for async JPEG (Tight) blits into the framebuffer.
  private tightScratch: HTMLCanvasElement | null = null;

  // ── PRP (Periodic Request Period) ───────────────────────────────────
  //
  // The main pump requests the next frame at each FBU header
  // (handleFramebufferUpdate), so a request is outstanding while we decode.
  // But when the screen goes IDLE, QEMU stops sending FBUs, so no header
  // fires and nothing re-primes the pump — the first post-idle change would
  // wait for a fresh request. A 50ms periodic incremental request keeps the
  // pipeline primed across idle. QEMU coalesces multiple outstanding
  // requests, so redundant requests cause no extra work.
  private _prpTimer: ReturnType<typeof setInterval> | null = null;
  // Leading-edge debounce for primeOnInput (see there).
  private _primeTimer: ReturnType<typeof setTimeout> | null = null;
  private static readonly PRIME_DEBOUNCE_MS = 50;

  // When this window is hidden (minimized / occluded / inactive), re-prime the
  // pump as soon as it becomes visible again — the PRP tick is gated on
  // visibility (see the timer) so a hidden console goes fully idle, and the
  // next change would otherwise wait for the first post-show tick. Registered
  // in the constructor, removed in dispose(). `document` is guarded for the
  // non-DOM test environment.
  private onVisibilityChange = (): void => {
    if (typeof document !== 'undefined' && !document.hidden && this.state === 'normal') {
      this.requestUpdate(true);
    }
  };

  // Fallback close timer armed by protocolReset (see there). Cleared by
  // dispose() — so a recreate on the proxy's rfb_reset cancels the fallback.
  private _protocolResetTimer: ReturnType<typeof setTimeout> | null = null;

  // ── Adaptive quality state machine ──────────────────────────────────
  //
  // Driven by RTT updates from the proxy's heartbeat control frames AND
  // by the FBU service-time signal below (which catches bandwidth
  // collapse the RTT can't see). Tracks the link tier for the UI badge
  // and client-side degradation levers — ConsoleView reads it at each
  // (re)connect to pick the connection's pixel format (lowColor); see the
  // commit site in _evaluateQuality for why a tier change deliberately
  // sends nothing to the server mid-connection.
  private _rttMs = 0;
  private _qualityLevel: QualityLevel = 'high';
  // Hysteresis: require N consecutive evaluations at the same level
  // before committing a transition. Prevents oscillation on noisy RTT.
  private _pendingQuality: QualityLevel = 'high';
  private _pendingQualityCount = 0;
  private static readonly QUALITY_HYSTERESIS = 2;
  private static readonly TIER_RANK: Record<QualityLevel, number> = {
    low: 0,
    medium: 1,
    high: 2,
  };
  private static readonly TIER_BELOW: Record<QualityLevel, QualityLevel> = {
    high: 'medium',
    medium: 'low',
    low: 'low',
  };

  // ── FBU service-time signal ─────────────────────────────────────────
  //
  // RTT alone misses bandwidth collapse: the heartbeat's latency can be
  // stale for many seconds, and a bufferbloated low-RTT link keeps the RTT
  // tier at 'high' while large FramebufferUpdates take hundreds of ms to
  // arrive. So each FBU is timed from its header (handleFramebufferUpdate)
  // to completion (flushUpdate) and the rolling p50 over a short window is
  // a live "can the pipe actually service a frame" signal that demotes the
  // tier even when RTT looks fine. FBUs below a byte floor are EXCLUDED
  // entirely: the tiny idle frames the 50ms PRP pump elicits are always
  // fast, so counting them would let an idle console masquerade as a
  // healthy link (driving promotion) — they neither demote nor count as
  // evidence for promotion.
  private static readonly SERVICE_WINDOW_MS = 3000;
  private static readonly SERVICE_DEMOTE_P50_MS = 250;
  private static readonly SERVICE_BYTE_FLOOR = 16 * 1024;
  private readonly _serviceSamples: { t: number; ms: number }[] = [];
  // Dedicated per-client received-byte counter. NOT rfbDiag.rxBytes — that
  // is a process-wide DIAG_TODO aggregate across every console window's
  // client and is slated for removal.
  private _rxTotal = 0;
  private _fbuStartTs = -1; // -1 = no FBU in flight
  private _fbuStartConsumed = 0;

  /** Bytes consumed from the stream so far (received minus still-buffered). */
  private _consumedBytes(): number {
    return this._rxTotal - this.reader.available();
  }

  /** Close out the in-flight FBU's service-time sample. Called from
   *  flushUpdate (FBU completion). Sub-floor FBUs record nothing. */
  private _recordFbuServiceTime(): void {
    if (this._fbuStartTs < 0) return;
    const now = performance.now();
    const ms = now - this._fbuStartTs;
    const bytes = this._consumedBytes() - this._fbuStartConsumed;
    this._fbuStartTs = -1;
    if (bytes < RFBClient.SERVICE_BYTE_FLOOR) return;
    this._serviceSamples.push({ t: now, ms });
    this._evaluateQuality();
  }

  /** Rolling p50 of FBU service times within SERVICE_WINDOW_MS, or null
   *  when the window holds no qualifying samples (idle console). */
  private _serviceTimeP50(now: number): number | null {
    const cutoff = now - RFBClient.SERVICE_WINDOW_MS;
    while (this._serviceSamples.length > 0 && this._serviceSamples[0]!.t < cutoff) {
      this._serviceSamples.shift();
    }
    if (this._serviceSamples.length === 0) return null;
    const sorted = this._serviceSamples.map((s) => s.ms).sort((a, b) => a - b);
    return sorted[sorted.length >> 1]!;
  }

  // On a loopback close, dispose FIRST (idempotent: clears the ~50ms request
  // timer + drain ports and detaches our listeners — it does NOT close the ws,
  // which is already closing) so a dead-but-unswapped socket can't keep firing
  // requestUpdate()->send() and spamming "[rfb.tx] send BLOCKED — ws not OPEN".
  // THEN notify: onClose still drives ConsoleView->'reconnecting' and the host's
  // self-heal reopen.
  private readonly closeHandler = () => {
    this.dispose();
    this.events.onClose?.();
  };
  // A WebSocket 'error' event is only actionable while the socket is genuinely
  // OPEN. The browser ALSO dispatches 'error' (immediately before 'close') when
  // a CONNECTING socket is aborted (fast open-then-close, sequential
  // open→close→open, unmount-during-connect) or on any abnormal close — those
  // are close artifacts, not faults, and firing onError there surfaced a
  // spurious "WebSocket error" on console close and could pin a terminal error.
  // Mirror the readyState guard send() already uses: fire only when OPEN; the
  // accompanying 'close' event drives recovery via closeHandler.
  private readonly errorHandler = () => {
    if (this.ws.readyState !== WebSocket.OPEN) return;
    this.events.onError?.(new Error('WebSocket error'));
  };

  // A recoverable stream-integrity failure (desync / decode error). The byte
  // stream cannot be realigned in place, so we close the WS — the host's
  // close-driven self-heal reopens the stream and re-runs the handshake,
  // re-fetching a full framebuffer that realigns us. Prefer onProtocolReset
  // (maps to "reconnecting"); fall back to the terminal onError for hosts that
  // don't wire it. The caller logs the diagnostic desync trail first.
  private protocolReset(reason: string): void {
    this.halted = true;
    if (!this.events.onProtocolReset) {
      // No reconnecting-aware handler: fall back to the terminal close.
      this.events.onError?.(new Error(reason));
      this.close();
      return;
    }
    this.events.onProtocolReset(reason);
    // Do NOT close the ws here. The proxy recovers an upstream drop / guest
    // reboot by re-handshaking on the SAME socket — it sends `rfb_reset`, which
    // drives ConsoleView to recreate this client (dispose → fresh client on the
    // same ws). Closing now races ahead of that and makes the proxy write the
    // RFB greeting into a dead socket ("websocket: close sent"), so it can never
    // re-handshake and the screen freezes. Keep the ws open and let the recreate
    // realign us. Fallback: if no recreate disposes this client within the
    // window (a pure client-side desync with a healthy upstream, where the proxy
    // has no reason to send rfb_reset), close to trigger the reopen self-heal.
    this._protocolResetTimer ??= setTimeout(() => {
      this._protocolResetTimer = null;
      this.close();
    }, PROTOCOL_RESET_FALLBACK_MS);
  }

  // Set once a protocolReset fires. The stream is byte-misaligned, so any
  // bytes still buffered are garbage — drain() checks this to stop parsing
  // them instead of cascading into more "desync" trips on the dead session.
  private halted = false;

  constructor(opts: RFBClientOptions) {
    this.ws = opts.ws;
    this.events = opts.events;
    this.enableExtendedKey = opts.enableExtendedKey ?? false;
    this.lowColor = opts.lowColor ?? false;
    this._qualityLevel = opts.initialQuality ?? 'high';
    this._pendingQuality = this._qualityLevel;
    this.ws.binaryType = 'arraybuffer';
    this.ws.addEventListener('message', this.onMessage);
    this.ws.addEventListener('close', this.closeHandler);
    this.ws.addEventListener('error', this.errorHandler);
    if (typeof document !== 'undefined') {
      document.addEventListener('visibilitychange', this.onVisibilityChange);
    }
  }

  /**
   * Is the QEMU Extended Key Event extension active on this client? True when
   * either the caller force-enabled it (enableExtendedKey) OR the server ACKed
   * our advertised -258 pseudo-encoding (extKeyAcked) — the latter is the normal
   * path and flips on automatically before the first screen FBU. The input
   * engine reads this per keystroke to choose scancode passthrough over the
   * en-us keysym translation.
   */
  extendedKeyEnabled(): boolean {
    return this.enableExtendedKey || this.extKeyAcked;
  }

  /**
   * Swap the event callbacks without recreating the protocol state. Used by
   * the renderer when React 19 <StrictMode> tears down + remounts the
   * ConsoleView effect: we want to keep the in-flight RFB negotiation but
   * point its callbacks at the new component's setState references.
   */
  setEvents(events: Partial<RFBEvents>): void {
    this.events = events;
  }

  /**
   * Detach our listeners from the WebSocket without closing it. The parent
   * (Shell) owns the WebSocket lifecycle: it creates the WS when the tab is
   * opened and closes it in closeTab. React 19's <StrictMode> runs effect
   * cleanups twice during dev as a sanity check; if we closed the WS here,
   * the connection would die mid-RFB-handshake on every dev mount — which
   * is exactly what was happening: Prism's "RFB 003.008\n" arrived, the
   * cleanup ran, the WS closed before we could even reply.
   */
  dispose(): void {
    if (this._prpTimer !== null) {
      clearInterval(this._prpTimer);
      this._prpTimer = null;
    }
    if (this._primeTimer !== null) {
      clearTimeout(this._primeTimer);
      this._primeTimer = null;
    }
    // A recreate (proxy rfb_reset) disposing this client cancels protocolReset's
    // fallback close — recovery is happening on the same ws.
    if (this._protocolResetTimer !== null) {
      clearTimeout(this._protocolResetTimer);
      this._protocolResetTimer = null;
    }
    // Closing the ports also drops any in-flight drain continuation — the
    // message is never delivered, so a disposed client can't resume parsing.
    if (this._drainChannel !== null) {
      this._drainChannel.port1.close();
      this._drainChannel.port2.close();
      this._drainChannel = null;
    }
    this.ws.removeEventListener('message', this.onMessage);
    this.ws.removeEventListener('close', this.closeHandler);
    this.ws.removeEventListener('error', this.errorHandler);
    if (typeof document !== 'undefined') {
      document.removeEventListener('visibilitychange', this.onVisibilityChange);
    }
    this.fbuSubscribers.clear();
    this.zrleInflater.reset();
    this.tightInflater.reset();
  }

  /**
   * Tear down the RFB session and explicitly close the underlying WS. Use
   * this when the protocol layer itself wants to terminate (e.g., the
   * server announced an unsupported encoding). For ordinary component
   * unmounts call dispose() instead.
   */
  close(): void {
    this.dispose();
    try {
      this.ws.close();
    } catch {
      /* noop */
    }
  }

  /** Current framebuffer, or null before ServerInit. */
  framebuffer(): Framebuffer | null {
    return this.fb;
  }

  /**
   * Read RGB bytes (3 per pixel) from the current framebuffer in
   * row-major order. Returns a fresh Uint8Array the caller owns;
   * mutating it does not affect the framebuffer. Coordinates are
   * clamped silently if they extend past the FB — under no
   * circumstances will this method read out of bounds, but a
   * miscaller might observe truncation at the right/bottom edge.
   *
   * Exposed so the sbtransport UplinkScanner can sample the
   * bottom-of-screen strip without needing a Framebuffer reference;
   * the call-site (`useAgent`) hands the bound reader into the
   * scanner once at mount time.
   */
  readRGB(x: number, y: number, w: number, h: number): Uint8Array {
    if (!this.fb) return new Uint8Array(0);
    const fb = this.fb;
    const clampedX = Math.max(0, Math.min(fb.width, x | 0));
    const clampedY = Math.max(0, Math.min(fb.height, y | 0));
    const clampedW = Math.max(0, Math.min(fb.width - clampedX, w | 0));
    const clampedH = Math.max(0, Math.min(fb.height - clampedY, h | 0));
    const out = new Uint8Array(clampedW * clampedH * 3);
    const srcStride = fb.width * 4;
    for (let row = 0; row < clampedH; row++) {
      const srcOff = (clampedY + row) * srcStride + clampedX * 4;
      const dstOff = row * clampedW * 3;
      for (let col = 0; col < clampedW; col++) {
        const s = srcOff + col * 4;
        const d = dstOff + col * 3;
        // FB is RGBA8888 in canvas-native byte order (R at offset 0).
        out[d] = fb.pixels[s]!;
        out[d + 1] = fb.pixels[s + 1]!;
        out[d + 2] = fb.pixels[s + 2]!;
      }
    }
    return out;
  }

  /**
   * Subscribe to per-rect FramebufferUpdate notifications. Returns
   * an unsubscribe function. Independent of `setEvents` so the
   * sbtransport scanner can install itself without coordinating
   * with ConsoleView's events object.
   */
  subscribeFramebufferUpdate(handler: RFBFramebufferUpdateHandler): () => void {
    this.fbuSubscribers.add(handler);
    return () => this.fbuSubscribers.delete(handler);
  }

  /**
   * Fan out a per-rect FramebufferUpdate to all subscribers. Called
   * immediately after a rect's pixel data has been written into the
   * framebuffer. The pixel reader is bound to the current
   * framebuffer at the moment of the call; subscribers that need
   * to defer should copy.
   */
  private fireFramebufferUpdate(rect: Rect): void {
    if (this.fbuSubscribers.size === 0) return;
    const reader: RFBPixelReader = (x, y, w, h) => this.readRGB(x, y, w, h);
    for (const sub of this.fbuSubscribers) {
      try {
        sub(rect, reader);
      } catch {
        // Subscriber crashes never break the protocol loop.
      }
    }
  }

  // ----- outbound -----
  //
  // Every public send-side API checks `this.state === 'normal'` first.
  // The handshake messages (version reply, security selection, ClientInit,
  // SetPixelFormat/SetEncodings/initial FBU request) are emitted by the
  // state machine itself and don't go through these APIs, so this gate is
  // safe and exists to prevent external callers — clipboard pollers,
  // mouse handlers, keyboard listeners — from injecting bytes mid-
  // handshake and corrupting the stream. We saw this in the wild: the
  // clipboard poll's first tick fired during the 'version' state, sent a
  // 1316-byte ClientCutText where the upstream expected 12 bytes of
  // version reply, and Prism aborted the WebSocket.

  /**
   * primeOnInput — ensure an incremental FramebufferUpdateRequest is OUTSTANDING
   * the instant a DISCRETE input (key event / mouse button change) lands.
   *
   * The request loop is completion-gated (one incremental request outstanding,
   * re-issued when each FramebufferUpdate finishes — the deliberate "natural
   * backpressure" that must NOT become request-ahead pipelining, locked by the
   * client.test backpressure test). That means after QEMU answers the last
   * request there is a window — up to one ~50ms PRP idle-backstop tick — where
   * NO request is outstanding. A click/keystroke makes the guest produce a
   * frame (and QEMU's input message resets vnc_refresh to 30ms), but with no
   * request outstanding that frame waits for the next PRP tick. priming on the
   * input closes that gap. Leading-edge debounced to the PRP cadence so an
   * input burst sends at most one extra 10-byte incremental request; incremental
   * only; no-op outside 'normal'. Mirrors onVisibilityChange's re-prime.
   */
  primeOnInput(): void {
    if (this.state !== 'normal' || this._primeTimer !== null) return;
    this.requestUpdate(true);
    this._primeTimer = setTimeout(() => {
      this._primeTimer = null;
    }, RFBClient.PRIME_DEBOUNCE_MS);
  }

  requestUpdate(incremental = true): void {
    // readyState gate: a close does NOT change `state` (stays 'normal'), so
    // without this a still-armed request timer would send() into a closed
    // socket and spam the "ws not OPEN" warning. Disposal on close (closeHandler)
    // is the primary stop; this is the defense-in-depth floor.
    if (this.state !== 'normal' || !this.fb || this.halted || this.ws.readyState !== WebSocket.OPEN)
      return;
    const buf = new ArrayBuffer(10);
    const v = new DataView(buf);
    v.setUint8(0, CLI_MSG_FRAMEBUFFER_UPDATE_REQUEST);
    v.setUint8(1, incremental ? 1 : 0);
    v.setUint16(2, 0);
    v.setUint16(4, 0);
    v.setUint16(6, this.fb.width);
    v.setUint16(8, this.fb.height);
    this.send(buf);
  }

  /**
   * Request a specific rectangle of the framebuffer.
   *
   * Used by the clipboard-convergence Plane 3 pump to force QEMU to
   * push the pixel-strip rect at the bottom of the screen on a
   * predictable cadence. Without this, QEMU's RFB encoder only sends
   * regions it detected as changed; layered-window-style paints from
   * our bridge overlay sometimes fail to register as a change in
   * Windows' graphics layer, so QEMU silently drops the strip update.
   *
   * Passing incremental=false tells the server "send me this region
   * regardless of any change tracking" (RFC 6143 §7.5.3). Cost: one
   * compressed-strip rect (~5-20 KB at Tight/Hextile) per call.
   *
   * Bounds-checked against the framebuffer; out-of-range requests
   * are silently dropped.
   */
  requestRectUpdate(x: number, y: number, w: number, h: number, incremental = false): void {
    if (this.state !== 'normal' || !this.fb || this.halted) return;
    if (x < 0 || y < 0 || w <= 0 || h <= 0) return;
    if (x + w > this.fb.width || y + h > this.fb.height) return;
    const buf = new ArrayBuffer(10);
    const v = new DataView(buf);
    v.setUint8(0, CLI_MSG_FRAMEBUFFER_UPDATE_REQUEST);
    v.setUint8(1, incremental ? 1 : 0);
    v.setUint16(2, x);
    v.setUint16(4, y);
    v.setUint16(6, w);
    v.setUint16(8, h);
    this.send(buf);
  }

  /**
   * Send many KeyEvent messages as a SINGLE WebSocket frame. The
   * loop in sendKeystrokeBurst originally fired N back-to-back
   * ws.send() calls; AHV's QEMU (or some layer in the Prism
   * Console Proxy path) drops ~80% of events when they arrive
   * as separate WS frames at this rate. Batching all N events
   * into one TCP segment delivers them serially to QEMU's RFB
   * parser, which then processes them at its own pace without
   * dropping any.
   *
   * This is the high-throughput sibling of sendKey() — sendKey
   * is retained for one-shot use (Send Keys menu chords, etc.).
   */
  /**
   * Batched, chunked, RATE-LIMITED KeyEvent send.
   *
   * The substrate ceiling is QEMU's virtual keyboard accept rate
   * — empirically ~150 events/sec on AHV (consistent with a 125 Hz
   * USB HID polling interval). Pushing keys faster than this fills
   * the WebSocket → QEMU → guest input pipeline and freezes
   * legitimate user input for the duration of the drain. The
   * 5070-byte clipboard freeze incident (≈20 000 events queued,
   * VM unusable for ~2 minutes) is the canonical example: the
   * renderer was dispatching at 1355 ev/s into a 150 ev/s pipe.
   *
   * Fix: pace the dispatcher at ≈120 ev/s — 80 % of the measured
   * guest accept rate — so the queue stays drained and competing
   * user input still gets through with no perceptible lag.
   *
   * Tuning:
   *  - CHUNK_SIZE = 12 events per ws.send() frame.
   *  - CHUNK_DELAY_MS = 100 ms between chunks.
   *  - Effective rate: 12 / 0.100 s = 120 ev/s ≈ 0.8 × 150.
   *
   * Wall-clock for sample payloads (each UTF-8 byte = 4 KeyEvents,
   * see TextSession wire format):
   *   8 B   →   32 events →  ~0.3 s
   *   64 B  →  256 events →  ~2.1 s
   *   128 B →  512 events →  ~4.3 s   (Tier 1 silent boundary)
   *   512 B → 2048 events →  ~17 s    (Tier 3 reject boundary)
   *
   * Single-chunk fast path: when total ≤ CHUNK_SIZE, bypass the
   * setTimeout pacing entirely — HC_FOCUS_GRAB (2-4 events) and
   * other tiny dispatches ship synchronously with the caller's
   * stack. This keeps the TextSession state-machine serial with
   * the wire flush and avoids a needless 100 ms hiccup on the
   * focus-grab gesture.
   */
  sendKeyEvents(
    events: readonly (readonly [keysym: number, down: boolean])[],
    opts: {
      /**
       * Fires after each chunk lands on the wire, with the cumulative
       * count of events sent so far and the burst total. Both the
       * single-chunk fast path and the multi-chunk paced path call
       * it. Used by TextSession to surface live progress for Tier-2
       * pastes (the multi-second drain).
       */
      onChunk?: (sent: number, total: number) => void;
    } = {},
  ): void {
    if (this.state !== 'normal') {
      console.warn('[rfb.tx] sendKeyEvents BLOCKED — state not normal', { state: this.state });
      return;
    }
    const total = events.length;
    if (total === 0) return;

    // Rate-limit to ≈120 ev/s (80 % of measured QEMU virtual-keyboard
    // ceiling of ~150 ev/s). Previously CHUNK_SIZE=64 / DELAY=10 =
    // 6400 ev/s — an order of magnitude over the substrate and the
    // direct cause of the 5070-byte freeze. See docstring above.
    const CHUNK_SIZE = 12;
    const CHUNK_DELAY_MS = 100;

    // ── Single-chunk fast path ────────────────────────────────────
    //
    // Most TextSession dispatches fall here: HC_FOCUS_GRAB (2 events),
    // short URL/snippet data planes (~30-100 events). Bypass the
    // sendChunk closure + setTimeout dance entirely — straight
    // encode + this.send(). Saves the closure allocation + one
    // micro-task hop AND keeps the wire flush synchronous with the
    // caller's stack so the surrounding TextSession state machine
    // is fully serial with the network ship.
    if (total <= CHUNK_SIZE) {
      const buf = new ArrayBuffer(total * 8);
      const v = new DataView(buf);
      for (let j = 0; j < total; j++) {
        const off = j * 8;
        const [keysym, down] = events[j]!;
        v.setUint8(off, CLI_MSG_KEY_EVENT);
        v.setUint8(off + 1, down ? 1 : 0);
        v.setUint16(off + 2, 0);
        v.setUint32(off + 4, keysym >>> 0);
      }
      rfbDiag.sendKeyCount += total;
      this.send(buf);
      opts.onChunk?.(total, total);
      return;
    }

    // ── Multi-chunk path: pace at CHUNK_DELAY_MS between chunks ───
    const sendChunk = (start: number): void => {
      if (this.state !== 'normal') return;
      const end = Math.min(start + CHUNK_SIZE, total);
      const chunkLen = end - start;
      const buf = new ArrayBuffer(chunkLen * 8);
      const v = new DataView(buf);
      for (let j = 0; j < chunkLen; j++) {
        const off = j * 8;
        const [keysym, down] = events[start + j]!;
        v.setUint8(off, CLI_MSG_KEY_EVENT);
        v.setUint8(off + 1, down ? 1 : 0);
        v.setUint16(off + 2, 0);
        v.setUint32(off + 4, keysym >>> 0);
      }
      rfbDiag.sendKeyCount += chunkLen;
      this.send(buf);
      opts.onChunk?.(end, total);
      if (end < total) {
        setTimeout(() => sendChunk(end), CHUNK_DELAY_MS);
      }
    };
    sendChunk(0);
  }

  sendKey(keysym: number, down: boolean): void {
    // Count every call (retained on window.__rfbDiag for DevTools inspection).
    // No per-keystroke console output: it would leak keysyms to anyone with the
    // console open and is dev-only diagnostics.
    rfbDiag.sendKeyCount++;
    if (this.state !== 'normal') return;
    const buf = new ArrayBuffer(8);
    const v = new DataView(buf);
    v.setUint8(0, CLI_MSG_KEY_EVENT);
    v.setUint8(1, down ? 1 : 0);
    v.setUint16(2, 0);
    v.setUint32(4, keysym >>> 0);
    this.send(buf);
  }

  /**
   * QEMU Extended Key Event message. Carries both the X11 keysym AND a raw
   * AT/PC set-1 scancode. Format (RFC-style, big-endian):
   *
   *   u8  type     (CLI_MSG_QEMU = 255)
   *   u8  subtype  (QEMU_SUBMSG_EXT_KEY = 0)
   *   u16 down     (1 = press, 0 = release)
   *   u32 keysym
   *   u32 keycode  (AT set-1 scancode, 0xe0 folded into the 0x80 high bit;
   *                 0 if not mapped)
   *
   * WHICH FIELD QEMU USES depends on the server's keyboard-layout (-k) setting:
   * if -k is set (AHV ships en-us), QEMU reverse-maps the KEYSYM and IGNORES the
   * scancode; only with NO -k does it inject the raw scancode and let the guest
   * apply its layout. We send BOTH correctly so we're right either way (and the
   * scancode still reaches pre-OS firmware/UEFI screens that read it directly).
   *
   * Extended-key events are sent unconditionally: the pseudo-encoding is always
   * announced in setEncodings(). `extendedKeyEnabled()` is advisory only — it
   * informs the caller's keysym-vs-scancode choice, not whether this frame may
   * be sent.
   */
  sendExtendedKey(keysym: number, keycode: number, down: boolean): void {
    // No enableExtendedKey gate: the pseudo-encoding is always announced in
    // setEncodings(), so the server accepts these frames. `keycode` is the final
    // AT set-1 wire byte from Keycodes.ts (already 0xe0-folded) — do NOT fold it
    // again here.
    if (this.state !== 'normal') return;
    const buf = new ArrayBuffer(12);
    const v = new DataView(buf);
    v.setUint8(0, CLI_MSG_QEMU);
    v.setUint8(1, QEMU_SUBMSG_EXT_KEY);
    v.setUint16(2, down ? 1 : 0);
    v.setUint32(4, keysym >>> 0);
    v.setUint32(8, keycode >>> 0);
    this.send(buf);
  }

  /** Convenience: press → release a sequence of keysyms with brief delays. */
  sendKeySequence(keysyms: number[], releaseAfterMs = 16): void {
    if (this.state !== 'normal') return;
    for (const k of keysyms) this.sendKey(k, true);
    setTimeout(() => {
      for (let i = keysyms.length - 1; i >= 0; i--) this.sendKey(keysyms[i]!, false);
    }, releaseAfterMs);
  }

  /**
   * Framebuffer dimensions as advertised by the server's ServerInit
   * message (and updated on resize). Returns null before the
   * handshake completes. Used by the Pointer-PHY encoder to compute
   * the RFB-pixel (x, y) values that, after QEMU's linear
   * translation to tablet space, land at the desired payload band.
   */
  get framebufferSize(): { width: number; height: number } | null {
    if (!this.fb) return null;
    return { width: this.fb.width, height: this.fb.height };
  }

  /**
   * True once the RFB handshake has completed and the client is in its
   * steady 'normal' state — i.e. send-side APIs (sendKey/sendKeySequence)
   * will actually transmit rather than silently drop. Callers that drive
   * keys around a (re)connect (e.g. the OVMF boot-resolution wizard after a
   * reset) poll this so keystrokes aren't fired into a mid-handshake client.
   */
  get isReady(): boolean {
    // `halted` matters: protocolReset keeps the ws open (state stays 'normal')
    // while waiting for the proxy's rfb_reset to recreate this client. During
    // that window the stream is dead and sends go nowhere — so a halted client
    // is NOT ready. Without this, the wizard's waitReady returns a false
    // positive and fires keys into a dead client (the "completed but nothing
    // happened" failure). A recreated client starts fresh with halted=false.
    //
    // The OPEN check is equally load-bearing: a plain socket close (the
    // loopback drop that drives self-heal) fires closeHandler but leaves
    // `state` at 'normal' and `halted` false — so without it a dead client
    // still reports ready. ConsoleView's readiness poll would then re-mark a
    // dropped console 'connected' over a corpse socket (disarming the give-up
    // watchdog → a frozen, fake-"connected" console). Ready means the stream
    // is actually usable, which a closed/closing socket is not.
    return this.state === 'normal' && !this.halted && this.ws.readyState === WebSocket.OPEN;
  }

  /**
   * Latest cursor pixmap (RGBA + alpha) decoded from ENC_CURSOR.
   * The renderer reads this on each pointer-move frame and blits
   * the pixmap at `(pointerX - hotspotX, pointerY - hotspotY)` so
   * the cursor follows the operator's pointer without an RFB
   * round-trip.
   */
  getCursorShape(): CursorShape | null {
    return this.cursorShape;
  }

  /**
   * Current adaptive quality level. Read by the UI to display the
   * active encoding tier in the status bar.
   */
  getQualityLevel(): QualityLevel {
    return this._qualityLevel;
  }

  /**
   * Feed a heartbeat control frame from the runtime proxy. Called by
   * ConsoleView when it receives a JSON text message with
   * `type: "heartbeat"`. The client uses the latency value to drive
   * the adaptive quality tier (UI badge + client-side degradation
   * levers; nothing is renegotiated with the server — see
   * _evaluateQuality).
   */
  feedHeartbeat(latencyMs: number): void {
    if (latencyMs < 0) return;
    this._rttMs = latencyMs;
    this._evaluateQuality();
  }

  /**
   * Evaluate the current RTT and decide whether to transition to a
   * different quality level. Uses hysteresis to prevent oscillation:
   * the candidate level must be stable for QUALITY_HYSTERESIS
   * consecutive evaluations before a transition is committed.
   */
  private _evaluateQuality(): void {
    // RTT-derived tier — the original signal (stale up to the heartbeat
    // cadence, blind to bufferbloat on a low-RTT link).
    let candidate: QualityLevel;
    if (this._rttMs > 150) {
      candidate = 'low';
    } else if (this._rttMs > 80) {
      candidate = 'medium';
    } else {
      candidate = 'high';
    }

    // FBU service-time veto: when the rolling p50 says large updates are
    // slow to service, demote ONE tier from the current level (or follow
    // RTT if it already says worse). Because a slow p50 caps the candidate
    // at one-below-current, it also blocks any RTT-driven promotion while
    // slow evidence sits in the window — promotion needs BOTH a good RTT
    // and sustained low service time ("sustained" = the whole 3s window's
    // p50 plus the QUALITY_HYSTERESIS consecutive evaluations below). An
    // empty window (idle console, no large FBUs to judge by) leaves the
    // RTT signal in sole charge, exactly as before this signal existed.
    const p50 = this._serviceTimeP50(performance.now());
    if (p50 !== null && p50 > RFBClient.SERVICE_DEMOTE_P50_MS) {
      const demoted = RFBClient.TIER_BELOW[this._qualityLevel];
      if (RFBClient.TIER_RANK[candidate] > RFBClient.TIER_RANK[demoted]) {
        candidate = demoted;
      }
    }

    if (candidate === this._qualityLevel) {
      // Already at this level — reset the pending counter.
      this._pendingQuality = candidate;
      this._pendingQualityCount = 0;
      return;
    }

    if (candidate === this._pendingQuality) {
      this._pendingQualityCount++;
    } else {
      this._pendingQuality = candidate;
      this._pendingQualityCount = 1;
    }

    if (this._pendingQualityCount >= RFBClient.QUALITY_HYSTERESIS) {
      this._qualityLevel = candidate;
      this._pendingQualityCount = 0;
      // A committed tier change deliberately sends NOTHING to the server.
      // Tight quality hints (-32..-23) are a double no-op against AHV's
      // QEMU: set_encodings stores quality only under `-vnc lossy=on`
      // (libvirt builds AHV's command line and has no such knob), and the
      // only consumers — Tight JPEG and the gradient filter — are
      // additionally compiled out of the AHV build (no libjpeg). Compression
      // hints are redundant or harmful: QEMU resets Tight compression to 9
      // (best bytes) on every hint-less SetEncodings, while levels below 9
      // both lower the zlib level and shrink tight_conf's max-subrect
      // limits — compress 1 shreds a 1080p update into ~1000 subrects, each
      // with its own header and palette, costing 1.5-3x the bytes of the
      // default. So the hint-less SetEncodings sent at handshake already
      // pins the best wire setting; the tier feeds the UI badge and the
      // 16bpp SetPixelFormat demotion ConsoleView applies per CONNECTION
      // at the next (re)connect — never mid-stream (see lowColor).
      this.events.onQualityChange?.(this._qualityLevel);
    }
  }

  /**
   * Last pointer position the OPERATOR's mouse actually sent (NOT
   * synthetic transport events). Used by the convergence Reconciler
   * to restore the guest cursor after a v3 burst — the burst's last
   * coordinate is (65535, 65535), which would otherwise leave the
   * guest cursor parked in the screen's bottom-right corner. Returns
   * null before the operator has moved the mouse over the canvas.
   */
  getLastOperatorPointer(): { x: number; y: number; buttonMask: number } | null {
    return this.lastOperatorPointer;
  }

  /**
   * Send a PointerEvent to the server.
   *
   * The optional `opts.synthetic` flag marks events that come from
   * the convergence-layer transport (v3 PHY bursts) rather than the
   * operator's intent. Synthetic events are NOT recorded as the
   * "last operator pointer position" — that field is consulted by
   * the Reconciler when it needs to restore the guest cursor after
   * a burst, and we don't want the burst's own (0, 0) / interior /
   * (65535, 65535) sequence to overwrite the operator's real
   * pointer position with transport noise.
   */
  sendPointer(x: number, y: number, buttonMask: number, opts?: { synthetic?: boolean }): void {
    if (this.state !== 'normal') {
      // Count drops on window.__rfbDiag; no console output (it would log pointer
      // coordinates, and the operator's natural mouse motion can briefly race
      // the handshake at session start, spamming the console).
      rfbDiag.droppedPointerEvents++;
      return;
    }
    if (!opts?.synthetic) {
      this.lastOperatorPointer = { x: x | 0, y: y | 0, buttonMask: buttonMask & 0xff };
    }
    const buf = new ArrayBuffer(6);
    const v = new DataView(buf);
    v.setUint8(0, CLI_MSG_POINTER_EVENT);
    v.setUint8(1, buttonMask & 0xff);
    v.setUint16(2, Math.max(0, Math.min(0xffff, x | 0)));
    v.setUint16(4, Math.max(0, Math.min(0xffff, y | 0)));
    this.send(buf);
  }

  /**
   * Chunked, batched PointerEvent send.
   *
   * Pack many PointerEvents into a small number of WebSocket frames
   * (one frame per chunk) so the renderer-to-QEMU TCP path doesn't
   * see one ws.send() per event. Matches the chunking strategy
   * used by {@link sendKeyEvents} — see that method's docstring
   * for the rationale around QEMU's input queue depth.
   *
   * Caller passes `[x, y, mask]` tuples; this method handles the
   * RFB binary encoding and the chunking schedule. Aborts cleanly
   * if the client's state leaves `'normal'` mid-burst (e.g. session
   * resumed, WebSocket closed).
   *
   * Used by the Pointer-PHY v1 burst encoder. See
   * `apps/desktop/src/renderer/phy/PointerBurstV1.ts`.
   */
  sendPointerEvents(
    events: readonly (readonly [x: number, y: number, mask: number])[],
    opts: { chunkSize?: number; chunkDelayMs?: number } = {},
  ): void {
    if (this.state !== 'normal') return;
    const total = events.length;
    if (total === 0) return;

    const CHUNK_SIZE = opts.chunkSize ?? 64;
    const CHUNK_DELAY_MS = opts.chunkDelayMs ?? 10;

    const sendChunk = (start: number): void => {
      if (this.state !== 'normal') return;
      const end = Math.min(start + CHUNK_SIZE, total);
      const len = end - start;
      const buf = new ArrayBuffer(len * 6);
      const v = new DataView(buf);
      for (let j = 0; j < len; j++) {
        const off = j * 6;
        const [x, y, mask] = events[start + j]!;
        v.setUint8(off, CLI_MSG_POINTER_EVENT);
        v.setUint8(off + 1, mask & 0xff);
        v.setUint16(off + 2, Math.max(0, Math.min(0xffff, x | 0)));
        v.setUint16(off + 4, Math.max(0, Math.min(0xffff, y | 0)));
      }
      this.send(buf);
      if (end < total) {
        setTimeout(() => sendChunk(end), CHUNK_DELAY_MS);
      }
    };
    sendChunk(0);
  }

  /**
   * Send a single Unicode codepoint as an RFB KeyEvent pair
   * (down + up). The codepoint is mapped via `unicodeKeysym` (see
   * keysyms.ts) so the guest-side TranslateMessage path can emit
   * the corresponding WM_CHAR. Used by the v3 clipboard text
   * session — `docs/clipboard-convergence-v3.md`.
   *
   * Returns a boolean for caller convenience: `false` if the state
   * isn't `normal` (caller can pace, give up, or queue).
   */
  sendUnicodeChar(codepoint: number): boolean {
    if (this.state !== 'normal') return false;
    // Defer the bounds check to unicodeKeysym; let it throw on a
    // caller bug rather than silently dropping a real codepoint.
    const ks = unicodeKeysym(codepoint);
    this.sendKey(ks, true);
    this.sendKey(ks, false);
    return true;
  }

  /**
   * Send the RFB SetDesktopSize client message (RFC 6143 §7.5.5).
   * Servers that honor the ExtendedDesktopSize extension will switch
   * the guest's framebuffer to the requested dimensions; servers
   * that don't will reply with a status != 0 in the next FBU.
   *
   * The renderer also dispatches a parallel resize ANNOUNCE through
   * the convergence Reconciler so the bridge daemon can drive
   * ChangeDisplaySettingsExW from inside the guest — that path
   * works on AHV's QEMU build where the server-side handler is
   * either missing or buggy. Calling both is intentional: best-
   * effort native + reliable in-guest fallback.
   *
   * Wire format (24 bytes total, big-endian):
   *
   *   u8  type      = 251 (CLI_MSG_SET_DESKTOP_SIZE)
   *   u8  padding   = 0
   *   u16 width
   *   u16 height
   *   u8  screens   = 1
   *   u8  padding   = 0
   *   // screen #0
   *   u32 id        = 0
   *   u16 x_pos     = 0
   *   u16 y_pos     = 0
   *   u16 width
   *   u16 height
   *   u32 flags     = 0
   *
   * Caller is expected to clamp to the operator's monitor bounds
   * before dispatch — ConsoleView debounces ResizeObserver bursts
   * at 250 ms and skips out-of-range targets per
   * docs/clipboard-convergence.md §"Resize-request payload".
   */
  sendSetDesktopSize(width: number, height: number): void {
    if (this.state !== 'normal') {
      console.warn('[rfb.tx] sendSetDesktopSize BLOCKED — state not normal', {
        state: this.state,
        width,
        height,
      });
      return;
    }
    const w = Math.max(0, Math.min(0xffff, width | 0));
    const h = Math.max(0, Math.min(0xffff, height | 0));
    const buf = new ArrayBuffer(24);
    const v = new DataView(buf);
    // Header
    v.setUint8(0, CLI_MSG_SET_DESKTOP_SIZE);
    v.setUint8(1, 0); // padding
    v.setUint16(2, w, false);
    v.setUint16(4, h, false);
    v.setUint8(6, 1); // screens
    v.setUint8(7, 0); // padding
    // Screen #0
    v.setUint32(8, 0, false); // id
    v.setUint16(12, 0, false); // x_pos
    v.setUint16(14, 0, false); // y_pos
    v.setUint16(16, w, false); // width (matches header)
    v.setUint16(18, h, false); // height (matches header)
    v.setUint32(20, 0, false); // flags
    this.send(buf);
  }

  // Returns whether the cut-text was actually put on the wire. The clipboard
  // engine gates its host_to_vm ledger + success audit on this: recording
  // "delivered" when the RFB stream was halted / re-handshaking / closed both
  // loses the paste AND writes a phantom host_to_vm ledger entry that then
  // echo-suppresses a genuine guest→host copy of the same text (poisoning the
  // reverse direction). Gate on TRUE readiness, not just state==='normal'.
  sendClientCutText(text: string): boolean {
    if (this.state !== 'normal' || this.halted || this.ws.readyState !== WebSocket.OPEN)
      return false;
    const encoded = new TextEncoder().encode(text);
    // QEMU's VNC server DISCONNECTS the client for any ClientCutText over
    // 1 MiB (ui/vnc.c "exceeds our limit of 1MB" → vnc_client_error; the
    // string is present in the AHV qemu-kvm 9.1 binary) — an oversized
    // cut-text doesn't just fail, it tears down the whole console session.
    // Reject under that with margin. Legitimate traffic never gets close: the
    // ClipboardEngine routes anything past DIRECT_MAX_BYTES through the chunked
    // Big-Clipboard Protocol, whose frames are sized to the guest's REPORTED
    // receive capacity (kilobytes, not megabytes), so this backstop catches
    // protocol bugs. (The proxy's 8 MB WS read limit is the looser,
    // secondary bound the previous 4 MB value guarded.)
    const MAX_CUT_TEXT = 900 * 1024;
    if (encoded.byteLength > MAX_CUT_TEXT) {
      console.warn(
        '[rfb.tx] sendClientCutText: payload %d bytes exceeds %d cap, rejected (would drop the WS session)',
        encoded.byteLength,
        MAX_CUT_TEXT,
      );
      return false;
    }
    const buf = new ArrayBuffer(8 + encoded.byteLength);
    const v = new DataView(buf);
    v.setUint8(0, CLI_MSG_CLIENT_CUT_TEXT);
    v.setUint32(4, encoded.byteLength, false);
    new Uint8Array(buf, 8).set(encoded);
    this.send(buf);
    return true;
  }

  // ----- inbound -----

  private send(buf: ArrayBuffer): void {
    // DIAG_TODO: count every WebSocket send + log if it's blocked.
    // Remove for prod.
    rfbDiag.sendBytes += buf.byteLength;
    // Halted = the RFB stream desynced (protocolReset) and we're holding the
    // socket OPEN for the proxy's rfb_reset to recreate a fresh client. Because
    // the socket is still OPEN, readyState alone would let writes through — but
    // the upstream is misaligned, so any operator input written now is silently
    // lost into a dead stream (the "the VM didn't see my keystrokes" class).
    // Drop all sends until a fresh, non-halted client takes over. The send-side
    // public guards only check state==='normal', and protocolReset leaves state
    // 'normal', so this single low-level gate is the reliable choke point.
    if (this.halted) return;
    if (this.ws.readyState !== WebSocket.OPEN) {
      console.warn('[rfb.tx] send BLOCKED — ws not OPEN', {
        readyState: this.ws.readyState,
        bytes: buf.byteLength,
      });
      return;
    }
    this.ws.send(buf);
  }

  private onMessage = (evt: MessageEvent): void => {
    if (typeof evt.data === 'string') {
      // Control frames from the runtime proxy. Ignored at the RFB layer.
      return;
    }
    const data = evt.data as ArrayBuffer;
    rfbDiag.rxBytes += data.byteLength; // DIAG_TODO
    rfbDiag.rxMessages += 1;
    this._rxTotal += data.byteLength; // service-time byte accounting
    this.reader.push(data);
    try {
      this.drain();
    } catch (err) {
      // A throw from drain() — a pixel-decoder desync (illegal subencoding,
      // inflate-bomb guard, over-read) OR a byte hiccup while still mid-handshake
      // (notably the proxy's IN-PLACE re-handshake after an upstream drop, where
      // this freshly-recreated client sits in 'version' state) — is RECOVERABLE.
      // Route to protocolReset, which KEEPS the loopback OPEN and lets the proxy
      // re-drive via rfb_reset. The old handshake-state branch closed the ws,
      // which raced that re-handshake: the proxy saw clientClosed and stranded
      // the session at "handshake aborted — client ws closed", so the console
      // never recovered. protocolReset's fallback timer still closes for a pure
      // client-side desync with no re-handshake coming. Genuinely TERMINAL faults
      // (security/auth rejection, out-of-range dims) close explicitly in the
      // handshake state machine — not here.
      this.protocolReset((err as Error).message);
    }
  };

  // Pending continuation from a time-budgeted drain yield.
  private _drainPending = false;

  // Reused MessageChannel for drain continuations (created on first yield).
  // port.postMessage schedules a macrotask without setTimeout(0)'s ~4ms
  // clamp, so a yield costs ~0.1ms instead of a dead sleep. Both ports are
  // closed in dispose() — each rfb_reset recreates the client, and unclosed
  // ports would leak per reconnect.
  private _drainChannel: MessageChannel | null = null;

  // Chromium's isInputPending: true when an operator input event (mouse/key)
  // is queued behind us. Where the API is absent this returns false and the
  // 50ms deadline below is the only yield trigger — identical to before.
  private static inputPending(): boolean {
    if (typeof navigator === 'undefined') return false;
    const scheduling = (
      navigator as Navigator & {
        scheduling?: { isInputPending?: (opts?: { includeContinuous?: boolean }) => boolean };
      }
    ).scheduling;
    return scheduling?.isInputPending?.({ includeContinuous: true }) === true;
  }

  private drain(): void {
    // Drive the state machine until we starve, the browser reports pending
    // operator input, or we exceed the time budget. During video playback, a
    // single WebSocket frame can deliver many rects that take 20-50ms to
    // decode synchronously. Previously we yielded after 8ms to prevent
    // starving mouse/keyboard events, but because browsers clamp
    // setTimeout(..., 0) to a 4ms minimum, yielding every 8ms added massive
    // overhead (e.g. 12ms of dead sleep time for a 30ms frame). The
    // continuation now rides a MessageChannel (~0.1ms per yield), and
    // isInputPending yields the moment input is actually waiting; the 50ms
    // budget stays as the backstop where isInputPending is unavailable (or
    // a pathological 100ms+ frame arrives).
    const deadline = performance.now() + 50; // 50ms budget
    let progressed = true;
    while (progressed) {
      if (this.halted) return; // a protocolReset fired — remaining bytes are garbage
      progressed = false;
      switch (this.state) {
        case 'version':
          if (this.reader.has(12)) {
            this.reader.readBytes(12); // RFB 003.008\n from server
            this.send(new TextEncoder().encode(RFB_PROTOCOL_VERSION).buffer);
            this.state = 'security';
            progressed = true;
          }
          break;
        case 'security': {
          if (!this.reader.has(1)) break;
          const nTypes = this.reader.readU8();
          if (nTypes === 0) {
            // Failure mode: server sends reason string.
            if (!this.reader.has(4)) {
              // Need more — rewind by pushing back.
              this.reader.push(new Uint8Array([nTypes]));
              break;
            }
            const len = this.reader.readU32();
            if (!this.reader.has(len)) {
              // Cannot rewind cleanly; emit a generic error.
              this.protocolReset('Server rejected security handshake');
              return;
            }
            const msg = new TextDecoder().decode(this.reader.readBytes(len));
            // Handshake-phase failures are RECOVERABLE during the proxy's in-place
            // re-handshake: a byte-misalignment makes the freshly-recreated client
            // mis-read this phase, and an immediate close() here strands the proxy
            // mid re-handshake ("handshake aborted — client ws closed"), so the
            // console never recovered. protocolReset keeps the loopback OPEN (the
            // proxy re-drives, or its fallback timer reopens a fresh stream); when
            // no onProtocolReset is wired it falls back to onError + close, so the
            // genuinely-terminal initial-connect case is unchanged.
            this.protocolReset('Security handshake failed: ' + msg);
            return;
          }
          if (!this.reader.has(nTypes)) {
            // Rewind. We only consumed nTypes (1 byte); re-push it.
            const back = new Uint8Array([nTypes]);
            this.reader.push(back);
            break;
          }
          const types = this.reader.readBytes(nTypes);
          // Prefer None (1). The runtime proxy already authenticated upstream.
          if (!Array.from(types).includes(1)) {
            // A genuine auth requirement arrives as a typed proxy 'fatal' control
            // frame; reaching here during a re-handshake is almost always a desync,
            // so keep the loopback OPEN and recover rather than self-closing it.
            this.protocolReset('Server requires authentication AHV Remote Console cannot provide');
            return;
          }
          this.send(new Uint8Array([1]).buffer);
          this.state = 'security_result';
          progressed = true;
          break;
        }
        case 'security_result':
          if (this.reader.has(4)) {
            const result = this.reader.readU32();
            if (result !== 0) {
              this.protocolReset('Security result: failure');
              return;
            }
            // ClientInit: shared flag = 1 (allow multiple connections).
            this.send(new Uint8Array([1]).buffer);
            this.state = 'init';
            progressed = true;
          }
          break;
        case 'init': {
          // ServerInit message: 2+2+16+4+N
          if (!this.reader.has(24)) break;
          // Peek name length to size the full message.
          const headerProbe = this.reader.peekAt(20, 4);
          const nameLen =
            (headerProbe[0]! << 24) |
            (headerProbe[1]! << 16) |
            (headerProbe[2]! << 8) |
            headerProbe[3]!;
          if (!this.reader.has(24 + nameLen)) break;
          const w = this.reader.readU16();
          const h = this.reader.readU16();
          this.reader.readBytes(16); // server pixel format
          this.reader.readU32(); // already-known name length
          const name = new TextDecoder().decode(this.reader.readBytes(nameLen));
          if (w > 7680 || h > 4320 || w === 0 || h === 0) {
            this.protocolReset(`Framebuffer dimensions out of range: ${w}x${h}`);
            return;
          }
          this.fb = newFramebuffer(w, h);
          this.events.onFramebufferReady?.(this.fb);
          this.events.onResize?.(w, h);
          void name;
          this.setPreferredPixelFormat();
          this.setEncodings();
          // Transition to 'normal' BEFORE the bootstrap requestUpdate.
          // The public outbound APIs (including requestUpdate) now early-
          // return unless state === 'normal' to prevent stray callers
          // from corrupting the handshake; that gate would otherwise
          // block this very call and the server would never send the
          // initial framebuffer.
          this.state = 'normal';
          this.requestUpdate(false);
          // ContinuousUpdates enable call removed — see setEncodings()
          // for the rationale. The explicit-request pump in
          // flushUpdate drives the stream.

          // PRP (Periodic Request Period) — idle/backstop pump. The main
          // pump requests the next frame at each FBU header (see
          // handleFramebufferUpdate). When the screen goes idle and QEMU
          // stops emitting FBUs, no request is outstanding, so this short
          // periodic incremental request re-primes the pipeline — the first
          // post-idle change then isn't delayed up to a full interval. 50ms
          // (~20 Hz) matches the design intent; QEMU coalesces redundant
          // incremental requests, so over-requesting costs nothing on the wire.
          this._prpTimer ??= setInterval(() => {
            // Skip the idle re-prime while this window is hidden (minimized /
            // occluded / inactive tab): a backgrounded console has nothing to
            // display, and at 20 Hz × many open consoles this pump was the
            // dominant "whole app feels heavy" CPU drain — Chromium's
            // background throttling is deliberately disabled for the live
            // console, so nothing else was throttling it. onVisibilityChange
            // re-primes the instant the window is shown again.
            const hidden = typeof document !== 'undefined' && document.hidden;
            if (this.state === 'normal' && !hidden) {
              this.requestUpdate(true);
            }
          }, 50);
          progressed = true;
          break;
        }
        case 'normal':
          progressed = this.processServerMessage();
          // Yield to the event loop so mouse/keyboard events are not starved
          // during heavy decode bursts (video playback): immediately when
          // input is queued, at the 50ms budget otherwise.
          if (progressed && (RFBClient.inputPending() || performance.now() > deadline)) {
            this.scheduleDrainContinuation();
            return;
          }
          break;
      }
    }
  }

  /**
   * Schedule a continuation of drain() on the next event-loop tick.
   * This lets queued input events (mousemove, keydown) process between
   * decode chunks, preventing the "high ping" feel during video.
   */
  private scheduleDrainContinuation(): void {
    if (this._drainPending) return;
    this._drainPending = true;
    if (this._drainChannel === null) {
      this._drainChannel = new MessageChannel();
      this._drainChannel.port1.onmessage = () => {
        this._drainPending = false;
        if (this.halted) return;
        try {
          this.drain();
        } catch (err) {
          // Same recovery rule as the onMessage drain catch: a desync OR a
          // mid-handshake hiccup is recoverable — keep the loopback OPEN via
          // protocolReset and let the proxy re-drive; never self-close it (that
          // strands the proxy's in-place re-handshake).
          this.protocolReset((err as Error).message);
        }
      };
    }
    this._drainChannel.port2.postMessage(0);
  }

  private setPreferredPixelFormat(): void {
    const buf = new ArrayBuffer(20);
    const v = new DataView(buf);
    v.setUint8(0, CLI_MSG_SET_PIXEL_FORMAT);
    if (this.lowColor) {
      // 16-bit truecolor RGB565 little-endian — the collapsed-link mode.
      // The decoders widen to the RGBA8888 framebuffer through the 565
      // lookup table (encodings.ts). Sent once at handshake and NEVER
      // renegotiated mid-stream (see RFBClientOptions.lowColor).
      v.setUint8(4, 16); // bpp
      v.setUint8(5, 16); // depth
      v.setUint8(6, 0); // big-endian = false
      v.setUint8(7, 1); // truecolor
      v.setUint16(8, 31); // red max
      v.setUint16(10, 63); // green max
      v.setUint16(12, 31); // blue max
      v.setUint8(14, 11); // red shift
      v.setUint8(15, 5); // green shift
      v.setUint8(16, 0); // blue shift
    } else {
      // 32-bit truecolor BGRX little-endian — what the canvas blit expects.
      v.setUint8(4, 32); // bpp
      v.setUint8(5, 24); // depth
      v.setUint8(6, 0); // big-endian = false
      v.setUint8(7, 1); // truecolor
      v.setUint16(8, 255); // red max
      v.setUint16(10, 255); // green max
      v.setUint16(12, 255); // blue max
      v.setUint8(14, 16); // red shift
      v.setUint8(15, 8); // green shift
      v.setUint8(16, 0); // blue shift
    }
    this.send(buf);
  }

  private setEncodings(): void {
    // Extended Clipboard (0xC0A1E5CE) is advertised by the Go runtime
    // proxy via injection into SetEncodings, NOT by the renderer
    // directly. The proxy handles the Extended Clipboard wire format
    // (CAPS negotiation, PROVIDE decompression) and translates to
    // legacy ServerCutText for the renderer. The renderer never sends
    // negative-length ClientCutText. This avoids the earlier "typing
    // stopped" bug on AHV's older QEMU builds while still enabling
    // native clipboard via QEMU 9.1's clipboard subsystem.
    // ContinuousUpdates extension was removed after black-screen
    // reports against AHV's QEMU build — the EnableContinuousUpdates
    // client message (type 150) we paired the encoding with appears
    // to put that server's parser in a state where it stops emitting
    // FBUs entirely. Until we have evidence AHV supports the extension
    // (e.g. by capturing what Prism's own noVNC sends), stick with
    // the explicit FramebufferUpdateRequest pump that's already
    // working in production.
    // Tight is the PREFERRED pixel encoding (listed before Raw so QEMU
    // selects it): content-adaptive — solid->Fill, low-color UI->indexed
    // palette+zlib, photographic/moving->JPEG. It is what Prism's own noVNC
    // web console negotiates against this exact QEMU, which is why that console
    // is smooth where uncompressed-Raw was laggy. The decoder is all-or-nothing
    // per rect (rfb/encodings.ts decodeTight) so the stream-alignment failures
    // that disabled ZRLE+Hextile ("Unsupported subencoding 19" / edge-tile byte
    // miscount) are structurally impossible. Raw stays as a guaranteed fallback;
    // ZRLE/Hextile remain disabled (Tight subsumes them).
    const encs = [
      ENC_COPY_RECT,
      ENC_TIGHT,
      ENC_RAW,
      ENC_DESKTOP_SIZE,
      ENC_CURSOR,
      // ExtendedDesktopSize advertises that we want — and can decode —
      // ExtendedDesktopSize FBU pseudo-rects emitted by servers that
      // honor CLI_MSG_SET_DESKTOP_SIZE. AHV's QEMU build may not
      // accept the message (no in-band negotiation ack), so the bridge
      // daemon also runs ChangeDisplaySettingsExW from inside the
      // guest as a belt-and-braces backstop. Servers that don't know
      // about -308 silently ignore the announcement.
      ENC_EXTENDED_DESKTOP_SIZE,
    ];
    // Always announce the QEMU Extended Key Event pseudo-encoding (-258).
    // Servers that don't recognize it silently ignore the announcement; servers
    // that DO (AHV's QEMU — the same one Prism's own noVNC console drives) then
    // accept scancode key events from us. This is the ONLY reliable way to send
    // keys to a pre-OS firmware menu / function keys (a plain keysym has no
    // meaning before the guest keymap loads). Announcing it unconditionally is
    // safe; the keysym-vs-scancode SEND decision is still made per-caller
    // (InputEngine via extendedKeyEnabled()).
    encs.push(ENC_QEMU_EXT_KEY_EVENT);
    // Deliberately NO Tight quality/compression pseudo-encodings: against
    // AHV's QEMU a hint-less list is already optimal — see _evaluateQuality.
    const buf = new ArrayBuffer(4 + encs.length * 4);
    const v = new DataView(buf);
    v.setUint8(0, CLI_MSG_SET_ENCODINGS);
    v.setUint16(2, encs.length);
    encs.forEach((enc, i) => v.setInt32(4 + i * 4, enc));
    this.send(buf);
  }

  // Returns true if it managed to consume something.
  private processServerMessage(): boolean {
    if (this.pendingRect && this.fb) {
      const r = this.resumeRect(this.pendingRect);
      if (r === DecoderResult.NeedMore) return false;
      // spanned ≥2 frames; byte delta not meaningful
      this.finalizeDecodedRect(this.pendingRect, -1);
      this.pendingRect = null;
      this.remainingRects--;
      if (this.remainingRects > 0) return true;
      this.flushUpdate();
      return true;
    }
    if (this.remainingRects > 0 && this.fb) {
      if (!this.reader.has(12)) return false;
      const x = this.reader.readU16();
      const y = this.reader.readU16();
      const w = this.reader.readU16();
      const h = this.reader.readU16();
      const enc = this.reader.readI32();
      const rect: Rect = { x, y, w, h, encoding: enc };
      // A real pixel-drawing rect (encoding >= 0; the pseudo-encodings
      // DesktopSize/ExtendedDesktopSize/Cursor are all negative and carry
      // their own geometry) MUST lie entirely within the current framebuffer.
      // Without this guard a compromised or MITM'd upstream can advertise a
      // 65535×65535 rect whose decode loop runs billions of synchronous
      // main-thread iterations from a handful of wire bytes — a Tight Fill is
      // ~4 bytes, and Tight COPY/PALETTE/GRADIENT and ZRLE read past their
      // short payloads as silently-zero-coerced out-of-bounds typed-array
      // reads, so the full w*h loop still completes — hard-freezing the
      // renderer (the operator can't even click Disconnect). The x+w>width
      // check at requestRectUpdate is an OUTBOUND request guard and does not
      // cover this inbound decode path. fb dims are themselves clamped to
      // 7680×4320 in applyResize, so this bounds per-rect work to the screen.
      if (rectExceedsFramebuffer(rect, this.fb.width, this.fb.height)) {
        // The garbage header means a PRIOR rect left the stream misaligned.
        // Dump the recent-rect history (the last entry is the prime suspect —
        // a decoder that consumed the wrong byte count) plus a hex window of
        // the bytes immediately following this bad header, so the desync can
        // be traced from logs alone without a packet capture.
        const trail = this.recentRects
          .map((rr) => `${rr.enc}@${rr.x},${rr.y} ${rr.w}x${rr.h}=${rr.bytes}B`)
          .join(' | ');
        const peekN = Math.min(32, this.reader.available());
        const hex =
          peekN > 0
            ? Array.from(this.reader.peekAt(0, peekN))
                .map((b) => b.toString(16).padStart(2, '0'))
                .join(' ')
            : '(none buffered)';
        console.error(
          `[rfb.desync] bounds guard tripped on rect ${x},${y} ${w}x${h} enc=${enc}\n` +
            `  recent rects (oldest→newest): ${trail || '(none)'}\n` +
            `  next ${peekN}B after bad header: ${hex}`,
        );
        // Recoverable desync, not a terminal error: the oversized geometry is
        // a symptom of a prior rect's byte miscount (or a corrupt/MITM'd
        // frame), and the decode-amplification guard already refused to act on
        // it. A fresh handshake realigns the stream, so route through the
        // host's reconnect rather than killing the console.
        this.protocolReset(
          `RFB rect ${x},${y} ${w}x${h} exceeds framebuffer ` +
            `${this.fb.width}x${this.fb.height} — stream desync, realigning`,
        );
        return false;
      }
      if (enc === ENC_DESKTOP_SIZE) {
        // No payload — new dimensions live in the rect header itself.
        this.recordRect(rect, 0);
        this.applyResize(rect);
        this.remainingRects--;
        if (this.remainingRects === 0) this.flushUpdate();
        return true;
      }
      const availBefore = this.reader.available();
      const result = this.resumeRect(rect);
      if (result === DecoderResult.NeedMore) {
        this.pendingRect = rect;
        return false;
      }
      this.finalizeDecodedRect(rect, availBefore - this.reader.available());
      this.remainingRects--;
      if (this.remainingRects === 0) this.flushUpdate();
      return true;
    }

    if (!this.reader.has(1)) return false;
    const msgType = this.reader.peekU8();
    // Per-type handlers. The dispatch order + each handler's side effects
    // are identical to the inline switch this replaced — the bodies were
    // moved verbatim into the handle* methods below; only an unknown
    // msgType falls through to the defensive skip-one-byte default.
    switch (msgType) {
      case SRV_MSG_FRAMEBUFFER_UPDATE:
        return this.handleFramebufferUpdate();
      case SRV_MSG_BELL:
        return this.handleBell();
      case SRV_MSG_SERVER_CUT_TEXT:
        return this.handleServerCutText();
      case SRV_MSG_SET_COLOUR_MAP_ENTRIES:
        return this.handleSetColourMapEntries();
      default:
        // Unknown message — skip a byte and hope for resync. The runtime
        // proxy should not be forwarding garbage, so this is purely defensive.
        this.reader.readU8();
        return true;
    }
  }

  /**
   * Shared finalize for a fully-decoded rect: record it in the diagnostic
   * ring, push it into the FBU accumulator, fan it out to per-rect
   * subscribers, and fire onRectDecoded for real (non-pseudo) encodings.
   * Moved verbatim from the two rect-completion paths in
   * processServerMessage; `bytes` is the synchronous payload consumption
   * (-1 for rects that spanned ≥2 WebSocket frames). The caller still owns
   * the surrounding remainingRects-- / flushUpdate sequencing.
   */
  private finalizeDecodedRect(rect: Rect, bytes: number): void {
    this.recordRect(rect, bytes);
    this.rectAccumulator.push(rect);
    this.fireFramebufferUpdate(rect);
    if (rect.encoding >= 0) {
      this.events.onRectDecoded?.(rect);
    }
  }

  // ── Per-server-message-type handlers ────────────────────────────────
  //
  // Each returns true if it consumed something (the parser loops), false
  // if it needs more bytes (NeedMore) or terminated. Bodies were moved
  // verbatim from the message-type switch in processServerMessage.

  private handleFramebufferUpdate(): boolean {
    if (!this.reader.has(4)) return false;
    this.reader.readU8(); // msg
    this.reader.readU8(); // pad
    this.remainingRects = this.reader.readU16();
    this.inflateBudget.reset(); // fresh per-FBU decompression budget
    // Service-time signal: stamp the header and snapshot the consumed-byte
    // counter; flushUpdate closes the sample when the last rect lands.
    this._fbuStartTs = performance.now();
    this._fbuStartConsumed = this._consumedBytes();
    if (this.remainingRects === 0) this.flushUpdate();
    return true;
  }

  private handleBell(): boolean {
    this.reader.readU8();
    this.events.onBell?.();
    return true;
  }

  private handleServerCutText(): boolean {
    if (!this.reader.has(8)) return false;
    // The length field is *signed*. A positive value is a legacy
    // plain-text payload; a negative value would be the Extended
    // Clipboard pseudo-encoding, which we no longer parse — the
    // v3 convergence Reconciler owns guest→host clipboard via
    // pixel-strip ANNOUNCEs. Extended payloads are still
    // length-consumed so the parser stays byte-aligned with the
    // wire (AHV-QEMU doesn't emit them without a vdagent, but a
    // future server might).
    const lenProbe = this.reader.peekAt(4, 4);
    const rawLen =
      (lenProbe[0]! << 24) | (lenProbe[1]! << 16) | (lenProbe[2]! << 8) | lenProbe[3]! | 0;
    const isExtended = rawLen < 0;
    const payloadLen = isExtended ? -rawLen : rawLen;
    // Reject an extended payload whose declared length exceeds the proxy's
    // 16 MB read cap. A huge-negative rawLen (e.g. INT32_MIN) yields a
    // payloadLen of ~2 GB that `has(8 + payloadLen)` can never satisfy, so
    // without this the parser sits in NeedMore forever — a silent hang on a
    // malformed/hostile frame. The proxy can never legitimately forward this
    // much, so treat it as a terminal protocol fault and tear down.
    const MAX_CUT_TEXT = 16 * 1024 * 1024;
    if (isExtended && payloadLen > MAX_CUT_TEXT) {
      this.events.onError?.(
        new Error(
          `ServerCutText extended payload length ${payloadLen} exceeds ${MAX_CUT_TEXT} cap`,
        ),
      );
      this.close();
      return false;
    }
    if (!this.reader.has(8 + payloadLen)) return false;
    this.reader.readU8(); // msg
    this.reader.readBytes(3); // padding
    this.reader.readU32(); // length (consumed)
    const payload = this.reader.readBytes(payloadLen);
    if (isExtended) {
      // Extended clipboard handled by proxy — ignore here.
    } else {
      // fatal:true so a truncated/corrupt payload (e.g. a vdagent PROVIDE
      // clipped at the proxy's 16 MB decompress cap) throws instead of
      // silently landing U+FFFD replacement chars on the host clipboard.
      try {
        const text = new TextDecoder('utf-8', { fatal: true }).decode(payload);
        this.events.onClipboard?.(text);
      } catch {
        console.warn(
          '[rfb.rx] ServerCutText: payload is not valid UTF-8 — dropped (truncated/corrupt)',
        );
      }
    }
    return true;
  }

  private handleSetColourMapEntries(): boolean {
    // SetColourMapEntries (RFC 6143 §7.6.2). We negotiate a truecolor
    // pixel format (setPreferredPixelFormat) so a colour map is never
    // semantically meaningful to us — but a server may still emit one, and
    // it is a VARIABLE-length message. Falling through to the default
    // "skip one byte" case would mis-consume it and desync the stream
    // (the next msgType byte would land mid-colour-map). Read the 6-byte
    // header (type + 1 pad + u16 firstColour + u16 nColours), then discard
    // the nColours*6 body (each entry is 3 × u16: red, green, blue).
    if (!this.reader.has(6)) return false;
    const cmHeader = this.reader.peekAt(0, 6);
    const nColours = (cmHeader[4]! << 8) | cmHeader[5]!;
    const cmTotal = 6 + nColours * 6;
    if (!this.reader.has(cmTotal)) return false;
    this.reader.readBytes(cmTotal); // header + body, discarded (truecolor)
    return true;
  }

  /**
   * Finish a server FramebufferUpdate: hand the painted rects to the
   * renderer, then immediately request the next incremental update.
   *
   * RFC 6143 §7.5.3: the server only sends a FramebufferUpdate in
   * response to a FramebufferUpdateRequest. Without this pump the
   * client receives exactly one snapshot (the initial non-incremental
   * request from the init state) and then the screen stays frozen —
   * cursors don't blink, console output doesn't appear, keystrokes
   * don't echo back. noVNC and every other RFB client run this same
   * request/update loop.
   */
  private flushUpdate(): void {
    if (this.rectAccumulator.length > 0) {
      this.events.onUpdate?.(this.rectAccumulator);
      this.rectAccumulator = [];
    }
    // FBU complete — close out its service-time sample BEFORE re-priming
    // the pump (the next request must not extend this frame's measurement).
    this._recordFbuServiceTime();
    this.requestUpdate(true);
  }

  // Async JPEG blit for Tight. The decoder consumes the JPEG bytes
  // synchronously (keeping the stream aligned) and hands them here.
  //
  // FAST PATH (onDirectImage wired): decode via createImageBitmap, then paint
  // the ImageBitmap directly onto the visible canvas with drawImage. This skips
  // the entire fb.pixels roundtrip (scratch canvas → getImageData → row-by-row
  // copy → putImageData) that made JPEG the slowest decode path during video.
  //
  // FALLBACK PATH (onDirectImage not wired): same as before — decode to scratch
  // canvas, copy into fb.pixels, trigger onUpdate for putImageData paint.
  private readonly handleTightImage = (rect: Rect, jpeg: Uint8Array): void => {
    const fb = this.fb;
    if (!fb) return;
    const dims = readJpegDimensions(jpeg);
    if (dims?.w !== rect.w || dims?.h !== rect.h) {
      console.warn(
        `[rfb.tight] dropping JPEG rect: declared ${dims ? `${dims.w}x${dims.h}` : 'unparseable'} ` +
          `≠ rect ${rect.w}x${rect.h}`,
      );
      return;
    }
    const blob = new Blob([new Uint8Array(jpeg)], { type: 'image/jpeg' });
    createImageBitmap(blob)
      .then((bmp) => {
        if (this.fb !== fb) {
          bmp.close();
          return;
        }
        // Fast path: paint the bitmap directly to the canvas.
        if (this.events.onDirectImage) {
          this.events.onDirectImage(bmp, rect.x, rect.y);
          bmp.close();
          return;
        }
        // Fallback: copy into fb.pixels for putImageData paint.
        let c = this.tightScratch;
        if (!c) {
          c = document.createElement('canvas');
          this.tightScratch = c;
        }
        if (c.width < rect.w) c.width = rect.w;
        if (c.height < rect.h) c.height = rect.h;
        const ctx = c.getContext('2d', { willReadFrequently: true });
        if (!ctx) {
          bmp.close();
          return;
        }
        ctx.drawImage(bmp, 0, 0);
        bmp.close();
        const src = ctx.getImageData(0, 0, rect.w, rect.h).data;
        const dst = fb.pixels;
        const srcStride = rect.w * 4;
        const dstStride = fb.width * 4;
        for (let row = 0; row < rect.h; row++) {
          const sOff = row * srcStride;
          const dOff = (rect.y + row) * dstStride + rect.x * 4;
          dst.set(src.subarray(sOff, sOff + srcStride), dOff);
        }
        this.events.onUpdate?.([rect]);
      })
      .catch(() => {
        /* malformed JPEG — keep prior pixels; a later FBU repaints */
      });
  };

  private resumeRect(rect: Rect & { _hex?: unknown }): DecoderResult {
    if (!this.fb) return DecoderResult.Done;
    switch (rect.encoding) {
      case ENC_RAW:
        return decodeRaw(this.reader, this.fb, rect, this.lowColor);
      case ENC_COPY_RECT:
        // Depth-agnostic: CopyRect moves pixels WITHIN the RGBA8888
        // framebuffer (4 src-position bytes on the wire, no pixel data), so
        // it needs no lowColor plumbing.
        return decodeCopyRect(this.reader, this.fb, rect);
      case ENC_HEXTILE:
        return decodeHextile(this.reader, this.fb, rect);
      case ENC_ZRLE:
        return decodeZRLE(this.reader, this.fb, rect, this.zrleInflater);
      case ENC_TIGHT:
        return decodeTight(
          this.reader,
          this.fb,
          rect,
          this.tightInflater,
          this.handleTightImage,
          this.lowColor,
        );
      case ENC_CURSOR:
        // Pseudo-encoding: cursor pixmap + mask (RFC 6143 §7.7.2).
        // Decoded + handed to the renderer overlay so the operator
        // sees the guest's cursor shape painted locally at the live
        // pointer position (VMware-Tools-style smoothness).
        return this.decodeCursorEncoding(rect);
      case ENC_EXTENDED_DESKTOP_SIZE:
        // Pseudo-encoding: server confirms a resolution change. The
        // rect's (w, h) carries the new framebuffer size; the body
        // carries per-screen metadata we don't use. We MUST consume
        // the body bytes or the next rect's header reads garbage —
        // exact bug that produced "Unsupported RFB encoding: 242"
        // in the field. Body layout per RFC 6143 §7.7.2:
        //
        //   number-of-screens : u8
        //   padding           : u8 × 3
        //   per screen (16 B each):
        //     id              : u32
        //     x_pos, y_pos    : u16, u16
        //     width, height   : u16, u16
        //     flags           : u32
        //
        // We need 4 + N*16 bytes total. Returning NeedMore on partial
        // arrival lets the parser re-enter once the wire has the
        // rest.
        return this.decodeExtendedDesktopSize(rect);
      case ENC_QEMU_EXT_KEY_EVENT:
        // QEMU acknowledges our advertised Extended Key Event pseudo-encoding
        // (-258, pushed in setEncodings) by sending a FramebufferUpdate that
        // contains ONE bodyless pseudo-rect at 0,0,client_width,client_height
        // (QEMU ui/vnc.c send_ext_key_event_ack). It carries NO pixel/data
        // payload — the rect header IS the whole message — so we consume nothing
        // beyond the already-read header and treat it as a no-op acknowledgment.
        // Before this case it fell through to "unsupported encoding", which
        // protocolReset the stream on the very FIRST frame (the ack precedes the
        // first screen FBU) → black screen + endless reconnect. The geometry is
        // the full framebuffer, so it harmlessly passes the bounds guard.
        //
        // The ack also CONFIRMS QEMU accepts ext-key frames, so switch to
        // scancode passthrough from here on: the guest applies its own keymap to
        // the raw scancode, which is correct for ANY guest layout and bypasses
        // the en-us keysym translation that corrupts non-en-us guests.
        this.extKeyAcked = true;
        return DecoderResult.Done;
      default: {
        // Unknown encoding — a prior rect's byte miscount left the stream
        // misaligned, so this "encoding" is really pixel bytes. Surface the
        // value in decimal + hex with rect geometry for log-only diagnosis,
        // then realign via the host's reconnect (same recoverable desync as
        // the bounds guard) instead of limping on with a terminal error.
        const enc = rect.encoding;
        const hex = (enc >>> 0).toString(16).padStart(8, '0');
        const geo = `${rect.x},${rect.y} ${rect.w}x${rect.h}`;
        this.protocolReset(
          `Unsupported RFB encoding: ${enc} (0x${hex}) at rect ${geo} — ` +
            `stream desync, realigning`,
        );
        return DecoderResult.Done;
      }
    }
  }

  /**
   * Consume the ExtendedDesktopSize pseudo-encoding body and apply
   * the new framebuffer dimensions. The rect's outer (w, h) is the
   * authoritative new size; we still parse the per-screen array to
   * stay byte-aligned with the wire, but discard its contents.
   */
  private decodeExtendedDesktopSize(rect: Rect): DecoderResult {
    if (!this.reader.has(4)) return DecoderResult.NeedMore;
    // Peek the screen count without consuming yet (defensive: if the
    // body is short we'll see NeedMore on the second has() call).
    const screensU8 = this.reader.peekAt(0, 1)[0]!;
    const totalBody = 4 + screensU8 * 16;
    if (!this.reader.has(totalBody)) return DecoderResult.NeedMore;
    // Consume the body — we don't actually use the per-screen
    // metadata; the outer rect's (w, h) is what matters.
    this.reader.readBytes(totalBody);
    this.applyResize(rect);
    return DecoderResult.Done;
  }

  /**
   * Parse the RFB Cursor pseudo-encoding (RFC 6143 §7.7.2). The
   * rect's `(x, y)` are the cursor's hotspot; `(w, h)` are the
   * pixmap dimensions. Body bytes:
   *
   *   1. `w * h * 4` bytes of pixel data in the negotiated pixel
   *      format. After setPreferredPixelFormat() the server sends
   *      pixels in BGRX little-endian, matching decodeRaw's
   *      assumptions — we read B, G, R, X per pixel and reorder to
   *      canvas-native RGBA.
   *   2. `ceil(w / 8) * h` mask bytes, MSB-first per row. Bit i in
   *      byte `row * rowBytes + (i / 8)` at bit `7 - (i % 8)` is
   *      the alpha selector for pixel `(i, row)`: 1 = opaque, 0 =
   *      transparent.
   *
   * Output layout: width*height*4 RGBA bytes with alpha pre-baked
   * from the mask, plus a separate width*height alpha array.
   * Either an existing alpha channel in the pixel data is ignored
   * (the X byte) or, equivalently, the mask wins — this is what
   * every other RFB client (noVNC, RealVNC, TightVNC) does.
   *
   * Zero-sized cursors (w=0 or h=0) are legal — the server uses
   * them to mean "hide the cursor." We deliver a zero-byte shape
   * so the overlay can suppress drawing.
   */
  private decodeCursorEncoding(rect: Rect): DecoderResult {
    const w = rect.w;
    const h = rect.h;
    // Pixels arrive in the NEGOTIATED format: 2-byte RGB565 in lowColor mode,
    // 4-byte BGRX otherwise. (AHV's std-VGA QEMU never sends this pseudo-
    // encoding, but the byte count must track the format or a server that
    // does would desync the stream.)
    const wireBpp = this.lowColor ? 2 : 4;
    const pixelBytes = w * h * wireBpp;
    const maskRowBytes = Math.ceil(w / 8);
    const maskBytes = maskRowBytes * h;
    const total = pixelBytes + maskBytes;
    if (!this.reader.has(total)) return DecoderResult.NeedMore;

    // Zero-sized cursor → server is requesting cursor-hide. Consume
    // any padding bytes (defensive — total should be 0 here) and
    // fire onCursorChange with a degenerate shape the overlay can
    // detect via width === 0.
    if (w === 0 || h === 0) {
      if (total > 0) this.reader.readBytes(total);
      const empty: CursorShape = {
        width: w,
        height: h,
        hotspotX: rect.x,
        hotspotY: rect.y,
        rgba: new Uint8ClampedArray(0),
        alpha: new Uint8Array(0),
      };
      this.cursorShape = empty;
      try {
        this.events.onCursorChange?.(empty);
      } catch {
        /* subscriber crash never breaks the protocol loop */
      }
      return DecoderResult.Done;
    }

    const pixelData = this.reader.readBytes(pixelBytes);
    const maskData = this.reader.readBytes(maskBytes);

    const rgba = new Uint8ClampedArray(w * h * 4);
    const alpha = new Uint8Array(w * h);

    for (let row = 0; row < h; row++) {
      const rowMaskOff = row * maskRowBytes;
      for (let col = 0; col < w; col++) {
        const pi = (row * w + col) * 4;
        // Wire pixel → R,G,B. 32bpp: BGRX little-endian (matches the
        // blitBGRX path in encodings.ts; X byte ignored — mask is the
        // source of truth). 16bpp: RGB565 little-endian via the 565 LUT.
        let r: number, g: number, b: number;
        if (this.lowColor) {
          [r, g, b] = read565Pixel(pixelData, (row * w + col) * 2);
        } else {
          b = pixelData[pi]!;
          g = pixelData[pi + 1]!;
          r = pixelData[pi + 2]!;
        }
        const maskByte = maskData[rowMaskOff + (col >> 3)]!;
        const bit = (maskByte >> (7 - (col & 7))) & 1;
        const a = bit === 1 ? 255 : 0;
        rgba[pi] = r;
        rgba[pi + 1] = g;
        rgba[pi + 2] = b;
        rgba[pi + 3] = a;
        alpha[row * w + col] = a;
      }
    }

    const shape: CursorShape = {
      width: w,
      height: h,
      hotspotX: rect.x,
      hotspotY: rect.y,
      rgba,
      alpha,
    };
    this.cursorShape = shape;
    try {
      this.events.onCursorChange?.(shape);
    } catch {
      /* subscriber crash never breaks the protocol loop */
    }
    return DecoderResult.Done;
  }

  private applyResize(rect: Rect): void {
    if (!this.fb) return;
    const w = rect.w;
    const h = rect.h;
    if (w === 0 || h === 0) return;
    if (w > 7680 || h > 4320) {
      this.events.onError?.(new Error(`Resize dimensions out of range: ${w}x${h}`));
      this.close();
      return;
    }
    if (w === this.fb.width && h === this.fb.height) return;
    this.fb = newFramebuffer(w, h);
    this.events.onFramebufferReady?.(this.fb);
    this.events.onResize?.(w, h);
    this.requestUpdate(false);
  }
}

export type { Framebuffer, Rect };
