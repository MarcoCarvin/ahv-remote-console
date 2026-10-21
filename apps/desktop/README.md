# Desktop app

The Electron desktop app that lists Nutanix AHV VMs, signs in to Prism, and shows VM consoles with clipboard sync.

## Overview

The package is `@ahv-remote-console/desktop`. It starts one local runtime from
[`apps/runtime`](../runtime/README.md) and supervises it. The runtime is the only
path to Prism. The desktop app holds no credentials of its own and never gives
the renderer a secret. See
[`docs/architecture/overview.md`](../../docs/architecture/overview.md) for how the
desktop app fits with the runtime, the AHV plugin and the agent.

The app runs in three processes with different trust levels:

- **main** (`src/main/`) is the Node process. It starts the runtime and sends it
  a per-launch IPC secret over stdin. It then checks the `secret_hash` in the
  runtime's handshake file and stops on a mismatch. It also creates hardened
  windows (sandbox, `contextIsolation`, no `nodeIntegration`), and runs the
  request signer, tray, menu, `ahv-remote-console://` deep links and auto-update.
- **preload** (`src/preload/preload.ts`) is the only `contextBridge` surface the
  renderer sees. The renderer has no `ipcRenderer`, `fs` or Node. Preload exposes
  `signRequest`, `signWebSocket`, `openConsoleWindow`, `setCaptureMode` and
  runtime-state subscriptions. Main computes the HMAC, so the secret never
  leaves main.
- **renderer** (`src/renderer/`) is the sandboxed React UI. It reaches the
  runtime only through the SDK over signed HTTP and WebSocket calls.

The signer in main allows only the `(method, path)` pairs the SDK calls. The list
is in [`src/main/signable.ts`](src/main/signable.ts). A compromised renderer
cannot get a signature for any other endpoint.

Console pop-outs load the same bundle in a new hardened window with
`?mode=console&session=<id>&vm=<name>`. For `mode=console`,
[`src/renderer/main.tsx`](src/renderer/main.tsx) mounts only `ConsoleWindowApp`.

## Build and test

Run these from `apps/desktop/`, or use `pnpm --filter @ahv-remote-console/desktop <script>` from the repository root.

| Command | What it does |
| --- | --- |
| `pnpm dev` | Builds main and preload, starts Vite for the renderer, then Electron. Only the renderer hot-reloads. Restart after main or preload changes. |
| `pnpm build` | Three Vite builds (renderer, main, preload) into `dist/`. |
| `pnpm package` | Build, then `electron-builder`. Also `package:win`, `package:linux`, `package:mac`. |
| `pnpm package:mac:dev` | Unsigned arm64 DMG for local macOS testing. |
| `pnpm test` | Unit tests (Vitest). |
| `pnpm e2e` | End-to-end tests (Playwright). |
| `pnpm lint` | ESLint. Build `packages/*` first, or type-aware rules report many `no-unsafe-*` warnings. |
| `pnpm typecheck` | `tsc --noEmit`. |

From the repository root, `make dev`, `make build`, `make package`, `make lint`, `make typecheck` and `make test` run the same steps for the whole repository.

The `afterPack` hook in [`scripts/after-pack.js`](scripts/after-pack.js) sets Electron Fuses on every platform. It turns off RunAsNode, `--inspect` and `NODE_OPTIONS`. It turns on `OnlyLoadAppFromAsar` and cookie encryption. It also runs macOS quarantine and codesign steps.

> [!NOTE]
> No Developer ID certificate is configured in this repository. Only ad-hoc or self-signed macOS builds work. Release signing and notarization need your own Developer ID.

## Structure

| Path | Contents |
| --- | --- |
| `src/main/` | Main process with the `main.ts` entry, `runtime-manager.ts` (spawn, secret, restart backoff), `signable.ts` and `signer.ts`, tray, menu, updater. |
| `src/preload/` | `preload.ts`, the renderer-to-main API. |
| `src/renderer/` | React UI. `rfb/` is the VNC client, `clipboard/` the clipboard engine and audit ledger, `input/` the keyboard mapping. |
| `scripts/` | `dev.mjs`, `after-pack.js`, `notarize.js`. |
| `e2e/` | Playwright tests. |
| `electron-builder.yml` | Packaging configuration. |

## Related documentation

- [Desktop app architecture](../../docs/architecture/desktop.md)
- [Architecture overview](../../docs/architecture/overview.md)
- [IPC reference](../../docs/reference/ipc.md)
- [Building](../../docs/development/build.md)
- [Testing](../../docs/development/testing.md)
- [Documentation index](../../docs/README.md)
