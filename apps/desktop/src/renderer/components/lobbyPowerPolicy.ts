/**
 * lobbyPowerPolicy — the pure decision table behind the lobby's per-tile power
 * controls (FE-01 idiom, like capabilityGate / createConsoleFocusPolicy /
 * serialCopy: the JSX only binds these results). The renderer vitest env is
 * `node` (no jsdom), so all the testable logic lives here and VMTile stays thin.
 *
 * The lobby surfaces three GRACEFUL verbs only — power-on, ACPI shutdown, ACPI
 * reboot — matching the operator-transparency stance in power_v3.go. Hard
 * OFF/RESET exist in the SDK action union but are intentionally NOT exposed
 * here; surface them later (behind their own extra-scary confirm) only if asked.
 */

import type { EndpointCapabilities } from '@ahv-remote-console/protocol';

/**
 * The three lobby-surfaced power verbs (a subset alias of the SDK's action
 * union — this is a renderer-local type, not a wire/protocol type):
 *   'ON'          — power on a stopped VM (one-click, no confirm).
 *   'OFF'         — graceful ACPI shutdown (confirm required).
 *   'ACPI_REBOOT' — graceful restart / cycle (confirm required).
 */
export type PowerActionKind = 'ON' | 'OFF' | 'ACPI_REBOOT';

/** What `onPowerAction` dispatches to the SDK. 'OFF' maps to the graceful
 *  ACPI shutdown verb, never the hard OFF — see the module note. */
export const POWER_SDK_ACTION: Record<PowerActionKind, 'ON' | 'ACPI_SHUTDOWN' | 'ACPI_REBOOT'> = {
  ON: 'ON',
  OFF: 'ACPI_SHUTDOWN',
  ACPI_REBOOT: 'ACPI_REBOOT',
};

export interface LobbyPowerGate {
  /** Render the per-tile power control group at all. */
  show: boolean;
  /** The buttons are interactive. Mirrors `show` today (no second axis), but
   *  kept distinct so a future "unknown disables with a tooltip" variant is a
   *  one-line change. */
  enabled: boolean;
}

/**
 * Gate the lobby power controls on the per-endpoint vm_power evidence. PROVEN
 * 'denied' hides the controls entirely (absent, not disabled); 'unknown' /
 * 'allowed' / an absent payload all SHOW them — mirroring capabilityGate's
 * `offerRestart` semantics ('unknown' is NOT denied, so a fresh sign-in
 * degrades nothing).
 */
export function lobbyPowerGate(capabilities?: EndpointCapabilities | null): LobbyPowerGate {
  const show = capabilities?.vm_power !== 'denied';
  return { show, enabled: show };
}

/**
 * The power verbs offered for a VM in the given power state (case-insensitive,
 * matching Lobby's `isOnState`): an ON VM offers Shut down + Restart; an OFF /
 * unknown VM offers Power on.
 */
export function powerActionsFor(status: string): PowerActionKind[] {
  return status.toLowerCase() === 'on' ? ['OFF', 'ACPI_REBOOT'] : ['ON'];
}

/**
 * Destructive / consequential actions require an explicit in-UI confirm before
 * dispatch (hard invariant). Power-on is benign — one-click.
 */
export function requiresConfirm(action: PowerActionKind): boolean {
  return action === 'OFF' || action === 'ACPI_REBOOT';
}

/** Button labels — jargon-free, no emoji. */
export const POWER_ACTION_LABEL: Record<PowerActionKind, string> = {
  ON: 'Power on',
  OFF: 'Shut down',
  ACPI_REBOOT: 'Restart',
};

/** Confirm-dialog body copy. Graceful semantics are stated plainly so the
 *  operator knows the guest may take a moment (or ignore the ACPI request). */
export const POWER_CONFIRM_COPY: Record<PowerActionKind, string> = {
  ON: '',
  OFF: 'Send a shutdown request to this VM? The guest closes gracefully and may take a moment.',
  ACPI_REBOOT:
    'Send a restart request to this VM? The guest reboots gracefully and may take a moment.',
};
