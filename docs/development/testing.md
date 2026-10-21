# Testing and CI

This page covers the tests, linters and type checks you run before you push, and the
GitHub Actions workflows that run on every change. It is for developers changing the
code. You do not need any of it to use the app.

For what each component does, see the [architecture overview](../architecture/overview.md).
The contracts that tests most often exercise are in the [protocol](../reference/protocol.md)
and [IPC](../reference/ipc.md) references.

## Quick reference

| Task | Command |
| --- | --- |
| All tests | `make test` |
| Go tests, whole module | `make test-go` |
| TypeScript tests | `make test-ts` |
| Lint Go and TypeScript | `make lint` |
| Typecheck TypeScript | `make typecheck` |
| Local CI sequence | `make ci` |

> [!IMPORTANT]
> On a fresh checkout, build the TypeScript libraries before you lint, typecheck or
> run TypeScript tests:
>
> ```bash
> pnpm -r --filter './packages/**' build
> ```
>
> Each library resolves to a gitignored `dist/` folder. Without it, Vitest fails with
> "Failed to resolve entry for package @ahv-remote-console/sdk", and type-aware ESLint
> treats every workspace import as `any` and reports many false `no-unsafe-*`
> warnings. `make build` and `make desktop` build the libraries for you.

## Running tests

`make test` runs `test-go`, then `test-ts`.

### Go tests

```bash
make test-go
```

This runs `go test -race -cover ./...` from the root. The repository has one Go
module, so this covers every Go package: the runtime (`apps/runtime`), the AHV plugin
(`plugins/ahv`), the agent (`apps/ahv-remote-console-agent`) and the `cmd/*` tools.
`-race` enables the race detector and `-cover` prints coverage per package.

The runtime and plugin Makefiles each have a `test` target with the same flags, scoped
to their own directory:

```bash
make -C apps/runtime test
make -C plugins/ahv test
```

> [!NOTE]
> The agent's Windows and Linux source files use build tags. A host build compiles
> and tests only the files for the host. To check the other platform paths, build
> with `GOOS=windows` or `GOOS=linux`. The platform-independent agent code, such as
> the suites under `apps/ahv-remote-console-agent/internal/vdagent/`, runs on any
> host. See [Agent](../architecture/agent.md).

### TypeScript tests

```bash
make test-ts
```

This runs `pnpm -r test`, which runs the `test` script in each workspace package that
has one. Two packages have tests:

| Package | Runner | Script |
| --- | --- | --- |
| `apps/desktop` | Vitest | `vitest run` |
| `packages/native-keyhook` | Node test runner | `node --test` |

The `protocol`, `sdk`, `telemetry` and `ui` packages have no `test` script.

Vitest finds `src/**/*.{test,spec}.{ts,tsx}` and skips the Playwright `e2e/` folder
(see [`apps/desktop/vitest.config.ts`](../../apps/desktop/vitest.config.ts)). The
suite covers the main process, for example `src/main/__tests__/signable.test.ts`, and
the renderer's RFB client under `src/renderer/rfb/`.

## Running a single test

Run one Go test by name, scoped to its package:

```bash
go test -race -run TestName ./apps/runtime/internal/proxy/...
```

`-run` takes a regular expression that matches `Test*` function names. You can drop
`-race` for a faster loop. Keep it for code that involves concurrency, such as the
proxy pumps, the session reaper or the agent serial loop.

Run one TypeScript test file:

```bash
pnpm --filter @ahv-remote-console/desktop test src/main/__tests__/signable.test.ts
```

Run one test case by its `describe` or `it` title:

```bash
pnpm --filter @ahv-remote-console/desktop test signable -t "rejects absolute http"
```

The `test` script runs once and exits. For watch mode, run `vitest` inside
`apps/desktop`.

## Lint

```bash
make lint
```

`make lint` lints Go and TypeScript:

1. `make -C apps/runtime lint` and `make -C plugins/ahv lint` each run `go vet ./...`,
   then `staticcheck ./...`. If `staticcheck` is not on `PATH`, the target installs it
   with `go install`.
2. `pnpm lint` runs `pnpm -r lint`, which runs ESLint in every workspace package.

ESLint warning limits:

| Package | Limit |
| --- | --- |
| `apps/desktop` | `--max-warnings 520` |
| `packages/protocol`, `sdk`, `telemetry`, `ui` | `--max-warnings 50` |

The shared configuration is [`eslint.config.mjs`](../../eslint.config.mjs). It uses
type-aware `recommendedTypeChecked` rules and ignores `apps/runtime/**`.

Other lint scripts:

| Command | What it does |
| --- | --- |
| `pnpm lint:go` | Runs the runtime and plugin Go lint targets |
| `pnpm lint:fix` | Runs ESLint with `--fix` in every package |
| `pnpm format:check` | Checks formatting with Prettier |
| `pnpm format` | Rewrites files with Prettier |

## Typecheck

```bash
make typecheck
```

This runs `pnpm typecheck`, which runs `tsc --noEmit` in every package that defines a
`typecheck` script: the desktop app and the libraries. It emits no files. Go has no
separate type-check step. The compiler and `go vet` cover it.

## Security scans

You can run the CI security scanners locally, with the same versions:

```bash
go install github.com/securego/gosec/v2/cmd/gosec@v2.22.7
go install golang.org/x/vuln/cmd/govulncheck@v1.1.4
gosec -severity=high ./...
govulncheck ./...
```

Mark a confirmed false positive inline with `#nosec` and a reason.

## CI workflows

CI runs on GitHub Actions. The workflows are in `.github/workflows/`.

| Workflow | Trigger | Jobs |
| --- | --- | --- |
| `build.yml` | Push to `main`, pull request to `main` | `lint`, `notices-drift`, `test`, `build-runtime`, `desktop` |
| `security.yml` | Push to `main`, pull request to `main`, weekly on Monday at 05:00 UTC | `codeql`, `gosec`, `govulncheck`, `npm-audit`, `sbom` |
| `secret-scan.yml` | Push to `main`, any pull request | `gitleaks` |
| `release.yml` | Push of a tag matching `v*.*.*` | `runtime`, `desktop`, `publish` |

### build.yml

| Job | Runs on | What it does |
| --- | --- | --- |
| `lint` | `ubuntu-22.04` | `pnpm lint` |
| `notices-drift` | `ubuntu-22.04` | Runs `pnpm notices:generate` and fails if `THIRD_PARTY_NOTICES.md` changes |
| `test` | `ubuntu-22.04`, `windows-2022` | `pnpm test:go`, then `pnpm test` |
| `build-runtime` | `ubuntu-22.04` | Builds the runtime for `linux/amd64`, `linux/arm64`, `windows/amd64` and `darwin/arm64` |
| `desktop` | `ubuntu-22.04`, `windows-2022` | `pnpm build`, then packages the desktop app unsigned |

> [!IMPORTANT]
> CI runs only the runtime and plugin Go tests. `pnpm test:go` runs
> `make -C apps/runtime test` and `make -C plugins/ahv test`. The agent packages and
> the `cmd/*` tools are not tested in CI. Run `make test-go` locally to cover them.

When you add or update a dependency, run `pnpm notices:generate` and commit the new
`THIRD_PARTY_NOTICES.md`, or `notices-drift` fails.

### security.yml

| Job | What it does |
| --- | --- |
| `codeql` | CodeQL analysis for JavaScript/TypeScript and Go |
| `gosec` | `gosec -severity=high` over `./apps/runtime/...` and `./plugins/ahv/...`. Fails on high-severity findings |
| `govulncheck` | `govulncheck ./...` in `apps/runtime` and in `plugins/ahv`. Fails on a reachable known vulnerability |
| `npm-audit` | `pnpm audit --prod --audit-level=high`. Fails on high-severity advisories |
| `sbom` | Uploads a CycloneDX SBOM as an artifact, kept for 90 days |

`gosec` does not scan the agent or the `cmd/*` tools.

### Shared settings

- Jobs install Go `1.25`, Node.js from `.nvmrc` and pnpm `11.1.3` as they need them.
  The `toolchain` line in `go.mod` then selects `go1.25.12` if the runner's Go is
  older.
- `gosec` (`v2.22.7`), `govulncheck` (`v1.1.4`) and `go-licenses` (`v1.6.0`) are
  pinned.
- `build.yml` cancels an older run on the same ref when a new one starts.
- Dependabot opens weekly update pull requests for GitHub Actions, Go modules and npm
  packages.

To require these checks before merge, set branch protection on `main`.

### What CI does not run

- `make typecheck`. No workflow has a typecheck job.
- `pnpm format:check`.
- The Playwright tests against a packaged app.
- A macOS desktop package.

## make ci and CI

`make ci` runs `install`, `lint`, `typecheck`, `test` and `build`. Use it before you
push. It differs from CI in three ways:

- It runs `typecheck`, which CI does not.
- It runs the whole Go module through `make test-go`, which covers more than CI.
- It does not run the security scans, packaging or Playwright tests.

## Testing a packaged app

Some changes need a packaged build to check, for example the asar layout,
`extraResources`, resource paths or Electron Fuses. Build an installer with
`make package`, or on macOS with `make package-mac-dev`. See
[Building from source](build.md#packaging).

To run the Playwright smoke tests locally against a packaged app:

```bash
pnpm --filter @ahv-remote-console/desktop package:mac
pnpm --filter @ahv-remote-console/desktop e2e
```

Use `package:linux` or `package:win` on other hosts. Playwright runs the specs in
`apps/desktop/e2e/` one at a time against the built app. No Nutanix cluster is needed.
See [`apps/desktop/playwright.config.ts`](../../apps/desktop/playwright.config.ts).

## Related documentation

- [Building from source](build.md)
- [Releasing](release.md)
- [Runtime proxy](../architecture/runtime-proxy.md), for the reconnect rules the proxy
  tests check
- [Agent](../architecture/agent.md)
- [Contributing](../../CONTRIBUTING.md)
- [Documentation index](../README.md)
