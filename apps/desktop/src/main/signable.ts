/**
 * Signing-oracle allowlist for the IPC "signing oracle" (sign-request /
 * sign-websocket in main.ts).
 *
 * The oracle HMACs whatever (method, URI) the renderer passes. Without a gate,
 * a compromised/XSS'd renderer could mint valid auth for ANY runtime endpoint
 * — e.g. pull the token-bearing diagnostics bundle, hit admin surfaces, or
 * (SEC-ORACLE-1) obtain a signature for an HTTP METHOD the SDK never issues on
 * an otherwise-allowlisted path (e.g. DELETE on a GET/POST-only path), widening
 * the oracle beyond the real API surface.
 *
 * We therefore bind BOTH the method AND the path to exactly what the SDK
 * legitimately calls (see packages/sdk/src/index.ts — this table is the
 * least-privilege mirror of that call inventory, including the read-only
 * Tasks-tray polls listTasks / getTask). The diagnostics bundle and
 * the bridge-envelope mint are deliberately ABSENT: main signs diagnostics
 * itself (runtime-manager.exportDiagnostics) and mints bridge envelopes via its
 * own dedicated handler — the renderer never needs to sign for either.
 *
 * This module is intentionally pure (no DOM / no Electron imports) so it can be
 * unit-tested in isolation.
 */

/** A single allowlisted (HTTP method, path-pattern) pair. */
export interface SignableEntry {
  /** HTTP method. WebSocket upgrades are signed as 'GET'. */
  readonly method: string;
  /** Regex the request PATH (query string stripped) must fully match. */
  readonly path: RegExp;
}

/**
 * Authoritative allowlist: every distinct (method, path) the SDK actually
 * issues via this.json / this.fetch, plus every WebSocket path opened via
 * signWebSocket (treated as GET). Keep this in lock-step with
 * packages/sdk/src/index.ts.
 */
export const SIGNABLE: readonly SignableEntry[] = [
  // getRuntimeState
  { method: 'GET', path: /^\/v1\/runtime\/state$/ },
  // listPlugins
  { method: 'GET', path: /^\/v1\/plugins$/ },
  // listEndpoints / upsertEndpoint
  { method: 'GET', path: /^\/v1\/config\/endpoints$/ },
  { method: 'POST', path: /^\/v1\/config\/endpoints$/ },
  // deleteEndpoint
  { method: 'DELETE', path: /^\/v1\/config\/endpoints\/[^/]+$/ },
  // getEndpointHealth
  { method: 'GET', path: /^\/v1\/config\/endpoints\/[^/]+\/health$/ },
  // getFeatureFlags
  { method: 'GET', path: /^\/v1\/config\/features$/ },
  // startAuth / completeAuth / refreshAuth / logout (all POST)
  { method: 'POST', path: /^\/v1\/plugins\/[^/]+\/auth\/(start|complete|refresh|logout)$/ },
  // whoAmI
  { method: 'GET', path: /^\/v1\/plugins\/[^/]+\/whoami$/ },
  // listResources
  { method: 'GET', path: /^\/v1\/plugins\/[^/]+\/resources$/ },
  // refreshResources
  { method: 'POST', path: /^\/v1\/plugins\/[^/]+\/resources\/refresh$/ },
  // openSession / listSessions
  { method: 'POST', path: /^\/v1\/sessions$/ },
  { method: 'GET', path: /^\/v1\/sessions$/ },
  // closeSession
  { method: 'DELETE', path: /^\/v1\/sessions\/[^/]+$/ },
  // openStream (WebSocket — signed as GET)
  { method: 'GET', path: /^\/v1\/sessions\/[^/]+\/stream$/ },
  // openPresenceStream (WebSocket — signed as GET)
  { method: 'GET', path: /^\/v1\/presence\/ws$/ },
  // recordAgentAudit / mountAgentISO
  { method: 'POST', path: /^\/v1\/agent\/(audit|mount-iso)$/ },
  // listISOImages (catalog picker — query string is stripped before match,
  // so the literal path regex is correct)
  { method: 'GET', path: /^\/v1\/agent\/images$/ },
  // vmPowerAction / addSerialPort
  { method: 'POST', path: /^\/v1\/vm\/(power|serial-port)$/ },
  // createSnapshot / restoreSnapshot — restore has its OWN path so a GET can
  // never reach it through the oracle; restore is DESTRUCTIVE (POST only).
  { method: 'POST', path: /^\/v1\/vm\/snapshots$/ },
  { method: 'POST', path: /^\/v1\/vm\/snapshots\/restore$/ },
  // listSnapshots — read-only (GET); plugin_id + resource_id ride the query
  // string (stripped before match), so the path stays a fixed literal.
  { method: 'GET', path: /^\/v1\/vm\/snapshots$/ },
  // getVMStats — read-only live VM stats poll. plugin_id + resource_id ride the
  // query string (stripped before match), so the path stays a fixed literal.
  { method: 'GET', path: /^\/v1\/vm\/stats$/ },
  // mountISO (catalog) / ejectISO — session_id rides the body, so the path
  // stays a fixed literal (no [^/]+ segment to widen the oracle)
  { method: 'POST', path: /^\/v1\/vm\/(mount-iso|eject-iso)$/ },
  // listTasks / getTask — the Tasks tray's read-only poll surface. GET only.
  { method: 'GET', path: /^\/v1\/tasks$/ },
  { method: 'GET', path: /^\/v1\/tasks\/[^/]+$/ },
];

/**
 * Throw unless (`method`, `uri`) is on the signing allowlist. `uri` is a
 * runtime path, optionally with a query string.
 *
 * Layered checks (defense in depth):
 *   1. The URI must be root-relative (`/v1/...`) — reject absolute,
 *      scheme-bearing, or protocol-relative (`//host`) URIs so the oracle can
 *      never be aimed at another origin.
 *   2. The PATH (query stripped) must not contain `..` path traversal.
 *   3. SOME allowlist entry must match BOTH the method (case-insensitive) AND
 *      the path-regex. Binding the method is what closes SEC-ORACLE-1: a path
 *      that the SDK only ever GETs cannot be DELETEd through the oracle.
 */
export function assertSignableURI(method: string, uri: string): void {
  if (typeof method !== 'string' || typeof uri !== 'string') {
    throw new Error('sign: method and uri must be strings');
  }
  // Reject anything that looks absolute or scheme-bearing.
  if (!uri.startsWith('/') || uri.startsWith('//')) {
    throw new Error('sign: refusing to sign non-relative URI');
  }
  const path = uri.split('?', 1)[0]!;
  if (path.includes('..')) {
    throw new Error('sign: refusing to sign URI with path traversal');
  }
  const normalizedMethod = method.toUpperCase();
  const allowed = SIGNABLE.some(
    (entry) => entry.method === normalizedMethod && entry.path.test(path),
  );
  if (!allowed) {
    throw new Error(`sign: (${normalizedMethod}, ${path}) not on signing allowlist`);
  }
}
