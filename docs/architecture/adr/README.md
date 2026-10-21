# Architecture decision records

This directory records the architectural decisions behind AHV Remote Console.
Each record describes one decision as it stands in the code, why it was made,
and what follows from it.

When a decision changes, write a new record that supersedes the old one, and set
the old record's status to point at it.

## Index

| ADR | Title | Status |
| --- | ----- | ------ |
| [0001](0001-electron-desktop-shell.md) | Electron is the desktop shell | Accepted (inferred from code; confirm) |
| [0002](0002-separate-go-runtime-daemon.md) | A separate Go runtime holds the Prism and VNC logic | Accepted (inferred from code; confirm) |
| [0003](0003-loopback-hmac-ipc.md) | Loopback HTTP and WebSocket with per-request HMAC | Accepted |
| [0004](0004-renderer-never-holds-secret.md) | The sandboxed renderer never holds the IPC secret | Accepted |
| [0005](0005-ahv-plugin-statically-linked.md) | The AHV plugin is linked into the runtime binary | Accepted |
| [0006](0006-clipboard-over-vdagent-serial.md) | Clipboard uses qemu-vdagent on a serial port, not the guest network | Accepted |
| [0007](0007-agent-iso-per-user-install.md) | The agent ships as an ISO and installs per user | Accepted |
| [0008](0008-auto-update-opt-in.md) | Auto-update is opt-in and off by default | Accepted |
| [0009](0009-single-go-module.md) | One Go module for the whole repository | Accepted |
| [0010](0010-single-pnpm-workspace.md) | One pnpm workspace for the TypeScript code | Accepted |
| [0011](0011-prism-delegated-authentication.md) | Prism handles identity and the app has none of its own | Accepted |

## Status values

| Status | Meaning |
| ------ | ------- |
| Accepted | The repository states the decision as the current design. |
| Accepted (inferred from code; confirm) | The code implements the decision, but the repository does not state the reasoning. A maintainer should confirm it. |
| Superseded by ADR NNNN | A later record replaces this one. |

## Template

Name the file `NNNN-short-slug.md` with the next free number. Keep the record
between 25 and 50 lines. If the repository does not record the alternatives
considered, say so in the context.

```markdown
# ADR NNNN: <decision in a short phrase>

## Status

Accepted

## Context

The problem or constraint that led to the decision.

## Decision

The decision, stated as the current design.

## Consequences

- What follows from the decision, good and bad.

## References

- [Related document](../overview.md)
```

## Related documentation

- [Architecture overview](../overview.md)
- [Glossary](../../reference/glossary.md)
