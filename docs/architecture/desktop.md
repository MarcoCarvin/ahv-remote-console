# Desktop app

The desktop app is the Electron application you launch. It lists VMs, signs in to
Prism and renders VM consoles with clipboard sync. This page describes its
processes, windows, the IPC secret handoff, the signing allowlist and its Electron
security settings. It is for developers working on `apps/desktop`.

## Contents

- [Responsibilities](#responsibilities)
- [Three trust zones](#three-trust-zones)
- [Startup and the main window](#startup-and-the-main-window)
- [IPC secret handoff](#ipc-secret-handoff)
- [Signing allowlist and trusted senders](#signing-allowlist-and-trusted-senders)
- [Console pop-out windows](#console-pop-out-windows)
- [Keyboard capture, menus and clipboard](#keyboard-capture-menus-and-clipboard)
- [SSO window](#sso-window)
- [Electron security settings](#electron-security-settings)
- [Packaging and signing](#packaging-and-signing)
- [Configuration](#configuration)
- [IPC surface](#ipc-surface)
- [Known limitations](#known-limitations)

## Responsibilities

- Start and supervise the Go runtime, and hold the IPC secret.
- Sign runtime requests for the renderer without exposing the secret.
- Create the main window, console pop-outs and the SSO window.
- Bridge the OS clipboard to the renderer.
- Build the tray and the application menu.

The app holds no credentials of its own. Even a compromised UI cannot reach Prism or
your infrastructure by itself.

## Three trust zones

Electron runs code in three places with different privileges. The security model
depends on keeping them apart.

| Zone | Source | Privilege |
| --- | --- | --- |
| Main process | [`src/main/main.ts`](../../apps/desktop/src/main/main.ts) | Full Node.js, filesystem, the secret |
| Preload | [`src/preload/preload.ts`](../../apps/desktop/src/preload/preload.ts) | Isolated world, exposes one frozen object |
| Renderer | `src/renderer` | Sandboxed React, no Node |

The main process starts the runtime, owns the HMAC secret, creates windows, signs
requests, brokers the clipboard and builds the menu and tray.

The preload is the renderer's entire surface to main. It exposes one frozen object,
`window.ahvRemoteConsole`, through `contextBridge.exposeInMainWorld`. The renderer
never sees `ipcRenderer`, `fs` or any Node module.

The renderer talks to the runtime only through the SDK (`@ahv-remote-console/sdk`).
The SDK asks main to sign each call. The renderer can get signatures for the URIs
main allows, but never holds the secret.

The main window and every console pop-out share one set of hardened
`webPreferences`:

| Setting | Value |
| --- | --- |
| `contextIsolation` | `true` |
| `sandbox` | `true` |
| `nodeIntegration` | `false` |
| `nodeIntegrationInWorker` | `false` |
| `nodeIntegrationInSubFrames` | `false` |
| `webSecurity` | `true` |
| `allowRunningInsecureContent` | `false` |
| `webviewTag` | `false` |
| `spellcheck` | `false` |
| `backgroundThrottling` | `false` |

A fourth window, the SSO window, is untrusted by design. See [SSO window](#sso-window).

## Startup and the main window

The app runs as a single instance. It takes a single-instance lock when the module
loads and quits if another copy is running. The running copy gets focus through the
`second-instance` event.

Before `app.whenReady()`, the app sets three Chromium switches. They turn off
renderer backgrounding, timer throttling and occluded-window backgrounding. The
renderer decodes frames, polls the clipboard and forwards input all the time, and
Chromium throttling makes the console feel slow.

The app also registers the `ahv-remote-console://` URL scheme for deep-link callbacks
before ready, because Electron rejects late registration.

Window creation does not wait for the runtime. The runtime starts in the background.
The window opens at once and shows a "connecting" overlay driven by the runtime
state. The handshake check runs in the `runtime:ready` handler, before the state
becomes `ready`.

The main window has these properties:

- Default size 1440x900, minimum 960x600.
- It is created with `show: false` and `backgroundColor: '#0f1b2a'`, so the first
  frames show the app background instead of a white flash.
- macOS uses `titleBarStyle: 'hiddenInset'`.
- It is shown on `did-finish-load`, not `ready-to-show`. On macOS a hidden window's
  first compositor frame has low priority, so `ready-to-show` can stall for seconds
  to minutes.
- With `AHV_REMOTE_CONSOLE_DEV=1` it loads `http://localhost:5173` (Vite). Otherwise
  it loads the bundled `renderer/index.html` from `file://`.

After the window exists, the app builds the tray and the application menu, registers
the IPC handlers and calls `initAutoUpdate`. It registers no global hotkeys.

Quit and activate follow platform conventions. `window-all-closed` quits on
platforms other than macOS. `activate` re-creates the main window if it was closed.
On `before-quit` the app stops the runtime, destroys the tray and zeroes the IPC
secret.

## IPC secret handoff

An HMAC secret that lives only in main protects the link between the desktop app and
the runtime. Main checks it once at startup.

1. On construction, main generates a 48-byte random secret with `randomBytes(48)`.
   It gives a copy to a `Signer`. It keeps the raw `ipcSecret` buffer only long
   enough to pass it to the runtime.
2. `RuntimeManager` starts the runtime with `--ipc-secret-stdin`, writes the
   hex-encoded secret to its stdin and closes stdin. The secret never appears in
   argv, where the process table would show it, or in an environment variable.
3. Before spawning, the manager deletes any stale `runtime.json`. The runtime binds
   a loopback port and writes a new `runtime.json` with `endpoint`, `pid`,
   `version`, `started_at` and `secret_hash`. The manager polls for that file.
4. On `runtime:ready`, main compares the file's `secret_hash` with
   `signer.secretHash()`. That value is the SHA-256 of the secret, base64url without
   padding, the same format the runtime writes.
5. On a mismatch main fails closed. It logs, moves to the `error` state and stops the
   runtime. On a match it zeroes the raw `ipcSecret` buffer, so the secret is no
   longer held in two places.
6. From then on, the renderer asks main to sign. `signRequest` builds the canonical
   string `METHOD\nURI\nTIMESTAMP\nNONCE\nSHA256(body)` and returns `X-HC-Sig`,
   `X-HC-Timestamp` and `X-HC-Nonce`. `signWebSocket` returns the
   `hc.v1.<sig>.<ts>.<nonce>` subprotocol.
7. On `before-quit` the signer is wiped. The buffer is zeroed and every signer method
   then throws.

The runtime binds a new port after a crash or a sleep and wake. The manager restarts
it with exponential backoff, up to 5 attempts, before it reports a terminal error.
An operator-triggered restart resets that budget. The main window and pop-outs
follow the runtime state and rebuild their SDK clients when the endpoint changes.

Source: [`signer.ts`](../../apps/desktop/src/main/signer.ts),
[`runtime-manager.ts`](../../apps/desktop/src/main/runtime-manager.ts).

## Signing allowlist and trusted senders

If main signed any URI, a compromised renderer could use it as a signing oracle for
any runtime endpoint. The allowlist in
[`signable.ts`](../../apps/desktop/src/main/signable.ts) prevents that.

Main calls `assertSignableURI(method, uri)` before every signature, for both
`signRequest` and `signWebSocket`. It checks three things in order:

1. The URI is root-relative (`/v1/...`). Absolute, scheme-bearing and
   protocol-relative (`//host`) URIs are rejected, so the signer cannot be aimed at
   another origin.
2. The path, without its query, does not contain `..`.
3. One allowlist entry matches both the HTTP method, case-insensitive, and the path
   pattern. Binding the method stops a path the SDK only reads from being deleted
   through the signer.

The `SIGNABLE` table is the least-privilege mirror of the SDK's calls in
[`packages/sdk/src/index.ts`](../../packages/sdk/src/index.ts). Keep the two in
step. WebSocket upgrades are GET requests, so the stream and presence paths are
listed under GET. The diagnostics bundle and the bridge-envelope mint are not on the
list, because main signs those itself. The module has no DOM or Electron imports, so
it can be unit-tested alone.

The signing handlers also refuse to sign until the runtime is ready.

Every privileged IPC handler first calls `assertTrustedSender(e)`. It finds the
window that owns `event.sender` and requires it to be in the trusted set. Only the
main window and console pop-outs are registered there. The SSO window never is. A
request that passes the allowlist is still rejected if it does not come from a
trusted window. Broadcasts also skip any window not on the default session.

Source: [`trustedSenders.ts`](../../apps/desktop/src/main/trustedSenders.ts).

## Console pop-out windows

You can pop a VM console out into its own window. The renderer calls
`window.ahvRemoteConsole.openConsoleWindow(sessionId, vmName)`, and main runs
`createConsoleWindow`.

- The pop-out uses the same hardened `webPreferences` and the same preload as the
  main window.
- Pop-outs are kept in a map keyed by session ID. Opening the same session again
  focuses the existing window.
- It loads the same bundle with `?mode=console&session=<id>&vm=<name>`. The renderer
  entry reads `mode` and mounts `ConsoleWindowApp` instead of `App`.
- Closing a pop-out closes only that VM's session. The window removes itself from
  the map on `closed`.
- The menu bar is hidden (`autoHideMenuBar: true`) because the pop-out has its own
  banner.
- Native fullscreen changes (green button, F11, `setFullScreen`) are sent to the
  renderer on `window-fullscreen`. The DOM `fullscreenchange` event does not fire
  for them.
- Renderer warnings and errors are copied into the main-process log, so an RFB
  desync leaves a trace on disk without DevTools.
- It fades in with `setOpacity` on `did-finish-load`, for the same macOS reason as
  the main window.

On sign-out, `closeConsoleWindows` closes every pop-out, so no console stays open on
credentials that logout has wiped.

### Pop-out renderer

[`ConsoleWindowApp`](../../apps/desktop/src/renderer/app/ConsoleWindowApp.tsx)
starts with only `sessionId` and `vmName` from the URL. It then works as follows:

- It builds its own SDK client against the runtime endpoint. Its signer forwards
  `signRequest` and `signWebSocket` to the contextBridge, so the secret never
  crosses.
- It opens its own stream and renders one `ConsoleView`, with its own pointer lock,
  keyboard capture and RFB client.
- It follows the runtime state, so a pop-out opened during a runtime restart shows
  progress. It rebuilds the client when the runtime endpoint changes, because each
  restart binds a new port.
- It reopens the stream after a real drop with a bounded backoff: 1 s, 2 s, 4 s,
  then 8 s five times.
- It fails fast when a socket never connected, for example for a deleted VM, a wrong
  endpoint or an unreachable cluster. It shows the error overlay instead of about
  40 seconds of reconnect flicker.
- It does not retry the terminal close codes in the table below.
- It does not close the old socket on a swap. After an upstream drop the proxy keeps
  the loopback socket open and handshakes again in place. Closing it from the
  renderer would race that recovery and freeze the console.
- If the runtime lost the session after a restart or idle reap, it opens a new one
  with `reopenSessionStream`. It uses the VM identity it saved from `listSessions`.
- The error overlay offers `reconnectNow` for `unreachable` and `reauthNow` for
  `session_expired`. `reauthNow` runs the SSO window again and re-authenticates in
  place. If the window has focus it opens SSO at once. Otherwise it waits for focus.
- On unmount or `beforeunload` it closes the runtime session on a best-effort basis.
  The runtime also reaps idle sessions.

| Close code | Name | Causes | Retried |
| --- | --- | --- | --- |
| 1008 | ClosePolicyViolation | `session_expired`, `rbac_denied`, `vm_not_found`, `config_error`, `session_closed` | No |
| 1009 | CloseMessageTooBig | `frame_too_large` | No |

## Keyboard capture, menus and clipboard

### Keyboard capture

A console must receive keys that would otherwise trigger menu accelerators such as
Cmd+Q and Cmd+W. The renderer calls `setCaptureMode(captured)`.

- Main tracks captured webContents IDs in `captureWindows`.
- It calls `setIgnoreMenuShortcuts(isCaptured)` on the sender. Unlike
  `before-input-event` with `preventDefault`, this does not block the renderer's own
  key events. The pop-out installs no `before-input-event` handler for this reason.
- Main removes the application menu (`Menu.setApplicationMenu(null)`) only when the
  focused window is captured. Otherwise it restores the default menu. One renderer
  therefore cannot suppress accelerators for every window. When the last captured
  window closes, the default menu returns.

macOS press-and-hold, the accent popup, is turned off for the app. A held key then
auto-repeats into the guest. `toggleFullscreen` toggles fullscreen.

### Menu

The application menu, in [`menu.ts`](../../apps/desktop/src/main/menu.ts), has
**Preferences**, **Export Diagnostics**, **Open Log File**, an **Open at Login**
checkbox, the standard Edit, View and Window roles, and a Help item. DevTools are
available only with `AHV_REMOTE_CONSOLE_DEV=1` or `AHV_REMOTE_CONSOLE_DEBUG=1`.
Production builds do not expose them.

### Clipboard bridge

Main polls the OS clipboard every 150 ms and sends changes to all trusted windows on
`clipboard-changed`. Polling in main is not subject to Chromium throttling, so sync
stays immediate when the window is in the background.

`clipboard-read` reads on demand. `clipboard-write` writes back, with one retry for
Windows clipboard contention. Clipboard broadcasts go only to trusted windows, never
to the SSO window.

## SSO window

The app has no identity of its own. To sign in, main opens a separate, untrusted
`BrowserWindow` that loads `<prism>/console/`. You complete whatever identity
provider flow Prism uses. When a Prism session cookie appears in the window's cookie
jar, the window returns the cookies and closes. Main passes them to the AHV plugin,
which stores them in the OS keychain.

The window is isolated:

- It has no preload, no Node and no contextBridge. It renders Prism only and never
  sees the IPC secret.
- It uses its own session partition per endpoint, `persist:sso-<endpointId>`.
  Cookies, cache and storage do not leak between endpoints or into the renderer's
  default partition.
- It is never added to the trusted set. It cannot call privileged IPC and receives
  no clipboard, runtime-state or update broadcasts.
- It takes its TLS settings from the stored endpoint, not from the renderer. The
  `open-sso-login` handler looks up the endpoint through a signed loopback call. It
  uses the stored host, `allow_insecure_tls` and pinned certificate. The renderer's
  `prismURL` host must match, and the URL must be `https://`.
- When a pin exists, the window accepts only a certificate whose SPKI hash matches.
  This is the same anchor the runtime uses, so a rotation that keeps the key still
  works.
- When the endpoint is marked insecure and has no pin yet, the window captures the
  cluster certificate once (trust on first use). It returns it with the cookies.
  The renderer shows the fingerprint, stores the pin after you confirm it, and
  only then sends the cookies to the runtime.
- Identity provider hosts other than the cluster use Chromium's default trust, so
  the usual SSO redirect works.
- It denies every `window.open` from Prism or the identity provider.
- It denies all permission requests.

The SSO window must follow redirects between Prism and the identity provider. For
that reason the app installs no global navigation guard.

Source: [`sso-login.ts`](../../apps/desktop/src/main/sso-login.ts).

## Electron security settings

### Content Security Policy and headers

Main injects a strict CSP on every response. The production policy is:

| Directive | Value |
| --- | --- |
| `default-src` | `'self'` |
| `script-src` | `'self'` (no inline, no eval) |
| `connect-src` | `'self' http://127.0.0.1:* ws://127.0.0.1:*` |
| `worker-src` | `'self' blob:` (needed by `DecompressionStream` in the Tight decoder) |
| `object-src`, `base-uri`, `frame-src`, `frame-ancestors`, `form-action` | `'none'` |

Development relaxes the policy only for Vite hot reload: `unsafe-inline`, `eval` and
`localhost:5173`.

Main also sets these headers:

- `X-Frame-Options: DENY`
- `X-Content-Type-Options: nosniff`
- `Referrer-Policy: no-referrer`
- `Permissions-Policy: camera=(), microphone=(), geolocation=()`

### Permissions and navigation

`setPermissionRequestHandler` and `setPermissionCheckHandler` deny every permission.

Trusted windows have navigation guards. `setWindowOpenHandler` denies every
`window.open` and sends the URL to the system browser. `will-navigate`,
`will-redirect` and `will-frame-navigate` allow only `localhost:5173` and `file://`
and cancel everything else. `openExternalSafely` opens only `https:` and `mailto:`
links.

### Electron Fuses

[`scripts/after-pack.js`](../../apps/desktop/scripts/after-pack.js) sets Electron
Fuses on the packaged binary on all platforms. It runs in the `afterPack` hook, so
the app signature covers the result.

| Fuse | Value |
| --- | --- |
| `RunAsNode` | `false` |
| `EnableNodeCliInspectArguments` | `false` |
| `EnableNodeOptionsEnvironmentVariable` | `false` |
| `OnlyLoadAppFromAsar` | `true` |
| `EnableCookieEncryption` | `true` |

These block a relaunch through `ELECTRON_RUN_AS_NODE`, `--inspect` or
`NODE_OPTIONS`. Such a relaunch would run any Node code under the app's signature and
entitlements, and would bypass the preload and the signing allowlist.

On macOS the hook applies an ad-hoc signature after setting the fuses, so an
unsigned development build still launches on Apple Silicon. A signed release is
signed again after this hook.

## Packaging and signing

electron-builder packages the app, configured in
[`electron-builder.yml`](../../apps/desktop/electron-builder.yml).

- The runtime ships in `extraResources` under `runtime/<os>-<arch>/` and is listed in
  `asarUnpack` so it can run.
- The agent ISO, third-party notices, runtime icons and an optional private CA
  bundle also ship in `extraResources`.
- On macOS, `afterPack` also removes the quarantine attribute and checks the runtime
  binary's permissions.
- The `afterSign` hook ([`notarize.js`](../../apps/desktop/scripts/notarize.js))
  notarizes on macOS only when notary credentials are present.
- Windows builds are signed with `signAndEditExecutable: true`.

## Configuration

| Variable | Effect |
| --- | --- |
| `AHV_REMOTE_CONSOLE_DEV=1` | Load the Vite dev server, relax the CSP for hot reload, enable DevTools |
| `AHV_REMOTE_CONSOLE_DEBUG=1` | Enable DevTools |
| `AHV_REMOTE_CONSOLE_AUTOUPDATE=1` | Turn on the updater in a packaged build |
| `CSC_LINK`, `CSC_NAME` | Developer ID certificate for macOS release signing |
| `APPLE_API_KEY`, `APPLE_API_KEY_ID`, `APPLE_API_ISSUER` | Notarization credentials |

The updater stays off unless `AHV_REMOTE_CONSOLE_AUTOUPDATE=1` is set and the build
is packaged.

## IPC surface

The full renderer API is the `ExposedAPI` object in
[`preload.ts`](../../apps/desktop/src/preload/preload.ts). Each call maps to an
`ipcMain.handle` registration in `main.ts`. See the [IPC reference](../reference/ipc.md)
for details.

| Renderer call | IPC channel |
| --- | --- |
| `getRuntimeState`, `onRuntimeState` | `get-runtime-state`, `runtime-state` |
| `restartRuntime` | `restart-runtime` |
| `signRequest` | `sign-request` |
| `signWebSocket` | `sign-websocket` |
| `setCaptureMode` | `set-capture-mode` |
| `openConsoleWindow` | `open-console-window` |
| `closeConsoleWindows` | `close-console-windows` |
| `openSSOLogin` | `open-sso-login` |
| `clipboardRead`, `clipboardWrite`, `onClipboardChanged` | `clipboard-read`, `clipboard-write`, `clipboard-changed` |
| `toggleFullscreen`, `onWindowFullscreen` | `toggle-fullscreen`, `window-fullscreen` |
| `saveDiagnostics` | `save-diagnostics` |
| `hcLog` | `hc-log` |
| `onOIDCCallback`, `onNavigate`, `onUpdateAvailable`, `onUpdateDownloaded` | `oidc-callback`, `navigate`, `update-available`, `update-downloaded` |

Every privileged handler checks for a trusted sender first. The two signing handlers
also apply the allowlist and the runtime-ready check.

## Known limitations

- macOS release signing is not configured. The `mac` block in
  `electron-builder.yml` sets `hardenedRuntime: true` and entitlements but no signing
  `identity`.
- `make package-mac` and `make prod-mac` refuse to build unless the keychain holds a
  "Developer ID Application" certificate or CI supplies `CSC_LINK` or `CSC_NAME`.
  Without one you get unsigned or ad-hoc development builds from
  `make package-mac-dev`.
- Without the `APPLE_API_*` variables, notarization is skipped. The build prints a
  "signed but NOT notarized" notice instead of failing.
- To ship a macOS build you need your own Developer ID certificate and notary
  credentials in the release CI environment.
- Auto-update install needs a signed app on macOS and Windows. The updater stays
  dormant until code signing and a monotonic release version are in place.
- `EnableEmbeddedAsarIntegrityValidation` is not enabled. It interacts with the
  order of notarization steps and is to be turned on separately.

## Related documentation

- [Architecture overview](overview.md)
- [Runtime and VNC proxy](runtime-proxy.md)
- [Update integrity](update-integrity.md)
- [IPC reference](../reference/ipc.md)
- [Console pop-out design](../design/console-popout.md)
- [SSO window design](../design/sso-login-window.md)
- [Capture and input design](../design/capture-and-input.md)
- [Release process](../development/release.md)
- [Threat model](../security/threat-model.md)
