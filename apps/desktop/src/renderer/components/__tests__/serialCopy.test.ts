import { describe, it, expect } from 'vitest';
import { serialCopy, applyRestartGate } from '../serialCopy';

/** Every note the producer side can emit (see serialCopy.ts header). */
const ALL_NOTES = [
  '',
  'serial_port_present',
  'serial_port_added',
  'serial_port_power_off_required',
  'serial_port_deferred',
  'serial_port_added_cycled',
  'serial_port_power_denied',
  'serial_port_error',
  'totally_unknown_note',
] as const;

describe('serialCopy', () => {
  it('maps power-off-required to a sticky warn that feeds the consent restart dialog', () => {
    const n = serialCopy('serial_port_power_off_required');
    expect(n.kind).toBe('warn');
    expect(n.sticky).toBe(true);
    expect(n.powerCycle).toBe(true);
    // The copy IS the consent-dialog body: it asks the operator to approve a
    // brief graceful restart instead of instructing a manual power-off.
    expect(n.text).toMatch(/restart/i);
    expect(n.text).toMatch(/do it now\?/i);
    expect(n.text).not.toMatch(/power the VM OFF/i);
  });

  it('maps deferred to a calm sticky info badge with no consent action', () => {
    const n = serialCopy('serial_port_deferred');
    expect(n.kind).toBe('info');
    expect(n.sticky).toBe(true);
    expect(n.powerCycle).toBeFalsy();
    expect(n.text).toMatch(/next time this VM is powered off and on/i);
  });

  it('maps added-cycled to an auto-dismissing ok', () => {
    const n = serialCopy('serial_port_added_cycled');
    expect(n.kind).toBe('ok');
    expect(n.sticky).toBe(false);
    expect(n.powerCycle).toBeFalsy();
    expect(n.text).toMatch(/setup complete/i);
  });

  it('maps power-denied to a sticky warn with NO consent action (retrying would re-hit the denial)', () => {
    const n = serialCopy('serial_port_power_denied');
    expect(n.kind).toBe('warn');
    expect(n.sticky).toBe(true);
    expect(n.powerCycle).toBeFalsy();
    // Honest, role-named cause + the deferred-only outcome: the VM was left
    // as found and the change applies on the next power-off cycle.
    expect(n.text).toMatch(/prism role/i);
    expect(n.text).toMatch(
      /ask an administrator to restart it, or it will activate the next time the VM is powered off and on/i,
    );
    expect(n.text).not.toMatch(/do it now\?/i);
  });

  it('maps error to a sticky err with no consent action', () => {
    const n = serialCopy('serial_port_error');
    expect(n.kind).toBe('err');
    expect(n.sticky).toBe(true);
    expect(n.powerCycle).toBeFalsy();
  });

  it('maps added to a non-sticky ok', () => {
    const n = serialCopy('serial_port_added');
    expect(n.kind).toBe('ok');
    expect(n.sticky).toBe(false);
  });

  it('maps present to a non-sticky ok', () => {
    const n = serialCopy('serial_port_present');
    expect(n.kind).toBe('ok');
    expect(n.sticky).toBe(false);
  });

  it('falls back to a non-sticky ok for empty / unknown notes', () => {
    expect(serialCopy('').kind).toBe('ok');
    expect(serialCopy('totally_unknown_note').kind).toBe('ok');
    expect(serialCopy('totally_unknown_note').sticky).toBe(false);
  });

  it('never exposes transport jargon (COM3 / serial / vdagent) in operator copy', () => {
    for (const note of ALL_NOTES) {
      expect(serialCopy(note).text).not.toMatch(/com3|serial|vdagent/i);
    }
  });
});

describe('applyRestartGate', () => {
  it('passes the consent dialog through untouched while the power capability is not proven denied', () => {
    const consent = serialCopy('serial_port_power_off_required');
    expect(applyRestartGate(consent, true)).toBe(consent);
  });

  it('renders the consent dialog without [Restart now] when vm_power is denied', () => {
    const gated = applyRestartGate(serialCopy('serial_port_power_off_required'), false);
    // No powerCycle flag = ConsoleView renders no [Restart now]/[Later]
    // actions — just the sticky, dismissible deferred-only notice.
    expect(gated.powerCycle).toBeFalsy();
    expect(gated.sticky).toBe(true);
    expect(gated.kind).toBe('warn');
    expect(gated.text).toMatch(/prism role/i);
    expect(gated.text).toMatch(
      /ask an administrator to restart it, or it will activate the next time the VM is powered off and on/i,
    );
    expect(gated.text).not.toMatch(/do it now\?/i);
    expect(gated.text).not.toMatch(/com3|serial|vdagent/i);
  });

  it('leaves notices without a consent action alone regardless of the gate', () => {
    for (const note of ['serial_port_deferred', 'serial_port_added', 'serial_port_error']) {
      const n = serialCopy(note);
      expect(applyRestartGate(n, false)).toBe(n);
      expect(applyRestartGate(n, true)).toBe(n);
    }
  });
});
