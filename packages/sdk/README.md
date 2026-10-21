# Runtime SDK

`@ahv-remote-console/sdk` is the typed TypeScript client the desktop app uses to call the runtime.

## Overview

The SDK is the only supported path from the desktop app to the runtime. Every
call goes over loopback HTTP or WebSocket and carries an HMAC signature. The
client runs in the desktop app's main process and in the sandboxed renderer.
See [`docs/architecture/overview.md`](../../docs/architecture/overview.md) for how
the desktop app and runtime fit together.

The package exports one class, `AHVRemoteConsoleClient`. It wraps the runtime API
and returns the types from [`@ahv-remote-console/protocol`](../protocol/README.md).

### Signing

The client never sees the IPC secret. You construct it with an `endpoint` and a
`Signer`. A `Signer` has two methods, `signRequest(method, uri, body)` and
`signWebSocket(uri)`. They ask the secret holder for the HMAC headers or the
WebSocket subprotocol token.

- In the renderer, the signer is `window.ahvRemoteConsole.signRequest` and
  `signWebSocket` from the preload bridge. Main checks each method and path
  against an allowlist before it signs, so the secret stays in main.
- In main, the signer calls the main-process signer directly.

### API surface

| Area | Methods |
| --- | --- |
| Runtime | `getRuntimeState`, `listPlugins`, `getFeatureFlags`, `listTasks`, `getTask` |
| Endpoints | `listEndpoints`, `upsertEndpoint`, `deleteEndpoint`, `getEndpointHealth` |
| Auth | `startAuth`, `completeAuth`, `refreshAuth`, `logout`, `whoAmI` |
| Resources and VMs | `listResources`, `refreshResources`, `vmPowerAction`, `addSerialPort`, `getVMStats`, snapshots |
| Sessions | `openSession`, `closeSession`, `listSessions` |
| Agent and ISOs | `mountAgentISO`, `listISOImages`, `mountISO`, `ejectISO`, `recordAgentAudit` |
| Streams | `openStream(sessionId)` for VNC (binary RFB plus JSON control frames), `openPresenceStream` |
| Binary | `fetchBinary` for signed downloads such as the diagnostics bundle. `diagnosticsBundleURL` returns the bare, unsigned URL. |

Every non-OK response throws `AHVRemoteConsoleError` with a `code`, a message
and the HTTP `status`. ISO calls document codes such as `iso_mount_unsupported`
(503), `iso_mount_failed` (502) and `image_publish_required`.

`recordAgentAudit` accepts only `bytes`, `direction` and an optional `sha256`.
It never takes clipboard text, so plaintext cannot reach the audit log through
the SDK.

## Build and test

The module is one file, `src/index.ts`, compiled with `tsc`. From the repository root:

```bash
pnpm --filter @ahv-remote-console/sdk build
pnpm --filter @ahv-remote-console/sdk typecheck
pnpm --filter @ahv-remote-console/sdk lint
pnpm -r --filter './packages/**' build    # build all packages
```

Build the packages before type-aware ESLint. Without the `dist` declarations it reports many `no-unsafe-*` warnings. Entry points are `dist/index.js` and `dist/index.d.ts`.

## Structure

| Path | Contents |
| --- | --- |
| `src/index.ts` | `AHVRemoteConsoleClient`, `Signer`, `AHVRemoteConsoleError`. |
| `dist/` | Build output. |

## Related documentation

- [IPC reference](../../docs/reference/ipc.md)
- [Protocol reference](../../docs/reference/protocol.md)
- [Runtime and proxy](../../docs/architecture/runtime-proxy.md)
- [Architecture overview](../../docs/architecture/overview.md)
