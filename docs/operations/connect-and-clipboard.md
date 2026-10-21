# Connecting to a VM

This page shows you how to open a VM console, install the agent from the agent ISO,
use the host and guest clipboard, and declare the guest keyboard layout. It is for
operators, with short engineer notes after each task.

## Quick start

1. Sign in to an endpoint through the SSO window. Your Prism session authorizes
   everything. The app has no identity of its own.
2. In the **Lobby**, click a VM tile, or click **Join** on a session that is already
   live. The console opens in its own window.
3. Once per VM, open the agent action in the console toolbar and choose
   **Install prerequisites**. This uploads the agent ISO, attaches it as a CD-ROM and
   adds the serial port the clipboard uses.
4. Inside the VM, run the agent installer from the new CD-ROM. The agent then starts
   at every login.
5. Copy on the host and paste in the VM, or the reverse. Text only.
6. If typed symbols come out wrong, open the globe menu and pick the layout the guest
   uses.

## Connect

### Sign in

1. Select an endpoint. The app opens the SSO window.
2. Sign in with your Prism credentials.

The runtime stores the session cookies in the OS keychain and replays them on REST
and VNC calls. It saves rotated cookies automatically. There is nothing
app-specific to enroll. See [Configuring endpoints](configure.md#credentials) and
the [AHV plugin](../architecture/ahv-plugin.md).

### Open a VM

After sign-in you land in the **Lobby**, the VM browser. It has three areas:

| Area        | Shows                                                         |
| ----------- | ------------------------------------------------------------- |
| **live now** | VMs with an open session. **Join** attaches to the existing console window. |
| **Recent**  | VMs you opened in the last 24 hours                           |
| **all VMs** | The full inventory. Search, sort, and filter by power state, favorites or your folders. |

1. Click a VM tile to open a console, or click **Join** on a live session.
2. If the VM is powered off, the console still opens. Use its power menu to power
   the VM on.

Each console is its own OS window with its own pointer capture, keyboard capture
and connection. Closing one window does not affect the others.

### Engineer notes

`Shell.openConsole` in [`apps/desktop/src/renderer/app/Shell.tsx`](../../apps/desktop/src/renderer/app/Shell.tsx)
avoids duplicate sessions for the same VM:

- If the Lobby shows a live session, it confirms with `listSessions()` first. The
  Lobby's list refreshes on a 30-second poll and can be stale, and a closed window
  deletes its session. Reopening a dead id would fail before the WebSocket upgrade.
- If the session is live, it focuses the existing window.
- Otherwise it creates a session without a WebSocket and opens a window, which opens
  its own stream.

The main process opens each console window on the renderer bundle with
`?mode=console&session=<id>&vm=<name>`. The renderer mounts `ConsoleWindowApp` from
[`apps/desktop/src/renderer/app/ConsoleWindowApp.tsx`](../../apps/desktop/src/renderer/app/ConsoleWindowApp.tsx).
On start it:

1. Waits for the runtime to report `ready`. It rebuilds the SDK client whenever the
   loopback endpoint changes, because the runtime binds a new port on every start.
2. Builds an `AHVRemoteConsoleClient` whose `signRequest` and `signWebSocket` run in
   the main process. The IPC secret never reaches the renderer.
3. Opens the stream through `reopenStream`, which creates the session again if the
   runtime lost it.

Reconnect behaviour:

| Situation                                   | Behaviour                                         |
| ------------------------------------------- | ------------------------------------------------- |
| Drop after a successful connect             | Retries after 1 s, 2 s, 4 s, then 8 s five times, then stops |
| Socket never connected (deleted VM, wrong endpoint, unreachable cluster) | Shows the error overlay at once |
| Close code 1008 or 1009 (`session_expired`, `rbac_denied`, `vm_not_found`, `config_error`, `session_closed`, `frame_too_large`) | No retry. The error overlay drives recovery. |
| `session_expired` on a focused console      | Opens the SSO window (`reauthNow`). The console resumes after sign-in. |

When you close the window, the app deletes the current session on unmount and on
`beforeunload`. The runtime also removes idle sessions, so a missed close does no
harm.

> [!NOTE]
> AHV's QEMU refreshes VNC adaptively at about 30 fps, has Tight JPEG compiled out,
> and ignores `SetDesktopSize`. 16bpp RGB565 reduces bandwidth. It is not a quality
> setting. See [Runtime proxy](../architecture/runtime-proxy.md).

## Install the agent

The clipboard needs the agent running inside the VM. You deliver it with the agent
ISO.

1. In the console toolbar, open the agent and keyboard action group and choose
   **Install prerequisites**. A progress banner shows prepare, upload and attach.
   The upload is about 11 MB and the whole step takes about 30 seconds.
2. When it finishes, the console shows which CD-ROM to open and which installer to
   run. It shows both Windows and Linux steps, because the host cannot tell the guest
   OS without NGT.
3. Inside the VM, open the new CD-ROM drive and install the agent:

   | Guest              | Install                                                        |
   | ------------------ | -------------------------------------------------------------- |
   | Windows            | Double-click `ahv-remote-console-agent.exe`. It installs into your user profile, starts at login, and offers to restart. |
   | Linux desktop      | Install `linux/ahv-remote-console-agent.rpm` or `.deb`, then run **AHV Remote Console Agent Setup**. |
   | Linux headless, or no admin | Run `sh <mount-point>/linux/install.sh`. No root needed. |

The agent always installs per user, never machine-wide, and starts at each login.

> [!IMPORTANT]
> Read [Guest agent](guest-agent.md) before you install it. It lists every change the
> installer makes, including the one elevated step that writes the VM's firmware boot
> resolution, and explains why unsigned builds can be blocked by EDR or antivirus
> software. Without the agent, the console works but copy and paste does not.
For install, daemon and uninstall modes, see the
[agent CLI reference](../reference/agent-cli.md).

Running **Install prerequisites** again after a successful mount reuses the existing
image, so it is safe to repeat.

### Serial port and restart

The clipboard uses a SPICE qemu-vdagent channel on a dedicated serial port. In the
guest it is COM3. In the Prism VM spec it is serial port index 2. AHV cannot add a
serial port to a running VM.

| VM state                    | What the mount does                                |
| --------------------------- | -------------------------------------------------- |
| Off                         | Adds the CD-ROM and COM3 in one change             |
| COM3 already present        | Attaches the CD-ROM only                           |
| Running, no COM3            | Attaches the CD-ROM. Adds COM3 the next time the VM is seen powered off, or now through a power cycle if you consent. |

To apply COM3 now, click **Restart now**. This is the only control that sends
`powerCycle: true`. The banner then shows shutting down, configuring and starting.
The graceful guest shutdown can take about 2 minutes.

> [!WARNING]
> Never request the power cycle from code. It must come from the operator's
> explicit **Restart now** click.

The result of the serial step comes back as `serialNote`:

| Value                            | Meaning                                                 |
| -------------------------------- | ------------------------------------------------------- |
| `serial_port_added`              | COM3 added                                              |
| `serial_port_added_cycled`       | COM3 added through the consented power cycle            |
| `serial_port_present`            | COM3 already there                                      |
| `serial_port_power_off_required` | VM is on and no consent was given. Added at the next observed power-off. |
| `serial_port_deferred`           | The power cycle was not performed                       |
| `serial_port_power_denied`       | The consented power operation got a Prism 403. The cycle stops, the VM is left as found, and the deferred add stays armed. It is not retried. |

### Engineer notes

- The `useAgent` hook exposes `mountISO`. Its core, `runMountISO`, drives the
  progress banner and calls the SDK.
- The SDK sends `POST /v1/agent/mount-iso` with `session_id` and an optional
  `power_cycle: true`.
- The runtime resolves the session to a plugin. If the plugin does not implement
  `ISOMountCapable`, it returns 503 `iso_mount_unsupported`. Otherwise it calls
  `MountAgentISO` and writes an audit record with `serial_note`, `image_uuid` and
  `reused`. The ISO bytes are never recorded.

The AHV plugin's `MountAgentISO` runs apart from the HTTP request with a 15-minute
budget, because the upload alone can take minutes over a VPN. It:

1. Resolves the session to its endpoint and VM UUID.
2. Reads the ISO from `build/agent.iso`, or from `AHV_REMOTE_CONSOLE_AGENT_ISO_PATH`.
3. Builds the image name. If the role cannot create images, it looks for a published
   image by name. If there is none, it fails with `ErrImagePublishRequired`. The
   console shows this as `image_publish_required`, a notice to ask an administrator
   to publish the agent image.
4. Uploads the image, or reuses one whose description carries the same SHA. It then
   attaches the CD-ROM and adds COM3 in `AttachCDROMAndSerialPort`. That is one GET,
   one PUT and a task wait, retried once on a 409. Fields the plugin does not model
   are kept.
5. Removes older images it owns in the background, skipping any still referenced.

Prism Central chooses the cluster for an uploaded image, which may not be the VM's.
Placement handling, the mount and name-based dedupe are described in the
[AHV plugin](../architecture/ahv-plugin.md).

Source: [`apps/desktop/src/renderer/components/useAgent.ts`](../../apps/desktop/src/renderer/components/useAgent.ts),
[`plugins/ahv/plugin.go`](../../plugins/ahv/plugin.go),
[`plugins/ahv/internal/api/mount.go`](../../plugins/ahv/internal/api/mount.go),
[`cmd/isobuilder/assets/README.txt`](../../cmd/isobuilder/assets/README.txt).

## Use the clipboard

> [!IMPORTANT]
> While your console window is open, the VM cannot be live-migrated, because the
> clipboard channel uses its serial port. Before a maintenance window, close your
> console windows for the affected VMs. The VM becomes migratable about 3 seconds
> later. See
> [Live migration and maintenance windows](../../README.md#live-migration-and-maintenance-windows).

There is no switch to turn on. Once the agent runs in the VM and COM3 exists, the
clipboard works. The console marks the agent connected as soon as the console tab is
active, because the clipboard flows over the serial channel.

1. Copy text on the host.
2. Paste in the VM.

The reverse direction works the same way.

- Only text is transferred. Images and files are not.
- The clipboard keeps working under a full-tunnel VPN. It travels over the console
  connection and the serial channel, below the guest's network stack.
- Clipboard text is never logged or audited. Only the byte length, direction and a
  SHA-256 are recorded.

For the data path and echo suppression, see [Clipboard](../architecture/clipboard.md).
For the agent side, see [Agent](../architecture/agent.md).

## Set the keyboard layout

The console sends keys the way a physical keyboard does. The VM applies its own
layout. If symbols come out wrong, tell the console which layout the guest uses.

1. Open the globe menu in the console toolbar.
2. Pick the guest layout: US, UK or Italian.

This is a declaration of the VM's layout. It changes nothing inside the VM and types
nothing. The choice is saved per VM in `localStorage`. The default is Italian.

If the guest has the wrong input language active, choose **Cycle guest input
language (Win+Space)** in the same menu. It sends Super+Space to the guest, the only
in-band option VNC offers.

### Engineer notes

The wire model is verified against AHV's QEMU `-k en-us` keymap. A character is sent
as the en-us base keysym of the physical key that carries it on the guest layout,
plus a modifier level: none, shift, altgr or shift+altgr. QEMU masks the keymap
entry's own shift bit, so the console controls only which key is pressed and the
modifiers with it.

The tables live in
[`apps/desktop/src/renderer/input/GuestLayouts.ts`](../../apps/desktop/src/renderer/input/GuestLayouts.ts).
`us`, `uk`, `it` and `de` are complete for their printable set. `fr` and `es` are
best-effort until validated live. Dead-key accents are not included. The picker
passes its selection to the input engine through `setGuestLayout`.

There are two layout lists:

- `KeyboardLayoutMenu.KEYBOARD_LAYOUTS` has 10 ids: us, uk, it, de, fr, es, pt-br,
  ch-de, ch-fr, jp.
- The globe picker offers only us, uk and it. A saved choice outside that set keeps
  working, because its table exists. It is not offered for new choices.

For keysyms, AltGr, characters a layout cannot produce, and the agent path for
them, see the [keyboard reference](../reference/keyboard.md).

## Limitations

- The clipboard carries text only.
- AHV cannot add a serial port to a running VM. COM3 needs a power-off or a
  consented power cycle.
- A role that cannot create images needs an administrator to publish the agent
  image.
- The globe picker offers only US, UK and Italian.
- The qemu-vdagent channel blocks QEMU live migration. A native fix is expected in
  AHV 12.0. The console-following libvirt hook (`scripts/ahv-libvirt-hook-qemu.py`)
  switches COM3 to qemu-vdagent only while a console is attached and switches it
  back a few seconds after disconnect, so idle VMs stay migratable.

> [!WARNING]
> The hook's manager runs in dry-run mode until you arm it with the
> `AHVRC_MGR_ARMED` environment variable or an `ahvrc-mgr-armed` flag file. Unarmed,
> it does not switch the backend. See [Live migration](../architecture/live-migration.md).

## Troubleshooting

| Symptom                                       | Likely cause                                      | See                                                  |
| --------------------------------------------- | ------------------------------------------------- | ---------------------------------------------------- |
| "Connecting…" then the error overlay          | The socket never connected: deleted VM, wrong endpoint or unreachable cluster | [Runtime proxy](../architecture/runtime-proxy.md) |
| **Re-authenticate** overlay                   | The Prism session expired. A focused console opens the SSO window. | [Configuring endpoints](configure.md#credentials) |
| Clipboard does nothing                        | Agent not running in the guest, or no COM3        | [Install the agent](#install-the-agent)              |
| "Ask an administrator to publish the agent image" | Role cannot create images and the image is not in the catalog | [AHV plugin](../architecture/ahv-plugin.md) |
| COM3 add deferred                             | VM running and no power-cycle consent. Added at the next observed power-off. | [Serial port and restart](#serial-port-and-restart) |
| Wrong symbols when typing                     | Declared layout differs from the VM's layout      | [Set the keyboard layout](#set-the-keyboard-layout)  |
| VM cannot live-migrate with a console open    | qemu-vdagent migration blocker; hook may be unarmed | [Live migration](../architecture/live-migration.md) |

For more, see [Troubleshooting](troubleshooting.md).

## Related documentation

- [Architecture overview](../architecture/overview.md)
- [Clipboard](../architecture/clipboard.md)
- [Keyboard reference](../reference/keyboard.md)
- [Agent CLI reference](../reference/agent-cli.md)
- [AHV plugin](../architecture/ahv-plugin.md)
- [Live migration](../architecture/live-migration.md)
