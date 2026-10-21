# Glossary

Terms used across the documentation, in alphabetical order.

**Agent**: the program that runs inside the guest VM (`apps/ahv-remote-console-agent`).
It handles the clipboard over the serial channel and sets the display size. See
[Agent command line](agent-cli.md).

**Agent ISO**: the ISO image that carries the agent installer. The runtime mounts it
into a guest so the operator can install the agent. Build it with `make iso`.

**AHV**: Nutanix's KVM-based hypervisor, managed through Prism.

**AHV plugin**: the Go component, linked into the runtime, that talks to Prism. It is
the only plugin in this repository. See
[Plugin SDK](../development/plugin-sdk.md).

**Audit chain**: the hash-linked audit log. Each record stores the hash of the
previous one, so changing an earlier record breaks the chain. See
[Audit and logging](../architecture/audit-and-logging.md).

**Clipboard echo suppression**: the mechanism that stops a copy from bouncing back to
the side it came from. See [Clipboard](../architecture/clipboard.md).

**Control frame**: a JSON text message on the console WebSocket, such as `heartbeat`
or `fatal`. Binary messages on the same socket carry RFB. See
[Wire protocol reference](protocol.md#control-frames).

**Desktop app**: the Electron application the operator runs. It starts the runtime,
holds the IPC secret in its main process, and draws the console in a sandboxed
renderer.

**Endpoint**: a configured Prism cluster the app can connect to. It is stored in the
runtime's configuration. See [Configuring endpoints](../operations/configure.md).

**Guest layout**: the keyboard layout you declare for a VM in the console's globe
menu: US, UK or Italian. The console types each character the way that layout produces
it. See [Keyboard mapping](keyboard.md).

**Handshake file**: the file `runtime.json` that the runtime writes at launch. It
holds the loopback endpoint and a hash of the IPC secret, which the desktop app
checks.

**IPC secret**: the per-launch secret the desktop app passes to the runtime over
stdin. Requests to the runtime are signed with it using HMAC-SHA256, and the renderer
never sees it. See [IPC and request signing](ipc.md).

**Keep-warm**: the agent routine that keeps the QEMU display slightly dirty while a
console is open, so the guest display keeps updating. See
[Agent architecture](../architecture/agent.md).

**Keyhook**: the native keyboard-grab add-on (`packages/native-keyhook`). It is a stub
on Linux and macOS in this release. Without it, the app uses the Send Keys menu.

**Live migration blocker**: a QEMU setting that stops a VM from being live-migrated
while the vdagent channel is attached. The deployed hook arms it only while needed.
See [Live migration](../architecture/live-migration.md).

**Main process**: the privileged Electron process of the desktop app. It starts the
runtime, holds the IPC secret and signs allowlisted requests for the renderer.

**Plugin**: a Go component that the runtime loads to talk to a platform. See
**AHV plugin**.

**Presence**: the runtime's record of other clients attached to the same session. A
peer times out after 15 seconds without a heartbeat, cursor positions are relayed at
30 Hz, and a backlog of 200 chat messages is kept (constants in
`apps/runtime/internal/presence`).

**Prism**: the Nutanix management interface. The app talks to Prism's REST API and
its VNC console endpoint, never to the hypervisor directly.

**Renderer**: the sandboxed part of the desktop app that draws windows and runs the
web UI. It has no Node APIs and reaches the runtime only through signed SDK calls.

**RFB**: the Remote Framebuffer protocol, the wire protocol behind VNC.

**Runtime**: the Go daemon (`apps/runtime`). It listens on loopback, serves the
desktop app's requests, and proxies the VNC stream.

**Send Keys menu**: the console toolbar menu that sends key combinations you cannot
type directly, such as Ctrl+Alt+Del. See [Keyboard mapping](keyboard.md#send-keys-menu).

**Session**: one open console. A session holds the upstream VNC connection and the
renderer's WebSocket stream for a single VM.

**Session cookies**: the Prism session cookies captured from the SSO window. The
runtime replays them on REST calls and on the VNC stream. They are stored in the OS
keychain or libsecret.

**Signing allowlist**: the fixed list of HTTP method and path pairs that the main
process will sign for the renderer. See [IPC and request signing](ipc.md#signing-allowlist).

**SSO window**: the embedded Prism login window. It is a separate, untrusted
`BrowserWindow` with its own partition, and it returns the session cookies to the
main process.

**Stream**: the WebSocket that carries RFB frames between the runtime and the
renderer.

**Tier**: one of the process layers: the desktop app (Electron), the runtime (Go
daemon), the AHV plugin (linked into the runtime) and the agent. See
[Architecture overview](../architecture/overview.md).

**TOFU / pin**: trust on first use. The first certificate a cluster presents can be
accepted and then pinned by its SPKI hash. A pinned endpoint accepts only that key.

**Update manifest**: the per-platform file that describes an update. The app checks
its signature before it applies an update. See
[Update integrity](../architecture/update-integrity.md).

**vdagent**: the SPICE guest agent protocol used by QEMU. The clipboard uses it over a
serial port: COM3 on Windows guests, `/dev/ttyS2` on Linux guests.

## Related documentation

- [Documentation index](../README.md)
- [Architecture overview](../architecture/overview.md)
- [Wire protocol reference](protocol.md)
