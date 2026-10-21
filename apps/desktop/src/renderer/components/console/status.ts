/**
 * Console status types + pure status helpers, extracted verbatim from
 * ConsoleView (FE-01). ConsoleView re-exports ConsoleStatus / ConsoleErrorCause
 * so its public API (imported by ConsoleWindowApp) is unchanged.
 */

// ConsoleErrorCause classifies WHY a console terminally failed, so the error
// overlay can offer the right one-click recovery. Mirrors the proxy's typed
// `fatal` control frame (apps/runtime/internal/proxy/proxy.go classifyFatal +
// frame_too_large):
//   - session_expired — the Prism sign-in lapsed; the fix is to re-authenticate
//   - rbac_denied      — authenticated but not permitted for this VM
//   - unreachable      — network/VM problem; the fix is to retry
//   - vm_not_found     — the VM was deleted/moved; reopen from the list
//   - config_error     — permanent TLS/pin config fault; fix the endpoint
//   - session_closed   — the runtime session ended (restart/reap); reopen
//   - frame_too_large  — protocol fault (oversized frame); reopen the console
export type ConsoleErrorCause =
  | 'session_expired'
  | 'rbac_denied'
  | 'unreachable'
  | 'vm_not_found'
  | 'config_error'
  | 'session_closed'
  | 'frame_too_large';

const KNOWN_CAUSES: readonly string[] = [
  'session_expired',
  'rbac_denied',
  'unreachable',
  'vm_not_found',
  'config_error',
  'session_closed',
  'frame_too_large',
];

// isRetryableCause reports whether a plain Reconnect can plausibly succeed.
// `unreachable` is a transient network/VM issue; `session_closed` (runtime
// restarted / idle-reaped) and `frame_too_large` (one bad frame) are recovered
// by the console window re-minting a fresh session on Reconnect (see
// ConsoleWindowApp.reopenStream). The rest are permanent (deleted VM, bad cert,
// RBAC) or need a different action (session_expired → sign in), so the overlay
// hides Reconnect rather than inviting a futile retry that reproduces the fault.
export function isRetryableCause(cause: ConsoleErrorCause | undefined): boolean {
  return (
    cause === undefined ||
    cause === 'unreachable' ||
    cause === 'session_closed' ||
    cause === 'frame_too_large'
  );
}

export type ConsoleStatus =
  | { kind: 'connecting' }
  | { kind: 'connected'; latencyMs: number; bytesIn: number; bytesOut: number }
  | { kind: 'reconnecting'; reason?: string; attempt?: number; max?: number }
  // The upstream reports the VM is powered off — an expected, recoverable
  // state (not an error). The proxy polls for power-on; the console resumes
  // automatically. Rendered as a calm notice, not the reconnect spinner.
  | { kind: 'poweredOff' }
  // The VM is powered on but its console is not serving yet (just booted /
  // QEMU VNC coming up). Like poweredOff this is expected and self-resolving —
  // the proxy retries with backoff and emits `vm_starting`. Rendered as a calm
  // "VM is starting…" notice, never the alarming reconnect spinner/toasts.
  | { kind: 'starting' }
  | { kind: 'error'; reason: string; cause?: ConsoleErrorCause };

// normalizeErrorCause narrows the proxy's `cause` string to the known union,
// dropping anything unexpected (so a future proxy cause can't crash the UI).
// An unknown cause maps to undefined, which the overlay renders as a neutral,
// non-recovery-presuming "lost connection" with the proxy's reason as detail.
export function normalizeErrorCause(c: string | undefined): ConsoleErrorCause | undefined {
  return c !== undefined && KNOWN_CAUSES.includes(c) ? (c as ConsoleErrorCause) : undefined;
}

// Live connection → dot colour + paired text (status is never colour-only).
export function statusLabel(status: ConsoleStatus): { color: string; text: string } {
  switch (status.kind) {
    case 'connected':
      return { color: 'var(--greenL)', text: `${status.latencyMs}ms` };
    case 'connecting':
      return { color: 'var(--amber)', text: 'connecting' };
    case 'reconnecting':
      return { color: 'var(--amber)', text: 'reconnecting' };
    case 'starting':
      return { color: 'var(--amber)', text: 'starting' };
    case 'poweredOff':
      return { color: 'var(--amber)', text: 'powered off' };
    default:
      return { color: 'var(--red)', text: 'disconnected' };
  }
}
