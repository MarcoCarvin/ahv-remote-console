# ADR 0004: The sandboxed renderer never holds the IPC secret

## Status

Accepted

## Context

The renderer runs the React UI. The architecture treats it as potentially
hostile, for example through XSS in plugin-rendered HTML or a compromised
dependency.

If the renderer could read the IPC secret, for example through a getter exposed
on `contextBridge`, any code in its V8 isolate could call that getter. A
compromised renderer would then have full access to the runtime, and the runtime
tunnels customer infrastructure credentials.

The repository does not record other alternatives considered.

## Decision

The secret stays in the main process. The renderer asks main to sign through two
narrow calls on the sealed preload, `signRequest` and `signWebSocket`. Only the
resulting HMAC headers, or the WebSocket subprotocol, cross the bridge.

Before it signs, main checks each method and URI pair against an allowlist. The
allowlist binds the HTTP method as well as the path, so a path the SDK only reads
with GET cannot be signed for DELETE.

## Consequences

- A compromised renderer can request signatures for allowlisted routes only. It
  cannot read the secret or sign for arbitrary endpoints.
- Every runtime call passes through main, which gives one audit point. The
  diagnostics bundle and the bridge-envelope mint are not on the allowlist, so
  main signs those itself.
- The allowlist must stay in step with the SDK. A new SDK call that is missing
  from the list fails at signing time.
- The renderer cannot verify signatures itself. It trusts main's output.

## References

- [Architecture overview](../overview.md)
- [Desktop app README](../../../apps/desktop/README.md)
- [`apps/desktop/src/main/signer.ts`](../../../apps/desktop/src/main/signer.ts)
- [`apps/desktop/src/main/signable.ts`](../../../apps/desktop/src/main/signable.ts)
- [`apps/desktop/src/preload/preload.ts`](../../../apps/desktop/src/preload/preload.ts)
- [`apps/desktop/src/main/main.ts`](../../../apps/desktop/src/main/main.ts)
