# Runtime

The Go runtime is a loopback-only HTTP and WebSocket service that brokers every call to Prism and proxies each VM console.

## Overview

The desktop app's main process starts the runtime at launch. It is the only
component that talks to Nutanix Prism and to the VM's VNC endpoint. The renderer
never reaches Prism directly. Every IPC call is authenticated with HMAC-SHA256
over a per-launch secret. See
[`docs/architecture/overview.md`](../../docs/architecture/overview.md) for the
whole system.

The runtime has four jobs:

- It is the loopback broker. It binds an ephemeral port on `127.0.0.1` and
  writes a handshake file, `runtime.json` (mode 0600), with the endpoint and the
  hash of the IPC secret. It rejects any non-loopback `Host` header and
  authenticates every request except the probes `/healthz`, `/readyz`,
  `/metrics` and `/version`.
- It proxies RFB/VNC. It bridges the session WebSocket
  (`/v1/sessions/{sid}/stream`) to the plugin's upstream VNC WebSocket. Each
  reconnect opens a fresh stream and repeats the RFB 3.8 handshake. Sessions are
  never resumed silently. The proxy also handles the RFB Extended Clipboard
  encoding.
- It hosts plugins. It opens and reaps console sessions. The AHV plugin is
  linked in at build time through a blank import. It is not loaded from disk.
- It keeps local state. Secrets live only in the OS keychain. It also keeps a
  hash-chained audit log, a JSON config and a network-state observer.

## Usage

`main.go` needs exactly one IPC secret source. The secret must be at least 32 bytes.

| Flag | Purpose |
| --- | --- |
| `--ipc-secret-stdin` | Read the secret from the first line of stdin. Used in production. |
| `--ipc-secret-env <NAME>` | Read the secret from an environment variable. For development. |
| `--log-level`, `--log-format`, `--log-file` | Logging. Defaults: `info`, `json`, stderr. |
| `--data-dir` | Data directory. |
| `--plugins-dir` | Plugins directory. Default: `<data-dir>/plugins`. |
| `--otlp-endpoint` | OTLP tracing endpoint. Overrides the configured value. |
| `--version` | Print the version and exit. |

Default data directories:

| OS | Path |
| --- | --- |
| Windows | `%APPDATA%\AHV Remote Console` |
| macOS | `~/Library/Application Support/AHV Remote Console` |
| Linux | `$XDG_DATA_HOME/ahvRemoteConsole` (falls back to `~/.local/share`) |

## Build and test

```bash
make runtime                  # from the repository root
make -C apps/runtime build    # CGO_ENABLED=0, -trimpath, version stamped
make -C apps/runtime test     # go test -race -cover ./...
make -C apps/runtime lint     # go vet and staticcheck
go test -race -run <TestName> ./apps/runtime/internal/proxy/...
```

`make -C apps/runtime dev` runs `go run` with debug text logging. It passes no IPC secret flag, so the runtime exits unless you add one. The desktop app's `pnpm dev` starts the runtime for you.

The build stamps version, commit and date into `internal/build` through `-ldflags`. The repository is one Go module with no per-app `go.mod`. `go.mod` pins the toolchain to `go1.26.10`.

## Structure

| Path | Contents |
| --- | --- |
| `cmd/ahv-remote-console-runtime/` | Entry point. Parses flags, loads the secret, writes the handshake, serves, shuts down. |
| `internal/ipc/` | Loopback server, CORS allowlist, `Host` check, HMAC, routes, nonces, peer checks. |
| `internal/proxy/` | Per-console VNC loop with heartbeat, reconnect, size caps, clipboard, keep-warm. |
| `internal/plugins/` | Plugin host. Opens, closes and reaps sessions, with a concurrency cap. |
| `internal/rfb/` | RFB 3.8 handshake, parser and Extended Clipboard codec. |
| `internal/keystore/` | OS keychain storage. No file fallback; fails closed. |
| `internal/audit/` | Append-only log with a SHA-256 hash chain. Clipboard action types exist but are not written yet. |
| `internal/config/` | `config.json` (0600) with endpoints, TLS pinning, telemetry and security flags. |
| `internal/handshake/` | Writes `runtime.json`. |
| `internal/netstate/`, `internal/presence/` | Network observer (2 s poll) and multi-viewer presence hub. |
| `internal/telemetry/`, `internal/logging/`, `internal/build/` | Opt-in OTLP tracing, slog setup, build info. |
| `pkg/sdk/` | Public plugin SDK with the `Plugin` interface and `Register()`. Semver-stable. |

## Related documentation

- [Runtime and proxy](../../docs/architecture/runtime-proxy.md)
- [Architecture overview](../../docs/architecture/overview.md)
- [Protocol reference](../../docs/reference/protocol.md)
- [IPC reference](../../docs/reference/ipc.md)
- [AHV plugin](../../docs/architecture/ahv-plugin.md)
