# Writing a runtime plugin

This guide shows how to write a runtime plugin against the public SDK in
[`apps/runtime/pkg/sdk`](../../apps/runtime/pkg/sdk/sdk.go). It is for Go developers
who want to add support for another upstream platform. The reference implementation
is the AHV plugin in [`plugins/ahv`](../../plugins/ahv/plugin.go).

Background reading: [architecture overview](../architecture/overview.md),
[runtime proxy](../architecture/runtime-proxy.md), [AHV plugin](../architecture/ahv-plugin.md)
and the [wire protocol](../reference/protocol.md).

## Contents

- [What a plugin is](#what-a-plugin-is)
- [Lifecycle](#lifecycle)
- [The Plugin interface](#the-plugin-interface)
- [Optional capability interfaces](#optional-capability-interfaces)
- [Linking a plugin into the runtime](#linking-a-plugin-into-the-runtime)
- [Endpoints, credentials and logging](#endpoints-credentials-and-logging)
- [The stream contract](#the-stream-contract)
- [Worked example](#worked-example)
- [Testing](#testing)
- [Compatibility](#compatibility)
- [Open questions](#open-questions)

## What a plugin is

A plugin is a Go value that implements `sdk.Plugin` for one upstream platform. It is
linked into the runtime binary. There is no out-of-process loader.

The host passes an `sdk.Dependencies` value to `Init`:

| Field | What it provides |
| --- | --- |
| `Logger` | A `slog` logger tagged `plugin=<id>` |
| `Keystore` | Secret storage. Keys are prefixed `plugin/<id>/` |
| `Telemetry` | Tracing |
| `Config` | Read-only endpoints and CA bundle paths |
| `Version` | The runtime version |
| `OnSessionsExpired` | Callback that sends sessions to the re-auth path |
| `NetState` | Always `nil` today. Treat it as possibly nil |

The host adapters are in
[`apps/runtime/internal/plugins/adapters.go`](../../apps/runtime/internal/plugins/adapters.go).

## Lifecycle

1. **Registration.** The package `init()` calls `sdk.Register`. This runs at import
   time, before the runtime starts.
2. **Load.** `host.LoadAll()` runs from the runtime's
   [`main.go`](../../apps/runtime/cmd/ahv-remote-console-runtime/main.go) after the
   handshake file is written. For each builder it calls `Manifest()`, then `Init` with
   a 10-second context. If `Init` returns an error, the plugin is left out and the
   error is added to the load error. Other plugins still load.
3. **Endpoints.** The host never pushes endpoints. The plugin reads
   `deps.Config.Endpoints()` when it needs them. `Dependencies` has no change
   callback. The AHV plugin reads them in its endpoint lookup and in `ListResources`.
4. **Sessions.** `OpenSession` returns a `SessionInfo`. The host stores it and creates
   an ID if the plugin left `ID` empty. It refuses new sessions once 16 are open.
5. **Stream.** The proxy calls `OpenStream` on the first connect and on every
   reconnect. See [The stream contract](#the-stream-contract).
6. **Teardown.** A renderer `DELETE` reaches `CloseSession` through the IPC router. A
   reaper scans every 30 seconds and closes sessions that have no stream attached and
   have been idle for 10 minutes.
7. **Shutdown.** `BeginShutdown` cancels the base context. `main.go` then calls
   `host.Shutdown` with a 10-second context. The host closes every open session, then
   calls each plugin's `Shutdown`.

Source: [`apps/runtime/internal/plugins/host.go`](../../apps/runtime/internal/plugins/host.go),
[`apps/runtime/internal/ipc/router.go`](../../apps/runtime/internal/ipc/router.go).

## The Plugin interface

The SDK states that all methods may run concurrently, plugins must be thread-safe,
and plugins must honour `ctx` cancellation.

| Method | Contract | AHV plugin |
| --- | --- | --- |
| `Manifest() Manifest` | Returns metadata. The comment says it is called once at startup, but the host calls it several times. Keep it cheap | `Manifest` in `plugin.go` |
| `Init(ctx, Dependencies) error` | An error leaves the plugin unregistered; others still load. The context has a 10 s deadline | Starts a keep-alive goroutine |
| `Shutdown(ctx) error` | Best effort, with a host timeout. Errors are logged, not fatal | Cancels background work and waits at most 5 s |
| `AuthStart(ctx, json.RawMessage) (any, error)` | Starts a credential flow. The body is plugin-specific JSON | Decodes the body and dispatches on `cookie` |
| `AuthComplete(ctx, json.RawMessage) (any, error)` | Finishes an IdP redirect or device flow. Tokens go to the Keystore | Returns an error; the cookie flow finishes in `AuthStart` |
| `AuthRefresh(ctx) error` | Called before expiry and after an upstream 401 | No-op |
| `Logout(ctx) error` | Removes stored credentials | Deletes Keystore entries and clears memory |
| `ListResources(ctx, ListQuery) (ListResult, error)` | Returns one page for the query. The SDK says nothing about nil slices | Always returns a non-nil slice, so the renderer never sees `null` |
| `RefreshResources(ctx) error` | Forces an upstream refresh and drops cached pages | `RefreshResources` in `plugin.go` |
| `OpenSession(ctx, resourceID) (SessionInfo, error)` | Called once when the operator picks a resource. `ID` is the session handle | Creates a UUID |
| `OpenStream(ctx, sessionID) (*websocket.Conn, error)` | Dials the upstream for each connection | See [The stream contract](#the-stream-contract) |
| `CloseSession(ctx, sessionID) error` | Called on renderer close, reap and shutdown. Must accept an unknown ID | Deletes the map entry and returns nil |

Rules for every method:

- **Errors.** Wrap errors with `%w`. Keep credentials out of error text. The telemetry
  adapter records `err.Error()` as a span attribute, and the proxy classifies errors
  by substring.
- **Context.** Honour `ctx`. Give every upstream call its own timeout. The AHV
  keep-alive uses 10 seconds.
- **Concurrency.** The plugin protects its own state. The AHV plugin uses a
  `sync.RWMutex` and a separate `tasksMu`. The host serializes only the dial for each
  session.

## Optional capability interfaces

A plugin adds features by implementing optional interfaces. The host detects them
with a type assertion where it needs them. Nothing is registered. The AHV plugin
asserts some of them at compile time with lines such as
`var _ sdk.HealthCapable = (*Plugin)(nil)`.

| Interface | Methods | Used by |
| --- | --- | --- |
| `HealthCapable` | `EndpointHealth` | Endpoint health route. Returns 503 `health_unsupported` if absent |
| `IdentityCapable` | `WhoAmI` | Identity route |
| `PowerActionCapable` | `PowerAction` | Power action route |
| `SerialPortCapable` | `AddSerialPort` | Serial port route |
| `SnapshotCapable` | `CreateSnapshot`, `ListSnapshots`, `RestoreSnapshot` | Snapshot routes |
| `StatsCapable` | `GetVMStats` | VM stats route |
| `TaskCapable` | `ListTasks`, `GetTask` | Task routes |
| `ISOMountCapable` | `MountAgentISO` | Agent ISO mount route |
| `ImageCatalogCapable` | `ListISOImages`, `MountISO`, `EjectISO` | Image catalog routes |

The routes are in the IPC router. Binding rules from the SDK comments:

- `StatsCapable` is read-only. It records no evidence and needs no confirmation.
- `SnapshotCapable.RestoreSnapshot` does not prompt. The caller must already have the
  operator's confirmation.
- `HealthCapable` must be an unauthenticated reachability probe. It sends no
  credentials and reads no response bodies.
- `TaskInfo.Label`, `Detail` and `Error` must never contain clipboard data or
  credentials.

`Manifest.Capabilities` is a free-form list of strings. The AHV plugin uses names
such as `console.vnc`. The host reports these strings.

## Linking a plugin into the runtime

1. Put the plugin in its own package and call `sdk.Register` from `init()`. `LoadAll`
   calls each builder once.
2. Add a blank import to
   [`apps/runtime/internal/plugins/registration.go`](../../apps/runtime/internal/plugins/registration.go).
   That one line is the whole change. The AHV line is:

   ```go
   _ "github.com/MarcoCarvin/ahv-remote-console/plugins/ahv"
   ```

3. Import only `apps/runtime/pkg/sdk`. Go's `internal` rule allows
   `apps/runtime/internal/...` only from code under `apps/runtime`, and `plugins/` is
   outside that tree. The host's own type aliases are in
   [`apps/runtime/internal/plugins/api.go`](../../apps/runtime/internal/plugins/api.go).

## Endpoints, credentials and logging

- **Endpoint fields.** `sdk.EndpointRef` has `ID`, `URL`, `PluginID`, `AuthMethods`,
  `PinnedCertB64`, `AllowInsecureTLS` and `Tags`. An endpoint with no auth methods is
  treated as cookie-only. Check `PluginID` before you use an endpoint, as the AHV
  plugin does.
- **TLS.** Certificate pins and the insecure-TLS flag come with the endpoint. Read CA
  bundle paths from `CustomCAFiles()` on every call. The AHV plugin reads them on each
  dial and fails closed on a pin mismatch.
- **Credentials.** Store credentials through `Keystore`. The AHV plugin stores its SSO
  cookies there. `Logout` deletes them and clears the copies in memory.
- **No credentials in logs.** The SDK forbids credentials and clipboard data in task
  text shown to operators. The AHV plugin also never logs upstream HTTP bodies,
  because the upstream firewall echoes request headers into 4xx responses.
- **Redaction is a fallback.** The `slog` handler in
  [`apps/runtime/internal/logging/logging.go`](../../apps/runtime/internal/logging/logging.go)
  replaces attributes whose keys look sensitive. `body` is not on its key list, so do
  not log bodies.

## The stream contract

`OpenStream(ctx, sessionID)` returns a WebSocket connection ready for the proxy, or an
error. The SDK comment says the connection must be "post-handshake". The AHV plugin
returns right after the WebSocket upgrade, and the proxy runs the RFB handshake on
each dial.

### Who calls it

The proxy dials through `sess.OpenStream`. The host wrapper holds the session lock for
the whole dial, so dials for one session run one at a time. The AHV dial uses a
20-second handshake timeout.

### Ownership

The proxy closes the connection on teardown. On a failed upgrade, the AHV plugin
closes the HTTP response body.

### Signalling errors

The proxy does not inspect error types. `classifyFatal` in
[`apps/runtime/internal/proxy/proxy.go`](../../apps/runtime/internal/proxy/proxy.go)
matches lower-case substrings of `err.Error()`:

| Substring | Cause sent to the renderer | Retried |
| --- | --- | --- |
| `session expired`, `no credentials available`, `session cookies missing` | `session_expired` | No |
| `not authorized` | `rbac_denied` | No |
| `vm not found` | `vm_not_found` | No |
| `session closed`, `unknown session` | `session_closed` | No |
| `pinned spki`, `tls verification disabled` | `config_error` | No |
| `http 500` to `http 504`, from `dial VNC websocket: HTTP %d` | "starting" state | Yes, quietly |
| `not powered on`, `powered off` | "powered off" state | Yes, by polling |

Use these exact strings in your errors. The AHV plugin emits them.

Retry behaviour:

- Powered-off and starting errors are retried on the first dial too.
- Any other first-dial error sends its classified cause, or `unreachable` if nothing
  matched.
- On a reconnect, a terminal cause stops the loop at once.
- Other reconnect errors back off and retry until the failure budget runs out, then
  close as `unreachable`.

### Closing

The plugin never sends frames to the renderer. The proxy sends the `fatal` control
frame and the WebSocket close code.

### Re-auth

If a keep-alive finds that credentials have stopped working, call
`deps.OnSessionsExpired` with the session IDs. The host signals those sessions, and
the proxy sends `session_expired` before the stream drops. The session stays open, so
the same ID can reconnect after re-auth.

## Worked example

This file compiles against the SDK. It implements all twelve `Plugin` methods and
registers itself. It has been built but not run. To use it, put it in its own package
and add a blank import to `registration.go`.

```go
package echoplugin

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"

	"github.com/MarcoCarvin/ahv-remote-console/apps/runtime/pkg/sdk"
)

const pluginID = "example-echo"

var sessionSeq atomic.Uint64

func init() {
	sdk.Register(func() sdk.Plugin { return &Plugin{} })
}

// Plugin is a minimal sdk.Plugin.
type Plugin struct {
	deps sdk.Dependencies
}

var _ sdk.Plugin = (*Plugin)(nil)

func (p *Plugin) Manifest() sdk.Manifest {
	return sdk.Manifest{
		ID:           pluginID,
		Name:         "Example Echo",
		Version:      "0.1.0",
		Description:  "Minimal plugin for the SDK guide.",
		Vendor:       "Example",
		Capabilities: []string{"discovery.vms"},
	}
}

func (p *Plugin) Init(_ context.Context, deps sdk.Dependencies) error {
	if deps.Logger == nil || deps.Config == nil {
		return errors.New("runtime did not provide Logger and Config")
	}
	p.deps = deps
	deps.Logger.Info("example plugin initialised")
	return nil
}

func (p *Plugin) Shutdown(_ context.Context) error { return nil }

func (p *Plugin) AuthStart(_ context.Context, _ json.RawMessage) (any, error) {
	return nil, errors.New("auth not implemented")
}

func (p *Plugin) AuthComplete(_ context.Context, _ json.RawMessage) (any, error) {
	return nil, errors.New("auth not implemented")
}

func (p *Plugin) AuthRefresh(_ context.Context) error { return nil }

func (p *Plugin) Logout(_ context.Context) error { return nil }

func (p *Plugin) ListResources(ctx context.Context, q sdk.ListQuery) (sdk.ListResult, error) {
	if err := ctx.Err(); err != nil {
		return sdk.ListResult{}, err
	}
	res := []sdk.Resource{{ID: "ep1/vm-1", Label: "demo VM", Status: "ON"}}
	return sdk.ListResult{Resources: res, Total: len(res), Page: q.Page, PageSize: q.PageSize}, nil
}

func (p *Plugin) RefreshResources(_ context.Context) error { return nil }

func (p *Plugin) OpenSession(_ context.Context, resourceID string) (sdk.SessionInfo, error) {
	id := fmt.Sprintf("sess-%d", sessionSeq.Add(1))
	return sdk.SessionInfo{ID: id, PluginID: pluginID, ResourceID: resourceID, OpenedAt: time.Now().UTC()}, nil
}

func (p *Plugin) OpenStream(ctx context.Context, sessionID string) (*websocket.Conn, error) {
	dialer := &websocket.Dialer{
		HandshakeTimeout: 20 * time.Second,
		TLSClientConfig:  &tls.Config{MinVersion: tls.VersionTLS13},
	}
	conn, resp, err := dialer.DialContext(ctx, "wss://upstream.example.invalid/console", nil)
	if err != nil {
		if resp != nil && resp.Body != nil {
			_ = resp.Body.Close()
		}
		return nil, fmt.Errorf("dial upstream for %s: %w", sessionID, err)
	}
	return conn, nil
}

func (p *Plugin) CloseSession(_ context.Context, _ string) error { return nil }
```

## Testing

The AHV tests use in-process fakes and `httptest` servers. None of them needs a live
Prism.

| Kind | Where |
| --- | --- |
| Stubs and fakes | `stubConfig` in [`health_test.go`](../../plugins/ahv/health_test.go), `recordingKeystore` in [`rotation_test.go`](../../plugins/ahv/rotation_test.go), `prismFake` and `newCyclePlugin` in [`mount_cycle_test.go`](../../plugins/ahv/mount_cycle_test.go), `newCatalogPlugin` in [`image_catalog_test.go`](../../plugins/ahv/image_catalog_test.go) |
| Health | `TestEndpointHealth_*` in `health_test.go` |
| Stats | `TestGetVMStats_HappyPath` in [`stats_test.go`](../../plugins/ahv/stats_test.go) |
| Stream preflight | `TestOpenStream_PreflightRefreshesStaleClusterUUID` in [`cluster_cache_test.go`](../../plugins/ahv/cluster_cache_test.go) |
| Keep-alive | `rotation_test.go` |
| Other plugin suites | [`capabilities_test.go`](../../plugins/ahv/capabilities_test.go), [`tasks_test.go`](../../plugins/ahv/tasks_test.go), [`deferred_serial_test.go`](../../plugins/ahv/deferred_serial_test.go), [`tlspin_test.go`](../../plugins/ahv/tlspin_test.go) |
| Host | `fakePlugin` and the reaper test in [`reaper_test.go`](../../apps/runtime/internal/plugins/reaper_test.go), [`reauth_test.go`](../../apps/runtime/internal/plugins/reauth_test.go), [`observability_test.go`](../../apps/runtime/internal/plugins/observability_test.go) |
| Proxy | [`reconnect_test.go`](../../apps/runtime/internal/proxy/reconnect_test.go) replaces `openUpstream` with a fake |

Run the plugin tests with the race detector:

```bash
go test -race -cover ./plugins/ahv/...
```

Run one test:

```bash
go test -race -run TestGetVMStats_HappyPath ./plugins/ahv/
```

See [Testing and CI](testing.md) for the other test commands.

## Compatibility

The package comment says the types and the `Plugin` interface "follow semantic
versioning. Breaking changes require a major-version bump". No other rules are stated.
See [Versioning](versioning.md).

Facts that affect compatibility:

- The SDK is part of the root Go module. There is no `apps/runtime/go.mod`.
- Adding a method to `Plugin` breaks every implementation, because Go interfaces are
  satisfied implicitly. The project has no stated policy for this.
- `Manifest` is documented as "called once", but the host calls it on load, for the host
  summary, on session open and for diagnostics. Keep it cheap.
- The `Registered` comment says later registrations are ignored. `Register` still
  appends, but the host reads the list once in `LoadAll`, so later calls have no
  effect on a running host.

## Open questions

- Whether the host changes any behaviour based on `Manifest.Capabilities` strings. It
  is only known to report them.
- Which handshake "post-handshake" in the `OpenStream` comment refers to.
- Whether anything besides the connection must be closed on `OpenStream` error paths.
- Whether the redaction in `logging.go` covers every log path. The handler was read,
  but not every logger was traced.
- Whether plugins can run out of process. No code path for it exists, and the AHV
  Makefile states that v1 is in-tree only.
- Whether the test suite passes on the current checkout. The tests were read, not run.
- Any compatibility policy beyond the package comment.

## Related documentation

- [AHV plugin](../architecture/ahv-plugin.md)
- [Runtime proxy](../architecture/runtime-proxy.md)
- [Protocol reference](../reference/protocol.md)
- [Testing and CI](testing.md)
- [Versioning](versioning.md)
