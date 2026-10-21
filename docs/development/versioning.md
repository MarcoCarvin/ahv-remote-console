# Versioning

This page lists the versioning rules the project follows. It is for maintainers who
cut releases.

## Application version

- The desktop app version is `version` in `apps/desktop/package.json`. The root
  `package.json` has its own version. Both are set by hand.
- Versions follow [Semantic Versioning 2.0.0](https://semver.org/spec/v2.0.0.html), as
  stated in [`CHANGELOG.md`](../../CHANGELOG.md).
- Each release version must be strictly greater than the one before. Release versions
  have no prerelease suffix, because a prerelease sorts below the plain version. The
  auto-updater depends on this order. See
  [Update integrity](../architecture/update-integrity.md).
- Local builds stamp binaries with `git describe --tags --always --dirty`. Release
  builds stamp the runtime with the tag name.

## Release trigger

Pushing a tag that matches `v*.*.*`, for example `v1.2.3`, starts
`.github/workflows/release.yml`. See [Releasing](release.md#release-workflow).

## Changelog

- Each release has a section in `CHANGELOG.md` in the Keep a Changelog 1.1.0 format.
- `[Unreleased]` lists changes that are not released yet.
- The release workflow uses the section for the tagged version as the release notes.

## Compatibility

| Surface | Rule |
| --- | --- |
| Plugin SDK (`apps/runtime/pkg/sdk`) | The package comment says it follows semantic versioning. No further rules are stated. See [Writing a runtime plugin](plugin-sdk.md#compatibility) |
| Wire protocols (runtime API, IPC, stream frames) | The [protocol](../reference/protocol.md) and [IPC](../reference/ipc.md) references describe the current contract. They have no version mechanism |
| Configuration file | Migration from older entry types is described in [Configuring](../operations/configure.md) |

## Who decides

The reference owner listed in [`MAINTAINERS.md`](../../MAINTAINERS.md) cuts releases.
See [`GOVERNANCE.md`](../../GOVERNANCE.md).

## Related documentation

- [Releasing](release.md)
- [Writing a runtime plugin](plugin-sdk.md)
- [Changelog](../../CHANGELOG.md)
