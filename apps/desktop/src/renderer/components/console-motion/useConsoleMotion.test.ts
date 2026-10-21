/**
 * Pure-logic tests for the console-motion state machine. NODE env, no
 * DOM — every input is a plain object. We test `deriveMotion`
 * directly (the hook is a thin React wrapper around it).
 */

import { describe, expect, it } from 'vitest';
// Import pure logic directly from the source modules (NOT the barrel),
// so the test doesn't pull in React components or `.css` side-effect
// imports — vitest runs in a NODE env with no DOM / CSS loader.
import { deriveMotion, DEGRADED_RTT_MS, type ConsoleMotionInput } from './useConsoleMotion';
import { signalActiveBars, signalColor, latencyCadenceMs, latencyColor } from './signal';

const base: ConsoleMotionInput = {
  status: { kind: 'connecting' },
  captured: false,
  fullscreen: false,
  backgrounded: false,
};

describe('deriveMotion — state', () => {
  it('connecting', () => {
    expect(deriveMotion(base, false).state).toBe('connecting');
  });

  it('connected + captured + healthy → capturedLive', () => {
    const r = deriveMotion(
      { ...base, status: { kind: 'connected', latencyMs: 20 }, captured: true },
      false,
    );
    expect(r.state).toBe('capturedLive');
    expect(r.flags.captureGlow).toBe(true);
  });

  it('connected + released + healthy → releasedLive', () => {
    const r = deriveMotion(
      { ...base, status: { kind: 'connected', latencyMs: 20 }, captured: false },
      false,
    );
    expect(r.state).toBe('releasedLive');
    expect(r.flags.captureGlow).toBe(false);
  });

  it('connected + rtt > 150 → degraded (regardless of capture)', () => {
    const r = deriveMotion(
      { ...base, status: { kind: 'connected', latencyMs: DEGRADED_RTT_MS + 1 }, captured: true },
      false,
    );
    expect(r.state).toBe('degraded');
  });

  it('reconnecting → reconnecting + sweep flag', () => {
    const r = deriveMotion({ ...base, status: { kind: 'reconnecting' } }, false);
    expect(r.state).toBe('reconnecting');
    expect(r.flags.reconnectSweep).toBe(true);
  });

  it('poweredOff', () => {
    expect(deriveMotion({ ...base, status: { kind: 'poweredOff' } }, false).state).toBe(
      'poweredOff',
    );
  });

  it('error → closed', () => {
    expect(deriveMotion({ ...base, status: { kind: 'error' } }, false).state).toBe('closed');
  });

  it('backgrounded overrides connected → idle + idleTilt', () => {
    const r = deriveMotion(
      {
        ...base,
        status: { kind: 'connected', latencyMs: 10 },
        captured: true,
        backgrounded: true,
      },
      false,
    );
    expect(r.state).toBe('idle');
    expect(r.flags.idleTilt).toBe(true);
    expect(r.flags.throughput).toBe(false);
  });
});

describe('deriveMotion — throughput flag', () => {
  it('on when live + bytes flowing', () => {
    const r = deriveMotion(
      { ...base, status: { kind: 'connected', latencyMs: 10, bytesIn: 1000, bytesOut: 0 } },
      false,
    );
    expect(r.flags.throughput).toBe(true);
  });
  it('off at 0 B/s', () => {
    const r = deriveMotion(
      { ...base, status: { kind: 'connected', latencyMs: 10, bytesIn: 0, bytesOut: 0 } },
      false,
    );
    expect(r.flags.throughput).toBe(false);
  });
});

describe('deriveMotion — reduced motion gate', () => {
  it('kills animated layers but keeps static glow + bars', () => {
    const r = deriveMotion(
      { ...base, status: { kind: 'connected', latencyMs: 10 }, captured: true },
      true,
    );
    expect(r.reducedMotion).toBe(true);
    // animated → off
    expect(r.flags.latencyRing).toBe(false);
    expect(r.flags.throughput).toBe(false);
    expect(r.flags.keystrokeRipple).toBe(false);
    expect(r.flags.cursorHandoff).toBe(false);
    expect(r.flags.connectFlip).toBe(false);
    expect(r.flags.idleTilt).toBe(false);
    // static-capable → kept
    expect(r.flags.captureGlow).toBe(true);
    expect(r.flags.signalBars).toBe(true);
  });

  it('reconnect sweep is killed under reduced motion', () => {
    const r = deriveMotion({ ...base, status: { kind: 'reconnecting' } }, true);
    expect(r.flags.reconnectSweep).toBe(false);
    expect(r.flags.signalBars).toBe(true);
  });
});

describe('signal bars mapping', () => {
  it('count by health', () => {
    expect(signalActiveBars(20, true)).toBe(4);
    expect(signalActiveBars(100, true)).toBe(3);
    expect(signalActiveBars(200, true)).toBe(2);
    expect(signalActiveBars(500, true)).toBe(1);
    expect(signalActiveBars(20, false)).toBe(0);
  });
  it('colour by health', () => {
    expect(signalColor(20, true)).toContain('green');
    expect(signalColor(100, true)).toContain('warn');
    expect(signalColor(500, true)).toContain('danger');
    expect(signalColor(20, false)).toContain('warn');
  });
});

describe('latency ring mapping', () => {
  it('cadence clamps to [800, 3000]', () => {
    expect(latencyCadenceMs(10)).toBe(800);
    expect(latencyCadenceMs(1500)).toBe(1500);
    expect(latencyCadenceMs(9999)).toBe(3000);
    expect(latencyCadenceMs(Number.NaN)).toBe(3000);
  });
  it('colour thresholds', () => {
    expect(latencyColor(10)).toContain('green');
    expect(latencyColor(100)).toContain('warn');
    expect(latencyColor(200)).toContain('danger');
  });
});
