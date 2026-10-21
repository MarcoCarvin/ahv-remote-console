# Architecture overview

This page is the starting point for developers new to AHV Remote Console. It names
the components, walks through one connection from click to pixels, and shows where
the trust boundaries are. Each component has its own deeper page, linked below.

## Contents

- [What the product does](#what-the-product-does)
- [Components](#components)
- [How a connection works](#how-a-connection-works)
- [Trust boundaries](#trust-boundaries)
- [Clipboard path and live migration](#clipboard-path-and-live-migration)
- [Known limitations](#known-limitations)

## What the product does

AHV Remote Console is a desktop client for Nutanix AHV VM consoles. Its main feature
is host-to-guest clipboard sync, which Nutanix Guest Tools does not provide.

The clipboard rides the VNC stream and a qemu-vdagent serial channel. Both sit below
the guest's network stack, so clipboard sync keeps working under a full-tunnel VPN.

When you launch the app, two processes start. One is the Electron window you see.
The other is a small local Go service that the window starts for you. Only that
service talks to your Prism cluster. The window never holds your credentials.

The app has no identity system of its own. You sign in through Prism's own SSO page
in an embedded window. Clipboard sync turns on once the agent is installed in the
guest. The agent ships as a CD-ROM ISO that you mount once per template.

## Components

| Component | Path | Language | Role |
| --- | --- | --- | --- |
| Desktop app | `apps/desktop` | Electron, React, TypeScript | UI and owner of the trust boundary |
| Runtime | `apps/runtime` | Go | Loopback HTTP and WebSocket broker, RFB/VNC proxy |
| AHV plugin | `plugins/ahv` | Go, linked into the runtime | Prism v3 REST, VNC, SSO-cookie auth, agent ISO mount |
| Agent | `apps/ahv-remote-console-agent` | Go | In-guest clipboard over serial, display sizing, keep-warm |

### Desktop app

The desktop app has three isolated parts:

- **Main** (`src/main`) runs Node. It starts and supervises the runtime, owns the IPC
  secret and enforces the trust boundary.
- **Renderer** (`src/renderer`) is the React UI. It runs fully sandboxed and never
  sees Node, `ipcRenderer` or the secret.
- **Preload** (`src/preload/preload.ts`) is the renderer's only path to main. It
  exposes a narrow `contextBridge` API.

See [Desktop app](desktop.md).

### Runtime

The runtime is a single-user HTTP and WebSocket server on `127.0.0.1`, on a port the
OS assigns. It is never reachable from outside loopback. Every request except the
health, readiness, metrics and version probes is HMAC-SHA256 authenticated.

The runtime hosts the plugins and runs the RFB/VNC proxy. The AHV plugin is
statically linked in, not loaded as a shared object. See
[Runtime and VNC proxy](runtime-proxy.md).

### AHV plugin

The AHV plugin speaks Prism Central v3 REST and dials the hypervisor VNC WebSocket.
It authenticates only with captured Prism SSO cookies. It also mounts the agent ISO
into guests. See [AHV plugin](ahv-plugin.md).

### Agent

The agent is one binary with several modes. With no arguments it is the WebView2 GUI
installer on Windows and the daemon elsewhere. With `--daemon` it runs the resident
clipboard bridge and display sizing.

The daemon bridges the guest clipboard over a SPICE qemu-vdagent ISA serial port.
That port is COM3 on Windows and `/dev/ttyS2` on Linux. The daemon also pins the
guest display resolution. See [Agent](agent.md).

### Shared code and build

The desktop app uses pnpm TypeScript packages under `packages/`:

- `@ahv-remote-console/protocol` holds the wire types. It has no dependencies, so
  the sandboxed renderer can import it.
- `@ahv-remote-console/sdk` is the typed runtime client.
- `ui`, `telemetry` and `native-keyhook` complete the set.

All Go code lives in a single module ([`go.mod`](../../go.mod)). There is no
per-app `go.mod`. The pure-Go `cmd/isobuilder` assembles the agent ISO. The root
[`Makefile`](../../Makefile) drives the whole build: Go, JavaScript, agent binaries,
ISO, packaging and signing.

## How a connection works

```text
+------------------------------------------------+
| Desktop app (Electron)                         |
|                                                |
|  Renderer (sandbox)       Main (Node)          |
|  React UI, SDK client,    signer (holds        |
|  RFB client               secret), allowlist,  |
|        |                  RuntimeManager       |
|        | preload: signRequest / signWebSocket  |
|        | (only HMAC headers cross)     |       |
+--------|-------------------------------|-------+
         | signed HTTP + WS              | stdin: secret
         | (127.0.0.1)                   | runtime.json: hash
         v                               v
+------------------------------------------------+
| Runtime (Go, loopback only)                    |
|  HMAC auth -> router -> proxy                  |
|  AHV plugin: SSO cookies, TLS pinning, ISO     |
+------------------------+-----------------------+
                         | TLS 1.3, pinned SPKI,
                         | session cookies replayed
                         v
+------------------------------------------------+
| Nutanix Prism Central: REST v3 + hypervisor VNC|
+------------------------+-----------------------+
                         | VNC stream; ClientCutText
                         | goes to the vdagent chardev
                         v
+------------------------------------------------+
| Guest VM: qemu-vdagent on COM3 / ttyS2 <-> agent|
+------------------------------------------------+
```

1. Main generates a 48-byte per-launch IPC secret. It
   starts the runtime and writes the secret as a hex line to the runtime's stdin.
2. The runtime binds loopback and writes a `runtime.json`
   handshake file. The file holds its endpoint and `base64url(sha256(secret))`.
3. Main checks that the handshake's `secret_hash` matches its own hash.
   On a match it trusts the runtime and zeroes its raw copy of the secret. On a
   mismatch it refuses the runtime.
4. The renderer asks main to open Prism's `/console/` login in a
   sandboxed SSO window on an isolated partition. Main captures the session cookies.
   The plugin stores them in the OS keychain and replays them on REST calls and on
   the VNC upgrade.
5. The renderer reaches the runtime only through the SDK.
   `openSession` sends `POST /v1/sessions`, and the router calls the plugin's
   `OpenSession`.
6. `openStream` upgrades a WebSocket at
   `/v1/sessions/{id}/stream`. The HMAC signature travels in `Sec-WebSocket-Protocol`
   as `hc.v1.<sig>.<ts>.<nonce>`, next to `hc.console.v1`. The router hands the
   socket to `proxy.HandleStream`.
7. The proxy dials the hypervisor VNC WebSocket directly over
   TLS 1.3 with the plugin's pinned configuration. There is no Prism ticket
   exchange. The replayed `Cookie` header is the authentication.
8. Frames flow back through the proxy to the renderer's RFB client,
   which decodes them onto a canvas.

### Reconnects and control frames

The proxy runs one loop: dial, RFB 3.8 handshake, bidirectional pumps, wait. On an
upstream failure it tears down and dials again.

Every reconnect runs the RFB handshake again. A reconnect is a new VNC connection,
and there is no session resumption. The proxy sends `rfb_reset` and `resumed` control
frames so the renderer rebuilds its RFB client. A reconnect path that skips the
handshake desyncs the stream after a guest reboot, so do not add one.

Control frames are JSON in WebSocket text frames. A `fatal` frame carries a `cause`
such as `session_expired`, `rbac_denied`, `vm_not_found`, `session_closed`,
`config_error`, `unreachable` or `frame_too_large`. The renderer uses the cause to
offer the right recovery. [Runtime and VNC proxy](runtime-proxy.md) lists every
frame and cause.

A session with no attached stream is reaped after an idle time of 10 minutes. The
proxy loop never closes the plugin session itself.

### Network tuning

The console path is tuned for low latency:

- `permessage-deflate` is off, and WebSocket buffers are 1 MiB.
- Both TCP connections use `TCP_NODELAY` and DSCP Expedited Forwarding (TOS `0xB8`).
- `TCP_QUICKACK` is set on Linux only.
- Raw framebuffer updates pass through unchanged. The upstream read cap is 160 MiB,
  enough for a full 8K raw frame plus headroom.

AHV's QEMU limits console fidelity: about 30 fps adaptive refresh, no Tight JPEG,
and a fixed display mode. The app's main bandwidth lever on a slow link is a 16bpp
RGB565 pixel format, set in
[`apps/desktop/src/renderer/rfb/client.ts`](../../apps/desktop/src/renderer/rfb/client.ts).

## Trust boundaries

### Renderer and main

This is the boundary that matters most. The design treats the renderer as possibly
hostile, for example through XSS in plugin-rendered HTML or a compromised dependency.
A compromised renderer can never obtain the IPC secret or aim the runtime at
arbitrary endpoints.

- The secret never leaves main. The renderer calls `signRequest` or
  `signWebSocket` across the contextBridge. Only the resulting HMAC headers, or the
  WebSocket subprotocol, come back.
- Main checks every URI before signing. `assertSignableURI` rejects absolute,
  scheme-bearing and protocol-relative URIs, and path traversal. Both the HTTP method
  and the path must match an allowlist entry. Binding the method stops a path the
  SDK only reads from being deleted through the signer.
- The allowlist is least privilege. It mirrors exactly what the SDK calls. The
  diagnostics bundle and the bridge-envelope mint are not on it, because main signs
  those itself.
- The renderer is sandboxed. It runs with `contextIsolation`, `sandbox`,
  `webSecurity` on and `nodeIntegration` off.
- Electron Fuses are set at pack time. `RunAsNode`,
  `EnableNodeCliInspectArguments` and `EnableNodeOptionsEnvironmentVariable` are
  disabled. `OnlyLoadAppFromAsar` and `EnableCookieEncryption` are enabled. A Node
  relaunch therefore cannot bypass the preload.

### Main and runtime

The stdin secret and the handshake-hash check form this boundary. Main detects and
refuses a stale runtime or one with the wrong secret.

The runtime also enforces the boundary on its own:

- It verifies the HMAC over `METHOD\nURI\nTIMESTAMP\nNONCE\nBODY_SHA256_HEX`.
- The timestamp must be within 60 seconds of now.
- A sliding nonce window blocks replays.
- It serves loopback only, rejects non-loopback peers, and checks the `Host` header
  to block DNS rebinding.
- CORS allows only the renderer's real origins (`file://` and the Vite dev origin).
  The opaque `null` origin is not accepted.

### Runtime and Prism

- Authentication is delegated to Prism. The app has no SAML, OIDC, MFA or RBAC
  of its own. The two declared auth kinds are `basic` (username and password checked
  by Prism) and `cookie` (embedded SSO).
- TLS fails closed with a pinned SPKI. The plugin pins the leaf certificate's
  SubjectPublicKeyInfo and checks the chain in `VerifyConnection`. An endpoint with
  `allow_insecure_tls` and no valid pin is disabled: every handshake is refused.
- Credentials live only in the OS keychain, never on disk.

### Clipboard data

- The audit API accepts only byte length, direction and an optional SHA-256. It
  never accepts the clipboard payload.
- The agent copies and wipes cleartext clipboard buffers after use.

See the [threat model](../security/threat-model.md) for the full analysis.

## Clipboard path and live migration

The clipboard does not go through Prism or Nutanix Guest Tools. The renderer's RFB
`ClientCutText` passes through the proxy unchanged. QEMU routes it to the
qemu-vdagent chardev on the guest's ISA serial port. The agent bridges it to the
guest OS clipboard. See [Clipboard](clipboard.md).

QEMU backs off its display refresh when the screen is idle. While a console is
attached, the agent keeps the display slightly dirty under a keep-warm lease. This
works on Windows guests only. On other guests the display operation is a no-op.

A qemu-vdagent chardev blocks QEMU live migration while it exists. AHV 12.0 fixes
this natively. For earlier versions, a libvirt hook
([`scripts/ahv-libvirt-hook-qemu.py`](../../scripts/ahv-libvirt-hook-qemu.py))
makes COM3 follow the console:

- COM3 is a plain unix socket while no console is attached.
- It becomes qemu-vdagent when QEMU reports `VNC_CONNECTED`, and reverts on
  `VNC_DISCONNECTED`.
- A VM becomes migratable about 3 seconds after its console closes.

> [!IMPORTANT]
> The hook runs in dry-run mode by default. It logs its decisions but changes
> nothing until you arm it with the `AHVRC_MGR_ARMED` environment variable or the
> `/etc/libvirt/hooks/ahvrc-mgr-armed` flag file.

See [Live migration](live-migration.md).

## Known limitations

- macOS builds are development or ad-hoc signed only. No Developer ID certificate is
  configured for release signing or notarization. To ship signed macOS builds you
  need your own Developer ID and notary credentials. The build refuses to produce a
  release macOS artifact without a real identity.
- The TypeScript `ControlFrame` union in `packages/protocol` mirrors only part of the
  Go `ctrlFrame`. It omits `rfb_reset`, `vm_powered_off`, `vm_starting` and the
  `fatal` cause. Treat the Go side as the contract.
- Keep-warm has no effect on non-Windows guests.

## Related documentation

- [Desktop app](desktop.md)
- [Runtime and VNC proxy](runtime-proxy.md)
- [AHV plugin](ahv-plugin.md)
- [Agent](agent.md)
- [Clipboard](clipboard.md)
- [Live migration](live-migration.md)
- [Update integrity](update-integrity.md)
- [Audit and logging](audit-and-logging.md)
- [Architecture decision records](adr/README.md)
- [IPC reference](../reference/ipc.md)
- [Protocol reference](../reference/protocol.md)
- [Threat model](../security/threat-model.md)
- [Glossary](../reference/glossary.md)
