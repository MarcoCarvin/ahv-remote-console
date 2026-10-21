import { useEffect, useMemo, useRef, useState } from 'react';
import type { Endpoint, EndpointHealth } from '@ahv-remote-console/protocol';
import { Icon } from './Icon';
import { getEndpointMeta, relativeTime } from '../app/recentStore';
import { titleBarLeft } from '../platform';
import { hostOS } from '../input/HostOS';
import { StatusDot, Spark, SignalStage, ViewToggle, BrandDecor } from './clusterShared';
import {
  T,
  hostPort,
  groupByCustomer,
  environmentOf,
  noDrag,
  UNGROUPED,
  type DotState,
  type SignalMood,
} from './clusterTokens';

/**
 * Connection Manager — List view (the accessible peer of the Cluster Map).
 *
 * PRIVACY: this renders BEFORE the operator authenticates to the selected
 * cluster, so it shows only locally-known facts — name, host:port, reachability
 * (a TCP probe, not a login) and the locally-stored last-connected time. No VM
 * counts, latency/RTT, or session/VM names (those need an authenticated session
 * and would leak private inventory on a picker). The empty space is filled with
 * brand presence (the signal stage), not fabricated data.
 *
 * Presentation only: selection, reachability, and every action are the app's
 * existing controlled state/handlers (shared with the Map via Shell).
 */
export function EndpointPicker({
  endpoints,
  onSelect,
  onAddNew,
  onEdit,
  health = {},
  probing = {},
  onProbe,
  onDiagnostics,
  onDelete,
  selectedId: selectedIdProp,
  onSelectId,
  onSetView,
  version,
}: {
  endpoints: Endpoint[];
  onSelect: (id: string) => void;
  onAddNew: () => void;
  onEdit?: (endpointId: string) => void;
  health?: Record<string, EndpointHealth>;
  probing?: Record<string, boolean>;
  onProbe?: (endpointId: string) => void;
  onDiagnostics?: (endpointId: string) => void;
  /** Delete this endpoint (the host confirms + calls deleteEndpoint). */
  onDelete?: (endpointId: string) => void;
  /** Controlled selection (shared with the Map). Falls back to internal. */
  selectedId?: string | null;
  onSelectId?: (id: string | null) => void;
  onSetView?: (v: 'map' | 'list') => void;
  /** App/runtime version for the status bar (non-private). */
  version?: string;
}) {
  const [internalSel, setInternalSel] = useState<string | null>(null);
  const selectedId = selectedIdProp !== undefined ? selectedIdProp : internalSel;
  const setSelectedId = (id: string | null) => {
    if (onSelectId) onSelectId(id);
    else setInternalSel(id);
  };

  const [query, setQuery] = useState('');
  const searchRef = useRef<HTMLInputElement>(null);
  const visible = useMemo(() => {
    const q = query.trim().toLowerCase();
    if (!q) return endpoints;
    return endpoints.filter(
      (ep) =>
        ep.label.toLowerCase().includes(q) ||
        hostPort(ep.url).toLowerCase().includes(q) ||
        environmentOf(ep).toLowerCase().includes(q),
    );
  }, [endpoints, query]);

  const groups = useMemo(() => groupByCustomer(visible), [visible]);
  const ordered = useMemo(() => groups.flatMap((g) => g.endpoints), [groups]);
  const showHeaders = groups.length > 1 || groups[0]?.customer !== UNGROUPED;
  const [collapsed, setCollapsed] = useState<Set<string>>(new Set());
  const toggleCustomer = (c: string) =>
    setCollapsed((prev) => {
      const next = new Set(prev);
      if (next.has(c)) next.delete(c);
      else next.add(c);
      return next;
    });

  // Keep a valid selection (independent of the search filter — the detail can
  // show an endpoint that's currently filtered out of the sidebar list).
  useEffect(() => {
    const first = endpoints[0];
    if (!first) {
      if (selectedId !== null) setSelectedId(null);
      return;
    }
    if (!selectedId || !endpoints.some((e) => e.id === selectedId)) {
      setSelectedId(first.id);
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [endpoints, selectedId]);

  // Re-probe the selected endpoint on demand (the Map's poll covers the rest).
  useEffect(() => {
    if (selectedId && !health[selectedId] && !probing[selectedId]) onProbe?.(selectedId);
  }, [selectedId, health, probing, onProbe]);

  const selected = useMemo(
    () => endpoints.find((e) => e.id === selectedId) ?? null,
    [endpoints, selectedId],
  );

  const dotState = (id: string): DotState =>
    probing[id] && !health[id] ? 'checking' : (health[id]?.status ?? 'checking');

  const counts = useMemo(() => {
    let reachable = 0;
    let unreachable = 0;
    for (const ep of endpoints) {
      const st = health[ep.id]?.status;
      if (st === 'healthy') reachable++;
      else if (st === 'offline') unreachable++;
    }
    return { reachable, unreachable };
  }, [endpoints, health]);

  // Keyboard: ↑/↓ select, ↵ connect (retry if offline), ⌘F / Ctrl+F focus search.
  // Cmd/Ctrl+K stays reserved for the app-wide command palette (App.tsx).
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if ((e.metaKey || e.ctrlKey) && !e.altKey && !e.shiftKey && e.key.toLowerCase() === 'f') {
        e.preventDefault();
        searchRef.current?.focus();
        return;
      }
      const ae = document.activeElement as HTMLElement | null;
      if (ae && (ae.tagName === 'INPUT' || ae.tagName === 'TEXTAREA' || ae.isContentEditable))
        return;
      if (e.key === 'ArrowDown' || e.key === 'ArrowUp') {
        if (ordered.length === 0) return;
        e.preventDefault();
        const idx = Math.max(
          0,
          ordered.findIndex((x) => x.id === selectedId),
        );
        const next = (e.key === 'ArrowDown' ? idx + 1 : idx - 1 + ordered.length) % ordered.length;
        const target = ordered[next];
        if (target) setSelectedId(target.id);
      } else if (e.key === 'Enter' && selectedId) {
        if (dotState(selectedId) === 'offline') onProbe?.(selectedId);
        else onSelect(selectedId);
      }
    };
    window.addEventListener('keydown', onKey);
    return () => window.removeEventListener('keydown', onKey);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [ordered, selectedId, onSelect, onProbe]);

  const toolbar = (
    <header
      style={
        {
          height: 56,
          flexShrink: 0,
          display: 'flex',
          alignItems: 'center',
          gap: 14,
          padding: `0 18px 0 ${titleBarLeft(18)}px`,
          background: T.soft,
          borderBottom: `1px solid ${T.line}`,
          WebkitAppRegion: 'drag',
        } as React.CSSProperties
      }
    >
      <Spark size={20} className="ec-spark-hoverspin" />
      <span style={{ fontFamily: T.sans, fontSize: 15, fontWeight: 700, color: T.ink }}>
        AHV Remote <span style={{ color: T.blue }}>Console</span>
      </span>
      <span style={{ flex: 1 }} />
      {onSetView && <ViewToggle view="list" onSetView={onSetView} />}
      <button
        className="ec-focus"
        onClick={onAddNew}
        aria-label="Add endpoint"
        title="Add endpoint"
        style={{
          ...noDrag,
          width: 34,
          height: 34,
          display: 'grid',
          placeItems: 'center',
          border: 'none',
          borderRadius: 8,
          background: T.blue,
          color: '#fff',
          cursor: 'pointer',
        }}
      >
        <Icon name="plus" size={16} />
      </button>
    </header>
  );

  if (endpoints.length === 0) {
    return (
      <div style={{ height: '100%', display: 'flex', flexDirection: 'column', background: T.bg }}>
        {toolbar}
        <div
          style={{
            flex: 1,
            display: 'flex',
            alignItems: 'center',
            justifyContent: 'center',
            padding: 48,
          }}
        >
          <button
            className="ec-focus"
            onClick={onAddNew}
            style={{
              display: 'flex',
              alignItems: 'center',
              gap: 14,
              padding: '20px 26px',
              background: T.soft,
              border: `1px dashed ${T.line2}`,
              borderRadius: 12,
              cursor: 'pointer',
            }}
          >
            <Icon name="plus" size={20} style={{ color: T.blue }} />
            <span style={{ textAlign: 'left' }}>
              <span
                style={{
                  display: 'block',
                  fontFamily: T.sans,
                  fontSize: 15,
                  fontWeight: 600,
                  color: T.ink,
                }}
              >
                Add your first endpoint
              </span>
              <span style={{ display: 'block', fontFamily: T.sans, fontSize: 12, color: T.ink3 }}>
                Connect to a Prism Central cluster
              </span>
            </span>
          </button>
        </div>
      </div>
    );
  }

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
      {toolbar}
      <div style={{ flex: 1, display: 'flex', minHeight: 0 }}>
        {/* ── Sidebar ─────────────────────────────────────────────────── */}
        <aside
          style={{
            width: 288,
            flexShrink: 0,
            borderRight: `1px solid ${T.line}`,
            background: T.soft2,
            display: 'flex',
            flexDirection: 'column',
            minHeight: 0,
          }}
        >
          <div style={{ padding: '12px 14px 8px' }}>
            <div style={{ position: 'relative' }}>
              <Icon
                name="search"
                size={13}
                style={{
                  position: 'absolute',
                  left: 11,
                  top: '50%',
                  transform: 'translateY(-50%)',
                  color: T.ink3,
                  pointerEvents: 'none',
                }}
              />
              <input
                ref={searchRef}
                value={query}
                onChange={(e) => setQuery(e.target.value)}
                placeholder="Search endpoints"
                aria-label="Search endpoints"
                className="ec-focus"
                style={{
                  width: '100%',
                  height: 34,
                  boxSizing: 'border-box',
                  padding: '0 34px 0 30px',
                  border: `1px solid ${T.line2}`,
                  borderRadius: 8,
                  background: T.bg,
                  fontFamily: T.mono,
                  fontSize: 12,
                  color: T.ink,
                  outline: 'none',
                }}
              />
              <span
                aria-hidden
                style={{
                  position: 'absolute',
                  right: 9,
                  top: '50%',
                  transform: 'translateY(-50%)',
                  fontFamily: T.mono,
                  fontSize: 10,
                  color: T.ink4,
                  border: `1px solid ${T.line2}`,
                  borderRadius: 4,
                  padding: '1px 4px',
                  pointerEvents: 'none',
                }}
              >
                {hostOS() === 'macos' ? '⌘F' : 'Ctrl+F'}
              </span>
            </div>
          </div>
          <div style={{ flex: 1, overflowY: 'auto', minHeight: 0, padding: '2px 0 8px' }}>
            {ordered.length === 0 ? (
              <div
                style={{
                  fontFamily: T.mono,
                  fontSize: 11,
                  color: T.ink3,
                  padding: '14px 18px',
                  textAlign: 'center',
                }}
              >
                No matches.
              </div>
            ) : (
              groups.map((g) => {
                const isCollapsed = collapsed.has(g.customer);
                return (
                  <div key={g.customer}>
                    {showHeaders && (
                      <button
                        className="ec-focus"
                        onClick={() => toggleCustomer(g.customer)}
                        aria-expanded={!isCollapsed}
                        style={{
                          width: '100%',
                          display: 'flex',
                          alignItems: 'center',
                          gap: 8,
                          padding: '12px 18px 4px',
                          background: 'transparent',
                          border: 'none',
                          cursor: 'pointer',
                          textAlign: 'left',
                        }}
                      >
                        <svg
                          width="9"
                          height="9"
                          viewBox="0 0 10 10"
                          style={{
                            transform: isCollapsed ? 'none' : 'rotate(90deg)',
                            transition: 'transform 120ms',
                            color: T.ink3,
                            flexShrink: 0,
                          }}
                          aria-hidden
                        >
                          <path
                            d="M3 2 L7 5 L3 8"
                            fill="none"
                            stroke="currentColor"
                            strokeWidth="1.6"
                            strokeLinecap="round"
                            strokeLinejoin="round"
                          />
                        </svg>
                        <span
                          style={{
                            flex: 1,
                            minWidth: 0,
                            fontFamily: T.mono,
                            fontSize: 10,
                            fontWeight: 700,
                            letterSpacing: '.18em',
                            textTransform: 'uppercase',
                            color: T.ink3,
                            overflow: 'hidden',
                            textOverflow: 'ellipsis',
                            whiteSpace: 'nowrap',
                          }}
                        >
                          {g.customer === UNGROUPED ? 'Endpoints' : g.customer}
                        </span>
                        <span
                          style={{
                            fontFamily: T.mono,
                            fontSize: 10,
                            color: T.ink4,
                            background: T.bg,
                            border: `1px solid ${T.line2}`,
                            borderRadius: 8,
                            padding: '1px 7px',
                          }}
                        >
                          {g.endpoints.length}
                        </span>
                      </button>
                    )}
                    {!isCollapsed &&
                      g.endpoints.map((ep) => {
                        const primary = environmentOf(ep) || ep.label;
                        const sel = ep.id === selectedId;
                        return (
                          <div
                            key={ep.id}
                            className={`ec-row ec-focus${sel ? ' ec-row--sel' : ''}`}
                            role="button"
                            tabIndex={0}
                            aria-current={sel}
                            onClick={() => setSelectedId(ep.id)}
                            onDoubleClick={() => onSelect(ep.id)}
                            onKeyDown={(e) => {
                              if (e.key === 'Enter' || e.key === ' ') {
                                e.preventDefault();
                                setSelectedId(ep.id);
                              }
                            }}
                            style={{
                              display: 'flex',
                              alignItems: 'center',
                              gap: 11,
                              padding: showHeaders ? '10px 18px 10px 28px' : '10px 18px',
                              minHeight: 44,
                              cursor: 'pointer',
                              borderLeft: `2px solid ${sel ? T.blue : 'transparent'}`,
                            }}
                          >
                            <StatusDot state={dotState(ep.id)} />
                            <div style={{ flex: 1, minWidth: 0 }}>
                              <div
                                style={{
                                  fontFamily: T.sans,
                                  fontSize: 14,
                                  fontWeight: 600,
                                  color: T.ink,
                                  overflow: 'hidden',
                                  textOverflow: 'ellipsis',
                                  whiteSpace: 'nowrap',
                                }}
                              >
                                {primary}
                              </div>
                              <div
                                style={{
                                  fontFamily: T.mono,
                                  fontSize: 11,
                                  color: T.ink3,
                                  overflow: 'hidden',
                                  textOverflow: 'ellipsis',
                                  whiteSpace: 'nowrap',
                                }}
                              >
                                {hostPort(ep.url)}
                              </div>
                            </div>
                          </div>
                        );
                      })}
                  </div>
                );
              })
            )}
          </div>
          <button
            className="ec-focus"
            onClick={onAddNew}
            style={{
              display: 'flex',
              alignItems: 'center',
              justifyContent: 'center',
              gap: 8,
              padding: '12px 16px',
              borderTop: `1px solid ${T.line}`,
              background: 'transparent',
              color: T.blue,
              fontFamily: T.sans,
              fontSize: 13,
              fontWeight: 600,
              cursor: 'pointer',
              minHeight: 44,
            }}
          >
            <Icon name="plus" size={14} />
            New endpoint
          </button>
        </aside>

        {/* ── Main ────────────────────────────────────────────────────── */}
        <main
          style={{
            flex: 1,
            display: 'flex',
            flexDirection: 'column',
            minHeight: 0,
            background: T.bg,
          }}
        >
          {selected && (
            <EndpointMain
              ep={selected}
              state={dotState(selected.id)}
              onConnect={() => onSelect(selected.id)}
              onRetry={() => onProbe?.(selected.id)}
              onEdit={onEdit ? () => onEdit(selected.id) : undefined}
              onDiagnostics={onDiagnostics ? () => onDiagnostics(selected.id) : undefined}
              onDelete={onDelete ? () => onDelete(selected.id) : undefined}
            />
          )}
        </main>
      </div>

      {/* ── Status bar ──────────────────────────────────────────────────── */}
      <footer
        style={{
          height: 30,
          flexShrink: 0,
          display: 'flex',
          alignItems: 'center',
          gap: 16,
          padding: '0 18px',
          background: T.soft,
          borderTop: `1px solid ${T.line}`,
          fontFamily: T.mono,
          fontSize: 11,
          color: T.ink3,
        }}
      >
        <span style={{ display: 'inline-flex', alignItems: 'center', gap: 6 }}>
          <StatusDot state="healthy" size={7} />
          {counts.reachable} reachable
        </span>
        <span style={{ display: 'inline-flex', alignItems: 'center', gap: 6 }}>
          <StatusDot state={counts.unreachable > 0 ? 'offline' : 'checking'} size={7} />
          {counts.unreachable} unreachable
        </span>
        <span style={{ flex: 1 }} />
        <span>
          {endpoints.length} endpoint{endpoints.length !== 1 ? 's' : ''}
        </span>
        <span style={{ color: T.ink4 }}>·</span>
        <span>not connected</span>
        {version && (
          <>
            <span style={{ color: T.ink4 }}>·</span>
            <span>v{version}</span>
          </>
        )}
      </footer>
    </div>
  );
}

interface StateUI {
  mood: SignalMood;
  color: string;
  // Color for the small caption dot (the "NOT CONNECTED" / "OFFLINE" tag). It's
  // the CONNECTION state, distinct from `color` (reachability): a reachable-but-
  // not-connected endpoint is neutral grey, not green.
  captionColor: string;
  pill: string;
  caption: string;
  headline: string;
  subtext: string;
}

function stateUI(state: DotState, name: string, host: string): StateUI {
  if (state === 'offline') {
    return {
      mood: 'error',
      color: T.red,
      captionColor: T.red,
      pill: 'Unreachable',
      caption: 'OFFLINE',
      headline: `Can’t reach ${name}.`,
      subtext: 'Check the host or your VPN, then Retry.',
    };
  }
  if (state === 'checking') {
    return {
      mood: 'active',
      color: T.blue,
      captionColor: T.blue,
      pill: 'Checking…',
      caption: 'CHECKING',
      headline: `Checking ${name}…`,
      subtext: `Probing ${host} for reachability.`,
    };
  }
  return {
    mood: 'idle',
    color: T.green,
    // Reachable (green pill) but NOT CONNECTED — the connection tag stays neutral
    // grey so a green dot doesn't read as "online".
    captionColor: T.ink4,
    pill: 'Reachable',
    caption: 'NOT CONNECTED',
    headline: 'Ready when you are.',
    subtext: `Hit Connect to bring ${name} online.`,
  };
}

function EndpointMain({
  ep,
  state,
  onConnect,
  onRetry,
  onEdit,
  onDiagnostics,
  onDelete,
}: {
  ep: Endpoint;
  state: DotState;
  onConnect: () => void;
  onRetry: () => void;
  onEdit?: () => void;
  onDiagnostics?: () => void;
  onDelete?: () => void;
}) {
  const host = hostPort(ep.url);
  const ui = stateUI(state, ep.label, host);
  const lastConn = getEndpointMeta(ep.id)?.lastConnAt ?? null;
  const [confirmDelete, setConfirmDelete] = useState(false);
  useEffect(() => setConfirmDelete(false), [ep.id]);

  const secBtn = (label: string, onClick: () => void) => (
    <button
      className="ec-focus"
      onClick={onClick}
      style={{
        height: 42,
        padding: '0 16px',
        fontFamily: T.sans,
        fontSize: 13,
        fontWeight: 500,
        color: T.ink2,
        background: T.bg,
        border: `1px solid ${T.line2}`,
        borderRadius: 9,
        cursor: 'pointer',
      }}
    >
      {label}
    </button>
  );

  const pillBg = `color-mix(in srgb, ${ui.color} 13%, #fff)`;

  return (
    <>
      {/* SECTION 1 · Endpoint block (white) */}
      <div style={{ background: T.bg, padding: '30px 36px 26px', flexShrink: 0 }}>
        <div style={{ display: 'flex', alignItems: 'center', gap: 13 }}>
          <span
            aria-hidden
            className={state === 'offline' ? undefined : 'ec-pulse'}
            style={{
              width: 12,
              height: 12,
              borderRadius: '50%',
              background: ui.color,
              color: ui.color,
              display: 'inline-block',
              flexShrink: 0,
            }}
          />
          <h1
            style={{
              margin: 0,
              fontFamily: T.sans,
              fontWeight: 300,
              fontSize: 40,
              letterSpacing: '-0.015em',
              color: T.ink,
              lineHeight: 1.05,
              overflow: 'hidden',
              textOverflow: 'ellipsis',
              whiteSpace: 'nowrap',
            }}
          >
            {ep.label}
          </h1>
          <span
            role="status"
            style={{
              flexShrink: 0,
              fontFamily: T.sans,
              fontSize: 11,
              fontWeight: 500,
              color: ui.color,
              background: pillBg,
              border: `1px solid color-mix(in srgb, ${ui.color} 30%, #fff)`,
              borderRadius: 6,
              padding: '4px 11px',
            }}
          >
            {ui.pill}
          </span>
        </div>

        <div
          style={{
            fontFamily: T.mono,
            fontSize: 13,
            color: T.ink3,
            marginTop: 10,
            whiteSpace: 'nowrap',
            overflow: 'hidden',
            textOverflow: 'ellipsis',
          }}
        >
          {host}
          <span style={{ color: T.ink4 }}> · </span>
          {state === 'checking'
            ? 'negotiating reachability'
            : lastConn
              ? `last connected ${relativeTime(lastConn)}`
              : 'never connected'}
        </div>

        <div style={{ display: 'flex', alignItems: 'center', gap: 11, marginTop: 24 }}>
          {/* Primary: Connect (reachable/checking) or Retry (offline). */}
          {state === 'offline' ? (
            <button className="ec-focus" onClick={onRetry} style={primaryBtn}>
              Retry
            </button>
          ) : (
            <button className="ec-focus" onClick={onConnect} style={primaryBtn}>
              Connect
            </button>
          )}
          {state === 'offline' && secBtn('Connect', onConnect)}
          {onEdit && secBtn('Edit', onEdit)}
          {onDiagnostics && secBtn('Diagnostics', onDiagnostics)}
          {/* Delete sits in the cluster (not orphaned far-right) and stays quiet
              until hovered — destructive, but not a standing red flag. */}
          {onDelete &&
            (confirmDelete ? (
              <span style={{ display: 'inline-flex', alignItems: 'center', gap: 8 }}>
                <span style={{ fontFamily: T.sans, fontSize: 12.5, color: T.ink2 }}>
                  Remove “{ep.label}”?
                </span>
                <button
                  className="ec-focus"
                  onClick={onDelete}
                  style={{
                    height: 42,
                    padding: '0 14px',
                    fontFamily: T.sans,
                    fontSize: 13,
                    fontWeight: 600,
                    color: '#fff',
                    background: T.red,
                    border: 'none',
                    borderRadius: 9,
                    cursor: 'pointer',
                  }}
                >
                  Delete
                </button>
                <button
                  className="ec-focus"
                  onClick={() => setConfirmDelete(false)}
                  style={{
                    height: 42,
                    padding: '0 10px',
                    fontFamily: T.sans,
                    fontSize: 13,
                    color: T.ink3,
                    background: 'transparent',
                    border: 'none',
                    cursor: 'pointer',
                  }}
                >
                  Cancel
                </button>
              </span>
            ) : (
              <button
                className="ec-focus ec-del-ghost"
                onClick={() => setConfirmDelete(true)}
                style={{
                  height: 42,
                  padding: '0 14px',
                  fontFamily: T.sans,
                  fontSize: 13,
                  fontWeight: 500,
                  color: T.ink3,
                  background: 'transparent',
                  border: '1px solid transparent',
                  borderRadius: 9,
                  cursor: 'pointer',
                  transition: 'color 120ms ease, background 120ms ease',
                }}
                onMouseEnter={(e) => {
                  e.currentTarget.style.background = `color-mix(in srgb, ${T.red} 10%, #fff)`;
                  e.currentTarget.style.color = T.red;
                }}
                onMouseLeave={(e) => {
                  e.currentTarget.style.background = 'transparent';
                  e.currentTarget.style.color = T.ink3;
                }}
              >
                Delete
              </button>
            ))}
        </div>
      </div>

      {/* SECTION 2 · State panel (tinted, brand hero) */}
      <div
        style={{
          flex: 1,
          minHeight: 0,
          display: 'flex',
          flexDirection: 'column',
          alignItems: 'center',
          justifyContent: 'center',
          textAlign: 'center',
          position: 'relative',
          overflow: 'hidden',
          background: T.soft,
          borderTop: `1px solid ${T.line2}`,
          boxShadow: 'inset 0 1px 0 #fff, inset 0 14px 30px -26px rgba(20,50,90,.28)',
        }}
      >
        <BrandDecor bars={false} />
        <span
          style={{
            position: 'absolute',
            top: 18,
            left: 36,
            display: 'inline-flex',
            alignItems: 'center',
            gap: 7,
            fontFamily: T.mono,
            fontSize: 10,
            letterSpacing: '.18em',
            textTransform: 'uppercase',
            color: T.ink3,
            zIndex: 2,
          }}
        >
          <span style={{ width: 6, height: 6, borderRadius: '50%', background: ui.captionColor }} />
          {ui.caption}
        </span>

        <SignalStage mood={ui.mood} />

        <h2
          style={{
            margin: '6px 0 0',
            fontFamily: T.sans,
            fontWeight: 300,
            fontSize: 28,
            letterSpacing: '-0.01em',
            color: T.ink,
            whiteSpace: 'nowrap',
            zIndex: 1,
          }}
        >
          {ui.headline}
        </h2>
        <p
          style={{
            margin: '10px 0 0',
            fontFamily: T.sans,
            fontSize: 13.5,
            lineHeight: 1.5,
            color: T.ink3,
            maxWidth: 320,
            zIndex: 1,
          }}
        >
          {ui.subtext}
        </p>
      </div>
    </>
  );
}

const primaryBtn: React.CSSProperties = {
  height: 42,
  padding: '0 22px',
  fontFamily: T.sans,
  fontSize: 13,
  fontWeight: 600,
  color: '#fff',
  background: T.blue,
  border: 'none',
  borderRadius: 9,
  cursor: 'pointer',
};
