// Lobby — the VM browser / session picker. Presentation-only redesign ("brand
// spine"): a full-height blue brand spine on the left and a calm paper canvas
// on the right. All VM data + connect/refresh/favorite handlers are passed in
// from Shell.tsx; this file owns markup + styling only.
//
// Local-only extras (no runtime/API involvement): user-scoped custom folders
// (foldersStore) and a swipeable RECENT carousel. Category switching uses a
// cheap opacity crossfade instead of per-tile layout springs (which janked with
// 200+ VMs).
import { useEffect, useMemo, useRef, useState } from 'react';
import { Dialog, Field, Spinner } from '@ahv-remote-console/ui';
import type {
  Endpoint,
  EndpointCapabilities,
  Resource,
  UserIdentity,
} from '@ahv-remote-console/protocol';
import { relativeTime, type RecentVMOpen } from '../app/recentStore';
import {
  getFolders,
  createFolder,
  createFolderWithVm,
  deleteFolder,
  toggleVmInFolder,
  type VMFolder,
} from '../app/foldersStore';
import { Spark } from './clusterShared';
import { T, environmentOf, noDrag, userInitials } from './clusterTokens';
import { Icon, type IconName } from './Icon';
import {
  lobbyPowerGate,
  powerActionsFor,
  requiresConfirm,
  POWER_ACTION_LABEL,
  POWER_CONFIRM_COPY,
  type PowerActionKind,
} from './lobbyPowerPolicy';
import { IS_MAC } from '../platform';

export type FilterChip = 'all' | 'on' | 'off' | 'starred' | 'recent';
export type SortMode = 'name' | 'power state' | 'recent';

interface LiveSession {
  id: string;
  resourceId: string;
  /** VM name captured at session-open (session.metadata.vm_name). Used as the
   *  live-now label when the session's resourceId no longer matches the loaded
   *  inventory (stale/removed VM) — avoids the generic "Console" fallback. */
  vmName?: string;
}

export interface LobbyProps {
  activeEndpoint: Endpoint | null;
  /** The signed-in Prism user (whoami); null until fetched / on fetch failure. */
  identity: UserIdentity | null;
  resources: Resource[];
  filteredResources: Resource[];
  liveSessions: LiveSession[];
  recentOpens: RecentVMOpen[];
  tabResourceIds: Set<string>;
  favIds: Set<string>;
  loadingResources: boolean;
  resourcesUpdatedAt: number | null;
  nowTick: number;
  query: string;
  activeFilter: FilterChip;
  sortMode: SortMode;
  onQueryChange: (v: string) => void;
  onFilterChange: (c: FilterChip) => void;
  onSortChange: (s: SortMode) => void;
  onToggleStar: (id: string) => void;
  onRefresh: () => void;
  onOpenConsole: (r: Resource) => void;
  /** Per-endpoint capability evidence for the active endpoint (Shell's
   *  endpointHealth[activeEndpointId].capabilities). null until probed —
   *  treated as 'unknown' (controls show; only a PROVEN denial hides them). */
  powerCapability: EndpointCapabilities | null;
  /** Dispatch a power verb at a VM (resource-based: no session needed, so it
   *  works on a powered-off VM). Awaited so the tile can clear its transitioning
   *  state on settle. */
  onPowerAction: (resourceId: string, action: PowerActionKind) => Promise<void> | void;
  /** Resume/join a live session in its console window (openConsoleWindow). */
  onJoin: (sessionId: string, label: string) => void;
  /** Return to the connection manager to pick a different cluster endpoint. */
  onSwitchCluster: () => void;
  /** Log off the current cluster (invalidate the Prism session) and return to
   *  the connection manager. */
  onSignOut: () => void;
}

const isOnState = (r: Resource | undefined) => r?.status?.toLowerCase() === 'on';

// Stagger period for the entrance cascade. The grid is responsive (auto-fill),
// so the real column count varies with width; this is a fixed period that still
// yields a left-leaning left-to-right sweep regardless of the actual columns.
const GRID_COLS = 4;

// RECENT only surfaces VMs opened within this window (rolling 24h), not every
// VM ever opened.
const RECENT_WINDOW_MS = 24 * 60 * 60 * 1000;

function initialsOf(label: string | undefined): string {
  const s = (label ?? '').trim();
  if (!s) return 'AH';
  const parts = s.split(/[\s—–|/:-]+/).filter(Boolean);
  if (parts.length >= 2) return (parts[0]![0]! + parts[1]![0]!).toUpperCase();
  return s.slice(0, 2).toUpperCase();
}

/** Small status dot, paired with an aria-label so status is never color-only. */
function Dot({
  color,
  label,
  size = 8,
  pulse = false,
  halo = false,
}: {
  color: string;
  label: string;
  size?: number;
  pulse?: boolean;
  halo?: boolean;
}) {
  return (
    <span
      role="img"
      aria-label={`Status: ${label}`}
      className={pulse ? 'ec-pulse' : undefined}
      style={{
        width: size,
        height: size,
        borderRadius: '50%',
        background: color,
        color, // ec-pulse ring reads currentColor
        flexShrink: 0,
        display: 'inline-block',
        boxShadow: halo ? '0 0 0 4px rgba(255,255,255,.18)' : undefined,
      }}
    />
  );
}

function FolderGlyph({ size = 13 }: { size?: number }) {
  return (
    <svg
      width={size}
      height={size}
      viewBox="0 0 16 16"
      fill="none"
      stroke="currentColor"
      strokeWidth="1.5"
      strokeLinejoin="round"
      aria-hidden="true"
      style={{ display: 'block' }}
    >
      <path d="M1.8 4.2a1 1 0 0 1 1-1h2.6l1.4 1.5h6a1 1 0 0 1 1 1v6.1a1 1 0 0 1-1 1H2.8a1 1 0 0 1-1-1z" />
    </svg>
  );
}

export function Lobby(props: LobbyProps) {
  const {
    activeEndpoint,
    identity,
    resources,
    filteredResources,
    liveSessions,
    recentOpens,
    tabResourceIds,
    favIds,
    loadingResources,
    resourcesUpdatedAt,
    nowTick,
    query,
    activeFilter,
    sortMode,
    onQueryChange,
    onFilterChange,
    onSortChange,
    onToggleStar,
    onRefresh,
    onOpenConsole,
    onJoin,
    onSwitchCluster,
    onSignOut,
    powerCapability,
    onPowerAction,
  } = props;

  const epId = activeEndpoint?.id;
  // vm_power gate is endpoint-wide (a Prism role denial applies to every VM),
  // so compute it once here and pass the result to every tile.
  const powerGate = lobbyPowerGate(powerCapability);
  const env = activeEndpoint ? environmentOf(activeEndpoint).trim() : '';

  // ── Custom folders (local, per endpoint) ──────────────────────────────────
  const [folders, setFolders] = useState<VMFolder[]>(() => getFolders(epId));
  const [selectedFolderId, setSelectedFolderId] = useState<string | null>(null);
  const [creatingFolder, setCreatingFolder] = useState(false);
  useEffect(() => {
    setFolders(getFolders(epId));
    setSelectedFolderId(null);
  }, [epId]);

  const handleCreateFolder = (name: string, color?: string) => {
    if (epId && name.trim()) setFolders(createFolder(epId, name, color));
  };
  const handleDeleteFolder = (id: string) => {
    if (!epId) return;
    setFolders(deleteFolder(epId, id));
    setSelectedFolderId((cur) => (cur === id ? null : cur));
  };
  const handleToggleVm = (folderId: string, vmId: string) => {
    if (epId) setFolders(toggleVmInFolder(epId, folderId, vmId));
  };
  // Drag-and-drop add: only adds (never toggles off) when a VM is dropped on a
  // folder chip.
  const addVmToFolder = (folderId: string, vmId: string) => {
    if (!epId) return;
    const f = folders.find((x) => x.id === folderId);
    if (f && !f.vmIds.includes(vmId)) setFolders(toggleVmInFolder(epId, folderId, vmId));
  };
  const handleCreateAndAdd = (name: string, vmId: string, color?: string) => {
    if (epId && name.trim()) setFolders(createFolderWithVm(epId, name, vmId, color));
  };

  // When a folder is selected we intersect it with the already query/sorted
  // list (Shell's filteredResources); selecting a folder resets the built-in
  // chip to 'all' so the two filter dimensions don't fight.
  const shown = useMemo(() => {
    if (!selectedFolderId) return filteredResources;
    const folder = folders.find((f) => f.id === selectedFolderId);
    if (!folder) return filteredResources;
    const ids = new Set(folder.vmIds);
    return filteredResources.filter((r) => ids.has(r.id));
  }, [filteredResources, folders, selectedFolderId]);

  // Dedupe "live now" to one band per VM. The runtime can hold more than one
  // session for the same VM (e.g. a console reopened before the old session
  // was reaped), but the operator only needs a single presence entry to rejoin.
  const liveByVm = useMemo(() => {
    const seen = new Set<string>();
    const out: LiveSession[] = [];
    for (const s of liveSessions) {
      if (seen.has(s.resourceId)) continue;
      seen.add(s.resourceId);
      out.push(s);
    }
    return out;
  }, [liveSessions]);

  // ── RECENT carousel ───────────────────────────────────────────────────────
  const recentRef = useRef<HTMLDivElement>(null);
  const scrollRecent = (dir: 1 | -1) =>
    recentRef.current?.scrollBy({ left: dir * 258, behavior: 'smooth' });

  // Recent: VMs opened in the LAST 24 HOURS for the active endpoint (not every
  // VM ever opened), newest first, capped at 6, cross-referenced against the
  // current inventory so entries for deleted/missing VMs silently drop. The
  // window is measured against `nowTick` (re-evaluated every ~5s) so stale
  // entries age out on their own without a reload.
  // A VM that's currently live appears in the "live now" strip below; don't
  // also list it under Recent — the same object in both sections (styled two
  // different ways) read as two different things.
  const liveVmIds = useMemo(() => new Set(liveByVm.map((s) => s.resourceId)), [liveByVm]);

  const epRecents = activeEndpoint
    ? recentOpens
        .filter((r) => r.epId === activeEndpoint.id)
        .filter((r) => nowTick - new Date(r.openedAt).getTime() <= RECENT_WINDOW_MS)
        .map((r) => ({ rec: r, res: resources.find((x) => x.id === r.id) }))
        .filter((x): x is { rec: RecentVMOpen; res: Resource } => x.res !== undefined)
        .filter((x) => !liveVmIds.has(x.res.id))
        .slice(0, 6)
    : [];

  const selectBuiltin = (chip: FilterChip) => {
    setSelectedFolderId(null);
    onFilterChange(chip);
  };
  const selectFolder = (id: string) => {
    setSelectedFolderId(id);
    onFilterChange('all');
  };

  return (
    <div style={{ height: '100%', display: 'flex', background: T.paper }}>
      {/* ── A) Brand spine ─────────────────────────────────────────────── */}
      <aside
        style={
          {
            width: 86,
            flexShrink: 0,
            // Match the connection-manager rail (light, quiet) so the content —
            // not the chrome — carries the brand color.
            background: T.soft2,
            color: T.ink2,
            borderRight: `1px solid ${T.line}`,
            display: 'flex',
            flexDirection: 'column',
            alignItems: 'center',
            padding: `${IS_MAC ? 34 : 22}px 0 22px`,
            WebkitAppRegion: 'drag',
          } as React.CSSProperties
        }
      >
        <Spark size={30} color={T.blue} />
        <div style={{ width: 26, height: 1, background: T.rule, margin: '20px 0' }} />
        <div style={{ flex: 1, display: 'flex', alignItems: 'center', justifyContent: 'center' }}>
          <span
            style={{
              writingMode: 'vertical-rl',
              transform: 'rotate(180deg)',
              fontFamily: T.mono,
              fontSize: 11,
              fontWeight: 600,
              textTransform: 'uppercase',
              letterSpacing: '.42em',
              color: T.ink3,
            }}
          >
            AHV Remote Console
          </span>
        </div>
        <div style={{ display: 'flex', flexDirection: 'column', gap: 14, alignItems: 'center' }}>
          {env && (
            <span
              style={{
                fontFamily: T.mono,
                fontSize: 8.5,
                fontWeight: 700,
                letterSpacing: '.16em',
                background: T.green,
                color: '#fff',
                padding: '4px 7px',
                borderRadius: 5,
                textTransform: 'uppercase',
              }}
            >
              {env}
            </span>
          )}
          <button
            onClick={onRefresh}
            aria-label="Refresh VM inventory"
            title="Refresh"
            className="ec-focus"
            style={{
              ...noDrag,
              background: 'transparent',
              border: 'none',
              color: T.ink3,
              cursor: 'pointer',
              padding: 4,
              display: 'grid',
              placeItems: 'center',
            }}
          >
            <Icon name="refresh-pulse" size={17} />
          </button>
          <AccountMenu
            identity={identity}
            endpointLabel={activeEndpoint?.label}
            onSwitchCluster={onSwitchCluster}
            onSignOut={onSignOut}
          />
        </div>
      </aside>

      {/* ── B) Canvas ──────────────────────────────────────────────────── */}
      <div
        style={{
          flex: 1,
          minWidth: 0,
          position: 'relative',
          overflowX: 'hidden',
          overflowY: 'auto',
          background: T.paper,
          padding: '40px 46px',
        }}
      >
        {/* Brand decorations (behind content, non-interactive) */}
        <div
          aria-hidden
          style={{
            position: 'absolute',
            top: -80,
            right: -80,
            width: 220,
            height: 220,
            borderRadius: '50%',
            border: `2px dashed ${T.bluePale}`,
            opacity: 0.7,
            pointerEvents: 'none',
          }}
        />
        <div
          aria-hidden
          style={{
            position: 'absolute',
            top: 150,
            left: 30,
            width: 78,
            height: 78,
            backgroundImage: `radial-gradient(${T.blueL} 1.6px, transparent 2px)`,
            backgroundSize: '13px 13px',
            opacity: 0.3,
            pointerEvents: 'none',
          }}
        />

        {/* 1) Header row */}
        <div
          style={{
            position: 'relative',
            display: 'flex',
            justifyContent: 'space-between',
            alignItems: 'flex-start',
          }}
        >
          <div>
            <h1
              style={{
                fontFamily: T.sans,
                fontWeight: 300,
                fontSize: 66,
                lineHeight: 0.9,
                letterSpacing: '-0.02em',
                margin: 0,
                color: T.ink,
              }}
            >
              Lobby
            </h1>
            <div
              style={{
                display: 'flex',
                alignItems: 'center',
                gap: 8,
                marginTop: 14,
                fontFamily: T.mono,
                fontSize: 11,
                textTransform: 'uppercase',
                letterSpacing: '.1em',
                whiteSpace: 'nowrap',
              }}
            >
              <Dot
                color={liveByVm.length > 0 ? T.green : T.ink4}
                label={`${liveByVm.length} live`}
                pulse={liveByVm.length > 0}
              />
              <span style={{ color: T.greenD, fontWeight: 600 }}>
                {liveByVm.length} session{liveByVm.length !== 1 ? 's' : ''} live
              </span>
              <span style={{ color: T.ink4 }}>·</span>
              <span style={{ color: T.ink2 }}>{resources.length} VMs available</span>
            </div>
          </div>
          <div
            style={{ display: 'flex', flexDirection: 'column', alignItems: 'flex-end', gap: 10 }}
          >
            <button
              onClick={onRefresh}
              className="ec-focus"
              style={{
                display: 'inline-flex',
                alignItems: 'center',
                gap: 8,
                background: T.ink,
                color: '#fff',
                border: 'none',
                borderRadius: 10,
                padding: '11px 18px',
                fontFamily: T.sans,
                fontWeight: 600,
                fontSize: 14,
                cursor: 'pointer',
              }}
            >
              <Icon name="refresh-pulse" size={15} />
              Refresh
            </button>
            <span style={{ fontFamily: T.mono, fontSize: 10.5, color: T.ink4 }}>
              {resourcesUpdatedAt
                ? `updated ${relativeTime(new Date(resourcesUpdatedAt).toISOString(), nowTick)}`
                : 'never updated'}
            </span>
          </div>
        </div>

        {/* 2) RECENT — swipeable carousel */}
        {epRecents.length > 0 && (
          <section style={{ position: 'relative', marginTop: 34 }}>
            <div style={{ display: 'flex', alignItems: 'center', gap: 12 }}>
              <span
                style={{
                  fontFamily: T.mono,
                  fontSize: 11,
                  fontWeight: 600,
                  textTransform: 'uppercase',
                  letterSpacing: '.18em',
                  color: T.ink3,
                }}
              >
                Recent
              </span>
              <span style={{ flex: 1, height: 1, background: T.rule }} />
              <CarouselArrow dir={-1} onClick={() => scrollRecent(-1)} />
              <CarouselArrow dir={1} onClick={() => scrollRecent(1)} />
            </div>
            <div
              ref={recentRef}
              className="ec-hscroll"
              style={{
                display: 'flex',
                gap: 14,
                overflowX: 'auto',
                scrollSnapType: 'x mandatory',
                marginTop: 14,
                paddingBottom: 2,
              }}
            >
              {epRecents.map(({ rec, res }, i) => {
                const on = isOnState(res);
                const active = i === 0;
                return (
                  <button
                    key={rec.id}
                    onClick={() => onOpenConsole(res)}
                    className="ec-focus"
                    style={{
                      flexShrink: 0,
                      width: 244,
                      scrollSnapAlign: 'start',
                      textAlign: 'left',
                      cursor: 'pointer',
                      borderRadius: 14,
                      padding: '16px 18px',
                      border: active ? `1.5px solid ${T.blue}` : `1px solid ${T.rule}`,
                      background: active ? T.blueTint : T.soft,
                      boxShadow: active ? '0 12px 28px -16px rgba(21,125,193,.4)' : undefined,
                      display: 'flex',
                      flexDirection: 'column',
                    }}
                  >
                    <div style={{ display: 'flex', alignItems: 'center', gap: 8, minWidth: 0 }}>
                      <Dot
                        color={on ? T.green : T.ink4}
                        label={on ? 'On' : 'Off'}
                        size={7}
                        pulse={on}
                      />
                      <span
                        style={{
                          fontFamily: T.mono,
                          fontWeight: 600,
                          fontSize: 15,
                          color: T.ink,
                          overflow: 'hidden',
                          textOverflow: 'ellipsis',
                          whiteSpace: 'nowrap',
                        }}
                      >
                        {rec.vmName}
                      </span>
                      {active && (
                        <span style={{ marginLeft: 'auto' }}>
                          <Spark size={14} color={T.blue} />
                        </span>
                      )}
                    </div>
                    <div style={{ fontFamily: T.mono, fontSize: 11, color: T.ink3, marginTop: 12 }}>
                      opened {relativeTime(rec.openedAt, nowTick)}
                    </div>
                    <div
                      style={{
                        display: 'inline-flex',
                        alignItems: 'center',
                        gap: 6,
                        marginTop: 12,
                        fontFamily: T.sans,
                        fontWeight: 600,
                        fontSize: 13.5,
                        color: active ? T.blueD : T.ink2,
                      }}
                    >
                      Reconnect <Icon name="arrow-right" size={13} />
                    </div>
                  </button>
                );
              })}
            </div>
          </section>
        )}

        {/* 3) live now */}
        {liveByVm.length > 0 && (
          <section style={{ position: 'relative', marginTop: 34 }}>
            <SectionItalic label="live now" />
            <div style={{ display: 'flex', flexDirection: 'column', gap: 14, marginTop: 14 }}>
              {liveByVm.map((s) => {
                const res = resources.find((r) => r.id === s.resourceId);
                // Prefer the live inventory label; fall back to the name captured
                // on the session itself (stale/removed VM not in the current
                // inventory) before the generic "Console".
                const label = res?.label ?? s.vmName ?? 'Console';
                const host = res?.cluster ?? activeEndpoint?.label ?? '';
                return (
                  // Slim strip, not a hero: a quiet blue-tinted row with a thin
                  // brand accent — one live session shouldn't eat the screen.
                  <div
                    key={s.id}
                    style={{
                      position: 'relative',
                      display: 'flex',
                      alignItems: 'center',
                      gap: 12,
                      padding: '10px 14px',
                      borderRadius: 10,
                      background: T.blueTint,
                      border: `1px solid ${T.bluePale}`,
                      boxShadow: `inset 3px 0 0 0 ${T.blue}`,
                    }}
                  >
                    <Dot color={T.green} label="Live" size={9} pulse />
                    <div
                      style={{
                        minWidth: 0,
                        flex: 1,
                        display: 'flex',
                        alignItems: 'baseline',
                        gap: 9,
                      }}
                    >
                      <span
                        style={{
                          fontFamily: T.mono,
                          fontWeight: 700,
                          fontSize: 14,
                          color: T.ink,
                          overflow: 'hidden',
                          textOverflow: 'ellipsis',
                          whiteSpace: 'nowrap',
                          maxWidth: '60%',
                        }}
                      >
                        {label}
                      </span>
                      {host && (
                        <span
                          style={{
                            fontFamily: T.mono,
                            fontSize: 11,
                            color: T.ink3,
                            overflow: 'hidden',
                            textOverflow: 'ellipsis',
                            whiteSpace: 'nowrap',
                          }}
                        >
                          {host}
                        </span>
                      )}
                    </div>
                    <button
                      onClick={() => onJoin(s.id, label)}
                      className="ec-focus"
                      style={{
                        flexShrink: 0,
                        background: T.blue,
                        color: '#fff',
                        border: 'none',
                        borderRadius: 8,
                        padding: '8px 18px',
                        fontFamily: T.sans,
                        fontWeight: 600,
                        fontSize: 13,
                        cursor: 'pointer',
                      }}
                    >
                      Join
                    </button>
                  </div>
                );
              })}
            </div>
          </section>
        )}

        {/* 4) all VMs */}
        <section style={{ position: 'relative', marginTop: 34 }}>
          <SectionItalic label="all VMs" />
          {/* Toolbar */}
          <div style={{ display: 'flex', gap: 12, alignItems: 'center', marginTop: 14 }}>
            <div style={{ flex: 1, minWidth: 0 }}>
              <Field
                value={query}
                onChange={(e) => onQueryChange(e.target.value)}
                placeholder="Search VMs…"
                iconLeft={<Icon name="search" size={14} style={{ color: T.ink3 }} />}
              />
            </div>
            <div style={{ display: 'flex', gap: 6, flexWrap: 'wrap', justifyContent: 'flex-end' }}>
              {/* Order: Starred → folders (dynamic) → On → Off → All. The
                  "+" create sits at the very end (below). 'recent' is dropped
                  here — recents have their own carousel above. */}
              <Chip
                label="starred"
                active={activeFilter === 'starred' && !selectedFolderId}
                onClick={() => selectBuiltin('starred')}
              />
              {folders.map((f) => (
                <Chip
                  key={f.id}
                  label={f.name}
                  active={selectedFolderId === f.id}
                  accent={f.color}
                  leading={
                    <span
                      style={{
                        width: 8,
                        height: 8,
                        borderRadius: '50%',
                        background: f.color ?? T.ink4,
                        flexShrink: 0,
                      }}
                    />
                  }
                  onClick={() => selectFolder(f.id)}
                  onDelete={() => handleDeleteFolder(f.id)}
                  onDropVm={(vmId) => addVmToFolder(f.id, vmId)}
                />
              ))}
              {(['on', 'off', 'all'] as FilterChip[]).map((chip) => {
                const on = activeFilter === chip && !selectedFolderId;
                return (
                  <Chip key={chip} label={chip} active={on} onClick={() => selectBuiltin(chip)} />
                );
              })}
              <div style={{ position: 'relative' }}>
                <button
                  onClick={() => setCreatingFolder((v) => !v)}
                  className="ec-focus"
                  aria-label="New folder"
                  title="New folder"
                  style={{
                    display: 'inline-flex',
                    alignItems: 'center',
                    gap: 5,
                    padding: '8px 12px',
                    borderRadius: 999,
                    border: `1px dashed ${T.rule2}`,
                    background: 'transparent',
                    color: T.ink3,
                    fontFamily: T.mono,
                    fontSize: 12,
                    fontWeight: 600,
                    cursor: 'pointer',
                  }}
                >
                  <FolderGlyph size={11} /> +
                </button>
                {creatingFolder && (
                  <NewFolderPopover
                    onCreate={(name, color) => {
                      handleCreateFolder(name, color);
                      setCreatingFolder(false);
                    }}
                    onClose={() => setCreatingFolder(false)}
                  />
                )}
              </div>
            </div>
            <select
              value={sortMode}
              onChange={(e) => onSortChange(e.target.value as SortMode)}
              aria-label="Sort VMs"
              style={{
                fontFamily: T.mono,
                fontSize: 12,
                padding: '8px 10px',
                borderRadius: 9,
                background: T.soft,
                color: T.ink2,
                border: `1px solid ${T.rule2}`,
                cursor: 'pointer',
                appearance: 'auto',
              }}
            >
              <option value="name">sort: name</option>
              <option value="power state">sort: power state</option>
              <option value="recent">sort: recent</option>
            </select>
          </div>

          {/* Grid — re-keyed per category so it crossfades (opacity only). */}
          {loadingResources ? (
            <div style={{ padding: 40, textAlign: 'center' }}>
              <Spinner label="Loading VMs…" />
            </div>
          ) : shown.length === 0 ? (
            <div
              style={{
                padding: 40,
                textAlign: 'center',
                fontFamily: T.mono,
                fontSize: 12,
                color: T.ink3,
              }}
            >
              {selectedFolderId ? 'This folder is empty.' : 'No VMs match.'}
            </div>
          ) : (
            <div
              // Re-keyed per category so every tile is a fresh node and replays
              // the staggered entrance (the cascade IS the switch animation).
              key={selectedFolderId ?? activeFilter}
              style={{
                display: 'grid',
                // Responsive, not a forced 4 columns: fit as many ~260px cards
                // as the canvas width allows, each flexing to fill — so the
                // grid adapts to any window size and never overflows the edge.
                gridTemplateColumns: 'repeat(auto-fill, minmax(260px, 1fr))',
                gap: 10,
                marginTop: 14,
              }}
            >
              {shown.map((r, i) => (
                <VMTile
                  key={r.id}
                  resource={r}
                  hasActiveTab={tabResourceIds.has(r.id)}
                  starred={favIds.has(r.id)}
                  folders={folders}
                  // Left-to-right sweep: column index dominates the delay; a small
                  // (capped) row term keeps it lively without lagging long lists.
                  enterDelayMs={(i % GRID_COLS) * 60 + Math.min(Math.floor(i / GRID_COLS), 14) * 8}
                  onToggleStar={() => onToggleStar(r.id)}
                  onToggleFolder={(folderId) => handleToggleVm(folderId, r.id)}
                  onCreateAndAdd={(name, color) => handleCreateAndAdd(name, r.id, color)}
                  onOpen={() => onOpenConsole(r)}
                  showPower={powerGate.show}
                  powerEnabled={powerGate.enabled}
                  onPowerAction={(action) => onPowerAction(r.id, action)}
                />
              ))}
            </div>
          )}
        </section>
      </div>
    </div>
  );
}

/** Filter chip — built-in or a custom folder (folders get a delete affordance). */
function Chip({
  label,
  active,
  onClick,
  leading,
  onDelete,
  accent,
  onDropVm,
}: {
  label: string;
  active: boolean;
  onClick: () => void;
  leading?: React.ReactNode;
  onDelete?: () => void;
  /** Active-state background (folder color); defaults to brand blue. */
  accent?: string;
  /** When set, the chip is a drop target: dropping a dragged VM adds it here. */
  onDropVm?: (vmId: string) => void;
}) {
  const [dragOver, setDragOver] = useState(false);
  const [popped, setPopped] = useState(false);
  const dropHandlers = onDropVm
    ? {
        onDragOver: (e: React.DragEvent) => {
          e.preventDefault();
          e.dataTransfer.dropEffect = 'copy';
          if (!dragOver) setDragOver(true);
        },
        onDragLeave: () => setDragOver(false),
        onDrop: (e: React.DragEvent) => {
          e.preventDefault();
          setDragOver(false);
          const id =
            e.dataTransfer.getData('application/x-ahv-vm') || e.dataTransfer.getData('text/plain');
          if (id) {
            onDropVm(id);
            setPopped(true); // play the landing pop (cleared on animationend)
          }
        },
      }
    : {};
  return (
    <span
      {...dropHandlers}
      className={popped ? 'ec-drop-pop' : undefined}
      onAnimationEnd={() => setPopped(false)}
      style={{
        display: 'inline-flex',
        alignItems: 'center',
        borderRadius: 999,
        background: dragOver ? 'var(--blueGhost)' : active ? (accent ?? T.blue) : 'transparent',
        color: active && !dragOver ? '#fff' : T.ink2,
        boxShadow: dragOver ? `0 0 0 2px ${accent ?? T.blue}` : '0 0 0 0 transparent',
        transition: 'box-shadow 130ms ease, background 130ms ease',
      }}
    >
      <button
        onClick={onClick}
        className="ec-focus"
        aria-pressed={active}
        style={{
          display: 'inline-flex',
          alignItems: 'center',
          gap: 5,
          padding: onDelete ? '8px 8px 8px 12px' : '8px 14px',
          border: 'none',
          background: 'transparent',
          color: 'inherit',
          fontFamily: T.mono,
          fontSize: 12,
          fontWeight: 600,
          cursor: 'pointer',
          borderRadius: 999,
        }}
      >
        {leading}
        {label}
      </button>
      {onDelete && (
        <button
          onClick={(e) => {
            e.stopPropagation();
            onDelete();
          }}
          aria-label={`Delete folder ${label}`}
          title="Delete folder"
          className="ec-focus"
          style={{
            border: 'none',
            background: 'transparent',
            color: 'inherit',
            cursor: 'pointer',
            padding: '0 10px 0 2px',
            fontSize: 13,
            lineHeight: 1,
            opacity: 0.7,
          }}
        >
          ×
        </button>
      )}
    </span>
  );
}

// Preset folder accents (token-based; no hardcoded hex). Color is optional.
const FOLDER_COLORS = [T.blue, T.green, T.amber, T.red, T.blueDk];

function ColorSwatches({ value, onChange }: { value?: string; onChange: (c?: string) => void }) {
  return (
    <div style={{ display: 'flex', gap: 6, alignItems: 'center' }}>
      {FOLDER_COLORS.map((c) => (
        <button
          key={c}
          type="button"
          onClick={() => onChange(c)}
          aria-label={`Set color ${c}`}
          aria-pressed={value === c}
          style={{
            width: 18,
            height: 18,
            borderRadius: '50%',
            background: c,
            border: value === c ? `2px solid ${T.ink}` : '2px solid transparent',
            cursor: 'pointer',
            padding: 0,
          }}
        />
      ))}
      <button
        type="button"
        onClick={() => onChange(undefined)}
        aria-label="No color"
        title="No color"
        aria-pressed={value === undefined}
        style={{
          width: 18,
          height: 18,
          borderRadius: '50%',
          background: 'transparent',
          border: value === undefined ? `2px solid ${T.ink3}` : `1px dashed ${T.rule2}`,
          color: T.ink4,
          fontSize: 11,
          lineHeight: 1,
          cursor: 'pointer',
          padding: 0,
        }}
      >
        ×
      </button>
    </div>
  );
}

/** Chip-row popover to create a new folder with an optional accent color. */
function NewFolderPopover({
  onCreate,
  onClose,
}: {
  onCreate: (name: string, color?: string) => void;
  onClose: () => void;
}) {
  const ref = useRef<HTMLDivElement>(null);
  const [name, setName] = useState('');
  const [color, setColor] = useState<string | undefined>(undefined);
  useEffect(() => {
    const close = (e: MouseEvent) => {
      if (ref.current && !ref.current.contains(e.target as Node)) onClose();
    };
    document.addEventListener('mousedown', close);
    return () => document.removeEventListener('mousedown', close);
  }, [onClose]);
  const submit = () => {
    if (name.trim()) onCreate(name, color);
  };
  return (
    <div
      ref={ref}
      style={{
        position: 'absolute',
        top: 'calc(100% + 8px)',
        right: 0,
        width: 232,
        background: T.paper,
        border: `1px solid ${T.rule2}`,
        borderRadius: 12,
        boxShadow: '0 12px 28px -10px rgba(15,34,53,.35)',
        padding: 12,
        zIndex: 200,
        display: 'flex',
        flexDirection: 'column',
        gap: 10,
      }}
    >
      <div
        style={{
          fontFamily: T.mono,
          fontSize: 9.5,
          textTransform: 'uppercase',
          letterSpacing: '.14em',
          color: T.ink3,
        }}
      >
        New folder
      </div>
      <input
        autoFocus
        value={name}
        onChange={(e) => setName(e.target.value)}
        onKeyDown={(e) => {
          if (e.key === 'Enter') submit();
          else if (e.key === 'Escape') onClose();
        }}
        placeholder="Folder name…"
        style={{
          padding: '8px 10px',
          borderRadius: 8,
          border: `1px solid ${T.rule2}`,
          outline: 'none',
          fontFamily: T.sans,
          fontSize: 13,
          color: T.ink,
        }}
      />
      <ColorSwatches value={color} onChange={setColor} />
      <button
        type="button"
        onClick={submit}
        className="ec-focus"
        disabled={!name.trim()}
        style={{
          alignSelf: 'flex-end',
          padding: '7px 16px',
          borderRadius: 8,
          border: 'none',
          background: name.trim() ? T.blue : T.rule2,
          color: '#fff',
          fontFamily: T.sans,
          fontWeight: 600,
          fontSize: 13,
          cursor: name.trim() ? 'pointer' : 'default',
        }}
      >
        Create
      </button>
    </div>
  );
}

function CarouselArrow({ dir, onClick }: { dir: 1 | -1; onClick: () => void }) {
  return (
    <button
      onClick={onClick}
      className="ec-focus"
      aria-label={dir < 0 ? 'Scroll recent left' : 'Scroll recent right'}
      title={dir < 0 ? 'Previous' : 'Next'}
      style={{
        width: 26,
        height: 26,
        borderRadius: 8,
        border: `1px solid ${T.rule2}`,
        background: T.bg,
        color: T.ink2,
        cursor: 'pointer',
        display: 'grid',
        placeItems: 'center',
        fontSize: 14,
        lineHeight: 1,
        flexShrink: 0,
      }}
    >
      {dir < 0 ? '‹' : '›'}
    </button>
  );
}

/** Italic section heading (live now / all VMs). */
function SectionItalic({ label }: { label: string }) {
  return (
    <h2
      style={{
        fontFamily: T.sans,
        fontWeight: 500,
        fontStyle: 'italic',
        fontSize: 17,
        color: T.ink2,
        margin: 0,
      }}
    >
      {label}
    </h2>
  );
}

/** Spine account menu: switch cluster endpoint or sign out of the current one.
 *  The avatar shows WHO is signed in (user initials), not where — falling back
 *  to the endpoint initials only until the identity has been fetched. */
function AccountMenu({
  identity,
  endpointLabel,
  onSwitchCluster,
  onSignOut,
}: {
  identity: UserIdentity | null;
  endpointLabel?: string;
  onSwitchCluster: () => void;
  onSignOut: () => void;
}) {
  const [open, setOpen] = useState(false);
  const ref = useRef<HTMLDivElement>(null);
  useEffect(() => {
    if (!open) return;
    const close = (e: MouseEvent) => {
      if (ref.current && !ref.current.contains(e.target as Node)) setOpen(false);
    };
    document.addEventListener('mousedown', close);
    return () => document.removeEventListener('mousedown', close);
  }, [open]);
  return (
    <div ref={ref} style={{ ...noDrag, position: 'relative' }}>
      <button
        onClick={() => setOpen((v) => !v)}
        aria-haspopup="menu"
        aria-expanded={open}
        aria-label="Account menu"
        title={identity?.username ?? endpointLabel ?? 'Account'}
        className="ec-focus"
        style={{
          width: 28,
          height: 28,
          borderRadius: '50%',
          background: T.blueGhost,
          border: `1px solid ${T.bluePale}`,
          color: T.blueD,
          cursor: 'pointer',
          display: 'grid',
          placeItems: 'center',
          fontFamily: T.mono,
          fontSize: 10,
          fontWeight: 600,
        }}
      >
        {userInitials(identity) ?? initialsOf(endpointLabel)}
      </button>
      {open && (
        <div
          role="menu"
          style={{
            position: 'absolute',
            bottom: 0,
            left: 'calc(100% + 10px)',
            minWidth: 188,
            background: T.paper,
            border: `1px solid ${T.rule2}`,
            borderRadius: 11,
            boxShadow: '0 12px 28px -10px rgba(15,34,53,.35)',
            padding: 6,
            zIndex: 200,
          }}
        >
          {(identity ?? endpointLabel) && (
            <div
              style={{
                padding: '6px 10px 8px',
                borderBottom: `1px solid ${T.rule}`,
                marginBottom: 4,
                overflow: 'hidden',
              }}
            >
              {identity && (
                <div
                  style={{
                    fontFamily: T.sans,
                    fontSize: 12,
                    fontWeight: 600,
                    color: T.ink,
                    overflow: 'hidden',
                    textOverflow: 'ellipsis',
                    whiteSpace: 'nowrap',
                  }}
                >
                  {/* eslint-disable-next-line @typescript-eslint/prefer-nullish-coalescing -- intentional falsy check: a whitespace-only display_name trims to "" and must fall through to username */}
                  {identity.display_name?.trim() || identity.username}
                </div>
              )}
              {endpointLabel && (
                <div
                  style={{
                    fontFamily: T.mono,
                    fontSize: 10,
                    color: T.ink3,
                    marginTop: identity ? 2 : 0,
                    overflow: 'hidden',
                    textOverflow: 'ellipsis',
                    whiteSpace: 'nowrap',
                  }}
                >
                  {endpointLabel}
                </div>
              )}
            </div>
          )}
          <MenuItem
            label="Switch cluster"
            onClick={() => {
              setOpen(false);
              onSwitchCluster();
            }}
          />
          <MenuItem
            label="Sign out"
            danger
            onClick={() => {
              setOpen(false);
              onSignOut();
            }}
          />
        </div>
      )}
    </div>
  );
}

function MenuItem({
  label,
  onClick,
  danger,
}: {
  label: string;
  onClick: () => void;
  danger?: boolean;
}) {
  return (
    <button
      role="menuitem"
      onClick={onClick}
      className="ec-focus"
      style={{
        display: 'block',
        width: '100%',
        textAlign: 'left',
        border: 'none',
        background: 'transparent',
        padding: '8px 10px',
        borderRadius: 7,
        cursor: 'pointer',
        fontFamily: T.sans,
        fontSize: 13,
        fontWeight: 500,
        color: danger ? T.red : T.ink,
      }}
      onMouseEnter={(e) => {
        e.currentTarget.style.background = 'var(--soft)';
      }}
      onMouseLeave={(e) => {
        e.currentTarget.style.background = 'transparent';
      }}
    >
      {label}
    </button>
  );
}

/** Per-tile popover to add/remove this VM from custom folders. */
function FolderMenu({
  folders,
  vmId,
  onToggleFolder,
  onCreateAndAdd,
  onClose,
}: {
  folders: VMFolder[];
  vmId: string;
  onToggleFolder: (folderId: string) => void;
  onCreateAndAdd: (name: string, color?: string) => void;
  onClose: () => void;
}) {
  const ref = useRef<HTMLDivElement>(null);
  const [name, setName] = useState('');
  const [color, setColor] = useState<string | undefined>(undefined);
  useEffect(() => {
    const close = (e: MouseEvent) => {
      if (ref.current && !ref.current.contains(e.target as Node)) onClose();
    };
    document.addEventListener('mousedown', close);
    return () => document.removeEventListener('mousedown', close);
  }, [onClose]);
  return (
    <div
      ref={ref}
      onClick={(e) => e.stopPropagation()}
      style={{
        position: 'absolute',
        top: 'calc(100% + 6px)',
        right: 0,
        width: 200,
        background: T.paper,
        border: `1px solid ${T.rule2}`,
        borderRadius: 11,
        boxShadow: '0 12px 28px -10px rgba(15,34,53,.35)',
        padding: 6,
        zIndex: 50,
      }}
    >
      <div
        style={{
          fontFamily: T.mono,
          fontSize: 9.5,
          textTransform: 'uppercase',
          letterSpacing: '.14em',
          color: T.ink3,
          padding: '4px 8px 6px',
        }}
      >
        Add to folder
      </div>
      {folders.length === 0 && (
        <div style={{ fontFamily: T.sans, fontSize: 12, color: T.ink3, padding: '0 8px 6px' }}>
          No folders yet.
        </div>
      )}
      {folders.map((f) => {
        const member = f.vmIds.includes(vmId);
        return (
          <button
            key={f.id}
            onClick={() => onToggleFolder(f.id)}
            className="ec-focus"
            role="menuitemcheckbox"
            aria-checked={member}
            style={{
              display: 'flex',
              alignItems: 'center',
              gap: 8,
              width: '100%',
              textAlign: 'left',
              border: 'none',
              background: 'transparent',
              padding: '7px 8px',
              borderRadius: 7,
              cursor: 'pointer',
              fontFamily: T.sans,
              fontSize: 13,
              color: T.ink,
            }}
            onMouseEnter={(e) => {
              e.currentTarget.style.background = 'var(--soft)';
            }}
            onMouseLeave={(e) => {
              e.currentTarget.style.background = 'transparent';
            }}
          >
            <span style={{ width: 14, color: T.green, flexShrink: 0 }}>{member ? '✓' : ''}</span>
            <span
              style={{
                width: 8,
                height: 8,
                borderRadius: '50%',
                background: f.color ?? T.ink4,
                flexShrink: 0,
              }}
            />
            <span style={{ overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>
              {f.name}
            </span>
          </button>
        );
      })}
      <div
        style={{
          display: 'flex',
          flexDirection: 'column',
          gap: 8,
          padding: '8px 6px 2px',
          borderTop: `1px solid ${T.rule}`,
          marginTop: 4,
        }}
      >
        <div style={{ display: 'flex', gap: 6 }}>
          <input
            value={name}
            onChange={(e) => setName(e.target.value)}
            onKeyDown={(e) => {
              if (e.key === 'Enter' && name.trim()) {
                onCreateAndAdd(name, color);
                setName('');
              }
            }}
            placeholder="New folder…"
            style={{
              flex: 1,
              minWidth: 0,
              padding: '6px 8px',
              borderRadius: 7,
              border: `1px solid ${T.rule2}`,
              outline: 'none',
              fontFamily: T.sans,
              fontSize: 12,
              color: T.ink,
            }}
          />
          <button
            onClick={() => {
              if (name.trim()) {
                onCreateAndAdd(name, color);
                setName('');
              }
            }}
            className="ec-focus"
            aria-label="Create folder and add VM"
            style={{
              border: 'none',
              background: T.blue,
              color: '#fff',
              borderRadius: 7,
              padding: '0 10px',
              cursor: 'pointer',
              fontSize: 14,
            }}
          >
            +
          </button>
        </div>
        <ColorSwatches value={color} onChange={setColor} />
      </div>
    </div>
  );
}

function VMTile({
  resource,
  hasActiveTab,
  starred,
  folders,
  enterDelayMs = 0,
  onToggleStar,
  onToggleFolder,
  onCreateAndAdd,
  onOpen,
  showPower,
  powerEnabled,
  onPowerAction,
}: {
  resource: Resource;
  hasActiveTab: boolean;
  starred: boolean;
  folders: VMFolder[];
  enterDelayMs?: number;
  onToggleStar: () => void;
  onToggleFolder: (folderId: string) => void;
  onCreateAndAdd: (name: string, color?: string) => void;
  onOpen: () => void;
  showPower: boolean;
  powerEnabled: boolean;
  onPowerAction: (action: PowerActionKind) => Promise<void> | void;
}) {
  const isOn = resource.status?.toLowerCase() === 'on';
  const secondary = [resource.ip, resource.cluster].filter(Boolean).join(' · ');
  const [folderMenuOpen, setFolderMenuOpen] = useState(false);
  const [dragging, setDragging] = useState(false);
  // Optimistic per-tile transitioning flag: set on dispatch, cleared on settle.
  // The lobby's status is cached inventory and won't flip instantly, so this is
  // what reflects the in-flight state until Shell re-fetches resources.
  const [transitioning, setTransitioning] = useState(false);
  const [confirmAction, setConfirmAction] = useState<PowerActionKind | null>(null);

  const dispatchPower = (action: PowerActionKind) => {
    setTransitioning(true);
    void Promise.resolve(onPowerAction(action)).finally(() => setTransitioning(false));
  };
  const inAnyFolder = folders.some((f) => f.vmIds.includes(resource.id));
  // Carry the accent of the first COLORED folder this VM belongs to onto the
  // card: a tinted border + a left accent stripe (the stripe survives the
  // hover border-color change since it's drawn with an inset box-shadow).
  const folderColor = folders.find((f) => f.color && f.vmIds.includes(resource.id))?.color;
  return (
    <div
      onClick={onOpen}
      className="ec-tile-in"
      // Drag a VM onto a folder chip to file it there (the folder menu remains
      // the keyboard/click-accessible path).
      draggable
      onDragStart={(e) => {
        e.dataTransfer.setData('application/x-ahv-vm', resource.id);
        e.dataTransfer.setData('text/plain', resource.id);
        e.dataTransfer.effectAllowed = 'copy';
        setDragging(true);
      }}
      onDragEnd={() => setDragging(false)}
      style={{
        position: 'relative',
        display: 'flex',
        alignItems: 'center',
        gap: 9,
        minWidth: 0,
        padding: '11px 13px',
        border: `1px solid ${folderColor ?? T.rule}`,
        borderRadius: 11,
        background: T.paper,
        boxShadow: folderColor ? `inset 3px 0 0 0 ${folderColor}` : undefined,
        cursor: 'pointer',
        userSelect: 'none',
        opacity: dragging ? 0.45 : 1,
        transform: dragging ? 'scale(0.97)' : 'none',
        animationDelay: `${enterDelayMs}ms`,
        transition:
          'border-color 120ms, background 120ms, opacity 140ms ease, transform 140ms ease',
      }}
      onMouseEnter={(e) => {
        const el = e.currentTarget as HTMLElement;
        el.style.borderColor = folderColor ?? 'var(--bluePale)';
        el.style.background = 'var(--blueTint)';
      }}
      onMouseLeave={(e) => {
        const el = e.currentTarget as HTMLElement;
        el.style.borderColor = folderColor ?? 'var(--rule)';
        el.style.background = 'var(--paper)';
      }}
      aria-busy={transitioning || undefined}
    >
      {transitioning ? (
        <Spinner size={11} color={T.blue} />
      ) : (
        <Dot color={isOn ? T.green : T.ink4} label={isOn ? 'On' : 'Off'} size={8} pulse={isOn} />
      )}
      <div style={{ minWidth: 0, flex: 1 }}>
        <span
          style={{
            display: 'block',
            fontFamily: T.mono,
            fontSize: 12.5,
            fontWeight: 600,
            color: T.ink,
            overflow: 'hidden',
            textOverflow: 'ellipsis',
            whiteSpace: 'nowrap',
          }}
        >
          {resource.label}
        </span>
        {secondary && (
          <span
            style={{
              display: 'block',
              fontFamily: T.mono,
              fontSize: 9.5,
              color: T.ink3,
              overflow: 'hidden',
              textOverflow: 'ellipsis',
              whiteSpace: 'nowrap',
              marginTop: 1,
            }}
          >
            {secondary}
          </span>
        )}
      </div>
      {!isOn && (
        <span style={{ fontFamily: T.mono, fontSize: 9, color: T.ink4 }} aria-hidden>
          off
        </span>
      )}
      {showPower && (
        <span
          style={{ display: 'inline-flex', alignItems: 'center', gap: 2, flexShrink: 0 }}
          // Power buttons live inside the whole-tile onClick=onOpen card; the
          // wrapper stops propagation as a backstop so a power click never also
          // launches a console.
          onClick={(e) => e.stopPropagation()}
        >
          {powerActionsFor(resource.status ?? '').map((action) => (
            <PowerButton
              key={action}
              action={action}
              vmLabel={resource.label}
              disabled={!powerEnabled || transitioning}
              onConfirm={() => setConfirmAction(action)}
              onImmediate={() => dispatchPower(action)}
            />
          ))}
        </span>
      )}
      <button
        aria-label="Add to folder"
        title="Add to folder"
        className="ec-focus"
        onClick={(e) => {
          e.stopPropagation();
          setFolderMenuOpen((v) => !v);
        }}
        style={{
          flexShrink: 0,
          border: 'none',
          background: 'transparent',
          cursor: 'pointer',
          padding: 2,
          display: 'grid',
          placeItems: 'center',
          color: inAnyFolder ? T.blue : T.ink4,
        }}
      >
        <FolderGlyph size={13} />
      </button>
      <span
        role="button"
        aria-label={starred ? 'Remove from favorites' : 'Add to favorites'}
        title={starred ? 'Remove from favorites' : 'Add to favorites'}
        onClick={(e) => {
          e.stopPropagation();
          onToggleStar();
        }}
        style={{
          flexShrink: 0,
          cursor: 'pointer',
          lineHeight: 1,
          fontSize: 14,
          color: starred ? T.green : T.ink4,
        }}
      >
        {starred ? '★' : '☆'}
      </span>
      {hasActiveTab && (
        <span
          title="Active session"
          aria-label="Active session"
          style={{
            position: 'absolute',
            top: 5,
            right: 5,
            width: 6,
            height: 6,
            borderRadius: '50%',
            background: T.green,
            border: `1.5px solid ${T.paper}`,
            boxSizing: 'content-box',
          }}
        />
      )}
      {folderMenuOpen && (
        <FolderMenu
          folders={folders}
          vmId={resource.id}
          onToggleFolder={onToggleFolder}
          onCreateAndAdd={onCreateAndAdd}
          onClose={() => setFolderMenuOpen(false)}
        />
      )}
      {confirmAction && (
        <PowerConfirm
          action={confirmAction}
          vmLabel={resource.label}
          onCancel={() => setConfirmAction(null)}
          onConfirm={() => {
            const action = confirmAction;
            setConfirmAction(null);
            dispatchPower(action);
          }}
        />
      )}
    </div>
  );
}

const POWER_ICON: Record<PowerActionKind, IconName> = {
  ON: 'power-on',
  OFF: 'power-off',
  ACPI_REBOOT: 'power-cycle',
};

/** A single per-tile power button. One-click verbs (Power on) dispatch
 *  immediately; destructive verbs open the confirm dialog instead. */
function PowerButton({
  action,
  vmLabel,
  disabled,
  onConfirm,
  onImmediate,
}: {
  action: PowerActionKind;
  vmLabel: string;
  disabled: boolean;
  onConfirm: () => void;
  onImmediate: () => void;
}) {
  const label = POWER_ACTION_LABEL[action];
  return (
    <button
      type="button"
      aria-label={`${label} ${vmLabel}`}
      title={label}
      className="ec-focus"
      disabled={disabled}
      onClick={(e) => {
        e.stopPropagation();
        if (requiresConfirm(action)) onConfirm();
        else onImmediate();
      }}
      style={{
        flexShrink: 0,
        border: 'none',
        background: 'transparent',
        cursor: disabled ? 'default' : 'pointer',
        padding: 3,
        display: 'grid',
        placeItems: 'center',
        color: action === 'ON' ? T.green : T.ink3,
        opacity: disabled ? 0.4 : 1,
        borderRadius: 6,
      }}
    >
      <Icon name={POWER_ICON[action]} size={14} />
    </button>
  );
}

/** Mandatory confirm dialog for the destructive power verbs (Shut down /
 *  Restart). Backdrop + surface follow the in-package popover styling; the
 *  a11y semantics (role=dialog, focus-trap, Escape) come from Dialog. */
function PowerConfirm({
  action,
  vmLabel,
  onCancel,
  onConfirm,
}: {
  action: PowerActionKind;
  vmLabel: string;
  onCancel: () => void;
  onConfirm: () => void;
}) {
  const label = POWER_ACTION_LABEL[action];
  return (
    <div
      // Backdrop. Stop propagation so a backdrop click never reaches the
      // tile's onClick=onOpen, and close on backdrop click.
      onClick={(e) => {
        e.stopPropagation();
        onCancel();
      }}
      style={{
        position: 'fixed',
        inset: 0,
        background: 'rgba(15,34,53,.35)',
        display: 'grid',
        placeItems: 'center',
        zIndex: 400,
      }}
    >
      <Dialog
        open
        onClose={onCancel}
        aria-label={`${label} ${vmLabel}`}
        style={{ outline: 'none' }}
      >
        <div
          onClick={(e) => e.stopPropagation()}
          style={{
            width: 360,
            maxWidth: '90vw',
            background: T.paper,
            border: `1px solid ${T.rule2}`,
            borderRadius: 14,
            boxShadow: '0 24px 48px -16px rgba(15,34,53,.45)',
            padding: 22,
            display: 'flex',
            flexDirection: 'column',
            gap: 14,
          }}
        >
          <div
            style={{
              fontFamily: T.sans,
              fontSize: 16,
              fontWeight: 600,
              color: T.ink,
            }}
          >
            {label} {vmLabel}?
          </div>
          <div style={{ fontFamily: T.sans, fontSize: 13.5, color: T.ink2, lineHeight: 1.5 }}>
            {POWER_CONFIRM_COPY[action]}
          </div>
          <div style={{ display: 'flex', justifyContent: 'flex-end', gap: 8, marginTop: 4 }}>
            <button
              type="button"
              onClick={(e) => {
                e.stopPropagation();
                onCancel();
              }}
              className="ec-focus"
              style={{
                padding: '8px 16px',
                borderRadius: 8,
                border: `1px solid ${T.rule2}`,
                background: 'transparent',
                color: T.ink2,
                fontFamily: T.sans,
                fontWeight: 600,
                fontSize: 13,
                cursor: 'pointer',
              }}
            >
              Cancel
            </button>
            <button
              type="button"
              autoFocus
              onClick={(e) => {
                e.stopPropagation();
                onConfirm();
              }}
              className="ec-focus"
              style={{
                padding: '8px 18px',
                borderRadius: 8,
                border: 'none',
                background: T.red,
                color: '#fff',
                fontFamily: T.sans,
                fontWeight: 600,
                fontSize: 13,
                cursor: 'pointer',
              }}
            >
              {label}
            </button>
          </div>
        </div>
      </Dialog>
    </div>
  );
}
