# ADR 0005: The AHV plugin is linked into the runtime binary

## Status

Accepted

## Context

The runtime hosts plugins. The AHV plugin is the only in-tree plugin. It speaks
Prism REST and VNC for the runtime.

A TODO in the runtime manager names a possible later option, shipping
out-of-tree plugins as `.so`, `.dylib` or `.dll` files.

The repository does not record why static linking was chosen over a loadable
plugin for v1. It records no decision on out-of-tree plugins beyond that TODO.

## Decision

In v1 the AHV plugin is statically linked into the runtime binary. There is no
shared object to load. A blank import in `registration.go` registers the plugin
at init.

`make plugins` is a placeholder that prints a notice. The `--plugins-dir` flag
has no effect for in-tree plugins.

## Consequences

- Building the runtime builds the plugin. Both share one release, one version
  stamp and one set of build flags (`CGO_ENABLED=0`).
- Adding or removing a plugin needs a runtime rebuild. There is no runtime
  discovery of in-tree plugins.
- The desktop app still passes the plugin directory to the runtime and creates
  it on disk, so the Go side's `MkdirAll` succeeds. This path must change if
  out-of-tree plugins ship.
- A plugin fault happens inside the runtime process. The runtime's recoverer
  middleware turns handler panics into 500 responses.

## References

- [Build guide](../../development/build.md)
- [AHV plugin](../ahv-plugin.md)
- [Runtime proxy](../runtime-proxy.md)
- [Runtime README](../../../apps/runtime/README.md)
- [`apps/runtime/internal/plugins/registration.go`](../../../apps/runtime/internal/plugins/registration.go)
- [`apps/desktop/src/main/runtime-manager.ts`](../../../apps/desktop/src/main/runtime-manager.ts)
