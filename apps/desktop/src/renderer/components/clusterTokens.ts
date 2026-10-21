// Non-component shared building blocks for the Select-Cluster screen family:
// the tool-grade brand tokens, status mapping, and customer/environment
// grouping. Split out of clusterShared.tsx so that file exports ONLY React
// components — a requirement for Vite React Fast Refresh (a module that mixes
// component and non-component exports forces a full reload on every edit).
import type { Endpoint, EndpointHealthStatus, UserIdentity } from '@ahv-remote-console/protocol';

// Tool-grade brand tokens (defined in packages/ui/src/tokens.css). On these
// light screens, color conveys status only; host:port/latency are mono.
export const T = {
  sans: 'var(--sans)',
  mono: 'var(--mono)',
  blue: 'var(--blue)',
  blueD: 'var(--blueD)',
  blueTint: 'var(--blueTint)',
  green: 'var(--green)',
  greenD: 'var(--greenD)',
  amber: 'var(--amber)',
  red: 'var(--red)',
  ink: 'var(--ink)',
  ink2: 'var(--ink2)',
  ink3: 'var(--ink3)',
  ink4: 'var(--ink4)',
  bg: 'var(--bg)',
  canvas: 'var(--canvas)',
  soft: 'var(--soft)',
  line: 'var(--line)',
  line2: 'var(--line2)',
  // Lobby + VM-Console redesign tokens (see packages/ui/src/tokens.css).
  blueDk: 'var(--blueDk)',
  blueL: 'var(--blueL)',
  bluePale: 'var(--bluePale)',
  blueGhost: 'var(--blueGhost)',
  greenL: 'var(--greenL)',
  paper: 'var(--paper)',
  soft2: 'var(--soft2)',
  rule: 'var(--rule)',
  rule2: 'var(--rule2)',
} as const;

export const noDrag = { WebkitAppRegion: 'no-drag' } as React.CSSProperties;

/**
 * Avatar initials for the signed-in user (lobby account menu): first letters
 * of the first two display-name words ("Marco Carvin" → MC); otherwise the
 * first two characters of the username's local part ("si-ai@…" → SI — a
 * username is an opaque identifier, NOT a "first-name last-name" pair, so it
 * is never split on separators). Null when no identity is available yet, so
 * the caller can fall back.
 */
export function userInitials(identity: UserIdentity | null | undefined): string | null {
  if (!identity) return null;
  const dn = (identity.display_name ?? '').trim();
  const words = dn.split(/\s+/).filter(Boolean);
  if (words.length >= 2) return (words[0]![0]! + words[1]![0]!).toUpperCase();
  const base = dn || (identity.username ?? '').split('@', 1)[0]!.trim();
  return base ? base.slice(0, 2).toUpperCase() : null;
}

// The brand mood for the connection-manager signal stage.
export type SignalMood = 'idle' | 'active' | 'error';

// ── Customer / environment grouping ──────────────────────────────────────
// A managed-service operator organizes clusters by CUSTOMER, each with one or
// more ENVIRONMENTS (PROD / DR / STAGING …). Those are explicit fields set in
// the Add/Edit form and persisted in the endpoint's `tags` map
// (tags.customer / tags.environment). For clusters created before those fields
// existed, we fall back to parsing the label's "Customer — Environment"
// naming convention so they still group sensibly instead of all landing under
// "Ungrouped".
export const UNGROUPED = 'Ungrouped';

// Exported for unit tests; the app consumes it only via customerOf/environmentOf.
export function splitLabel(label: string): { customer: string; environment: string } {
  const s = (label || '').trim();
  // Require an explicit separator (em/en dash, |, /, :, or a SPACED hyphen) so
  // ordinary hyphenated names like "prod-cluster-01" are not split.
  const m = /^(.*?)\s*[—–|/:]\s*(.+)$/.exec(s) ?? /^(.*?)\s+-\s+(.+)$/.exec(s);
  if (m) return { customer: (m[1] ?? '').trim(), environment: (m[2] ?? '').trim() };
  return { customer: '', environment: '' };
}

// Module-private: only groupByCustomer (below) consumes this; not re-exported.
function customerOf(ep: Endpoint): string {
  const t = ep.tags?.customer?.trim();
  // eslint-disable-next-line @typescript-eslint/prefer-nullish-coalescing -- intentional falsy check: an empty tag/customer string must fall through to the next source
  return t || splitLabel(ep.label).customer || UNGROUPED;
}

export function environmentOf(ep: Endpoint): string {
  const t = ep.tags?.environment?.trim();
  // eslint-disable-next-line @typescript-eslint/prefer-nullish-coalescing -- intentional falsy check: an empty environment tag must fall through to splitLabel
  return t || splitLabel(ep.label).environment;
}

export interface EndpointGroup {
  customer: string;
  endpoints: Endpoint[];
}

// Groups endpoints by customer; named customers sort alphabetically, with the
// catch-all "Ungrouped" always last.
export function groupByCustomer(endpoints: Endpoint[]): EndpointGroup[] {
  const map = new Map<string, Endpoint[]>();
  for (const ep of endpoints) {
    const c = customerOf(ep);
    const arr = map.get(c);
    if (arr) arr.push(ep);
    else map.set(c, [ep]);
  }
  return [...map.entries()]
    .sort(([a], [b]) =>
      a === UNGROUPED ? 1 : b === UNGROUPED ? -1 : a.localeCompare(b, undefined, { numeric: true }),
    )
    .map(([customer, eps]) => ({ customer, endpoints: eps }));
}

// Reachability state plus the UI-only "checking" shown while a probe runs.
export type DotState = EndpointHealthStatus | 'checking';

export function statusMeta(s: DotState): { color: string; label: string } {
  switch (s) {
    case 'healthy':
      return { color: T.green, label: 'Reachable' };
    case 'offline':
      return { color: T.red, label: 'Offline' };
    default:
      return { color: T.ink4, label: 'Checking…' };
  }
}

export function hostPort(rawUrl: string): string {
  try {
    const u = new URL(rawUrl);
    return u.port ? `${u.hostname}:${u.port}` : u.hostname;
  } catch {
    return rawUrl;
  }
}
