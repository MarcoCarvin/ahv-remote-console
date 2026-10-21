# Runtime and VNC proxy

The runtime is the Go daemon in `apps/runtime` that the desktop app starts at launch.
It brokers every privileged call between the sandboxed renderer and Prism, and it
proxies each console WebSocket to the cluster's RFB/VNC stream. This page is for
developers working on the runtime.

## Contents

- [Responsibilities](#responsibilities)
- [Startup and the handshake file](#startup-and-the-handshake-file)
- [Request authentication](#request-authentication)
- [Routes](#routes)
- [Proxy lifecycle](#proxy-lifecycle)
- [Reconnect and failure handling](#reconnect-and-failure-handling)
- [Control frames](#control-frames)
- [Heartbeat and liveness](#heartbeat-and-liveness)
- [Session reaping](#session-reaping)
- [Data path](#data-path)
- [Keep-warm lease](#keep-warm-lease)
- [Shutdown](#shutdown)
- [Constants](#constants)
- [Known limitations](#known-limitations)

## Responsibilities

The runtime has two main parts:

- The broker is a loopback HTTP and WebSocket server. It authenticates every request
  and routes it to a plugin. Source:
  [`apps/runtime/internal/ipc/server.go`](../../apps/runtime/internal/ipc/server.go),
  [`router.go`](../../apps/runtime/internal/ipc/router.go).
- The proxy bridges one renderer console WebSocket to one upstream Prism VNC
  WebSocket. Source:
  [`apps/runtime/internal/proxy/proxy.go`](../../apps/runtime/internal/proxy/proxy.go).

The clipboard data itself does not pass through the runtime as a separate channel.
It rides the in-guest vdagent over an ISA serial channel. The proxy does take part
in clipboard negotiation (Extended Clipboard injection and translation). It also
arms the keep-warm lease over the same channel.

When a console says "reconnecting", "VM is powered off" or "session expired", the
message comes from this layer.

## Startup and the handshake file

`run()` in
[`cmd/ahv-remote-console-runtime/main.go`](../../apps/runtime/cmd/ahv-remote-console-runtime/main.go)
sets up logging, data directories, the IPC secret, configuration, telemetry, the
keystore, the plugin host, the audit log and the IPC server. The order matters:

1. Bind `127.0.0.1:0` and learn the port the OS assigned.
2. Write the handshake file `runtime.json`, so the desktop app can find the runtime.
3. Load plugins.
4. Start serving.

Plugins load after the handshake file is on disk. A plugin's `Init()` may do bounded
network I/O, and a slow but healthy cold start must not look like a hang.

The handshake file, defined in
[`internal/handshake/handshake.go`](../../apps/runtime/internal/handshake/handshake.go),
holds:

- the version and PID,
- the endpoint `http://127.0.0.1:<port>`,
- `secret_hash`, which is `base64url-no-padding(sha256(secret))`,
- a start timestamp.

The file is written atomically with mode 0600. The runtime creates a temp file with
`O_CREATE|O_EXCL`, which refuses a planted symlink, then renames it and sets the
mode. `O_EXCL` is used because `O_NOFOLLOW` does not exist on Windows. The file is
removed on graceful shutdown.

## Request authentication

### The IPC secret

The desktop app passes a per-launch secret with `--ipc-secret-stdin`. Development
builds can use `--ipc-secret-env` instead.

- The secret arrives hex-encoded and is decoded to bytes.
- With `--ipc-secret-env`, the environment variable is unset right after it is read.
- The secret must be at least 32 bytes.
- It lives only in memory as `ipc.Secret`, is never logged, and is zeroed by
  `Wipe()` on exit.

The desktop app checks `secret_hash` in the handshake file to confirm the runtime
holds the right secret.

### HMAC check

Every request except the probes below is HMAC-SHA256 authenticated in `hmacAuth`.
The canonical string is:

```text
METHOD "\n" REQUEST_URI "\n" TIMESTAMP "\n" NONCE "\n" hex(sha256(BODY))
```

The signature arrives in `X-HC-Sig` as hex. `hmacAuth` also enforces these rules:

- The timestamp must be within 60 seconds of now.
- A sliding-window nonce cache rejects replays.
- The body is capped at 8 MiB before it is buffered for the HMAC, so a caller cannot
  stream an unbounded body.
- The comparison is constant-time.

A WebSocket upgrade cannot always carry custom headers. For upgrades the signature
travels in `Sec-WebSocket-Protocol` as `hc.v1.<sig>.<ts>.<nonce>`.

The paths `/healthz`, `/readyz`, `/metrics` and `/version` need no HMAC. They return
only coarse health data and are reachable only on loopback. `/version` returns a
coarse version string, never the exact commit, so an unauthenticated caller cannot
fingerprint the build.

### Middleware and origin policy

`withMiddleware` wraps the router in this order: `loopbackOnly`, `corsBridge`,
`requestID`, `recoverer`, `hmacAuth`.

| Middleware | Behaviour |
| --- | --- |
| `loopbackOnly` | Rejects non-loopback peers and any `Host` other than `127.0.0.1`, `localhost` or `[::1]`. Blocks DNS rebinding. |
| `corsBridge` | Allows credentialed CORS only for `file://`, `http://localhost:5173` and `http://127.0.0.1:5173`. Answers OPTIONS preflights before the HMAC layer. Does not accept the `null` origin. |
| `requestID` | Tags each request with an ID. |
| `recoverer` | Turns a handler panic into a 500 instead of crashing. |
| `hmacAuth` | Checks the signature as described above. |

The WebSocket upgraders also check the `Origin` header against loopback, as a second
layer on top of the signed upgrade. The console proxy uses `originIsLocal` and the
presence socket uses `wsOriginIsLocal`.

## Routes

The console-related routes are:

| Route | Purpose |
| --- | --- |
| `POST /v1/sessions` | Open a session |
| `GET /v1/sessions` | List sessions |
| `DELETE /v1/sessions/{sid}` | Close a session |
| `/v1/sessions/{sid}/stream` | Console WebSocket, the proxy entry point |
| `GET /healthz` | Liveness |
| `GET /readyz` | Readiness. Returns 503 until a plugin is loaded. |
| `GET /metrics` | Prometheus text metrics |
| `GET /version` | Coarse version string |

The renderer reaches these routes only through the SDK (`@ahv-remote-console/sdk`).
The [IPC reference](../reference/ipc.md) lists the full surface.

## Proxy lifecycle

`sessionStream` resolves the session and calls `HandleStream` or
`HandleStreamWithRole`. On entry the proxy does the following:

- It upgrades the renderer socket with `permessage-deflate` off, 1 MiB read and write
  buffers, and the `hc.console.v1` subprotocol.
- It sets an 8 MiB read limit on the renderer side, and applies `TCP_NODELAY` and
  DSCP-EF to the socket.
- It calls `sess.StreamConnected()` and `StreamDisconnected()` around the stream's
  life, so the reaper can tell a live console from an abandoned one.
- It roots its work in a context joined to both the request (renderer drop) and the
  host's shutdown context.

The session then runs as one loop in `run()`:

```text
dial upstream -> RFB 3.8 handshake -> start 3 pumps -> wait
                                                        |
        upstream failure: pumps exit, teardown, dial again, handshake again
```

### Dial

`dialUpstream` calls `sess.OpenStream`, the plugin's RFB dial, for a fresh upstream
socket on every connect. On each dial it:

- sets the 160 MiB upstream read limit,
- resets `upstreamPongSeen` to false, so each connection must answer a ping before
  the half-open watchdog arms,
- installs a pong handler that measures upstream round-trip time and arms the rolling
  read deadline,
- sets `TCP_NODELAY`, DSCP-EF, `TCP_QUICKACK` and TCP keepalive on the socket.

### Handshake

`pumpHandshake` runs the RFB 3.8 handshake. It reads from server and client in turn
and forwards each message. A `HandshakeTracker`
([`internal/rfb/handshake.go`](../../apps/runtime/internal/rfb/handshake.go)) follows
the states ProtocolVersion, SecurityTypes, SecurityResult, ClientInit and
ServerInit.

The tracker handles security strictly:

- Prism authenticates through the WebSocket and offers only security type 1 (None).
  The tracker accepts only type 1. Anything else moves it to the terminal `HsError`
  state.
- A non-zero `SecurityResult`, a zero `SecurityTypes` count or an unsupported chosen
  type also moves it to `HsError`. It first consumes the RFB 3.8 reason string, so
  the stream stays byte-aligned.
- In `HsError`, `Failing()` reports true and `Done()` stays false. The proxy never
  moves a desynced or failed stream into normal message flow.
- Length fields are computed as `int64` and bounds-checked once. A `uint32` length
  cannot overflow a 32-bit `int` on windows/386.

After the handshake, the bytes-per-pixel value from ServerInit goes to the stream
parser in [`internal/rfb/stream.go`](../../apps/runtime/internal/rfb/stream.go), so
it can walk variable-length encodings.

Each handshake has a 30-second deadline (`handshakeDeadline`). A renderer that does
not complete the handshake after a reset cannot hang the proxy.

### Pumps and teardown

After the handshake, `run()` starts three goroutines:

| Goroutine | Direction | Work |
| --- | --- | --- |
| `clientToUpstream` | Renderer to Prism | Reads renderer frames, filters or rewrites some, writes upstream |
| `upstreamToClient` | Prism to renderer | Reads into a reused buffer, inspects or rewrites some binary frames, writes to the renderer |
| `heartbeat` | Both | Pings, stats, clipboard request retries, keep-warm |

`run()` waits on all three. When one exits, `teardown()` unblocks the others.

`teardown` sets an immediate read deadline on both sockets to release their blocked
reads. It does not close the renderer socket, because the session continues across
an upstream reconnect. It is idempotent, and each pump defers it, so whichever pump
fails first releases the other.

Both pumps recover from panics and mark the session terminal instead of crashing the
runtime. The proxy treats the upstream RFB stream as untrusted.

## Reconnect and failure handling

### Every reconnect runs the handshake again

A reconnect is a new VNC connection. There is no resumption, so the RFB handshake
must run again.

On the first connect, `run()` sends one `connected` control frame. On every later
iteration it:

1. calls `p.hs.Reset()` to reset the handshake tracker,
2. clears the renderer read deadline so handshake reads block normally,
3. sends `rfb_reset` and then `resumed`, so the renderer replaces its RFB client. The
   new client sends `SetEncodings` again, which triggers the Extended Clipboard
   injection again,
4. resets per-connection state: `extClipCapsSent`, any pending clipboard request
   deadline and attempts, and the keep-warm probe budget and next-send time.

> [!WARNING]
> Do not add a reconnect path that dials again without a new handshake. It desyncs
> the whole stream after a guest reboot. Without `HandshakeTracker.Reset()`, `Done()`
> stays true from the first connect, and the new handshake bytes are parsed as
> normal messages.

### Reconnect or give up

`run()` separates recoverable upstream loss from terminal conditions:

| Condition | Detection | Behaviour |
| --- | --- | --- |
| VM powered off | `isPoweredOffErr` | Not a failure. Sends `vm_powered_off`, resets the failure count, polls every 3 s while the renderer stays connected. |
| VM starting | `isStartingErr` (upstream HTTP 5xx) | Sends `vm_starting`, backs off and retries. Still bounded by `maxReconnectFailures`. |
| First dial fails | | Fatal. Classifies the error and closes. |
| Terminal cause on reconnect | `classifyFatal` | Fails at once with the specific cause instead of retrying into `unreachable`. |
| Transient failure | | Sends `reconnect_attempt`, backs off with jitter, retries up to 60 times. |

Terminal causes are an expired or absent session, an RBAC denial, a deleted VM, a
closed session, and a permanent TLS or configuration fault.

`classifyFatal` does not match generic `x509` or `certificate` text. Those errors can
be transient on an endpoint without a pin, for example during a load balancer
failover or clock skew after wake. Only the deterministic fail-closed faults
("pinned spki", "tls verification disabled") are terminal.

Backoff grows exponentially from 500 ms to 30 s with plus or minus 25 percent jitter.
The jitter comes from a real PRNG. A clock-based value would collapse to a constant on
macOS's coarse clock, and many goroutines released by one event, such as a runtime
restart or a VPN flap, would dial Prism in lockstep.

## Control frames

Control frames are JSON in WebSocket text frames. Binary frames carry RFB. The wire
shape is the Go `ctrlFrame` struct, sent by `sendCtrl`.

| `type` | When | Fields |
| --- | --- | --- |
| `connected` | First successful dial and handshake | |
| `rfb_reset` | Every reconnect, before the new handshake | |
| `resumed` | Every reconnect, after `rfb_reset` | |
| `reconnecting` | A live session dropped, or a handshake retry | |
| `reconnect_attempt` | A transient dial retry | `reason`, `attempt`, `max` |
| `vm_powered_off` | Upstream reports the VM is off | |
| `vm_starting` | Upstream 5xx while the console starts | |
| `heartbeat` | On connect, then about every second | `latency_ms`, `bytes_in`, `bytes_out` |
| `clipboard_failed` | A guest copy could not be delivered | `reason` |
| `fatal` | Terminal give-up | `cause` |
| `pong` | Reply to a renderer `{"type":"ping"}` | |

The only frame the renderer sends is `{"type":"ping"}`, which `handleClientControl`
answers with `pong`.

### Fatal causes

A `fatal` frame carries a stable `cause`, so the renderer can offer the right
recovery.

| Cause | Meaning | Source |
| --- | --- | --- |
| `session_expired` | Session cookie dead or absent. The renderer offers SSO re-auth in the console. Also raised mid-session when an active stream's cookie expires. | `classifyFatal`, `run()` |
| `rbac_denied` | Not authorized | `classifyFatal` |
| `vm_not_found` | VM deleted, or gone from the cluster | `classifyFatal` |
| `session_closed` | The runtime already closed the session: tab closed, reaper, or `DELETE` | `classifyFatal` |
| `config_error` | Pinned-SPKI mismatch, or `allow_insecure_tls` without a pin | `classifyFatal` |
| `unreachable` | Retries exhausted, or the console never became ready | `run()` |
| `frame_too_large` | An upstream frame exceeded the 160 MiB read cap | `upstreamToClient` |

`closeFatal` sends the `fatal` frame and a WebSocket close frame with the cause as
the close reason. It then waits 150 ms so the renderer receives both before the
socket closes. The close code is 1008 (`ClosePolicyViolation`) for
`session_expired`, `rbac_denied`, `vm_not_found`, `session_closed` and
`config_error`, and 1009 (`CloseMessageTooBig`) for `frame_too_large`. The renderer
does not retry these codes. See [Desktop app](desktop.md#pop-out-renderer).

## Heartbeat and liveness

The `heartbeat` goroutine runs two tickers:

- A 15-second liveness tick (`heartbeatInterval`).
- A 1-second tick for stats, clipboard request retries and keep-warm.

Renderer liveness uses a WebSocket ping every 15 seconds with a 30-second pong
deadline (`pongDeadline`). A late pong is ignored once the connection is tearing
down, so it cannot race `teardown`.

The same 15-second tick pings the upstream with an RFC3339Nano timestamp. The pong
handler reads the echo to measure the real runtime-to-Prism round trip. The renderer
ping is on loopback and near zero, so the upstream value is the useful latency. If
the upstream ping write fails, the heartbeat tears the session down and `run()` dials
again.

`sendStats` sends a `heartbeat` control frame with `latency_ms` and cumulative
`bytes_in` and `bytes_out`. It sends one at once on each connect, then one per second,
so the console's byte counter updates every second. A sub-millisecond link reports
1 ms, because 0 means "not measured".

Byte counts are the session totals in `SessionStats.BytesFromClient` and
`BytesFromUpstream`. Both pumps increment them atomically.

### Half-open upstream watchdog

The proxy also guards against a half-open upstream, a dead socket that still looks
connected.

- The watchdog arms only after the upstream has answered at least one ping
  (`upstreamPongSeen`). A Prism that never answers keeps a blocking read with no
  deadline, so an idle but healthy VM does not reconnect by mistake.
- Once armed, every upstream read or pong extends a rolling read deadline
  (`upstreamReadDeadline`, default `pongDeadline`). A silent upstream then fails a
  read within that window, and `run()` dials again.
- For an upstream that never answers pings and also drops traffic, for example a VPN
  black hole or a NAT idle drop, TCP keepalive is set to a 30-second period
  (`setUpstreamKeepAlive`).

## Session reaping

The plugin host owns sessions, not the proxy. Source:
[`apps/runtime/internal/plugins/host.go`](../../apps/runtime/internal/plugins/host.go).
A session outlives any single stream, so the renderer can attach to the same session
ID after a short drop. When `run()` ends, `sessionStream` does not close the plugin
session. Closing it would make the renderer's reopen return 404, and the console
would stay "reconnecting".

The reaper cleans up sessions the renderer abandoned without a
`DELETE /v1/sessions/{sid}`, for example after a crash:

- Each session tracks an attached-stream count and a `lastSeen` timestamp. Both pumps
  call `Touch()` on every frame.
- Every 30 seconds the reaper closes any session with no attached stream whose
  `lastSeen` is older than 10 minutes.
- The 10-minute grace covers a laptop sleep or VPN change. A deliberate window close
  still sends `DELETE` at once.
- The reaper picks candidates without a lock. `reapIfStillIdle` then checks
  `StreamAttached()` and `lastSeen` again under the write lock before it deletes, so
  a session that reconnected in between is kept.

The host allows at most 16 concurrent sessions.

## Data path

### Network settings

- `permessage-deflate` is off on both the renderer upgrade and the upstream dial.
  Console traffic compresses poorly and the CPU cost adds latency.
- WebSocket read and write buffers are 1 MiB.
- `TCP_NODELAY` is set on both sides, so a 6-byte pointer event does not wait for
  Nagle's 40 ms delay.
- DSCP-EF (TOS `0xB8`) marks the flow as low-latency, the same class as VoIP.
- `TCP_QUICKACK` turns off delayed ACKs on Linux.
- DSCP and `TCP_QUICKACK` are best-effort and do nothing on Windows.

Source: [`proxy_sockopt_unix.go`](../../apps/runtime/internal/proxy/proxy_sockopt_unix.go),
[`proxy_sockopt_windows.go`](../../apps/runtime/internal/proxy/proxy_sockopt_windows.go).

### Buffers and size limits

`upstreamToClient` reuses one `bytes.Buffer` for every message. A new buffer per
message at 30 fps would allocate megabytes per second and cause GC jitter. The pump
owns the buffer, and each frame is written to the renderer before the next read.

After a large raw frame, the buffer is released once its capacity exceeds 8 MiB
(`upstreamReadBufShrinkCap`). A rare 160 MB frame then does not hold that memory for
the rest of the session.

The slice `handleServerBinary` returns points into the reused buffer. Nothing may
keep it past the loop iteration. Any rewritten frame must be a new slice.

| Direction | Limit | Reason |
| --- | --- | --- |
| Renderer to proxy | 8 MiB (`maxMessageSize`) | Input messages are tiny. The cap bounds a misbehaving local sender. |
| Upstream to proxy | 160 MiB (`maxUpstreamMessageSize`) | Covers a full raw framebuffer at the 8K limit. |

When an upstream frame exceeds 160 MiB, the pump detects `websocket.ErrReadLimit` and
sends a `frame_too_large` fatal. It does not dial again into the same frame.

### What the proxy inspects

The proxy does not decode pixel data. The renderer does that. `FramebufferUpdate`
(type 0) and `Bell` (type 2) pass through unchanged.

On the renderer side, `handleClientBinary`:

- adds the Extended Clipboard pseudo-encoding to `SetEncodings` and appends a one-time
  CAPS response (`rfb.InjectExtendedClipboard`, `rfb.BuildExtClipCapsResponse` in
  [`internal/rfb/extclip.go`](../../apps/runtime/internal/rfb/extclip.go)),
- removes `KeyEvent`, `PointerEvent` and QEMU client messages from a non-controller
  when presence is on. A frame that becomes empty is dropped,
- passes `SetDesktopSize` through unchanged. AHV's stdvga ignores it.

On the upstream side, `handleServerBinary`:

- turns an Extended Clipboard `ServerCutText` (negative length, zlib PROVIDE) into a
  legacy `ServerCutText` the renderer understands (`rfb.TranslateExtClipServerCutText`),
- answers a NOTIFY with a REQUEST,
- reports `clipboard_failed` when a PROVIDE cannot be decoded, instead of losing the
  copy silently.

`ClientCutText` from host to guest is not changed. QEMU routes it to the vdagent
serial channel.

AHV splits large Tight `FramebufferUpdate` messages across WebSocket frames. A
continuation frame can start with `0x03` and have a negative int32 at bytes 4 to 8,
which looks like Extended Clipboard. The proxy treats a frame as Extended Clipboard
only if it has the zero 3-byte padding and a complete message. It never drops a frame
it is unsure about. It forwards it unchanged.

If a guest-copy PROVIDE does not arrive, `clipReqDeadline` arms a resend.
`maybeRetryClipRequest` runs about once per second and resends the REQUEST up to 3
times before it reports `clipboard_failed`.

## Keep-warm lease

QEMU's adaptive `vnc_refresh` timer backs off from 30 ms to 3000 ms on an idle
framebuffer, so the first frame after idle is seconds old. While a console stream is
attached, the agent keeps the display slightly dirty by toggling one pixel's lowest
bit. It does this under a lease that the proxy arms and renews.

The lease uses the clipboard side channel, the same channel as resize tokens. Its
tokens are command markers, never operator clipboard data.

> [!NOTE]
> The proxy sends lease tokens for every guest, but only Windows guests act on them.
> On Linux and macOS the agent's `newPlatformOp` is a no-op
> ([`op_other.go`](../../apps/ahv-remote-console-agent/internal/keepwarm/op_other.go)).
> The lease arms, the platform operation fails, and the agent marks itself
> unsupported. The console works as usual, without the refresh floor.

The protocol:

1. Until the agent shows it understands keep-warm, the proxy sends only
   `__AHVRC_RESIZE__keepwarm-probe` (`keepwarmProbeToken`). The probe sits in the
   resize namespace because every released agent intercepts `__AHVRC_RESIZE__` and
   discards a payload it cannot parse. An old agent therefore ignores it. The proxy
   sends at most 10 probes per upstream connection (`keepwarmMaxProbes`).
2. A current agent answers `__AHVRC_KEEPWARM_ACK__` (`keepwarmAckText`) in an
   ordinary clipboard transfer. The proxy swallows the ack. It is not forwarded to
   the renderer, not written to the host clipboard and not logged. The proxy records
   `keepwarmAcked`.
3. After the ack, the proxy sends lease tokens. `__AHVRC_KEEPWARM__45` arms or extends
   a 45-second lease. `__AHVRC_KEEPWARM__0` cancels it.
4. `maybeSendKeepwarm` runs on the 1-second tick and sends a token every 20 seconds
   (`keepwarmRenewInterval`), well inside the 45-second lease.
5. On a terminal detach, such as shutdown, a closed console or re-auth,
   `sendKeepwarmCancel` sends the cancel token on a best-effort basis. The agent then
   stops at once instead of waiting out the lease. A reconnect does not cancel. The
   lease persists across dials.

The side channel must never delay a real copy. `maybeSendKeepwarm` waits in two
cases:

- while a guest-to-host Extended Clipboard transfer is in progress
  (`clipReqDeadline` armed),
- for 3 seconds after any real clipboard traffic in either direction
  (`keepwarmClipQuiet`, tracked by `markClipActivity` in `lastClipActivity`).

A delayed send retries on the next 1-second tick. The 45-second lease absorbs the
delay.

QEMU's clipboard manager sends one console's cut text to every attached peer. A
sibling console's keep-warm tokens can therefore arrive as if they were guest
clipboard. `isKeepwarmEcho` recognizes and swallows them without recording activity
or an ack.

## Shutdown

Graceful shutdown drains live consoles promptly instead of waiting for the full
grace period:

1. `host.BeginShutdown()` cancels the host's base context. Each proxy's context
   derives from both the request and this base context. `http.Server.Shutdown` does
   not cancel a hijacked WebSocket request's context, so this step is what releases
   active consoles. The per-connection watcher arms `teardown`'s immediate deadline
   and the pumps stop.
2. The network-state poll is cancelled.
3. `srv.Shutdown(ctx)` returns quickly because the pumps have drained. Its 10-second
   timeout is only an upper bound.

When the parent context is cancelled, the proxy also sends a best-effort
`CloseGoingAway` frame. The renderer then shows a clean "disconnected" state instead
of a dropped socket. The reaper goroutine stops on the same cancel, and shutdown
waits for it through `reaperDone`.

## Constants

| Constant | Value | File |
| --- | --- | --- |
| `heartbeatInterval` | 15 s | `proxy.go` |
| `pongDeadline` | 30 s | `proxy.go` |
| `writeDeadline` | 15 s | `proxy.go` |
| `minBackoff`, `maxBackoff` | 500 ms, 30 s | `proxy.go` |
| `maxMessageSize` (renderer) | 8 MiB | `proxy.go` |
| `maxUpstreamMessageSize` | 160 MiB | `proxy.go` |
| `upstreamReadBufShrinkCap` | 8 MiB | `proxy.go` |
| `handshakeDeadline` | 30 s | `proxy.go` |
| `maxReconnectFailures` | 60 | `proxy.go` |
| `poweredOffPollInterval` | 3 s | `proxy.go` |
| Keep-warm lease, renew, quiet | 45 s, 20 s, 3 s | `proxy.go` |
| `keepwarmMaxProbes` | 10 | `proxy.go` |
| HMAC clock skew | 60 s | `server.go` |
| IPC body cap | 8 MiB | `server.go` |
| `maxConcurrentSessions` | 16 | `host.go` |
| `sessionIdleTTL` | 10 min | `host.go` |
| `reaperInterval` | 30 s | `host.go` |

## Known limitations

- Keep-warm has no effect on non-Windows guests.
- The TypeScript `ControlFrame` union in `packages/protocol` covers only part of the
  Go `ctrlFrame`. Treat the Go side as the contract.
- DSCP marking and `TCP_QUICKACK` do nothing on Windows. `TCP_QUICKACK` also does
  nothing on macOS.

## Related documentation

- [Architecture overview](overview.md)
- [Desktop app](desktop.md)
- [AHV plugin](ahv-plugin.md)
- [Clipboard](clipboard.md)
- [Agent](agent.md)
- [IPC reference](../reference/ipc.md)
- [Protocol reference](../reference/protocol.md)
