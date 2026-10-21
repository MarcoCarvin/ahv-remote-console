import { describe, expect, it } from 'vitest';
import { assertSignableURI, SIGNABLE } from '../signable';

/**
 * SEC-ORACLE-1: the IPC signing oracle must bind BOTH the HTTP method and the
 * path. These tests pin the allowlist to the SDK's exact call inventory
 * (packages/sdk/src/index.ts) and prove that cross-method / off-allowlist /
 * malformed URIs are rejected.
 */

/** Every (method, uri) the SDK legitimately signs. Mirrors index.ts. */
const ALLOWED: readonly { method: string; uri: string; sdk: string }[] = [
  { method: 'GET', uri: '/v1/runtime/state', sdk: 'getRuntimeState' },
  { method: 'GET', uri: '/v1/plugins', sdk: 'listPlugins' },
  { method: 'GET', uri: '/v1/config/endpoints', sdk: 'listEndpoints' },
  { method: 'POST', uri: '/v1/config/endpoints', sdk: 'upsertEndpoint' },
  { method: 'DELETE', uri: '/v1/config/endpoints/ep-123', sdk: 'deleteEndpoint' },
  { method: 'GET', uri: '/v1/config/endpoints/ep-123/health', sdk: 'getEndpointHealth' },
  { method: 'GET', uri: '/v1/config/features', sdk: 'getFeatureFlags' },
  { method: 'POST', uri: '/v1/plugins/ahv/auth/start', sdk: 'startAuth' },
  { method: 'POST', uri: '/v1/plugins/ahv/auth/complete', sdk: 'completeAuth' },
  { method: 'POST', uri: '/v1/plugins/ahv/auth/refresh', sdk: 'refreshAuth' },
  { method: 'POST', uri: '/v1/plugins/ahv/auth/logout', sdk: 'logout' },
  { method: 'GET', uri: '/v1/plugins/ahv/whoami?endpoint_id=ep-123', sdk: 'whoAmI' },
  { method: 'GET', uri: '/v1/plugins/ahv/resources', sdk: 'listResources' },
  {
    method: 'GET',
    uri: '/v1/plugins/ahv/resources?q=web&page=2&page_size=50',
    sdk: 'listResources (with query)',
  },
  { method: 'POST', uri: '/v1/plugins/ahv/resources/refresh', sdk: 'refreshResources' },
  { method: 'POST', uri: '/v1/sessions', sdk: 'openSession' },
  { method: 'GET', uri: '/v1/sessions', sdk: 'listSessions' },
  { method: 'DELETE', uri: '/v1/sessions/sess-9', sdk: 'closeSession' },
  { method: 'POST', uri: '/v1/agent/audit', sdk: 'recordAgentAudit' },
  { method: 'POST', uri: '/v1/agent/mount-iso', sdk: 'mountAgentISO' },
  { method: 'GET', uri: '/v1/agent/images?session_id=sess-9', sdk: 'listISOImages' },
  { method: 'POST', uri: '/v1/vm/power', sdk: 'vmPowerAction' },
  { method: 'POST', uri: '/v1/vm/serial-port', sdk: 'addSerialPort' },
  { method: 'POST', uri: '/v1/vm/snapshots', sdk: 'createSnapshot' },
  { method: 'POST', uri: '/v1/vm/snapshots/restore', sdk: 'restoreSnapshot' },
  {
    method: 'GET',
    uri: '/v1/vm/snapshots?plugin_id=nutanix-ahv&resource_id=ep-1%2Fvm-1',
    sdk: 'listSnapshots',
  },
  {
    method: 'GET',
    uri: '/v1/vm/stats?plugin_id=nutanix-ahv&resource_id=ep-1%2Fvm-1',
    sdk: 'getVMStats',
  },
  { method: 'POST', uri: '/v1/vm/mount-iso', sdk: 'mountISO (catalog)' },
  { method: 'POST', uri: '/v1/vm/eject-iso', sdk: 'ejectISO' },
  { method: 'GET', uri: '/v1/tasks', sdk: 'listTasks' },
  { method: 'GET', uri: '/v1/tasks/task-abc123', sdk: 'getTask' },
  // WebSocket paths — signed as GET via signWebSocket.
  { method: 'GET', uri: '/v1/sessions/sess-9/stream', sdk: 'openStream (WS)' },
  {
    method: 'GET',
    uri: '/v1/presence/ws?session_id=sess-9&vm_uuid=vm-1',
    sdk: 'openPresenceStream (WS)',
  },
];

describe('assertSignableURI — legitimate SDK calls are ALLOWED', () => {
  it.each(ALLOWED)('allows $method $uri ($sdk)', ({ method, uri }) => {
    expect(() => assertSignableURI(method, uri)).not.toThrow();
  });

  it('accepts case-insensitive methods (the contextBridge may pass lowercase)', () => {
    expect(() => assertSignableURI('get', '/v1/runtime/state')).not.toThrow();
    expect(() => assertSignableURI('post', '/v1/sessions')).not.toThrow();
  });
});

describe('assertSignableURI — cross-method attacks are REJECTED', () => {
  const CROSS_METHOD: readonly { method: string; uri: string; why: string }[] = [
    { method: 'DELETE', uri: '/v1/runtime/state', why: 'DELETE on a GET-only path' },
    { method: 'DELETE', uri: '/v1/sessions', why: 'DELETE on the GET/POST collection path' },
    {
      method: 'DELETE',
      uri: '/v1/config/features',
      why: 'DELETE on a GET-only path',
    },
    { method: 'GET', uri: '/v1/vm/power', why: 'GET on a POST-only action' },
    { method: 'GET', uri: '/v1/vm/serial-port', why: 'GET on a POST-only action' },
    {
      method: 'GET',
      uri: '/v1/vm/snapshots/restore',
      why: 'GET on the POST-only DESTRUCTIVE restore path — no read path can reach restore',
    },
    {
      method: 'DELETE',
      uri: '/v1/vm/snapshots',
      why: 'DELETE on the create/list snapshots path (SEC-ORACLE-1 method binding)',
    },
    {
      method: 'POST',
      uri: '/v1/vm/stats',
      why: 'POST on the GET-only read-only stats path (SEC-ORACLE-1 method binding)',
    },
    { method: 'GET', uri: '/v1/agent/mount-iso', why: 'GET on a POST-only action' },
    {
      method: 'DELETE',
      uri: '/v1/vm/mount-iso',
      why: 'DELETE on a POST-only action (SEC-ORACLE-1)',
    },
    { method: 'GET', uri: '/v1/vm/mount-iso', why: 'GET on a POST-only action' },
    { method: 'DELETE', uri: '/v1/vm/eject-iso', why: 'DELETE on a POST-only action' },
    { method: 'POST', uri: '/v1/agent/images', why: 'POST on a GET-only list path' },
    { method: 'PUT', uri: '/v1/config/endpoints', why: 'PUT never used by the SDK' },
    {
      method: 'DELETE',
      uri: '/v1/plugins/ahv/auth/logout',
      why: 'DELETE on a POST-only auth path',
    },
    {
      method: 'POST',
      uri: '/v1/config/endpoints/ep-123',
      why: 'POST on the DELETE-only per-id endpoint path',
    },
    {
      method: 'POST',
      uri: '/v1/sessions/sess-9',
      why: 'POST on the DELETE-only per-id session path',
    },
    {
      method: 'POST',
      uri: '/v1/plugins/ahv/resources',
      why: 'POST on the GET-only resources list path',
    },
    {
      method: 'DELETE',
      uri: '/v1/sessions/sess-9/stream',
      why: 'DELETE on a GET (WS) path',
    },
    { method: 'POST', uri: '/v1/tasks', why: 'POST on the GET-only tasks list (SEC-ORACLE-1)' },
    {
      method: 'DELETE',
      uri: '/v1/tasks/task-abc123',
      why: 'DELETE on the GET-only per-task path (no cancel via the oracle)',
    },
    { method: 'POST', uri: '/v1/tasks/task-abc123', why: 'POST on a GET-only per-task path' },
  ];

  it.each(CROSS_METHOD)('rejects $method $uri ($why)', ({ method, uri }) => {
    expect(() => assertSignableURI(method, uri)).toThrow(/not on signing allowlist/);
  });
});

describe('assertSignableURI — malformed / non-relative URIs are REJECTED', () => {
  it('rejects absolute http(s) URLs', () => {
    expect(() => assertSignableURI('GET', 'http://evil.example/v1/runtime/state')).toThrow(
      /non-relative URI/,
    );
    expect(() => assertSignableURI('GET', 'https://127.0.0.1/v1/runtime/state')).toThrow(
      /non-relative URI/,
    );
  });

  it('rejects protocol-relative (`//host`) URIs', () => {
    expect(() => assertSignableURI('GET', '//evil.example/v1/runtime/state')).toThrow(
      /non-relative URI/,
    );
  });

  it('rejects scheme-bearing / non-slash-prefixed URIs', () => {
    expect(() => assertSignableURI('GET', 'file:///etc/passwd')).toThrow(/non-relative URI/);
    expect(() => assertSignableURI('GET', 'v1/runtime/state')).toThrow(/non-relative URI/);
    expect(() => assertSignableURI('GET', 'ws://127.0.0.1/v1/presence/ws')).toThrow(
      /non-relative URI/,
    );
  });

  it('rejects `..` path traversal', () => {
    expect(() => assertSignableURI('GET', '/v1/runtime/../diagnostics/bundle')).toThrow(
      /path traversal/,
    );
    expect(() => assertSignableURI('GET', '/v1/sessions/..%2f..')).toThrow(/path traversal/);
  });

  it('rejects non-string args', () => {
    // @ts-expect-error — exercising the runtime guard against bad IPC input.
    expect(() => assertSignableURI(undefined, '/v1/runtime/state')).toThrow(/must be strings/);
    // @ts-expect-error — exercising the runtime guard against bad IPC input.
    expect(() => assertSignableURI('GET', 42)).toThrow(/must be strings/);
  });
});

describe('assertSignableURI — off-allowlist paths are REJECTED', () => {
  const OFF_ALLOWLIST: readonly { method: string; uri: string }[] = [
    { method: 'GET', uri: '/v1/diagnostics/bundle' },
    { method: 'GET', uri: '/v1/diagnostics' },
    { method: 'POST', uri: '/v1/agent/mint' },
    { method: 'GET', uri: '/v1/admin/users' },
    { method: 'GET', uri: '/v1/runtime/state/extra' },
    { method: 'GET', uri: '/v1/plugins/ahv/resources/extra' },
    { method: 'GET', uri: '/v2/runtime/state' },
    { method: 'GET', uri: '/' },
  ];

  it.each(OFF_ALLOWLIST)('rejects $method $uri', ({ method, uri }) => {
    expect(() => assertSignableURI(method, uri)).toThrow(/not on signing allowlist/);
  });
});

describe('SIGNABLE table integrity', () => {
  it('only contains methods the SDK actually issues', () => {
    const methods = new Set(SIGNABLE.map((e) => e.method));
    for (const m of methods) {
      expect(['GET', 'POST', 'DELETE']).toContain(m);
    }
  });

  it('every entry path-regex is anchored at both ends', () => {
    for (const entry of SIGNABLE) {
      expect(entry.path.source.startsWith('^')).toBe(true);
      expect(entry.path.source.endsWith('$')).toBe(true);
    }
  });
});
