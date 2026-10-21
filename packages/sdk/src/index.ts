/**
 * @ahv-remote-console/sdk — typed client for the local Go runtime, used by the
 * Electron main process and (via the contextBridge) the renderer. Every call
 * is HMAC-signed and goes over loopback HTTP/WS.
 *
 * The client never sees the IPC secret directly. Callers inject a Signer
 * (typically the contextBridge-exposed `window.ahvRemoteConsole.signRequest` /
 * `signWebSocket` in the renderer, or the main-process Signer instance
 * itself for code running in main). This keeps the secret inside one
 * process boundary.
 */

import type {
  CapabilityState,
  CatalogISOImage,
  ControlFrame,
  ClientControlFrame,
  Endpoint,
  EndpointCapabilities,
  EndpointHealth,
  GetTaskResponse,
  ListResourcesResponse,
  ListTasksResponse,
  OpenSessionResponse,
  PluginManifest,
  ListSnapshotsResponse,
  RuntimeState,
  SessionInfo,
  SnapshotInfo,
  TaskInfo,
  TaskState,
  UpsertEndpointRequest,
  VMStats,
  VMStatsResponse,
  WhoAmIResponse,
} from '@ahv-remote-console/protocol';

/**
 * Signer is the bridge that asks main to sign a runtime-bound request.
 * In the renderer it's `window.ahvRemoteConsole.signRequest` /
 * `signWebSocket`; in main it's a direct call against the Signer
 * instance.
 */
export interface Signer {
  signRequest(
    method: string,
    uri: string,
    body: string | null,
  ): Promise<Record<string, string>> | Record<string, string>;
  signWebSocket(uri: string): Promise<string> | string;
}

export interface ClientOptions {
  endpoint: string;
  signer: Signer;
}

export class AHVRemoteConsoleClient {
  constructor(private readonly opts: ClientOptions) {}

  // --------------- typed REST ---------------

  async getRuntimeState(): Promise<RuntimeState> {
    return this.json<RuntimeState>('GET', '/v1/runtime/state');
  }

  async listPlugins(): Promise<{ plugins: PluginManifest[] }> {
    return this.json('GET', '/v1/plugins');
  }

  // --------------- endpoint management ---------------

  async listEndpoints(): Promise<{ endpoints: Endpoint[] }> {
    return this.json('GET', '/v1/config/endpoints');
  }

  /**
   * Coarse reachability of a single endpoint, for the connection manager's
   * status dot. Probes only this endpoint (no fan-out) and returns just a
   * status — endpoints with no stored credentials (or that the cluster
   * was never contacted for) come back as `status: 'offline'`.
   */
  async getEndpointHealth(endpointId: string): Promise<{ health: EndpointHealth }> {
    return this.json('GET', `/v1/config/endpoints/${encodeURIComponent(endpointId)}/health`);
  }

  async upsertEndpoint(body: UpsertEndpointRequest): Promise<{ endpoint: Endpoint }> {
    return this.json('POST', '/v1/config/endpoints', body);
  }

  async deleteEndpoint(id: string): Promise<void> {
    await this.fetch('DELETE', `/v1/config/endpoints/${encodeURIComponent(id)}`, null);
  }

  async startAuth(pluginId: string, body: unknown): Promise<unknown> {
    return this.json('POST', `/v1/plugins/${encodeURIComponent(pluginId)}/auth/start`, body);
  }

  async completeAuth(pluginId: string, body: unknown): Promise<unknown> {
    return this.json('POST', `/v1/plugins/${encodeURIComponent(pluginId)}/auth/complete`, body);
  }

  async refreshAuth(pluginId: string): Promise<void> {
    await this.json('POST', `/v1/plugins/${encodeURIComponent(pluginId)}/auth/refresh`, {});
  }

  async logout(pluginId: string): Promise<void> {
    await this.fetch('POST', `/v1/plugins/${encodeURIComponent(pluginId)}/auth/logout`, null);
  }

  /** The user the endpoint's stored session authenticates as (lobby account menu). */
  async whoAmI(pluginId: string, endpointId: string): Promise<WhoAmIResponse> {
    return this.json(
      'GET',
      `/v1/plugins/${encodeURIComponent(pluginId)}/whoami?endpoint_id=${encodeURIComponent(endpointId)}`,
    );
  }

  async listResources(
    pluginId: string,
    params: { q?: string; page?: number; pageSize?: number } = {},
  ): Promise<ListResourcesResponse> {
    const qs = new URLSearchParams();
    if (params.q) qs.set('q', params.q);
    if (params.page !== undefined) qs.set('page', String(params.page));
    if (params.pageSize !== undefined) qs.set('page_size', String(params.pageSize));
    const suffix = qs.toString() ? `?${qs.toString()}` : '';
    return this.json('GET', `/v1/plugins/${encodeURIComponent(pluginId)}/resources${suffix}`);
  }

  async refreshResources(pluginId: string): Promise<void> {
    await this.json('POST', `/v1/plugins/${encodeURIComponent(pluginId)}/resources/refresh`, {});
  }

  async openSession(pluginId: string, resourceId: string): Promise<OpenSessionResponse> {
    return this.json('POST', '/v1/sessions', { plugin_id: pluginId, resource_id: resourceId });
  }

  async closeSession(sessionId: string): Promise<void> {
    await this.fetch('DELETE', `/v1/sessions/${encodeURIComponent(sessionId)}`, null);
  }

  // Clipboard sync now runs entirely in-band over the VNC stream —
  // see apps/desktop/src/renderer/bridge/ for the renderer side and
  // apps/ahv-remote-console-agent for the in-guest daemon. No SDK
  // methods needed; the bridge probes itself via the VNC channel.

  /**
   * Record a bridge-lifecycle event in the runtime's append-only audit
   * log. Used by the renderer's useAgent hook to emit audit entries on
   * the bridge daemon's behalf (the daemon talks via the in-band VNC
   * channel and has no direct runtime IPC).
   *
   * COMPLIANCE INVARIANT: `extra` MUST NEVER contain clipboard
   * plaintext. Only `bytes` (payload length), `direction`, and an
   * optional `sha256` (hex digest of the payload, for cross-consumer
   * dedupe) are accepted.
   */
  async recordAgentAudit(
    action:
      | 'bridge.connect'
      | 'bridge.disconnect'
      | 'bridge.clip_in'
      | 'bridge.clip_out'
      | 'bridge.probe_reject'
      | 'bridge.install'
      | 'bridge.uninstall',
    sessionId: string,
    extra: { bytes?: number; direction?: 'host_to_vm' | 'vm_to_host'; sha256?: string } = {},
  ): Promise<void> {
    const body: Record<string, unknown> = { action, sessionId };
    if (extra.bytes !== undefined) body.bytes = extra.bytes;
    if (extra.direction !== undefined) body.direction = extra.direction;
    if (extra.sha256 !== undefined) body.sha256 = extra.sha256;
    await this.fetch('POST', '/v1/agent/audit', JSON.stringify(body));
  }

  /**
   * Ask the runtime to upload the bundled bridge installer ISO to the
   * cluster's Image Service (idempotent — re-uses the existing image
   * if it's already there) and attach it as a CD-ROM to the session's
   * VM. The guest then sees a new drive containing `Install.cmd`,
   * which the operator runs once to register the bridge. Replaces the
   * auto-typed install one-liner / HTTP binary-download path.
   *
   * Returns the image UUID (for operator-facing diagnostics), a hint
   * at the drive letter the guest will see (e.g. "D:"), and a
   * `reused` flag — true when the image was already present in the
   * Image Service and the runtime only had to perform the attach.
   *
   * Errors surface as `AHVRemoteConsoleError` with code:
   *   - `iso_mount_unsupported` (HTTP 503) — the plugin behind this
   *     session does not implement ISO mount (non-AHV protocols).
   *   - `iso_mount_failed` (HTTP 502) — Prism rejected the upload or
   *     attach. The error message carries Prism's verbatim response.
   *   - `image_publish_required` — the agent ISO is not in the cluster's
   *     image catalog AND the signed-in role's image-create is denied, so
   *     the upload was not attempted. The renderer maps this code to the
   *     "ask your administrator to publish the agent image" notice.
   *
   * `options.powerCycle` (wire field `power_cycle`, default absent) asks
   * the runtime to orchestrate a graceful VM power cycle so the serial
   * port the clipboard channel needs can be attached (it cannot be
   * hot-added). It MUST only be set on an explicit operator consent —
   * the renderer's [Restart now] confirmation. The outcome lands in
   * `serialNote` (see serialCopy.ts), including the cycle-flow values
   * `serial_port_added_cycled` (port added via the orchestrated cycle),
   * `serial_port_deferred` (cycle not performed; the add is pending the
   * next observed power-off), and `serial_port_power_denied` (the
   * consented cycle hit a Prism 403 on the power operation: the cycle was
   * aborted with the VM left as found, the deferred add stays armed, and
   * the runtime does not retry).
   */
  async mountAgentISO(
    sessionId: string,
    options?: { powerCycle?: boolean },
  ): Promise<{
    imageUuid: string;
    driveLetterHint: string;
    reused: boolean;
    serialNote: string;
    /** Tasks-tray handle for this install op — poll getTask(taskId) for real
     *  install progress (also discoverable via listTasks by session_id). */
    taskId: string;
  }> {
    const body: Record<string, unknown> = { session_id: sessionId };
    // power_cycle defaults to absent on the wire; it is only ever sent as
    // an explicit true that traces back to operator consent.
    if (options?.powerCycle === true) body.power_cycle = true;
    const reply = await this.json<{
      image_uuid: string;
      drive_letter_hint: string;
      reused: boolean;
      serial_note?: string;
      task_id?: string;
    }>('POST', '/v1/agent/mount-iso', body);
    return {
      imageUuid: reply.image_uuid,
      driveLetterHint: reply.drive_letter_hint,
      reused: reply.reused,
      serialNote: reply.serial_note ?? '',
      taskId: reply.task_id ?? '',
    };
  }

  /**
   * List the mountable Image-Service ISOs in the session endpoint's catalog.
   * Backs the "Mount ISO from catalog" picker — distinct from
   * {@link mountAgentISO}, which mounts the bundled agent ISO. session_id rides
   * the query string (a GET) so the signing path-regex stays a fixed literal.
   *
   * Errors surface as `AHVRemoteConsoleError`:
   *   - `iso_mount_unsupported` (503) — the session's plugin can't list catalog ISOs.
   *   - `iso_catalog_failed` (502) — the upstream image list failed.
   */
  async listISOImages(sessionId: string): Promise<{ images: CatalogISOImage[] }> {
    return this.json<{ images: CatalogISOImage[] }>(
      'GET',
      `/v1/agent/images?session_id=${encodeURIComponent(sessionId)}`,
    );
  }

  /**
   * Mount a chosen catalog ISO (by UUID) into the session VM's first CD-ROM
   * drive. A media change — like {@link mountAgentISO}'s attach — but for an
   * arbitrary catalog image and WITHOUT provisioning the COM3 serial channel.
   * Idempotent on the runtime side (`reused` is true when that ISO was already
   * in the drive).
   *
   * Errors surface as `AHVRemoteConsoleError`:
   *   - `iso_mount_unsupported` (503) / `iso_mount_failed` (502, Prism's verbatim reason).
   */
  async mountISO(
    sessionId: string,
    imageUuid: string,
  ): Promise<{ imageUuid: string; driveLetterHint: string; reused: boolean }> {
    const reply = await this.json<{
      image_uuid: string;
      drive_letter_hint: string;
      reused: boolean;
    }>('POST', '/v1/vm/mount-iso', { session_id: sessionId, image_uuid: imageUuid });
    return {
      imageUuid: reply.image_uuid,
      driveLetterHint: reply.drive_letter_hint,
      reused: reply.reused,
    };
  }

  /**
   * Eject the media from the session VM's first CD-ROM drive (never a disk).
   * Idempotent — `ejected` is false when the drive was already empty / there is
   * no CD-ROM device.
   *
   * Errors surface as `AHVRemoteConsoleError`:
   *   - `iso_mount_unsupported` (503) / `iso_eject_failed` (502).
   */
  async ejectISO(sessionId: string): Promise<{ ejected: boolean }> {
    return this.json<{ ejected: boolean }>('POST', '/v1/vm/eject-iso', {
      session_id: sessionId,
    });
  }

  async vmPowerAction(
    pluginId: string,
    resourceId: string,
    action: 'ON' | 'OFF' | 'ACPI_SHUTDOWN' | 'ACPI_REBOOT' | 'RESET',
  ): Promise<void> {
    await this.json('POST', '/v1/vm/power', {
      plugin_id: pluginId,
      resource_id: resourceId,
      action,
    });
  }

  /**
   * Attach the COM3 serial port (for the vdagent clipboard channel) to a VM.
   * Resource-based (like vmPowerAction) so it works on a powered-OFF VM with
   * no active session — the only state in which AHV will actually attach the
   * port. Returns the provisioning note (see serialCopy.ts) and the VM power
   * state at call time.
   */
  async addSerialPort(
    pluginId: string,
    resourceId: string,
  ): Promise<{ note: string; powerState: string }> {
    const reply = await this.json<{ note?: string; power_state?: string }>(
      'POST',
      '/v1/vm/serial-port',
      { plugin_id: pluginId, resource_id: resourceId },
    );
    return { note: reply.note ?? '', powerState: reply.power_state ?? '' };
  }

  /**
   * Read one live VM utilization sample (CPU / memory / disk / network) for the
   * console stats panel. Read-only — it reads a point-in-time sample and mutates
   * nothing, so it is NOT capability-gated and needs no confirmation.
   *
   * plugin_id + resource_id ride the query string (a GET), mirroring whoAmI;
   * resource_id is the same compound "<endpointId>/<vmUUID>" used by
   * vmPowerAction / addSerialPort.
   *
   * Errors surface as `AHVRemoteConsoleError`:
   *   - `stats_unsupported` (503) — the plugin can't report VM stats.
   *   - `rbac_denied` (403) — the upstream denied the read for this role.
   *   - `stats_failed` (502) — the upstream stats query failed.
   */
  async getVMStats(pluginId: string, resourceId: string): Promise<VMStatsResponse> {
    return this.json(
      'GET',
      `/v1/vm/stats?plugin_id=${encodeURIComponent(pluginId)}&resource_id=${encodeURIComponent(resourceId)}`,
    );
  }

  /**
   * Create a named VM snapshot (a v3 recovery point). resource_id is the same
   * compound "<endpointId>/<vmUUID>" used by vmPowerAction / addSerialPort.
   * Creating a snapshot is a VM-config write — gate the UI on vm_update. The v3
   * create returns only a task UUID (no recovery-point UUID/timestamp), so the
   * returned uuid/createdAt are empty here; callers re-list to surface the new
   * recovery point.
   */
  async createSnapshot(
    pluginId: string,
    resourceId: string,
    name: string,
  ): Promise<{ uuid: string; name: string; createdAt: string; snapshotType: string }> {
    const reply = await this.json<SnapshotInfo>('POST', '/v1/vm/snapshots', {
      plugin_id: pluginId,
      resource_id: resourceId,
      name,
    });
    return {
      uuid: reply.uuid,
      name: reply.name,
      createdAt: reply.created_at ?? '',
      snapshotType: reply.snapshot_type ?? '',
    };
  }

  /**
   * List a VM's snapshots, newest-first. Read-only. plugin_id + resource_id
   * ride the query string (a GET), mirroring getVMStats — fetch() signs the
   * path WITH the query, but assertSignableURI strips the query before matching
   * the allowlist regex, so the bare-path entry matches.
   */
  async listSnapshots(pluginId: string, resourceId: string): Promise<ListSnapshotsResponse> {
    return this.json(
      'GET',
      `/v1/vm/snapshots?plugin_id=${encodeURIComponent(pluginId)}&resource_id=${encodeURIComponent(resourceId)}`,
    );
  }

  /**
   * Restore from a recovery point ("snapshot").
   *
   * On v3 this CREATES A NEW VM from the recovery point — it is NOT a guaranteed
   * in-place revert of the source VM. Callers MUST obtain an explicit operator
   * confirmation BEFORE calling this — the SDK, runtime, and plugin do NOT
   * prompt. See SnapshotsPanel's mandatory confirm dialog.
   */
  async restoreSnapshot(pluginId: string, resourceId: string, snapshotId: string): Promise<void> {
    await this.json('POST', '/v1/vm/snapshots/restore', {
      plugin_id: pluginId,
      resource_id: resourceId,
      snapshot_id: snapshotId,
    });
  }

  async listSessions(): Promise<{ sessions: SessionInfo[] }> {
    return this.json('GET', '/v1/sessions');
  }

  /**
   * List the runtime's tracked long-running tasks (power / serial-port / agent
   * ISO) for the Tasks tray. Read-only — it DISPLAYS in-flight ops; it never
   * triggers them.
   */
  async listTasks(): Promise<ListTasksResponse> {
    return this.json('GET', '/v1/tasks');
  }

  /**
   * Refresh ONE task by handle, surfacing the real upstream state (percent /
   * succeeded / failed). Throws `AHVRemoteConsoleError` with code
   * `task_not_found` (404) when the handle is unknown / aged out — the tray
   * drops the row rather than erroring.
   */
  async getTask(id: string): Promise<GetTaskResponse> {
    return this.json('GET', `/v1/tasks/${encodeURIComponent(id)}`);
  }

  diagnosticsBundleURL(): string {
    // Returns the bare bundle URL only — it carries no signature. The
    // diagnostics bundle is a binary stream that must be fetched with a
    // signed request, so callers download it via fetchBinary().
    return `${this.opts.endpoint}/v1/diagnostics/bundle`;
  }

  async fetchBinary(method: string, uri: string): Promise<ArrayBuffer> {
    const headers = await this.opts.signer.signRequest(method, uri, null);
    const res = await fetch(`${this.opts.endpoint}${uri}`, {
      method,
      headers,
    });
    if (!res.ok) throw await this.toError(res);
    return res.arrayBuffer();
  }

  // --------------- session stream ---------------

  /**
   * Open the bidirectional WebSocket stream for an active session. The
   * returned WebSocket forwards binary frames (RFB protocol bytes) and text
   * frames (JSON control frames). Callers are expected to wire onmessage and
   * onclose.
   */
  async openStream(sessionId: string): Promise<WebSocket> {
    const uri = `/v1/sessions/${encodeURIComponent(sessionId)}/stream`;
    const subprotocol = await this.opts.signer.signWebSocket(uri);
    const wsURL = this.opts.endpoint.replace(/^http/, 'ws') + uri;
    const ws = new WebSocket(wsURL, [subprotocol, 'hc.console.v1']);
    ws.binaryType = 'arraybuffer';
    return ws;
  }

  // --------------- presence ---------------

  async getFeatureFlags(): Promise<Record<string, boolean>> {
    const res = await this.json<{ features: Record<string, boolean> }>(
      'GET',
      '/v1/config/features',
    );
    return res.features;
  }

  async openPresenceStream(sessionId: string, vmUuid: string): Promise<WebSocket> {
    const qs = `session_id=${encodeURIComponent(sessionId)}&vm_uuid=${encodeURIComponent(vmUuid)}`;
    const uri = `/v1/presence/ws?${qs}`;
    const subprotocol = await this.opts.signer.signWebSocket(uri);
    const wsURL = this.opts.endpoint.replace(/^http/, 'ws') + uri;
    return new WebSocket(wsURL, [subprotocol, 'hc.presence.v1']);
  }

  // --------------- helpers ---------------

  private async json<T>(method: string, uri: string, body?: unknown): Promise<T> {
    const bodyText = body === undefined ? null : JSON.stringify(body);
    const res = await this.fetch(method, uri, bodyText);
    if (res.status === 204) return undefined as T;
    return (await res.json()) as T;
  }

  private async fetch(method: string, uri: string, body: string | null): Promise<Response> {
    const signedHeaders = await this.opts.signer.signRequest(method, uri, body);
    const headers: Record<string, string> = { ...signedHeaders };
    if (body !== null) headers['Content-Type'] = 'application/json';
    const res = await fetch(`${this.opts.endpoint}${uri}`, {
      method,
      headers,
      body,
    });
    if (!res.ok) throw await this.toError(res);
    return res;
  }

  private async toError(res: Response): Promise<AHVRemoteConsoleError> {
    let body: string;
    let parsed: { error?: { code?: string; message?: string } } | null = null;
    try {
      body = await res.text();
      parsed = JSON.parse(body) as { error?: { code?: string; message?: string } };
    } catch {
      body = res.statusText;
    }
    const message = parsed?.error?.message ?? body ?? res.statusText;
    const code = parsed?.error?.code ?? `http_${res.status}`;
    return new AHVRemoteConsoleError(code, message, res.status);
  }
}

export class AHVRemoteConsoleError extends Error {
  constructor(
    public readonly code: string,
    message: string,
    public readonly status?: number,
  ) {
    super(message);
    this.name = 'AHVRemoteConsoleError';
  }
}

export type {
  CapabilityState,
  CatalogISOImage,
  ControlFrame,
  ClientControlFrame,
  ListSnapshotsResponse,
  SessionInfo,
  SnapshotInfo,
  PluginManifest,
  RuntimeState,
  EndpointCapabilities,
  EndpointHealth,
  TaskInfo,
  TaskState,
  VMStats,
  VMStatsResponse,
};
