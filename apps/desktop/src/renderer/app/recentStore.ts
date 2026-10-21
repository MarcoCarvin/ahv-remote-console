/**
 * localStorage-backed stores for the v2 lobby + endpoint UX.
 *
 * Two related shapes:
 *   - EndpointMeta: vmCount + lastConnAt per endpoint, surfaced as the
 *     two metadata lines on each card in the cluster picker (SC.1).
 *   - RecentVMOpen: { id, vmName, epId, openedAt } per opened VM, used
 *     by the lobby's RECENT section (LB.5) to show last-N opens.
 *
 * Both are best-effort caches — stale data is acceptable, missing data
 * means "first time" rather than an error. All operations no-op when
 * localStorage is unavailable (private browsing, sandbox restrictions).
 */

const ENDPOINT_META_KEY = 'ahv-endpoint-meta';
const RECENT_OPENS_KEY = 'ahv-recent-vm-opens';
const NODE_POSITIONS_KEY = 'ahv-cluster-node-positions';
const CLUSTER_LINKS_KEY = 'ahv-cluster-links';
const MAX_RECENT_OPENS = 6;

export interface EndpointMeta {
  /** Cluster VM count at last successful resource fetch. */
  vmCount: number;
  /** ISO timestamp of the last successful connection to this endpoint. */
  lastConnAt: string | null;
  /**
   * Coarse link-quality hint from the LAST console session on this endpoint
   * (the RFB client's hysteresis-stable tier; mirrors QualityLevel in
   * rfb/client). Used to SEED connection #1's pixel format so a chronically
   * slow-but-stable WAN endpoint opens at 16bpp instead of paying 32bpp for the
   * whole session (no re-dial occurs to trigger the adaptive switch). Only the
   * 'low' value is acted on; a recovered endpoint persists 'high'/'medium',
   * which the seed ignores → it re-opens 32bpp.
   */
  lastTier?: 'high' | 'medium' | 'low';
}

export interface RecentVMOpen {
  id: string;
  vmName: string;
  epId: string;
  /** ISO timestamp of when the VM was opened. */
  openedAt: string;
}

type EndpointMetaMap = Record<string, EndpointMeta>;

function readMetaMap(): EndpointMetaMap {
  try {
    return JSON.parse(localStorage.getItem(ENDPOINT_META_KEY) ?? '{}') as EndpointMetaMap;
  } catch {
    return {};
  }
}

function writeMetaMap(map: EndpointMetaMap): void {
  try {
    localStorage.setItem(ENDPOINT_META_KEY, JSON.stringify(map));
  } catch {
    // Quota / private-mode — best-effort.
  }
}

export function getEndpointMeta(epId: string): EndpointMeta | null {
  const m = readMetaMap()[epId];
  return m ?? null;
}

export function setEndpointMeta(epId: string, meta: Partial<EndpointMeta>): void {
  const map = readMetaMap();
  const prev = map[epId] ?? { vmCount: 0, lastConnAt: null };
  map[epId] = { ...prev, ...meta };
  writeMetaMap(map);
}

export function recordEndpointConnection(epId: string): void {
  setEndpointMeta(epId, { lastConnAt: new Date().toISOString() });
}

export function getRecentVMOpens(): RecentVMOpen[] {
  try {
    return JSON.parse(localStorage.getItem(RECENT_OPENS_KEY) ?? '[]') as RecentVMOpen[];
  } catch {
    return [];
  }
}

export function recordVMOpen(id: string, vmName: string, epId: string): void {
  const prev = getRecentVMOpens().filter((r) => r.id !== id);
  prev.unshift({ id, vmName, epId, openedAt: new Date().toISOString() });
  if (prev.length > MAX_RECENT_OPENS) prev.length = MAX_RECENT_OPENS;
  try {
    localStorage.setItem(RECENT_OPENS_KEY, JSON.stringify(prev));
  } catch {
    // best-effort
  }
}

// ---- Cluster Map view-state (positions + operator-drawn links) ----
//
// Both are view-local, per-user (localStorage), keyed by endpoint id. They are
// additive UI state the backend doesn't model: node layout is pure
// presentation, and links are operator-asserted relationships (the data layer
// exposes no real replication/DR topology). Best-effort like the rest of this
// store — missing data means "use the auto-layout / no links".

export interface NodePosition {
  x: number;
  y: number;
}

export interface ClusterLink {
  id: string;
  aEndpointId: string;
  bEndpointId: string;
  /** Free-text relationship the operator typed (e.g. "Replicates to", "DR pair"). */
  label: string;
  /** Always 'user' for now — the backend exposes no system topology. */
  source: 'user';
}

export function getNodePositions(): Record<string, NodePosition> {
  try {
    return JSON.parse(localStorage.getItem(NODE_POSITIONS_KEY) ?? '{}') as Record<
      string,
      NodePosition
    >;
  } catch {
    return {};
  }
}

/** Persist the full position map (callers debounce drag updates). */
export function setNodePositions(map: Record<string, NodePosition>): void {
  try {
    localStorage.setItem(NODE_POSITIONS_KEY, JSON.stringify(map));
  } catch {
    // quota / private-mode — best-effort
  }
}

export function getClusterLinks(): ClusterLink[] {
  try {
    const raw = JSON.parse(localStorage.getItem(CLUSTER_LINKS_KEY) ?? '[]') as ClusterLink[];
    if (!Array.isArray(raw)) return [];
    // Migrate links saved under the old fixed `type` enum to the free-text label.
    return raw.map((l) => {
      const legacy = (l as ClusterLink & { type?: string }).type;
      return l.label ? l : { ...l, label: legacy ?? 'Link' };
    });
  } catch {
    return [];
  }
}

function writeClusterLinks(links: ClusterLink[]): void {
  try {
    localStorage.setItem(CLUSTER_LINKS_KEY, JSON.stringify(links));
  } catch {
    // best-effort
  }
}

/**
 * Add an operator-drawn link between two endpoints with a free-text label.
 * De-duplicates by unordered pair (A–B === B–A); an existing pair's label is
 * updated in place. Returns the new link list.
 */
export function addClusterLink(
  aEndpointId: string,
  bEndpointId: string,
  label: string,
): ClusterLink[] {
  if (aEndpointId === bEndpointId) return getClusterLinks();
  const text = label.trim() || 'Link';
  const links = getClusterLinks();
  const samePair = (l: ClusterLink) =>
    (l.aEndpointId === aEndpointId && l.bEndpointId === bEndpointId) ||
    (l.aEndpointId === bEndpointId && l.bEndpointId === aEndpointId);
  const existing = links.find(samePair);
  if (existing) {
    existing.label = text;
  } else {
    links.push({ id: crypto.randomUUID(), aEndpointId, bEndpointId, label: text, source: 'user' });
  }
  writeClusterLinks(links);
  return links;
}

/** Remove a link by id. Returns the new link list. */
export function removeClusterLink(id: string): ClusterLink[] {
  const links = getClusterLinks().filter((l) => l.id !== id);
  writeClusterLinks(links);
  return links;
}

/**
 * Drop links that reference an endpoint that no longer exists (e.g. after a
 * connection is deleted), so the map doesn't render dangling edges. Returns the
 * pruned list.
 */
export function pruneClusterLinks(validEndpointIds: Set<string>): ClusterLink[] {
  const links = getClusterLinks().filter(
    (l) => validEndpointIds.has(l.aEndpointId) && validEndpointIds.has(l.bEndpointId),
  );
  writeClusterLinks(links);
  return links;
}

/**
 * Human-friendly relative time: "just now", "4s ago", "12m ago",
 * "3h ago", "2d ago". Used for both the endpoint card's "last conn"
 * line and the lobby's "updated Ns ago" timestamp.
 */
export function relativeTime(iso: string | null, now: number = Date.now()): string {
  if (!iso) return 'never';
  const t = Date.parse(iso);
  if (!Number.isFinite(t)) return 'never';
  const diffSec = Math.max(0, Math.floor((now - t) / 1000));
  if (diffSec < 5) return 'just now';
  if (diffSec < 60) return `${diffSec}s ago`;
  const min = Math.floor(diffSec / 60);
  if (min < 60) return `${min}m ago`;
  const hr = Math.floor(min / 60);
  if (hr < 24) return `${hr}h ago`;
  const d = Math.floor(hr / 24);
  return `${d}d ago`;
}
