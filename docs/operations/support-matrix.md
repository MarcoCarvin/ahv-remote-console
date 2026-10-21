# Support matrix

This page lists the platforms, build targets and Prism versions the repository
packages, builds or assumes. A build target is not a support promise unless the
documentation says so. Rows marked "not stated" have no statement in the repository.

## Desktop app packaging

| Platform | Architecture | Installer        | Status                       |
| -------- | ------------ | ---------------- | ---------------------------- |
| Windows  | x64          | NSIS `.exe`      | Packaged                     |
| Windows  | ia32         | NSIS `.exe`      | Packaged                     |
| Windows  | arm64        | None             | Not packaged                 |
| Windows  | Any          | MSI              | Dropped. WiX 4 under Wine fails. |
| macOS    | arm64        | `.dmg`           | Packaged. See [Signing](#signing). |
| macOS    | x64          | `.dmg`           | Packaged. See [Signing](#signing). |
| Linux    | x64          | AppImage, `.deb` | Packaged                     |
| Linux    | arm64        | AppImage, `.deb` | Packaged by config           |
| Linux    | Any          | `.rpm`           | Dropped. The space in the product name breaks the build. |

Artifact names follow `ahv-remote-console-${version}-${os}-${arch}.${ext}`.

### Signing

| Platform | Status                                                                     |
| -------- | -------------------------------------------------------------------------- |
| macOS    | `package-mac` requires Developer ID signing and refuses to build without it. Notarization runs only with App Store Connect credentials. |
| macOS    | The public release workflow has no macOS desktop job.                       |
| Windows  | Signing reads the `WIN_CERT_BASE64` and `WIN_CERT_PASSWORD` secrets.        |
| Linux    | No OS-level signature on the AppImage. The app checks a signed update manifest. See [Update integrity](../architecture/update-integrity.md). |

Source: [`apps/desktop/electron-builder.yml`](../../apps/desktop/electron-builder.yml),
[`Makefile`](../../Makefile),
[`.github/workflows/release.yml`](../../.github/workflows/release.yml).

## Release build targets

### Go runtime

| GOOS    | GOARCH | Runner                                      |
| ------- | ------ | ------------------------------------------- |
| linux   | amd64  | ubuntu-22.04                                |
| linux   | arm64  | ubuntu-22.04                                |
| windows | amd64  | ubuntu-22.04, cross-built, `CGO_ENABLED=0`  |
| darwin  | arm64  | ubuntu-22.04, cross-built, `CGO_ENABLED=0`  |
| darwin  | amd64  | Not in the matrix                           |
| windows | arm64  | Not in the matrix                           |

### Desktop packaging jobs

| Runner       | Runtime artifact used  |
| ------------ | ---------------------- |
| ubuntu-22.04 | runtime-linux-amd64    |
| windows-2022 | runtime-windows-amd64  |

- The desktop job uploads `.exe`, `.msi`, `.AppImage`, `.deb`, `.rpm` and
  `latest*.yml`.
- The release job attaches the Ubuntu and Windows outputs to the GitHub Release.
- No job packages the arm64 Linux desktop build or a macOS build. The arm64 Linux
  AppImage and `.deb` are configured but not built by the release workflow. The
  repository does not say whether this is a decision.

### Build toolchain

| Tool                       | Version                                                         |
| -------------------------- | --------------------------------------------------------------- |
| Node.js                    | `>=24.0.0` (engines), `24.15.0` (`.nvmrc`). CI reads `.nvmrc`.  |
| pnpm                       | `pnpm@11.1.3` (packageManager), engines `>=11.0.0`, CI `11.1.3` |
| Go                         | `go 1.26.0` in `go.mod`, `toolchain go1.26.10`, CI `1.26`       |
| Electron                   | `42.2.0`                                                        |
| electron-builder           | `26.8.1`                                                        |
| electron-updater           | `6.8.9`                                                         |
| TypeScript                 | `6.0.3`                                                         |
| Chromium inside Electron   | Not stated                                                      |
| Bash on the build host     | Required. On Windows, Git Bash, WSL or MSYS2.                   |

The native keyboard addon is a Node-API prebuilt binary, so the build needs no C or
C++ toolchain.

Source: [`.github/workflows/release.yml`](../../.github/workflows/release.yml),
[`package.json`](../../package.json),
[`apps/desktop/package.json`](../../apps/desktop/package.json),
[`go.mod`](../../go.mod).

## Agent

### Targets

| Item                                  | Value                                                |
| ------------------------------------- | ---------------------------------------------------- |
| Agent Makefile targets                | linux/amd64, windows/amd64, windows/arm64            |
| Targets staged into the ISO           | linux/amd64, windows/amd64                           |
| Windows arm64 agent                   | Built, not staged into the ISO                       |
| Guest architecture                    | "AHV guests are x86-64 only"                         |
| Windows binary subsystem              | GUI (`-H windowsgui`)                                |
| Static binaries                       | Yes, `CGO_ENABLED=0`                                 |
| Windows guest OS versions             | Not stated                                           |
| Linux guest distributions and versions | Not stated. The Linux packages are `.rpm` and `.deb`. |
| Guest CPU other than x86-64           | Not supported by the ISO                             |

### Install methods

| Guest OS      | Install method                                                     | Scope                     |
| ------------- | ------------------------------------------------------------------ | ------------------------- |
| Windows       | Double-click `ahv-remote-console-agent.exe` from the ISO. Graphical installer (WebView2). Falls back to a headless install without the WebView2 runtime, for example on older Server SKUs. | Per user (HKCU Run key) |
| Linux desktop | Install the `.rpm` or `.deb` from `linux/`, then run **AHV Remote Console Agent Setup** | Per user (systemd `--user`) |
| Linux headless | `sh <mount>/linux/install.sh`                                     | Per user                  |
| macOS         | None. `--install` returns an unsupported-platform error.           | Not applicable            |

The `.rpm` and `.deb` are built with the pure-Go packager.

### Features by guest OS

| Feature             | Windows                               | Linux                                                       | Other            |
| ------------------- | ------------------------------------- | ----------------------------------------------------------- | ---------------- |
| Clipboard sync      | Win32 `user32`                        | `xclip` or `xsel` (X11), `wl-copy` and `wl-paste` (Wayland). Without a helper, sync is off and the agent keeps running. | macOS unsupported |
| Display sizing      | `ChangeDisplaySettingsExW`, may reboot | xrandr, gdbus to Mutter, kscreen-doctor, wlr-randr. No reboot. | Not applicable |
| Keep-warm           | Supported                             | Stub, no-op                                                 | Stub, no-op      |
| UEFI variable change | Supported, needs elevation           | Not supported                                               | Not supported    |
| `--uplink-probe`    | Supported                             | Error (Windows only)                                        | Error            |

Source: [`apps/ahv-remote-console-agent/Makefile`](../../apps/ahv-remote-console-agent/Makefile),
[Agent](../architecture/agent.md), [agent CLI reference](../reference/agent-cli.md).

## Prism and AHV

| Item                                    | Value                                                     |
| --------------------------------------- | --------------------------------------------------------- |
| VM list, detail, power, images          | Prism Central v3 REST, `/api/nutanix/v3/...`              |
| Power actions and console token         | Prism Central v4 VMM, `/api/vmm/v4.0/ahv/config/vms/...`  |
| Task polling (v4)                       | `/api/prism/v4.0/config/tasks/{extId}`                    |
| v3-only Prism Central                   | A 404 on the v4 VMs root means v3-only. The app then uses v3 power calls. |
| Minimum Prism Central version           | Not stated                                                |
| Minimum Prism Element version           | Not stated                                                |
| Minimum AOS version                     | Not stated. The stats code notes that AOS versions vary.  |
| Prism Central version seen in code      | A comment records a synchronous upload observed on "pc.7.5+". This is an observation, not a minimum. |
| AHV version with native migration support | AHV 12.0, planned. The current mitigation stops being needed at that version. |
| AHV QEMU build                          | QEMU 9.1. `qemu-vdagent` blocks live migration while it exists. |

The AHV 12.0 statement is a Nutanix roadmap item, recorded in the documentation as
vendor-confirmed. The repository does not test against AHV 12.0.

Source: [AHV plugin](../architecture/ahv-plugin.md),
[Live migration](../architecture/live-migration.md),
[`plugins/ahv/internal/api/client.go`](../../plugins/ahv/internal/api/client.go).

## Desktop runtime

| Item                     | Value                                                         |
| ------------------------ | ------------------------------------------------------------- |
| Desktop shell            | Electron 42.2.0                                               |
| Browser engine           | Chromium, shipped with Electron. Version not stated.          |
| Windows agent GUI engine | WebView2                                                      |
| Default window           | 1440x900, minimum 960x600                                     |
| Linux dependencies (deb) | libgtk-3-0, libnotify4, libnss3, libxss1, libxtst6, xdg-utils |
| URL scheme               | `ahv-remote-console://`                                       |

## Known limits

| Combination                                    | Limit                                                      |
| ---------------------------------------------- | ---------------------------------------------------------- |
| Desktop on RHEL or Fedora                      | No RPM. Repack the `.deb` or use the AppImage. Whether that counts as support is not stated. |
| MSI installer                                  | Dropped                                                    |
| Windows arm64 guest                            | Agent built, not shipped in the ISO                        |
| macOS guest                                    | No clipboard, keep-warm is a no-op, no agent install       |
| Linux guest without a clipboard helper         | Clipboard sync off, agent keeps running                    |
| Linux guest display change                     | Applied live, no reboot. Reboot scheduling is Windows only. |
| Windows guest without WebView2                 | Headless install, no GUI                                   |
| `--install` on other operating systems         | Unsupported                                                |
| Live migration with the clipboard channel active | `qemu-vdagent` blocks migration. The app parks the channel on a socket while idle. |
| Auto-update on macOS or Windows builds that are not code-signed | The update library refuses the update. The flag is off by default. |
| Build host without bash                        | Not supported                                              |
| Prerelease versions on the update feed         | Not offered by the updater                                 |

## Not stated in the repository

The repository has no statement and no test for:

- Minimum Prism Central, Prism Element or AOS version.
- Chromium version inside Electron 42.2.0.
- Windows guest OS versions and Linux guest distributions.
- Windows, macOS or Linux desktop OS versions, minimum or maximum.
- Whether arm64 Linux desktop builds are tested in CI.
- Whether macOS guests will be supported.
- The desktop OS version floor for the Electron build.

## Related documentation

- [Installing and building](install.md)
- [Connecting to a VM](connect-and-clipboard.md)
- [Agent](../architecture/agent.md)
- [Agent CLI reference](../reference/agent-cli.md)
- [Release process](../development/release.md)
- [Update integrity](../architecture/update-integrity.md)
