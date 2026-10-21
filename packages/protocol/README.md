# Protocol types

`@ahv-remote-console/protocol` defines the TypeScript types and constants for every message between the desktop app and the runtime.

## Overview

The package holds types and constants only. It has no runtime dependencies, so
the sandboxed renderer can import it. The renderer, the desktop app's main
process and the SDK all use these shapes. The Go runtime serves and reads the
same JSON. The wire version is `PROTOCOL_VERSION = 1`. See
[`docs/architecture/overview.md`](../../docs/architecture/overview.md) for how the
desktop app and runtime connect.

All definitions are in [`src/index.ts`](src/index.ts):

| Family | Types |
| --- | --- |
| Runtime handshake | `RuntimeHandshake`, including `secret_hash` (base64url of the SHA-256 of the IPC secret). |
| Endpoints and auth | `Endpoint`, `UpsertEndpointRequest`, `AuthKind` (`'basic' \| 'cookie'`), `SessionCookie`. `Endpoint` has `has_pinned_cert`, `pinned_cert_b64`, `allow_insecure_tls`. |
| Resources | `Resource`, `ListResourcesResponse`. |
| Identity | `UserIdentity`, `WhoAmIResponse`. |
| Sessions | `SessionInfo`, `OpenSessionRequest`, `OpenSessionResponse`. |
| Endpoint health | `EndpointHealth`, `EndpointHealthStatus`, `EndpointCapabilities`, `CapabilityState`. |
| Control frames | `ControlFrame` (runtime to client, for example `connected`, `heartbeat`, `reconnecting`, `resumed`, `clipboard_ready`) and `ClientControlFrame` (`ping`, `clipboard`, `paste_as_typing`, `resize`). Sent as JSON in WebSocket text frames. |
| Presence | `PresencePeer`, `PresenceMessage` and payloads for cursor, role and chat. |
| Errors and state | `APIError`, `NetworkSnapshot`, `RuntimeState`. |
| IPC signing | `IPC_HEADER_SIG`, `IPC_HEADER_TS`, `IPC_HEADER_NONCE`, and `canonicalRequest()`. |

The desktop app's main process checks `secret_hash` at startup against its own
hash. The renderer never sees the IPC secret, so it does not do this check. The
doc comment on `RuntimeHandshake` that says the renderer verifies it is out of
date.

> [!IMPORTANT]
> `canonicalRequest()` builds `METHOD\nURI\nTIMESTAMP\nNONCE\nBODY_SHA256_HEX`.
> This field order must match the runtime.

The consumers are [`@ahv-remote-console/sdk`](../sdk/README.md) and
[`apps/desktop`](../../apps/desktop/README.md), in both main and renderer.

## Build and test

Plain TypeScript, no bundler. From the repository root:

```bash
pnpm --filter @ahv-remote-console/protocol build       # tsc to dist/
pnpm --filter @ahv-remote-console/protocol typecheck
pnpm --filter @ahv-remote-console/protocol lint
```

Build this package before you run type-aware ESLint elsewhere. Other packages read `dist/index.d.ts`; without it they report many `no-unsafe-*` warnings. The package exports `dist/index.js` and `dist/index.d.ts`.

## Structure

| Path | Contents |
| --- | --- |
| `src/index.ts` | All types, constants and `canonicalRequest()`. |
| `dist/` | Build output. |
| `tsconfig.json` | Compiler settings. |

## Related documentation

- [Protocol reference](../../docs/reference/protocol.md)
- [IPC reference](../../docs/reference/ipc.md)
- [Runtime and proxy](../../docs/architecture/runtime-proxy.md)
- [Architecture overview](../../docs/architecture/overview.md)
- [Documentation index](../../docs/README.md)
