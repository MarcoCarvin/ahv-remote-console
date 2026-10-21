# IPC and request signing

This page describes how the desktop app talks to the local runtime and how the app
stops a sandboxed renderer from getting unrestricted access to it. It covers the IPC
secret, the signing allowlist, the HMAC header scheme on both sides, the preload
bridge and the SDK client. It is for developers who change any of these parts.

The renderer never holds the credential that authenticates to the runtime. It can only
ask the main process to sign a small, fixed set of API calls. A hijacked page cannot
create its own runtime access, reach an arbitrary route, or download the diagnostics
bundle.

## Contents

- [Processes and trust boundary](#processes-and-trust-boundary)
- [Secret provisioning and handshake](#secret-provisioning-and-handshake)
- [The signing oracle](#the-signing-oracle)
- [Signing allowlist](#signing-allowlist)
- [HMAC header scheme](#hmac-header-scheme)
- [Runtime verification](#runtime-verification)
- [Preload bridge](#preload-bridge)
- [SDK client](#sdk-client)
- [Invariants](#invariants)

## Processes and trust boundary

| Process | What it is | Holds the IPC secret |
| --- | --- | --- |
| Renderer | Sandboxed React and TypeScript. `contextIsolation` on, `sandbox` on, `nodeIntegration` off. | No |
| Main | Privileged Electron Node process. Starts the runtime and exposes a narrow set of IPC handlers. | Yes |
| Runtime | Go daemon (`apps/runtime`). Loopback HTTP and WebSocket broker plus RFB proxy. Binds an ephemeral loopback port. | Yes |

The renderer has no Node APIs and no `ipcRenderer`. Its only surface to main is what
`contextBridge.exposeInMainWorld('ahvRemoteConsole', api)` publishes. It calls
`signRequest` or `signWebSocket` across that bridge, and only the HMAC header values
or the WebSocket subprotocol string come back. The runtime accepts only loopback
connections and rejects any request whose HMAC does not verify.

```text
renderer --signRequest(method, uri, body)--> main  (checks allowlist, signs)
renderer <--------X-HC-* header values------ main
renderer --signed HTTP / WebSocket----------> runtime (verifies HMAC)
```

## Secret provisioning and handshake

The secret is created once per launch and is never written to disk in clear text.

1. Main starts the runtime with `--ipc-secret-stdin` and the data and plugin directory
   arguments. It uses `stdio: ['pipe','pipe','pipe']` and `windowsHide: true`.
2. Main writes the secret to the child's stdin as a hex string followed by a newline,
   then closes stdin.
3. The runtime reads the secret from stdin, binds its loopback port, and writes the
   handshake file `runtime.json` in the data directory. It removes the file on exit.
4. Main parses `runtime.json` into `RuntimeHandshake`. On `runtime:ready` it compares
   the file's `secret_hash` with the hash of its own copy of the secret.
5. On a mismatch, main fails closed. It broadcasts `state: 'error'` and stops the
   runtime. A mismatch means a tampered runtime, an orphaned older runtime that won
   the race, or a broken spawn.
6. On a match, main zeroes the raw spawn secret buffer. The long-lived `Signer` keeps
   its own copy for signing.

`secret_hash` is `base64url(sha256(secret))` with padding removed.

| Side | How it computes the hash |
| --- | --- |
| Runtime | `Secret.HashB64()` with `base64.RawURLEncoding` |
| Main | `Signer.secretHash()`: base64 without padding, `+` and `/` replaced by `-` and `_` |

The handshake file fields are listed in the
[protocol reference](protocol.md#runtime-handshake-file). The renderer has its own
copy of the `RuntimeHandshake` type for its state display.

Both sides keep the secret in a byte buffer that can be wiped:

| Side | Wipe call | Effect |
| --- | --- | --- |
| Main | `Signer.wipe()` | Zeroes the buffer. Every later method throws `signer wiped`. |
| Runtime | `Secret.Wipe()` | Zeroes the backing storage |

Source: [`apps/desktop/src/main/runtime-manager.ts`](../../apps/desktop/src/main/runtime-manager.ts),
[`apps/desktop/src/main/signer.ts`](../../apps/desktop/src/main/signer.ts),
[`apps/runtime/internal/ipc/secret.go`](../../apps/runtime/internal/ipc/secret.go),
[`apps/runtime/cmd/ahv-remote-console-runtime/main.go`](../../apps/runtime/cmd/ahv-remote-console-runtime/main.go).

## The signing oracle

Main acts as a **signing oracle**. It computes an HMAC for a `(method, uri)` that the
renderer supplies. Without limits, a compromised renderer could get valid signatures
for any runtime route. It could download the token-bearing diagnostics bundle, call
admin routes, or sign an HTTP method the SDK never uses on an allowed path.

Main limits this with an allowlist that binds both the method and the path to what
the SDK calls. Two IPC handlers sign:

| IPC channel | Handler | Signs |
| --- | --- | --- |
| `ahv-remote-console:sign-request` | `handleSignRequest` | HTTP requests |
| `ahv-remote-console:sign-websocket` | `handleSignWebSocket` | WebSocket upgrades, always validated as `GET` |

Each handler runs three checks, in this order, before it signs:

| Check | Function or rule | On failure |
| --- | --- | --- |
| Trusted sender | `assertTrustedSender(e)` | Throws |
| Allowlist | `assertSignableURI(method, uri)` | Throws, see [Validation steps](#validation-steps) |
| Runtime ready | Runtime state must be `ready` | Throws `runtime starting, try again` |

The trusted-sender check finds the `BrowserWindow` that owns the event sender. That
window must be in the tracked trusted set: the main window and console pop-out
windows, all on the default Electron session. The embedded Prism SSO window shows
untrusted third-party content on a `persist:sso-*` partition. It is never registered,
so it cannot reach the oracle.

The readiness check lets the renderer show a retry banner instead of a generic network
error after `ECONNREFUSED`.

For `sign-request`, the body must be a string or `null`. Any other type throws.

Two sensitive operations never go through the oracle:

| Operation | How main handles it |
| --- | --- |
| Diagnostics bundle | `RuntimeManager.exportDiagnostics` signs `GET /v1/diagnostics/bundle` with its own `Signer` |
| Bridge envelopes | Minted by a dedicated handler |

Source: [`apps/desktop/src/main/main.ts`](../../apps/desktop/src/main/main.ts),
[`apps/desktop/src/main/trustedSenders.ts`](../../apps/desktop/src/main/trustedSenders.ts).

## Signing allowlist

The allowlist is `SIGNABLE` in
[`apps/desktop/src/main/signable.ts`](../../apps/desktop/src/main/signable.ts). It
mirrors the call inventory of `packages/sdk/src/index.ts`, and the two must change
together. Each `SignableEntry` is a `{ method, path }` pair. `path` is an anchored
regex that the request path, with the query string removed, must match in full.

### Validation steps

`assertSignableURI(method, uri)` applies these checks in order. Each failure throws an
`Error`.

| Step | Rule | Error message |
| --- | --- | --- |
| 1 | `method` and `uri` are strings | `sign: method and uri must be strings` |
| 2 | URI starts with `/` and not with `//`. Rejects absolute, scheme-bearing and protocol-relative URIs, so the oracle cannot target another origin. | Descriptive error |
| 3 | The path, with the query removed, contains no `..` | Descriptive error |
| 4 | Some entry matches both the method, compared in upper case, and the path regex | `sign: (METHOD, /path) not on signing allowlist` |

Step 4 binds the method. A path the SDK only reads with GET cannot be signed for
DELETE.

### Allowlist entries

In the path column, `{id}` stands for `[^/]+`: one non-slash path segment, usually a
URL-encoded ID.

| Method | Path pattern | SDK method |
| --- | --- | --- |
| GET | `/v1/runtime/state` | `getRuntimeState` |
| GET | `/v1/plugins` | `listPlugins` |
| GET | `/v1/config/endpoints` | `listEndpoints` |
| POST | `/v1/config/endpoints` | `upsertEndpoint` |
| DELETE | `/v1/config/endpoints/{id}` | `deleteEndpoint` |
| GET | `/v1/config/endpoints/{id}/health` | `getEndpointHealth` |
| GET | `/v1/config/features` | `getFeatureFlags` |
| POST | `/v1/plugins/{id}/auth/(start\|complete\|refresh\|logout)` | `startAuth`, `completeAuth`, `refreshAuth`, `logout` |
| GET | `/v1/plugins/{id}/whoami` | `whoAmI` |
| GET | `/v1/plugins/{id}/resources` | `listResources` |
| POST | `/v1/plugins/{id}/resources/refresh` | `refreshResources` |
| POST | `/v1/sessions` | `openSession` |
| GET | `/v1/sessions` | `listSessions` |
| DELETE | `/v1/sessions/{id}` | `closeSession` |
| GET | `/v1/sessions/{id}/stream` | `openStream` (WebSocket) |
| GET | `/v1/presence/ws` | `openPresenceStream` (WebSocket) |
| POST | `/v1/agent/(audit\|mount-iso)` | `recordAgentAudit`, `mountAgentISO` |
| GET | `/v1/agent/images` | `listISOImages` |
| POST | `/v1/vm/(power\|serial-port)` | `vmPowerAction`, `addSerialPort` |
| POST | `/v1/vm/snapshots` | `createSnapshot` |
| POST | `/v1/vm/snapshots/restore` | `restoreSnapshot` |
| GET | `/v1/vm/snapshots` | `listSnapshots` |
| GET | `/v1/vm/stats` | `getVMStats` |
| POST | `/v1/vm/(mount-iso\|eject-iso)` | `mountISO`, `ejectISO` |
| GET | `/v1/tasks` | `listTasks` |
| GET | `/v1/tasks/{id}` | `getTask` |

Notes on the table:

- WebSocket paths are listed under GET because the upgrade request is a GET and is
  validated as one.
- Query strings are removed before matching. `whoAmI`'s `?endpoint_id=...` and the
  pagination query of `listResources` pass whatever their values are.
- `restoreSnapshot` has its own path, so a GET can never reach the restore action
  through the oracle.
- `/v1/diagnostics/bundle` is not in the table. The renderer cannot sign for it.

## HMAC header scheme

Both signing methods build a canonical string and compute HMAC-SHA256 over it with the
IPC secret. The canonical string has the same layout on both sides:

```text
METHOD \n URI \n TIMESTAMP \n NONCE \n BODY_SHA256_HEX
```

### HTTP requests

`Signer.signRequest(method, uri, body)` in main computes:

```ts
timestamp = Math.floor(Date.now() / 1000).toString()          // Unix seconds
nonce     = randomBytes(16).toString('hex')                    // 16 random bytes
bodyHash  = sha256(body ?? '').hex                             // null body hashes ''
canonical = [method.toUpperCase(), uri, timestamp, nonce, bodyHash].join('\n')
sig       = HMAC-SHA256(secret, canonical).hex
```

It returns three headers:

| Header | Value |
| --- | --- |
| `X-HC-Sig` | `sig`, hex |
| `X-HC-Timestamp` | `timestamp`, Unix seconds |
| `X-HC-Nonce` | `nonce`, hex |

### WebSocket upgrades

`Signer.signWebSocket(uri)` builds the same canonical string with method `GET` and the
hash of an empty body. It returns one subprotocol token:

```text
hc.v1.<sig>.<timestamp>.<nonce>
```

The token travels in `Sec-WebSocket-Protocol` because some proxies strip custom
headers from upgrade requests.

### The URI field

The client signs `uri` exactly as the SDK passes it, including any query string. The
runtime verifies over `r.URL.RequestURI()`, which is the path plus the encoded query.
The query string is part of the signed material, even though main removes it for the
allowlist check.

## Runtime verification

The `hmacAuth` middleware wraps every handler except these exempt paths:

| Exempt path | Why |
| --- | --- |
| `/healthz` | Coarse health only; an orchestrator cannot sign |
| `/readyz` | Same |
| `/metrics` | Gauges for a Prometheus scraper |
| `/version` | Version string |

For every other request the middleware runs these steps:

1. Extract the auth material. It reads `X-HC-Sig`, `X-HC-Timestamp` and `X-HC-Nonce`
   first. For WebSocket upgrades it falls back to the `Sec-WebSocket-Protocol` token
   `hc.v1.<sig>.<ts>.<nonce>` (`extractAuthMaterial`).
2. Check clock skew. The timestamp must be within 60 seconds of the runtime's clock.
3. Check for replay. The nonce must not be in the sliding-window nonce cache.
4. Read the body, capped at 8 MiB with `http.MaxBytesReader`, so a caller that holds
   the secret cannot exhaust memory.
5. Rebuild the canonical string, compute the MAC with `Secret.MAC`, and compare in
   constant time (`ConstantTimeEqual`).

The runtime builds the canonical string as:

```go
Method + "\n" + r.URL.RequestURI() + "\n" + ts + "\n" + nonce + "\n" + hex(sha256(body))
```

### Verification errors

| Condition | Response |
| --- | --- |
| No signature headers and no subprotocol token | `401 missing signature` |
| Timestamp more than 60 s from now | `401 stale or future-dated request` |
| Nonce already seen | `401 replay detected` |
| MAC does not match | `401 bad signature` |

The nonce cache is `newNonceCache(8192, 2*skew)`. It removes expired entries as it
goes. When it reaches its cap, it evicts the oldest quarter by timestamp, which are the
entries closest to expiry, rather than in map order. Evicting by map order could
widen the replay window.

### Network checks around the HMAC

`withMiddleware` adds two more layers:

| Layer | Rule |
| --- | --- |
| Loopback only (`loopbackOnly`) | The remote address must be a loopback IP. The `Host` header must be `127.0.0.1`, `localhost` or `[::1]`, which blocks DNS rebinding. |
| CORS allowlist (`corsBridge`) | Only `file://`, `http://localhost:5173` and `http://127.0.0.1:5173` get CORS headers. The opaque `null` origin is rejected, so a sandboxed iframe or `data:` page cannot reach the runtime with credentials. `OPTIONS` preflights return before the HMAC layer, because preflights cannot carry signature headers. |

Source: [`apps/runtime/internal/ipc/server.go`](../../apps/runtime/internal/ipc/server.go),
[`apps/runtime/internal/ipc/nonce.go`](../../apps/runtime/internal/ipc/nonce.go).

## Preload bridge

The renderer's whole surface to main is the `ExposedAPI` object published as
`window.ahvRemoteConsole`. The renderer has no direct `ipcRenderer`, `fs` or other
Node module. Each method wraps `ipcRenderer.invoke` or `ipcRenderer.on`.

### Signing members

| Member | Signature | Returns |
| --- | --- | --- |
| `signRequest` | `(method, uri, body) => Promise<Record<string,string>>` | The HMAC headers only. `body` is a string or `null`; main hashes it. |
| `signWebSocket` | `(uri) => Promise<string>` | The `Sec-WebSocket-Protocol` token |

### Other members

These are not part of the signing oracle.

| Group | Members |
| --- | --- |
| Runtime lifecycle | `getRuntimeState`, `onRuntimeState`, `restartRuntime` |
| Diagnostics | `saveDiagnostics`. Main signs the bundle download; the renderer only triggers the save. |
| Console | `setCaptureMode`, `openConsoleWindow`, `closeConsoleWindows`, `toggleFullscreen`, `onWindowFullscreen` |
| SSO | `openSSOLogin` (opens the Prism login window on an isolated partition and returns captured cookies and the trust-on-first-use certificate), `onOIDCCallback`, `onNavigate` |
| Host clipboard | `onClipboardChanged`, `clipboardRead`, `clipboardWrite` |
| Updates | `onUpdateAvailable`, `onUpdateDownloaded` |
| Logging | `hcLog`: sends renderer log lines to the main-process log file without waiting for a reply |
| Platform | `platform`: the value of `process.platform` |

Every privileged handler in main checks `assertTrustedSender` before it acts, not only
the two signing handlers.

Source: [`apps/desktop/src/preload/preload.ts`](../../apps/desktop/src/preload/preload.ts).

## SDK client

`@ahv-remote-console/sdk` is the only path to the runtime over signed HTTP and
WebSocket. Both the renderer and the main process use it.

Source: [`packages/sdk/src/index.ts`](../../packages/sdk/src/index.ts).

### Construction

`AHVRemoteConsoleClient` takes `{ endpoint, signer }` (`ClientOptions`). The signer is
injected, so the client never touches the secret.

```ts
export interface Signer {
  signRequest(method, uri, body): Promise<Record<string, string>> | Record<string, string>;
  signWebSocket(uri): Promise<string> | string;
}
```

In the renderer, the signer is the contextBridge:

```ts
new AHVRemoteConsoleClient({
  endpoint,
  signer: {
    signRequest: (method, uri, body) => window.ahvRemoteConsole.signRequest(method, uri, body),
    signWebSocket: (uri) => window.ahvRemoteConsole.signWebSocket(uri),
  },
});
```

`App.tsx` builds it this way, and the console pop-out window (`ConsoleWindowApp.tsx`)
builds the same shape. In main, the `Signer` instance is passed directly. `endpoint` is
the loopback base URL from the handshake file. The client is rebuilt when the loopback
port changes after a runtime restart. React components get the client from
`ClientContext` through `useClient`.

### Request mechanics

| Helper | What it does |
| --- | --- |
| `fetch(method, uri, body)` | Calls `signer.signRequest`, adds the returned headers, adds `Content-Type: application/json` only when there is a body, calls `fetch(endpoint + uri)`, and throws `AHVRemoteConsoleError` on a non-OK response |
| `json<T>(method, uri, body?)` | Serialises the body as JSON (`undefined` becomes `null`), calls `fetch`, and returns `undefined` for a 204 |

`AHVRemoteConsoleError` carries a machine-readable `code` and the HTTP `status`.
`toError` parses the runtime's `{ error: { code, message } }` envelope, written by
`writeErr` in the runtime. When there is no code it uses `http_<status>`.

### REST methods

Each method maps to one allowlist entry.

| SDK method | Wire call |
| --- | --- |
| `getRuntimeState()` | GET `/v1/runtime/state` |
| `listPlugins()` | GET `/v1/plugins` |
| `listEndpoints()` | GET `/v1/config/endpoints` |
| `getEndpointHealth(id)` | GET `/v1/config/endpoints/{id}/health` |
| `upsertEndpoint(body)` | POST `/v1/config/endpoints` |
| `deleteEndpoint(id)` | DELETE `/v1/config/endpoints/{id}` |
| `startAuth(pluginId, body)` | POST `/v1/plugins/{id}/auth/start` |
| `completeAuth(pluginId, body)` | POST `/v1/plugins/{id}/auth/complete` |
| `refreshAuth(pluginId)` | POST `/v1/plugins/{id}/auth/refresh` |
| `logout(pluginId)` | POST `/v1/plugins/{id}/auth/logout` |
| `whoAmI(pluginId, endpointId)` | GET `/v1/plugins/{id}/whoami?endpoint_id=...` |
| `listResources(pluginId, params)` | GET `/v1/plugins/{id}/resources[?q&page&page_size]` |
| `refreshResources(pluginId)` | POST `/v1/plugins/{id}/resources/refresh` |
| `openSession(pluginId, resourceId)` | POST `/v1/sessions` |
| `closeSession(sessionId)` | DELETE `/v1/sessions/{id}` |
| `listSessions()` | GET `/v1/sessions` |
| `getFeatureFlags()` | GET `/v1/config/features` |
| `recordAgentAudit(action, sessionId, extra)` | POST `/v1/agent/audit` |
| `mountAgentISO(sessionId, options?)` | POST `/v1/agent/mount-iso` |
| `vmPowerAction(pluginId, resourceId, action)` | POST `/v1/vm/power` |
| `addSerialPort(pluginId, resourceId)` | POST `/v1/vm/serial-port` |

Method details:

| Method | Detail |
| --- | --- |
| `recordAgentAudit` | `action` is a fixed union of `bridge.*` events. `extra` accepts only `bytes`, `direction` and an optional `sha256`, never clipboard text. |
| `mountAgentISO` | Documents the runtime error codes `iso_mount_unsupported`, `iso_mount_failed` and `image_publish_required`. The `power_cycle` field is only ever set to an explicit `true` that comes from operator consent. |
| `vmPowerAction`, `addSerialPort` | Take plugin and resource IDs, so they work on a powered-off VM with no open session |

There are no clipboard methods. Clipboard sync runs inside the VNC stream, not over
signed IPC.

### WebSocket streams

Each stream method signs the path with `signer.signWebSocket`, changes `http` to `ws`
in the endpoint, and opens the socket with the HMAC token as the first subprotocol and
an application subprotocol second. The runtime advertises the application
subprotocol.

| Method | Path | Subprotocols | Notes |
| --- | --- | --- | --- |
| `openStream(sessionId)` | `/v1/sessions/{id}/stream` | `[hmacToken, 'hc.console.v1']` | `binaryType = 'arraybuffer'`. Binary frames carry RFB; text frames carry JSON control frames. |
| `openPresenceStream(sessionId, vmUuid)` | `/v1/presence/ws?session_id&vm_uuid` | `[hmacToken, 'hc.presence.v1']` | |

### Diagnostics bundle

`diagnosticsBundleURL()` returns the bundle URL with no signature.
`fetchBinary(method, uri)` signs a request without a body and downloads the stream.
The bundle path is not on the allowlist. In practice main fetches the bundle and signs
`GET /v1/diagnostics/bundle` itself in `RuntimeManager.exportDiagnostics`. The renderer
never calls `signRequest` for it.

## Invariants

| Invariant | Enforced by |
| --- | --- |
| The renderer never holds the IPC secret | Preload bridge exposes only signing results |
| Only allowlisted `(method, path)` pairs are signed for the renderer | `assertSignableURI` in `signable.ts`, kept in step with the SDK |
| Diagnostics bundle and bridge envelopes are never signed for the renderer | Absent from the allowlist; main handles both |
| Runtime accepts only signed loopback requests | `loopbackOnly`, Host check, CORS allowlist, `hmacAuth` |
| Replays and stale requests fail | 60 s skew window, nonce cache, constant-time compare |
| The secret never reaches disk in clear text | Passed on stdin, checked by hash, zeroed after use |

## Related documentation

- [Wire protocol reference](protocol.md)
- [Desktop app](../architecture/desktop.md)
- [Threat model](../security/threat-model.md)
- [Security overview](../security/README.md)
- [Glossary](glossary.md)
