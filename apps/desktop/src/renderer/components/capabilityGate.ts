/**
 * capabilityGate — permission-aware gating of the clipboard-prerequisite
 * controls, driven by the per-endpoint health payload's `capabilities`
 * (GET /v1/config/endpoints/{id}/health).
 *
 * Capabilities mirror EVIDENCE, not policy: the runtime marks an operation
 * 'denied' only after a read-only probe proved it or a real Prism 403 was
 * observed (the evidence cache clears on re-auth). 'unknown' is NOT denied
 * — every control renders normally until a denial is proven, so a fresh
 * sign-in degrades nothing.
 *
 * The decision table is a pure function (FE-01 idiom, like
 * createConsoleFocusPolicy and serialCopy): ConsoleView only binds the
 * result to JSX. The hook below is the console window's health consumer —
 * the connection-manager poller (Shell.probeEndpoint) lives in the MAIN
 * window, and a pop-out console is its own OS window, so it reads the same
 * health surface itself.
 */

import { useCallback, useEffect, useState } from 'react';
import type { EndpointCapabilities, EndpointHealth } from '@ahv-remote-console/protocol';

export type CapabilityNoticeKey = 'vm_update_denied' | 'image_publish_required';

export interface CapabilityGate {
  /** Render the single "Install prerequisites" setup control. */
  showPrereqs: boolean;
  /**
   * Render the "Mount ISO from catalog…" list item. Gated on image_read only
   * (listing the catalog is a read); hidden ONLY on a PROVEN image_read denial,
   * shown while unknown so a fresh sign-in degrades nothing. The mount/eject
   * actions themselves are VM-spec updates, still gated on `showPrereqs`.
   */
  showCatalog: boolean;
  /** The consent dialog may offer [Restart now]. */
  offerRestart: boolean;
  /** One-time learned-denial notice to surface (null = nothing to say). */
  notice: CapabilityNoticeKey | null;
}

/**
 * Operator copy for the learned-denial notices. The cause is always the
 * Prism ROLE, never the app, and the copy carries no transport jargon.
 * Shown ONCE per endpoint when the denial is first learned (ConsoleView
 * persists "already shown"); the gated controls are simply absent after.
 */
export const CAPABILITY_NOTICE_COPY: Record<CapabilityNoticeKey, string> = {
  vm_update_denied:
    "Your Prism role doesn't allow VM configuration changes — ask an administrator to run setup.",
  image_publish_required:
    "The agent installer image isn't available on this cluster, and your Prism role " +
    "doesn't allow adding images — ask your administrator to publish the agent image.",
};

export function capabilityGate(capabilities?: EndpointCapabilities | null): CapabilityGate {
  // vm_update gates the setup control: every half of the prerequisite flow
  // (CD-ROM attach, agent-channel add) is a VM spec update.
  const vmUpdateDenied = capabilities?.vm_update === 'denied';
  // image_create only matters while the agent ISO is NOT already in the
  // cluster catalog — catalog presence removes the requirement entirely
  // (attach-only mounts work for VM-update-only operators).
  const imagePublishRequired =
    capabilities?.image_create === 'denied' && capabilities?.agent_iso_in_catalog === false;
  return {
    showPrereqs: !vmUpdateDenied && !imagePublishRequired,
    // Catalog listing only needs image_read; hide solely on a PROVEN denial.
    showCatalog: capabilities?.image_read !== 'denied',
    offerRestart: capabilities?.vm_power !== 'denied',
    notice: vmUpdateDenied
      ? 'vm_update_denied'
      : imagePublishRequired
        ? 'image_publish_required'
        : null,
  };
}

/** The slice of the SDK client the consumer needs — narrow so tests can stub it. */
export interface EndpointHealthClient {
  getEndpointHealth(endpointId: string): Promise<{ health: EndpointHealth }>;
}

/**
 * useEndpointCapabilities — fetch the endpoint's capability evidence once
 * when the endpoint resolves, plus an explicit refresh() after operations
 * that generate new evidence runtime-side (mount, add-channel, the consent
 * cycle). No interval poll: capabilities only move at auth epochs and on
 * evidenced operations, both of which this window observes. A fetch
 * failure or an absent `capabilities` field resolves to unknown (show
 * everything) — reachability noise must never hide controls.
 */
export function useEndpointCapabilities(
  client: EndpointHealthClient,
  endpointId: string,
): { capabilities: EndpointCapabilities | null; refresh: () => void } {
  const [capabilities, setCapabilities] = useState<EndpointCapabilities | null>(null);
  const refresh = useCallback(() => {
    if (!endpointId) return;
    client
      .getEndpointHealth(endpointId)
      .then(({ health }) => setCapabilities(health.capabilities ?? null))
      .catch(() => {
        /* keep last known evidence — a blip must not re-show hidden controls */
      });
  }, [client, endpointId]);
  useEffect(() => {
    refresh();
  }, [refresh]);
  return { capabilities, refresh };
}
