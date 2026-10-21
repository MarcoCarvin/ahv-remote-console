# Scripts and tools

This page lists every script in `scripts/` and every command under `cmd/` that an
operator or developer runs. Each entry gives the purpose, who runs it, usage,
what it changes and its safety notes.

In examples, `<host>` is an AHV host, `<vm-uuid>` a libvirt domain UUID and
`<qmp-socket>` a QEMU monitor socket. Paths are relative to the repository root.

> [!WARNING]
> Entries marked **Changes live state: yes** alter a running VM or an AHV host.
> Each says whether a dry-run mode exists.

## Contents

- [Hypervisor host tools](#hypervisor-host-tools)
- [Developer tools](#developer-tools)
- [Build commands](#build-commands)
- [scripts/install and scripts/certs](#scriptsinstall-and-scriptscerts)
- [Unreferenced scripts and header mismatches](#unreferenced-scripts-and-header-mismatches)
- [Related documentation](#related-documentation)

## Hypervisor host tools

### scripts/ahv-libvirt-hook-qemu.py

The console-following libvirt hook and vdagent manager. COM3 is a plain unix
socket while idle and becomes a `qemu-vdagent` chardev with `clipboard=on` only
while a VNC console is attached. A vdagent chardev blocks live migration for its
lifetime. The hook handles libvirt events and starts a singleton `--manager`,
which does the swapping. The boot-time inject function,
`inject_vdagent_clipboard`, is still in the file but `main()` does not call it.

| Field              | Value                                                                  |
| ------------------ | ---------------------------------------------------------------------- |
| Runs it            | libvirt, as `/etc/libvirt/hooks/qemu`; the operator for `--manager-*`  |
| Prerequisites      | `/usr/bin/python3`, `virsh`, root                                      |
| Changes live state | Yes                                                                    |
| Dry-run            | Yes, until armed                                                       |

The Nutanix `ahv.libvirt_hook` package is optional. Without it the manager and
the selftest still run. Root is needed because the manager writes to
`/etc/libvirt/hooks`, `/var/run` and `/var/log/libvirt/qemu`.

**Usage**

```bash
/etc/libvirt/hooks/qemu <domain> <action> <subaction> <extra>   # called by libvirt, XML on stdin
python3 /etc/libvirt/hooks/qemu --manager                       # singleton manager
python3 scripts/ahv-libvirt-hook-qemu.py --manager-selftest     # off-host selftest, stubs virsh
python3 /etc/libvirt/hooks/qemu --manager-report                # read-only report per VM with COM3
```

**What it changes**

- `ensure_manager()` starts one manager with `setsid`, guarded by a lock on
  `/var/run/ahvrc-mgr.lock`.
- The manager runs `chardev-change` through `virsh qemu-monitor-command --hmp`.
  Attach sets `qemu-vdagent,clipboard=on`. Park restores
  `socket,path=<path>,server=on,wait=off` from the live XML, which clears the
  blocker.
- It logs to `/var/log/libvirt/qemu/hook.log` and `ahvrc-mgr.log`. The manager
  log is truncated at 10 MiB, not rotated.
- The marker file `/etc/libvirt/hooks/ahvrc-maintenance` parks every local VM,
  including VMs with a live console.

**Dry-run**

The manager is in dry-run mode unless `AHVRC_MGR_ARMED=1` is set or the flag file
`/etc/libvirt/hooks/ahvrc-mgr-armed` exists. Unarmed, it logs each decision as
`DRY-RUN would chardev-change ...` and swaps nothing.
`ahvrc-deploy-onehost.sh` creates the flag file, so a deploy through it arms the
manager.

**Safety**

- COM1, iobase `0x3F8`, belongs to Nutanix Guest Tools and is never targeted.
  COM3 is found by iobase `0x3E8` or port `2`, not by `charserial0`.
- Park happens when the last console closes, so an idle VM stays migratable.
- `--manager-report` is read-only and never swaps.
- The maintenance signal is not finalized. The marker file is the only input.

See [Setting up AHV hosts](hypervisor-setup.md).

### scripts/ahvrc-deploy-onehost.sh

Per-host, hook-only deploy. It validates the staged hook before touching the
host, backs up the original hook once, removes an earlier systemd unit, installs
and arms the hook, starts the manager once and verifies.

| Field              | Value                                                                  |
| ------------------ | ---------------------------------------------------------------------- |
| Runs it            | `deploy-vdagent-manager.sh`, on each host under sudo. Not run by hand  |
| Prerequisites      | `/tmp/ahvrc-newhook.py` staged; `systemctl`, `install`, `pkill`, `setsid`; root |
| Changes live state | Yes                                                                    |
| Dry-run            | No                                                                     |

**Usage**

```bash
sudo bash /tmp/ahvrc-deploy.sh <expected-md5-of-staged-hook>
```

**What it changes**

1. Parses the staged file and runs its `--manager-selftest`.
2. Compares its MD5 with the argument and aborts on a mismatch.
3. Copies the original hook to `qemu.ahvrc-orig`, only if that backup is absent.
4. Disables and removes the `ahvrc-vdagent-manager` unit.
5. Installs the hook to `/etc/libvirt/hooks/qemu` with mode 0755 and touches
   `ahvrc-mgr-armed`.
6. Kills the old manager, removes the lock and starts one new manager.
7. Deletes a fixed list of `/tmp` staging files.
8. Prints the hook MD5, backup and arm state, systemd state, manager PID and the
   first lines of `--manager-report`.

The original hook backup is kept. The `/tmp` deletion cannot be undone, but those
files are staging copies.

**Safety**

- The gates are syntax, selftest and MD5.
- The script has no `set -e`.

### scripts/deploy-vdagent-manager.sh

Deploys the manager to AHV hosts over SSH. It runs the hook's
`--manager-selftest` locally, then for each host copies the hook to
`/tmp/ahvrc-newhook.py` and the one-host script to `/tmp/ahvrc-deploy.sh`. It
then runs the one-host script with sudo and the local hook's MD5. The header
calls the run idempotent.

| Field              | Value                                                                  |
| ------------------ | ---------------------------------------------------------------------- |
| Runs it            | The operator, from a workstation with SSH access to the hosts          |
| Prerequisites      | `python3`, `md5sum`; SSH key; sudo password file; companion files      |
| Changes live state | Yes, on every listed host                                              |
| Dry-run            | No                                                                     |

It needs an SSH key for the `nutanix` user, `~/.ssh/nutanix` by default or the
path in `AHVRC_SSH_KEY`. It reads the sudo password from `/tmp/test.txt`, which
the header describes as specific to its own environment. The hook and the one-host
script must be in the same directory as this script.

**Usage**

```bash
./scripts/deploy-vdagent-manager.sh <host> [<host> ...]
```

With no host it prints usage and exits with code 2. `set -e` stops the run if
the local selftest fails.

**What it changes**

Everything the one-host script changes, on every listed host.

**Safety**

- The sudo password is written into the remote command string, so it shows in
  the remote process list while that command runs.
- sudo's stderr is discarded (`2>/dev/null`), so a failed sudo is not reported.
- The rollback steps in the header name the systemd unit, which the one-host
  script no longer installs. See [Roll back](hypervisor-setup.md#roll-back).

### scripts/ahv-clipboard-relay.py

Bridges the VNC clipboard and a serial socket for one domain. Text the operator
copies goes to the guest socket. Guest text goes back as ClientCutText.

| Field              | Value                                                                  |
| ------------------ | ---------------------------------------------------------------------- |
| Runs it            | The operator, on the AHV host, as a background process                |
| Prerequisites      | `virsh`; a serial socket for the domain; Python 3 standard library     |
| Changes live state | No domain config change. Writes clipboard text to guest and session    |
| Dry-run            | No                                                                     |

It calls `virsh qemu-monitor-command --hmp "info vnc"`. It looks for the socket
at `/var/lib/libvirt/qemu/<domain>.serial.2` or `.serial.0`. The file is not
executable.

**Usage**

```bash
python3 scripts/ahv-clipboard-relay.py <domain-uuid>
```

**What it changes**

- Opens a second VNC client and the serial socket.
- Writes clipboard text into the guest and into the operator's session.
- Appends to `/tmp/clipboard-relay.log` and never truncates it.

**Safety**

- The file has no safety notes.
- Guest messages over 1 MiB and VNC cut text over 10 MiB are dropped.

### scripts/direct-hmp-chardev-change.py

A test script. It sends HMP `chardev-change` straight to the QMP socket,
bypassing virsh and libvirtd. It tests whether the HMP path applies
`clipboard=on` when virsh is not involved.

| Field              | Value                                                                  |
| ------------------ | ---------------------------------------------------------------------- |
| Runs it            | A developer or operator, on the AHV host, as root                      |
| Prerequisites      | Python 3 standard library; access to the socket under `/var/lib/libvirt/qemu/` |
| Changes live state | Yes                                                                    |
| Dry-run            | No                                                                     |

The file is not executable.

**Usage**

```bash
python3 scripts/direct-hmp-chardev-change.py <qmp-socket> <chardev-id>
python3 scripts/direct-hmp-chardev-change.py <vm-uuid>     # finds the socket, uses charserial0
```

**What it changes**

- Sends HMP `chardev-change <id> qemu-vdagent,clipboard=on` to a running VM.
- Sends a QMP `chardev-change` with `clipboard: true`.
- Leaves the VM blocked from live migration while vdagent stays attached. It does
  not restore the socket backend.

**Safety**

> [!CAUTION]
> The one-argument form uses `charserial0`, which is not always COM3. Pass the
> chardev ID explicitly. Restore the socket backend yourself afterwards.

### scripts/ahv-clipboard-diagnostic.sh

A read-only checklist for whether transparent clipboard sharing is possible. It
reports the QEMU version, `qemu-vdagent` chardev support and vmport. For one VM it
also reports the command line, serial devices, QMP socket and libvirt XML.

| Field              | Value                                                                  |
| ------------------ | ---------------------------------------------------------------------- |
| Runs it            | The operator, on the AHV host or a CVM                                 |
| Prerequisites      | `bash`, `virsh`, `ps`, `rpm`, `lsmod`, `ip`; root                      |
| Changes live state | No. Its only VM call is `query-chardev`                                |
| Dry-run            | Not needed                                                             |

**Usage**

```bash
bash scripts/ahv-clipboard-diagnostic.sh [<vm-uuid>]
ssh root@<host> 'bash -s' < scripts/ahv-clipboard-diagnostic.sh
```

Without a UUID it lists running VMs and skips the VM-specific sections.

**What it changes**

Nothing.

**Safety**

- The header has no safety notes.
- When QEMU is not at the standard paths, section 1 runs `find /`, which can be
  slow.

### scripts/vm-migratable.sh

Reports whether one VM, or every running VM, can be live-migrated. It uses
QEMU's `query-migrate` `blocked-reasons` as ground truth.

| Field              | Value                                                                  |
| ------------------ | ---------------------------------------------------------------------- |
| Runs it            | The operator, on the AHV host, as root                                 |
| Prerequisites      | `virsh`, `python3`                                                     |
| Changes live state | No. It calls `query-migrate`, `query-chardev` and `query-vnc`          |
| Dry-run            | Not needed                                                             |

The file is executable. The header also gives a CVM form,
`hostssh "bash /tmp/vm-migratable.sh all"`. `hostssh` is not part of this
repository.

**Usage**

```bash
sudo ./scripts/vm-migratable.sh [<vm-uuid> | all]
```

The default is `all`. Output is one line per VM, `MIGRATABLE` or `BLOCKED`, with
the reason.

**What it changes**

Nothing.

**Safety**

Under the console-following manager, a VM is blocked exactly while a console is
attached.

## Developer tools

These Node tools run on a developer workstation. None of them touches a
hypervisor, and none has a dry-run mode.

### scripts/dev.mjs

Cross-platform dev runner. It builds the Go runtime and the AHV plugin, builds
the agent ISO, then starts the desktop dev runner with
`AHV_REMOTE_CONSOLE_AGENT_ISO_PATH` set to `build/agent.iso`.

| Field         | Value                                                     |
| ------------- | --------------------------------------------------------- |
| Runs it       | A developer, directly or through `make dev`               |
| Prerequisites | Node, `pnpm`, `make`, Go, installed workspace dependencies |
| Changes       | Local build artifacts and a local app process             |

**Usage**

```bash
node scripts/dev.mjs
```

**Safety**

It exits with an error if `build/agent.iso` is missing after `make iso`.

### scripts/build-icons.mjs

Generates every app icon from the brand spark. The icon is a flat two-colour
monitor with the spark cut out of the screen. Icons are full-bleed for Windows, Linux and web, and
padded to a squircle for macOS `.icns`.

| Field         | Value                                                         |
| ------------- | ------------------------------------------------------------- |
| Runs it       | A developer, through `pnpm build:icons`; Makefile `icons` and `prod*` targets |
| Prerequisites | Node with `sharp` and `png2icons`; `assets/brand/spark-white.png` |
| Changes       | Rewrites icon files under `assets/icon/` and `build/icons/`   |

`pnpm build:icons` runs `build-icon-sprite.mjs` next.

**Usage**

```bash
node scripts/build-icons.mjs
```

**Safety**

A failed `.icns` or `.ico` step sets exit code 1 and the script continues.

### scripts/build-icon-sprite.mjs

Combines the SVG files in `assets/icons/` into one `<svg>` sprite of `<symbol>`
elements, used with `<use href="#name"/>`.

| Field         | Value                                                  |
| ------------- | ------------------------------------------------------ |
| Runs it       | A developer, through `pnpm build:icons`, after `build-icons.mjs` |
| Changes       | Writes `apps/desktop/public/icons/sprite.svg`          |

**Usage**

```bash
node scripts/build-icon-sprite.mjs
```

### scripts/verify-icons.mjs

Checks that icon assets exist with the right dimensions. It covers PNG sizes 16 to
1024, the `.icns` and `.ico` bundles, the source SVG, the build bundles and six
1024 px modes.

| Field         | Value                                           |
| ------------- | ----------------------------------------------- |
| Runs it       | A developer or CI, through `pnpm verify:icons`  |
| Changes       | Nothing. Exits with code 1 on any failure       |

**Usage**

```bash
node scripts/verify-icons.mjs
```

### scripts/gen-third-party-notices.mjs

Generates `THIRD_PARTY_NOTICES.md`, which is not edited by hand. It covers the Go
runtime and the in-guest agent for `windows`, `linux` and `darwin`, production
desktop dependencies through `license-checker`, and the Electron runtime.
`cmd/isobuilder` is excluded.

| Field         | Value                                                           |
| ------------- | --------------------------------------------------------------- |
| Runs it       | A developer, through `pnpm notices:generate`                    |
| Prerequisites | `go`; `go-licenses` in `$(go env GOPATH)/bin`; network access to npm |
| Changes       | Rewrites `THIRD_PARTY_NOTICES.md` at the repository root        |

**Usage**

Run from the repository root:

```bash
node scripts/gen-third-party-notices.mjs
```

It creates and removes a temporary `gonotices-*` directory.

## Build commands

These Go commands write files only. They change no live state and have no
dry-run mode.

### cmd/isobuilder

Assembles the agent ISO, a hybrid ISO9660 and Joliet image that is uploaded to
the image service and attached to a guest as a CD-ROM. It is pure Go and does not
call `mkisofs`, `hdiutil` or `genisoimage`. It embeds `README.txt`, `autorun.inf`
and `install.sh` from `cmd/isobuilder/assets/`.

| Field         | Value                                                              |
| ------------- | ------------------------------------------------------------------ |
| Runs it       | CI and the developer build, through `make iso`                     |
| Prerequisites | `ahv-remote-console-agent-<os>-<arch>[.exe]` binaries; Linux `.rpm` and `.deb` packages |
| Changes       | Writes one ISO and prints its path, size and SHA-256               |

`make iso` depends on `agent-pkgs`, which depends on `agent-bins`.

**Usage**

```bash
go run ./cmd/isobuilder -agent-dir <dir> -out <file.iso> -version <version> [-label <prefix>] [-mtime <unix-seconds>]
```

`-agent-dir`, `-out` and `-version` are required. `-mtime` sets every timestamp
in the ISO, so identical inputs give an identical image. On failure the command
removes the partial ISO.

The ISO includes `cmd/isobuilder/assets/install.sh`. It installs the agent for the
current user only, for headless guests:

```bash
sh install.sh [none|WxH|max]
```

### cmd/agentpkg

Wraps the static Linux agent binary in `.rpm` and `.deb` packages, so a desktop
Linux guest can install it with the package manager. The packages install the
binary, a setup launcher and an icon. They add no machine-wide autostart. The
launcher, `cmd/agentpkg/assets/ahv-remote-console-agent-setup.desktop`, runs
`--setup`. Packages are built with `nfpm`, for x86-64 only.

| Field         | Value                                                              |
| ------------- | ------------------------------------------------------------------ |
| Runs it       | CI and the developer build, through `make agent-pkgs`              |
| Prerequisites | linux/amd64 agent binary; 256x256 PNG icon (default `build/icons/256x256.png`) |
| Changes       | Writes `ahv-remote-console-agent.rpm` and `.deb` into `-out`       |

**Usage**

```bash
go run ./cmd/agentpkg -bin <agent-binary> -icon <256.png> -out <dir> [-version <v>] [-mtime <unix-seconds>]
```

`-bin`, `-icon` and `-out` are required. `-version` defaults to `dev`. The
Makefile passes the commit date as `-mtime`.

## scripts/install and scripts/certs

- `scripts/install/` does not exist.
- `scripts/certs/` does not exist. Source builds look for
  `<repo>/scripts/certs/update-ca-bundle.pem`, in
  [`apps/desktop/src/main/autoUpdate.ts`](../../apps/desktop/src/main/autoUpdate.ts).
  Only a file the operator supplies satisfies it.

## Unreferenced scripts and header mismatches

### Scripts no other page names

No file in `docs/`, other than this page, and not the top-level `README.md`
names these scripts:

- `scripts/ahv-clipboard-relay.py`
- `scripts/direct-hmp-chardev-change.py`
- `scripts/vm-migratable.sh`
- `scripts/build-icons.mjs`
- `scripts/build-icon-sprite.mjs`
- `scripts/verify-icons.mjs`
- `scripts/gen-third-party-notices.mjs`

The icon scripts are named only in `package.json`. The notices script is named
only in `THIRD_PARTY_NOTICES.md` and `apps/desktop/electron-builder.yml`.
`build/icons/README.md`, outside `docs/`, names `scripts/build-icons.mjs`.

### Script paths in docs that do not resolve from the repository root

- `./scripts/notarize.js` in [install.md](install.md). The path is relative to
  `apps/desktop`, where `electron-builder.yml` lives. The file exists only as
  `apps/desktop/scripts/notarize.js`.
- `scripts/certs/` in [install.md](install.md) and
  [release.md](../development/release.md). The directory does not exist.

All other `scripts/` paths in `docs/` and `README.md` resolve.

### Header claims that do not match the code

- `scripts/ahv-clipboard-relay.py` says it is "Started by the libvirt hook". The
  hook source does not reference the relay.
- `scripts/deploy-vdagent-manager.sh` says it installs the systemd unit and has a
  default host range. The code runs a hook-only deploy, which removes the unit,
  and requires host arguments.

## Related documentation

- [Setting up AHV hosts](hypervisor-setup.md)
- [Troubleshooting](troubleshooting.md)
- [vdagent and live migration](../architecture/live-migration.md)
- [Clipboard](../architecture/clipboard.md)
- [Agent CLI reference](../reference/agent-cli.md)
- [Release process](../development/release.md)
