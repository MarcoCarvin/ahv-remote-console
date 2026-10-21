# Agent

The agent is an optional program that runs inside the AHV guest VM and adds clipboard sync and display sizing.

## Overview

The console works without the agent. You can connect, see the screen and type.
The agent adds two things the hypervisor side cannot do alone:

- Clipboard sync between your workstation and the guest. It uses a SPICE
  `qemu-vdagent` channel over an ISA serial port, below the guest network stack,
  so it keeps working under a full-tunnel VPN.
- Display sizing. It sets the guest to a standard resolution at boot and at run
  time, because the emulated VGA in AHV ignores remote resize requests.

It is one binary that behaves differently by flag and OS. It installs per user,
never machine-wide, so the clipboard stays scoped to your own login session.
Operators usually mount the agent ISO and double-click the binary on Windows, or
run the launcher or `--setup` on Linux. See
[`docs/architecture/overview.md`](../../docs/architecture/overview.md) for how
the agent fits with the desktop app and runtime.

## Usage

| Invocation | What it does |
| --- | --- |
| no flag | GUI installer on Windows; `--daemon` elsewhere. |
| `--daemon` | Resident agent with clipboard bridge and display pin. Autostart runs this. |
| `--install`, `--uninstall` | Add or remove the per-user autostart. |
| `--setup` | Linux dialog-driven install (zenity, kdialog or terminal). |
| `--apply-display` | Elevated one-shot. Writes the OVMF boot resolution and exits. |
| `--uplink-probe` | Windows-only one-shot uplink test, about 50 s. |
| `--version` | Print build info and exit. |

`--default-resolution=<WxH|max|none>` sets the resolution. The default is `1920x1080` on Windows, `max` on Linux and `none` elsewhere. `--log-level` takes `debug`, `info`, `warn` or `error`.

The default serial port is `COM3` on Windows and `/dev/ttyS2` on Linux. Only the Windows boot resolution change needs a UAC prompt. Resizing at run time does not.

The agent never logs clipboard text. It logs only length, direction and SHA-256, and wipes the buffers after use. The vdagent channel holds a QEMU live-migration blocker while a console is attached. See [live migration](../../docs/architecture/live-migration.md).

## Build and test

The agent is part of the single repository-wide Go module. Run these from the repository root:

```bash
make agent-bins     # cross-compile and stage binaries in build/agent/
make iso            # build build/agent.iso (runs agent-pkgs and agent-bins first)
make -C apps/ahv-remote-console-agent build GOOS=windows GOARCH=amd64
make -C apps/ahv-remote-console-agent all-platforms
make -C apps/ahv-remote-console-agent test
```

- Every binary is static (`CGO_ENABLED=0`) and built with `-trimpath`. The `all-platforms` matrix is `linux/amd64`, `windows/amd64` and `windows/arm64`.
- Windows binaries link as the GUI subsystem (`-H windowsgui`), so autostart opens no console window. Logs go to the agent log file.
- `all-platforms` signs Windows binaries with Authenticode when `CODESIGN_CERT_PATH` and `CODESIGN_CERT_PASS_FILE` are set. Without them it prints a warning and continues.
- `go generate ./cmd/ahv-remote-console-agent` regenerates the committed icon and version resource `rsrc_windows_amd64.syso`. Run it only when the icon or version changes.
- Windows and Linux files carry build tags. Cross-compile with the matching `GOOS` to build them on another host.

## Structure

| Path | Contents |
| --- | --- |
| `cmd/ahv-remote-console-agent/` | Flags, mode dispatch, daemon, logging, per-OS install, serial port and GUI. |
| `internal/vdagent/` | SPICE `qemu-vdagent` protocol with capabilities, clipboard and serial framing. |
| `internal/clipboard/` | Guest clipboard. Win32 on Windows; `wl-clipboard`, `xclip` or `xsel` on Linux. |
| `internal/display/` | List and apply display modes. |
| `internal/uefivar/` | Writes the OVMF `PlatformConfig` boot resolution. |
| `internal/keepwarm/` | Windows-only frame-rate floor against QEMU idle backoff. No-op on other guests. |
| `internal/uplinkprobe/` | The `--uplink-probe` test. |
| `internal/build/` | Link-time `Version`, `Commit` and `Date`. |

## Related documentation

- [Agent architecture](../../docs/architecture/agent.md)
- [Agent CLI reference](../../docs/reference/agent-cli.md)
- [Clipboard](../../docs/architecture/clipboard.md)
- [Live migration](../../docs/architecture/live-migration.md)
- [Architecture overview](../../docs/architecture/overview.md)
