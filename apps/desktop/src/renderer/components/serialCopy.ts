/**
 * serialCopy maps the runtime's COM3 serial-port provisioning outcome
 * (the SDK `serialNote`) to operator-facing copy for the install notice.
 *
 * The note keys stay technical (they are wire identifiers, shared with the
 * Go side and the logs) but the operator copy deliberately never says
 * COM3 / serial / vdagent — to the operator it is just "the agent channel".
 *
 * Pure and DOM-free so it can be unit-tested directly. Keep the keys in
 * sync with the producer side:
 *   - apps/runtime/pkg/sdk/sdk.go  (SerialNote doc)
 *   - plugins/ahv/plugin.go        (MountAgentISO note mapping)
 *   - plugins/ahv/internal/api/client.go (EnsureSerialPort)
 */
export type SerialNoticeKind = 'ok' | 'info' | 'warn' | 'err';

export interface SerialNotice {
  text: string;
  kind: SerialNoticeKind;
  /** Sticky notices stay until dismissed (no auto-timeout). */
  sticky: boolean;
  /**
   * When true the VM needs a full power OFF→ON before the agent channel can
   * activate (AHV cannot hot-add the serial port, and an in-guest reboot does
   * NOT restart QEMU). The UI renders the consent dialog — [Restart now]
   * re-calls mountISO with powerCycle:true, [Later] leaves the deferred
   * state. The orchestrated cycle only ever happens off that explicit
   * consent.
   */
  powerCycle?: boolean;
}

const FALLBACK: SerialNotice = {
  text: 'Agent channel ready — clipboard prerequisite OK.',
  kind: 'ok',
  sticky: false,
};

const SERIAL_COPY: Record<string, SerialNotice> = {
  '': FALLBACK,
  serial_port_present: {
    text: 'Agent channel already set up — clipboard prerequisite OK.',
    kind: 'ok',
    sticky: false,
  },
  serial_port_added: {
    text: 'Agent channel added — it activates the next time the VM starts, then clipboard works.',
    kind: 'ok',
    sticky: false,
  },
  // The consent-dialog body: ConsoleView renders this with the
  // [Restart now] / [Later] actions (powerCycle flag).
  serial_port_power_off_required: {
    text:
      'First-time setup needs a brief VM restart. The VM will shut down gracefully and ' +
      'start again (this can take a few minutes). Do it now?',
    kind: 'warn',
    sticky: true,
    powerCycle: true,
  },
  // Deferred badge: the add is armed runtime-side and applies on the next
  // observed power-off. Calm info, sticky so it survives until a later
  // success note replaces it.
  serial_port_deferred: {
    text: 'Agent channel will activate the next time this VM is powered off and on.',
    kind: 'info',
    sticky: true,
  },
  serial_port_added_cycled: {
    text: 'Setup complete — VM restarted with the agent channel.',
    kind: 'ok',
    sticky: false,
  },
  // The consented cycle hit a Prism "forbidden" on the power operation: the
  // cycle was aborted with the VM left as found, the deferred apply stays
  // armed, and the runtime does not retry. NO powerCycle action — offering
  // [Restart now] again would only re-hit the same denial.
  serial_port_power_denied: {
    text:
      "The VM was left running — your Prism role doesn't allow VM power actions. " +
      'Ask an administrator to restart it, or it will activate the next time the VM is ' +
      'powered off and on.',
    kind: 'warn',
    sticky: true,
  },
  serial_port_error: {
    text:
      'Could not set up the agent channel — clipboard sharing will not work until this is ' +
      'fixed (check Prism permissions and that the VM is reachable).',
    kind: 'err',
    sticky: true,
  },
};

/** Resolve a serial note to its operator-facing notice (never null). */
export function serialCopy(note: string): SerialNotice {
  return SERIAL_COPY[note] ?? FALLBACK;
}

/**
 * Consent-dialog gating: when the endpoint's vm_power capability is a
 * PROVEN denial (capabilityGate.offerRestart === false), the dialog must
 * never offer [Restart now] — the orchestrated cycle would only re-hit the
 * same denial. The notice keeps its sticky warn shape but drops the
 * powerCycle action and switches to deferred-only copy. Identity for
 * notices without a consent action and while the capability is unknown
 * (unknown is NOT denied). Pure, so the variant is unit-testable.
 */
export function applyRestartGate(notice: SerialNotice, offerRestart: boolean): SerialNotice {
  if (offerRestart || !notice.powerCycle) return notice;
  return {
    text:
      "First-time setup needs a brief VM restart, but your Prism role doesn't allow VM " +
      'power actions — ask an administrator to restart it, or it will activate the next ' +
      'time the VM is powered off and on.',
    kind: 'warn',
    sticky: true,
  };
}
