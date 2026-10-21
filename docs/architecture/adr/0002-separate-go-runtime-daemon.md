# ADR 0002: A separate Go runtime holds the Prism and VNC logic

## Status

Accepted (inferred from code; confirm)

## Context

The desktop app must talk to Prism Central and hold a long-lived VNC proxy for
each open console. Those connections must survive renderer reloads, sleep and VPN
gaps. The architecture overview describes the runtime as the only component that
talks to Prism, and the proxy as the part of the system that resilience depends
on.

The repository does not record why this logic lives outside the Electron main
process, or whether running the proxy inside main was considered.

## Decision

Electron main spawns and supervises one local Go process, the runtime in
`apps/runtime`. The runtime is a loopback-only HTTP and WebSocket broker. It hosts
the AHV plugin and runs the RFB/VNC proxy. The renderer never talks to Prism
directly.

Main starts the runtime, passes it the per-launch secret and verifies its
handshake. See [ADR 0003](0003-loopback-hmac-ipc.md) and
[ADR 0004](0004-renderer-never-holds-secret.md).

## Consequences

- The runtime owns console sessions, their upstream VNC connections, Prism calls
  and TLS pinning. The Node main process holds no cluster connections.
- Socket tuning is written in Go and applies to the runtime's own sockets.
  It sets TCP_NODELAY, DSCP, 1 MiB upgrade buffers and a 160 MiB read cap.
- The desktop app supervises a second process. `runtime-manager.ts` handles
  start, restart and handshake failures. The renderer rebuilds its SDK client
  when the runtime port changes.
- A runtime crash ends every open console until the runtime is respawned. The
  desktop app shows a runtime state and a restart path for this case.
- The runtime reaps idle sessions after a TTL. The proxy loop never closes the
  plugin session itself.

## References

- [Architecture overview](../overview.md)
- [Runtime proxy](../runtime-proxy.md)
- [`apps/desktop/src/main/runtime-manager.ts`](../../../apps/desktop/src/main/runtime-manager.ts)
- [`apps/runtime/internal/proxy/proxy_sockopt_unix.go`](../../../apps/runtime/internal/proxy/proxy_sockopt_unix.go)
- [`apps/runtime/internal/plugins/host.go`](../../../apps/runtime/internal/plugins/host.go)
