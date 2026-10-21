/**
 * Clipboard loop-suppression ledger.
 *
 * Replaces the previous `lastSeen` string-equality check in the
 * Agent bridge and the renderer's pollClipboard. Two failure
 * modes that ledger fixes which string-equality could not:
 *
 *   1. Legitimate round-trip: operator copies "secret" on host,
 *      pushes to VM, guest copies the same "secret" back on host.
 *      String-equality drops the second one as a loop; the ledger
 *      sees that the (HOST→VM, hash) entry is from >threshold ms
 *      ago and lets the (VM→HOST) transfer through.
 *
 *   2. Multi-format: the same plain-text payload arrives once as
 *      "text/plain" and once as "text/html" (just plain wrapping).
 *      Ledger keyed on direction+hash+format treats these as
 *      separate transfers; string-equality would conflate them.
 *
 * The ledger is bounded (LRU): we never store the plaintext, only
 * a fixed-size SHA-256 hash + direction + format + timestamp.
 * Memory cost is ~80B per remembered transfer; cap at 128 entries
 * keeps the cost negligible even under aggressive copy/paste.
 *
 * Threshold semantics: an entry "expires" after `windowMs` so that
 * legitimate re-copies of the same text (operator copies again
 * intentionally) are not silently dropped forever. The default
 * 10s window matches operator perception of "I did that 'just now'
 * vs 'a while ago'", and is short enough that deliberately re-copying
 * the same text a few seconds later still transfers. The guest agent
 * carries its own (3s) echo suppression as the first line of defense,
 * so this window only needs to cover a fast round-trip, not the worst-
 * case slow-serial transfer time.
 */

import { sha256Hex } from './hash';

export type Direction = 'host_to_vm' | 'vm_to_host';

export interface LedgerEntry {
  direction: Direction;
  hash: string;
  format: string;
  ts: number; // ms epoch
}

export interface Ledger {
  /**
   * Returns true if forwarding `(direction, hash, format)` would be
   * a loop (the *opposite* direction recorded the same payload
   * within `windowMs`). Side effect: even when returning true, the
   * matching entry is *not* consumed — repeated checks within the
   * window will keep returning true. This keeps the dedupe robust
   * against retransmits/dups arriving close together.
   */
  isLoop(direction: Direction, hash: string, format: string): boolean;
  /** Record a transfer in the ledger. */
  record(direction: Direction, hash: string, format: string): void;
  /** Number of entries currently held (mostly for tests/telemetry). */
  size(): number;
  /** Drop all entries; e.g. on session reconnect. */
  clear(): void;
}

export interface LedgerOptions {
  /** Max entries before LRU eviction. Default 64. */
  capacity?: number;
  /** Window in ms during which a transfer is considered "echo-able". Default 10_000. */
  windowMs?: number;
  /** Time source. Defaults to Date.now; injectable for tests. */
  now?: () => number;
}

export function createLedger(opts: LedgerOptions = {}): Ledger {
  const capacity = opts.capacity ?? 128;
  const windowMs = opts.windowMs ?? 10_000;
  const now = opts.now ?? (() => Date.now());

  // Linked list semantics via array push/splice — N is small enough
  // (<=64) that the O(N) operations don't matter, and the simpler
  // structure is easier to audit.
  const entries: LedgerEntry[] = [];

  const evictExpired = (cutoff: number) => {
    while (entries.length > 0) {
      const head = entries[0];
      if (!head || head.ts >= cutoff) return;
      entries.shift();
    }
  };

  return {
    isLoop(direction, hash, format) {
      const cutoff = now() - windowMs;
      evictExpired(cutoff);
      const opposite: Direction = direction === 'host_to_vm' ? 'vm_to_host' : 'host_to_vm';
      // Linear scan — small N, branch-light, no map allocations.
      for (let i = entries.length - 1; i >= 0; i--) {
        const e = entries[i];
        if (!e) continue;
        if (e.direction === opposite && e.hash === hash && e.format === format) {
          return true;
        }
      }
      return false;
    },
    record(direction, hash, format) {
      entries.push({ direction, hash, format, ts: now() });
      while (entries.length > capacity) entries.shift();
    },
    size() {
      return entries.length;
    },
    clear() {
      entries.length = 0;
    },
  };
}

export { sha256Hex };
