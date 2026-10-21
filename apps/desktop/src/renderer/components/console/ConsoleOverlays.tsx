/**
 * Terminal-error overlay pieces, extracted verbatim from ConsoleView (FE-01).
 * Pure presentational components — no closures over ConsoleView state.
 */

import { type ConsoleErrorCause, isRetryableCause } from './status';

export function OverlayButton({
  onClick,
  children,
  primary,
}: {
  onClick: () => void;
  children: string;
  primary?: boolean;
}) {
  return (
    <button
      type="button"
      onClick={onClick}
      style={{
        padding: '8px 18px',
        borderRadius: 8,
        border: primary ? '1px solid var(--hc-accent-500)' : '1px solid rgba(255,255,255,0.25)',
        background: primary ? 'var(--hc-accent-500)' : 'transparent',
        color: '#fff',
        cursor: 'pointer',
        fontSize: 13,
        fontWeight: 600,
        fontFamily: 'var(--hc-font-sans)',
      }}
    >
      {children}
    </button>
  );
}

// ErrorOverlayBody turns the terminal-error overlay into a cause-specific,
// actionable panel: session_expired -> Re-authenticate, unreachable -> Reconnect,
// rbac_denied -> explain (no retry can help). Always offers Close so the
// operator is never stuck staring at a non-interactive message.
export function ErrorOverlayBody({
  cause,
  onReconnect,
  onReauth,
  onClose,
}: {
  cause?: ConsoleErrorCause;
  onReconnect?: () => void;
  onReauth?: () => void;
  onClose: () => void;
}) {
  // Per-cause copy in plain operator language with a concrete next step — no
  // jargon (Prism/TLS/RBAC/session/runtime) and no raw proxy reason strings
  // (those stay in the logs). Retryability (the Reconnect button) is decided
  // separately by isRetryableCause; session_closed / frame_too_large are now
  // retryable because Reconnect re-mints a fresh session (ConsoleWindowApp).
  const COPY: Partial<Record<ConsoleErrorCause, { title: string; detail: string }>> = {
    session_expired: {
      title: 'Please sign in again',
      detail: 'Your sign-in timed out. Sign in to pick up right where you left off.',
    },
    rbac_denied: {
      title: "You don't have access to this VM",
      detail:
        "Your account isn't allowed to open this VM's screen. Ask your IT administrator to grant you access, then open it again.",
    },
    vm_not_found: {
      title: 'This VM is no longer available',
      detail:
        'It may have been deleted or moved. You can close this window — if it was only moved, open it again from the main app window.',
    },
    config_error: {
      title: "Can't connect securely to this VM",
      detail:
        'The secure-connection settings for this site need attention. Ask your IT administrator to check them, then open the VM again.',
    },
    session_closed: {
      title: 'Connection to this VM dropped',
      detail: 'The link to this VM was reset. Click Reconnect to pick it back up.',
    },
    frame_too_large: {
      title: 'Display hiccup',
      detail:
        "Something went wrong showing this VM's screen. Click Reconnect to try again, or close this window and open the VM again.",
    },
  };
  const copy = (cause ? COPY[cause] : undefined) ?? {
    title: 'Lost connection to the VM',
    detail: "We couldn't reach this VM. Click Reconnect to try again.",
  };
  const { title, detail } = copy;
  return (
    <>
      <div style={{ fontSize: 15, fontWeight: 600, color: 'var(--hc-error-400, #ef4444)' }}>
        {title}
      </div>
      <div
        style={{
          fontSize: 12.5,
          opacity: 0.85,
          maxWidth: 400,
          textAlign: 'center',
          lineHeight: 1.5,
        }}
      >
        {detail}
      </div>
      <div style={{ display: 'flex', gap: 10, marginTop: 6 }}>
        {cause === 'session_expired' && onReauth && (
          <OverlayButton onClick={onReauth} primary>
            Sign in
          </OverlayButton>
        )}
        {isRetryableCause(cause) && onReconnect && (
          <OverlayButton onClick={onReconnect} primary>
            Reconnect
          </OverlayButton>
        )}
        <OverlayButton onClick={onClose}>Close</OverlayButton>
      </div>
    </>
  );
}
