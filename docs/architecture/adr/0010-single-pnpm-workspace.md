# ADR 0010: One pnpm workspace for the TypeScript code

## Status

Accepted

## Context

The TypeScript code has one app and several libraries. The desktop app uses
`protocol` (wire types), `sdk` (the typed runtime client), `ui`, `telemetry` and
`native-keyhook`. The SDK is the only path from the renderer to the runtime.

The `protocol` package is imported from the sandboxed renderer, so it must stay
free of runtime dependencies.

The repository gives no reason for keeping every TypeScript package in one
workspace. It does not record the alternatives considered, such as separate
repositories or a lockfile per package.

## Decision

`pnpm-workspace.yaml` declares one workspace with two globs, `apps/desktop` and
`packages/*`. The desktop app depends on the packages through `workspace:*`.

All workspace settings live in that file.

- an isolated node linker
- `preferWorkspacePackages`
- an explicit `allowBuilds` list
- one override for a vulnerable `js-yaml` range

## Consequences

- One lockfile, `pnpm-lock.yaml`, pins the whole TypeScript tree. Overrides apply
  to every package. The `js-yaml` override exists because electron-updater pulls
  in a vulnerable range.
- Workspace packages resolve to local source. A change in `packages/protocol`
  reaches the desktop app without a publish.
- Build the packages before type-aware lint, or the lint reports false
  positives.
- The workspace does not enforce that `protocol` stays dependency-free. The
  package header states the rule.
- `allowBuilds` is an allowlist. Add any new package that needs install scripts.

## References

- [Build guide](../../development/build.md)
- [Architecture overview](../overview.md)
- [`pnpm-workspace.yaml`](../../../pnpm-workspace.yaml)
- [`packages/protocol/src/index.ts`](../../../packages/protocol/src/index.ts)
