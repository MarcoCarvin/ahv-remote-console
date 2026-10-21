# AHV plugin

The AHV plugin is the Go component that talks to Nutanix Prism. It signs in to the
cluster, lists VMs, opens the hypervisor VNC stream and installs the agent's
prerequisites in a guest. This page describes the Prism calls it makes, how it keeps
the session alive, and how the agent ISO mount works.

## Contents

- [Responsibilities](#responsibilities)
- [Authentication](#authentication)
- [Prism REST calls](#prism-rest-calls)
- [Session cookies](#session-cookies)
- [Console WebSocket](#console-websocket)
- [Keep-alive and endpoint health](#keep-alive-and-endpoint-health)
- [Agent ISO mount](#agent-iso-mount)
- [Security properties](#security-properties)
- [Configuration](#configuration)
- [Known limitations](#known-limitations)

## Responsibilities

The plugin lives in [`plugins/ahv`](../../plugins/ahv). It is linked into the runtime,
not shipped as a separate binary. At init it registers itself with the SDK plugin
registry under the ID `nutanix-ahv`. Its small Prism client lives in
[`plugins/ahv/internal/api`](../../plugins/ahv/internal/api).

It is the only part of the product that knows about Prism Central. It:

- authenticates against the cluster with the operator's Prism session,
- lists VMs and reads VM details,
- opens the VNC WebSocket for a console,
- uploads the agent ISO, attaches it as a CD-ROM and adds the COM3 serial port the
  clipboard channel uses,
- runs VM power actions.

For operators, three points matter:

- You sign in with your normal Prism credentials or SSO. There is no separate
  account, MFA or directory to set up.
- The **Install prerequisites** and **Add serial port** actions map to Prism image
  uploads and VM spec edits. You can run them again safely. They never detach your
  disks or hard-power-off a VM.
- TLS fails closed. A cluster with a wrong or missing certificate pin is refused.

Source: [`plugins/ahv/plugin.go`](../../plugins/ahv/plugin.go).

## Authentication

The plugin has no identity system of its own. There is no app-side SAML, OIDC, MFA,
SCIM or RBAC. The package declares two authentication shapes:

| Shape | How it works |
| --- | --- |
| Local credentials | Username and password checked by Prism at `/api/nutanix/v3/users/me`. Covers local, AD/LDAP and directory users. |
| Embedded SSO (cookie) | For clusters behind an identity provider. The desktop app opens Prism's `/console/` login in a sandboxed window and returns the session cookies to the plugin. |

Only the `cookie` method is wired in. `AuthStart` rejects any other method.
`AuthComplete` and `AuthRefresh` do nothing, because the SSO flow completes inside
`AuthStart`. The manifest advertises both `auth.local` and `auth.sso`.

`WhoAmI` reads `/users/me` to report which Prism user the stored session belongs to.
The account menu therefore shows the operator, not the cluster.

Session cookies are stored in the OS keychain (Credential Manager, Keychain or
libsecret) under `endpoint/<id>/sso_cookies_json`. The keychain copy is the durable
source. `Logout` deletes the keychain entry for every known endpoint, drops in-memory
state and closes the clients.

## Prism REST calls

The client wraps a small set of Prism Central v3 endpoints and a few v4 ones. It is
not a general Prism SDK.

| Purpose | Method and path |
| --- | --- |
| Auth probe, keep-alive | `GET /api/nutanix/v3/users/me` |
| Reachability ping, no credentials | `GET /api/nutanix/v3/users/me` |
| VM list, paginated | `POST /api/nutanix/v3/vms/list` |
| VM detail | `GET /api/nutanix/v3/vms/{uuid}` |
| Image list, paginated | `POST /api/nutanix/v3/images/list` |
| Image create, metadata only | `POST /api/nutanix/v3/images` |
| Image upload | `PUT /api/nutanix/v3/images/{uuid}/file` |
| VM spec edit (CD-ROM, serial, power) | `PUT /api/nutanix/v3/vms/{uuid}` |
| Task poll (v3) | `GET /api/nutanix/v3/tasks/{uuid}` |
| Power actions (v4) | `POST /api/vmm/v4.0/ahv/config/vms/{uuid}/$actions/<verb>` |
| Task poll (v4) | `GET /api/prism/v4.0/config/tasks/{extId}` |

Source: [`client.go`](../../plugins/ahv/internal/api/client.go),
[`images.go`](../../plugins/ahv/internal/api/images.go).

### Transport

- There are two HTTP clients. The control-plane client has a 60-second timeout. The
  upload client has no client timeout and relies on the request context, with a
  5-minute response-header timeout. A large ISO upload over a slow VPN is not cut
  off.
- A non-2xx response becomes `*APIError{Method, Path, Status, Body}`.
  `IsUnauthorized` treats both 401 and 403 as a session or permission failure that
  sends the user to sign-in. `HTTPStatus` returns the raw status.
- An egress proxy works on both REST and WebSocket. Without an explicit `ProxyURL`,
  the client uses `http.ProxyFromEnvironment`, so `HTTP_PROXY`, `HTTPS_PROXY` and
  `NO_PROXY` work with no extra setup. The `network.proxy_url` and
  `network.no_proxy` settings override them. The VNC dialer uses the same proxy
  function.

### Power actions use v4

The v3 API (`PUT vms/{uuid}` with `power_state`) accepts only `ON` and `OFF`. It has
no reboot, reset or power cycle. The power buttons therefore use the v4 VMM `$actions`
endpoints.

| Button | v4 action |
| --- | --- |
| `ON` | `power-on` |
| `OFF` | `power-off` (hard force-off) |
| `ACPI_SHUTDOWN` | `guest-shutdown` |
| `ACPI_REBOOT` | `guest-reboot` |
| `RESET` | `reset` |

A v4 mutation needs an `If-Match` ETag, so the client reads the VM first. It also
sends a new `Ntnx-Request-Id` idempotency key. The 202 response must include a task
`extId` to poll. An empty `extId` on a 202 counts as a failure.

`HasVMMv4` detects a v3-only Prism Central with one cached probe per client. A 404 on
the v4 VMs collection means v3 only. Any other status means v4 is available. On a
v3-only cluster the user sees "power actions require a Prism Central with VMM v4"
instead of a bare 404.

## Session cookies

### Capture

`authStartCookie` takes the cookies the desktop app captured from the SSO window. It
builds a probe client and checks the session against `/users/me`. Only if that check
passes does it save the cookies to the keychain and install the client.

### Replay

A `CookieJar` holds the live cookies and builds the `Cookie` header by joining the
strings as they are. It does not use `http.Cookie.String()`, which strips Prism's
double-quoted values and makes Prism reject the request with "Invalid cookies
present". The jar supplies the request header and also records `Set-Cookie`
rotation.

### Rotation

Prism Central's IAM service refreshes the session on the server and returns a new
session cookie in `Set-Cookie`. If the plugin replayed the first captured cookie
forever, the session would expire on its fixed schedule and the console would keep
reconnecting. The plugin captures rotation from two places:

1. REST responses. `cookieCaptureTransport` feeds every response's `Set-Cookie`
   headers into the jar. The 30-second keep-alive gives one rotation chance every 30
   seconds.
2. The VNC WebSocket upgrade response. It does not pass through the REST transport.
   After a successful upgrade, `OpenStream` passes its `Set-Cookie` headers to the
   jar through `RotateCookies`.

When a value changes, the jar calls `onRotate`. That saves the new set to the
keychain and updates the in-memory `endpointState` under the plugin lock, so a client
rebuild starts from the latest cookies.

The merge is keyed by cookie name and capped at 64 cookies (`maxJarCookies`). A
compromised upstream cannot grow the header without limit and kill the session with
an oversized header.

### Cold start

On restart the plugin loads the cookies from the keychain and rebuilds the client
when first needed, through `clientFor`.

### Invalidation

A 401 from any path, whether a keep-alive, a VM list or the WebSocket upgrade, calls
`invalidateEndpoint`. It:

- closes the client,
- drops the in-memory credentials (Go strings, which are not zeroed),
- deletes the keychain entry,
- clears the resource cache,
- moves live console sessions on that endpoint to the re-auth state.

It does not delete the session objects, so the same session ID reconnects after
re-auth.

## Console WebSocket

The default path has no ticket exchange. The WebSocket path is stable, and the
replayed session cookies are the authentication.

`OpenSession` creates the session ID, records the endpoint, VM and cluster, and sets
the console capabilities `vnc.rfb38`, `vnc.extended_clipboard` and
`vnc.extended_desktop_size`.

It needs the VM's cluster UUID. Prism Central manages several clusters, and the VNC
endpoint needs the UUID in the query string to route the upgrade. Without it the
upgrade returns 404. The UUID usually comes from the cached `vms/list` result the
lobby already fetched. On a cache miss the plugin calls `GetVM`.

The v3 console URL, confirmed against Prism Central 7.5.1.4, is:

```text
wss://<pc>:9440/vnc/vm/<vm_uuid>/proxy?proxyClusterUuid=<cluster_uuid>
```

The upgrade request must match what Prism expects:

- The query string has only `proxyClusterUuid`. Adding `clusterUuid` makes Prism
  Central return 403.
- No subprotocol is offered. Prism's web UI sends no `Sec-WebSocket-Protocol`.
  Offering `binary` makes Prism Central return 403.
- The headers match the browser: `Origin`, `Cookie`, `User-Agent`,
  `Accept-Language`, `Cache-Control` and `Pragma`. The request leaves out
  `Sec-Fetch-*`, `Referer`, `X-XSRF-TOKEN` and CSRF tokens, because the Prism gateway
  rejects the upgrade when they are present.
- The VM UUID is checked as a canonical UUID and passed through `url.PathEscape`. A
  crafted resource ID cannot add path or query parts to the upstream URL.
- `permessage-deflate` is off and buffers are 1 MB, so a large framebuffer update is
  read or written in one system call.

### Dial failures

When Prism rejects the upgrade, the plugin never logs or shows the response body.
Prism's WAF copies request headers, including `Cookie` and `Authorization`, into 4xx
bodies. The log redactor matches by attribute key, so the body would leak
credentials into logs and on-screen errors.

| Upstream result | Plugin behaviour |
| --- | --- |
| 401 | Invalidates the endpoint and reports "session expired". |
| 403 | Reports "not authorized to open console for this VM". The session is kept. |
| Transport failure (status 0) | Transient. Retries soon, with no v3 check, because that would use the same failing path. |
| Any other status | Runs a `GetVM` check, limited to 3 s, and reports `vm powered off`, `vm not found` or `session expired`. The proxy keys off these causes. |

A stale cached cluster UUID shows up in that last case. The plugin drops the cache
entry first, and a successful read stores a fresh value, so the next dial recovers.

### v4 console token

A v4 path can mint a short-lived per-VM `VmConsoleToken` (a JWT) and dial
`wss://<pc>/console/launch/<uuid>?VmConsoleToken=<jwt>`, still sending the session
cookie. It is opt-in and requires `HasVMMv4`. The v3 `/vnc/vm/<uuid>/proxy` dial
stays the default and the fallback.

## Keep-alive and endpoint health

### Keep-alive

A background ticker calls `/users/me` every 30 seconds for each active endpoint. An
operator working in a console makes no other Prism call. Without the ping the
session would lapse, and the next VM list or upgrade would return 401. Some clusters,
especially some SSO setups, have very short idle windows, so the interval is short.

A keep-alive that returns 401 invalidates the endpoint at once. The operator does not
keep looking at a console that seems live but is dead. A 403 does not invalidate,
because it means per-resource RBAC or a transient WAF response.

The same tick also runs the deferred COM3 watcher and capability probes.

### Endpoint health

`EndpointHealth` drives the status dot on the cluster map. It uses a separate client
with no stored cookies, honours the endpoint's TLS pin, and sends one short GET. Any
HTTP response, including 401 or 403, means the host is up. Only a transport failure
means offline. No credentials are sent and no body is read, so probing every node
leaks nothing and works before sign-in.

## Agent ISO mount

**Install prerequisites** runs this flow. It puts the agent ISO into Prism's image
catalog, attaches it to the VM as a CD-ROM and adds the COM3 serial port the
clipboard needs. The flow is idempotent from start to end.

The entry point is `MountAgentISO`. It detaches from the HTTP request context,
because Go cancels that context when the response returns. The whole operation is
limited to 15 minutes, because the upload alone can take minutes over a VPN.

### Catalog name and reuse

The catalog name does not depend on content. Production uses the fixed name
`ahv-remote-console-agent.iso`. Development builds use
`ahv-remote-console-agent-dev-<version>.iso`.

The content SHA-256 goes in `spec.description` as `sha256:<hex>`. It is not in the
name and not in `checksum_value`. Prism does not fill `checksum_value` for an
operator upload, so a check on it would never match, and every mount would create a
duplicate image.

`classifySameName` decides what to do with existing entries:

| Existing entry | Action |
| --- | --- |
| Same name, matching description SHA, state `COMPLETE` (or empty on older builds) | Reuse |
| Same name, different or missing SHA | Override: delete the old entry and upload the new one |
| Extra entries with the same name | Delete them, best-effort, skipping images in use |

### Upload in two steps

`UploadImage` runs two steps, each confirmed by a task poll:

1. `POST /api/nutanix/v3/images` creates the catalog entry with metadata only, no
   `data_source_reference`. The plugin waits for the create task before the upload.
   An upload before the create commits fails with HTTP 500.
2. `PUT /api/nutanix/v3/images/{uuid}/file` streams the ISO as octet-stream. It
   returns a task UUID in the JSON body or a `Location` header, depending on the
   Prism build. The plugin polls it to completion, up to about 10 minutes.

If the create task, the upload or the ingest task fails, `deleteOrphan` removes the
half-created entry. Otherwise the reuse logic could later skip around a broken
INACTIVE duplicate.

The plugin lists images again right before the POST. This narrows, but cannot close,
the race when two VMs mount at once, because Prism does not enforce unique names. v3
task polling backs off exponentially, up to 5 seconds.

`MountAgentISO` retries a failed upload up to 3 times, after 750 ms and then 2 s.
`ErrImageNotPlaced` and an RBAC 403 are not retried, because neither is transient.

### Cluster placement

An uploaded image lands on one cluster that Prism Central chooses, not always the
VM's cluster. The v3 image spec has no reliable placement input.
`initial_placement_ref_list` is sent, but Prism does not always honour it.

`imageActiveOnCluster` reads `status.resources.current_cluster_reference_list` to
learn where the image is usable. On a single-cluster Prism Element (empty cluster
UUID) it checks `status.state == "COMPLETE"` instead.

`ensurePlacedOnCluster` polls for up to about 45 seconds for the image to show as
active on the VM's cluster. Prism Central can finish replication a few seconds after
the ingest task reports SUCCEEDED.

If the image is not on that cluster, the attach fails with
`NO_POLICY_COMPLIANT_CLUSTER`. The fix is an AHV image placement policy or a catalog
checkout to the VM's cluster. Neither is a plain v3 REST call.

`ResolvePublishedImage` is a reuse-only variant. It finds an existing entry with the
same name without creating or deleting anything, for operators whose role cannot
create images. It still checks placement, read-only.

### CD-ROM attach

Prism v3 `PUT /vms/{uuid}` replaces the whole spec. Any field left out is cleared. So
every VM edit reads the full spec, changes a copy, removes `status` (Prism rejects it
on write) and sends the document back. This keeps metadata, including `spec_version`
for optimistic concurrency, and every field the plugin does not model.

Every VM spec edit retries once on HTTP 409 (`CONCURRENT_REQUESTS_NOT_ALLOWED` or a
`spec_version` conflict). A fresh GET picks up the new `spec_version`.

The attach changes media and never adds a device. AHV does not hot-add IDE or SATA
disks to a running VM. The plugin finds the first existing CD-ROM and sets its
`data_source_reference` to the image UUID, the same as swapping a disc. AHV supports
that at runtime.

- A VM with no CD-ROM gets an error asking you to add an empty CD-ROM in Prism and
  try again.
- Attaching the same image again does nothing (`reused=true`, no PUT).
- The drive letter hint is `D:`, the first CD-ROM on Windows. It is a guess only.

Source: [`cdrom.go`](../../plugins/ahv/internal/api/cdrom.go),
[`mount.go`](../../plugins/ahv/internal/api/mount.go).

### COM3 serial port

COM3 is serial port index 2. `EnsureSerialPort(ctx, vmUUID, 2)` adds a port at that
index for the vdagent clipboard channel. It is idempotent. Two AHV behaviours affect
it:

- `PUT /vms` is asynchronous. A 2xx means only that Prism accepted it. The plugin
  polls the edit task to learn whether the change applied.
- AHV cannot hot-add a serial port. On a running VM the add ends in
  `PowerCycleRequired`. The VM must be fully powered off and on again. A guest ACPI
  reboot is not enough, because it does not restart the QEMU process, so the device
  never appears.

`attachWithSerial` picks a strategy:

| VM state | Action |
| --- | --- |
| COM3 present | CD-ROM media change only |
| COM3 absent, VM off | One merged spec PUT adds the ISO and COM3 together (`AttachCDROMAndSerialPort`). One `spec_version` bump, one task, no 409 window. |
| COM3 absent, VM on, consent given (`opts.PowerCycle`) | Power cycle with consent, described below |
| COM3 absent, VM on, no consent | Attach the ISO hot, run the serial add that reports `power_off_required`, and arm a deferred add |

### Deferred COM3 add

When a serial add cannot apply to a running VM, the plugin keeps the VM in an
in-memory pending set. On a later keep-alive tick where it sees the VM off, it
completes the add. This path never issues a power operation. It only watches.

The pending set is in memory only. The renderer's notice and the **Add serial port**
button let you arm it again. That button calls `AddSerialPort`, which works on a VM
that is off and has no active session.

### Image cleanup

After a successful mount, a background sweep deletes older plugin images. It touches
only the plugin's own image names, skips images in use, and ignores errors. It is
tied to the plugin's lifetime, so `Shutdown` cancels it and waits for it.

### Power cycle with consent

Only `mountWithPowerCycle` changes power state during a mount, and only with explicit
operator consent. It uses the v3 spec round trip (`SetPowerStateV3`), because v3 OFF
can carry `power_state_mechanism {"mechanism":"ACPI"}`. That is a graceful guest
shutdown, the only OFF this client requests.

1. Request an ACPI guest shutdown and poll for OFF every 3 seconds, for up to 120
   seconds (`cycleOffBudget`).
2. If the guest does not reach OFF in time, abort. There is no hard power-off and no
   power-on. The ISO attaches hot and COM3 is deferred. Power state stays as it was.
3. Once OFF is seen, run the merged ISO and COM3 PUT, then power the VM back on. The
   VM is powered on even if the merged update failed.

The mount flow never issues a hard power-off.

Source: [`power_v3.go`](../../plugins/ahv/internal/api/power_v3.go).

### Clipboard channel and live migration

The agent uses the COM3 port for clipboard sync. The clipboard rides SPICE
qemu-vdagent over an ISA serial port (COM3, or `/dev/ttyS2` on Linux), below the
guest network stack, so it works under a full-tunnel VPN. qemu-vdagent blocks QEMU
live migration. A console-following libvirt hook keeps idle and closed-console VMs
migratable. See [Live migration](live-migration.md) and [Agent](agent.md).

For the plugin, the contract is short. COM3 must be present, and adding it can need a
full power cycle.

## Security properties

- No credential reflection in logs or on screen. The plugin never logs or shows the
  body of a rejected upgrade, because Prism's WAF copies `Cookie` and
  `Authorization` into it.
- Credentials live only in the OS keychain, never on disk. In memory they are Go
  strings, dropped but not zeroed on logout.
- TLS fails closed with SPKI pinning. The plugin pins the leaf certificate's
  SubjectPublicKeyInfo, so a certificate rotation on the same key still works. It
  checks the pin in `VerifyConnection`, which covers new and resumed sessions.
- An endpoint with `allow_insecure_tls` and no valid pin is disabled.
  `VerifyConnection` always returns an error instead of falling back to system
  roots. A pin that does not parse disables the endpoint the same way.
- Operator `CustomCAFiles` add to the trust pool.
- Resource IDs must be canonical 8-4-4-4-12 UUIDs before any value goes into a Prism
  URL.
- The mount flow only changes a CD-ROM's media. It never adds or removes disks. Its
  only power change is a graceful ACPI cycle with consent. It never hard-powers-off
  a VM.
- Session cookies are capped at 64 per jar, so an upstream cannot grow the cookie
  header without limit.

## Configuration

| Setting | Effect |
| --- | --- |
| `AHV_REMOTE_CONSOLE_AGENT_ISO_PATH` | Path of the agent ISO. Default `build/agent.iso`. The older `AHV_REMOTE_CONSOLE_BRIDGE_ISO_PATH` is still read. |
| `HTTP_PROXY`, `HTTPS_PROXY`, `NO_PROXY` | Egress proxy for REST and the VNC WebSocket |
| Endpoint `allow_insecure_tls` and pin | TLS trust, see [Security properties](#security-properties) |
| `security.custom_ca_files` | Extra trusted CAs (`CustomCAFiles`) |
| `network.proxy_url`, `network.no_proxy` | Explicit egress proxy and its bypass list. When empty, the environment variables apply |

See [Configuring endpoints](../operations/configure.md).

## Known limitations

- Reuse and override depend on `spec.description` surviving an operator upload. The
  code marks this as an assumption to check on each target Prism build.
- `ensurePlacedOnCluster` runs in a temporary diagnostic mode. When placement is not
  yet reported, it logs and continues to the attach. It does not delete the entry and
  return `ErrImageNotPlaced`. That error path is wired but not used, and the code
  marks the mode to be reverted. You may see a mount succeed that the normal code
  would refuse, or a later `NO_POLICY_COMPLIANT_CLUSTER` attach error instead of the
  clearer pre-check error.
- The v4 power path and the v4 console-token path follow the documented v4 contract
  but are not yet validated against a live Prism build. The open points are the exact
  request paths, the ETag and `If-Match` requirement, and the task output field that
  carries the JWT. Do not make v4 the default console dial until they are validated.
- The code comments call the CD-ROM "SATA", but the attach does not set an adapter
  type. It keeps `device_type` and `disk_address` of the existing CD-ROM. The adapter
  is whatever the VM already has.
- `auth.local` is advertised in the manifest, but `AuthStart` handles only the
  `cookie` method.

## Related documentation

- [Architecture overview](overview.md)
- [Runtime and VNC proxy](runtime-proxy.md)
- [Agent](agent.md)
- [Live migration](live-migration.md)
- [Configuring endpoints](../operations/configure.md)
- [Hypervisor setup](../operations/hypervisor-setup.md)
- [Plugin SDK](../development/plugin-sdk.md)
- [Threat model](../security/threat-model.md)
