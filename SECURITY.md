# Security policy

This page explains how to report a vulnerability in AHV Remote Console and what
happens after you report it.

## Reporting a vulnerability

Use GitHub private vulnerability reporting for this repository: open the
**Security** tab, then select **Report a vulnerability**.

> [!IMPORTANT]
> Do not open a public issue or pull request for a security problem.

Include in your report:

- A clear description of the vulnerability.
- A minimal reproducer or proof of concept.
- The affected version, from `ahv-remote-console-runtime --version`.
- The platform: Windows, macOS or Linux, and the build identifier.
- Any suggested fix.
- Whether you want to be credited in the advisory.

## Response targets

| Phase | Target |
| --- | --- |
| Acknowledgement | within 2 business days |
| Triage and severity assessment | within 5 business days |
| Patch availability (critical) | within 14 calendar days |
| Patch availability (high) | within 30 calendar days |
| Public advisory | coordinated with reporter |

## Safe harbor

We will not take legal action against researchers who:

- Make a good-faith effort to follow this policy.
- Avoid privacy violations, destruction of data and service degradation.
- Do not take any data beyond what is needed to show the issue.
- Give us reasonable time to fix the issue before public disclosure.

## Supported versions

Only the latest minor release receives security updates.

## Data handling and privacy

Telemetry is off by default. It is described in
[`packages/telemetry/README.md`](packages/telemetry/README.md). The audit log in
`apps/runtime/internal/audit` never records clipboard contents.

## Related documentation

- [Security documentation](docs/security/README.md)
- [Threat model](docs/security/threat-model.md)
- [Data handling](docs/security/data-handling.md)
- [Desktop app architecture](docs/architecture/desktop.md)
- [IPC reference](docs/reference/ipc.md)
