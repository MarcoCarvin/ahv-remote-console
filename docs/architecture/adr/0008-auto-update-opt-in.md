# ADR 0008: Auto-update is opt-in and off by default

## Status

Accepted

## Context

The desktop app uses electron-updater with a generic feed. A comment in the
updater code says it must stay off in normal builds until two conditions are met.

1. Code signing. electron-updater refuses to apply an update to an unsigned app
   on macOS and Windows. With unsigned builds, an update would download and then
   fail to install, every six hours.
2. A monotonic release version. A feed version of `<ver>-build.<pipeline>` sorts
   below the installed `<ver>` in semver, so no update would be detected.

The repository records no other alternatives and no date for changing the
default.

## Decision

The updater starts only when all of these hold.

- `AHV_REMOTE_CONSOLE_AUTOUPDATE` is exactly `1`.
- The build is packaged.
- A feed URL is set.

Any other value, or no value, leaves it off. Dev and debug builds skip it. When
it is on, a Linux update must pass a manifest signature check against a
configured public key. The update is refused if the check fails.

## Consequences

- Packaged builds do not contact the update feed by default. To get updates,
  set the variable and the feed URL.
- An unsigned build does not keep downloading an update it cannot install.
- The switch is a process environment variable. It is a deployment setting, not
  a UI setting.
- The default can change only once signing and a monotonic version exist.
- Linux updates fail closed on a bad manifest signature.

## References

- [Configuring the app](../../operations/configure.md)
- [Desktop app](../desktop.md)
- [`apps/desktop/src/main/autoUpdate.ts`](../../../apps/desktop/src/main/autoUpdate.ts)
- [`apps/desktop/src/main/updateVerify.ts`](../../../apps/desktop/src/main/updateVerify.ts)
