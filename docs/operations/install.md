# Installing and building

This page shows you how to install the packaged desktop app, build it from source,
and produce signed release installers. Operators need only the first section.
Engineers who build or ship the app should read on.

The desktop app is the only thing you install on your workstation. It bundles the Go
runtime and starts it for you. The agent is a separate, optional install inside each
VM. See [Connecting to a VM](connect-and-clipboard.md).

## Install the packaged app

The application is called **AHV Remote Console**. Installers are named
`ahv-remote-console-<version>-<os>-<arch>.<ext>` and are attached to GitHub Releases.

| Platform | Installer           | Architectures |
| -------- | ------------------- | ------------- |
| Windows  | NSIS `.exe`         | x64, ia32     |
| macOS    | `.dmg`              | arm64, x64    |
| Linux    | `.AppImage`, `.deb` | x64, arm64    |

### Windows

1. Download the `.exe` installer for your architecture.
2. Run it. The installer is not one-click: you can choose the install directory.
3. Keep the default per-user install, or opt into a machine-wide install. A
   machine-wide install asks for elevation.

There is no MSI package.

### macOS

1. Download the `.dmg` that matches your Mac. Use the arm64 build on Apple Silicon.
   The x64 build under Rosetta slows the whole app, including the bundled runtime.
2. Open the `.dmg` and install the app.

### Linux

1. Download the `.AppImage` or the `.deb`.
2. Install the `.deb` with your package manager, or make the AppImage executable and
   run it:

   ```bash
   sudo apt install ./ahv-remote-console-<version>-linux-<arch>.deb
   ```

   ```bash
   chmod +x ahv-remote-console-<version>-linux-<arch>.AppImage
   ./ahv-remote-console-<version>-linux-<arch>.AppImage
   ```

The `.deb` depends on GTK3, libnotify, NSS, libXScrnSaver, libXtst and xdg-utils.

There is no RPM package. On RHEL or Fedora, run the AppImage or repack the `.deb`.

### After installing

- The installer registers the `ahv-remote-console://` URL scheme. The main process
  forwards URLs on it to the renderer. No sign-in flow uses it. Prism sign-in runs
  in the SSO window.
- Clipboard sync needs the agent inside each VM. The agent ISO ships inside the app
  and you mount it from the console toolbar. See
  [Connecting to a VM](connect-and-clipboard.md#install-the-agent).
- In-app auto-update is off by default. To turn it on, see
  [Auto-update feed](#auto-update-feed).

### Auto-update feed

1. Host a generic update feed. It serves the platform manifest (`latest.yml`,
   `latest-mac.yml` or `latest-linux.yml`) and the artifact it references.
2. Set `AHV_REMOTE_CONSOLE_AUTOUPDATE=1` and `AHV_REMOTE_CONSOLE_UPDATE_URL` in the
   app's environment.
3. On Linux, also set `AHV_REMOTE_CONSOLE_UPDATE_PUBKEY_B64`. The update is refused
   unless `latest-linux.yml.minisig` verifies against this key.
4. If the feed uses a private CA, place the CA bundle at
   `certs/update-ca-bundle.pem` in the app's resources directory. For a source
   build, use `scripts/certs/` at the repository root. The updater sets
   `NODE_EXTRA_CA_CERTS` to it before it runs.

> [!IMPORTANT]
> An update installs only when the feed's version is semver-greater than the
> installed one. Do not add a prerelease suffix to release versions. Dev, debug and
> unpackaged builds skip the updater.

The variables are listed in [Configuring endpoints](configure.md#auto-update-feed-optional).

Source: [`apps/desktop/electron-builder.yml`](../../apps/desktop/electron-builder.yml),
[`apps/desktop/src/main/autoUpdate.ts`](../../apps/desktop/src/main/autoUpdate.ts).

## Build from source

### Prerequisites

| Tool | Version                                |
| ---- | -------------------------------------- |
| Go   | 1.25, toolchain pinned to `go1.25.12`  |
| Node | 24 or later                            |
| pnpm | 11 or later (`pnpm@11.1.3`)            |
| bash | Any real bash on `PATH`                |

- The pinned Go toolchain carries standard-library patches for two CVEs in
  `crypto/x509` and `net/textproto` that `govulncheck` flags in CI. With
  `GOTOOLCHAIN=auto`, Go fetches that toolchain if yours is older. A newer local
  toolchain is used as-is.
- The Makefile recipes use bash-only syntax. On Windows, bash comes from `PATH`.
  Git Bash, WSL and MSYS2 all work.
- The project is one Go module and one pnpm workspace. The root `Makefile` drives Go,
  JavaScript, the agent ISO, packaging and signing.

### Steps

1. Clone the repository and install dependencies:

   ```bash
   git clone https://github.com/MarcoCarvin/ahv-remote-console.git
   cd ahv-remote-console
   make install
   ```

   `make install` runs `pnpm install --frozen-lockfile`, falling back to a
   non-frozen install, and `go mod download` for `apps/runtime` and `plugins/ahv`.

2. Run the app in dev mode:

   ```bash
   make dev
   ```

   This regenerates icon assets, builds the host-arch runtime and AHV plugin, builds
   the agent ISO with `make iso`, then starts Vite and Electron with
   `AHV_REMOTE_CONSOLE_DEV=1`. The runtime finds the ISO at `build/agent.iso` through
   `AHV_REMOTE_CONSOLE_AGENT_ISO_PATH`. Only the renderer hot-reloads. Restart
   `make dev` after changes to the main process or the preload.

3. Build for the host platform:

   ```bash
   make build
   ```

   `make build` runs these targets, which you can also run one at a time:

   | Target         | Builds                                                       |
   | -------------- | ------------------------------------------------------------ |
   | `make runtime` | The Go runtime                                               |
   | `make plugins` | The AHV plugin, statically linked into the runtime binary    |
   | `make desktop` | The desktop app and workspace packages (`pnpm --filter '@ahv-remote-console/*' build`) |

4. Build the agent and its ISO:

   ```bash
   make agent-bins
   make iso
   ```

   `make iso` always rebuilds the agent binaries and packages first, so it never
   ships a stale artifact. The agent Makefile builds `linux/amd64`, `windows/amd64`
   and `windows/arm64`. Only `linux/amd64` and `windows/amd64` go into the ISO,
   because AHV guests are x86-64. The ISO also carries `.rpm` and `.deb` packages of
   the Linux agent, built with the pure-Go `cmd/agentpkg` packager. No rpm or dpkg
   tools are needed on the host.

5. Run the quality gates:

   ```bash
   make test
   make lint
   make typecheck
   ```

   | Command          | Runs                                                                      |
   | ---------------- | ------------------------------------------------------------------------- |
   | `make test`      | `go test -race -cover ./...` over the whole module, then `pnpm -r test` (vitest) |
   | `make lint`      | The runtime and plugin Go linters, then `pnpm lint` (`pnpm -r lint`)      |
   | `make typecheck` | `pnpm -r typecheck` (`tsc --noEmit`)                                      |
   | `make ci`        | `install lint typecheck test build`, the local equivalent of CI           |

To run a single test:

```bash
go test -race -run TestName ./apps/runtime/internal/proxy/...
pnpm --filter @ahv-remote-console/desktop test <file-or-pattern>
```

Add `-t "name"` to the pnpm command for a single case.

> [!IMPORTANT]
> Build the workspace packages before type-aware lint or the desktop tests. Both
> resolve `@ahv-remote-console/*` imports from each package's built `dist/`.
> Without it, ESLint reports false `no-unsafe-*` errors and vitest cannot resolve
> the SDK entry.
>
> ```bash
> pnpm -r --filter './packages/**' build
> ```

### CI workflows

CI runs on GitHub Actions in `.github/workflows/`.

| Workflow       | Trigger             | Jobs                                                                                   |
| -------------- | ------------------- | -------------------------------------------------------------------------------------- |
| `build.yml`    | Push, pull request  | `lint`, `notices-drift` (stale `THIRD_PARTY_NOTICES.md`), `test` on Ubuntu and Windows, `build-runtime`, `desktop` on Ubuntu and Windows |
| `security.yml` | Push, pull request  | `gosec` (HIGH and above), `govulncheck` over `apps/runtime` and `plugins/ahv`; `npm-audit` (`pnpm audit --prod --audit-level=high`). The workflow comments say HIGH findings block merges. |
| `release.yml`  | `v*.*.*` tags       | Builds the runtime and installers, signs the Windows installer, publishes a GitHub Release with the matching `CHANGELOG.md` section |

There is no end-to-end job and no macOS job.

## Package release installers

1. Run the packaging target for your platform:

   ```bash
   make package
   make package-win
   make package-linux
   make prod
   ```

   | Target               | Produces                                                  |
   | -------------------- | --------------------------------------------------------- |
   | `make package`       | The installer for the current platform                    |
   | `make package-win`   | Windows NSIS, x64 and ia32                                 |
   | `make package-linux` | Linux AppImage and `.deb`                                  |
   | `make prod`          | The full pipeline: dependencies, build, ISO, stage, package |

2. Collect the output from `apps/desktop/release/`. `make prod` lists that directory
   when it finishes.

`make package` runs `build` and `stage-runtime`, then electron-builder through
`pnpm --filter @ahv-remote-console/desktop package`. `stage-runtime` copies the Go
runtime to `build/runtime/<os>-<arch>/` and renames it to
`ahv-remote-console-runtime`, the exact name the main process starts. On a macOS
host it builds both arm64 and x64. electron-builder picks these up through
`extraResources`.

| Artifact                          | Location                     |
| --------------------------------- | ---------------------------- |
| Installers                        | `apps/desktop/release/`      |
| Staged Go runtime                 | `build/runtime/<os>-<arch>/` |
| Agent ISO                         | `build/agent.iso`            |
| Staged agent binaries and packages | `build/agent/`              |
| Icon assets                       | `build/icons/`               |

### Electron hardening at pack time

The `afterPack` hook in
[`apps/desktop/scripts/after-pack.js`](../../apps/desktop/scripts/after-pack.js)
sets Electron Fuses on all platforms before signing, so the signature covers them:

- Disabled: `RunAsNode`, Node CLI inspect arguments, and the `NODE_OPTIONS`
  variable. This stops a relaunch with `ELECTRON_RUN_AS_NODE` or `--inspect` from
  running arbitrary Node under the app's signature.
- Enabled: `OnlyLoadAppFromAsar` and cookie encryption.

On macOS the hook also removes the `com.apple.quarantine` attribute, makes the
bundled runtime executable, and runs `codesign --verify` on it. On unsigned dev
builds a failed verify is a warning.

## Signing

Sign everything you distribute: the desktop installer and the guest agent. Unsigned
builds trigger SmartScreen on Windows and Gatekeeper on macOS, and EDR or antivirus
products can block them. The guest agent is the most likely to be flagged, because
it starts at login and uses the clipboard and a serial port. See
[Guest agent](guest-agent.md#code-signing-and-endpoint-security).

### macOS

> [!WARNING]
> The repository has no Developer ID certificate. Local macOS builds are ad-hoc
> signed and cannot be shipped.

To ship a signed, notarized build:

1. Install a **Developer ID Application** certificate in the keychain, or set
   `CSC_LINK` or `CSC_NAME`.
2. Set the App Store Connect API credentials `APPLE_API_KEY`, `APPLE_API_KEY_ID` and
   `APPLE_API_ISSUER`. Without them, notarization (`afterSign:
   ./scripts/notarize.js`) does nothing.
3. Run the release gate:

   ```bash
   make prod-mac
   ```

   `verify-mac-release` refuses any artifact that is ad-hoc signed, lacks the
   hardened runtime, lacks a 10-character Team ID, or still carries the dev
   `disable-library-validation` entitlement.

`make package-mac` refuses to build without a real identity. An ad-hoc app with the
production hardened-runtime entitlements aborts at launch under macOS library
validation, because the Electron Framework needs a Team ID.

For a local, unsigned test build, run:

```bash
make package-mac-dev
```

This sets `CSC_IDENTITY_AUTO_DISCOVERY=false`, uses the dev entitlements
(`disable-library-validation`), builds the host architecture only, and names the
artifact `…-DEV-UNSIGNED-…`. The `afterPack` hook re-applies an ad-hoc signature so
it launches on Apple Silicon. Use it for local testing only.

The GitHub workflows have no macOS job, so CI does not supply a signing identity.

### Windows

The release workflow signs the NSIS installers with `CSC_LINK` (base64 PFX) and
`CSC_KEY_PASSWORD`. These come from the `WIN_CERT_BASE64` and `WIN_CERT_PASSWORD`
repository secrets. Local builds are unsigned.

The agent's Windows binaries are Authenticode-signed only when a certificate is
configured. Otherwise the build prints a SmartScreen warning and continues.

### Linux

The AppImage and `.deb` are not code-signed. Integrity comes from TLS to the
download location and, for auto-update, the signed `latest-linux.yml.minisig`
manifest.

Source: [`Makefile`](../../Makefile),
[`apps/desktop/electron-builder.yml`](../../apps/desktop/electron-builder.yml),
[`.github/workflows/release.yml`](../../.github/workflows/release.yml).

## Troubleshooting

For diagnostics bundles, console errors and agent problems, see
[Troubleshooting](troubleshooting.md).

## Related documentation

- [Architecture overview](../architecture/overview.md)
- [Desktop app and trust boundary](../architecture/desktop.md)
- [Runtime proxy](../architecture/runtime-proxy.md)
- [AHV plugin](../architecture/ahv-plugin.md)
- [Agent](../architecture/agent.md) and [agent CLI reference](../reference/agent-cli.md)
- [Live migration](../architecture/live-migration.md)
- [Protocol reference](../reference/protocol.md)
- [Update integrity](../architecture/update-integrity.md)
- [Project README](../../README.md)
