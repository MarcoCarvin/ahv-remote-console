/**
 * Tests for lobbyPowerPolicy — the pure decision table behind the lobby's
 * per-tile power controls (FE-01 idiom: VMTile only binds the result to JSX).
 *
 * The semantics pinned here:
 *   - vm_power 'denied' hides the controls entirely (show:false / enabled:false);
 *     'unknown' / absent / null / 'allowed' all SHOW them ('unknown' is NOT
 *     denied — same stance as capabilityGate.offerRestart);
 *   - an ON VM offers [OFF, ACPI_REBOOT]; an OFF/unknown VM offers [ON]
 *     (case-insensitive on status);
 *   - destructive actions (OFF, ACPI_REBOOT) require an explicit confirm;
 *     power-on does not;
 *   - label/copy maps are non-empty and carry no emoji / transport jargon.
 */

import { describe, it, expect } from 'vitest';
import type { EndpointCapabilities } from '@ahv-remote-console/protocol';
import {
  lobbyPowerGate,
  powerActionsFor,
  requiresConfirm,
  POWER_ACTION_LABEL,
  POWER_CONFIRM_COPY,
  POWER_SDK_ACTION,
  type PowerActionKind,
} from '../lobbyPowerPolicy';

/** All-unknown baseline; override per case. */
function caps(over: Partial<EndpointCapabilities> = {}): EndpointCapabilities {
  return {
    image_read: 'unknown',
    image_create: 'unknown',
    vm_update: 'unknown',
    vm_power: 'unknown',
    agent_iso_in_catalog: false,
    ...over,
  };
}

describe('lobbyPowerGate', () => {
  it('hides the controls when vm_power is proven denied', () => {
    expect(lobbyPowerGate(caps({ vm_power: 'denied' }))).toEqual({ show: false, enabled: false });
  });

  it('shows the controls while vm_power is unknown (unknown is NOT denied)', () => {
    expect(lobbyPowerGate(caps())).toEqual({ show: true, enabled: true });
  });

  it('shows the controls when capabilities are absent (no health payload yet)', () => {
    expect(lobbyPowerGate(undefined)).toEqual({ show: true, enabled: true });
    expect(lobbyPowerGate(null)).toEqual({ show: true, enabled: true });
  });

  it('shows the controls when vm_power is allowed', () => {
    expect(lobbyPowerGate(caps({ vm_power: 'allowed' }))).toEqual({ show: true, enabled: true });
  });
});

describe('powerActionsFor', () => {
  it('offers Shut down + Restart for an ON VM (case-insensitive)', () => {
    expect(powerActionsFor('ON')).toEqual(['OFF', 'ACPI_REBOOT']);
    expect(powerActionsFor('on')).toEqual(['OFF', 'ACPI_REBOOT']);
  });

  it('offers Power on for an OFF / unknown / empty VM', () => {
    expect(powerActionsFor('OFF')).toEqual(['ON']);
    expect(powerActionsFor('off')).toEqual(['ON']);
    expect(powerActionsFor('')).toEqual(['ON']);
    expect(powerActionsFor('UNKNOWN')).toEqual(['ON']);
  });
});

describe('requiresConfirm', () => {
  it('requires confirmation for the destructive verbs', () => {
    expect(requiresConfirm('OFF')).toBe(true);
    expect(requiresConfirm('ACPI_REBOOT')).toBe(true);
  });

  it('does not require confirmation to power on', () => {
    expect(requiresConfirm('ON')).toBe(false);
  });
});

describe('copy + action maps', () => {
  const kinds: PowerActionKind[] = ['ON', 'OFF', 'ACPI_REBOOT'];

  it('maps the lobby verbs to the graceful SDK actions (never hard OFF/RESET)', () => {
    expect(POWER_SDK_ACTION).toEqual({
      ON: 'ON',
      OFF: 'ACPI_SHUTDOWN',
      ACPI_REBOOT: 'ACPI_REBOOT',
    });
  });

  it('has a non-empty, jargon-free, emoji-free label for every verb', () => {
    for (const k of kinds) {
      const label = POWER_ACTION_LABEL[k];
      expect(label.length).toBeGreaterThan(0);
      expect(label).not.toMatch(/com3|serial|vdagent|acpi|rfb|vnc/i);
      // No emoji / non-ASCII pictographs.
      expect(label).not.toMatch(/[\u{1F000}-\u{1FAFF}\u{2600}-\u{27BF}]/u);
    }
  });

  it('has confirm copy for the destructive verbs and none for power-on', () => {
    expect(POWER_CONFIRM_COPY.ON).toBe('');
    expect(POWER_CONFIRM_COPY.OFF.length).toBeGreaterThan(0);
    expect(POWER_CONFIRM_COPY.ACPI_REBOOT.length).toBeGreaterThan(0);
    for (const k of ['OFF', 'ACPI_REBOOT'] as PowerActionKind[]) {
      expect(POWER_CONFIRM_COPY[k]).not.toMatch(/com3|serial|vdagent|acpi|rfb|vnc/i);
      expect(POWER_CONFIRM_COPY[k]).not.toMatch(/[\u{1F000}-\u{1FAFF}\u{2600}-\u{27BF}]/u);
    }
  });
});
