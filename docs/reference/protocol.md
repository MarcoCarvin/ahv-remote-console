# Wire protocol reference

This page describes the wire formats between the desktop app, the local runtime and
the Nutanix Prism/QEMU VNC server. It covers the loopback HTTP and WebSocket API, the
JSON control frames on the console WebSocket, and the subset of RFB 3.8 (VNC) that
the runtime parses and the renderer negotiates. It is for developers who build or
debug the client.

When this page and the code disagree, the code is authoritative.

## Contents

- [Overview](#overview)
- [Shared wire types](#shared-wire-types)
- [Request signing](#request-signing)
- [Loopback API routes](#loopback-api-routes)
- [Errors](#errors)
- [The console WebSocket](#the-console-websocket)
- [Control frames](#control-frames)
- [RFB (VNC)](#rfb-vnc)
- [Clipboard on the wire](#clipboard-on-the-wire)
- [Security properties on the wire](#security-properties-on-the-wire)
- [Known limitations](#known-limitations)

## Overview

Three hops carry console traffic. Each has its own framing.

```text
renderer --(loopback HTTP + WS, HMAC-signed)--> runtime
runtime  --(RFB 3.8 over WebSocket)----------> Prism / QEMU VNC
QEMU     --(qemu-vdagent, ISA serial port)----> in-guest agent
```

| Hop | Transport | What it carries |
| --- | --- | --- |
| Renderer to runtime | HTTP and WebSocket on `127.0.0.1` | REST calls through the SDK; the console stream at `/v1/sessions/{id}/stream` |
| Runtime to Prism/QEMU | RFB 3.8 over the Prism VNC WebSocket | Pixels, input and clipboard |
| QEMU to guest | SPICE qemu-vdagent over an ISA serial port | Clipboard text |

The renderer never talks to Prism directly. It calls the runtime through the typed
SDK client (`@ahv-remote-console/sdk`). Every request is HMAC-SHA256 signed, and the
renderer never holds the signing secret.

The runtime dials the Prism VNC WebSocket, performs an RFB 3.8 handshake, and pumps
bytes in both directions. It parses the RFB stream only enough to inject Extended
Clipboard and keep the stream framed. Pixel data is forwarded unchanged.

Clipboard text rides the VNC stream as native RFB messages (`ClientCutText` and
`ServerCutText`). QEMU 9.1 carries it through its clipboard subsystem to the agent
over a serial channel. This path is below the guest's network stack, so clipboard
keeps working under a full-tunnel VPN.

On the console WebSocket, text and binary messages have different meanings:

| WebSocket message | Content |
| --- | --- |
| Binary | Raw RFB protocol bytes |
| Text | A JSON control frame (VM, session, reconnect state, fatal errors) |

## Shared wire types

The package `@ahv-remote-console/protocol` is the dependency-free TypeScript source of
the shapes shared by renderer and runtime. The current protocol version is `1`.

Source: [`packages/protocol/src/index.ts`](../../packages/protocol/src/index.ts).

### Runtime handshake file

At launch the runtime writes a handshake file. The desktop app reads it to learn the
loopback endpoint and to check that both sides hold the same IPC secret.

| Field | Type | Meaning |
| --- | --- | --- |
| `version` | string | Runtime version |
| `pid` | number | Runtime process ID |
| `endpoint` | string | Loopback base URL the SDK targets |
| `secret_hash` | string | `base64url(sha256(ipc_secret))` |
| `started_at` | string | Launch timestamp |

See [IPC and request signing](ipc.md#secret-provisioning-and-handshake) for how the
hash is checked.

### Endpoints, resources and sessions

| Type | Purpose |
| --- | --- |
| `Endpoint`, `UpsertEndpointRequest` | Cluster URL, enabled auth methods, TLS pin |
| `Resource`, `ListResourcesResponse` | VMs and paginated listings |
| `SessionInfo`, `OpenSessionRequest`, `OpenSessionResponse` | A console session bound to a `plugin_id` and `resource_id` |
| `WhoAmIResponse` | The user the endpoint's stored Prism session authenticates as |
| `EndpointHealth` | Status, round-trip time and capability evidence for one endpoint |
| `TaskInfo` | One long-running operation shown in the Tasks tray |

Endpoint fields worth knowing:

| Field | Meaning |
| --- | --- |
| `AuthKind` | Exactly `'basic'` or `'cookie'` |
| `pinned_cert_b64` | The server's public TLS certificate, base64 DER. Exposed only so the desktop app can pin the SSO window to the same certificate the runtime trusts. |
| `allow_insecure_tls` | Exists, but the runtime disables an endpoint that sets it without a valid pin (fail-closed) |

### Identity

The app has no identity system of its own. `WhoAmIResponse` reports the user behind
the stored Prism session, for the lobby account menu. There is no app-side SAML,
OIDC, MFA or RBAC. Authentication is delegated to Prism SSO.

### Endpoint health and capabilities

`EndpointHealth` drives the status dot on the cluster map.

| Field | Values | Meaning |
| --- | --- | --- |
| `status` | `'healthy'`, `'offline'` | Reachability. The probe is unauthenticated: it sends no credentials and reads no body. |
| `rtt_ms` | number | Round-trip time. Absent when offline. |
| `capabilities.*` | `'unknown'`, `'allowed'`, `'denied'` | Permission evidence, not policy |

A capability is `denied` only after a read-only probe proved it or Prism returned a
real 403. `unknown` is not `denied`. The UI shows controls normally while a capability
is unknown and removes them only on proven denials.

### Task states

`TaskInfo.state` is one of `'running'`, `'succeeded'` or `'failed'`. `kind` is one of
`'vm_power'`, `'serial_port'` or `'agent_iso'`. The `label`, `detail` and `error`
strings are operator-facing and never carry clipboard text or credentials.

## Request signing

The protocol package defines the header names and canonical request so both sides
agree.

| Header | Value |
| --- | --- |
| `X-HC-Sig` | Hex HMAC-SHA256 of the canonical request |
| `X-HC-Timestamp` | Unix seconds |
| `X-HC-Nonce` | Hex nonce |

The signed string (`canonicalRequest`) is:

```text
METHOD\nURI\nTIMESTAMP\nNONCE\nBODY_SHA256_HEX
```

The SDK client `AHVRemoteConsoleClient` is the only path to the runtime. It never
holds the IPC secret. It asks an injected `Signer` to sign each call:

- For a REST call, `fetch()` asks the signer to sign `(method, uri, body)` and
  attaches the returned headers.
- For a WebSocket, `openStream` asks the signer to sign the URI. It passes the
  signature as the first WebSocket subprotocol and `hc.console.v1` as the second.

In the renderer, the `Signer` is the contextBridge functions
`window.ahvRemoteConsole.signRequest` and `signWebSocket`. Before the main process
signs anything, it checks `(method, path)` against a least-privilege allowlist. The
allowlist binds both the HTTP method and the path, so a path the SDK only reads with
GET cannot be signed for DELETE. It also rejects absolute, scheme-bearing,
protocol-relative (`//host`) and `..`-bearing URIs.

The full signing algorithm, the allowlist checks and the runtime's verification steps
are in [IPC and request signing](ipc.md).

Source: [`packages/sdk/src/index.ts`](../../packages/sdk/src/index.ts),
[`apps/desktop/src/main/signable.ts`](../../apps/desktop/src/main/signable.ts).

## Loopback API routes

The signing allowlist is the complete list of routes the renderer can reach. Each
entry maps to one SDK method.

| Method | Path | SDK method |
| --- | --- | --- |
| GET | `/v1/runtime/state` | `getRuntimeState` |
| GET | `/v1/plugins` | `listPlugins` |
| GET | `/v1/config/endpoints` | `listEndpoints` |
| POST | `/v1/config/endpoints` | `upsertEndpoint` |
| DELETE | `/v1/config/endpoints/{id}` | `deleteEndpoint` |
| GET | `/v1/config/endpoints/{id}/health` | `getEndpointHealth` |
| GET | `/v1/config/features` | `getFeatureFlags` |
| POST | `/v1/plugins/{id}/auth/{start,complete,refresh,logout}` | Auth lifecycle |
| GET | `/v1/plugins/{id}/whoami` | `whoAmI` |
| GET | `/v1/plugins/{id}/resources` | `listResources` |
| POST | `/v1/plugins/{id}/resources/refresh` | `refreshResources` |
| POST | `/v1/sessions` | `openSession` |
| GET | `/v1/sessions` | `listSessions` |
| DELETE | `/v1/sessions/{id}` | `closeSession` |
| GET | `/v1/sessions/{id}/stream` | `openStream` (WebSocket, signed as GET) |
| GET | `/v1/presence/ws` | `openPresenceStream` (WebSocket, signed as GET) |
| POST | `/v1/agent/audit` | `recordAgentAudit` |
| POST | `/v1/agent/mount-iso` | `mountAgentISO` |
| GET | `/v1/agent/images` | `listISOImages` |
| POST | `/v1/vm/power` | `vmPowerAction` |
| POST | `/v1/vm/serial-port` | `addSerialPort` |
| POST | `/v1/vm/mount-iso` | `mountISO` |
| POST | `/v1/vm/eject-iso` | `ejectISO` |
| POST | `/v1/vm/snapshots` | `createSnapshot` |
| GET | `/v1/vm/snapshots` | `listSnapshots` |
| POST | `/v1/vm/snapshots/restore` | `restoreSnapshot` |
| GET | `/v1/vm/stats` | `getVMStats` |
| GET | `/v1/tasks` | `listTasks` |
| GET | `/v1/tasks/{id}` | `getTask` |

Two operations are not on the allowlist, so the renderer can never sign for them:

| Operation | How it is reached |
| --- | --- |
| Diagnostics bundle (`/v1/diagnostics/bundle`) | The main process signs it itself. The SDK exposes `diagnosticsBundleURL()` as a bare unsigned URL and downloads with `fetchBinary()`, signed in main. |
| Bridge-envelope mint | A dedicated handler in the main process |

## Errors

REST errors use the `APIError` envelope:

```json
{ "error": { "code": "<code>", "message": "<message>" } }
```

The SDK raises them as `AHVRemoteConsoleError` with `code`, `message` and the HTTP
`status`. When the body is not valid JSON, the code is `http_{status}`.

## The console WebSocket

`/v1/sessions/{id}/stream` carries the live console.

| Property | Value |
| --- | --- |
| Subprotocol | `hc.console.v1` |
| Read and write buffers | 1 MiB |
| Per-message compression | Off |
| Origin check | Local origins only |
| Max message, renderer to runtime | 8 MiB (`maxMessageSize`) |
| Max message, upstream to runtime | 160 MiB (`maxUpstreamMessageSize`) |

The origin check is defence in depth on top of the HMAC. Empty and `null` origins and
non-HTTP schemes, such as the packaged app's `file://` or `app://`, are allowed. Only
remote `http` and `https` origins are rejected.

Renderer messages (keys, pointer, cut text) are small, so the 8 MiB limit bounds a
misbehaving local sender. The 160 MiB upstream limit covers a full raw framebuffer at
the largest display the renderer allows. A larger frame ends the session with
`fatal{cause: "frame_too_large"}` instead of a reconnect loop.

On both sockets the proxy sets `TCP_NODELAY`, DSCP Expedited Forwarding (TOS `0xB8`)
and `TCP_QUICKACK` for low latency. `TCP_QUICKACK` is Linux only and has no effect on
macOS.

### Proxy lifecycle

The proxy runs one loop per session:

```text
dial upstream -> RFB 3.8 handshake -> pump both ways -> wait
on upstream failure: tear down, then dial again
```

Every reconnect performs a fresh RFB handshake. There is no resumption. Reconnecting
without a new handshake desyncs the stream after a guest reboot. On a reconnect the
proxy resets its handshake tracker and sends `rfb_reset` then `resumed`. The renderer
then recreates its RFB client and runs the handshake against the new upstream.

| Timer or limit | Value |
| --- | --- |
| Ping to the renderer | Every 15 s (`heartbeatInterval`) |
| Pong deadline from the renderer | 30 s (`pongDeadline`) |
| Reconnect backoff | 500 ms to 30 s (`minBackoff`, `maxBackoff`) |
| Reconnect budget | 60 consecutive dial or handshake failures (`maxReconnectFailures`), then `fatal{cause: "unreachable"}` |
| Powered-off poll | About every 3 s |

The proxy also pings the upstream to measure the real cluster round-trip time. The
pong echoes the ping's RFC 3339 timestamp, which the proxy turns into `latency_ms`.
A half-open watchdog arms a rolling read deadline on the upstream, but only after the
upstream has answered a ping. An idle but healthy VM therefore does not trigger a
reconnect.

Source: [`apps/runtime/internal/proxy/proxy.go`](../../apps/runtime/internal/proxy/proxy.go).

## Control frames

Control frames are JSON objects sent as WebSocket text frames. The runtime uses one
struct for all of them (`ctrlFrame`) and omits empty fields.

### Fields

| Field | Type | Used by |
| --- | --- | --- |
| `type` | string | Every frame |
| `cause` | string | `fatal`: machine-readable classification |
| `reason` | string | `fatal`, `reconnect_attempt`, `clipboard_failed`: human-readable detail |
| `latency_ms` | number | `heartbeat`: upstream round-trip time |
| `bytes_in`, `bytes_out` | number | `heartbeat`: cumulative byte totals |
| `attempt`, `max` | number | `reconnect_attempt`: "attempt N of M" |

### Runtime to renderer

| `type` | Sent when | Fields | Renderer effect |
| --- | --- | --- | --- |
| `connected` | First successful dial and handshake | None | Sets status to connected |
| `heartbeat` | On connect, then about every second | `latency_ms`, `bytes_in`, `bytes_out` | Latency badge, byte counters, adaptive quality input |
| `reconnect_attempt` | Each failed reconnect dial | `reason`, `attempt`, `max` | "Reconnecting, attempt N of M" |
| `reconnecting` | Re-handshake or pump restart | None | Reconnecting overlay |
| `rfb_reset` | Start of every reconnect handshake | None | Recreates the RFB client; resets the clipboard ledger and input modifiers |
| `resumed` | After `rfb_reset`, on a successful dial | None | Dial succeeded. The console is not yet usable, so status does not change to connected. |
| `vm_powered_off` | Upstream reports the VM is off | None | "Powered off" notice. The proxy polls about every 3 s. |
| `vm_starting` | VM is on but the console returns HTTP 5xx | None | "Starting" notice. The proxy retries with backoff. |
| `pong` | Reply to a client `ping` | None | Liveness reply |
| `clipboard_failed` | A guest copy could not be delivered or decoded | `reason` | Warns that the paste may be stale |
| `fatal` | The proxy gives up | `cause`, `reason` | Error overlay with the matching recovery action |

`latency_ms` never reports `0`. A sub-millisecond loopback rounds up to `1`. The byte
counters are session totals (`BytesFromUpstream`, `BytesFromClient`).

Every control frame also proves the loopback is alive. The renderer's reconnect
watchdog uses this to tell a healthy upstream reconnect from a dead socket.

### Renderer to runtime

The runtime accepts one client text frame: `{ "type": "ping" }`. It replies with
`pong`. All other renderer-to-runtime traffic is binary RFB.

### Fatal causes and close codes

`fatal` carries a stable `cause`, so the renderer can offer the right recovery instead
of guessing from a bare socket close. `classifyFatal` maps an upstream or handshake
error to a cause. `unreachable` and `frame_too_large` are sent directly where the proxy
gives up.

| `cause` | Trigger | WebSocket close code | Recovery | Retryable |
| --- | --- | --- | --- | --- |
| `session_expired` | "session expired", "no credentials available", "session cookies missing" | 1008 Policy Violation | SSO sign-in inside the console | No |
| `rbac_denied` | "not authorized" | 1008 Policy Violation | None, not permitted | No |
| `vm_not_found` | "vm not found": VM deleted or moved | 1008 Policy Violation | Reopen from the list | No |
| `session_closed` | "session closed", "unknown session": reaped or deleted | 1008 Policy Violation | Reopen; the renderer creates a new session | Yes |
| `config_error` | "pinned spki", "tls verification disabled" | 1008 Policy Violation | Fix the endpoint | No |
| `unreachable` | Dial failures beyond the retry budget, or the console never became ready | 1011 Internal Server Error | Reconnect | Yes |
| `frame_too_large` | Upstream frame larger than `maxUpstreamMessageSize` | 1009 Message Too Big | Reopen the console | Yes |

The classifier does not treat generic `x509` or `certificate` errors as terminal.
Those also occur for transient TLS conditions on a mid-session dial, such as a load
balancer failover or clock skew after wake on an unpinned endpoint. They stay
retryable.

The renderer mirrors this set as `ConsoleErrorCause`. `normalizeErrorCause` maps any
unknown cause to a neutral "lost connection", so a new proxy cause cannot crash the
UI. `isRetryableCause` decides whether the overlay shows **Reconnect**.

Source: [`apps/desktop/src/renderer/components/console/status.ts`](../../apps/desktop/src/renderer/components/console/status.ts),
[`apps/desktop/src/renderer/components/ConsoleView.tsx`](../../apps/desktop/src/renderer/components/ConsoleView.tsx).

## RFB (VNC)

The runtime's RFB 3.8 parser streams and inspects at the same time. It finds message
boundaries and reads structural data (rectangle coordinates, encoding IDs, clipboard
payloads) without decoding pixels. Everything else is forwarded unchanged.

Source: [`apps/runtime/internal/rfb/`](../../apps/runtime/internal/rfb/).

### Handshake

The proxy runs this handshake on every dial. The security type is always `1` (None).
Prism authenticates through the signed WebSocket upgrade, not through VNC auth.

```text
S->C ProtocolVersion "RFB 003.008\n"   (12 bytes)
C->S ProtocolVersion "RFB 003.008\n"   (12 bytes)
S->C SecurityTypes    1-byte count + N type bytes
C->S SecurityType     1 byte (chosen)
S->C SecurityResult   4 bytes (0 = OK)
C->S ClientInit       1 byte (shared flag)
S->C ServerInit       24 + nameLen bytes
```

`HandshakeTracker` enforces these rules:

- The chosen security type must be `1`, and the server must have offered it.
  Anything else puts the tracker in the terminal `HsError` state and the connection is
  torn down.
- A non-zero `SecurityResult` or a `SecurityTypes` count of 0 is an auth failure. The
  tracker consumes the full reason string and enters `HsError`, so trailing bytes are
  never read as messages.
- `ServerInit` is parsed for width, height, bits per pixel, depth and pixel-format
  shifts. Bits per pixel sets the parser's bytes per pixel, which the variable-length
  encodings need.

### Message types

| Direction | Message | Type ID |
| --- | --- | --- |
| Server to client | `FramebufferUpdate` | 0 |
| Server to client | `SetColourMapEntries` | 1 |
| Server to client | `Bell` | 2 |
| Server to client | `ServerCutText` | 3 |
| Client to server | `SetPixelFormat` | 0 |
| Client to server | `SetEncodings` | 2 |
| Client to server | `FramebufferUpdateRequest` | 3 |
| Client to server | `KeyEvent` | 4 |
| Client to server | `PointerEvent` | 5 |
| Client to server | `ClientCutText` | 6 |
| Client to server | `SetDesktopSize` | 251 |
| Client to server | `QEMUClientMessage` (subtype 0 = Extended Key Event) | 255 |

The proxy parses `QEMUClientMessage` only to keep the client stream framed. It never
sends one itself.

`SetDesktopSize` is forwarded but has no end-to-end effect. AHV guests run stdvga with
fixed, driver-pinned modes, so the renderer does not request resizes. It still
announces `ExtendedDesktopSize`. The agent resizes the display from inside the guest.

### Encodings

The renderer advertises these encodings in this order, so QEMU picks Tight first:

| Encoding | ID | Kind |
| --- | --- | --- |
| `CopyRect` | 1 | Pixel |
| `Tight` | 7 | Pixel, preferred |
| `Raw` | 0 | Pixel, fallback |
| `DesktopSize` | -223 | Pseudo |
| `Cursor` | -239 | Pseudo |
| `ExtendedDesktopSize` | -308 | Pseudo |
| QEMU Extended Key Event | -258 | Pseudo |

ZRLE and Hextile are disabled in the renderer. Tight covers the same cases, and its
all-or-nothing per-rectangle decoder cannot lose stream alignment.

The runtime parser can walk more encodings than the renderer requests, so the proxy
can frame anything the upstream sends: `Raw`, `CopyRect`, `RRE` (2), `Hextile` (5),
`Tight` (7), `ZRLE` (16), `TightPNG` (-260), and the `Cursor`, `DesktopSize` and
`ExtendedDesktopSize` pseudo-encodings. An unknown encoding is forwarded as opaque
data.

There is no JPEG on the wire. AHV's QEMU is built without libjpeg, so Tight JPEG never
appears. The parser and the renderer's Tight decoder both handle JPEG for safety
only. Tight quality and compression pseudo-encodings have no effect on this QEMU and
are not sent.

### Pixel format

The renderer sends `SetPixelFormat` once, at handshake. It never changes it mid-stream.

| Mode | Format | bpp | depth | Details |
| --- | --- | --- | --- | --- |
| Default | Truecolor BGRX, little-endian | 32 | 24 | |
| Low colour (`lowColor`) | Truecolor RGB565, little-endian | 16 | 16 | Max red 31, green 63, blue 31; shifts 11/5/0 |

With JPEG unavailable, 16 bpp is the remaining bandwidth lever. It applies per
connection.

### Length checks

Every variable-length encoding is measured in `int64` and checked against the buffer
length before slicing. This stops a `u16 * u16 * bpp` or `u32` overflow from wrapping
negative on the 32-bit `windows/386` build and crashing the runtime. A negative or
overrunning length returns `ErrIncomplete` instead of panicking.

## Clipboard on the wire

QEMU 9.1 delivers clipboard only through the RFB Extended Clipboard pseudo-encoding
`0xC0A1E5CE` (`EncExtendedClipboard`). There is no legacy `ServerCutText` clipboard
path on this QEMU. The proxy translates, so the renderer never speaks the extended
format.

1. When the renderer sends `SetEncodings`, the proxy appends `0xC0A1E5CE` if it is
   missing. The renderer does not advertise it itself.
2. The proxy then sends clipboard capabilities upstream once, for text with a 10 MB
   maximum (`BuildExtClipCapsResponse`). It does not wait for QEMU to send its
   capabilities.
3. The proxy translates guest copies. Extended Clipboard marks its format with a negative
   4-byte length, so the proxy reads the length as a signed `int32`. A `PROVIDE`
   message carries zlib-compressed text after a 4-byte flags word. The proxy
   decompresses it, capped at 16 MB against decompression bombs, and rebuilds a
   positive-length `ServerCutText` for the renderer
   (`TranslateExtClipServerCutText`).

| Flag | Position in the first 4-byte word | Meaning |
| --- | --- | --- |
| `CAPS` | High bits | Capabilities |
| `REQUEST` | High bits | Ask for clipboard data |
| `PEEK` | High bits | Ask which formats are available |
| `NOTIFY` | High bits | New clipboard available. The proxy answers with a `REQUEST` (`BuildExtClipRequest`). |
| `PROVIDE` | High bits | Clipboard data. A `PROVIDE` that fails to decode produces a `clipboard_failed` frame. |
| `TEXT` | Low bits | Data type: text |

From host to guest, the renderer sends a plain positive-length `ClientCutText` and the
proxy forwards it. The renderer never sends an extended `ClientCutText`. It caps the
payload at 4 MiB, so an oversized paste cannot exceed the proxy's 8 MiB read limit and
end the session. The clipboard engine applies a 4 MB host-to-guest cap before that.

QEMU carries this traffic to the agent over a SPICE qemu-vdagent ISA serial port: COM3
on Windows, `/dev/ttyS2` on Linux. The serial format between QEMU and the agent is
outside the scope of this page.

### Reserved clipboard prefixes

The proxy and the agent reserve two prefixes inside the clipboard channel for control
messages. These are never written to the host clipboard, never forwarded to the
renderer, and never logged in clear text.

| Prefix | Purpose |
| --- | --- |
| `__AHVRC_RESIZE__` | Display-size commands and the keep-warm probe |
| `__AHVRC_KEEPWARM__` | Keep-warm lease tokens. A suffix of `45` arms a 45 s lease. `0` cancels it. |

QEMU has a single adaptive `vnc_refresh` timer. On an idle framebuffer it backs off
from 30 ms to 3000 ms, so the first frame after idle is seconds old. While a console is
attached, the agent keeps the display slightly dirty under a lease that the proxy
renews over this channel. Only Windows guests do the dirtying.

## Security properties on the wire

- Clipboard plaintext never reaches logs or audit. An audit entry about clipboard
  data holds only byte length, direction and an optional SHA-256
  (`recordAgentAudit`). The route exists, but nothing in the repository calls it in
  this release.
- TLS is fail-closed with pinned-SPKI verification. `config_error` is the
  deterministic, non-retryable signal for a pin mismatch or for `allow_insecure_tls`
  without a valid pin.
- Authentication is delegated to Prism SSO. The wire carries no app identity, only
  the captured Prism session that the runtime replays.

## Known limitations

- The declared types do not match the live wire. The protocol package declares a
  `ControlFrame` union (including `error`, `pong` and `clipboard_ready` with a
  `version` field) and a `ClientControlFrame` union (`ping`, `clipboard`,
  `paste_as_typing`, `resize`). Most of these are not on the wire:
  - `clipboard_ready` is never sent.
  - `rfb_reset`, `vm_powered_off`, `vm_starting`, `fatal` and `clipboard_failed` are
    sent but missing from the union. The renderer reads them with a permissive
    `JSON.parse`.
  - `clipboard`, `paste_as_typing` and `resize` client frames are declared but never
    sent. Clipboard uses RFB `ClientCutText` (`sendClientCutText`). Resize uses the
    `__AHVRC_RESIZE__` clipboard prefix.

  The proxy code (`sendCtrl`, `handleClientControl`) and the renderer's `ConsoleView`
  handler define the real wire. Reconcile the types to the proxy, not the reverse.
- Live-migration handling is not visible in this protocol. qemu-vdagent registers an
  unconditional QEMU migration blocker. The deployed mitigation is a libvirt hook that
  arms the vdagent backend only while a console is connected. That behaviour is
  outside this protocol and was not checked against the files covered by this page.

## Related documentation

- [IPC and request signing](ipc.md)
- [Keyboard mapping](keyboard.md)
- [Runtime proxy](../architecture/runtime-proxy.md)
- [Clipboard](../architecture/clipboard.md)
- [Live migration](../architecture/live-migration.md)
- [Glossary](glossary.md)
