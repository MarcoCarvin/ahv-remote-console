# ADR 0011: Prism handles identity and the app has none of its own

## Status

Accepted

## Context

The app uses Nutanix Prism for two things. The REST API lists VMs, manages images
and edits VM specs. The hypervisor VNC WebSocket carries the console. Prism has
its own login, including SSO for clusters behind an identity provider. The
operator acts as themselves in Prism, and the app should not add accounts or
directory settings of its own. The repository does not record the alternatives considered, such as an app-side
OIDC or SAML login, or a Prism service account. A v4 console-token path exists in
code but is not the default.

## Decision

The app implements no identity. It has no SAML, OIDC, MFA, SCIM or RBAC of its
own. The AHV plugin signs in through Prism's own login page in an embedded SSO window.
It captures the session cookies, stores them in the OS keychain, and replays them
on REST calls and on the VNC WebSocket upgrade. There is no separate ticket
exchange. Prism v3 REST and the v3 VNC proxy path are the default. The v4
console-token path is opt-in.

Basic username and password auth is declared, but `cookie` is the only auth
method wired in the current code path.

## Consequences

- Operators need no separate account, MFA or directory setup in the app. Access
  is what Prism grants that user. Per-resource RBAC denials appear as
  `rbac_denied`.
- Cookies are the credential. They live in byte slices the plugin can zero, are
  never written to disk, and are captured again when Prism rotates them.
- A keep-alive call to `/users/me` every 30 seconds keeps a console-only session
  warm, because a console makes no other Prism call.
- The app depends on Prism's login page and cookie names. A change in Prism's
  login flow breaks sign-in until the plugin is updated.
- The v4 path still needs live validation. Until then, v3 stays the default.
- The app enforces no access policy of its own. By design it acts as the
  signed-in operator and changes guest settings only through the documented
  procedure.

## References

- [AHV plugin](../ahv-plugin.md)
- [Architecture overview](../overview.md)
- [`plugins/ahv/plugin.go`](../../../plugins/ahv/plugin.go)
