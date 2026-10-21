/**
 * Tests for the pure status helpers extracted out of ConsoleView (FE-01):
 * normalizeErrorCause (narrows the proxy's free-form cause string) and
 * statusLabel (maps a live ConsoleStatus to its dot colour + text). Both are
 * DOM-free, so they import cleanly into the node test environment.
 */

import { describe, it, expect } from 'vitest';
import {
  normalizeErrorCause,
  isRetryableCause,
  statusLabel,
  type ConsoleStatus,
} from '../console/status';

describe('normalizeErrorCause', () => {
  it('passes through every known cause (must stay in sync with the proxy)', () => {
    for (const c of [
      'session_expired',
      'rbac_denied',
      'unreachable',
      'vm_not_found',
      'config_error',
      'session_closed',
      'frame_too_large',
    ]) {
      expect(normalizeErrorCause(c)).toBe(c);
    }
  });

  it('drops unknown / undefined causes so a future proxy cause cannot crash the UI', () => {
    expect(normalizeErrorCause(undefined)).toBeUndefined();
    expect(normalizeErrorCause('')).toBeUndefined();
    expect(normalizeErrorCause('something_new')).toBeUndefined();
  });
});

describe('isRetryableCause', () => {
  it('transient + re-mintable causes offer a plain Reconnect', () => {
    expect(isRetryableCause(undefined)).toBe(true);
    expect(isRetryableCause('unreachable')).toBe(true);
    // session_closed (runtime restart / idle-reap) and frame_too_large are now
    // recoverable: Reconnect re-mints a fresh session (ConsoleWindowApp.reopenStream).
    expect(isRetryableCause('session_closed')).toBe(true);
    expect(isRetryableCause('frame_too_large')).toBe(true);
  });
  it('permanent causes are NOT retryable (no futile Reconnect)', () => {
    for (const c of ['rbac_denied', 'vm_not_found', 'config_error'] as const) {
      expect(isRetryableCause(c)).toBe(false);
    }
    // session_expired routes to Sign in, not Reconnect
    expect(isRetryableCause('session_expired')).toBe(false);
  });
});

describe('statusLabel', () => {
  it('connected shows the latency in ms with the green dot', () => {
    const s: ConsoleStatus = { kind: 'connected', latencyMs: 42, bytesIn: 0, bytesOut: 0 };
    expect(statusLabel(s)).toEqual({ color: 'var(--greenL)', text: '42ms' });
  });

  it('the transient states are amber', () => {
    expect(statusLabel({ kind: 'connecting' })).toEqual({
      color: 'var(--amber)',
      text: 'connecting',
    });
    expect(statusLabel({ kind: 'reconnecting' })).toEqual({
      color: 'var(--amber)',
      text: 'reconnecting',
    });
    expect(statusLabel({ kind: 'starting' })).toEqual({ color: 'var(--amber)', text: 'starting' });
    expect(statusLabel({ kind: 'poweredOff' })).toEqual({
      color: 'var(--amber)',
      text: 'powered off',
    });
  });

  it('error is red and labelled disconnected', () => {
    expect(statusLabel({ kind: 'error', reason: 'boom' })).toEqual({
      color: 'var(--red)',
      text: 'disconnected',
    });
  });
});
