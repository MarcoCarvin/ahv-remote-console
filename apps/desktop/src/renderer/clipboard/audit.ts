/**
 * Clipboard audit metadata — the ONLY shape the renderer can hand
 * to an audit emitter for clipboard transfers.
 *
 * The whole point is the type signature: there's no `text` field,
 * no `payload`, no `content`. A reviewer can grep the codebase for
 * `ClipboardMeta` and convince themselves no path exists that
 * leaks plaintext into an audit record. The runtime's audit logger
 * (apps/runtime/internal/audit/audit.go) already records the same
 * shape — this keeps the renderer side consistent.
 *
 * If a future caller actually needs the text (e.g. to do DLP
 * pattern matching locally), they must do it BEFORE constructing
 * ClipboardMeta and only push the *result* of the check (a tag, a
 * verdict) — not the text — into the audit envelope.
 *
 * See docs/INTERACTION-ARCH.md §9 (Security) and §14
 * (Anti-pattern remediation).
 */

import type { Direction } from './Ledger';

export interface ClipboardMeta {
  direction: Direction;
  /** Hex-encoded SHA-256 of the payload bytes. */
  hash: string;
  /** Payload size in bytes. */
  bytes: number;
  /** MIME-ish format identifier; "text/plain" is the only one wired today. */
  format: string;
  /** Sub-millisecond wall clock for ordering across renderer + main. */
  ts: number;
  /**
   * Opaque session ID — runtime uses this to scope audit events to
   * a specific VM tab. Empty for renderer-internal events that
   * haven't crossed the runtime boundary yet.
   */
  sessionId: string;
  /**
   * Was the payload truncated by the renderer for safety (e.g.
   * paste-as-typing 2KB cap)? If so, downstream audit consumers
   * can decide whether to flag.
   */
  truncated: boolean;
}

/**
 * Type-signed emitter contract. Implementations MUST NOT accept any
 * other shape — adding a parameter would require updating every
 * concrete emitter, surfacing the change in code review. Function
 * type rather than class so the dependency is trivially substitutable
 * in tests.
 */
export type ClipboardAuditEmit = (meta: ClipboardMeta) => void;

/**
 * No-op emitter for the default case where the renderer doesn't
 * locally audit clipboard (runtime does the canonical recording on
 * the IPC boundary). Kept here so callers can always provide a
 * stable function reference and the engine never needs an `if
 * (emitter)` branch.
 */
export const noopClipboardAudit: ClipboardAuditEmit = () => {
  /* noop: renderer-side audit disabled; runtime records canonically */
};
