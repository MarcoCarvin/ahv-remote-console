# Agent command line

The agent runs inside a Nutanix AHV guest VM. It syncs the clipboard between host and
guest and sets a standard display resolution for the desktop app. This page lists its
command-line flags and what each mode does on Windows and Linux. It is for operators
who install the agent and developers who change it.

The agent is one binary: `ahv-remote-console-agent`, or `ahv-remote-console-agent.exe`
on Windows. It ships on the agent ISO. There are no subcommands. Each mode is a
boolean flag, and two value flags change how the modes behave.

> [!TIP]
> Most operators never type a flag. On Windows, mount the agent ISO and double-click
> the binary to run the graphical installer. On Linux, run the **Agent Setup**
> launcher, which runs `--setup`. The other flags are for unattended installs,
> automation and diagnostics.

Source: [`apps/ahv-remote-console-agent/cmd/ahv-remote-console-agent/`](../../apps/ahv-remote-console-agent/cmd/ahv-remote-console-agent/),
mainly `main.go` and its platform-specific siblings.

## Contents

- [Flags](#flags)
- [No flag](#no-flag)
- [--daemon](#--daemon)
- [--install](#--install)
- [--uninstall](#--uninstall)
- [--setup](#--setup)
- [--apply-display](#--apply-display)
- [--uplink-probe](#--uplink-probe)
- [--version](#--version)
- [--default-resolution](#--default-resolution)
- [--log-level](#--log-level)
- [Logging](#logging)
- [Elevation](#elevation)
- [Windows and Linux compared](#windows-and-linux-compared)
- [Known limitations](#known-limitations)

## Flags

| Flag | Kind | Purpose | Windows | Linux | Elevation |
| --- | --- | --- | --- | --- | --- |
| (none) | Mode | Platform default | Graphical installer | Runs as `--daemon` | None |
| `--daemon` | Mode | Resident agent: clipboard bridge and display pin | Yes | Yes | None |
| `--install` | Mode | Per-user autostart install | `HKCU\...\Run` | systemd `--user` unit | None |
| `--uninstall` | Mode | Reverse of `--install` | Yes | Yes | None |
| `--setup` | Mode | Interactive install with dialogs | Error; use the graphical installer | Yes | None. May ask for `pkexec` for the serial group. |
| `--apply-display` | Mode | One-time write of the OVMF boot resolution | Yes | Yes, through efivarfs | Required: Administrator or SYSTEM, or root |
| `--uplink-probe` | Mode | One-time uplink experiment, about 50 s | Yes | Error, Windows only | None |
| `--version` | Mode | Print build information and exit | Yes | Yes | None |
| `--default-resolution=<WxH\|max\|largest\|none>` | Value | Standard guest resolution | Default `1920x1080` | Default `max` | n/a |
| `--log-level=<debug\|info\|warn\|error>` | Value | Log level | Default `info` | Default `info` | n/a |

On other platforms the `--default-resolution` default is `none`.

`run()` checks the mode flags in this order and runs the first one set: `--version`,
`--install`, `--uninstall`, `--setup`, `--apply-display`, `--uplink-probe`,
`--daemon`, then the platform default. `--install` and `--uninstall` together are
rejected as mutually exclusive.

## No flag

| Platform | Behaviour |
| --- | --- |
| Windows | Opens the graphical installer: a WebView2 window styled like the desktop app. This is what the operator double-clicks on the ISO. |
| Windows without WebView2 | Falls back to a headless install with plain message boxes, for example on older Windows Server SKUs |
| Linux and others | No GUI. Runs as the resident daemon, the same as `--daemon`. |

The Windows installer works in two steps:

1. It installs the binary as the per-user resident agent, without elevation.
2. If the display is not already at the standard resolution, it relaunches itself
   elevated with `--apply-display` to write the OVMF boot resolution. This step is
   silent in the installer UI. See [Elevation](#elevation).

## `--daemon`

The resident agent. The autostart entry runs it at every login. It runs the clipboard
bridge and the display pin.

### Clipboard bridge

The bridge speaks the SPICE qemu-vdagent protocol over an ISA serial port.

| Platform | Serial port |
| --- | --- |
| Windows | `COM3` |
| Linux | `/dev/ttyS2` |

The AHV libvirt hook switches the serial backend to qemu-vdagent a few seconds after
QEMU starts. The agent therefore tries to open the port up to 10 times, 1 second
apart, before it disables the clipboard.

### Display pin

At startup the daemon sets the resolution from `--default-resolution`
(`applyDefaultResolution`). This runs in the background, is best effort, and never
blocks or breaks the clipboard. See [`--default-resolution`](#--default-resolution).

### Missing parts

The daemon keeps running when parts are missing:

| Missing | Effect |
| --- | --- |
| Guest clipboard helper. Linux needs `wl-clipboard` for Wayland, or `xclip` or `xsel` for X11. | Logs a warning and stays running with the clipboard disabled. The display pin still works. |
| Serial port | Clipboard disabled. The daemon keeps running. |

### Single instance

The daemon allows one instance per user. It tries to take the guard up to 10 times
over about 5 seconds. If another instance holds it, this one exits cleanly with no
error.

| Platform | Guard |
| --- | --- |
| Windows | Named mutex `Local\AHVRemoteConsoleBridge-<SID>`, falling back to the user name. Scope is the logon session. |
| Linux | `flock(2)` on `$XDG_RUNTIME_DIR/hcbridge.lock`, or `/tmp/hcbridge-<uid>.lock` |
| Others | No-op |

The daemon logs to `hcbridge.log` as well as stderr. See [Logging](#logging).

The keep-warm lease, which counters AHV's idle frame-rate backoff, lives in the
vdagent layer and is active only on Windows guests. It has no CLI flag.

## `--install`

Copies the binary to a per-user location, registers per-user autostart, and exits. It
needs no administrator or root rights. Install is per user so the clipboard stays
scoped to the operator's session.

| Platform | Binary location | Autostart |
| --- | --- | --- |
| Windows | `%LOCALAPPDATA%\AHV Remote Console\ahv-remote-console-agent.exe` | `HKCU\...\CurrentVersion\Run` value `AHV Remote Console Guest Agent` |
| Linux | `~/.local/bin/ahv-remote-console-agent` | systemd user unit `~/.config/systemd/user/ahv-remote-console-agent.service` |
| Others | Not supported. Returns an error. | |

The autostart command is `--daemon --default-resolution=<res>`. The chosen resolution
is written into it, so every login sets the same resolution.

After install, the Windows installer starts the new daemon detached, so the clipboard
works in the current session without a new login. The Linux installer runs
`systemctl --user daemon-reload`, `enable` and `restart`. It uses `restart` rather
than `enable --now` so a re-run picks up the new binary.

Both installers can upgrade in place. Before copying, they stop any running agent
under the current name and under the legacy name `hcbridge`, so the target file is not
in use. Afterwards they remove legacy `hcbridge` autostart entries.

## `--uninstall`

Reverses `--install`. No elevation needed.

| Platform | What it removes |
| --- | --- |
| Windows | The `HKCU\...\Run` value, the installed binary, and leftover legacy `hcbridge` entries |
| Linux | Runs `systemctl --user disable --now`, removes the unit file, the binary and legacy `hcbridge` entries, then runs `daemon-reload` |
| Others | Returns the unsupported-platform error |

## `--setup`

Interactive install for Linux, the counterpart of the Windows graphical installer.
GNOME does not run binaries from removable media. The `.rpm` and `.deb` packages
therefore install the binary to `/usr/bin` with an **Agent Setup** launcher that runs
`--setup`.

On Windows and other platforms, `--setup` returns an error that tells the operator to
run the agent with no arguments.

`runSetupMode` runs these steps:

1. Picks a dialog backend: `zenity` (GNOME) or `kdialog` (KDE) if found on `PATH`,
   otherwise a plain terminal if stdin is a TTY. The terminal mode makes `--setup` work
   over SSH. If none works, it fails and points to `--install`.
2. Shows a confirmation dialog, then a resolution picker with `none`, `1920x1080`,
   `2560x1440`, `3840x2160` and `max`. `none` is preselected.
3. Runs the same per-user install as `--install`.
4. Checks serial-port access, the one thing the daemon cannot fix itself:
   - If the user cannot read and write the vdagent serial port, it offers to run
     `pkexec usermod -aG <group> <user>`. This asks for an administrator password. The
     user must log in again afterwards.
   - If the port does not exist, it advises a full power-off, adding the serial port,
     and powering on again.

`--setup` runs without elevation. Only the optional serial-group fix asks for a
password.

## `--apply-display`

Writes the OVMF boot resolution, the firmware's preferred-resolution setting, from
`--default-resolution`, then exits. The Windows installer runs it as an elevated
relaunch. Operators do not normally run it by hand.

| Value | Target |
| --- | --- |
| `WxH` | That exact mode |
| `max`, `largest` | The largest available mode |
| `none` or empty | No write. Exits without touching the firmware variable store. |

It writes OVMF's `PlatformConfig` UEFI variable (`uefivar.SetBootResolution`), reads
it back to verify, and does not reboot.

| Platform | Mechanism | Requirement |
| --- | --- | --- |
| Windows | `SetFirmwareEnvironmentVariableExW` | `SeSystemEnvironmentPrivilege`, held only by an elevated Administrator or SYSTEM token. Without it the command fails with an error asking to run elevated. |
| Linux | efivarfs | root, for example through `sudo` or `pkexec`. The daemon never does this write. |
| Others | Not supported | Returns an error |

The write is best effort. A read-only OVMF variable store, which is common on some AHV
builds, a legacy-BIOS VM, or a target that cannot be resolved are not errors. The
daemon still sets the running display at login.

## `--uplink-probe`

A one-time diagnostic. It runs a Win32 painting experiment with five variants for
about 50 seconds, then exits. It skips the daemon lifecycle: no single-instance guard
and no clipboard. The operator runs it once and reports which colours appeared.

| Platform | Behaviour |
| --- | --- |
| Windows | Runs the probe |
| Linux and others | Returns `ErrNotImplemented` ("only implemented on Windows") |

No elevation needed. It honours `--log-level`.

## `--version`

Prints one line and exits:

```text
ahv-remote-console-agent <version> (<commit>, built <date>)
```

Version, commit and date are set at link time through the Makefile's `-ldflags -X`.
Builds from `go run` or `go test` print `dev (unknown, built unknown)`.

## `--default-resolution`

The standard guest resolution. One value drives both the boot-time OVMF write
(`--apply-display`) and the per-login display pin (`--daemon`). Install writes it
into the autostart command.

| Value | Effect |
| --- | --- |
| `none` or empty | Leave the resolution unchanged |
| `WxH`, for example `1920x1080` | Set exactly that mode. There is no snapping to the nearest mode (`display.ApplyExact`). |
| `max`, `largest` | The largest mode the display offers |

| Platform | Default | Reason |
| --- | --- | --- |
| Windows | `1920x1080` | Known to work on AHV standard VGA; the driver always offers this mode |
| Linux | `max` | xrandr and gdbus apply changes live, and modes list reliably once the graphical session is up |
| Others | `none` | The display code is a no-op |

> [!IMPORTANT]
> The value ends up in command lines: the `HKCU\...\Run` value, the systemd
> `ExecStart` line and the elevated `ShellExecute` parameters. `parseFlags` therefore
> validates it strictly at startup. It accepts only `none`, `max`, `largest`, or a bare
> `WxH` with a lowercase `x`, two positive base-10 integers and nothing after them.
> Anything else is rejected. This blocks a value like
> `--default-resolution="1920x1080 --uninstall"` from turning into an autostart entry
> that uninstalls the agent.

How the daemon applies the pin:

| Platform | Mechanism | Notes |
| --- | --- | --- |
| Windows | `ChangeDisplaySettingsExW` | If Windows accepts the mode but needs a reboot to show it, the daemon saves it to the registry and schedules a reboot with a 5-second countdown, so it applies at the next login |
| Linux | xrandr on X11, or the compositor's tool on Wayland | Applies live, no reboot |
| Others | Not run | Skipped, so no "not available" warning at each login |

The daemon can start before the graphical session. The pin therefore waits for the
display backend for up to about 2 minutes, checking every 5 seconds. On Windows the
modes are available at once, so the first check succeeds.

## `--log-level`

Sets the log level for the daemon and the probe. The default is `info`. Accepted values
are `debug`, `info`, `warn` and `error`. Logs are JSON.

## Logging

The daemon and the probe write to a log file as well as stderr. An autostarted agent
has no console, so stderr is lost. The file is the place to look when diagnosing.

| Platform | Log file |
| --- | --- |
| Windows | `%LOCALAPPDATA%\AHV Remote Console\hcbridge.log` |
| Linux | `$XDG_DATA_HOME/ahv-remote-console/hcbridge.log`, by default `~/.local/share/ahv-remote-console/hcbridge.log` |
| Others | No log file |

The file is appended across restarts, so a restart keeps the previous run's log.
Every 5 minutes the agent checks the size. Above 5 MiB it truncates the file in place.
The clipboard is never logged in clear text, only its length, direction and SHA-256.

The environment variable `HC_BRIDGE_LOG_APPEND` is still read but matches the default,
append, so it changes nothing.

## Elevation

| Needs elevation | Modes |
| --- | --- |
| No | No flag, `--daemon`, `--install`, `--uninstall`, `--setup`, `--uplink-probe`, `--version` |
| Yes | `--apply-display`: Administrator or SYSTEM on Windows, root on Linux |

The agent is per user by design, so installing and running it never needs
administrator rights.

On Windows, the graphical installer runs the elevated step by relaunching itself with
the UAC `runas` verb (`runElevated`). It does this only when the display is not
already at the standard resolution.

> [!NOTE]
> The UAC dialog names this binary, "AHV Remote Console Guest Agent", not a display
> tool. The installer window, or the message box when there is no window, states
> before the prompt that it writes the firmware boot resolution. If the operator declines
> the prompt, or the variable store is read-only, setup continues and the daemon sets
> the running display anyway.

## Windows and Linux compared

| Aspect | Windows | Linux |
| --- | --- | --- |
| No-flag behaviour | WebView2 graphical installer, headless fallback | Runs the daemon |
| Interactive install | Graphical installer; no `--setup` | `--setup` with zenity, kdialog or a TTY |
| Autostart | `HKCU\...\Run` value `AHV Remote Console Guest Agent` | systemd `--user` unit `ahv-remote-console-agent.service` |
| Install location | `%LOCALAPPDATA%\AHV Remote Console\ahv-remote-console-agent.exe` | `~/.local/bin/ahv-remote-console-agent` |
| Clipboard serial port | `COM3` | `/dev/ttyS2` |
| Single-instance guard | Mutex `Local\AHVRemoteConsoleBridge-<SID>` | `flock` on `$XDG_RUNTIME_DIR/hcbridge.lock` |
| Default resolution | `1920x1080` | `max` |
| Live resolution change | `ChangeDisplaySettingsExW`, may need a reboot | xrandr or gdbus, live |
| `--apply-display` | `SetFirmwareEnvironmentVariableExW`, elevated | efivarfs, root |
| `--uplink-probe` | Runs | Returns an error |
| Log file | `%LOCALAPPDATA%\AHV Remote Console\hcbridge.log` | `~/.local/share/ahv-remote-console/hcbridge.log` |
| CLI output | Binary is GUI-subsystem; `attachParentConsole` reattaches to the calling shell | Console-subsystem; nothing to do |

On Windows the binary is linked as a GUI-subsystem program (`-H windowsgui`), so the
autostarted daemon never flashes a console window. Without help, CLI output such as
`--version` or a flag error would disappear when run from `cmd.exe`. The agent calls
`attachParentConsole()` at startup, before parsing flags, to point stdout and stderr
at the parent console. When there is no parent console, or off Windows, the call does
nothing.

## Known limitations

- The code currently sets the base log level to `debug` by default while handshake and
  clipboard problems are diagnosed (marked `DIAG_TODO` in `buildLogger`). An explicit
  `--log-level` still selects `debug`, `warn` or `error` as expected.
- The legacy name `hcbridge` remains in the single-instance guard names, the lock file
  name and the log file name `hcbridge.log`. They are kept so an in-place upgrade can
  find and stop a running legacy daemon.
- On platforms other than Windows and Linux, install, uninstall and the daemon's
  platform features are stubs that return errors or do nothing. This keeps the agent
  buildable on developer machines such as macOS.

## Related documentation

- [Agent architecture](../architecture/agent.md)
- [Connecting and using the clipboard](../operations/connect-and-clipboard.md)
- [Support matrix](../operations/support-matrix.md)
- [Building](../development/build.md)
- [Glossary](glossary.md)
