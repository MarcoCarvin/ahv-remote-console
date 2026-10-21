# Security documentation

These pages describe how AHV Remote Console protects credentials, the console
stream and the clipboard, and what data it keeps.

| Page | Contents |
| --- | --- |
| [Threat model](threat-model.md) | Assets, trust boundaries, threat actors, threats with controls and residual risk, non-goals, known gaps |
| [Data handling](data-handling.md) | What data the app stores, where, for how long, and what leaves the workstation |

## Reporting a vulnerability

Report vulnerabilities privately through GitHub's private vulnerability
reporting, as described in [SECURITY.md](../../SECURITY.md). Do not open a public
issue or pull request. Include a description, a minimal reproducer, the affected
version and your platform.

## Related documentation

- [Architecture overview](../architecture/overview.md)
- [Desktop architecture](../architecture/desktop.md)
- [Runtime and proxy](../architecture/runtime-proxy.md)
- [AHV plugin](../architecture/ahv-plugin.md)
- [In-guest agent](../architecture/agent.md)
- [IPC reference](../reference/ipc.md)
