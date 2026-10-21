# ADR 0007: The agent ships as an ISO and installs per user

## Status

Accepted

## Context

The clipboard feature needs a resident program inside each guest. The product
does not use Nutanix Guest Tools for the clipboard, so the agent cannot come
from NGT. The operator must be able to install it without the app pushing code
into the guest. The operator mounts the agent ISO once per template.

The host does not know the guest OS. The agent ISO carries both Windows and Linux
installers, and the console tells the operator which one to run. The repository does not record why an ISO was chosen, or which other delivery
methods were considered.

## Decision

The agent is one binary. `cmd/isobuilder` packages it with the other assets into
an ISO. The AHV plugin uploads the ISO to Prism's image service and attaches it
to the VM as a CD-ROM. SATA is the default, and an existing CD-ROM adapter is
reused. The operator runs the installer inside the guest.

The install is per user and never machine-wide.

- On Windows, an `HKCU` Run entry starts the agent as the logged-in user.
- On Linux, a per-user `systemd --user` unit runs it. A headless install script
  also runs without root.

## Consequences

- The operator controls installation. The app runs no guest code unless the
  operator runs the installer.
- The clipboard is scoped to the operator's own session. A second user on the
  same VM cannot read it.
- The ISO must be rebuilt after the agent changes. `make iso` depends on
  `agent-bins`, so run `make agent-bins` first or the ISO ships stale binaries.
- The upload is a multi-megabyte transfer through Prism. The plugin uses a
  separate upload client with a 5-minute response-header timeout.
- On Windows, the display-resize path may need one elevation prompt. The prompt
  names the binary.

## References

- [Agent](../agent.md)
- [AHV plugin](../ahv-plugin.md)
- [Connect and clipboard](../../operations/connect-and-clipboard.md)
- [`cmd/isobuilder/main.go`](../../../cmd/isobuilder/main.go)
