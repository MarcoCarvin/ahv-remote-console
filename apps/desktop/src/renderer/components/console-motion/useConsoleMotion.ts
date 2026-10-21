/**
 * useConsoleMotion — derives the "active console motion" state from the
 * console's live signals and exposes per-layer active flags + the
 * reduced-motion gate.
 *
 * The derivation is a small, PURE state machine (see `deriveMotion`)
 * so it can be unit-tested with plain objects in the NODE vitest env —
 * no DOM, no real KeyboardEvent. The hook itself only wires that pure
 * function to React + the app-wide MotionContext.
 *
 *   connecting
 *     → live(captured)  ⇄  live(released)
 *     → degraded        (connected & rtt > 150ms)
 *     → reconnecting    → live | closed
 *   backgrounded        → idle    (overrides connected states)
 *   poweredOff          → poweredOff
 *   error               → closed
 *
 * The hook reads `reducedMotion` from `!useMotion().enabled` (the
 * single source of truth for prefers-reduced-motion in this app). When
 * reduced, every "animated" layer flag is forced off so callers that
 * spawn particles / run RAF loops can skip the work entirely; the CSS
 * hard-gate in consoleMotion.css is the visual backstop.
 */

import { useMemo } from 'react';
import { useMotion } from '../../motion/MotionContext';

export type ConsoleMotionStatusKind =
  | 'connecting'
  | 'connected'
  | 'reconnecting'
  | 'poweredOff'
  | 'error';

export interface ConsoleMotionStatus {
  kind: ConsoleMotionStatusKind;
  latencyMs?: number;
  bytesIn?: number;
  bytesOut?: number;
}

export interface ConsoleMotionInput {
  status: ConsoleMotionStatus;
  captured: boolean;
  fullscreen: boolean;
  backgrounded: boolean;
}

export type ConsoleMotionState =
  | 'connecting'
  | 'capturedLive'
  | 'releasedLive'
  | 'degraded'
  | 'reconnecting'
  | 'poweredOff'
  | 'idle'
  | 'closed';

/** Per-layer active flags. True = the layer should animate. */
export interface ConsoleMotionFlags {
  /** C1 — capture boundary glow. */
  captureGlow: boolean;
  /** C2 — latency ping ring. */
  latencyRing: boolean;
  /** C3 — throughput particle stream. */
  throughput: boolean;
  /** C4 — signal bars. */
  signalBars: boolean;
  /** C5 — cursor handoff ring. */
  cursorHandoff: boolean;
  /** C6 — keystroke ripples. */
  keystrokeRipple: boolean;
  /** C7 — remote peer cursors. */
  remoteCursors: boolean;
  /** C8 — reconnect scanline sweep. */
  reconnectSweep: boolean;
  /** C9 — clipboard flash. */
  clipboardFlash: boolean;
  /** C10 — connect flip (one-time). */
  connectFlip: boolean;
  /** C11 — idle parallax tilt. */
  idleTilt: boolean;
  /** C12 — fullscreen/background morph. */
  fullscreenMorph: boolean;
}

export interface ConsoleMotionResult {
  state: ConsoleMotionState;
  reducedMotion: boolean;
  flags: ConsoleMotionFlags;
}

/** RTT above this (ms) marks the link as degraded. */
export const DEGRADED_RTT_MS = 150;

/**
 * Pure derivation: input + reducedMotion → state + per-layer flags.
 * Exported for unit testing. No React, no DOM.
 */
export function deriveMotion(
  input: ConsoleMotionInput,
  reducedMotion: boolean,
): ConsoleMotionResult {
  const { status, captured, fullscreen, backgrounded } = input;
  const rtt = status.latencyMs ?? 0;

  let state: ConsoleMotionState;
  if (status.kind === 'connecting') {
    state = 'connecting';
  } else if (status.kind === 'reconnecting') {
    state = 'reconnecting';
  } else if (status.kind === 'poweredOff') {
    state = 'poweredOff';
  } else if (status.kind === 'error') {
    state = 'closed';
  } else {
    // connected. Backgrounded overrides to idle; otherwise degraded by
    // RTT, else captured/released live.
    if (backgrounded) {
      state = 'idle';
    } else if (rtt > DEGRADED_RTT_MS) {
      state = 'degraded';
    } else if (captured) {
      state = 'capturedLive';
    } else {
      state = 'releasedLive';
    }
  }

  // A live link = anything where the framebuffer is streaming. Degraded
  // is still "live" (just unhealthy); idle is connected-but-backgrounded.
  const live =
    state === 'capturedLive' ||
    state === 'releasedLive' ||
    state === 'degraded' ||
    state === 'idle';

  const bytesPerSec = (status.bytesIn ?? 0) + (status.bytesOut ?? 0);

  // Raw layer intent (before the reduced-motion gate). These describe
  // "is this layer relevant right now", independent of animation.
  const raw: ConsoleMotionFlags = {
    captureGlow: captured && live,
    latencyRing: status.kind === 'connected',
    throughput: live && !backgrounded && bytesPerSec > 0,
    signalBars: status.kind === 'connected' || status.kind === 'reconnecting',
    cursorHandoff: captured && live && !backgrounded,
    keystrokeRipple: captured && live && !backgrounded,
    remoteCursors: live,
    reconnectSweep: state === 'reconnecting',
    clipboardFlash: live,
    connectFlip: state === 'capturedLive' || state === 'releasedLive' || state === 'degraded',
    idleTilt: state === 'idle',
    fullscreenMorph: backgrounded || fullscreen,
  };

  if (!reducedMotion) {
    return { state, reducedMotion, flags: raw };
  }

  // Reduced motion: kill every *animated* layer. C1 capture glow stays
  // (it renders a STATIC glow via CSS), C4 signal bars stay (static
  // bars), C12 morph stays (CSS resolves it to a no-op end-state).
  const gated: ConsoleMotionFlags = {
    ...raw,
    latencyRing: false,
    throughput: false,
    cursorHandoff: false,
    keystrokeRipple: false,
    remoteCursors: raw.remoteCursors, // still render peers, just no glide
    reconnectSweep: false,
    clipboardFlash: false,
    connectFlip: false,
    idleTilt: false,
  };

  return { state, reducedMotion, flags: gated };
}

export function useConsoleMotion(input: ConsoleMotionInput): ConsoleMotionResult {
  const { enabled } = useMotion();
  const reducedMotion = !enabled;

  return useMemo(
    () => deriveMotion(input, reducedMotion),
    // Destructure into stable primitives so the memo doesn't rerun on
    // every parent render that passes a fresh `status`/`input` object.
    // eslint-disable-next-line react-hooks/exhaustive-deps
    [
      input.status.kind,
      input.status.latencyMs,
      input.status.bytesIn,
      input.status.bytesOut,
      input.captured,
      input.fullscreen,
      input.backgrounded,
      reducedMotion,
    ],
  );
}
