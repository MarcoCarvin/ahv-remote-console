/**
 * ConsoleView — renders a single active session.
 *
 * Owns:
 *   - the canvas that displays the framebuffer.
 *   - mouse capture via Pointer Lock with Ctrl+Alt release.
 *   - keyboard pass-through with OS-shortcut suppression while captured.
 *   - clipboard bridge (host → guest via cut-text, guest → host via the
 *     onClipboard callback).
 *   - dynamic resize: when the window changes size, request a guest-side
 *     framebuffer resize via ExtendedDesktopSize.
 */

import { useCallback, useEffect, useMemo, useRef, useState } from 'react';
import { useResizeObserver, useToast } from '@ahv-remote-console/ui';
import { computeCanvasGeometry } from './canvasGeometry';
import { useAgent, type AgentMountState } from './useAgent';
import { serialCopy, applyRestartGate, type SerialNotice } from './serialCopy';
import { capabilityGate, useEndpointCapabilities, CAPABILITY_NOTICE_COPY } from './capabilityGate';
import { useConsoleMotion } from './console-motion';
import { RFBClient } from '../rfb/client';
import type { QualityLevel, Rect } from '../rfb/client';
import { createInputEngine, type InputEngine } from '../input/InputEngine';
import { layoutSnapshot } from '../input/Layout';
import { hostOS } from '../input/HostOS';
import { createClipboardEngine, type ClipboardEngine } from '../clipboard/ClipboardEngine';
import { useFullscreen, useWindowFullscreen } from '../presence/FullscreenToggle';
import { LOCK_COMBO_KEYSYMS } from '../input/lockCombos';
import { useFitMode } from '../presence/DisplayFitMenu';
import { useKeyboardLayout } from '../presence/KeyboardLayoutMenu';
import { useFeatureFlags } from '../presence/useFeatureFlags';
import { useShortcutsOverlay, ShortcutsOverlay } from '../presence/ShortcutsOverlay';
import { useClient } from '../app/ClientContext';
import { useTasks } from '../app/useTasks';
import { getEndpointMeta, setEndpointMeta } from '../app/recentStore';
import { type ConsoleStatus, type ConsoleErrorCause, normalizeErrorCause } from './console/status';
import { createConsoleFocusPolicy, type ConsoleFocusPolicy } from './consoleFocusPolicy';
import {
  buttonForBrowser,
  cursorShapeToCss,
  paintFull,
  paintRects,
  paintBitmap,
} from './console/framebufferCanvas';
import { ErrorOverlayBody } from './console/ConsoleOverlays';
import { AmbientLine, TopRail } from './console/ConsoleRails';
import { useChromeReveal } from './console/useChromeReveal';
import { CaptureIndicator } from './console/CaptureIndicator';
import { ConsoleTaskBar } from './console/ConsoleTaskBar';
import { StatsPanel } from './console/StatsPanel';
import { SnapshotsPanel } from './SnapshotsPanel';
import { Overlay } from './console/Overlay';
import type { VMStats } from '@ahv-remote-console/sdk';
import type { ClusterMenuItem } from './FloatingCluster';

// Re-export the status types so existing importers (ConsoleWindowApp) keep
// `import { ConsoleView, type ConsoleStatus } from '../components/ConsoleView'`.
export type { ConsoleStatus, ConsoleErrorCause };

export interface ConsoleViewProps {
  sessionId: string;
  vmName: string;
  vmUuid?: string;
  /** Active when this is the foregrounded tab. */
  active: boolean;
  /** True when this view fills a pop-out console window (ConsoleWindowApp).
   *  Drives focus auto-capture: a pop-out focuses the canvas on connect and
   *  on window focus so typing works without a click; a view embedded in the
   *  main window must never steal focus, so it stays opt-in via click. */
  popout?: boolean;
  /** WebSocket already opened against the runtime's session-stream endpoint. */
  ws: WebSocket;
  onClose: () => void;
  onStatusChange?: (status: ConsoleStatus) => void;
  /** Manually re-open the session stream — the error overlay's "Reconnect"
   *  action for an `unreachable` failure. */
  onReconnect?: () => void;
  /** Re-authenticate to Prism — the error overlay's "Re-authenticate" action
   *  for a `session_expired` failure. Re-runs the embedded SSO flow, then
   *  reconnects in place (wired by the host window). */
  onReauth?: () => void;
}

export function ConsoleView({
  sessionId,
  vmName,
  vmUuid,
  active,
  popout = false,
  ws,
  onClose,
  onStatusChange,
  onReconnect,
  onReauth,
}: ConsoleViewProps) {
  // The in-guest bridge receives clipboard via the vdagent serial
  // channel (COM3). Every Mac clipboard change is mirrored into the
  // guest pasteboard automatically via ClientCutText → QEMU → vdagent.
  const clientRef = useRef<RFBClient | null>(null);
  // Set by the RFB-setup effect; called from the control-frame handler on an
  // `rfb_reset` (proxy re-handshaked a reconnected upstream) to tear down and
  // recreate the RFBClient so it re-runs the RFB handshake from `version`.
  const recreateRfbClientRef = useRef<(() => void) | null>(null);
  // Last committed adaptive-quality tier, persisted across RFB client
  // recreations (reconnect / proxy rfb_reset) within this window session.
  // Updated by onQualityChange; read at each client construction to pick the
  // per-CONNECTION pixel format (lowColor) and to seed the fresh client's
  // machine — without the seed, a client restarted at the default 'high'
  // would never re-emit 'high' on a recovered link, leaving this ref pinned
  // low forever.
  const qualityTierRef = useRef<QualityLevel>('high');
  // Seed-once latch for the per-endpoint link-tier seed (see the first client
  // construction): connection #1 opens at the endpoint's last-known pixel format
  // so a chronically slow-but-stable WAN endpoint doesn't pay 32bpp all session.
  const qualitySeededRef = useRef(false);
  // Self-heal support: the previously-seen WebSocket (to detect a Shell-driven
  // swap on loopback reopen) and the time the last control frame arrived (a
  // frame = the loopback is alive; used by the reconnecting watchdog to tell a
  // healthy upstream-reconnect from a dead socket).
  const prevWsRef = useRef<WebSocket | null>(null);
  const lastFrameAtRef = useRef(0);
  // Churn-proof reconnect deadline. The per-status watchdogs below are keyed on
  // status.kind, so a fast self-heal reopen loop (status flipping
  // connecting⇄reconnecting every ~1s) tears them down before they can fire and
  // an unrecoverable reconnect would spin forever. unhealthySinceRef marks when
  // we LEFT a healthy 'connected' state; gaveUpRef latches the terminal give-up
  // so a background self-heal socket swap can't silently un-stick the overlay.
  const unhealthySinceRef = useRef<number | null>(null);
  const gaveUpRef = useRef(false);
  const sdkClient = useClient();
  // Each pop-out console window has its own SDK client, so it polls + shows its
  // OWN Tasks tray (power / serial / agent-ISO ops fired from this console).
  const tasksController = useTasks(sdkClient);
  // Route every console notice through the ONE shared, seamless toast surface
  // (ToastProvider mounted by ConsoleWindowApp) instead of a second bespoke
  // bottom-right system. The adapter keeps the existing toast(text, kind) call
  // signature (incl. the SnapshotsPanel `toast` prop) so call sites are
  // unchanged; it only maps the kind vocabulary (ok→success, err→error).
  const toastCtx = useToast();
  const toast = useCallback(
    (text: string, kind: 'info' | 'ok' | 'err' = 'info') =>
      toastCtx.push(kind === 'ok' ? 'success' : kind === 'err' ? 'error' : 'info', text),
    [toastCtx],
  );

  // QEMU Extended Key Event: send raw scancodes so the GUEST applies its own
  // keyboard layout / input language (the RDP model). DEFAULT OFF and OPT-IN:
  // AHV's QEMU mis-handles CLI_MSG_QEMU (msg 255) and corrupts the RFB stream
  // ("Unsupported RFB encoding: -258 … stream misaligned" → the console can't
  // connect), so we only enable it when the `qemu_ext_key_event` runtime flag
  // is EXPLICITLY true (set after validating against the cluster's QEMU).
  // With it off we use the keysym path, where most fixes still apply (dead
  // keys, AltGr/Option escape, Cmd→Ctrl, Clear/numpad keysyms).
  const featureFlags = useFeatureFlags(sdkClient);
  const extKeyEnabled = featureFlags.qemu_ext_key_event === true;
  const extKeyEnabledRef = useRef(extKeyEnabled);
  useEffect(() => {
    extKeyEnabledRef.current = extKeyEnabled;
  }, [extKeyEnabled]);
  // Reliable large clipboard paste (opt-in). Held in a ref because the clipboard
  // engine is created once (below) but the flag arrives asynchronously; the
  // engine reads it freshly per paste via reprobeCapacity().
  const largePasteEnabled = featureFlags.clipboard_large_paste === true;
  const largePasteEnabledRef = useRef(largePasteEnabled);
  useEffect(() => {
    largePasteEnabledRef.current = largePasteEnabled;
  }, [largePasteEnabled]);

  const agent = useAgent({
    clientRef,
    active,
    sessionId,
  });
  // Narrow the mount state once so the staged progress banner (and its segment
  // loop) keep the 'mounting' type inside JSX closures.
  const mounting = agent.mountState.kind === 'mounting' ? agent.mountState : null;
  // Keep the latest onStatusChange in a ref so effects can call it without
  // listing the callback in their dependency array. Inline arrow-function
  // props produce a new reference on every parent render; depending on them
  // turns "status changed" effects into a setState loop with the parent.
  const onStatusChangeRef = useRef(onStatusChange);
  useEffect(() => {
    onStatusChangeRef.current = onStatusChange;
  }, [onStatusChange]);

  // Surface the long-running agent-install task in the Tasks tray. The task
  // poller only re-polls while something is already RUNNING, so a task created
  // after an idle poll (the install fires from an otherwise-idle console) stays
  // invisible until an explicit refresh — the same kick powerAction does. The
  // mount is one blocking call, so refresh on entering 'mounting' and once more
  // shortly after, to bridge the few ms before the runtime registers the task.
  // Without this the "Installing agent…" banner pointed the operator at an empty
  // tray. The ref keeps the effect firing only on the transition (the poller's
  // refresh identity changes each render).
  const isMounting = agent.mountState.kind === 'mounting';
  const tasksRefreshRef = useRef(tasksController.refresh);
  tasksRefreshRef.current = tasksController.refresh;
  useEffect(() => {
    if (!isMounting) return;
    tasksRefreshRef.current();
    const t = setTimeout(() => tasksRefreshRef.current(), 1500);
    return () => clearTimeout(t);
  }, [isMounting]);

  const canvasRef = useRef<HTMLCanvasElement>(null);
  // The console root element — the positioning context for the canvas
  // container, rails, and overlays.
  const rootRef = useRef<HTMLDivElement>(null);
  const [containerRef, containerSize] = useResizeObserver<HTMLDivElement>();
  const [fbSize, setFbSize] = useState<{ width: number; height: number } | null>(null);
  const [status, setStatus] = useState<ConsoleStatus>({ kind: 'connecting' });
  // ----- guest cursor overlay -----
  //
  // VMware-Tools-style cursor smoothness: parse the guest's cursor
  // shape from ENC_CURSOR, hide the OS cursor over the canvas, and
  // paint the parsed pixmap locally at the live pointer position
  // every animation frame. Operator sees zero RFB round-trip
  // latency on cursor motion.
  //
  // VMware-Tools-style cursor: when the server sends ENC_CURSOR
  // (guest cursor shape), we convert the RGBA pixmap to a CSS
  // cursor URL and let the OS render it at native speed — zero JS
  // latency, zero rAF delay, zero canvas painting.
  const [cssCursor, setCssCursor] = useState<string | null>(null);
  // Sticky, operator-facing notice for the bridge-prerequisite (COM3 serial
  // port) provisioning outcome. This does NOT
  // auto-dismiss for info/warn/error states — the consent-restart dialog, the
  // deferred badge, and "couldn't set up the agent channel" must stay until
  // the operator acts on them (the deferred badge survives the session and is
  // only replaced by a later note). Previously the serialNote arrived in
  // agent.mountState but was never rendered, so the operator was never told
  // whether the channel existed or that a power-cycle was required (the
  // reported gap).
  const [installNotice, setInstallNotice] = useState<SerialNotice | null>(null);
  // Capture state is just "does the canvas have focus." Replaces the older
  // pointer-lock-based useMouseCapture which never activated in Electron's
  // renderer (Chromium rejected requestPointerLock with a SecurityError).
  // With focus-based capture: clicking the canvas focuses it, keystrokes
  // attached to the canvas element flow to the VM, Ctrl+Alt blurs it.
  const [captured, setCaptured] = useState(false);
  // Keyboard LOCK mode (VMware-style WINDOWED auto-grab). `locked` is true only
  // while the native main-process keyboard grab is actually armed for this
  // window — set from the setLockGrab() result, so it stays false (no banner)
  // on Wayland / macOS / a box without the native addon, where the grab can't
  // engage and host-reserved combos stay on the Send Keys menu. locked implies
  // captured. lockedRef mirrors it for event handlers without re-binding.
  const [locked, setLocked] = useState(false);
  const lockedRef = useRef(false);
  lockedRef.current = locked;
  const fullscreen = useFullscreen();
  // Full-bleed when EITHER fullscreen flavor is active: DOM element-fullscreen
  // (tracked by fullscreenchange) or NATIVE window fullscreen (green button /
  // F11 → win.setFullScreen, pushed from main). In full-bleed the canvas drops
  // its rail margins and the hover-reveal rails overlay the guest — standard
  // fullscreen-console chrome — instead of leaving dead bands where the rails
  // would dock.
  const windowFullscreen = useWindowFullscreen();
  const fullBleed = fullscreen.isFullscreen || windowFullscreen;
  const fitModeState = useFitMode(sessionId);
  const kbdLayout = useKeyboardLayout(sessionId);
  const shortcuts = useShortcutsOverlay(captured);

  // Backgrounded = window/tab hidden. Drives the idle motion state (C12) and
  // gates ambient layers so we don't burn frames animating an unseen console.
  const [backgrounded, setBackgrounded] = useState(() => document.hidden);
  useEffect(() => {
    const onVis = () => setBackgrounded(document.hidden);
    document.addEventListener('visibilitychange', onVis);
    return () => document.removeEventListener('visibilitychange', onVis);
  }, []);

  // Active-console motion (C1–C12): derive the per-layer flags + reduced-motion
  // gate from the live signals. Each layer below mounts behind its flag.
  const motion = useConsoleMotion({
    status:
      status.kind === 'connected'
        ? {
            kind: 'connected',
            latencyMs: status.latencyMs,
            bytesIn: status.bytesIn,
            bytesOut: status.bytesOut,
          }
        : // 'starting' (VM booting) animates like 'connecting' — the console is
          // coming up; the motion layer has no separate starting state.
          { kind: status.kind === 'starting' ? 'connecting' : status.kind },
    captured,
    fullscreen: fullscreen.isFullscreen,
    backgrounded,
  });

  // ----- RFB client lifecycle -----

  useEffect(() => {
    // If the WebSocket was SWAPPED (Shell reopened the loopback stream after
    // an unexpected drop — the self-heal), this is a fresh connection: reset
    // to 'connecting' so the new stream's frames drive us back to 'connected'.
    // Without this, a prior 'reconnecting'/'error' would stick and the
    // frame-fallback (which only clears 'connecting') couldn't recover. Skips
    // the initial mount and StrictMode's same-ws re-run (only fires on a real
    // ws change).
    // Skip the reset once we've terminally given up (the churn-proof deadline
    // below latched gaveUpRef): a background self-heal socket swap must not
    // silently un-stick the error overlay. An explicit Retry clears the latch.
    if (prevWsRef.current !== null && prevWsRef.current !== ws) {
      // The ws was SWAPPED for a different socket (self-heal reopen, or a
      // runtime-restart client rebuild). Dispose the PREVIOUS socket's cached
      // RFBClient so its still-attached 'error'/'close' listeners can't fire a
      // phantom onError('WebSocket error') into THIS fresh stream — the
      // stale-client race that surfaced "Lost connection to the VM" on an
      // otherwise-healthy reconnect. dispose() only detaches listeners/timers;
      // it does NOT close the (already-dead) old socket. Different-OBJECT swap
      // only: StrictMode's same-ws re-run keeps prevWsRef.current === ws (no
      // dispose), and the in-place rfb_reset recreate disposes via
      // recreateRfbClientRef, so neither double-disposes here.
      (
        prevWsRef.current as WebSocket & { __hcRfbClient?: { dispose: () => void } }
      ).__hcRfbClient?.dispose();
      // Don't un-stick a terminally-given-up overlay (gaveUpRef): only an
      // explicit Retry clears that. A live swap otherwise resets to connecting.
      if (!gaveUpRef.current) setStatus({ kind: 'connecting' });
    }
    prevWsRef.current = ws;

    // RFB negotiation state must survive React 19 <StrictMode>'s
    // initial-mount dry-run (effect → cleanup → effect again). The
    // upstream is already sending bytes by the time we mount; if a fresh
    // RFBClient is constructed on the second mount, it would resume in
    // 'version' state while the upstream has already moved past
    // 'security'. We attach the instance to the WebSocket itself and
    // reuse it across mounts.
    // Present on requestAnimationFrame: paints are vsync-aligned and coalesced
    // (every FBU that lands within one refresh interval composites ONCE), which
    // is what reads as "smooth" — frame-time consistency matters more than the
    // ~1ms-vs-16ms scheduling delay, and that delay is negligible next to the
    // WAN round-trip anyway. Painting still runs AFTER the RFB drain accumulates
    // rects, so it never blocks the WebSocket read path. (rAF also pauses when
    // the tab is hidden — see the dirtyQueue cap below for that case.)
    let rafHandle = 0;
    const dirtyQueue: Rect[] = [];
    // When the tab is hidden, rAF stops firing but FBUs keep arriving, so the
    // queue could grow unbounded. Past this cap we drop the queue and repaint
    // the whole framebuffer once on the next frame (a full repaint subsumes any
    // backlog), bounding memory without losing correctness.
    let needsFullRepaint = false;
    const MAX_QUEUED_RECTS = 4096;
    const flushPaint = () => {
      rafHandle = 0;
      if (needsFullRepaint) {
        needsFullRepaint = false;
        dirtyQueue.length = 0;
        const fb = clientRef.current?.framebuffer();
        if (fb) {
          paintFull(canvasRef.current, fb);
        }
        return;
      }
      if (dirtyQueue.length === 0) return;
      const rects = dirtyQueue.splice(0, dirtyQueue.length);
      // Paint guest pixels into the single canvas (native resolution).
      // The browser's CSS compositor will scale it to the physical pixel grid.
      paintRects(canvasRef.current, clientRef.current, rects);
    };
    const schedulePaint = () => {
      if (rafHandle === 0) rafHandle = requestAnimationFrame(flushPaint);
    };

    // Pixels flowing prove the RFB session is live — downgrade a stale
    // connecting/reconnecting overlay to connected. Shared by onFramebufferReady
    // and onUpdate; leaves error/poweredOff (and connected) untouched.
    // The `markedLive` flag prevents calling setStatus on every single frame
    // during video — even bail-out updaters cost React scheduler time at 30fps.
    let markedLive = false;
    const markLiveFromFrames = () => {
      markedLive = true;
      setStatus((prev) =>
        prev.kind === 'connecting' || prev.kind === 'reconnecting'
          ? { kind: 'connected', latencyMs: 0, bytesIn: 0, bytesOut: 0 }
          : prev,
      );
    };

    const events: Partial<ConstructorParameters<typeof RFBClient>[0]['events']> = {
      onFramebufferReady: (fb) => {
        // Full reset on every (re)open: clear any queued rects from a prior
        // session, adjust state to the new native resolution and
        // paint the first full frame. This is what makes
        // a lobby→reopen render correctly instead of
        // painting a truncated first frame against stale dimensions.
        dirtyQueue.length = 0;
        needsFullRepaint = false;
        setFbSize({ width: fb.width, height: fb.height });
        paintFull(canvasRef.current, fb);
        // A full frame is definitive proof the (re)connected RFB session is
        // live — clear any stale connecting/reconnecting overlay. See onUpdate.
        markLiveFromFrames();
      },
      onUpdate: (rects) => {
        // A framebuffer update means the RFB session is genuinely live, so it
        // clears BOTH 'connecting' and 'reconnecting': during a real upstream
        // outage no pixels flow (upstream is down), so the only way frames
        // arrive is a working connection. The proxy's 'connected'/'resumed'
        // control frame is one-shot — if it's missed (reattach to an already-up
        // upstream, recreate-on-rfb_reset, a frame lost around idle), pixels
        // flowing is what gets us off the overlay instead of a dead-end spinner.
        if (!markedLive) markLiveFromFrames();
        if (needsFullRepaint || dirtyQueue.length + rects.length > MAX_QUEUED_RECTS) {
          // Backlog (typically a hidden tab where rAF is paused) — collapse to a
          // single full repaint instead of unbounded per-rect accumulation.
          needsFullRepaint = true;
          dirtyQueue.length = 0;
          schedulePaint();
          return;
        }
        // Small updates paint IMMEDIATELY instead of waiting for the next rAF
        // tick: click feedback / caret blinks are tiny rects, and the up-to-16ms
        // vsync wait is pure added photon latency for them (putImageData now
        // composites at the NEXT vsync; queued via rAF it composites at the one
        // after). Only when nothing is already queued (ordering) and the tab is
        // visible (hidden tabs must not burn paint — the rAF pause is the
        // throttle). Motion/video updates exceed the area cutoff and keep the
        // vsync-coalesced batch path — frame-time consistency is what reads as
        // smooth there.
        let area = 0;
        for (const r of rects) {
          area += r.w * r.h;
        }
        const SMALL_UPDATE_AREA = 64 * 1024; // px — ~256x256, well under any video frame
        if (
          area <= SMALL_UPDATE_AREA &&
          dirtyQueue.length === 0 &&
          rafHandle === 0 &&
          document.visibilityState === 'visible'
        ) {
          paintRects(canvasRef.current, clientRef.current, rects);
          return;
        }
        for (const r of rects) dirtyQueue.push(r);
        schedulePaint();
      },
      // Direct JPEG paint: paint the decoded ImageBitmap straight onto the
      // visible canvas via drawImage, bypassing the fb.pixels → putImageData
      // roundtrip. This eliminates 3 intermediate buffer copies and is the
      // primary performance path for Tight JPEG rects during video playback.
      onDirectImage: (bmp, x, y) => {
        paintBitmap(canvasRef.current, bmp, x, y);
      },
      onResize: (w, h) => setFbSize({ width: w, height: h }),
      onCursorChange: (shape) => {
        setCssCursor(cursorShapeToCss(shape));
      },
      // Persist the committed tier so it survives client recreations — it
      // decides the NEXT connection's pixel format (see the creation sites).
      // Also persist it per-endpoint (localStorage) so a FUTURE session's
      // connection #1 opens at the right pixel format. onQualityChange fires
      // only after the client's hysteresis, so `level` is already stable. We
      // store the actual tier (not just 'low'): a recovered endpoint records
      // 'high'/'medium', which the seed ignores, so it re-opens 32bpp.
      onQualityChange: (level) => {
        qualityTierRef.current = level;
        const epId = resourceId.split('/')[0] ?? '';
        if (epId) setEndpointMeta(epId, { lastTier: level });
      },
      onClipboard: (text) => {
        void clipboardRef.current?.handleGuestClipboard(text);
      },
      onError: () => {
        // A bare transport WebSocket error (the only thing client.ts emits here,
        // and now only while the socket is OPEN) is a DROP, not a terminal fault
        // — treat it EXACTLY like onClose so the self-heal can recover. Going
        // terminal here was the reconnect dead-end: the 'error' event fires
        // BEFORE 'close', so an unconditional terminal error pinned the console
        // (the subsequent onClose refuses to downgrade an 'error'). Real
        // terminal failures arrive as typed proxy control frames (error/fatal,
        // handled below) or the 1008/1009 close codes (armReconnect); the
        // frame-gated watchdogs and the 90s deadline own give-up. Status
        // propagates through the status effect, so no direct onStatusChange push.
        setStatus((prev) =>
          prev.kind === 'connected' || prev.kind === 'connecting'
            ? { kind: 'reconnecting', reason: 'connection lost' }
            : prev,
        );
      },
      onProtocolReset: (reason) => {
        // The RFB stream desynced (a decoder mis-counted, or a corrupt frame).
        // This is recoverable, NOT terminal. CRITICAL: protocolReset deliberately
        // KEEPS the loopback ws OPEN (see client.ts) — recovery is the proxy's
        // IN-PLACE re-handshake (it sends rfb_reset; recreateRfbClientRef rebuilds
        // this client on the SAME socket). Do NOT "restore" a close-on-reset here:
        // closing the loopback races the proxy's re-handshake write ("websocket:
        // close sent"), which makes it end the run loop with no rfb_reset and
        // freezes the console (the same class as the swapWs close-collision). We
        // only flip to the calm 'reconnecting' notice; the 90s client-side
        // fallback is the sole close, for a pure client-side desync with a live
        // upstream the proxy has no reason to re-handshake.
        console.warn('[rfb] protocol reset — realigning stream:', reason);
        setStatus((prev) =>
          prev.kind === 'connected' || prev.kind === 'connecting'
            ? { kind: 'reconnecting', reason: 'stream realigning' }
            : prev,
        );
      },
      onClose: () => {
        // The loopback WS to the runtime closed. Shell auto-reopens the stream
        // (self-heal) and swaps our `ws` prop, which resets us to 'connecting'
        // and re-attaches. Show 'reconnecting' meanwhile (never freeze on a
        // dead 'connected'/'connecting'). If the reopen never succeeds, the
        // frame-gated reconnecting watchdog below resolves to an actionable
        // error, so we never spin forever. Leaves error/poweredOff as-is.
        setStatus((prev) =>
          prev.kind === 'connected' || prev.kind === 'connecting'
            ? { kind: 'reconnecting', reason: 'connection lost' }
            : prev,
        );
      },
    };
    interface WSWithClient extends WebSocket {
      __hcRfbClient?: RFBClient;
    }
    const tagged = ws as WSWithClient;
    let client: RFBClient;
    if (tagged.__hcRfbClient) {
      client = tagged.__hcRfbClient;
      client.setEvents(events);
    } else {
      // SEED connection #1's tier from this endpoint's persisted link profile,
      // exactly once. A chronically slow-but-stable WAN endpoint would otherwise
      // open at the default 'high'/32bpp and stay there all session (no re-dial
      // occurs to trigger the adaptive 16bpp switch). Conservative + bounded:
      // only ever seed DOWN to 'low' from a recorded 'low'; a since-recovered
      // endpoint persisted 'high'/'medium' (ignored here), so it re-opens 32bpp,
      // and the live machine re-evaluates within seconds regardless. Reads from
      // localStorage history, NOT the prior connection's RTT (which the proxy
      // resets on re-dial), so it doesn't fight the resilience hardening.
      if (!qualitySeededRef.current) {
        qualitySeededRef.current = true;
        const epId = resourceId.split('/')[0] ?? '';
        if (epId && getEndpointMeta(epId)?.lastTier === 'low') {
          qualityTierRef.current = 'low';
        }
      }
      // lowColor pins this CONNECTION's pixel format to 16bpp RGB565 when
      // the link tier is 'low' at connect time. Sticky by design: a
      // connection opened at 565 stays 565 until the next reconnect even if
      // the tier improves mid-session, because SetPixelFormat must never
      // change mid-stream (an in-flight FBU encoded in the old format would
      // garbage-decode) — the re-handshake that constructs a fresh client
      // is the only safe switch point. initialQuality seeds the machine so
      // the persisted tier keeps evolving instead of resetting to 'high'.
      client = new RFBClient({
        ws,
        events,
        enableExtendedKey: extKeyEnabledRef.current,
        lowColor: qualityTierRef.current === 'low',
        initialQuality: qualityTierRef.current,
      });
      tagged.__hcRfbClient = client;
    }
    clientRef.current = client;

    // Allow the control-frame handler to recreate the RFBClient on an
    // rfb_reset (upstream reconnect → proxy re-runs the RFB handshake on the
    // fresh VNC connection). Disposing the old client detaches its ws
    // listeners; the new one starts in `version` state and re-drives the
    // client side of the handshake (re-sending SetEncodings, which the proxy
    // re-injects Extended Clipboard into) so the stream realigns instead of
    // mis-parsing the server's handshake bytes as RFB messages.
    recreateRfbClientRef.current = () => {
      clientRef.current?.dispose();
      // Re-handshake = the one safe point to (re)apply the low-color
      // decision; same sticky-per-connection rule as the creation above.
      const fresh = new RFBClient({
        ws,
        events,
        enableExtendedKey: extKeyEnabledRef.current,
        lowColor: qualityTierRef.current === 'low',
        initialQuality: qualityTierRef.current,
      });
      tagged.__hcRfbClient = fresh;
      clientRef.current = fresh;
    };

    // Expose the active RFBClient to diagnostics globals (e.g. the
    // pointer-PHY characterization harness invoked via
    // window.__phyTest). The getter form lets the harness pick up the
    // CURRENT client even if a session is reconnected mid-test.
    //
    // Gated behind Vite's import.meta.env.DEV — TRUE only in `vite dev`
    // mode (when AHV_REMOTE_CONSOLE_DEV=1 is set by scripts/dev.sh and
    // Vite is running in HMR mode). Bundled production builds have
    // DEV=false and the assignment is a no-op. Combined with the
    // DevTools menu gate in apps/desktop/src/main/menu.ts this closes
    // a low-severity review finding (informational).
    if (import.meta.env.DEV) {
      (window as Window & { __hcRfbClientGetter?: () => RFBClient | null }).__hcRfbClientGetter =
        (): RFBClient | null => clientRef.current;
    }

    // Listen for runtime control frames over the same WebSocket (sent as text).
    const onText = (e: MessageEvent) => {
      if (typeof e.data !== 'string') return;
      try {
        const frame = JSON.parse(e.data) as {
          type: string;
          cause?: string;
          reason?: string;
          attempt?: number;
          max?: number;
          bytes_in?: number;
          bytes_out?: number;
          latency_ms?: number;
        };
        // Any control frame proves the loopback is alive. The reconnecting
        // watchdog uses this to distinguish a healthy upstream-reconnect (the
        // proxy keeps sending reconnect_attempt/heartbeat frames over the live
        // loopback) from a dead socket (no frames at all).
        lastFrameAtRef.current = Date.now();
        switch (frame.type) {
          case 'connected':
            // The authoritative "stream is up" signal. After a terminal give-up,
            // only an explicit Retry may revive the console (same invariant the
            // [ws] self-heal swap guards with !gaveUpRef.current).
            if (gaveUpRef.current) break;
            setStatus({
              kind: 'connected',
              latencyMs: frame.latency_ms ?? 0,
              bytesIn: 0,
              bytesOut: 0,
            });
            clipboardRef.current?.clearLedger();
            break;
          case 'resumed':
            // 'resumed' = the proxy re-dialed the upstream (bare dial success) —
            // but it is emitted BEFORE the in-place RFB re-handshake completes,
            // so the stream is NOT usable yet. Do NOT latch 'connected' here:
            // doing so cleared unhealthySinceRef/gaveUpRef and disarmed the 60s/
            // 90s recovery watchdogs, so a stalled or looping re-handshake spun on
            // the spinner forever with no escape to the error overlay. Stay in the
            // recovering state; the top-of-handler lastFrameAt update keeps the
            // reconnect watchdog fed, and the readiness poll (clientRef.isReady)
            // or real pixels (markLiveFromFrames) flip to 'connected' once the
            // stream is genuinely usable. The paired rfb_reset resets clipboard
            // loop-state; reset here too in case 'resumed' ever arrives alone.
            if (gaveUpRef.current) break;
            clipboardRef.current?.clearLedger();
            break;
          case 'reconnecting':
          case 'reconnect_attempt':
            // Likewise don't re-arm the (re)connecting watchdogs / readiness poll
            // after a terminal give-up — that state is left only via Retry.
            if (gaveUpRef.current) break;
            setStatus({
              kind: 'reconnecting',
              reason: frame.reason,
              attempt: frame.attempt,
              max: frame.max,
            });
            break;
          case 'vm_powered_off':
            // Expected state (operator powered the VM down) — show a calm
            // notice with a Power-on action, not the reconnect spinner. The
            // proxy keeps polling; a later connected/resumed frame clears it.
            // Preserve the existing reference so the ~3s poll re-emitting this
            // frame doesn't churn re-renders / status-change subscribers.
            setStatus((prev) => (prev.kind === 'poweredOff' ? prev : { kind: 'poweredOff' }));
            break;
          case 'vm_starting':
            // VM powered on but its console isn't serving yet (booting). Calm
            // "starting" state — no reconnect spinner, no flashing. Ref-preserve
            // so the proxy re-emitting this on each backoff tick doesn't churn.
            setStatus((prev) => (prev.kind === 'starting' ? prev : { kind: 'starting' }));
            break;
          case 'rfb_reset':
            // The proxy reconnected the upstream and is re-running the RFB
            // handshake on the fresh VNC connection. Recreate our RFBClient so
            // it re-handshakes (rather than mis-parsing the new server's
            // handshake bytes as messages), and clear stale clipboard
            // loop-suppression state — the new guest session is a clean slate.
            recreateRfbClientRef.current?.();
            clipboardRef.current?.clearLedger();
            // Re-baseline the input engine against the fresh guest, exactly as we
            // reset the clipboard ledger: release any modifier / synthetic Ctrl /
            // held key the engine still believes is down (a pre-reconnect Shift /
            // Cmd→Ctrl / printable would otherwise stay "held" on a guest that
            // never saw the keydown), and forget the lock-key cache.
            inputRef.current?.releaseAllModifiers();
            break;
          case 'heartbeat':
            // Feed the measured upstream RTT to the RFB client's adaptive-
            // quality machine (RTT-tiered Tight JPEG quality / zlib level).
            // Without this call the machine never receives an RTT, so it stays
            // pinned to the top quality tier regardless of link latency — on a
            // slow link that means oversized frames, lower effective fps, and
            // stalls. (The machine itself is in client.ts: feedHeartbeat →
            // _evaluateQuality → setEncodings.)
            if (typeof frame.latency_ms === 'number') {
              clientRef.current?.feedHeartbeat(frame.latency_ms);
            }
            setStatus((prev) =>
              prev.kind === 'connected'
                ? {
                    kind: 'connected',
                    latencyMs: frame.latency_ms ?? prev.latencyMs,
                    bytesIn: frame.bytes_in ?? prev.bytesIn,
                    bytesOut: frame.bytes_out ?? prev.bytesOut,
                  }
                : prev,
            );
            break;
          case 'error':
          case 'fatal':
            // Terminal failure with a typed cause from the proxy. The cause
            // drives the actionable error overlay (re-authenticate vs reconnect
            // vs reopen) instead of a dead-end message.
            setStatus({
              kind: 'error',
              reason: frame.reason ?? 'upstream error',
              cause: normalizeErrorCause(frame.cause),
            });
            break;
          case 'clipboard_failed':
            // Clipboard sync-status notifications were removed by request: the
            // proxy still emits this frame, but it is intentionally not surfaced
            // to the operator. The failure is still logged proxy-side.
            break;
        }
      } catch {
        /* not JSON; ignore */
      }
    };
    ws.addEventListener('message', onText);

    return () => {
      ws.removeEventListener('message', onText);
      // Cancel any pending paint frame so a queued rAF callback can't fire
      // after unmount (touching a disposed offscreen / stale closure).
      if (rafHandle !== 0) {
        cancelAnimationFrame(rafHandle);
        rafHandle = 0;
      }
      // Deliberately do NOT dispose the RFB client. It is attached to the
      // WebSocket (ws.__hcRfbClient) so subsequent mounts reuse the same
      // protocol state. When Shell.closeTab calls ws.close(), the
      // listeners stop firing and the client is GC'd along with the WS.
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps -- resourceId is read only as a once-per-connection seed (guarded by qualitySeededRef); re-running this WS/RFB setup effect on it would re-handshake the live stream
  }, [ws]);

  // Propagate status changes only when the status value itself changes.
  // onStatusChange is read through a ref so an inline callback in the parent
  // doesn't make this effect re-fire and trip Maximum-update-depth-exceeded.
  useEffect(() => {
    onStatusChangeRef.current?.(status);
    // Feed the churn-proof reconnect deadline (below): remember when we LEFT a
    // healthy 'connected' state, and clear the marker + give-up latch once we're
    // genuinely connected again.
    if (status.kind === 'connected') {
      unhealthySinceRef.current = null;
      gaveUpRef.current = false;
    } else if (status.kind === 'poweredOff' || status.kind === 'starting') {
      // Calm, expected, self-resolving states (a slow guest reboot can sit here
      // for minutes). They must NOT count toward the give-up budget — clear the
      // marker so a long-but-legitimate boot doesn't trip the terminal deadline
      // when it flaps back through 'reconnecting'.
      unhealthySinceRef.current = null;
    } else if (
      (status.kind === 'connecting' || status.kind === 'reconnecting') &&
      unhealthySinceRef.current === null
    ) {
      unhealthySinceRef.current = Date.now();
    }
  }, [status]);

  // Connecting watchdog: 'connecting' must never hang forever. If we neither
  // connect (control frame OR first framebuffer) nor see the socket close
  // within the window — e.g. a hung upstream dial, or a loopback drop that
  // fired no close event — resolve to an actionable error so the operator can
  // reopen the session instead of staring at a spinner. Generous window to
  // tolerate slow VMs / high-latency links. Re-armed only while connecting.
  useEffect(() => {
    if (status.kind !== 'connecting') return;
    const CONNECT_WATCHDOG_MS = 45_000;
    const t = setTimeout(() => {
      setStatus((prev) =>
        prev.kind === 'connecting'
          ? {
              kind: 'error',
              reason: 'the VM did not respond — it may be unreachable or still starting',
            }
          : prev,
      );
    }, CONNECT_WATCHDOG_MS);
    return () => clearTimeout(t);
  }, [status.kind]);

  // Reconnecting watchdog: a HEALTHY upstream reconnect keeps frames flowing
  // over the live loopback (the proxy sends reconnect_attempt/heartbeat
  // frames), so we leave it alone. But if NO frame arrives for the window, the
  // loopback itself is dead and Shell's reopen hasn't restored it — resolve to
  // an actionable error instead of an endless spinner. Polls rather than a
  // one-shot timer so a late frame keeps extending the deadline.
  useEffect(() => {
    if (status.kind !== 'reconnecting') return;
    const RECONNECT_STALE_MS = 60_000;
    const id = setInterval(() => {
      if (Date.now() - lastFrameAtRef.current > RECONNECT_STALE_MS) {
        setStatus((prev) =>
          prev.kind === 'reconnecting'
            ? { kind: 'error', reason: 'lost connection to the VM and could not reconnect' }
            : prev,
        );
      }
    }, 5_000);
    return () => clearInterval(id);
  }, [status.kind]);

  // NOTE: there is deliberately NO renderer-side "connected" liveness watchdog.
  // A previous one (gap #4) closed the loopback socket on a heartbeat stall —
  // but it could not distinguish a dead LOOPBACK from a dead UPSTREAM with a
  // healthy loopback, so on a VM power-off it slammed the healthy loopback shut
  // and turned the proxy's calm `vm_powered_off` poll into a "connection lost"
  // reconnect (frozen screen). The correct layers handle both cases: a dead
  // loopback / proxy crash closes the WS → the onclose handler above goes to
  // 'reconnecting'; a dead/half-open UPSTREAM is the proxy's job (TCP keepalive
  // on the upstream conn + the 15s upstream ping → it re-dials and sends the
  // right control frame: reconnecting / vm_powered_off / vm_starting).

  // Readiness-based recovery. markLiveFromFrames clears the (re)connecting
  // overlay when PIXELS flow — but a static guest screen (a firmware/boot
  // splash, an idle desktop) produces no framebuffer updates after the stream
  // realigns, so the frame-driven clear never fires and the overlay sticks even
  // though the RFB client is back to 'normal'. Poll the client's handshake
  // state as an independent liveness signal and lift the overlay the moment the
  // connection is genuinely re-established, regardless of pixel activity.
  useEffect(() => {
    // Skip while this window is hidden: a backgrounded console gains nothing
    // from polling, and the control-frame path (connected/resumed) still flips
    // it to 'connected' independently. Re-arms when the window is shown again
    // (backgrounded is in the deps). Removes a 2 Hz poll that, stacked across
    // many reconnecting consoles during a VPN flap, added to the idle CPU drag.
    if (backgrounded) return;
    if (status.kind !== 'connecting' && status.kind !== 'reconnecting') return;
    const id = setInterval(() => {
      // Once we've terminally given up, only an explicit Retry (handleReconnect)
      // may revive the console — never this readiness poll. isReady can read true
      // over a dead-but-OPEN loopback (upstream gone, local socket still OPEN),
      // which would otherwise flip a given-up console back to a fake 'connected'.
      if (gaveUpRef.current) return;
      if (!clientRef.current?.isReady) return;
      setStatus((prev) =>
        prev.kind === 'connecting' || prev.kind === 'reconnecting'
          ? { kind: 'connected', latencyMs: 0, bytesIn: 0, bytesOut: 0 }
          : prev,
      );
    }, 500);
    return () => clearInterval(id);
  }, [status.kind, backgrounded]);

  // Churn-proof terminal deadline. The 'connecting' (45s) and 'reconnecting'
  // (60s) watchdogs above are re-armed on every status.kind change, so a self-
  // heal reopen loop that flips status connecting⇄reconnecting every ~1s keeps
  // resetting them and they never fire — the console would spin forever when a
  // reopen genuinely can't succeed (runtime restarted, session reaped past its
  // idle TTL, auth expired mid-session). This deadline is anchored to when we
  // last LEFT 'connected' and runs on a fixed interval (mounted once, no
  // status.kind dep) so churn can't defeat it: after the budget with no healthy
  // 'connected' in between, surface an actionable error + Retry and latch
  // gaveUpRef so the background self-heal's socket swaps can't un-stick it.
  useEffect(() => {
    const RECONNECT_BUDGET_MS = 90_000;
    // A 5s interval that observes a much larger wall-clock jump means the OS
    // slept or the renderer was frozen (background throttle) — NOT real reconnect
    // time. Re-baseline the wall-clock anchors by the gap instead of firing, so a
    // self-healing console gets its FULL budget after wake rather than being
    // failed instantly on a stale pre-sleep timestamp (#12).
    const SLEEP_GAP_MS = 15_000;
    let lastTick = Date.now();
    const id = setInterval(() => {
      const now = Date.now();
      const gap = now - lastTick;
      lastTick = now;
      if (gap > SLEEP_GAP_MS) {
        if (unhealthySinceRef.current !== null) unhealthySinceRef.current += gap;
        // Also push the frame-staleness anchor so the 'reconnecting'/'connected'
        // watchdogs don't false-fire on wake from a pre-sleep lastFrameAt.
        if (lastFrameAtRef.current !== 0) lastFrameAtRef.current += gap;
        return;
      }
      if (gaveUpRef.current) return;
      const since = unhealthySinceRef.current;
      if (since === null || now - since <= RECONNECT_BUDGET_MS) return;
      // The 90s budget elapsed — but only give up if the LOOPBACK has ALSO gone
      // silent. While the runtime proxy is still re-handshaking upstream (its own
      // budget is ~27 min), it keeps sending reconnect_attempt/heartbeat control
      // frames over the live loopback, refreshing lastFrameAtRef. Giving up at
      // 90s while those frames still arrive would false-drop a recoverable spike
      // (the long-latency / VPN-blip case). Only a genuinely dead loopback — no
      // frame for 60s, the same window the 'reconnecting' watchdog trusts — is
      // terminal here.
      if (now - lastFrameAtRef.current <= 60_000) return;
      setStatus((prev) => {
        if (prev.kind !== 'connecting' && prev.kind !== 'reconnecting') return prev;
        gaveUpRef.current = true;
        return { kind: 'error', reason: 'lost connection to the VM and could not reconnect' };
      });
    }, 5_000);
    return () => clearInterval(id);
  }, []);

  // Explicit Retry from the error overlay clears the terminal give-up latch and
  // the health marker so the fresh stream is allowed back to 'connecting' →
  // 'connected' (otherwise gaveUpRef would keep the error pinned forever).
  const handleReconnect = useCallback(() => {
    gaveUpRef.current = false;
    unhealthySinceRef.current = null;
    setStatus({ kind: 'connecting' });
    onReconnect?.();
  }, [onReconnect]);

  // Debounce the reconnecting OVERLAY so a flapping/brief reconnect doesn't
  // flash the full-screen banner on and off. Show it only after 'reconnecting'
  // has persisted past the delay; hide instantly on any other state. (The
  // status-strip chip still reflects the live state — this only gates the big
  // overlay.)
  const [showReconnectOverlay, setShowReconnectOverlay] = useState(false);
  useEffect(() => {
    if (status.kind !== 'reconnecting') {
      setShowReconnectOverlay(false);
      return;
    }
    const t = setTimeout(() => setShowReconnectOverlay(true), 900);
    return () => clearTimeout(t);
  }, [status.kind]);

  // Connection-state overlay (reconnecting / error / powered-off) is a SINGLE,
  // always-mounted layer that FADES in/out over the frozen last framebuffer —
  // never a hard popup that blanks the screen. On an uplink error or a flapping
  // reconnect the operator keeps seeing the (dimmed) last frame with a calm
  // notice fading in, instead of an intermittent blank/white flash. We retain
  // the descriptor through the fade-out (matching the 240ms opacity transition)
  // so the content doesn't vanish before the layer has dimmed away.
  const wantedKind: 'poweredOff' | 'reconnecting' | 'error' | null =
    status.kind === 'poweredOff'
      ? 'poweredOff'
      : status.kind === 'error'
        ? 'error'
        : // A booting VM reuses the calm reconnecting overlay visual (dimmed
          // last frame + notice), with fixed "VM is starting…" text and no
          // attempt counter — shown promptly, not gated like a real reconnect.
          status.kind === 'starting'
          ? 'reconnecting'
          : status.kind === 'reconnecting' && showReconnectOverlay
            ? 'reconnecting'
            : null;
  const wantedReason =
    status.kind === 'starting'
      ? 'VM is starting…'
      : status.kind === 'error' || status.kind === 'reconnecting'
        ? status.reason
        : undefined;
  // Reconnect progress (attempt N of M) carried as primitives so the overlay
  // effect doesn't re-fire on every render from a fresh object identity.
  const wantedAttempt = status.kind === 'reconnecting' ? status.attempt : undefined;
  const wantedMax = status.kind === 'reconnecting' ? status.max : undefined;
  const wantedCause = status.kind === 'error' ? status.cause : undefined;
  const overlayVisible = wantedKind !== null;
  const [overlay, setOverlay] = useState<{
    kind: 'poweredOff' | 'reconnecting' | 'error';
    reason?: string;
    attempt?: number;
    max?: number;
    cause?: ConsoleErrorCause;
  } | null>(null);
  useEffect(() => {
    if (wantedKind) {
      setOverlay({
        kind: wantedKind,
        reason: wantedReason,
        attempt: wantedAttempt,
        max: wantedMax,
        cause: wantedCause,
      });
      return;
    }
    const t = setTimeout(() => setOverlay(null), 280);
    return () => clearTimeout(t);
  }, [wantedKind, wantedReason, wantedAttempt, wantedMax, wantedCause]);

  // ----- pop-out focus auto-capture -----
  //
  // The pure policy (createConsoleFocusPolicy above) decides; these effects
  // only glue it to the DOM. Gated on `active` like the rest of the input
  // wiring; the policy itself is inert unless `popout`.
  const focusPolicyRef = useRef<ConsoleFocusPolicy | null>(null);
  focusPolicyRef.current ??= createConsoleFocusPolicy(popout);
  useEffect(() => {
    const focusNow = focusPolicyRef.current?.statusChanged(status.kind, {
      hasOsFocus: document.hasFocus(),
      canvasAvailable: canvasRef.current !== null,
    });
    if (focusNow && active) canvasRef.current?.focus();
  }, [status.kind, active]);
  // The proxy's 'connected' control frame can precede the first framebuffer —
  // the canvas (gated on fbSize) is not rendered yet at the connect
  // transition, so the focus request waits here for it. Boolean dep on
  // purpose: a guest RESIZE must not re-focus.
  const hasCanvas = fbSize !== null;
  useEffect(() => {
    if (!hasCanvas || !active) return;
    if (focusPolicyRef.current?.canvasReady()) canvasRef.current?.focus();
  }, [hasCanvas, active]);
  // Alt-tab back to the pop-out: re-focus the canvas so typing resumes. The
  // policy keeps this inert while an overlay is up (its buttons must stay
  // keyboard-reachable — and ConsoleWindowApp's one-shot session_expired
  // focus listener owns that error case) and after a deliberate Ctrl+Alt
  // release.
  useEffect(() => {
    if (!popout) return;
    const onWindowFocus = () => {
      if (active && focusPolicyRef.current?.windowFocused()) canvasRef.current?.focus();
    };
    window.addEventListener('focus', onWindowFocus);
    return () => window.removeEventListener('focus', onWindowFocus);
  }, [popout, active]);

  // ----- keyboard pass-through -----
  //
  // The InputEngine owns all keyboard state — modifier tracking,
  // Mac→PC remap, layout-escape, release chord. This effect only
  // glues DOM listeners to the engine. See docs/INTERACTION-ARCH.md
  // §3 for the module boundaries and §4 for the state machine.
  //
  // Keyboard always routes through RFB now that the dial-back
  // agent is gone. NGT's exec round-trip latency (~200-500ms)
  // would make per-keystroke routing unusable; VNC keysym +
  // QEMU Extended Key Event remain the right path. NGT only
  // owns the clipboard channel.
  // The release chord (bare Ctrl+Alt) is wired into the engine ONCE, so it
  // reads its action via a ref: drop capture by blurring the canvas. Tell the
  // focus policy first — a deliberate release must stay released (a window
  // 'focus' event must not auto-recapture until a click or a reconnect).
  const onReleaseChordRef = useRef<() => void>(() => {
    focusPolicyRef.current?.releasedByChord();
    canvasRef.current?.blur();
  });
  const inputRef = useRef<InputEngine | null>(null);
  inputRef.current ??= createInputEngine({
    client: () => clientRef.current,
    onReleaseChord: () => onReleaseChordRef.current(),
  });

  // Push the operator's DECLARED guest layout into the engine: every typed
  // character is translated into the key sequence that layout uses
  // (GuestLayouts.ts). Persisted per VM by useKeyboardLayout; runs on mount so
  // a restored pick applies before the first keystroke.
  useEffect(() => {
    inputRef.current?.setGuestLayout(kbdLayout.layout.id);
  }, [kbdLayout.layout.id]);

  // ── Keyboard LOCK mode (VMware-style windowed auto-grab) ───────────────────
  // Enter: ask main to arm the native OS keyboard grab for this window. Main
  // installs the per-OS hook, suppresses host-reserved combos (Alt+Tab, Super,
  // Alt+Esc) on the host, and forwards each back as a closed LockCombo we inject
  // into the guest (see the onLockCombo effect). It returns false where the grab
  // can't engage (Wayland / macOS / no native addon) — then we stay UNlocked and
  // those combos remain on the Send Keys menu. No fullscreen, no button: the
  // grab follows focus. OS-protected combos (Ctrl+Alt+Del, Win+L, Cmd+Tab, VT
  // switches) can never be grabbed and stay on Send Keys — the banner says so.
  const enterLock = useCallback(async () => {
    const armed = await window.ahvRemoteConsole.setLockGrab(true);
    setLocked(armed);
  }, []);

  const exitLock = useCallback(() => {
    void window.ahvRemoteConsole.setLockGrab(false);
    inputRef.current?.releaseAllModifiers();
    setLocked(false);
  }, []);

  // The bare Ctrl+Alt release chord drops the grab AND returns focus to the host
  // (VMware-exact): blur the canvas so onBlur fires, which also disarms the grab.
  // When not locked it is just the original blur-to-release.
  useEffect(() => {
    onReleaseChordRef.current = () => {
      if (lockedRef.current) exitLock();
      focusPolicyRef.current?.releasedByChord();
      canvasRef.current?.blur();
    };
  }, [exitLock]);

  // A host-reserved combo the native grab suppressed arrives here as a closed
  // enum; inject the matching keysym chord into the guest (identical to the Send
  // Keys path). Subscribed once — clientRef is a ref, no deps churn.
  useEffect(() => {
    return window.ahvRemoteConsole.onLockCombo(({ combo }) => {
      clientRef.current?.sendKeySequence(LOCK_COMBO_KEYSYMS[combo]);
    });
  }, []);

  // Probe the host's keyboard layout once. The result feeds future
  // layout-aware composition (today it only surfaces a warning if
  // the operator's layout reports keys our static keycode table
  // doesn't have, so the agent-uinput path will hit keycode=0 for
  // those — RFB keysym path is unaffected).
  useEffect(() => {
    void layoutSnapshot().then((snap) => {
      if (snap && snap.unmapped.length > 0) {
        // eslint-disable-next-line no-console
        console.info(
          `[input] host layout reports ${snap.unmapped.length} keys without a static evdev mapping: ${snap.unmapped.slice(0, 5).join(', ')}${snap.unmapped.length > 5 ? '…' : ''}`,
        );
      }
    });
  }, []);

  // Global blur / visibilitychange watchdog. Cmd+Tab, Alt+Tab, app-menu
  // open, modal popups, document hidden — every event that takes focus
  // out of the canvas without going through onBlur on the canvas itself
  // would otherwise leave the guest with stuck modifiers. We listen at
  // window+document scope so it fires regardless of focus path.
  // Independent of `active`/`fbSize` so it works during the resize
  // window where the canvas is being recreated.
  useEffect(() => {
    const watchdog = () => {
      inputRef.current?.releaseAllModifiers();
    };
    window.addEventListener('blur', watchdog);
    document.addEventListener('visibilitychange', () => {
      if (document.hidden) watchdog();
    });
    return () => {
      window.removeEventListener('blur', watchdog);
      // Anonymous visibilitychange handler retained intentionally — it
      // closes over `watchdog` and `inputRef.current` survives the
      // detach because the engine is created once via inputRef.
    };
  }, []);

  useEffect(() => {
    if (!active || !fbSize) return;
    const el = canvasRef.current;
    if (!el) return;

    // Focus IS the grab trigger (VMware-exact auto-grab — no button): clicking
    // into the console arms the native windowed keyboard grab; losing focus
    // (click away / Ctrl+Alt release chord / window blur) disarms it.
    const onFocus = () => {
      setCaptured(true);
      void enterLock();
    };
    const onBlur = () => {
      inputRef.current?.releaseAllModifiers();
      setCaptured(false);
      void exitLock();
    };
    const onKeyDown = (e: KeyboardEvent) => {
      inputRef.current?.handleKeyDown(e);
      // The keystroke makes the guest produce a frame; ensure an incremental
      // request is outstanding so it isn't held for the next idle-backstop tick
      // (debounced inside the client). Lives here, not in sendKey, to keep the
      // client's send methods pure protocol encoders.
      clientRef.current?.primeOnInput();
    };
    const onKeyUp = (e: KeyboardEvent) => {
      inputRef.current?.handleKeyUp(e);
    };
    const onCompositionStart = () => {
      inputRef.current?.handleCompositionStart();
    };
    const onCompositionUpdate = (e: CompositionEvent) => {
      inputRef.current?.handleCompositionUpdate(e.data);
    };
    const onCompositionEnd = (e: CompositionEvent) => {
      inputRef.current?.handleCompositionEnd(e.data);
    };

    el.addEventListener('focus', onFocus);
    el.addEventListener('blur', onBlur);
    el.addEventListener('keydown', onKeyDown);
    el.addEventListener('keyup', onKeyUp);
    el.addEventListener('compositionstart', onCompositionStart);
    el.addEventListener('compositionupdate', onCompositionUpdate);
    el.addEventListener('compositionend', onCompositionEnd);
    return () => {
      el.removeEventListener('focus', onFocus);
      el.removeEventListener('blur', onBlur);
      el.removeEventListener('keydown', onKeyDown);
      el.removeEventListener('keyup', onKeyUp);
      el.removeEventListener('compositionstart', onCompositionStart);
      el.removeEventListener('compositionupdate', onCompositionUpdate);
      el.removeEventListener('compositionend', onCompositionEnd);
    };
  }, [active, fbSize, enterLock, exitLock]);

  // Notify main process so it can suspend the app menu / accelerators while
  // the canvas is focused (otherwise Cmd+W etc. would close the tab instead
  // of being delivered to the guest).
  useEffect(() => {
    window.ahvRemoteConsole.setCaptureMode(captured).catch(() => undefined);
  }, [captured]);

  // ----- clipboard bridge (ClipboardEngine) -----
  //
  // Owns the host-pasteboard poll and the loop-suppression ledger.
  // Host→Guest: ClipboardEngine polls NSPasteboard, detects changes,
  //   calls forwardHostToVM → sendClientCutText → QEMU VNC server
  //   → QEMU clipboard subsystem → vdagent chardev → serial → bridge.
  // Guest→Host: bridge polls guest clipboard, sends vdagent grab/data
  //   → QEMU clipboard subsystem → Extended Clipboard ServerCutText
  //   → proxy translates to legacy → renderer → ClipboardEngine
  //   → NSPasteboard.

  const clipboardRef = useRef<ClipboardEngine | null>(null);
  clipboardRef.current ??= createClipboardEngine({
    sessionId: () => sessionId,
    forwardHostToVM: (text: string): boolean => clientRef.current?.sendClientCutText(text) ?? false,
    // Clipboard sync-status notifications were removed by request — log the
    // failure (no plaintext: message is a size/reason only) without an
    // operator-facing notice.
    onError: (dir, message) => console.warn('[clipboard] transfer failed', dir, message),
    // Opt-in reliable large paste: read the flag freshly per paste (it arrives
    // after this engine is constructed). Only affects the large-paste path.
    reprobeCapacity: () => largePasteEnabledRef.current,
  });
  useEffect(() => {
    const stop = clipboardRef.current?.startHostPoll();
    return () => {
      stop?.();
    };
  }, []);

  // Legacy UplinkScanner + pixel-strip poll removed.
  // Clipboard now flows through the vdagent serial channel (COM3).
  // Guest→Host: bridge sends VD_AGENT_CLIPBOARD via COM3 → QEMU
  //   clipboard subsystem → VNC ServerCutText → renderer.
  // Host→Guest: renderer sends ClientCutText → QEMU clipboard
  //   subsystem → vdagent → COM3 → bridge → SetClipboardData.

  // ----- mouse events -----

  useEffect(() => {
    const el = canvasRef.current;
    if (!el || !fbSize) return;

    let buttonMask = 0;
    // Last absolute fb-space position, used by wheel events which carry no
    // movement of their own.
    const lastPos = { x: fbSize.width / 2, y: fbSize.height / 2 };

    const absPos = (e: MouseEvent): { x: number; y: number } => {
      const rect = el.getBoundingClientRect();
      const x = ((e.clientX - rect.left) * fbSize.width) / (rect.width || 1);
      const y = ((e.clientY - rect.top) * fbSize.height) / (rect.height || 1);
      return {
        x: Math.max(0, Math.min(fbSize.width - 1, x)) | 0,
        y: Math.max(0, Math.min(fbSize.height - 1, y)) | 0,
      };
    };

    // Mouse-move coalescing. Trackpads emit ≥90 Hz, gaming mice 1000 Hz.
    // We coalesce moves with a 7ms floor (~140 Hz cap), which sits just under
    // QEMU's ~150 ev/s input ceiling — leaving headroom for keyboard/wheel
    // events to share the pipe without oversaturating it — and is far above any
    // human-perceptible cursor-smoothness threshold. (Was 4ms/~250 Hz, which
    // overshot the ceiling.) Button events (down/up) and wheel still fire
    // synchronously for click responsiveness.
    const pending = { x: lastPos.x, y: lastPos.y, dirty: false };
    let pointerTimer: ReturnType<typeof setTimeout> | null = null;
    let lastPointerSendTime = 0;
    const POINTER_MIN_INTERVAL = 7; // ms (~140 Hz, just under QEMU's ~150 ev/s)
    const flushPointer = () => {
      pointerTimer = null;
      if (!pending.dirty) return;
      pending.dirty = false;
      lastPointerSendTime = performance.now();
      clientRef.current?.sendPointer(pending.x, pending.y, buttonMask);
    };
    const schedulePointerFlush = () => {
      if (pointerTimer !== null) return;
      const elapsed = performance.now() - lastPointerSendTime;
      if (elapsed >= POINTER_MIN_INTERVAL) {
        flushPointer();
      } else {
        pointerTimer = setTimeout(flushPointer, POINTER_MIN_INTERVAL - elapsed);
      }
    };

    const move = (e: MouseEvent) => {
      const p = absPos(e);
      lastPos.x = p.x;
      lastPos.y = p.y;
      pending.x = p.x;
      pending.y = p.y;
      pending.dirty = true;
      schedulePointerFlush();
    };
    const down = (e: MouseEvent) => {
      buttonMask |= buttonForBrowser(e.button);
      const p = absPos(e);
      lastPos.x = p.x;
      lastPos.y = p.y;
      // Synchronous: click latency is user-perceptible at single-ms
      // resolution and coalescing would feel mushy.
      clientRef.current?.sendPointer(p.x, p.y, buttonMask);
      clientRef.current?.primeOnInput();
    };
    const up = (e: MouseEvent) => {
      buttonMask &= ~buttonForBrowser(e.button);
      const p = absPos(e);
      lastPos.x = p.x;
      lastPos.y = p.y;
      clientRef.current?.sendPointer(p.x, p.y, buttonMask);
      clientRef.current?.primeOnInput();
    };
    const wheel = (e: WheelEvent) => {
      inputRef.current?.sendWheel(lastPos.x | 0, lastPos.y | 0, buttonMask, e.deltaY);
      e.preventDefault();
    };
    const contextmenu = (e: MouseEvent) => e.preventDefault();

    el.addEventListener('mousemove', move);
    el.addEventListener('mousedown', down);
    el.addEventListener('mouseup', up);
    el.addEventListener('wheel', wheel, { passive: false });
    el.addEventListener('contextmenu', contextmenu);
    return () => {
      if (pointerTimer !== null) clearTimeout(pointerTimer);
      el.removeEventListener('mousemove', move);
      el.removeEventListener('mousedown', down);
      el.removeEventListener('mouseup', up);
      el.removeEventListener('wheel', wheel);
      el.removeEventListener('contextmenu', contextmenu);
    };
  }, [fbSize]);

  // ----- display fit mode + canvas sizing -----
  //
  // Four modes:
  //   fit          — scale canvas to fill container, preserve aspect ratio (default)
  //   pixel        — 1:1 native pixels; container scrolls if canvas overflows
  //   stretch      — fill container, ignore aspect ratio
  //   resize-guest — same visual as 'fit' (SetDesktopSize is deprecated/unsupported on AHV)
  //
  // Sizing + compositing are unified: computeCanvasGeometry derives the CSS
  // size AND the physical-device-pixel backing-store size, and composite()
  // draws the native-resolution offscreen into that backing store. We publish
  // the compositor through composeRef so the (stale-closure) paint path can
  // recomposite without re-subscribing, and run it here whenever fbSize /
  // containerSize / fitMode / dpr changes — covering window resize, DPI
  // change, fit-mode toggle and lobby→reopen.

  const fitMode = fitModeState.fitMode;

  useEffect(() => {
    if (!fbSize || !containerSize.width || !containerSize.height) return;
    const canvas = canvasRef.current;
    if (!canvas) return;

    // 1:1 Backing Store: the canvas pixels exactly match the guest framebuffer.
    if (canvas.width !== fbSize.width) canvas.width = fbSize.width;
    if (canvas.height !== fbSize.height) canvas.height = fbSize.height;

    // CSS Scaling: compute the display size to preserve aspect ratio, and
    // let the browser's hardware compositor scale the canvas to the screen.
    const geom = computeCanvasGeometry(fbSize, containerSize, fitMode);
    canvas.style.width = `${geom.cssWidth}px`;
    canvas.style.height = `${geom.cssHeight}px`;
  }, [fbSize, containerSize, fitMode]);

  // Guest display-resolution auto-request was DEPRECATED.
  //
  // The original design tracked the container size and sent
  // SetDesktopSize (RFC 6143 §7.5.5) so the guest VM screen
  // resolution would auto-follow the window. AHV's QEMU does NOT
  // honor SetDesktopSize (the AHV guest VMs ship with a stdvga
  // driver pinned to one supported mode list, which is a driver
  // limitation we can't work around from outside the guest), so
  // the proxy converted each request to a clipboard side-channel
  // command (__AHVRC_RESIZE__WxH). The side-channel ended up
  // flooding the clipboard transport during window-resize gestures
  // and starved real operator copies — see hcbridge.log analysis
  // 2026-05-28: 18 of 22 host→guest clipboard frames were
  // __AHVRC_RESIZE__ payloads, none reached SetClipboardData.
  //
  // Since resizing the guest never actually worked on AHV anyway,
  // removing the auto-request fully frees the clipboard channel.
  // The viewport scales the framebuffer to fit on the host side
  // (see the fit-mode effect above); the guest stays at whatever
  // resolution Windows chose at boot.

  // "Push pasteboard to guest" action, surfaced as a toolbar
  // button. Reads the Mac pasteboard and asks the bridge to write
  // it into the guest's clipboard. After that the operator's
  // Cmd+V in the guest pastes the right thing — the guest's own
  // clipboard, which now mirrors the Mac's.
  //
  // The continuous host-poll already mirrors Mac→guest within
  // ~100ms, so this button is mostly for "I just copied
  // something, paste it RIGHT NOW without waiting for the next
  // poll tick."
  const resourceId = vmUuid ?? '';
  // Permission-aware gating of the prerequisite controls. The resource id is
  // "<endpointId>/<vmUUID>" (see ConsoleWindowApp.reauthNow), so the endpoint
  // resolves client-side. Until it does — and while every capability is
  // unknown — the gate shows everything: only PROVEN denials degrade.
  const endpointId = resourceId.split('/')[0] ?? '';
  const { capabilities, refresh: refreshCapabilities } = useEndpointCapabilities(
    sdkClient,
    endpointId,
  );
  const gate = capabilityGate(capabilities);

  // Live VM stats panel — read-only, toggled by a toolbar button (default
  // closed). When open AND the window is foregrounded, light-poll getVMStats
  // (~5s) so an open panel stays fresh without flooding Prism; the poll pauses
  // when the window is hidden (reusing the `backgrounded` gate the readiness
  // poll already uses) and stops entirely when the panel is closed. Read-only,
  // so NO capability gate.
  const [statsOpen, setStatsOpen] = useState(false);
  // Snapshots panel toggle. Create/Restore inside it gate on vm_update (proven-
  // denial only); restore is DESTRUCTIVE behind a mandatory confirm dialog.
  const [snapshotsOpen, setSnapshotsOpen] = useState(false);
  const [stats, setStats] = useState<VMStats | null>(null);
  const [statsLoading, setStatsLoading] = useState(false);
  const [statsError, setStatsError] = useState<string | null>(null);
  useEffect(() => {
    if (!statsOpen || backgrounded || !resourceId) return;
    let cancelled = false;
    const poll = async () => {
      setStatsLoading(true);
      try {
        const res = await sdkClient.getVMStats('nutanix-ahv', resourceId);
        if (cancelled) return;
        setStats(res.stats);
        setStatsError(null);
      } catch (err) {
        if (cancelled) return;
        setStatsError((err as Error).message);
      } finally {
        if (!cancelled) setStatsLoading(false);
      }
    };
    void poll();
    const id = setInterval(() => void poll(), 5000);
    return () => {
      cancelled = true;
      clearInterval(id);
    };
  }, [statsOpen, backgrounded, resourceId, sdkClient]);

  const powerAction = useCallback(
    async (action: 'ON' | 'OFF' | 'ACPI_SHUTDOWN' | 'ACPI_REBOOT' | 'RESET') => {
      // The op registers a task as soon as the runtime starts it — bump the
      // poller so the running task surfaces in the event bar immediately. The
      // in-progress + success notices live in the event bar now (no toast).
      tasksController.refresh();
      try {
        await sdkClient.vmPowerAction('nutanix-ahv', resourceId, action);
      } catch (err) {
        toast(`Power failed: ${(err as Error).message}`, 'err');
      } finally {
        // Terminal now — re-poll so the task flips to succeeded/failed.
        tasksController.refresh();
      }
    },
    [sdkClient, resourceId, toast, tasksController],
  );

  // Surface the bridge-ISO mount outcome to the operator. Reads the resolved
  // mount state (not the stale closure) so it can render the real serialNote:
  // a transient toast plus a notice banner that's sticky for info/warn/error
  // (the consent-restart dialog, the deferred badge, or a setup error).
  // Shared by both "Install prerequisites" entry points (toolbar + status
  // footer).
  const surfaceMountOutcome = useCallback(
    (ms: AgentMountState) => {
      if (ms.kind === 'mounted') {
        const c = serialCopy(ms.serialNote);
        // The guest OS isn't knowable host-side (no NGT), so name both
        // entry points; the operator looking at the console knows which
        // applies. driveLetterHint is a Windows-only guess.
        const next =
          c.kind === 'ok'
            ? ` Windows: open ${ms.driveLetterHint || 'the CD-ROM'} and run ahv-remote-console-agent.exe.` +
              ' Linux: install the package from its linux folder.'
            : '';
        setInstallNotice({ ...c, text: c.text + next });
      } else if (ms.kind === 'mountError' && ms.code === 'image_publish_required') {
        // Typed outcome: the catalog lacks the agent image and the role's
        // image-create is a proven denial — the runtime never attempted the
        // upload. Same copy as the learned-denial notice; the capability
        // refresh below hides the prerequisites control going forward.
        setInstallNotice({
          text: CAPABILITY_NOTICE_COPY.image_publish_required,
          kind: 'warn',
          sticky: true,
        });
      } else if (ms.kind === 'mountError') {
        setInstallNotice({
          text: `Agent install failed: ${ms.reason}`,
          kind: 'err',
          sticky: true,
        });
      }
      // Every resolved mount attempt generates fresh permission evidence
      // runtime-side (403s mark denials, successes mark allowed) — re-read it.
      refreshCapabilities();
    },
    [refreshCapabilities],
  );

  // Manual "Add serial port (COM3)" — re-introduced as a Prerequisites-menu
  // item. Standalone from the agent-ISO mount: just provisions the COM3 vdagent
  // channel (the runtime registers a 'serial_port' task; refresh so the event
  // bar surfaces it immediately, the same kick powerAction does). The outcome
  // (added / already present / power-off-required / error) rides the notice
  // banner — no toast. Resource-based, so it works with the VM off / no live
  // session, which is exactly when AHV can attach the port.
  const addSerialPort = useCallback(async () => {
    tasksController.refresh();
    try {
      const { note } = await sdkClient.addSerialPort('nutanix-ahv', resourceId);
      setInstallNotice(serialCopy(note));
    } catch (err) {
      setInstallNotice({
        text: `Couldn't add the COM3 serial port: ${(err as Error).message}`,
        kind: 'err',
        sticky: true,
      });
    } finally {
      tasksController.refresh();
    }
  }, [sdkClient, resourceId, tasksController]);

  // Auto-dismiss only the non-sticky (success) install notices. warn/error
  // notices stay until the operator dismisses them (they require action).
  useEffect(() => {
    if (!installNotice || installNotice.sticky) return;
    const t = setTimeout(() => setInstallNotice(null), 6000);
    return () => clearTimeout(t);
  }, [installNotice]);

  // Learned-denial notice disabled by request: opening a console must not raise
  // any auto-notification. A proven denial is conveyed by the gated control
  // simply being absent; the operator can still trigger the prerequisites flow
  // to get an explicit, action-driven message. (Was a one-time per-endpoint
  // warn banner driven off gate.notice.)

  // Operator consent for the orchestrated power cycle — [Restart now] in the
  // prerequisite dialog. This is the ONLY place powerCycle:true is ever set:
  // it re-calls mountISO so the staged progress banner runs with the cycle
  // labels (shutting down → configuring → starting), then surfaces the
  // resulting note (serial_port_added_cycled / serial_port_deferred / error).
  const consentRestart = useCallback(() => {
    void (async () => {
      surfaceMountOutcome(await agent.mountISO({ powerCycle: true }));
    })();
  }, [agent, surfaceMountOutcome]);

  const clusterButtons = useMemo(() => {
    // Snapshot the catalog state so the CD/ISO menu list re-renders as the
    // on-demand load resolves (idle → loading → loaded/error).
    const catalog = agent.catalogState;
    // Layouts offered in the picker — the deployments this product targets
    // (US/UK/Italian guests). GuestLayouts.ts still carries de/fr/es tables,
    // so a previously-persisted pick keeps translating; it just isn't offered
    // for NEW picks until a deployment needs it.
    const KBD_LAYOUTS = [
      { id: 'us', label: 'US' },
      { id: 'uk', label: 'UK' },
      { id: 'it', label: 'Italian' },
    ];
    // Send-Keys catalog. Each entry forwards a pre-composed keysym chord via
    // sendKeySequence (press-all-then-release-all) so the guest sees it as one
    // combo. These are the keys a host swallows (Alt+Tab, Win+*), can't type
    // (Ctrl+Alt+Del SAS), or that have no key on a Mac (Insert/PrintScreen/
    // Pause/Menu) — exactly the VMware-Remote-Console "Send Ctrl-Alt-Del" menu,
    // expanded. Keysyms are raw X11 codes (see rfb/keysyms.ts):
    //   Ctrl 0xffe3 · Shift 0xffe1 · Alt 0xffe9 · Win/Super 0xffeb · Del 0xffff
    //   Esc 0xff1b · Tab 0xff09 · BackSpace 0xff08 · Print 0xff61 · Insert 0xff63
    //   Pause 0xff13 · Menu 0xff67 · F1..F10 0xffbe..0xffc7 · NumLock 0xff7f
    //   ScrollLock 0xff14 · arrows Left/Up/Right/Down 0xff51..0xff54 · space 0x20
    //   · letters = ASCII (e.g. 'i' 0x69, 's' 0x73, 'v' 0x76, 'x' 0x78).
    const sk = (label: string, keys: number[], danger = false): ClusterMenuItem => ({
      label,
      danger,
      onClick: () => {
        clientRef.current?.sendKeySequence(keys);
        toast(`Sent ${label}`, 'ok');
      },
    });
    const hdr = (label: string): ClusterMenuItem => ({ label, header: true });
    return [
      {
        icon: 'keyboard' as const,
        title: 'Send Keys',
        menu: [
          // The menu follows the usual VM-console set: the system chords, the four
          // combos the native keyboard grab forwards (Win, Alt+Tab, Alt+Shift+Tab,
          // Alt+Esc — kept byte-identical to LOCK_COMBO_KEYSYMS so this menu is their
          // fallback where the grab is unavailable), keys a host intercepts or a Mac
          // lacks, and Linux virtual-terminal switching. Ordinary guest shortcuts
          // are typed directly while the keyboard is captured.
          hdr('System'),
          sk('Ctrl + Alt + Del', [0xffe3, 0xffe9, 0xffff]),
          sk('Task Manager — Ctrl+Shift+Esc', [0xffe3, 0xffe1, 0xff1b]),
          // The Mac Option (third-level / AltGr) layer can't pass through in
          // scancode mode — Mac and Windows layouts put these glyphs on different
          // physical keys — so on a Mac host the symbols an operator needs most are
          // sent here as keysyms, which the guest composes via its own keymap.
          // Other hosts type them directly (AltGr passes through).
          ...(hostOS() === 'macos'
            ? [
                hdr('Symbols (Mac Option layer)'),
                sk('@  at', [0x40]),
                sk('#  hash', [0x23]),
                sk('{  brace left', [0x7b]),
                sk('}  brace right', [0x7d]),
                sk('[  bracket left', [0x5b]),
                sk(']  bracket right', [0x5d]),
                sk('\\  backslash', [0x5c]),
                sk('|  pipe', [0x7c]),
                sk('~  tilde', [0x7e]),
                sk('`  backtick', [0x60]),
                sk('€  euro', [0x20ac]),
              ]
            : []),
          hdr('Windows'),
          sk('Win — Start', [0xffeb]),
          // A Mac keyboard has no Win key; Ctrl+Esc is the universal Start-menu
          // opener that needs no Super key.
          sk('Ctrl + Esc — Start menu (no Win key)', [0xffe3, 0xff1b]),
          sk('Alt + Tab', [0xffe9, 0xff09]),
          sk('Alt + Shift + Tab', [0xffe9, 0xffe1, 0xff09]),
          sk('Alt + Esc — Cycle windows', [0xffe9, 0xff1b]),
          sk('Alt + F4 — Close window', [0xffe9, 0xffc1]),
          hdr('Single keys'),
          sk('PrintScreen', [0xff61]),
          sk('Insert', [0xff63]),
          sk('Pause / Break', [0xff13]),
          // Toggle keys a Mac keyboard lacks entirely.
          sk('Num Lock', [0xff7f]),
          sk('Scroll Lock', [0xff14]),
          // Bare function keys (XK_F1..F12 = 0xffbe..0xffc9). Routine in
          // BIOS/UEFI/boot menus. F11/F12 were unreachable any other way — F11 is
          // swallowed by our own fullscreen toggle, and a Mac maps several to
          // media/brightness — so sending them as keysyms is the only path.
          hdr('Function keys'),
          sk('F1', [0xffbe]),
          sk('F2', [0xffbf]),
          sk('F3', [0xffc0]),
          sk('F4', [0xffc1]),
          sk('F5', [0xffc2]),
          sk('F6', [0xffc3]),
          sk('F7', [0xffc4]),
          sk('F8', [0xffc5]),
          sk('F9', [0xffc6]),
          sk('F10', [0xffc7]),
          sk('F11 — host fullscreen key', [0xffc8]),
          sk('F12', [0xffc9]),
          hdr('Linux virtual terminals'),
          sk('Ctrl + Alt + F1', [0xffe3, 0xffe9, 0xffbe]),
          sk('Ctrl + Alt + F2', [0xffe3, 0xffe9, 0xffbf]),
          sk('Ctrl + Alt + F3', [0xffe3, 0xffe9, 0xffc0]),
          sk('Ctrl + Alt + F4', [0xffe3, 0xffe9, 0xffc1]),
          sk('Ctrl + Alt + F5', [0xffe3, 0xffe9, 0xffc2]),
          sk('Ctrl + Alt + F6', [0xffe3, 0xffe9, 0xffc3]),
          sk('Ctrl + Alt + F7 — back to GUI', [0xffe3, 0xffe9, 0xffc4]),
          sk('Ctrl + Alt + Backspace — restart X', [0xffe3, 0xffe9, 0xff08], true),
        ],
      },
      // ONE setup + media control (icon 'disc'): the agent-prerequisite install
      // PLUS arbitrary-ISO mount-from-catalog and eject, merged into a single
      // menu so there is exactly one CD/ISO toolbar entry — two separate 'disc'
      // buttons collided on the React list key. Gating is unchanged: install +
      // eject are vm-spec updates (gate.showPrereqs); the catalog LIST is gated
      // on image_read (gate.showCatalog); the button is ABSENT when neither
      // applies (proven denials — see capabilityGate + the notice effect above).
      ...(gate.showPrereqs || gate.showCatalog
        ? [
            {
              icon: 'disc' as const,
              title: 'Prerequisites & media',
              // Load the catalog lazily when the menu opens — a large
              // multi-cluster catalog must never slow the toolbar.
              onOpen: gate.showCatalog
                ? () => {
                    void agent.loadISOCatalog();
                  }
                : undefined,
              menu: [
                ...(gate.showPrereqs
                  ? [
                      {
                        label: 'Install prerequisites',
                        onClick: () => {
                          void (async () => {
                            // The staged progress banner (agent.mountState) shows the
                            // prepare → upload → attach sequence; surfaceMountOutcome
                            // renders the resolved serialNote / errors.
                            surfaceMountOutcome(await agent.mountISO());
                          })();
                        },
                      },
                      {
                        // Manual COM3 channel add, separate from the full mount —
                        // for when the agent is delivered another way. AHV can
                        // only attach the port with the VM OFF; the outcome notice
                        // says if a power-off is required.
                        label: 'Add serial port (COM3)',
                        onClick: () => {
                          void addSerialPort();
                        },
                      },
                    ]
                  : []),
                ...(gate.showCatalog
                  ? [
                      hdr('Mount ISO from catalog'),
                      // The list reflects agent.catalogState (re-rendered as it
                      // loads). A non-loaded/empty/error state shows a single
                      // muted status row instead of image entries.
                      ...(catalog.kind === 'loaded' && catalog.images.length > 0
                        ? catalog.images.map((img) => ({
                            label: img.name,
                            onClick: () => {
                              void (async () => {
                                surfaceMountOutcome(await agent.mountChosenISO(img.uuid));
                              })();
                            },
                          }))
                        : [
                            {
                              label:
                                catalog.kind === 'loading'
                                  ? 'Loading…'
                                  : catalog.kind === 'error'
                                    ? `Couldn't load: ${catalog.reason}`
                                    : catalog.kind === 'loaded'
                                      ? 'No ISO images in catalog'
                                      : 'Loading…',
                              header: true,
                            },
                          ]),
                    ]
                  : []),
                ...(gate.showPrereqs
                  ? [
                      {
                        label: 'Eject CD-ROM',
                        onClick: () => {
                          void (async () => {
                            const r = await agent.ejectISO();
                            if (r.kind === 'ejected') {
                              toast(r.ejected ? 'CD-ROM ejected' : 'No media to eject', 'ok');
                            } else if (r.kind === 'error') {
                              toast(`Eject failed: ${r.reason}`, 'err');
                            }
                          })();
                        },
                      },
                    ]
                  : []),
              ],
            },
          ]
        : []),
      {
        icon: 'globe' as const,
        // The operator DECLARES which keyboard layout the guest VM is set to;
        // the InputEngine then translates every typed character into the key
        // sequence THAT layout uses (GuestLayouts.ts) — e.g. on an Italian
        // guest '£' becomes Shift+3 and '@' becomes AltGr+ò. Picking here is
        // declarative (persisted per VM, pushed to the engine via the effect
        // above); it does NOT type into the guest. VNC cannot read or set the
        // guest's actual layout — the explicit Win+Space entry below cycles the
        // guest's installed input languages when they genuinely diverge.
        title: `Guest keyboard layout: ${kbdLayout.layout.label}`,
        menu: [
          ...KBD_LAYOUTS.map((l) => ({
            label: `${kbdLayout.layout.id === l.id ? '●' : ' '} ${l.label}`,
            onClick: () => {
              kbdLayout.setLayoutId(l.id);
              toast(`Typing as a ${l.label} guest layout`, 'ok');
            },
          })),
          {
            label: 'Cycle guest input language (Win+Space)',
            onClick: () => {
              // Win+Space (Super_L + Space) cycles the guest's installed input
              // languages — the only in-band lever VNC offers.
              clientRef.current?.sendKeySequence([0xffeb, 0x0020]);
              toast('Sent Win+Space — check the VM tray for the active language', 'info');
            },
          },
        ],
      },
      {
        icon: 'power-dot' as const,
        title: 'Power',
        tone: 'danger' as const,
        menu: [
          {
            label: 'Power ON',
            onClick: () => {
              void powerAction('ON');
            },
          },
          {
            label: 'Restart (Guest)',
            onClick: () => {
              void powerAction('ACPI_REBOOT');
            },
          },
          {
            label: 'Shutdown (Guest)',
            onClick: () => {
              void powerAction('ACPI_SHUTDOWN');
            },
          },
          {
            label: 'Force reset',
            onClick: () => {
              void powerAction('RESET');
            },
            danger: true,
          },
          {
            label: 'Force off',
            onClick: () => {
              void powerAction('OFF');
            },
            danger: true,
          },
        ],
      },
      {
        // Snapshots panel toggle. The panel itself gates Create/Restore on
        // vm_update (proven-denial only) and gates restore behind a mandatory
        // confirm dialog, so the toolbar button shows for every VM (a read-only
        // list never proves anything either way).
        icon: 'layers' as const,
        title: 'Snapshots',
        onClick: () => {
          setSnapshotsOpen((v) => !v);
        },
      },
      {
        // Live VM stats panel toggle. Read-only (no power/spec mutation), so it
        // is NOT capability-gated — it shows for every VM. Toggles the
        // light-polling StatsPanel overlay; the poll itself is gated on
        // statsOpen + foreground (see the effect above).
        icon: 'info' as const,
        title: 'Live VM stats',
        onClick: () => {
          setStatsOpen((v) => !v);
        },
      },
      {
        // Toggle fullscreen of the whole console view. Also on Ctrl+Cmd+F / F11
        // (useFullscreen), but the button is the discoverable affordance.
        icon: 'fullscreen' as const,
        title: 'Fullscreen',
        onClick: () => {
          fullscreen.toggleFullscreen();
        },
      },
    ];
  }, [
    agent,
    kbdLayout,
    fullscreen,
    powerAction,
    toast,
    surfaceMountOutcome,
    addSerialPort,
    gate.showPrereqs,
    gate.showCatalog,
  ]);

  // Hover-reveal for the top chrome rail. In full-bleed (fullscreen) the reveal
  // requires a deliberate shove to the very top edge so the rail can't keep
  // popping over the guest's own top-edge window controls; windowed mode keeps a
  // forgiving reach. Auto-hide after inactivity; ⌘/Ctrl+K pins it open.
  const chrome = useChromeReveal(fullBleed);

  // Consent-dialog gating, applied at RENDER time so a vm_power denial
  // learned after the dialog opened still removes [Restart now] and swaps in
  // the deferred-only copy. applyRestartGate is identity for every other
  // notice and while the capability is unknown.
  const shownNotice = installNotice ? applyRestartGate(installNotice, gate.offerRestart) : null;

  // Guest-viewport top inset. The toolbar is always shown (pinned by default), so
  // the guest always sits in a FIXED band below it — a constant inset, so the
  // guest never re-scales on pointer movement (the earlier dynamic band resized
  // the guest on every top-edge hover). The guest's own top-row UI (a browser tab
  // strip, an app menu) therefore stays reachable, never under our chrome. 44px
  // clears the floating TopRail (top:7 + height:34 + margin); in macOS windowed
  // mode the guest below 44 sits clear of the hiddenInset traffic lights, and the
  // rail's own left padding (titleBarLeft) clears them for the toolbar content.
  const viewportTop = 44;

  return (
    <div
      ref={rootRef}
      style={{
        position: 'relative',
        height: '100%',
        background: '#05090f',
        overflow: 'hidden',
      }}
    >
      {/* The guest fills the window horizontally; vertically it starts at the
          fixed viewportTop band (see above) below the always-shown toolbar —
          constant, so the guest never resizes. Any remaining bars are pure
          aspect-ratio letterbox (the guest's fixed resolution in a
          differently-shaped window), not reserved chrome.

          RESIZE LOOP FIX: containerRef is on this OUTER div, NOT on the inner
          div that wraps the canvas. composite() sets canvas.style.width/height,
          which changes the inner wrapper's layout — if containerRef were on the
          same element, the ResizeObserver would fire on every composite() call,
          producing an infinite geometry recalculation. The outer div is sized
          purely by CSS (absolute inset), so it is stable and never feeds back. */}
      <div
        ref={containerRef}
        style={{
          position: 'absolute',
          left: 0,
          right: 0,
          top: viewportTop,
          bottom: 0,
          // viewportTop is constant (toolbar always shown), so the guest never
          // resizes — no `top` transition, no ResizeObserver churn.
          display: 'flex',
          alignItems: 'center',
          justifyContent: 'center',
          overflow: fitMode === 'pixel' ? 'auto' : 'hidden',
          // Isolate the ResizeObserver target: contain the flex child so the
          // canvas CSS changes reported by composite() don't leak back into
          // this element's own reported size.
          contain: 'strict',
        }}
      >
        {fbSize ? (
          <div
            style={{
              // The wrapper is inline-block so it shrink-wraps the canvas.
              // IMPORTANT: do NOT use position:relative here — it makes
              // this div a new containing block, which makes it participate
              // in layout and can change the outer container's measured size
              // if overflow is not strictly hidden above.
              position: 'relative',
              display: 'inline-block',
              lineHeight: 0,
              // Explicitly limit this wrapper to the canvas CSS size so it
              // never forces the outer container wider/taller than its bounds.
              overflow: 'hidden',
            }}
          >
            <canvas
              ref={canvasRef}
              tabIndex={0}
              role="img"
              // Name the viewer for screen readers. The framebuffer pixels are
              // inherently opaque over VNC (we can't expose guest content), so
              // naming the canvas + how to release capture is the realistic
              // ceiling — do not over-promise guest accessibility (WCAG 4.1.2).
              aria-label={`${vmName} console — keyboard captured; press Ctrl+Alt to release`}
              onMouseDown={() => {
                // A click is the operator opting back in after a Ctrl+Alt
                // release — re-arm the auto-focus policy.
                focusPolicyRef.current?.pointerDown();
                canvasRef.current?.focus();
              }}
              style={{
                // Backing-store size is exactly the guest framebuffer resolution
                // (set by the geometry useEffect). CSS width/height scale it to
                // fit the container. The browser's native GPU compositor handles
                // the up/down sampling, providing standard NoVNC-style quality.
                display: 'block',
                // HOST CURSOR POLICY:
                //  1. When the guest sends a cursor shape (ENC_CURSOR) we
                //     adopt it as a CSS cursor so it tracks at native OS
                //     speed, zero round-trip latency.
                //  2. Otherwise show the host's default arrow pointer so the
                //     operator always has a visible reference point — the
                //     guest's own software cursor (drawn into its framebuffer)
                //     remains visible below it, giving an accurate position
                //     indicator without hiding the host pointer entirely.
                //     (AHV std-VGA never sends ENC_CURSOR, so 'default' is
                //     the common case.)
                cursor: cssCursor ?? 'default',
                // No focus ring: the canvas takes focus on click (tabIndex=0)
                // to capture keyboard, but the browser's default focus outline
                // draws a border around the guest viewer — visual noise on a
                // full-bleed console. Capture state is shown by the titlebar
                // status dot, not a border.
                outline: 'none',
              }}
            />
          </div>
        ) : (
          <div
            style={{
              display: 'flex',
              flexDirection: 'column',
              alignItems: 'center',
              gap: 12,
              color: 'var(--hc-fg-2)',
            }}
          >
            <svg
              width="24"
              height="24"
              viewBox="0 0 24 24"
              style={{ animation: 'hc-spin 1s linear infinite' }}
            >
              <circle
                cx="12"
                cy="12"
                r="10"
                fill="none"
                stroke="currentColor"
                strokeWidth="2"
                strokeDasharray="50"
                strokeLinecap="round"
              />
            </svg>
            <span style={{ fontSize: 12 }}>Connecting to VM console…</span>
            <style>{`@keyframes hc-spin { to { transform: rotate(360deg); } }`}</style>
          </div>
        )}
      </div>
      {/* A) Ambient latency line — always visible, RTT/state-driven. */}
      <AmbientLine status={status} />

      {/* Top rail — hover-reveal chrome (commands + status). Revealed only from
          the TOP edge (see useChromeReveal) so the bottom edge stays free for
          the guest's own taskbar / window manager. The former bottom status rail
          was removed: it duplicated info and, full-bleed, it covered the guest
          taskbar every time the pointer neared the bottom. */}
      <TopRail
        vmName={vmName}
        status={status}
        buttons={clusterButtons}
        onClose={onClose}
        signalBars={motion.flags.signalBars}
        revealed={chrome.revealed}
        hoverProps={chrome.hoverProps}
      />

      {/* Live VM stats overlay — read-only, light-polling. Centered fade-in
          overlay; dismissed by its close button, Esc, or a backdrop click.
          Mounted only while toggled open. */}
      {statsOpen && (
        <Overlay label="Live VM stats" onClose={() => setStatsOpen(false)}>
          <StatsPanel
            stats={stats}
            loading={statsLoading}
            error={statsError}
            onClose={() => setStatsOpen(false)}
          />
        </Overlay>
      )}

      {/* Snapshots panel — create / list / restore. Centered fade-in overlay.
          Restore is DESTRUCTIVE and gated behind the panel's mandatory confirm
          (itself an overlay). Write controls gate on vm_update (PROVEN-denial
          only; unknown stays enabled). */}
      {snapshotsOpen && resourceId && (
        <Overlay label="Snapshots" onClose={() => setSnapshotsOpen(false)}>
          <SnapshotsPanel
            sdkClient={sdkClient}
            pluginId="nutanix-ahv"
            resourceId={resourceId}
            vmName={vmName}
            toast={toast}
            gateCanWrite={capabilities?.vm_update !== 'denied'}
            onClose={() => setSnapshotsOpen(false)}
          />
        </Overlay>
      )}

      {/* Connection-state overlay — ONE always-mounted layer that FADES over the
          frozen last frame, so an uplink error or a flapping reconnect surfaces
          a calm notice instead of an intermittent blank/white popup. Content is
          retained through the fade-out (see the overlay state above). */}
      {/* Screen-reader announcer for connection state. The visible overlay below
          is aria-hidden (decorative blur), so without this a non-sighted operator
          is never told the session dropped or recovered (WCAG 4.1.3 status
          messages). Assertive for errors, polite otherwise; visually hidden. */}
      <div
        role="status"
        aria-live={status.kind === 'error' ? 'assertive' : 'polite'}
        style={{
          position: 'absolute',
          width: 1,
          height: 1,
          margin: -1,
          padding: 0,
          overflow: 'hidden',
          clipPath: 'inset(50%)',
          whiteSpace: 'nowrap',
          border: 0,
        }}
      >
        {status.kind === 'connected'
          ? `${vmName} console connected`
          : status.kind === 'connecting' || status.kind === 'starting'
            ? `Connecting to ${vmName}`
            : status.kind === 'reconnecting'
              ? `Connection lost — reconnecting to ${vmName}`
              : status.kind === 'poweredOff'
                ? `${vmName} is powered off`
                : status.kind === 'error'
                  ? `${vmName} console error — action needed`
                  : ''}
      </div>
      <div
        aria-hidden={!overlayVisible}
        style={{
          position: 'absolute',
          inset: 0,
          zIndex: 50,
          display: 'flex',
          alignItems: 'center',
          justifyContent: 'center',
          flexDirection: 'column',
          gap: overlay?.kind === 'poweredOff' ? 14 : 8,
          background:
            overlay?.kind === 'poweredOff' ? 'rgba(10, 18, 28, 0.86)' : 'rgba(0, 0, 0, 0.55)',
          backdropFilter: 'blur(4px)',
          color: '#fff',
          fontSize: 14,
          fontFamily:
            overlay?.kind === 'poweredOff' ? 'var(--hc-font-sans)' : 'var(--hc-font-mono)',
          opacity: overlayVisible ? 1 : 0,
          pointerEvents: overlayVisible ? 'auto' : 'none',
          transition: 'opacity 240ms ease',
        }}
      >
        {overlay?.kind === 'poweredOff' && (
          <>
            <div style={{ fontSize: 40, opacity: 0.85 }}>⏻</div>
            <div style={{ fontSize: 15, fontWeight: 600 }}>This VM is powered off</div>
            <div
              style={{
                fontSize: 12.5,
                opacity: 0.8,
                maxWidth: 380,
                textAlign: 'center',
                lineHeight: 1.5,
              }}
            >
              The console reconnects automatically as soon as the VM is powered on.
            </div>
            <button
              type="button"
              onClick={() => void powerAction('ON')}
              style={{
                marginTop: 4,
                padding: '8px 18px',
                borderRadius: 8,
                border: '1px solid var(--hc-accent-500)',
                background: 'var(--hc-accent-500)',
                color: '#fff',
                cursor: 'pointer',
                fontSize: 13,
                fontWeight: 600,
              }}
            >
              Power on VM
            </button>
          </>
        )}
        {overlay?.kind === 'reconnecting' && (
          <>
            <svg
              width="20"
              height="20"
              viewBox="0 0 24 24"
              style={{ animation: 'hc-spin 1s linear infinite' }}
            >
              <circle
                cx="12"
                cy="12"
                r="10"
                fill="none"
                stroke="currentColor"
                strokeWidth="2"
                strokeDasharray="50"
                strokeLinecap="round"
              />
            </svg>
            <span>
              Connection lost — reconnecting
              {overlay.attempt && overlay.max
                ? ` · attempt ${overlay.attempt} of ${overlay.max}`
                : ''}
              …
            </span>
          </>
        )}
        {overlay?.kind === 'error' && (
          <ErrorOverlayBody
            cause={overlay.cause}
            onReconnect={handleReconnect}
            onReauth={onReauth}
            onClose={onClose}
          />
        )}
      </div>
      {/* Centered, filling progress bar for EVERY running op (power / serial /
          agent-ISO install). Aggregates the same task list the corner tray polls;
          `pendingLabel` gives instant feedback while the agent-install task is
          still being registered. Idle renders nothing. */}
      <ConsoleTaskBar
        tasks={tasksController.tasks}
        pendingLabel={mounting ? 'Installing agent…' : undefined}
      />
      {!mounting && shownNotice && (
        <div
          role="status"
          style={{
            position: 'absolute',
            bottom: 96,
            left: '50%',
            transform: 'translateX(-50%)',
            zIndex: 61,
            maxWidth: '80%',
            padding: '10px 14px',
            borderRadius: 8,
            display: 'flex',
            alignItems: 'flex-start',
            gap: 10,
            background:
              shownNotice.kind === 'err'
                ? 'rgba(40, 12, 12, 0.94)'
                : shownNotice.kind === 'warn'
                  ? 'rgba(42, 30, 8, 0.94)'
                  : shownNotice.kind === 'info'
                    ? 'rgba(10, 24, 40, 0.94)'
                    : 'rgba(12, 36, 20, 0.94)',
            border: `1px solid ${
              shownNotice.kind === 'err'
                ? 'var(--hc-error-400, #ef4444)'
                : shownNotice.kind === 'warn'
                  ? '#f59e0b'
                  : shownNotice.kind === 'info'
                    ? 'var(--hc-accent-600, #1668a5)'
                    : 'var(--hc-ok-400, #22c55e)'
            }`,
            color: '#fff',
            fontSize: 12,
            lineHeight: 1.45,
            fontFamily: 'var(--hc-font-sans)',
            boxShadow: '0 6px 20px rgba(0,0,0,.4)',
          }}
        >
          <span style={{ flex: 1 }}>{shownNotice.text}</span>
          {shownNotice.powerCycle && (
            <>
              {/* Consent dialog actions. [Restart now] is the ONLY trigger of
                  the orchestrated power cycle (powerCycle:true); [Later] keeps
                  the runtime-armed deferred state and shows its badge. */}
              <button
                type="button"
                onClick={consentRestart}
                style={{
                  flexShrink: 0,
                  padding: '4px 10px',
                  borderRadius: 6,
                  border: '1px solid #f59e0b',
                  background: 'transparent',
                  color: '#fbbf24',
                  cursor: 'pointer',
                  fontSize: 12,
                }}
              >
                Restart now
              </button>
              <button
                type="button"
                onClick={() => setInstallNotice(serialCopy('serial_port_deferred'))}
                style={{
                  flexShrink: 0,
                  padding: '4px 8px',
                  borderRadius: 6,
                  border: '1px solid rgba(255,255,255,.25)',
                  background: 'transparent',
                  color: '#fff',
                  cursor: 'pointer',
                  fontSize: 12,
                }}
              >
                Later
              </button>
            </>
          )}
          {shownNotice.sticky && !shownNotice.powerCycle && (
            <button
              type="button"
              aria-label="Dismiss"
              onClick={() => setInstallNotice(null)}
              style={{
                flexShrink: 0,
                padding: '4px 8px',
                borderRadius: 6,
                border: '1px solid rgba(255,255,255,.25)',
                background: 'transparent',
                color: '#fff',
                cursor: 'pointer',
                fontSize: 12,
              }}
            >
              Dismiss
            </button>
          )}
        </div>
      )}
      {/* Keyboard-LOCK banner only (shown while locked). The on-open capture
          hint + pill were removed so opening a console raises no notifications. */}
      <CaptureIndicator locked={locked} />
      <fullscreen.FullscreenToast />
      <ShortcutsOverlay isOpen={shortcuts.isOpen} onClose={shortcuts.close} />
    </div>
  );
}
