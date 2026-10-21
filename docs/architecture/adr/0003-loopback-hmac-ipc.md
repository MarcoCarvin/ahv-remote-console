# ADR 0003: Loopback HTTP and WebSocket with per-request HMAC

## Status

Accepted

## Context

The desktop main process and the Go runtime are two processes on one machine.
The runtime holds Prism credentials and cookies. Any other local caller that
reaches its port would get the same access. The runtime must never be reachable
from outside 127.0.0.1, and every IPC call must be authenticated with
HMAC-SHA256 using a secret that exists only for one launch. WebSocket upgrades carry the signature in the `Sec-WebSocket-Protocol` header
rather than a custom header, because some proxies strip custom headers from
upgrade requests. The repository does not record the alternatives considered,
such as a Unix domain socket or mutual TLS on loopback.

## Decision

- The runtime listens on `127.0.0.1` on a port the OS assigns. It rejects a
  non-loopback peer and a non-loopback Host header.
- Main generates a 48-byte random secret per launch and writes it as hex to the
  runtime's stdin. The runtime writes a handshake file with its endpoint and
  `base64url(sha256(secret))`. Main checks that hash before it trusts the runtime.
- Each request carries an HMAC-SHA256 over the method, URI, timestamp, nonce and
  body SHA-256. The timestamp must be within 60 seconds. A sliding nonce window
  blocks replay. WebSocket upgrades carry the same values in the subprotocol
  `hc.v1.<sig>.<ts>.<nonce>`.
- Liveness, readiness, metrics and version probes are exempt from HMAC.

## Consequences

- A local caller without the per-launch secret cannot produce a valid signature.
  The renderer does not hold it ([ADR 0004](0004-renderer-never-holds-secret.md)).
- The renderer must echo the negotiated subprotocol on the upgrade, or the
  handshake fails.
- Main refuses a stale or wrong runtime when the hash does not match. After the
  handshake, main zeroes the buffer it used to spawn the runtime.
- The nonce cache holds 8192 entries over a two-minute window (`2*skew`). Older
  requests fail the timestamp check instead.
- Loopback auth protects only the local broker. Prism auth is separate
  ([ADR 0011](0011-prism-delegated-authentication.md)).

## References

- [Architecture overview](../overview.md)
- [`apps/runtime/cmd/ahv-remote-console-runtime/main.go`](../../../apps/runtime/cmd/ahv-remote-console-runtime/main.go)
- [`apps/runtime/internal/ipc/server.go`](../../../apps/runtime/internal/ipc/server.go)
- [`apps/desktop/src/main/main.ts`](../../../apps/desktop/src/main/main.ts)
- [`apps/desktop/src/main/signer.ts`](../../../apps/desktop/src/main/signer.ts)
