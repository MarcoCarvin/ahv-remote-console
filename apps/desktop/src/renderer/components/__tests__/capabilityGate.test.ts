/**
 * Tests for capabilityGate — the pure decision table behind the renderer's
 * permission-aware gating of the clipboard-prerequisite controls (FE-01
 * idiom: ConsoleView only binds the result to JSX).
 *
 * The semantics pinned here:
 *   - 'unknown' is NOT denied: everything renders normally until a denial
 *     is PROVEN (a 403 evidenced runtime-side, or a read-only probe);
 *   - vm_update denied hides BOTH the prerequisites and add-channel
 *     controls (absent, not disabled) and raises the one-time notice;
 *   - image_create denied only matters while the agent ISO is NOT already
 *     in the cluster catalog — catalog presence removes the requirement
 *     entirely (the full flow works for VM-update-only operators);
 *   - vm_power denied only removes the consent dialog's [Restart now];
 *   - notice copy names the Prism role as the cause, never the app, and
 *     carries no transport jargon.
 */

import { describe, it, expect } from 'vitest';
import type { EndpointCapabilities } from '@ahv-remote-console/protocol';
import { capabilityGate, CAPABILITY_NOTICE_COPY } from '../capabilityGate';

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

const SHOW_ALL = {
  showPrereqs: true,
  showCatalog: true,
  offerRestart: true,
  notice: null,
};

describe('capabilityGate', () => {
  it('shows everything when capabilities are absent (no health payload yet)', () => {
    expect(capabilityGate(undefined)).toEqual(SHOW_ALL);
    expect(capabilityGate(null)).toEqual(SHOW_ALL);
  });

  it('shows everything while all capabilities are unknown (unknown is NOT denied)', () => {
    expect(capabilityGate(caps())).toEqual(SHOW_ALL);
  });

  it('shows everything when capabilities are allowed', () => {
    expect(
      capabilityGate(
        caps({
          image_read: 'allowed',
          image_create: 'allowed',
          vm_update: 'allowed',
          vm_power: 'allowed',
          agent_iso_in_catalog: true,
        }),
      ),
    ).toEqual(SHOW_ALL);
  });

  it('vm_update denied hides prerequisites AND add-channel, with the one-time notice', () => {
    const g = capabilityGate(caps({ vm_update: 'denied' }));
    expect(g.showPrereqs).toBe(false);
    expect(g.notice).toBe('vm_update_denied');
    // vm_power is still unknown — the restart offer is not degraded.
    expect(g.offerRestart).toBe(true);
  });

  it('vm_power denied only removes the restart offer', () => {
    const g = capabilityGate(caps({ vm_power: 'denied' }));
    expect(g).toEqual({ ...SHOW_ALL, offerRestart: false });
  });

  it('image_create denied + ISO missing from catalog hides prerequisites with the publish notice', () => {
    const g = capabilityGate(caps({ image_create: 'denied', agent_iso_in_catalog: false }));
    expect(g.showPrereqs).toBe(false);
    // Adding the channel is a VM update, not an image op — stays visible.
    expect(g.offerRestart).toBe(true);
    expect(g.notice).toBe('image_publish_required');
  });

  it('catalog presence removes the image_create requirement entirely', () => {
    const g = capabilityGate(caps({ image_create: 'denied', agent_iso_in_catalog: true }));
    expect(g).toEqual(SHOW_ALL);
  });

  it('a missing catalog image alone (image_create unknown) degrades nothing', () => {
    expect(capabilityGate(caps({ agent_iso_in_catalog: false }))).toEqual(SHOW_ALL);
  });

  it('image_read denied hides ONLY the catalog list (mount/eject still gate on vm_update)', () => {
    const g = capabilityGate(caps({ image_read: 'denied' }));
    expect(g.showCatalog).toBe(false);
    // The catalog LIST needs image_read, but everything else is untouched —
    // the prerequisites control and restart offer gate on vm_update / vm_power.
    expect(g.showPrereqs).toBe(true);
    expect(g.offerRestart).toBe(true);
    expect(g.notice).toBeNull();
  });

  it('image_read unknown still shows the catalog (unknown is NOT denied)', () => {
    expect(capabilityGate(caps()).showCatalog).toBe(true);
  });

  it('vm_update denial takes precedence over the publish notice', () => {
    const g = capabilityGate(
      caps({ vm_update: 'denied', image_create: 'denied', agent_iso_in_catalog: false }),
    );
    expect(g.showPrereqs).toBe(false);
    expect(g.notice).toBe('vm_update_denied');
  });

  it('uses the agreed vm_update denial copy verbatim', () => {
    expect(CAPABILITY_NOTICE_COPY.vm_update_denied).toBe(
      "Your Prism role doesn't allow VM configuration changes — ask an administrator to run setup.",
    );
  });

  it('publish copy asks the administrator to publish the agent image', () => {
    expect(CAPABILITY_NOTICE_COPY.image_publish_required).toMatch(
      /ask your administrator to publish the agent image/i,
    );
  });

  it('notice copy names the Prism role as the cause and stays jargon-free', () => {
    for (const text of Object.values(CAPABILITY_NOTICE_COPY)) {
      expect(text).toMatch(/prism role/i);
      expect(text).not.toMatch(/com3|serial|vdagent|403|rbac/i);
    }
  });
});
