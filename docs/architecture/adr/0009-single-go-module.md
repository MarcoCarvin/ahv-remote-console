# ADR 0009: One Go module for the whole repository

## Status

Accepted

## Context

The repository holds several Go programs. They are the runtime, the AHV plugin,
the agent, and the `cmd/*` tools `isobuilder` and `agentpkg`. They ship as one
product and share the vdagent and protocol code.

Splitting the code into separate modules lets a test run cover only some of it.
A run limited to the runtime and plugin suites leaves the agent's clipboard
suite untested on merge.

The repository does not record other alternatives, such as one module per app.
The per-tier Makefiles remain as thin wrappers.

## Decision

There is one `go.mod` at the repository root, with module path
`github.com/MarcoCarvin/ahv-remote-console`. No app or plugin has its own
`go.mod`. Every Go package lives in that module.

## Consequences

- `go test ./...`, `go vet ./...`, `gosec ./...` and `govulncheck ./...` from the
  root cover the whole codebase in one pass, including the agent and the
  `cmd/*` tools.
- The build guide says the code must not be split into several modules again.
- The Go version in `go.mod` applies everywhere. The runtime and the agent build
  with it.
- Windows and Linux agent files are build-tagged inside the module, so a host
  build does not compile every file. Cross-compile with `GOOS` to exercise the
  other platform's paths.

## References

- [Build guide](../../development/build.md)
- [Testing guide](../../development/testing.md)
- [Architecture overview](../overview.md)
- [Runtime README](../../../apps/runtime/README.md)
- [`go.mod`](../../../go.mod)
