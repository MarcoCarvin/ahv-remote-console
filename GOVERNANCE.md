# Governance

This page explains who makes decisions in AHV Remote Console and how. The project is
run by its maintainers. The reference owner is listed in [MAINTAINERS.md](MAINTAINERS.md).

## Decisions

- The maintainer who reviews a pull request decides on day-to-day changes.
- Changes to the trust boundary, the clipboard path, IPC signing, cryptography or the
  release process need review from the reference owner before merge. See
  [Security-sensitive changes](CONTRIBUTING.md#security-sensitive-changes).
- The reference owner cuts releases. Each release is tagged, and
  `.github/workflows/release.yml` attaches its artifacts to a GitHub Release.

## Security

Vulnerabilities are handled under [SECURITY.md](SECURITY.md), not through the normal
issue flow.

## Changes to this document

Changes to governance, maintainers or the code of conduct go through a pull request
reviewed by the reference owner.
