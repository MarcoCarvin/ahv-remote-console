# In-guest agent

The agent is the optional program that runs inside the VM. This page explains
what it does to the guest, how its modes fit together, and how it talks to the
host. It is for operators who install it and developers who work on it.

## What the agent does

The console works without the agent: you can connect, see the screen and type.
The agent adds two things the hypervisor side cannot do alone:

- **Clipboard sync** between your workstation and the guest OS. This is the main
  reason to install it. The clipboard travels over a serial port, below the
  guest network stack, so it keeps working under a full-tunnel VPN.
- **Display sizing.** AHV's emulated VGA ignores the usual remote-resize
  request, so the agent sets the guest resolution from inside, at boot and at
  login.

The agent is one binary. The same executable is the graphical installer, the
resident daemon that runs at login, and a few one-shot maintenance and
diagnostic modes. It installs per user, never machine-wide.

```text
 workstation          AHV host                 guest VM
 desktop app --VNC--> QEMU --serial (COM3)--> agent daemon
                      qemu-vdagent backend      |-- guest clipboard
                                                '-- display settings
```

## Effect on the guest

- It runs as the logged-in user, never as SYSTEM or root. Windows uses an
  `HKCU\...\Run` entry; Linux uses a `systemd --user` unit. Another user on the
  same VM cannot read your clipboard.
- It stores no credentials or secrets. It never logs clipboard contents, only
  the message type and byte length.
- On Windows it can change the boot resolution, which needs one UAC prompt. The
  prompt names the binary ("AHV Remote Console Guest Agent"), not a resolution
  tool. The resize is never shown in the UI.
- On Windows guests, while a console is attached, it keeps the display "warm"
  with one invisible pixel in a screen corner, and hides the guest mouse cursor
  so you see only your local cursor. Both stop when you close the console. See
  [Keep-warm](#keep-warm-windows-only).

### Installing

The agent ships on an ISO. Upload it to the Prism Image Service, attach it to
the VM as a CD-ROM, and run the agent from the disc.

- On Windows, double-click `ahv-remote-console-agent.exe`. With no arguments it
  is the graphical installer. It copies itself into your profile, starts at
  login, and offers to restart.
- On desktop Linux, install the `.rpm` or `.deb` from the `linux/` folder, then
  launch **AHV Remote Console Agent Setup** and follow the dialogs.
- On headless Linux, or without admin rights, run
  `sh <mount>/linux/install.sh`. It installs for your user only.

You can eject the disc after install. To remove the agent, run it with
`--uninstall`. Full flag details are in the [agent CLI reference](../reference/agent-cli.md).

### When the clipboard does not work

The agent degrades instead of failing. If the serial port or a guest clipboard
helper is missing, the daemon keeps running and still sets the display. Only
the clipboard bridge is off. Two common causes:

- The serial port is absent. AHV must add it to the VM. This needs a full
  power-off and power-on. A reboot from inside the guest is not enough.
- On Linux, your user cannot read the serial device. Setup detects this and
  offers to add you to the owning group. The change takes effect at next login.

## Modes

`parseFlags` reads the flags once and `run` dispatches. `--install` and
`--uninstall` together are rejected.

| Invocation | What it does |
| --- | --- |
| no arguments, Windows | WebView2 graphical installer |
| no arguments, other OS | resident daemon |
| `--daemon` | resident agent: clipboard bridge and display pin. The autostart entry runs this. |
| `--install` | copy itself to the per-user install directory, register autostart, exit |
| `--uninstall` | reverse of `--install` |
| `--setup` (Linux) | dialog-driven install (zenity, kdialog or terminal) |
| `--apply-display` | elevated one-shot: write the boot resolution and exit |
| `--version` | print build info and exit |
| `--uplink-probe` | one-shot diagnostic of about 50 s, then exit |
| `--default-resolution` | the standard resolution target (modifier) |
| `--log-level` | `debug`, `info`, `warn` or `error` (default `info`) |

### `--default-resolution`

This flag is the single source of the standard console resolution. The default
depends on the platform: `1920x1080` on Windows, `max` on Linux, `none`
elsewhere. Accepted values:

- `WxH`, a bare token such as `1920x1080`
- `max` or `largest`, the biggest mode the display offers
- `none` or empty, to leave the display alone

The value is validated as soon as it is parsed. `parseResolution` accepts
exactly two positive integers joined by one lowercase `x`. This stops a raw
string from reaching the autostart command, the `HKCU\Run` value or the
elevated `ShellExecute` call. Without it, a value such as
`"1920x1080 --uninstall"` would be split by the OS at next login and bake an
uninstall into autostart.

The chosen value is passed to every process the agent spawns: the autostart
command, the immediate detached start, and the elevated boot-resolution write.

### Windows console output

The Windows binary is linked with `-H windowsgui`, so the autostarted daemon
never flashes a console window. As a result, output from a CLI mode has no
console when you run it from `cmd.exe`. `run` calls `attachParentConsole()`
before parsing flags, so even a flag error is visible.

Source: [`apps/ahv-remote-console-agent/cmd/ahv-remote-console-agent/main.go`](../../apps/ahv-remote-console-agent/cmd/ahv-remote-console-agent/main.go)

## Clipboard over serial

The agent speaks the **SPICE vdagent protocol** over an ISA serial port: `COM3`
on Windows, `/dev/ttyS2` on Linux. This is the protocol spice-vdagent uses over
virtio-serial. On AHV, a libvirt hook on the host swaps the port's backend to
`qemu-vdagent` while a console is connected. The QEMU vdagent backend does not
care which serial frontend carries the bytes.

The port is opened raw, 115200 8N1, with exclusive access. Reads time out after
about 100 ms so the read loop can check for shutdown.

### Wire format

All fields are little-endian.

| Part | Size | Fields |
| --- | --- | --- |
| VDIChunkHeader | 8 bytes | `uint32 port` (`1` host to guest, `2` guest to host), `uint32 size` |
| VDAgentMessage | 20 bytes | `protocol` (=1), `type`, `uint64 opaque`, `size`, then payload |

A message is split into chunks of at most 1024 payload bytes. Reassembly
handles multi-chunk bodies, headers that straddle a chunk boundary, and several
messages in one chunk. Caps: 16 MB per reassembled message, 10 MB per clipboard
payload.

### Capabilities and backend swaps

The agent advertises `CapClipboardByDemand` and `CapClipboardSelection`. It never
advertises `CapClipboardGrabSerial`. Clipboard counts as enabled only when the
host advertises `CapClipboardByDemand` back.

The backend can change under the agent. The host hook swaps it to
`qemu-vdagent` when a console opens, and a new vdagent peer can appear after a
live migration. The agent handles this with a permanent capabilities
heartbeat: `capsRetryLoop` sends `ANNOUNCE_CAPABILITIES` every 5 s until the
clipboard is first negotiated, then every 15 s for the life of the process. A
new peer is answered within that interval, with no restart.

Other recovery paths:

- Opening the serial port is retried up to 10 times, 1 s apart, before the
  clipboard is disabled.
- A stall of more than 3 s inside a frame (`frameStallTimeout`) is treated as
  the backend going away. The frame is dropped and reassembly resets.
- Read errors back off progressively, and leftover bytes from the old backend
  are drained.

### Clipboard flows

**Host to guest.** The host sends a `GRAB`, the agent requests the data, the
host sends it, and the agent writes it to the guest clipboard. Before writing,
the agent sanitizes the text again: it strips C0 and C1 controls, DEL, bidi
overrides and zero-width characters, and keeps tab, newline and CR. The write
is retried up to 3 times to absorb Windows `OpenClipboard` contention. If it
still fails, the agent returns the error instead of reporting success.

**Guest to host.** A `Watch` event triggers a clipboard read. If the text is new
and not an echo, the agent sends a `GRAB` and keeps the text in a **grab
cache**. The host's `CLIPBOARD_REQUEST` is served from that cache, not from a
second OS read. A second read can stall for about 2 s under Windows contention
or come back empty.

**Echo suppression.** Before writing host text, the agent records a hash of its
canonical form, with line endings folded to LF. A guest change with the same
hash is its own write coming back and is not sent to the host. A CRLF round
trip through the Windows clipboard therefore does not cause an echo. If the
write fails, the marker is cleared, so a later identical guest copy still goes
through.

### Control tokens

Three tokens with a fixed prefix ride the clipboard channel. The agent handles
them before any clipboard write. They come from the runtime proxy and are never
operator data.

| Token | Purpose |
| --- | --- |
| `__AHVRC_RESIZE__<W>x<H>` | display resize, bounded to 320x240 through 7680x4320 |
| `__AHVRC_KEEPWARM__<ttl_s>` | keep-warm lease: arm or extend if `ttl>0`, disarm if `0`. TTL is capped at 300 s. |
| `__AHVRC_RESIZE__keepwarm-probe` | capability probe. A current agent replies `__AHVRC_KEEPWARM_ACK__`. An older agent sends it to the resize handler, which ignores it. |

### Cleartext handling

Clipboard cleartext is wiped wherever it is buffered:

- the clipboard adapter copies and then wipes
- `wipeBytes` zeroes outbound buffers
- the grab cache is wiped on use, on each new watch event, on read-loop
  recovery, and after 10 s (`grabCacheTTL`)

Payloads are never logged, only type and length. The `clipboard.Provider`
contract forbids writing cleartext to disk.

### Guest clipboard backends

| Guest | Backend | Change detection |
| --- | --- | --- |
| Windows | `user32` `GetClipboardData` / `SetClipboardData` | event-driven |
| Linux, Wayland | `wl-copy` / `wl-paste` | `wl-paste --type text/plain --watch` |
| Linux, X11 | `xclip` or `xsel` | polling fallback |
| macOS | not supported | |

On Linux a missing helper is not fatal. The agent logs that clipboard sync is
off and keeps running.

Source: [`internal/vdagent/agent.go`](../../apps/ahv-remote-console-agent/internal/vdagent/agent.go),
[`internal/vdagent/protocol.go`](../../apps/ahv-remote-console-agent/internal/vdagent/protocol.go),
[`internal/clipboard/clipboard.go`](../../apps/ahv-remote-console-agent/internal/clipboard/clipboard.go)

## Keep-warm (Windows only)

Keep-warm works around two effects of AHV's QEMU display:

- On an idle desktop, QEMU's `vnc_refresh` timer backs off toward 3000 ms, so
  the first frame after idle is slow.
- AHV's standard VGA has no hardware cursor. The guest draws its own pointer,
  which trails your local cursor by a VNC round trip.

The runtime proxy drives keep-warm with a lease over the clipboard channel. It
sends `__AHVRC_KEEPWARM__45` and renews it every 20 s while a console is
attached, and sends `ttl=0` on detach. If no cancel arrives, the lease expires
on its own. While the lease is live, two operations run on a locked OS thread:

1. **Display dirty op.** A 1x1 transparent, topmost tool window in a screen
   corner changes its single pixel by one bit every 25 ms. A real pixel change
   is needed, because QEMU's dirty-page tracking ignores anything that does not
   touch the framebuffer. 25 ms keeps `vnc_refresh` at its 30 ms floor.
2. **Cursor takeover.** Every standard system cursor is replaced with an
   invisible one through `SetSystemCursor`. On lease end the cursors are
   restored with `SystemParametersInfo(SPI_SETCURSORS)`.

Process exit does not undo `SetSystemCursor`. So on shutdown the daemon calls
`keepwarm.Stop()`, which waits up to 3 s for the cursors to be restored. At
startup the agent also restores the cursor scheme, in case an earlier daemon
died during a lease.

On Linux and macOS both operations are no-ops. They return
`errors.ErrUnsupported` once and are not retried. The console works, without
the refresh floor or the cursor takeover. The agent is a static `CGO_ENABLED=0`
binary with no in-process X11 or Wayland client to draw a 1x1 surface, and
spawning a helper at this rate is not worth it.

Source: [`internal/keepwarm/keepwarm.go`](../../apps/ahv-remote-console-agent/internal/keepwarm/keepwarm.go)

## Display sizing

AHV's emulated VGA (QEMU std-VGA, Bochs) ignores the RFB `SetDesktopSize`
request, so resizing must happen inside the guest. There are two paths.

### Boot resolution (`--apply-display`, Windows, elevated)

`internal/uefivar` writes OVMF's `PlatformConfig` UEFI variable from the running
guest. This replaces the F2 "Change Preferred Resolution" menu.

- The variable is `{ UINT32 width; UINT32 height }`, little-endian.
- It is written with the attributes OVMF uses: `NON_VOLATILE |
  BOOTSERVICE_ACCESS | RUNTIME_ACCESS` (0x7). The firmware rejects any other set.
- A reboot applies the new resolution to the boot display adapter.

This needs `SeSystemEnvironmentPrivilege`. The agent enables it only around the
single `SetVariable` call. Other platforms get a stub. A read-only variable
store is not fatal. For `none`, or a `max` that cannot be resolved, nothing is
written.

### Runtime resolution (daemon, not elevated)

At every daemon start, which means every login, the daemon sets the standard
resolution on the running display. It runs off the startup path, waits for the
display modes to appear, and gives up quietly after about 2 minutes on a
headless session.

- **Windows** uses `EnumDisplaySettingsExW` and `ChangeDisplaySettingsExW` on the
  primary monitor. `ApplyExact` sets exactly the requested mode, with no
  nearest-mode fallback. If the change needs a restart
  (`DISP_CHANGE_RESTART`), it returns `ErrRestartRequired` and the daemon
  schedules a reboot with a 5-second countdown.
- **Linux** uses the running display server, never a raw DRM modeset: `xrandr`
  on X11 (it can add a missing mode through `cvt`), Mutter through `gdbus`,
  `kscreen-doctor` on KDE, or `wlr-randr`. Each change is confirmed by reading
  the mode back, because these tools' exit codes are unreliable. The change
  applies live, with no reboot. If the session environment is not yet
  imported, the daemon reads `DISPLAY` and `WAYLAND_DISPLAY` from a compositor
  process of the same user.

`PickBestMode` chooses an exact match first, then the closest mode no larger on
either axis, then the smallest mode larger on both. `LargestMode` backs the
`max` target. The resize control token uses `ApplyMode`, which picks the best
fit. The startup pin uses `ApplyExact`, which is exact or nothing.

Source: [`internal/display/display.go`](../../apps/ahv-remote-console-agent/internal/display/display.go),
[`internal/uefivar/uefivar.go`](../../apps/ahv-remote-console-agent/internal/uefivar/uefivar.go)

## Autostart and the single-instance guard

### Install and autostart

`--install` copies the binary to a per-user location and registers autostart
that runs `--daemon --default-resolution=<res>`.

| | Windows | Linux |
| --- | --- | --- |
| Binary | `%LOCALAPPDATA%\AHV Remote Console\ahv-remote-console-agent.exe` | `~/.local/bin/ahv-remote-console-agent` |
| Autostart | `HKCU\Software\Microsoft\Windows\CurrentVersion\Run`, value `AHV Remote Console Guest Agent` | `~/.config/systemd/user/ahv-remote-console-agent.service`, `Restart=on-failure` |
| Start now | detached process (`DETACHED_PROCESS`) | `systemctl --user daemon-reload`, `enable`, `restart` |

On Linux the installer uses `restart`, not `enable --now`. `--now` does nothing
for a unit that is already active, so the new binary would not take over.
Windows registry calls go through `advapi32` directly.

Running the installer again is safe. It stops other running instances first so
the target file is not held open. On Linux it unlinks the old binary before
copying, to avoid `ETXTBSY`.

### Legacy `hcbridge` install

The agent's older name is `hcbridge`. The install directory and guard names are
the same under both names, so a running `hcbridge` daemon would hold the guard
and make the new daemon exit. Both installers therefore stop the legacy daemon
and remove its autostart entry and binary on every install and uninstall.

### Single-instance guard

The daemon expects one process per user session, because it owns the keyboard
hook, the clipboard watcher and the serial port. It takes a per-user guard
before doing anything else. It retries for about 5 s (10 x 500 ms), since the OS
can take a moment to release the guard of a process the installer has
stopped. If the guard is still held, the daemon exits cleanly.

- **Windows:** named mutex `Local\AHVRemoteConsoleBridge-<SID>`. It falls back to
  the user name, then to a fixed suffix. `Local\` is scoped per logon session,
  so each user and RDP session has its own. The OS releases the mutex even on a
  crash.
- **Linux:** `flock(2)` with `LOCK_EX|LOCK_NB` on `$XDG_RUNTIME_DIR/hcbridge.lock`,
  or `/tmp/hcbridge-<uid>.lock` as a fallback. The kernel releases it on exit.

## ISO delivery

The agent ships as an ISO9660 + Joliet image built by
[`cmd/isobuilder`](../../cmd/isobuilder/main.go), written in pure Go. It does not
call `mkisofs` or `hdiutil`, so it builds on the macOS CI runner.

| Path on the disc | Contents |
| --- | --- |
| `/ahv-remote-console-agent.exe` | Windows binary. The build fails if no agent binary is present. |
| `/linux/ahv-remote-console-agent-linux-*` | raw Linux binaries |
| `/linux/ahv-remote-console-agent.rpm`, `.deb` | desktop Linux packages |
| `/linux/install.sh` | per-user fallback installer |
| `/README.txt`, `/autorun.inf` | text assets, converted to CRLF |

The Linux binaries and `install.sh` are marked executable in RockRidge, so a
loop mount that allows exec can run them directly.

### Build pipeline

The root Makefile chains three targets:

1. `agent-bins` cross-compiles the guest targets into `build/agent/`.
2. `agent-pkgs` builds the `.rpm` and `.deb`.
3. `iso` assembles `build/agent.iso`.

The agent's own Makefile builds `linux/amd64`, `windows/amd64` and
`windows/arm64`. The root Makefile stages only the two amd64 targets, so the
shipped ISO is x86-64 only. AHV guests are x86-64 in practice, and the Windows
resource file is generated for amd64 only. All targets build with
`CGO_ENABLED=0`, and the Windows build links `-H windowsgui`.

ISO timestamps can be set to the git commit date (`-mtime`). Identical inputs
then produce an identical image, and the SHA-256-named catalog image dedupes.

## Live migration

The `qemu-vdagent` backend blocks QEMU live migration for as long as it exists.
A host-side libvirt hook arms the backend only while a console is connected and
removes it shortly after disconnect, so idle VMs stay migratable. The agent
needs no special handling: its capabilities heartbeat answers the new peer
after each swap or migration. See [vdagent and live migration](live-migration.md).

## Diagnostics

- `--version` prints `ahv-remote-console-agent <version> (<commit>, built <date>)`
  from values set at build time with `-ldflags -X`.
- `--uplink-probe` runs a one-shot painting test of about 50 s with five
  variants, then exits. It skips the single-instance guard and the clipboard.
  You report which colours appear. It is Windows-only and returns
  `ErrNotImplemented` elsewhere.
- The daemon logs JSON to a file and to stderr. On Windows the file is
  `%LOCALAPPDATA%\AHV Remote Console\hcbridge.log`. The file is the main sink,
  because the Windows build has no console. It appends across restarts and is
  kept under 5 MiB by a periodic truncator. Log paths for each OS are in
  [Audit log and application logs](audit-and-logging.md#guest-agent-hcbridgelog).

## Design rules

- No clipboard plaintext in logs or audit. Any record about clipboard data
  carries only byte length, direction and SHA-256. No clipboard audit records
  are written in this release. Cleartext buffers are wiped after use.
- Per user, never machine-wide. Autostart is `HKCU\Run` or `systemd --user`.
- Degrade, do not fail. A missing serial port or clipboard helper disables
  only the clipboard bridge.
- Static binaries. All builds are `CGO_ENABLED=0`. Only the amd64 targets
  ship on the ISO.
- Silent display resize. The resize is never named in the UI or the UAC
  prompt.

## Related documentation

- [Agent CLI reference](../reference/agent-cli.md)
- [Clipboard](clipboard.md)
- [vdagent and live migration](live-migration.md)
- [Audit log and application logs](audit-and-logging.md)
- [Hypervisor setup](../operations/hypervisor-setup.md)
- [Connect and clipboard](../operations/connect-and-clipboard.md)
