import { useCallback, useEffect, useMemo, useRef, useState } from 'react';
import { m, AnimatePresence, LayoutGroup } from 'framer-motion';
import { Spinner, useToast } from '@ahv-remote-console/ui';
import { type Route, type Direction, inferDirection } from './routeReducer';
import { fadeThroughVariants, sharedAxisX } from '../motion/variants';
import type {
  Endpoint,
  EndpointHealth,
  Resource,
  UserIdentity,
} from '@ahv-remote-console/protocol';
import { useClient } from './ClientContext';
import { SignInPanel } from '../components/SignInPanel';
import { AddEndpointPanel } from '../components/AddEndpointPanel';
import { EndpointPicker } from '../components/EndpointPicker';
import { Lobby } from '../components/Lobby';
import { POWER_SDK_ACTION, type PowerActionKind } from '../components/lobbyPowerPolicy';
import {
  recordVMOpen,
  getRecentVMOpens,
  setEndpointMeta,
  recordEndpointConnection,
  getNodePositions,
  setNodePositions,
  getClusterLinks,
  addClusterLink,
  removeClusterLink,
  pruneClusterLinks,
  type RecentVMOpen,
  type NodePosition,
  type ClusterLink,
} from './recentStore';
import { ClusterMap } from '../components/ClusterMap';
import { TasksTray } from '../components/TasksTray';
import { useTasks } from './useTasks';

type FilterChip = 'all' | 'on' | 'off' | 'starred' | 'recent';
type SortMode = 'name' | 'power state' | 'recent';

const RECENT_KEY = 'ahv-recent-vms';
const MAX_RECENT = 50;
const FAVORITES_KEY = 'ahv-favorite-vms';

function getRecentVmIds(): string[] {
  try {
    return JSON.parse(localStorage.getItem(RECENT_KEY) ?? '[]') as string[];
  } catch {
    return [];
  }
}
function pushRecentVmId(id: string) {
  const ids = getRecentVmIds().filter((x) => x !== id);
  ids.unshift(id);
  if (ids.length > MAX_RECENT) ids.length = MAX_RECENT;
  localStorage.setItem(RECENT_KEY, JSON.stringify(ids));
}

function getFavoriteVmIds(): Set<string> {
  try {
    return new Set(JSON.parse(localStorage.getItem(FAVORITES_KEY) ?? '[]') as string[]);
  } catch {
    return new Set();
  }
}
function toggleFavoriteVmId(id: string): Set<string> {
  const favs = getFavoriteVmIds();
  if (favs.has(id)) favs.delete(id);
  else favs.add(id);
  localStorage.setItem(FAVORITES_KEY, JSON.stringify([...favs]));
  return favs;
}

const C = {
  paper: '#ffffff',
  paperBorder: '#dbe7f0',
  tile: '#f6fafd',
  tileBorder: '#dbe7f0',
  ink: '#0f2235',
  muted: 'var(--hc-fg-2)',
  cardBg: 'var(--hc-bg-0)',
  cardText: 'var(--hc-fg-0)',
  cardBorder: 'var(--hc-bg-3)',
  stageBg: 'var(--hc-bg-0)',
  green: 'var(--hc-success)',
} as const;

type AppStage =
  | 'loading'
  | 'no-endpoints'
  | 'add-endpoint'
  | 'select-endpoint'
  | 'sign-in'
  | 'ready';

// `onOpenPalette` is still accepted (App.tsx passes it) but no longer used —
// the in-window console tab/palette subsystem was removed (consoles now open in
// their own OS windows). Kept in the prop type so the live caller compiles.
export function Shell(_props: { onOpenPalette: () => void }) {
  const client = useClient();
  const toast = useToast();
  // Shared Tasks-tray poller: the tray always renders the long-running ops, and
  // powerVm bumps refresh() so a just-fired power task appears immediately
  // rather than on the next poll.
  const tasksController = useTasks(client);
  const [appStage, setAppStage] = useState<AppStage>('loading');
  const [endpoints, setEndpoints] = useState<Endpoint[]>([]);
  const [activeEndpointId, setActiveEndpointId] = useState<string | null>(null);
  const [resources, setResources] = useState<Resource[]>([]);
  const [loadingResources, setLoadingResources] = useState(false);
  // Who the active endpoint's session authenticates as (lobby account menu).
  const [identity, setIdentity] = useState<UserIdentity | null>(null);
  const [query, setQuery] = useState('');
  // Live sessions are sourced from the RUNTIME (consoles open in their own OS
  // windows). listSessions() is the source of truth; the lobby's "live now"
  // section and the per-tile "already open" dot read from this.
  const [liveSessions, setLiveSessions] = useState<
    { id: string; resourceId: string; vmName?: string }[]
  >([]);
  // Reachability of endpoints the user has selected in the connection manager,
  // keyed by endpoint id. We probe ONLY the selected endpoint on demand (no
  // fan-out), so this fills in as the operator browses the list.
  const [endpointHealth, setEndpointHealth] = useState<Record<string, EndpointHealth>>({});
  const [probingEndpoints, setProbingEndpoints] = useState<Record<string, boolean>>({});
  // When set, the add/edit panel edits this existing endpoint in place rather
  // than creating a new one.
  const [editingEndpointId, setEditingEndpointId] = useState<string | null>(null);
  // True when sign-in was reached via an explicit Connect/Add — lets the sign-in
  // screen auto-open SSO. False for background session-expiry (no surprise popup).
  const [signInAuto, setSignInAuto] = useState(false);
  // Cluster Map view-state. Map is the default; List is the accessible fallback
  // (the lean EndpointPicker). Selection is shared across both views.
  const [clusterView, setClusterView] = useState<'map' | 'list'>('map');
  const [mapSelectedId, setMapSelectedId] = useState<string | null>(null);
  const [nodePositions, setNodePositionsState] = useState<Record<string, NodePosition>>(() =>
    getNodePositions(),
  );
  const [clusterLinks, setClusterLinks] = useState<ClusterLink[]>(() => getClusterLinks());
  const [activeFilter, setActiveFilter] = useState<FilterChip>('all');
  const [sortMode, setSortMode] = useState<SortMode>('name');
  const [favIds, setFavIds] = useState<Set<string>>(() => getFavoriteVmIds());
  // Applied-once guard for the default lobby filter (Starred-if-any-else-All).
  const initialFilterSetRef = useRef(false);
  // Resource IDs with an open() in flight — blocks the double-open race that
  // spawns two console windows for the same VM (see openConsole).
  const openingRef = useRef<Set<string>>(new Set());
  // App/runtime version for the connection-manager status bar (from the
  // handshake — non-private, already used elsewhere).
  const [appVersion, setAppVersion] = useState('');
  useEffect(() => {
    void window.ahvRemoteConsole.getRuntimeState().then((s) => {
      if (s.handshake?.version) setAppVersion(s.handshake.version);
    });
  }, []);
  // Powers the v2 LB.5 RECENT section. Hydrated from localStorage on
  // mount and refreshed after each openConsole call.
  const [recentOpens, setRecentOpens] = useState<RecentVMOpen[]>(() => getRecentVMOpens());
  // Powers the v2 LB.5 "updated Ns ago" timestamp. Set on each
  // refreshResources success; the rendered label re-derives on
  // each tick via relativeTime().
  const [resourcesUpdatedAt, setResourcesUpdatedAt] = useState<number | null>(null);
  // Forces a re-render of the timestamp every 5s. Tracks Date.now()
  // at the latest tick; relativeTime() reads it for "Ns ago" output.
  const [nowTick, setNowTick] = useState(Date.now());
  const authPluginId = 'nutanix-ahv';
  const activeEndpoint = useMemo(
    () => endpoints.find((e) => e.id === activeEndpointId) ?? endpoints[0] ?? null,
    [endpoints, activeEndpointId],
  );

  // Default the lobby filter to Starred when the cluster has any starred VM,
  // otherwise All. Applied once per endpoint, after its inventory first loads,
  // and never overrides a filter the operator subsequently chooses.
  useEffect(() => {
    initialFilterSetRef.current = false;
  }, [activeEndpointId]);
  useEffect(() => {
    if (initialFilterSetRef.current || resources.length === 0) return;
    initialFilterSetRef.current = true;
    setActiveFilter(resources.some((r) => favIds.has(r.id)) ? 'starred' : 'all');
  }, [resources, favIds]);
  const reloadEndpoints = useCallback(async () => {
    const res = await client.listEndpoints();
    setEndpoints(res.endpoints);
    return res.endpoints;
  }, [client]);

  // Pull the live session list from the runtime. The lobby is now the only
  // place that visualises live sessions (consoles live in their own windows),
  // so this is the canonical source. Errors are swallowed — we keep the last
  // known list rather than flickering the "live now" section on a transient
  // RPC failure.
  const refreshLiveSessions = useCallback(async () => {
    try {
      const { sessions } = await client.listSessions();
      setLiveSessions(
        sessions.map((s) => ({
          id: s.id,
          resourceId: s.resource_id,
          vmName: s.metadata?.vm_name,
        })),
      );
    } catch {
      /* keep last known live sessions */
    }
  }, [client]);

  // Probe one endpoint's reachability via the runtime's UNAUTHENTICATED ping
  // (no credentials sent, no body read — see plugin EndpointHealth). A failed
  // probe shows "offline" rather than hanging; concurrent calls for the same id
  // are de-duped via probingEndpoints.
  const probeEndpoint = useCallback(
    async (endpointId: string) => {
      setProbingEndpoints((prev) => (prev[endpointId] ? prev : { ...prev, [endpointId]: true }));
      try {
        const { health } = await client.getEndpointHealth(endpointId);
        setEndpointHealth((prev) => ({ ...prev, [endpointId]: health }));
      } catch {
        setEndpointHealth((prev) => ({
          ...prev,
          [endpointId]: { endpoint_id: endpointId, status: 'offline' },
        }));
      } finally {
        setProbingEndpoints((prev) => {
          const next = { ...prev };
          delete next[endpointId];
          return next;
        });
      }
    },
    [client],
  );

  // The cluster map shows a status dot per node, so it pings ALL endpoints.
  // This is safe to fan out: the probe is unauthenticated (host up/down +
  // latency only), so it leaks nothing and needs no sign-in.
  const probeAllEndpoints = useCallback(() => {
    for (const ep of endpoints) void probeEndpoint(ep.id);
  }, [endpoints, probeEndpoint]);

  // Any probe in flight drives the status-bar burst spin.
  const healthSyncing = useMemo(
    () => Object.values(probingEndpoints).some(Boolean),
    [probingEndpoints],
  );

  /* --- Reachability poll on the cluster screen (30s, visibility-gated) --- */
  useEffect(() => {
    if (appStage !== 'select-endpoint') return;
    probeAllEndpoints();
    const id = setInterval(() => {
      if (document.visibilityState !== 'visible') return;
      probeAllEndpoints();
    }, 30_000);
    return () => clearInterval(id);
  }, [appStage, probeAllEndpoints]);

  // Drop links that reference endpoints which no longer exist whenever the
  // endpoint list changes (e.g. after a connection is deleted).
  useEffect(() => {
    if (endpoints.length === 0) return;
    setClusterLinks(pruneClusterLinks(new Set(endpoints.map((e) => e.id))));
  }, [endpoints]);

  // Persist a node's new position (called on drag end / keyboard nudge).
  const moveNode = useCallback((id: string, p: NodePosition) => {
    setNodePositionsState((prev) => {
      const next = { ...prev, [id]: p };
      setNodePositions(next);
      return next;
    });
  }, []);

  const addLink = useCallback((a: string, b: string, label: string) => {
    setClusterLinks(addClusterLink(a, b, label));
  }, []);

  const removeLink = useCallback((id: string) => {
    setClusterLinks(removeClusterLink(id));
  }, []);

  // Delete a connection: removes it from the runtime config (clearing its saved
  // sign-in), then reloads. The pruneClusterLinks effect drops dangling links;
  // we also clear its health entry + selection. Routes to the empty-state add
  // flow if it was the last endpoint.
  const deleteEndpoint = useCallback(
    async (id: string) => {
      try {
        await client.deleteEndpoint(id);
        setEndpointHealth((prev) => {
          const next = { ...prev };
          delete next[id];
          return next;
        });
        setMapSelectedId((cur) => (cur === id ? null : cur));
        const eps = await reloadEndpoints();
        if (eps.length === 0) setAppStage('no-endpoints');
        // No success toast: the tile disappears from the connection manager.
      } catch (err) {
        toast.push('error', 'Failed to remove endpoint', (err as Error).message);
      }
    },
    [client, reloadEndpoints, toast],
  );

  useEffect(() => {
    void (async () => {
      try {
        const eps = await reloadEndpoints();
        if (eps.length === 0) {
          setAppStage('no-endpoints');
          return;
        }
        setAppStage('select-endpoint');
      } catch (err) {
        toast.push('error', 'Startup failed', (err as Error).message);
        setAppStage('no-endpoints');
      }
    })();
  }, []); // eslint-disable-line react-hooks/exhaustive-deps

  // Single VM-list loader for the whole flow. `silent` is the connect/sign-in
  // path (any failure just routes to sign-in); otherwise (lobby poll / manual
  // refresh) a non-auth error surfaces a toast. The endpoint id is passed
  // explicitly because the connect path sets activeEndpointId in the same tick.
  const loadResources = useCallback(
    async (endpointId: string, opts: { silent?: boolean } = {}) => {
      setLoadingResources(true);
      try {
        const result = await client.listResources(authPluginId, {
          q: query,
          page: 0,
          pageSize: 200,
        });
        const list = result.resources ?? [];
        setResources(list);
        setResourcesUpdatedAt(Date.now());
        setEndpointMeta(endpointId, { vmCount: list.length });
        setAppStage('ready');
      } catch (err) {
        const e = err as Error;
        const authError = /no credentials|credential|unauthorized|session expired|401|403/i.test(
          e.message,
        );
        if (opts.silent || authError) {
          if (!opts.silent) toast.push('warn', 'Session expired', 'Please sign in again.');
          setAppStage('sign-in');
        } else {
          toast.push('error', 'Failed to list VMs', e.message);
        }
      } finally {
        setLoadingResources(false);
      }
    },
    [client, query, toast],
  );

  // Lobby poll / manual refresh: reloads the active endpoint, surfacing errors.
  const refreshResources = useCallback(() => {
    if (!activeEndpoint) return;
    // A background/lobby reload that hits a 401 routes to sign-in — but must NOT
    // auto-open SSO (the operator didn't ask for it just now).
    setSignInAuto(false);
    void loadResources(activeEndpoint.id);
  }, [activeEndpoint, loadResources]);

  /* --- Seed live sessions once the lobby is ready --- */
  // The lobby's "live now" section reflects sessions still alive in the
  // runtime (e.g. console windows left open across a main-window reload).
  useEffect(() => {
    if (appStage !== 'ready') return;
    void refreshLiveSessions();
  }, [appStage, refreshLiveSessions]);

  /* --- Who's signed in: fetched once per lobby entry / endpoint switch --- */
  // Drives the account-menu avatar (user initials, not cluster initials). A
  // fetch failure just leaves identity null — the avatar falls back to the
  // endpoint initials, and sign-in routing stays owned by loadResources.
  useEffect(() => {
    if (appStage !== 'ready' || !activeEndpointId) {
      setIdentity(null);
      return;
    }
    let cancelled = false;
    client
      .whoAmI(authPluginId, activeEndpointId)
      .then((res) => {
        if (!cancelled) setIdentity(res.identity ?? null);
      })
      .catch(() => {
        if (!cancelled) setIdentity(null);
      });
    return () => {
      cancelled = true;
    };
  }, [appStage, activeEndpointId, client, authPluginId]);

  /* --- "updated Ns ago" label tick (5s cadence) --- */
  // Drives the LB.5 top-bar timestamp. 5s strikes a balance between
  // accuracy ("12s ago") and re-render churn (300 ticks/hour vs 3600
  // for 1s). The label re-derives in relativeTime() — no setState
  // chain past nowTick.
  useEffect(() => {
    if (appStage !== 'ready') return;
    const id = setInterval(() => setNowTick(Date.now()), 5_000);
    return () => clearInterval(id);
  }, [appStage]);

  /* --- Auto-refresh resources every 30s while on Lobby screen --- */
  // Was 10s, but each refresh triggers a full listResources RPC + a
  // grid re-render with motion components. Bump to 30s — the operator
  // sees fresh data on tab switches and explicit refresh anyway.
  // Visibility-gated: don't poll when the window isn't visible.
  useEffect(() => {
    if (appStage !== 'ready') return;
    const id = setInterval(() => {
      if (document.visibilityState !== 'visible') return;
      // The main window is always the lobby now (consoles live in their own
      // OS windows), so we always refresh both resources and live sessions.
      void refreshResources();
      void refreshLiveSessions();
    }, 30_000);
    return () => clearInterval(id);
  }, [appStage, refreshResources, refreshLiveSessions]);

  /* --- Re-fetch when the search query changes (debounced) --- */
  // The server filters by q.Search (plugins/ahv ListResources), so `resources`
  // must be re-fetched whenever the query changes. Without this, `resources`
  // stayed narrowed to a PREVIOUS query (set by the 30s poll or a manual
  // refresh), and the client-side filteredResources memo could only narrow that
  // stale set further — so changing or CLEARING the filter did nothing until a
  // manual refresh re-fetched (the "filter works only once" bug). loadResources
  // is recreated with the current query, so keying on `query` captures it fresh;
  // debounced so each keystroke isn't a Prism RPC.
  useEffect(() => {
    if (appStage !== 'ready' || !activeEndpointId) return;
    const id = setTimeout(() => {
      // A search-driven reload that hits a 401 routes to sign-in but must not
      // auto-open SSO (mirrors refreshResources).
      setSignInAuto(false);
      void loadResources(activeEndpointId);
    }, 300);
    return () => clearTimeout(id);
    // Keyed on the search query; loadResources closes over the current query and
    // is intentionally NOT a dep (its identity churns with query, which we track
    // here), so it isn't re-fired by unrelated identity changes.
  }, [query, activeEndpointId, appStage]); // eslint-disable-line react-hooks/exhaustive-deps

  /* --- Filtering + sorting pipeline --- */
  // Resources with a live session, used to flag "already open" VMs in the
  // grid. Sourced from the runtime's session list.
  const tabResourceIds = useMemo(
    () => new Set(liveSessions.map((s) => s.resourceId)),
    [liveSessions],
  );

  const filteredResources = useMemo(() => {
    const q = query.trim().toLowerCase();
    let list = resources;
    // Text search over the OPERATOR-VISIBLE fields only — name, IP, cluster.
    // We deliberately do NOT match r.id: it is "<endpointId>/<vmUUID>", and a
    // 32-char hex UUID contains almost any short substring (e.g. "43"), so
    // including it made a query match a flood of unrelated VMs by their hidden
    // UUID — the reported "search is buggy". The id is never shown, so it is
    // never what the operator is typing.
    if (q)
      list = list.filter(
        (r) =>
          r.label.toLowerCase().includes(q) ||
          (r.cluster?.toLowerCase().includes(q) ?? false) ||
          (r.ip?.toLowerCase().includes(q) ?? false),
      );
    // chip filter
    if (activeFilter === 'on') list = list.filter((r) => r.status?.toLowerCase() === 'on');
    else if (activeFilter === 'off') list = list.filter((r) => r.status?.toLowerCase() === 'off');
    else if (activeFilter === 'starred') list = list.filter((r) => favIds.has(r.id));
    else if (activeFilter === 'recent') {
      const ids = getRecentVmIds();
      const idSet = new Set(ids);
      list = list.filter((r) => idSet.has(r.id));
    }
    // sort
    const recentIds = getRecentVmIds();
    const recentIndex = (id: string) => {
      const i = recentIds.indexOf(id);
      return i === -1 ? Infinity : i;
    };
    list = [...list].sort((a, b) => {
      if (sortMode === 'name') return a.label.localeCompare(b.label);
      if (sortMode === 'power state') {
        const aOn = a.status?.toLowerCase() === 'on' ? 0 : 1;
        const bOn = b.status?.toLowerCase() === 'on' ? 0 : 1;
        return aOn - bOn || a.label.localeCompare(b.label);
      }
      /* recent */ return recentIndex(a.id) - recentIndex(b.id);
    });
    return list;
  }, [resources, query, activeFilter, sortMode, favIds]);

  const openConsole = useCallback(
    async (resource: Resource) => {
      // Double-open guard: rapid clicks (or a click before the first
      // openSession resolves) would each create a NEW session for the same VM
      // and spawn a second window — the main-process de-dupe only catches the
      // SAME sessionId, and `liveSessions` (React state) updates too late to
      // block a concurrent call. The synchronous ref blocks it reliably.
      if (openingRef.current.has(resource.id)) return;
      openingRef.current.add(resource.id);
      try {
        pushRecentVmId(resource.id);
        // RECENT section (LB.5) uses the structured store: { id, vmName,
        // epId, openedAt }. Legacy pushRecentVmId stays for the filter
        // chip; this new record powers the top-of-lobby RECENT row.
        if (activeEndpoint) {
          recordVMOpen(resource.id, resource.label, activeEndpoint.id);
          recordEndpointConnection(activeEndpoint.id);
          setRecentOpens(getRecentVMOpens());
        }
        // Consoles open in their own OS window. If this resource already has a
        // live session, just focus its window (openConsoleWindow de-dupes by
        // sessionId). Otherwise create a session WITHOUT a WebSocket — the
        // console window opens its own stream — then spawn the window.
        const existing = liveSessions.find((s) => s.resourceId === resource.id);
        if (existing) {
          // `liveSessions` (React state) is only reconciled on a 30s poll, so it
          // can be stale: closing a console window DELETEs its session
          // server-side, and a quick reopen against that dead id would 404 before
          // the WS upgrade and dead-end on the error overlay (connectedRef never
          // latches → no self-heal). Confirm the session is still live before
          // reusing; otherwise drop the stale entry and fall through to mint a
          // fresh one.
          let stillLive = false;
          try {
            const { sessions } = await client.listSessions();
            stillLive = sessions.some((s) => s.id === existing.id);
          } catch {
            /* couldn't confirm — don't trust the stale cache; mint fresh below */
          }
          if (stillLive) {
            await window.ahvRemoteConsole.openConsoleWindow(existing.id, resource.label);
            return;
          }
          setLiveSessions((prev) => prev.filter((s) => s.id !== existing.id));
        }
        const open = await client.openSession(authPluginId, resource.id);
        await window.ahvRemoteConsole.openConsoleWindow(open.session.id, resource.label);
        // Optimistically reflect the new session in the lobby right away;
        // the 30s poll reconciles afterwards.
        setLiveSessions((prev) =>
          prev.some((s) => s.id === open.session.id)
            ? prev
            : [...prev, { id: open.session.id, resourceId: resource.id, vmName: resource.label }],
        );
        void refreshLiveSessions();
      } catch (err) {
        toast.push('error', `Failed to open ${resource.label}`, (err as Error).message);
      } finally {
        openingRef.current.delete(resource.id);
      }
    },
    [client, liveSessions, activeEndpoint, toast, refreshLiveSessions],
  );

  // Power a VM from the lobby. Resource-based (no session), so it works on a
  // powered-off VM. The lobby's status comes from cached inventory and won't
  // flip instantly, so the tile drives an optimistic transitioning state and we
  // reconcile here: re-probe the endpoint (refreshes vm_power evidence — a Prism
  // 403 flips the capability to 'denied' and hides the control thereafter) and
  // re-fetch resources after a short settle so the On/Off dot catches up. ACPI
  // verbs are graceful, so the guest may lag the toast — don't imply it's done.
  const powerVm = useCallback(
    async (resourceId: string, action: PowerActionKind) => {
      // The power op registers a task the moment the runtime starts it — bump
      // the tray poller so the running task surfaces without waiting for the
      // next interval tick.
      tasksController.refresh();
      try {
        await client.vmPowerAction(authPluginId, resourceId, POWER_SDK_ACTION[action]);
        // No success toast: the lobby TasksTray shows the running power task and
        // the tile flips state; over-toasting "sent" implied completion early.
      } catch (err) {
        toast.push('error', 'Power action failed', (err as Error).message);
      } finally {
        if (activeEndpointId) void probeEndpoint(activeEndpointId);
        // Cached status lags the action (the guest takes time + the inventory
        // poll is periodic); refresh after a short settle to catch the flip.
        setTimeout(() => void refreshResources(), 4000);
        // The op has now reached a terminal state — re-poll so the tray flips
        // the task to succeeded/failed.
        tasksController.refresh();
      }
    },
    [client, toast, probeEndpoint, activeEndpointId, refreshResources, tasksController],
  );

  // Log off the current cluster (invalidate the Prism session server-side) and
  // return to the connection manager. "Switch cluster" (no sign-out) just
  // navigates back to select-endpoint; this additionally drops the session.
  const signOut = useCallback(async () => {
    try {
      await client.logout(authPluginId);
    } catch (err) {
      toast.push('error', 'Sign out failed', (err as Error).message);
    }
    // Close any open console pop-outs: logout wiped every endpoint's credentials,
    // so they'd otherwise linger on dead cookies until their next re-dial (#7).
    await window.ahvRemoteConsole.closeConsoleWindows().catch(() => {
      /* best-effort — pop-outs also fail closed on their next re-dial */
    });
    setLiveSessions([]);
    setAppStage('select-endpoint');
  }, [client, toast]);

  // ──────────── v2 route-level transitions ────────────
  // Derive the current Route from appStage, track navigation direction across
  // changes, pick the variant set per pair. Single <AnimatePresence
  // mode="wait"> wrapped in <LayoutGroup> so T4 layoutId morphs (tile ↔ console
  // viewport) work across the route swap.
  const currentRoute: Route = useMemo(() => {
    if (appStage === 'loading') return 'splash';
    if (appStage === 'no-endpoints') return 'no-endpoints';
    if (appStage === 'add-endpoint') return 'add-endpoint';
    if (appStage === 'select-endpoint') return 'endpoint';
    if (appStage === 'sign-in') return 'signin';
    return 'lobby';
  }, [appStage]);

  const prevRouteRef = useRef<Route>(currentRoute);
  const [direction, setDirection] = useState<Direction>('forward');
  useEffect(() => {
    const prev = prevRouteRef.current;
    if (prev !== currentRoute) {
      setDirection(inferDirection(prev, currentRoute));
      prevRouteRef.current = currentRoute;
    }
  }, [currentRoute]);

  // T1 fade-through ONLY for splash → signin (and hard auth resets).
  // Everything else: sharedAxisX with the inferred direction.
  const routeVariants = useMemo(() => {
    const prev = prevRouteRef.current;
    if (prev === 'splash' && currentRoute === 'signin') return fadeThroughVariants;
    return sharedAxisX(direction);
  }, [currentRoute, direction]);

  // Each branch assigns to `content` instead of early-returning so
  // they all flow into the single <AnimatePresence> at the bottom of
  // this function.
  let content: React.ReactNode = null;
  if (appStage === 'loading') {
    content = (
      <div
        style={{
          height: '100%',
          display: 'flex',
          alignItems: 'center',
          justifyContent: 'center',
          background: C.paper,
        }}
      >
        <Spinner label="Loading endpoints..." />
      </div>
    );
  } else if (appStage === 'no-endpoints' || appStage === 'add-endpoint') {
    const editing = editingEndpointId
      ? (endpoints.find((e) => e.id === editingEndpointId) ?? undefined)
      : undefined;
    content = (
      <AddEndpointPanel
        pluginId={authPluginId}
        endpoint={editing}
        onCancel={
          appStage === 'add-endpoint' && endpoints.length > 0
            ? () => {
                setEditingEndpointId(null);
                setAppStage('select-endpoint');
              }
            : undefined
        }
        onSaved={(id) => {
          void (async () => {
            await reloadEndpoints();
            if (editing) {
              // Edited in place — return to the connection manager and refresh
              // its reachability dot (the URL/TLS may have changed).
              setEditingEndpointId(null);
              setActiveEndpointId(id);
              setAppStage('select-endpoint');
              void probeEndpoint(id);
            } else {
              // Just added → explicit intent, so auto-open SSO on the sign-in screen.
              setActiveEndpointId(id);
              setSignInAuto(true);
              setAppStage('sign-in');
            }
          })();
        }}
      />
    );
  } else if (appStage === 'select-endpoint') {
    // Shared handlers across the Map and List views.
    const connectEndpoint = (id: string) => {
      setActiveEndpointId(id);
      // Explicit Connect → if it routes to sign-in, jump straight to the SSO popup.
      setSignInAuto(true);
      void loadResources(id, { silent: true });
    };
    const startAddEndpoint = () => {
      setEditingEndpointId(null);
      setAppStage('add-endpoint');
    };
    const startEditEndpoint = (id: string) => {
      setEditingEndpointId(id);
      setAppStage('add-endpoint');
    };
    const exportDiagnostics = () => {
      void (async () => {
        try {
          const path = await window.ahvRemoteConsole.saveDiagnostics();
          if (path) toast.push('success', 'Diagnostics exported', path);
        } catch (err) {
          toast.push('error', 'Diagnostics export failed', (err as Error).message);
        }
      })();
    };
    content =
      clusterView === 'map' ? (
        <ClusterMap
          endpoints={endpoints}
          health={endpointHealth}
          probing={probingEndpoints}
          syncing={healthSyncing}
          positions={nodePositions}
          links={clusterLinks}
          selectedId={mapSelectedId}
          onSelectId={setMapSelectedId}
          onConnect={connectEndpoint}
          onAddNew={startAddEndpoint}
          onEdit={startEditEndpoint}
          onDiagnostics={exportDiagnostics}
          onDelete={(id) => void deleteEndpoint(id)}
          onProbe={(id) => void probeEndpoint(id)}
          onMoveNode={moveNode}
          onAddLink={addLink}
          onRemoveLink={removeLink}
          onSetView={setClusterView}
        />
      ) : (
        <EndpointPicker
          endpoints={endpoints}
          health={endpointHealth}
          probing={probingEndpoints}
          selectedId={mapSelectedId}
          onSelectId={setMapSelectedId}
          onProbe={(id) => void probeEndpoint(id)}
          onSelect={connectEndpoint}
          onAddNew={startAddEndpoint}
          onEdit={startEditEndpoint}
          onDiagnostics={exportDiagnostics}
          onDelete={(id) => void deleteEndpoint(id)}
          onSetView={setClusterView}
          version={appVersion}
        />
      );
  } else if (appStage === 'sign-in') {
    const ep = activeEndpoint ?? endpoints[0];
    if (!ep) {
      content = (
        <AddEndpointPanel
          pluginId={authPluginId}
          onSaved={(id) => {
            void (async () => {
              await reloadEndpoints();
              setActiveEndpointId(id);
              setSignInAuto(true);
              setAppStage('sign-in');
            })();
          }}
        />
      );
    } else {
      if (activeEndpointId !== ep.id) setActiveEndpointId(ep.id);
      content = (
        <SignInPanel
          endpoint={ep}
          autoStart={signInAuto}
          onBack={() => setAppStage('select-endpoint')}
          onSignedIn={() => {
            // Post-auth retry must not re-pop SSO if resources still fail.
            setSignInAuto(false);
            setTimeout(() => {
              void loadResources(ep.id, { silent: true });
            }, 500);
          }}
        />
      );
    }
  }

  content ??= (
    <Lobby
      activeEndpoint={activeEndpoint}
      identity={identity}
      resources={resources}
      filteredResources={filteredResources}
      liveSessions={liveSessions}
      recentOpens={recentOpens}
      tabResourceIds={tabResourceIds}
      favIds={favIds}
      loadingResources={loadingResources}
      resourcesUpdatedAt={resourcesUpdatedAt}
      nowTick={nowTick}
      query={query}
      activeFilter={activeFilter}
      sortMode={sortMode}
      onQueryChange={setQuery}
      onFilterChange={setActiveFilter}
      onSortChange={setSortMode}
      onToggleStar={(id) => setFavIds(toggleFavoriteVmId(id))}
      onRefresh={() => void refreshResources()}
      onOpenConsole={(r) => void openConsole(r)}
      onJoin={(sessionId, label) =>
        void window.ahvRemoteConsole.openConsoleWindow(sessionId, label)
      }
      onSwitchCluster={() => setAppStage('select-endpoint')}
      onSignOut={() => void signOut()}
      powerCapability={
        (activeEndpointId ? endpointHealth[activeEndpointId]?.capabilities : null) ?? null
      }
      onPowerAction={(resourceId, action) => powerVm(resourceId, action)}
    />
  );

  // Single AnimatePresence wraps every route. LayoutGroup tracks
  // layout state across the route swap so T4 layoutId morphs (when
  // wired on VMTile + ConsoleView) animate from tile to viewport
  // rather than snapping. mode="wait" guarantees only one route at a
  // time — no overlap, no flicker.
  return (
    <LayoutGroup>
      <AnimatePresence mode="wait" custom={direction}>
        <m.div
          key={currentRoute}
          variants={routeVariants}
          initial="initial"
          animate="animate"
          exit="exit"
          style={{ height: '100%', width: '100%' }}
        >
          {content}
        </m.div>
      </AnimatePresence>
      {/* Always-on Tasks tray: long-running power / serial / ISO ops stay
          visible regardless of which route is active. Idle renders nothing. */}
      <TasksTray client={client} controller={tasksController} />
    </LayoutGroup>
  );
}

// EndpointPicker is imported from ../components/EndpointPicker.tsx
