import { useCallback, useEffect, useMemo, useRef, useState } from 'react';
import type { Endpoint, EndpointHealth } from '@ahv-remote-console/protocol';
import { Icon } from './Icon';
import { hostOS } from '../input/HostOS';
import { getEndpointMeta, type ClusterLink, type NodePosition } from '../app/recentStore';
import { Burst, BrandDecor, BrandToolbar } from './clusterShared';
import {
  T,
  noDrag,
  statusMeta,
  hostPort,
  groupByCustomer,
  UNGROUPED,
  type DotState,
} from './clusterTokens';
import { NODE_W, NODE_H, MIN_ZOOM, MAX_ZOOM } from './clustermap/constants';
import { linkHealthMeta, type LinkHealth } from './clustermap/linkHealth';
import { useMapViewport } from './clustermap/useMapViewport';
import { NodeInspect } from './clustermap/NodeInspect';
import { EdgeInspect } from './clustermap/EdgeInspect';
import { InspectorPanel } from './clustermap/InspectorPanel';

// Optional quick-fill suggestions for the link label — the field is free text,
// these are just one-click conveniences for common relationships.
const LINK_SUGGESTIONS = ['Replication', 'DR pair', 'Backup'];

// Anchor an edge endpoint on a node's BORDER (not its centre), pushed out by a
// small gap, so the connection line and its flowing pulse stop at the box edge
// instead of running through the node and rendering over its contents. `from`
// and `to` are node centres; returns the exit point on `from`'s rectangle
// facing `to`.
function edgeAnchor(
  from: { x: number; y: number },
  to: { x: number; y: number },
): { x: number; y: number } {
  const GAP = 8;
  const dx = to.x - from.x;
  const dy = to.y - from.y;
  const len = Math.hypot(dx, dy);
  if (len === 0) return { x: from.x, y: from.y };
  // Scale to the nearer rectangle edge (vertical vs horizontal), then add GAP.
  const tx = dx !== 0 ? NODE_W / 2 / Math.abs(dx) : Infinity;
  const ty = dy !== 0 ? NODE_H / 2 / Math.abs(dy) : Infinity;
  const t = Math.min(tx, ty);
  return {
    x: from.x + dx * t + (dx / len) * GAP,
    y: from.y + dy * t + (dy / len) * GAP,
  };
}

export interface ClusterMapProps {
  endpoints: Endpoint[];
  health: Record<string, EndpointHealth>;
  probing: Record<string, boolean>;
  /** Any reachability probe in flight — spins the status-bar burst. */
  syncing: boolean;
  positions: Record<string, NodePosition>;
  links: ClusterLink[];
  selectedId: string | null;
  onSelectId: (id: string | null) => void;
  onConnect: (id: string) => void;
  onAddNew: () => void;
  onEdit: (id: string) => void;
  onDiagnostics: (id: string) => void;
  /** Delete this endpoint (host confirms + calls deleteEndpoint). */
  onDelete: (id: string) => void;
  /** Re-probe one endpoint (Retry). */
  onProbe: (id: string) => void;
  /** Persist a node's new position (called on drag end). */
  onMoveNode: (id: string, pos: NodePosition) => void;
  onAddLink: (a: string, b: string, label: string) => void;
  onRemoveLink: (id: string) => void;
  onSetView: (v: 'map' | 'list') => void;
  version?: string;
}

export function ClusterMap(props: ClusterMapProps) {
  const {
    endpoints,
    health,
    probing,
    syncing,
    positions,
    links,
    selectedId,
    onSelectId,
    onConnect,
    onAddNew,
    onEdit,
    onDiagnostics,
    onDelete,
    onProbe,
    onMoveNode,
    onAddLink,
    onRemoveLink,
    onSetView,
    version,
  } = props;

  // Default layout for endpoints without a saved position: one block per
  // customer (environments flow left-to-right, 4 per row), customer blocks
  // stacked vertically with a gap for the frame label — so the grouping is
  // visible before any manual arrangement.
  const defaults = useMemo(() => {
    const m: Record<string, NodePosition> = {};
    const COL = 260;
    const ROW = 190;
    const PER_ROW = 4;
    const BLOCK_GAP = 72;
    let y = 132;
    for (const g of groupByCustomer(endpoints)) {
      g.endpoints.forEach((ep, i) => {
        m[ep.id] = { x: 120 + (i % PER_ROW) * COL, y: y + Math.floor(i / PER_ROW) * ROW };
      });
      const rows = Math.max(1, Math.ceil(g.endpoints.length / PER_ROW));
      y += rows * ROW + BLOCK_GAP;
    }
    return m;
  }, [endpoints]);

  // Live positions (seeded from saved + defaults; mutated during a drag).
  const [pos, setPos] = useState<Record<string, NodePosition>>({});
  useEffect(() => {
    setPos((prev) => {
      const next: Record<string, NodePosition> = {};
      for (const ep of endpoints) next[ep.id] = prev[ep.id] ?? positions[ep.id] ?? defaults[ep.id]!;
      return next;
    });
  }, [endpoints, positions, defaults]);

  const center = useCallback(
    (id: string) => {
      const p = pos[id] ?? positions[id] ?? defaults[id] ?? { x: 0, y: 0 };
      return { x: p.x + NODE_W / 2, y: p.y + NODE_H / 2 };
    },
    [pos, positions, defaults],
  );

  // Labeled bounding frames, one per customer — drawn behind the nodes so the
  // grouping reads at a glance ("Customer A has PROD/DR/STAGING"). Computed from
  // live positions, so they follow nodes as they're dragged. Skipped entirely
  // when nothing is grouped (a single Ungrouped bucket).
  const customerFrames = useMemo(() => {
    const groups = groupByCustomer(endpoints);
    if (groups.length <= 1 && groups[0]?.customer === UNGROUPED) return [];
    const PAD = 24;
    const LABEL_H = 22;
    return groups.flatMap((g) => {
      let minX = Infinity;
      let minY = Infinity;
      let maxX = -Infinity;
      let maxY = -Infinity;
      for (const ep of g.endpoints) {
        const p = pos[ep.id] ?? positions[ep.id] ?? defaults[ep.id];
        if (!p) continue;
        minX = Math.min(minX, p.x);
        minY = Math.min(minY, p.y);
        maxX = Math.max(maxX, p.x + NODE_W);
        maxY = Math.max(maxY, p.y + NODE_H);
      }
      if (!Number.isFinite(minX)) return [];
      return [
        {
          customer: g.customer,
          count: g.endpoints.length,
          x: minX - PAD,
          y: minY - PAD - LABEL_H,
          w: maxX - minX + PAD * 2,
          h: maxY - minY + PAD * 2 + LABEL_H,
        },
      ];
    });
  }, [endpoints, pos, positions, defaults]);

  // ── viewport pan/zoom ─────────────────────────────────────────────────────
  // State + fit/wheel/world-projection moved verbatim into useMapViewport
  // (FE-06). The hook is invoked here, at the exact point this block lived,
  // so the React hook-call order across the component is unchanged.
  const { viewportRef, view, setView, fit, onWheel, toWorld } = useMapViewport(
    endpoints,
    pos,
    defaults,
  );

  // ── gestures: pan / node-drag / link-draft ────────────────────────────────
  const gesture = useRef<
    | { kind: 'pan'; sx: number; sy: number; ox: number; oy: number }
    | { kind: 'node'; id: string; dx: number; dy: number; moved: boolean }
    | null
  >(null);
  // Link creation is a click-to-start → move → click-to-finish flow (no
  // button-hold): `linking` holds the source node while the operator moves to a
  // target and clicks it. `linkPreview` is the elastic curve's free end.
  const [linking, setLinking] = useState<{ from: string } | null>(null);
  const [linkPreview, setLinkPreview] = useState<{ x: number; y: number } | null>(null);
  const [linkMenu, setLinkMenu] = useState<{ a: string; b: string; sx: number; sy: number } | null>(
    null,
  );
  const [linkLabel, setLinkLabel] = useState('');

  const cancelLinking = useCallback(() => {
    setLinking(null);
    setLinkPreview(null);
  }, []);
  const [selectedEdgeId, setSelectedEdgeId] = useState<string | null>(null);
  const [hoverId, setHoverId] = useState<string | null>(null);

  const beginPan = (e: React.PointerEvent) => {
    if (linking) {
      cancelLinking();
      return;
    }
    setLinkMenu(null);
    setSelectedEdgeId(null);
    onSelectId(null);
    gesture.current = { kind: 'pan', sx: e.clientX, sy: e.clientY, ox: view.x, oy: view.y };
    (e.currentTarget as HTMLElement).setPointerCapture?.(e.pointerId);
  };

  const beginNodeDrag = (e: React.PointerEvent, id: string) => {
    e.stopPropagation();
    // While linking, a node press must not start a drag — its click finalizes.
    if (linking) return;
    const w = toWorld(e.clientX, e.clientY);
    const p = pos[id] ?? defaults[id]!;
    gesture.current = { kind: 'node', id, dx: w.x - p.x, dy: w.y - p.y, moved: false };
    (e.currentTarget as HTMLElement).setPointerCapture?.(e.pointerId);
  };

  // Click a node: while linking, finalize the link (open the relation menu);
  // otherwise select it. Drag-vs-click is disambiguated by gesture.moved.
  const onNodeClick = (e: React.MouseEvent, id: string) => {
    if (linking) {
      e.stopPropagation();
      if (linking.from !== id) {
        setLinkLabel('');
        setLinkMenu({ a: linking.from, b: id, sx: e.clientX, sy: e.clientY });
      }
      cancelLinking();
    }
  };

  const onPointerMove = (e: React.PointerEvent) => {
    // The elastic preview follows the cursor with no button held.
    if (linking) setLinkPreview(toWorld(e.clientX, e.clientY));
    const g = gesture.current;
    if (!g) return;
    if (g.kind === 'pan') {
      setView((v) => ({ ...v, x: g.ox + (e.clientX - g.sx), y: g.oy + (e.clientY - g.sy) }));
    } else if (g.kind === 'node') {
      const w = toWorld(e.clientX, e.clientY);
      g.moved = true;
      setPos((prev) => ({
        ...prev,
        [g.id]: { x: Math.round(w.x - g.dx), y: Math.round(w.y - g.dy) },
      }));
    }
  };

  const endGesture = (_e: React.PointerEvent) => {
    const g = gesture.current;
    gesture.current = null;
    if (g?.kind !== 'node') return;
    if (g.moved) {
      const p = pos[g.id];
      if (p) onMoveNode(g.id, p);
    } else if (!linking) {
      onSelectId(g.id);
      setSelectedEdgeId(null);
    }
  };

  const startLinking = (e: React.PointerEvent | React.MouseEvent, id: string) => {
    e.stopPropagation();
    setSelectedEdgeId(null);
    setLinkMenu(null);
    setLinking({ from: id });
    setLinkPreview(center(id));
  };

  // ── derived ───────────────────────────────────────────────────────────────
  const dotState = (id: string): DotState =>
    probing[id] && !health[id] ? 'checking' : (health[id]?.status ?? 'checking');

  const linkHealth = useCallback(
    (l: ClusterLink): LinkHealth => {
      const a = health[l.aEndpointId]?.status;
      const b = health[l.bEndpointId]?.status;
      if (a === 'offline' || b === 'offline') return 'down';
      if (!a || !b) return 'degraded';
      return 'ok';
    },
    [health],
  );

  const selected = endpoints.find((e) => e.id === selectedId) ?? null;
  const selectedEdge = links.find((l) => l.id === selectedEdgeId) ?? null;
  const peersOf = (id: string) => links.filter((l) => l.aEndpointId === id || l.bEndpointId === id);

  // ── keyboard ───────────────────────────────────────────────────────────────
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      // Cmd/Ctrl+F focuses cluster search (the platform "Find" convention).
      if ((e.metaKey || e.ctrlKey) && !e.altKey && !e.shiftKey && e.key.toLowerCase() === 'f') {
        e.preventDefault();
        searchRef.current?.focus();
        return;
      }
      const ae = document.activeElement as HTMLElement | null;
      if (ae && (ae.tagName === 'INPUT' || ae.tagName === 'TEXTAREA' || ae.isContentEditable))
        return;
      if (e.key === 'Escape') {
        if (linking) {
          cancelLinking();
        } else {
          onSelectId(null);
          setSelectedEdgeId(null);
          setLinkMenu(null);
        }
      } else if (e.key === 'Enter' && selectedId) {
        onConnect(selectedId);
      } else if (
        selectedId &&
        ['ArrowUp', 'ArrowDown', 'ArrowLeft', 'ArrowRight'].includes(e.key)
      ) {
        e.preventDefault();
        const step = e.shiftKey ? 10 : 1;
        const p = pos[selectedId] ?? defaults[selectedId]!;
        const np = {
          x: p.x + (e.key === 'ArrowRight' ? step : e.key === 'ArrowLeft' ? -step : 0),
          y: p.y + (e.key === 'ArrowDown' ? step : e.key === 'ArrowUp' ? -step : 0),
        };
        setPos((prev) => ({ ...prev, [selectedId]: np }));
        onMoveNode(selectedId, np);
      }
    };
    window.addEventListener('keydown', onKey);
    return () => window.removeEventListener('keydown', onKey);
  }, [selectedId, pos, defaults, onConnect, onSelectId, onMoveNode, linking, cancelLinking]);

  // ── search ───────────────────────────────────────────────────────────────
  const [query, setQuery] = useState('');
  const searchRef = useRef<HTMLInputElement>(null);
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      // UX-03: '/' focuses cluster search (Vim/GitHub-style). Cmd+K/Ctrl+K is
      // reserved app-wide for the command palette (see App.tsx), so the map's
      // search no longer steals it. The action is unchanged — still focuses
      // the search input; only the trigger key moved.
      if (e.key !== '/' || e.metaKey || e.ctrlKey || e.altKey) return;
      // Don't hijack '/' while the operator is typing into a field (including
      // the search box itself) — let it land as a literal character.
      const ae = document.activeElement as HTMLElement | null;
      if (ae && (ae.tagName === 'INPUT' || ae.tagName === 'TEXTAREA' || ae.isContentEditable))
        return;
      e.preventDefault();
      searchRef.current?.focus();
    };
    window.addEventListener('keydown', onKey);
    return () => window.removeEventListener('keydown', onKey);
  }, []);
  const q = query.trim().toLowerCase();
  const matches = (ep: Endpoint) =>
    !q || ep.label.toLowerCase().includes(q) || ep.url.toLowerCase().includes(q);
  const locate = () => {
    const hit = endpoints.find(matches);
    if (!hit) return;
    onSelectId(hit.id);
    const c = center(hit.id);
    const vp = viewportRef.current;
    if (vp)
      setView((v) => ({
        ...v,
        x: vp.clientWidth / 2 - c.x * v.zoom,
        y: vp.clientHeight / 2 - c.y * v.zoom,
      }));
  };

  const tally = { healthy: 0, offline: 0, unknown: 0 };
  for (const ep of endpoints) {
    const st = health[ep.id]?.status;
    if (st === 'healthy') tally.healthy++;
    else if (st === 'offline') tally.offline++;
    else tally.unknown++;
  }

  const prefersReduced =
    typeof window !== 'undefined' &&
    window.matchMedia?.('(prefers-reduced-motion: reduce)').matches;

  return (
    <div
      style={{
        height: '100%',
        display: 'flex',
        flexDirection: 'column',
        background: T.bg,
        overflow: 'hidden',
      }}
    >
      <BrandToolbar
        view="map"
        onSetView={onSetView}
        onAddNew={onAddNew}
        middle={
          <label
            style={{
              ...noDrag,
              display: 'flex',
              alignItems: 'center',
              gap: 8,
              background: T.bg,
              border: `1px solid ${T.line2}`,
              borderRadius: 8,
              padding: '0 10px',
              height: 32,
              width: 240,
            }}
          >
            <Icon name="search" size={14} style={{ color: T.ink3, flexShrink: 0 }} />
            <input
              ref={searchRef}
              value={query}
              onChange={(e) => setQuery(e.target.value)}
              onKeyDown={(e) => {
                if (e.key === 'Enter') locate();
              }}
              placeholder="Search clusters"
              aria-label="Search clusters"
              style={{
                border: 'none',
                outline: 'none',
                background: 'transparent',
                flex: 1,
                minWidth: 0,
                fontFamily: T.sans,
                fontSize: 13,
                color: T.ink,
              }}
            />
            <kbd
              style={{
                fontFamily: T.mono,
                fontSize: 10,
                color: T.ink3,
                border: `1px solid ${T.line2}`,
                borderRadius: 4,
                padding: '1px 5px',
              }}
            >
              {hostOS() === 'macos' ? '⌘F' : 'Ctrl+F'}
            </kbd>
          </label>
        }
      />

      {/* Canvas + inspect */}
      <div style={{ flex: 1, display: 'flex', minHeight: 0 }}>
        <div
          ref={viewportRef}
          className="cm-bg"
          onPointerDown={(e) => {
            if (e.target === e.currentTarget) beginPan(e);
          }}
          onPointerMove={onPointerMove}
          onPointerUp={endGesture}
          onWheel={onWheel}
          style={{
            position: 'relative',
            flex: 1,
            minWidth: 0,
            overflow: 'hidden',
            background: T.canvas,
            backgroundImage: 'radial-gradient(#dce6f0 1px, transparent 1.6px)',
            backgroundSize: '34px 34px',
            cursor: linking
              ? 'crosshair'
              : gesture.current?.kind === 'pan'
                ? 'grabbing'
                : 'default',
          }}
        >
          {/* Brand decoration — same subtle background layer as the
              connection list (EndpointPicker), so the map canvas doesn't feel
              empty. Fixed to the viewport (does not pan/zoom), behind nodes. */}
          <BrandDecor animated />
          {/* world layer */}
          <div
            style={{
              position: 'absolute',
              inset: 0,
              transform: `translate(${view.x}px, ${view.y}px) scale(${view.zoom})`,
              transformOrigin: '0 0',
              pointerEvents: 'none',
            }}
          >
            {/* customer frames (behind edges + nodes) */}
            {customerFrames.map((f) => (
              <div
                key={f.customer}
                aria-hidden
                style={{
                  position: 'absolute',
                  left: f.x,
                  top: f.y,
                  width: f.w,
                  height: f.h,
                  border: `1px solid ${T.line2}`,
                  borderRadius: 16,
                  background: 'rgba(120, 140, 170, 0.045)',
                  pointerEvents: 'none',
                }}
              >
                <span
                  style={{
                    position: 'absolute',
                    top: 6,
                    left: 16,
                    display: 'flex',
                    alignItems: 'center',
                    gap: 6,
                    fontFamily: T.mono,
                    fontSize: 11,
                    fontWeight: 700,
                    letterSpacing: 0.5,
                    textTransform: 'uppercase',
                    color: T.ink3,
                  }}
                >
                  {f.customer}
                  <span style={{ color: T.ink4, fontWeight: 500 }}>· {f.count}</span>
                </span>
              </div>
            ))}
            {/* edges (behind nodes) */}
            <svg
              width={6000}
              height={6000}
              style={{
                position: 'absolute',
                top: 0,
                left: 0,
                overflow: 'visible',
                pointerEvents: 'none',
              }}
            >
              {links.map((l) => {
                const ca = center(l.aEndpointId);
                const cb = center(l.bEndpointId);
                if (!ca || !cb) return null;
                // Anchor to the node borders (not centres) so the line + its
                // flowing pulse stop at the box edge, never run through the node.
                const a = edgeAnchor(ca, cb);
                const b = edgeAnchor(cb, ca);
                const lh = linkHealth(l);
                const meta = linkHealthMeta(lh);
                const dx = b.x - a.x;
                const path = `M ${a.x} ${a.y} C ${a.x + dx / 2} ${a.y}, ${b.x - dx / 2} ${b.y}, ${b.x} ${b.y}`;
                const sel = selectedEdgeId === l.id;
                // Every connection flows — it is a live link, not a static
                // diagram line. Health is conveyed by colour + speed + opacity,
                // never by going static: 'down' just flows slower and dimmer.
                const flowClass = lh === 'ok' ? 'ec-flow-ok' : 'ec-flow-deg';
                const flowDur = lh === 'ok' ? '2s' : '3.4s';
                return (
                  <g
                    key={l.id}
                    style={{ pointerEvents: 'auto', cursor: 'pointer' }}
                    onPointerDown={(e) => {
                      e.stopPropagation();
                      setSelectedEdgeId(l.id);
                      onSelectId(null);
                    }}
                  >
                    {/* fat invisible hit area */}
                    <path
                      id={`cm-edge-${l.id}`}
                      d={path}
                      fill="none"
                      stroke="transparent"
                      strokeWidth={16}
                    />
                    <path
                      d={path}
                      fill="none"
                      stroke={meta.color}
                      strokeWidth={sel ? 3 : 2}
                      strokeLinecap="round"
                      className={flowClass}
                      opacity={lh === 'down' ? 0.7 : 1}
                    />
                    {!prefersReduced && (
                      <g opacity={lh === 'down' ? 0.7 : 1}>
                        {/* soft glow trailing the pulse */}
                        <circle r={7} fill={meta.color} opacity={0.18}>
                          <animateMotion dur={flowDur} repeatCount="indefinite">
                            <mpath href={`#cm-edge-${l.id}`} />
                          </animateMotion>
                        </circle>
                        {/* bright core pulse — reads as data flowing along the link */}
                        <circle r={3.5} fill={meta.color}>
                          <animateMotion dur={flowDur} repeatCount="indefinite">
                            <mpath href={`#cm-edge-${l.id}`} />
                          </animateMotion>
                        </circle>
                      </g>
                    )}
                  </g>
                );
              })}
              {/* elastic link preview (click-to-start → move → click target) */}
              {linking &&
                linkPreview &&
                (() => {
                  const a = center(linking.from);
                  return (
                    <path
                      d={`M ${a.x} ${a.y} L ${linkPreview.x} ${linkPreview.y}`}
                      fill="none"
                      stroke={T.blue}
                      strokeWidth={2}
                      strokeLinecap="round"
                      className="ec-flow-draft"
                    />
                  );
                })()}
            </svg>

            {/* edge labels */}
            {links.map((l) => {
              const a = center(l.aEndpointId);
              const b = center(l.bEndpointId);
              if (!a || !b) return null;
              const mx = (a.x + b.x) / 2;
              const my = (a.y + b.y) / 2;
              const meta = linkHealthMeta(linkHealth(l));
              return (
                <div
                  key={`lbl-${l.id}`}
                  onPointerDown={(e) => {
                    e.stopPropagation();
                    setSelectedEdgeId(l.id);
                    onSelectId(null);
                  }}
                  style={{
                    position: 'absolute',
                    left: mx,
                    top: my,
                    transform: 'translate(-50%,-50%)',
                    pointerEvents: 'auto',
                    cursor: 'pointer',
                    display: 'inline-flex',
                    alignItems: 'center',
                    gap: 5,
                    background: T.bg,
                    border: `1px solid ${selectedEdgeId === l.id ? T.blue : T.line}`,
                    borderRadius: 999,
                    padding: '2px 8px',
                    fontFamily: T.mono,
                    fontSize: 10,
                    color: T.ink2,
                    whiteSpace: 'nowrap',
                  }}
                >
                  <span
                    style={{
                      width: 6,
                      height: 6,
                      borderRadius: '50%',
                      background: meta.color,
                      display: 'inline-block',
                    }}
                  />
                  {l.label}
                </div>
              );
            })}

            {/* nodes */}
            {endpoints.map((ep) => {
              const p = pos[ep.id] ?? defaults[ep.id]!;
              const st = dotState(ep.id);
              const meta = statusMeta(st);
              const isSel = ep.id === selectedId;
              const dim = q ? !matches(ep) : false;
              const offline = st === 'offline';
              const rtt = health[ep.id]?.rtt_ms;
              const vmCount = getEndpointMeta(ep.id)?.vmCount;
              const metaLine = [vmCount ? `${vmCount} VMs` : null, rtt != null ? `${rtt}ms` : null]
                .filter(Boolean)
                .join(' · ');
              return (
                <div
                  key={ep.id}
                  className="ec-focus"
                  role="button"
                  tabIndex={0}
                  aria-label={`${ep.label} — ${meta.label}`}
                  aria-current={isSel}
                  onPointerDown={(e) => beginNodeDrag(e, ep.id)}
                  onClick={(e) => onNodeClick(e, ep.id)}
                  onPointerEnter={() => setHoverId(ep.id)}
                  onPointerLeave={() => setHoverId((h) => (h === ep.id ? null : h))}
                  onKeyDown={(e) => {
                    if (e.key === 'Enter') {
                      e.preventDefault();
                      onConnect(ep.id);
                    }
                  }}
                  style={{
                    position: 'absolute',
                    left: p.x,
                    top: p.y,
                    width: NODE_W,
                    height: NODE_H,
                    boxSizing: 'border-box',
                    display: 'flex',
                    alignItems: 'center',
                    gap: 8,
                    padding: '0 12px',
                    background: T.bg,
                    // While linking, every OTHER node is a valid drop target →
                    // highlight it; the source node is dimmed slightly.
                    border: `${isSel ? 2 : 1.5}px solid ${
                      linking && linking.from !== ep.id ? T.blue : isSel ? T.blue : meta.color
                    }`,
                    borderRadius: 12,
                    boxShadow:
                      linking && linking.from !== ep.id
                        ? `0 0 0 4px ${T.blueTint}`
                        : isSel
                          ? `0 0 0 4px ${T.blueTint}`
                          : '0 1px 3px rgba(15,34,53,.08)',
                    opacity: dim ? 0.35 : linking?.from === ep.id ? 0.6 : offline ? 0.7 : 1,
                    pointerEvents: 'auto',
                    cursor: linking ? (linking.from === ep.id ? 'default' : 'pointer') : 'grab',
                    userSelect: 'none',
                  }}
                >
                  {/* status dot (top-left) — beats + emits a ring so every node reads as live */}
                  <span
                    role="img"
                    aria-label={`Status: ${meta.label}`}
                    className="ec-dot-live"
                    style={{
                      position: 'absolute',
                      top: 8,
                      left: 8,
                      width: 8,
                      height: 8,
                      borderRadius: '50%',
                      background: meta.color,
                      color: meta.color,
                    }}
                  />
                  <Burst size={22} color={offline ? T.ink4 : meta.color} spin={st === 'checking'} />
                  <div style={{ flex: 1, minWidth: 0 }}>
                    <div
                      style={{
                        fontFamily: T.sans,
                        fontWeight: 600,
                        fontSize: 13,
                        color: T.ink,
                        overflow: 'hidden',
                        textOverflow: 'ellipsis',
                        whiteSpace: 'nowrap',
                      }}
                    >
                      {ep.label}
                    </div>
                    <div
                      style={{
                        fontFamily: T.mono,
                        fontSize: 10,
                        color: T.ink3,
                        overflow: 'hidden',
                        textOverflow: 'ellipsis',
                        whiteSpace: 'nowrap',
                      }}
                    >
                      {metaLine || hostPort(ep.url)}
                    </div>
                  </div>
                  {/* "+" link affordance: appears on hover, click to start a
                      link, then click the target cluster. Hidden while linking. */}
                  {!linking && (hoverId === ep.id || isSel) && (
                    <button
                      className="ec-focus"
                      aria-label={`Link ${ep.label} to another cluster`}
                      title="Link to another cluster"
                      onPointerDown={(e) => e.stopPropagation()}
                      onClick={(e) => startLinking(e, ep.id)}
                      style={{
                        position: 'absolute',
                        right: -10,
                        top: NODE_H / 2 - 10,
                        width: 20,
                        height: 20,
                        borderRadius: '50%',
                        background: T.blue,
                        color: '#fff',
                        border: `2px solid ${T.bg}`,
                        display: 'flex',
                        alignItems: 'center',
                        justifyContent: 'center',
                        fontSize: 14,
                        lineHeight: 1,
                        cursor: 'pointer',
                        boxShadow: '0 1px 3px rgba(15,34,53,.25)',
                      }}
                    >
                      +
                    </button>
                  )}
                </div>
              );
            })}
          </div>

          {/* linking hint */}
          {linking && (
            <div
              style={{
                position: 'absolute',
                top: 12,
                left: '50%',
                transform: 'translateX(-50%)',
                background: T.ink,
                color: '#fff',
                fontFamily: T.sans,
                fontSize: 12,
                padding: '6px 12px',
                borderRadius: 999,
                boxShadow: '0 2px 8px rgba(15,34,53,.25)',
                pointerEvents: 'none',
              }}
            >
              Click another cluster to link · Esc to cancel
            </div>
          )}

          {/* zoom controls */}
          <div
            style={{
              position: 'absolute',
              left: 12,
              bottom: 12,
              display: 'flex',
              flexDirection: 'column',
              gap: 6,
            }}
          >
            <MapBtn
              onClick={() => setView((v) => ({ ...v, zoom: Math.min(MAX_ZOOM, v.zoom * 1.2) }))}
              label="Zoom in"
            >
              ＋
            </MapBtn>
            <MapBtn
              onClick={() => setView((v) => ({ ...v, zoom: Math.max(MIN_ZOOM, v.zoom / 1.2) }))}
              label="Zoom out"
            >
              －
            </MapBtn>
            <MapBtn onClick={fit} label="Fit to view">
              ⤢
            </MapBtn>
          </div>

          {/* link label entry: the operator types what the relationship is */}
          {linkMenu && (
            <form
              onSubmit={(e) => {
                e.preventDefault();
                const text = linkLabel.trim();
                if (text) onAddLink(linkMenu.a, linkMenu.b, text);
                setLinkMenu(null);
                setLinkLabel('');
              }}
              style={{
                position: 'fixed',
                left: linkMenu.sx,
                top: linkMenu.sy,
                width: 240,
                background: T.bg,
                border: `1px solid ${T.line2}`,
                borderRadius: 10,
                boxShadow: '0 8px 24px rgba(15,34,53,.18)',
                padding: 12,
                zIndex: 20,
              }}
            >
              <div style={{ fontFamily: T.sans, fontSize: 11, color: T.ink3, marginBottom: 6 }}>
                What is this link?
              </div>
              <input
                autoFocus
                value={linkLabel}
                onChange={(e) => setLinkLabel(e.target.value)}
                onKeyDown={(e) => {
                  if (e.key === 'Escape') {
                    e.stopPropagation();
                    setLinkMenu(null);
                    setLinkLabel('');
                  }
                }}
                placeholder="e.g. Replicates to, DR pair…"
                aria-label="Link description"
                style={{
                  width: '100%',
                  boxSizing: 'border-box',
                  padding: '8px 10px',
                  border: `1px solid ${T.line2}`,
                  borderRadius: 8,
                  fontFamily: T.sans,
                  fontSize: 13,
                  color: T.ink,
                  outline: 'none',
                }}
              />
              <div style={{ display: 'flex', flexWrap: 'wrap', gap: 4, marginTop: 8 }}>
                {LINK_SUGGESTIONS.map((s) => (
                  <button
                    key={s}
                    type="button"
                    className="ec-focus"
                    onClick={() => setLinkLabel(s)}
                    style={{
                      fontFamily: T.sans,
                      fontSize: 11,
                      color: T.ink2,
                      background: T.soft,
                      border: `1px solid ${T.line2}`,
                      borderRadius: 999,
                      padding: '3px 9px',
                      cursor: 'pointer',
                    }}
                  >
                    {s}
                  </button>
                ))}
              </div>
              <div style={{ display: 'flex', justifyContent: 'flex-end', gap: 8, marginTop: 10 }}>
                <button
                  type="button"
                  className="ec-focus"
                  onClick={() => {
                    setLinkMenu(null);
                    setLinkLabel('');
                  }}
                  style={{
                    fontFamily: T.sans,
                    fontSize: 12,
                    color: T.ink3,
                    background: 'transparent',
                    border: 'none',
                    cursor: 'pointer',
                  }}
                >
                  Cancel
                </button>
                <button
                  type="submit"
                  className="ec-focus"
                  disabled={!linkLabel.trim()}
                  style={{
                    fontFamily: T.sans,
                    fontSize: 12,
                    fontWeight: 600,
                    color: '#fff',
                    background: linkLabel.trim() ? T.blue : T.ink4,
                    border: 'none',
                    borderRadius: 6,
                    padding: '5px 12px',
                    cursor: linkLabel.trim() ? 'pointer' : 'not-allowed',
                  }}
                >
                  Link
                </button>
              </div>
            </form>
          )}
        </div>

        {/* inspect panel */}
        {(selected ?? selectedEdge) && (
          <InspectorPanel>
            {selectedEdge ? (
              <EdgeInspect
                link={selectedEdge}
                health={linkHealthMeta(linkHealth(selectedEdge))}
                nameOf={(id) => endpoints.find((e) => e.id === id)?.label ?? id}
                onRemove={() => {
                  onRemoveLink(selectedEdge.id);
                  setSelectedEdgeId(null);
                }}
                onClose={() => setSelectedEdgeId(null)}
              />
            ) : selected ? (
              <NodeInspect
                ep={selected}
                state={dotState(selected.id)}
                peers={peersOf(selected.id)}
                nameOf={(id) => endpoints.find((e) => e.id === id)?.label ?? id}
                linkHealth={linkHealth}
                onConnect={() => onConnect(selected.id)}
                onRetry={() => onProbe(selected.id)}
                onEdit={() => onEdit(selected.id)}
                onDiagnostics={() => onDiagnostics(selected.id)}
                onDelete={() => onDelete(selected.id)}
                onUnlink={(lid) => onRemoveLink(lid)}
                onClose={() => onSelectId(null)}
              />
            ) : null}
          </InspectorPanel>
        )}
      </div>

      {/* status bar */}
      <footer
        style={{
          height: 28,
          flexShrink: 0,
          display: 'flex',
          alignItems: 'center',
          gap: 14,
          padding: '0 14px',
          background: T.soft,
          borderTop: `1px solid ${T.line}`,
          fontFamily: T.mono,
          fontSize: 11,
          color: T.ink3,
        }}
      >
        <span style={{ display: 'flex', alignItems: 'center', gap: 12 }}>
          <Tally color={T.green} n={tally.healthy} label="healthy" />
          <span style={{ color: T.ink4 }}>·</span>
          <Tally color={T.red} n={tally.offline} label="offline" />
          {tally.unknown > 0 && (
            <>
              <span style={{ color: T.ink4 }}>·</span>
              <Tally color={T.ink4} n={tally.unknown} label="unknown" />
            </>
          )}
        </span>
        <span style={{ flex: 1 }} />
        <span style={{ display: 'flex', alignItems: 'center', gap: 6 }} aria-live="polite">
          <Burst size={13} color={syncing ? T.greenD : T.ink4} spin={syncing} />
          {syncing ? 'syncing…' : 'sync 30s'}
        </span>
        <span>{links.length} links</span>
        <span>{version ?? 'v2'}</span>
      </footer>
    </div>
  );
}

// ───────────────────────────── sub-components ───────────────────────────────

function MapBtn({
  onClick,
  label,
  children,
}: {
  onClick: () => void;
  label: string;
  children: React.ReactNode;
}) {
  return (
    <button
      className="ec-focus"
      onClick={onClick}
      aria-label={label}
      title={label}
      style={{
        width: 30,
        height: 30,
        display: 'flex',
        alignItems: 'center',
        justifyContent: 'center',
        background: T.bg,
        border: `1px solid ${T.line2}`,
        borderRadius: 8,
        color: T.ink2,
        fontSize: 15,
        cursor: 'pointer',
        boxShadow: '0 1px 3px rgba(15,34,53,.08)',
      }}
    >
      {children}
    </button>
  );
}

function Tally({ color, n, label }: { color: string; n: number; label: string }) {
  return (
    <span style={{ display: 'flex', alignItems: 'center', gap: 5 }}>
      <span
        style={{
          width: 6,
          height: 6,
          borderRadius: '50%',
          background: color,
          display: 'inline-block',
        }}
      />
      {n} {label}
    </span>
  );
}
