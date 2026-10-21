# Building from source

This page shows how to build, run and package AHV Remote Console from source. It is
for developers working on the code. To install and run a released build, see
[Installing](../operations/install.md) and the [project README](../../README.md).

For how the parts fit together at runtime, read the
[architecture overview](../architecture/overview.md) first.

## Quick reference

| Task | Command |
| --- | --- |
| Install dependencies | `make install` |
| Run the app in dev mode | `make dev` |
| Build everything for this host | `make build` |
| Build the runtime only | `make runtime` |
| Build the desktop app only | `make desktop` |
| Build the agent ISO | `make iso` |
| Run all tests | `make test` |
| Lint Go and TypeScript | `make lint` |
| Typecheck TypeScript | `make typecheck` |
| Build an installer for this host | `make package` |
| Run the local CI sequence | `make ci` |
| List annotated targets | `make help` |

The root [`Makefile`](../../Makefile) is the entry point. Most targets call a
per-component Makefile or a pnpm workspace command.

## Prerequisites

| Tool | Version | Source |
| --- | --- | --- |
| Go | 1.25.0 or later; toolchain pinned to `go1.25.12` | `go.mod` |
| Node.js | 24.0.0 or later; `24.15.0` recommended | `package.json`, `.nvmrc` |
| pnpm | `11.1.3` (minimum 11.0.0) | `package.json` (`packageManager`) |
| bash | any | required by every Makefile |
| git | any | version stamps |

Notes:

- **Go.** The `toolchain go1.25.12` line is a floor. With the default
  `GOTOOLCHAIN=auto`, an older 1.25.x fetches `go1.25.12`. A newer toolchain, such as
  1.26.x, is used as is. The pinned patch release contains standard library fixes for
  GO-2026-5037 (`crypto/x509`) and GO-2026-5039 (`net/textproto`), which `govulncheck`
  would otherwise report.
- **pnpm.** Use the exact pinned version. pnpm 11 sandboxes dependency build scripts
  and quarantines very recent releases. The repository configures both in
  `pnpm-workspace.yaml`.
- **bash.** Recipes use bash-only syntax. On Windows, `make` looks for a bare `bash`
  on `PATH`. Git Bash, WSL and MSYS2 all provide one.
- **git.** `git describe` and `git log` supply the version, commit and date that are
  stamped into binaries. A checkout without git history stamps `0.0.0-dev` and
  `unknown`.

## Setup

1. Clone the repository.
2. Install the Node.js version from `.nvmrc` and pnpm 11.1.3.
3. Install the JavaScript and Go dependencies:

   ```bash
   make install
   ```

   This runs `pnpm install --frozen-lockfile`, falling back to `pnpm install` if the
   lockfile is out of date. It then runs `go mod download`.

4. Build the TypeScript libraries once, so lint, typecheck and tests can resolve them:

   ```bash
   pnpm -r --filter './packages/**' build
   ```

## Repository layout

The repository has one Go module and one pnpm workspace.

### Go module

There is a single `go.mod` at the root. The runtime, the AHV
plugin, the agent and the `cmd/*` tools (`isobuilder`, `agentpkg`) all belong to it.
`go test ./...` from the root covers all of them.

The per-component Makefiles (`apps/runtime/Makefile`, `plugins/ahv/Makefile`,
`apps/ahv-remote-console-agent/Makefile`) set `GOOS`, `GOARCH` and linker flags, then
call `go build` or `go test` on the same module. They do not define separate modules.

The AHV plugin is linked statically into the runtime binary. There is no `.so` or
`.dll`. A blank import in
[`apps/runtime/internal/plugins/registration.go`](../../apps/runtime/internal/plugins/registration.go)
pulls it in, so building the runtime builds the plugin.

### pnpm workspace

`pnpm-workspace.yaml` lists `apps/desktop` and `packages/*`:

- `apps/desktop` is the Electron desktop app (`@ahv-remote-console/desktop`).
- `packages/*` holds the libraries it uses: `native-keyhook`, `protocol`, `sdk`,
  `telemetry` and `ui`. The desktop app depends on them through `workspace:*`.

Workspace settings: `nodeLinker: isolated`, `preferWorkspacePackages: true`, an
`allowBuilds` list of packages allowed to run build scripts, and a
`minimumReleaseAgeExclude` list for pinned versions newer than pnpm's quarantine.

The root `package.json` has scripts that mirror many `make` targets, such as
`pnpm build:all`, `pnpm test:all` and `pnpm lint:go`.

## The dev loop

```bash
make dev
```

`make dev` regenerates the icons, then runs [`scripts/dev.mjs`](../../scripts/dev.mjs).
That script:

1. Builds the runtime and the AHV plugin for the host.
2. Runs `make iso` and passes `build/agent.iso` to the desktop app through
   `AHV_REMOTE_CONSOLE_AGENT_ISO_PATH`. It stops with an error if the ISO is missing.
3. Starts the desktop dev runner, `pnpm --filter @ahv-remote-console/desktop dev`.

The desktop dev runner, [`apps/desktop/scripts/dev.mjs`](../../apps/desktop/scripts/dev.mjs):

1. Builds the preload and main bundles once. They are not watched.
2. Starts the Vite dev server for the renderer and waits for
   `http://localhost:5173/` to respond.
3. Launches Electron with `AHV_REMOTE_CONSOLE_DEV=1`. The main process starts the
   runtime.

> [!NOTE]
> Only the React renderer hot-reloads. After you change main-process or preload
> code, restart `make dev`.

On macOS the dev runner also relabels the local Electron bundle so the Dock shows the
product name.

## Building each component

```bash
make build      # runtime + plugins + desktop for the host
make runtime    # Go runtime
make plugins    # prints a notice; the plugin is linked into the runtime
make desktop    # TypeScript libraries and desktop app
```

| Target | What it does |
| --- | --- |
| `runtime` | Builds `./cmd/ahv-remote-console-runtime` with `CGO_ENABLED=0` and `-trimpath`. Writes `apps/runtime/bin/ahv-remote-console-runtime-<goos>-<goarch>` and copies it to `apps/runtime/bin/ahv-remote-console-runtime`, the file dev mode starts. |
| `plugins` | Calls `plugins/ahv/Makefile`, which only prints that the plugin is in-tree. |
| `desktop` | Runs `pnpm --filter '@ahv-remote-console/*' build`. This builds every workspace library and the desktop app. The desktop build runs three production Vite builds: renderer, main and preload. |

The runtime is a static binary with no libc dependency. This lets a macOS host
cross-build both macOS architectures for packaging.

## Building the agent ISO

The optional agent ships as a hybrid ISO9660 and Joliet image, `build/agent.iso`. You
upload it to the Prism Image Service, and the runtime attaches it to a VM as a CD-ROM.

```bash
make iso
```

`make iso` depends on `agent-pkgs`, which depends on `agent-bins`. Running `make iso`
alone always rebuilds the binaries and packages first.

| Target | What it does |
| --- | --- |
| `agent-bins` | Runs `make -C apps/ahv-remote-console-agent all-platforms`, then copies the Linux amd64 and Windows amd64 binaries to `build/agent/`. |
| `agent-pkgs` | Wraps the Linux binary as `.rpm` and `.deb` with `cmd/agentpkg`. This uses nfpm in pure Go, so you do not need `rpm` or `dpkg`. |
| `iso` | Assembles `build/agent.iso` with `cmd/isobuilder` and prints its size and SHA-256. |

Details:

- The agent build matrix is `linux/amd64`, `windows/amd64` and `windows/arm64`. All are
  static (`CGO_ENABLED=0`). `agent-bins` stages only the two amd64 binaries, so the
  `windows/arm64` build is not in the ISO.
- Windows agent binaries link with `-H windowsgui`, so no console window opens in the
  guest when the agent starts.
- `agent-pkgs` needs `build/icons/256x256.png`. If it is missing, `pnpm build:icons`
  creates it.
- Package and ISO timestamps come from the HEAD commit date, not the clock. The same
  commit produces a byte-identical ISO. This matters because the image catalog
  identifies the ISO by the SHA-256 in its name.
- Override the output paths with `AGENT_BIN_DIR` (default `build/agent`) and
  `AGENT_ISO` (default `build/agent.iso`).

For agent behaviour and its command line, see [Agent](../architecture/agent.md) and
the [agent CLI reference](../reference/agent-cli.md).

## Packaging

```bash
make package          # installer for the current platform
make package-win      # Windows (NSIS)
make package-linux    # Linux (AppImage and deb)
make package-mac      # macOS, requires a signing identity
make package-mac-dev  # macOS, unsigned local build
```

`make package` runs `build`, then `stage-runtime`, then electron-builder.

`stage-runtime` builds the runtime and copies it to `build/runtime/<os>-<arch>/`,
where electron-builder picks it up. It renames the file to
`ahv-remote-console-runtime`, the exact name the packaged app starts. It clears the
destination directory first, so no older binary is packaged. On macOS it builds both
`arm64` and `x64`, because the macOS target packages both. It builds the host
architecture last, so `apps/runtime/bin/ahv-remote-console-runtime` stays usable by
dev mode.

For a full production build, use `make prod`, or `prod-mac`, `prod-win` or
`prod-linux`. These run `install`, `build`, `agent-bins`, `iso`, `stage-runtime`, the
icon build and electron-builder in order. Installers land in `apps/desktop/release/`.

`make prod-win-local` builds a Windows NSIS installer for x64 only. It works with
Wine on macOS, which is 64-bit only. The full x64 and ia32 build needs a host with
32-bit Wine.

> [!IMPORTANT]
> `make package-mac` and `make prod-mac` stop unless a Developer ID Application
> certificate is in the keychain, or `CSC_LINK` or `CSC_NAME` is set. An ad-hoc build
> with the production entitlements aborts at launch. For local work, use
> `make package-mac-dev`, which builds an unsigned app named `DEV-UNSIGNED` for the
> host architecture.

Signing, notarization, the macOS release gate and the release workflow are described
in [Releasing](release.md).

## Common problems

| Symptom | Cause | Fix |
| --- | --- | --- |
| ESLint reports many `no-unsafe-*` warnings | Workspace libraries are not built, so imports resolve to `any` | Run `pnpm -r --filter './packages/**' build` |
| Vitest fails with "Failed to resolve entry for package" | Same as above. Library `dist/` folders are gitignored | Same as above |
| `make` on Windows fails with "the system cannot find the path specified" | No `bash` on `PATH` | Install Git Bash, WSL or MSYS2 |
| Binaries report version `0.0.0-dev` and commit `unknown` | No git history in the checkout | Build from a full clone |
| `make dev` stops with an ISO error | `make iso` did not produce `build/agent.iso` | Run `make iso` and read its output |
| Main-process change has no effect in dev mode | Only the renderer hot-reloads | Restart `make dev` |
| `make package-mac` fails with "no 'Developer ID Application' identity" | No signing identity | Use `make package-mac-dev`, or see [Releasing](release.md) |

## Related documentation

- [Testing and CI](testing.md)
- [Releasing](release.md)
- [Architecture overview](../architecture/overview.md)
- [Desktop app](../architecture/desktop.md)
- [Contributing](../../CONTRIBUTING.md)
- [Documentation index](../README.md)
