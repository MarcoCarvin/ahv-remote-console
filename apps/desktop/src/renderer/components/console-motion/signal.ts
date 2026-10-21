/**
 * Pure mapping helpers for the latency ring (C2) and signal bars (C4).
 *
 * Kept in a side-effect-free `.ts` module (no React, no `.css` import)
 * so they can be unit-tested in the NODE vitest env and re-used by the
 * components without dragging in styles.
 */

const MIN_CADENCE_MS = 800;
const MAX_CADENCE_MS = 3000;

/** Latency ring colour by RTT: green < 50, amber 50–150, red > 150. */
export function latencyColor(rttMs: number): string {
  if (rttMs < 50) return 'var(--cm-green)';
  if (rttMs <= 150) return 'var(--cm-warn)';
  return 'var(--cm-danger)';
}

/** Ping cadence = clamp(rttMs, 800, 3000) ms. NaN → slowest. */
export function latencyCadenceMs(rttMs: number): number {
  const v = Number.isFinite(rttMs) ? rttMs : MAX_CADENCE_MS;
  return Math.min(MAX_CADENCE_MS, Math.max(MIN_CADENCE_MS, v));
}

export const SIGNAL_BAR_COUNT = 4;

/** Number of lit bars (0–4) by health. */
export function signalActiveBars(rttMs: number, connected: boolean): number {
  if (!connected) return 0;
  if (rttMs < 50) return 4;
  if (rttMs <= 150) return 3;
  if (rttMs <= 300) return 2;
  return 1;
}

/** Bar colour by health. */
export function signalColor(rttMs: number, connected: boolean): string {
  if (!connected) return 'var(--cm-warn)';
  if (rttMs < 50) return 'var(--cm-green)';
  if (rttMs <= 300) return 'var(--cm-warn)';
  return 'var(--cm-danger)';
}
