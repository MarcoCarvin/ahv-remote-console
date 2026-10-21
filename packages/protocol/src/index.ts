/**
 * @ahv-remote-console/protocol — wire-format types shared between the Electron
 * desktop shell and the Go runtime. Keep this package free of runtime
 * dependencies: it must be safe to import from the renderer sandbox.
 */

export const PROTOCOL_VERSION = 1 as const;

// -------- Runtime handshake (file written by ahv-remote-console-runtime) --------

export interface RuntimeHandshake {
  version: string;
  pid: number;
  endpoint: string;
  /** base64url(sha256(ipc_secret)). The renderer verifies it has the right secret. */
  secret_hash: string;
  started_at: string;
}

// -------- Plugin metadata --------

export interface PluginManifest {
  id: string;
  name: string;
  version: string;
  description: string;
  vendor: string;
  capabilities: string[];
}

// -------- Endpoints (Prism Central / equivalent) --------

/**
 * Authentication methods an endpoint may enable. v1 ships exactly two:
 *
 *   * 'basic'  — username + password. Prism resolves whether the account is
 *                local, AD/LDAP, or directory-backed.
 *   * 'cookie' — embedded SSO. The desktop shell hosts Prism's /console/
 *                login in a sandboxed BrowserWindow, the user authenticates
 *                with whatever IdP Prism is wired to, and the captured
 *                session cookies are replayed on every API call.
 */
export type AuthKind = 'basic' | 'cookie';

export interface SessionCookie {
  name: string;
  value: string;
}

export interface Endpoint {
  id: string;
  label: string;
  plugin_id: string;
  url: string;
  auth_methods: AuthKind[];
  has_pinned_cert: boolean;
  /**
   * The cluster's pinned certificate (base64 DER), when configured. This is
   * the server's PUBLIC TLS cert (no secret), surfaced so the desktop shell
   * can pin the embedded SSO login window to the same cert the runtime
   * trusts. Absent when no pin is configured.
   */
  pinned_cert_b64?: string;
  allow_insecure_tls: boolean;
  tags?: Record<string, string>;
}

export interface UpsertEndpointRequest {
  id?: string;
  label: string;
  plugin_id: string;
  url: string;
  auth_methods: AuthKind[];
  pinned_cert_b64?: string;
  allow_insecure_tls?: boolean;
  tags?: Record<string, string>;
}

// -------- Resources (VMs etc) --------

export interface Resource {
  id: string;
  label: string;
  status: string;
  description?: string;
  cluster?: string;
  ip?: string;
  tags?: Record<string, string>;
}

export interface ListResourcesResponse {
  resources: Resource[];
  total: number;
  page: number;
  page_size: number;
}

// -------- Identity --------

/**
 * The user an endpoint's stored session authenticates as, served by the
 * runtime at GET /v1/plugins/{id}/whoami?endpoint_id=. Backs the lobby
 * account menu (avatar initials + signed-in-as line).
 */
export interface UserIdentity {
  username: string;
  display_name?: string;
}

export interface WhoAmIResponse {
  identity: UserIdentity;
}

// -------- Sessions --------

export interface SessionInfo {
  id: string;
  plugin_id: string;
  resource_id: string;
  opened_at: string;
  capabilities?: string[];
  metadata?: Record<string, string>;
}

export interface OpenSessionRequest {
  plugin_id: string;
  resource_id: string;
}

export interface OpenSessionResponse {
  session: SessionInfo;
}

// -------- Arbitrary catalog ISO mount / eject --------

/**
 * One mountable Image-Service ISO offered by the catalog picker (served by the
 * runtime at GET /v1/agent/images?session_id=…). Minimal — the picker lists by
 * `name` and mounts by `uuid`. Field names are snake_case on the wire to match
 * the Go json tags; the SDK maps them to camelCase.
 */
export interface CatalogISOImage {
  uuid: string;
  name: string;
  state: string;
  description?: string;
  sizeBytes?: number;
}

export interface ListISOImagesResponse {
  images: CatalogISOImage[];
}

/**
 * Response from POST /v1/vm/mount-iso. Reuses the agent mount wire shape:
 * the image UUID that was attached, a best-guess guest drive letter, and a
 * `reused` flag (the image was already in that CD-ROM, so it was a no-op).
 */
export interface MountISOResponse {
  image_uuid: string;
  drive_letter_hint: string;
  reused: boolean;
}

/** Response from POST /v1/vm/eject-iso. `ejected` is false on an idempotent
 *  no-op (the drive was already empty / there is no CD-ROM device). */
export interface EjectISOResponse {
  ejected: boolean;
}

// -------- VM snapshots --------

/**
 * One VM recovery point ("snapshot") surfaced to the console Snapshots panel.
 * v3 models snapshots as recovery points; fields reflect what they provide.
 */
export interface SnapshotInfo {
  uuid: string;
  name: string;
  /** Upstream creation timestamp (RFC3339-ish); the renderer formats it. */
  created_at?: string;
  /** Garbage-collection time; absent when the recovery point never expires. */
  expires_at?: string;
  /** Recovery-point lifecycle state (e.g. COMPLETE). */
  state?: string;
  /** recovery_point_type: CRASH_CONSISTENT / APPLICATION_CONSISTENT / LIVE. */
  snapshot_type?: string;
}

/** Body of POST /v1/vm/snapshots — create a named snapshot. */
export interface CreateSnapshotRequest {
  plugin_id: string;
  resource_id: string;
  name: string;
}

/**
 * Body of POST /v1/vm/snapshots/restore — restore from a recovery point.
 * On v3 this CREATES A NEW VM from the recovery point; it is NOT a guaranteed
 * in-place revert of the source VM. The renderer MUST obtain explicit operator
 * confirmation before issuing this; neither the runtime nor the plugin prompts.
 */
export interface RestoreSnapshotRequest {
  plugin_id: string;
  resource_id: string;
  snapshot_id: string;
}

/** Response from GET /v1/vm/snapshots. */
export interface ListSnapshotsResponse {
  snapshots: SnapshotInfo[];
}

// -------- Endpoint health --------

/**
 * Reachability of a single endpoint, served by the runtime at
 * GET /v1/config/endpoints/{id}/health. An unauthenticated ping (no credentials
 * sent, no body read) backs the cluster-map status dot — it carries no cluster
 * detail, so probing every node leaks nothing.
 *
 *   'healthy' — the host answered (reachable).
 *   'offline' — the reachability probe failed (network / DNS / TLS).
 */
export type EndpointHealthStatus = 'healthy' | 'offline';

/**
 * What the signed-in Prism role is known to be able to do. This mirrors
 * EVIDENCE, not policy: 'denied' is only ever set after a read-only probe
 * proved it or a real Prism 403 was observed; successes set 'allowed'.
 * 'unknown' is NOT denied — the UI shows controls normally while unknown
 * and only degrades on proven denials.
 */
export type CapabilityState = 'unknown' | 'allowed' | 'denied';

export interface EndpointCapabilities {
  image_read: CapabilityState;
  image_create: CapabilityState;
  vm_update: CapabilityState;
  vm_power: CapabilityState;
  /** The agent installer ISO is already published in the cluster's image
   *  catalog — when true, image_create is not required for the full flow. */
  agent_iso_in_catalog: boolean;
}

export interface EndpointHealth {
  endpoint_id: string;
  status: EndpointHealthStatus;
  /** Reachability round-trip latency in ms (absent when offline). */
  rtt_ms?: number;
  /**
   * Per-endpoint permission evidence for the signed-in role (probed once per
   * auth epoch + accumulated 403/success evidence, cleared on re-auth).
   * Absent until the runtime has anything to report — treat as all-unknown.
   */
  capabilities?: EndpointCapabilities;
}

// -------- Live VM stats (console stats panel) --------

/**
 * One point-in-time VM utilization sample, served by GET /v1/vm/stats and
 * rendered in the console stats panel. Read-only — reading a sample mutates
 * nothing.
 *
 * Units:
 *   - cpu_usage_ppm / memory_usage_ppm are parts-per-million (ppm); divide by
 *     10000 to get a percentage.
 *   - disk_*_bps / net_*_bps are bytes per second.
 *
 * Any numeric field equal to `-1` is the UNAVAILABLE sentinel: the upstream did
 * not report that metric for this sample. The renderer shows it as 'n/a' rather
 * than a misleading 0.
 */
export interface VMStats {
  cpu_usage_ppm: number;
  memory_usage_ppm: number;
  disk_read_bps: number;
  disk_write_bps: number;
  net_rx_bps: number;
  net_tx_bps: number;
  /** ISO-8601 timestamp of when the runtime read the sample (host clock). */
  sampled_at: string;
}

export interface VMStatsResponse {
  stats: VMStats;
}

// -------- Long-running tasks (Tasks tray) --------

/**
 * Lifecycle state of a long-running task surfaced in the Tasks tray.
 *   'running'   — in flight (percent_complete drives the bar).
 *   'succeeded' — finished OK.
 *   'failed'    — finished with an error (see `error`).
 */
export type TaskState = 'running' | 'succeeded' | 'failed';

/**
 * One long-running operation (power action, serial-port add, agent ISO install)
 * shown in the Tasks tray. Served by GET /v1/tasks and GET /v1/tasks/{id}.
 *
 * COMPLIANCE: `label`, `detail`, and `error` are operator-facing strings only —
 * they NEVER carry clipboard plaintext or credentials. `label` is host-composed
 * ('Power OFF <vm>'); `error` is the upstream task's own operator-facing failure
 * detail; the agent-ISO task carries byte length / phase text only.
 */
export interface TaskInfo {
  id: string;
  plugin_id: string;
  /** Task taxonomy: 'vm_power' | 'serial_port' | 'agent_iso'. */
  kind: string;
  label: string;
  resource_id?: string;
  session_id?: string;
  state: TaskState;
  percent_complete: number;
  /** Operator-facing current-step text (e.g. 'Attached the installer CD-ROM'). */
  detail?: string;
  /** Terminal failure reason (never payload/credential data). */
  error?: string;
  started_at: string;
  updated_at: string;
}

export interface ListTasksResponse {
  tasks: TaskInfo[];
}

export interface GetTaskResponse {
  task: TaskInfo;
}

// -------- WebSocket control frames (JSON over text frames) --------

export type ControlFrame =
  | { type: 'connected' }
  | { type: 'heartbeat'; bytes_in?: number; bytes_out?: number; latency_ms?: number }
  | { type: 'reconnecting'; reason?: string }
  | { type: 'reconnect_attempt'; reason?: string }
  | { type: 'resumed' }
  | { type: 'error'; reason?: string }
  | { type: 'pong' }
  | { type: 'clipboard_ready'; version: 'proxy-mediated' | 'legacy' };

// -------- Client → runtime control frames over the same channel --------

export type ClientControlFrame =
  | { type: 'ping' }
  | { type: 'clipboard'; mime: 'text/plain'; data: string }
  | { type: 'paste_as_typing'; layout?: string; data: string }
  | { type: 'resize'; width: number; height: number };

// -------- Presence --------

export type PresenceRole = 'controller' | 'viewer';

export interface PresencePeer {
  identity: { session_id: string; user_id: string; vm_uuid: string; runtime_instance_id: string };
  name: string;
  color: string;
  role: PresenceRole;
  cursor: { x: number; y: number };
  viewport: { w: number; h: number };
  joined_at: string;
  last_seen: string;
}

export type PresenceMessageType =
  | 'presence.join'
  | 'presence.leave'
  | 'presence.heartbeat'
  | 'presence.cursor'
  | 'presence.role_change'
  | 'presence.room_state'
  | 'presence.chat'
  | 'presence.chat_history';

export interface PresenceMessage {
  type: PresenceMessageType;
  session_id: string;
  vm_uuid?: string;
  payload?: unknown;
}
export interface PresenceJoinPayload {
  name: string;
  color: string;
  role: PresenceRole;
  viewport: { w: number; h: number };
  identity: PresencePeer['identity'];
}
export interface PresenceRoomStatePayload {
  peers: PresencePeer[];
}
export interface PresenceCursorPayload {
  x: number;
  y: number;
}
export interface PresenceRoleChangePayload {
  role: PresenceRole;
  from?: string;
}
export interface PresenceChatPayload {
  text: string;
  ts: number;
  name?: string;
  color?: string;
}
export interface PresenceChatMessage {
  session_id: string;
  name: string;
  color: string;
  text: string;
  ts: number;
}
export interface PresenceChatHistoryPayload {
  messages: PresenceChatMessage[];
}

// -------- Errors --------

export interface APIError {
  error: {
    code: string;
    message: string;
  };
}

// -------- Network state --------

export interface NetworkInterface {
  name: string;
  mac?: string;
  up: boolean;
  loopback: boolean;
  addresses: string[];
}

export interface NetworkSnapshot {
  captured_at: string;
  interfaces: NetworkInterface[];
}

// -------- Runtime state --------

export interface RuntimeState {
  version: string;
  plugins: PluginManifest[];
  network: NetworkSnapshot;
  observability: {
    traces_enabled: boolean;
  };
}

// -------- IPC headers --------

export const IPC_HEADER_SIG = 'X-HC-Sig';
export const IPC_HEADER_TS = 'X-HC-Timestamp';
export const IPC_HEADER_NONCE = 'X-HC-Nonce';

/**
 * Canonical request layout for HMAC verification. Order must match the runtime.
 * METHOD\nURI\nTIMESTAMP\nNONCE\nBODY_SHA256_HEX
 */
export function canonicalRequest(args: {
  method: string;
  uri: string;
  timestamp: string;
  nonce: string;
  bodyHashHex: string;
}): string {
  return [args.method, args.uri, args.timestamp, args.nonce, args.bodyHashHex].join('\n');
}
