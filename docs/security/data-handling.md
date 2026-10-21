# Data handling

This page lists the data AHV Remote Console handles, where it is stored, how long
it lasts, and whether it leaves the workstation. It is for operators and
reviewers who need to answer privacy or retention questions.

"Leaves the machine" means the app sends the data over a network connection off
the workstation. Traffic to the runtime on the loopback address does not count.

## Contents

- [Summary](#summary)
- [Session cookies](#session-cookies)
- [Endpoint configuration](#endpoint-configuration)
- [IPC secret and handshake file](#ipc-secret-and-handshake-file)
- [Clipboard](#clipboard)
- [Audit log](#audit-log)
- [Logs](#logs)
- [Telemetry](#telemetry)
- [Diagnostics bundles](#diagnostics-bundles)
- [Renderer storage](#renderer-storage)
- [Agent files in the guest](#agent-files-in-the-guest)
- [Outbound network connections](#outbound-network-connections)
- [Deleting data](#deleting-data)
- [Open questions](#open-questions)

## Summary

| Data | Where stored | Lifetime | Leaves the machine |
| --- | --- | --- | --- |
| Prism session cookies, persisted | OS keychain, service `ahvRemoteConsole` | Until sign-out or session expiry. Endpoint delete does not remove them | Yes, to Prism only |
| Session cookies in runtime memory | Go strings in the AHV plugin | Until logout or expiry | Yes, to Prism only |
| Session cookies during SSO sign-in | `persist:sso-<id>` partition; main and renderer memory | Never cleared by the code | No. Passed to the runtime over loopback |
| Endpoint configuration | `<data-dir>/config.json`, mode 0600 | Until edited or deleted | No |
| Proxy URL and telemetry endpoint | `config.json` | As above | Yes, when a proxy or telemetry is used |
| IPC secret | Main and runtime memory; stdin pipe | Runtime process life | No |
| Handshake file | `<data-dir>/runtime.json`, mode 0600 | While the runtime runs | No |
| Host clipboard text | Main process memory | Until the clipboard changes | Yes, to the focused console |
| Clipboard text in transit | RFB stream through the runtime proxy | In flight | Yes, to Prism, then the guest |
| Clipboard loop-suppression ledger | Renderer memory, hashes only | 10 s, 128 entries | No |
| Clipboard in the agent | Guest memory during a transfer | Not verified | Yes, to the host over serial |
| Audit log | `<data-dir>/audit.log`, mode 0600 | No deletion or rotation | No. Tail goes into diagnostics |
| Runtime log | `<data-dir>/runtime.log` | No rotation | No. Tail goes into diagnostics |
| Desktop log | electron-log `main.log` | Library default | No |
| Agent log `hcbridge.log` | Guest user data directory | Truncated to the last 5 MiB | No |
| Renderer local storage | Chromium profile of the desktop app | No in-app clear | No |
| Renderer IndexedDB and sessionStorage | Not used | Not applicable | Not applicable |
| Runtime traces | Not stored | Off by default | Only when enabled, to the collector |
| Renderer telemetry events | Not stored, discarded | Not applicable | No |
| Diagnostics bundle | Zip at a path the operator chooses | Until the operator deletes it | Only if the operator sends it |
| Agent ISO | Cluster Image Service | Until removed in Prism | Yes, uploaded to Prism |
| Linux open-at-login entry | `$XDG_CONFIG_HOME/autostart/ahv-remote-console.desktop` | Until turned off | No |
| Agent install files | Guest user directories | Until uninstalled | No |

## Session cookies

The app does not collect a username or password. The operator signs in inside
the SSO window. That window is a sandboxed `BrowserWindow` on its own partition,
with no preload script and no Node access. The app reads only the Prism session
cookies from that partition and sends them to the runtime with `startAuth`. The
auth routes are on the main process signing allowlist.

The runtime stores the cookies in the OS keychain through
[`go-keyring`](https://github.com/zalando/go-keyring) v0.2.8. There is no file
fallback, and the app adds no encryption of its own.

| OS | Store | Notes |
| --- | --- | --- |
| Windows | Credential Manager (wincred) | Value written as `CredentialBlob` |
| macOS | `security` command | Value sent base64-encoded on stdin, not on the command line |
| Linux | Secret Service over D-Bus | Login collection unlocked first |

The full key is `plugin/ahv/endpoint/<endpoint-id>/sso_cookies_json`. The
runtime adds the `plugin/<id>/` prefix for every plugin. On each start it writes
and deletes a `__selftest__` entry.

The plugin writes the entry at sign-in after Prism accepts the cookies, and
again when Prism rotates a cookie. After a restart it reads the entry back to
restore the session.

If the keychain is unavailable:

- At startup, the self-test fails and the runtime exits with `open keystore`.
  The desktop app shows a start failure that points to `runtime.log`.
- At sign-in, the write fails with `persist cookies` and no session is created.
- On rotation, the write error is ignored. Memory holds the new cookies and the
  keychain keeps the old ones.

The plugin deletes keychain entries in two cases. Sign-out deletes the entry for
every endpoint it knows. Session expiry reported by Prism deletes the entry for
that endpoint. Deleting an endpoint changes `config.json` only and leaves the
keychain entry until sign-out.

In memory, cookies are Go strings in `api.SessionCookie`. Go strings cannot be
zeroed, so the plugin's `wipe()` drops references and does not overwrite bytes.
The configure guide describes them as byte slices the plugin can zero. The code
does not do that.

Source: [`sso-login.ts`](../../apps/desktop/src/main/sso-login.ts),
[`keystore.go`](../../apps/runtime/internal/keystore/keystore.go),
[`plugin.go`](../../plugins/ahv/plugin.go).

## Endpoint configuration

The desktop app passes `--data-dir` to the runtime. The runtime uses the same
default when the flag is absent.

| OS | Data directory |
| --- | --- |
| Windows | `%APPDATA%\AHV Remote Console` |
| macOS | `~/Library/Application Support/AHV Remote Console` |
| Linux | `$XDG_DATA_HOME/ahvRemoteConsole`, default `~/.local/share/ahvRemoteConsole` |

The desktop app creates the directory with no explicit mode. The runtime writes
`config.json` and its temp file with mode 0600, and creates the directory with
mode 0700 if it is missing.

`config.json` has the sections `ui`, `network`, `security`, `telemetry`,
`endpoints` and `feature_flags`. Each endpoint holds `id`, `label`, `plugin_id`,
`url`, `auth_methods`, `pinned_cert_b64`, `allow_insecure_tls` and `tags`. The
file holds no password, token or cookie. The pin is a public certificate.

Two fields can hold text that might include credentials.
`network.proxy_url` is parsed but stored as written, including any user
information. `telemetry.otlp_endpoint` is only checked for a scheme. Neither is
redacted from diagnostics bundles.

A missing file is replaced with defaults. Legacy auth values `oidc` and `basic`
become `cookie` on load. Validation errors at startup are logged as warnings and
the runtime continues. Validation on save is strict.

Source: [`config.go`](../../apps/runtime/internal/config/config.go).

## IPC secret and handshake file

The desktop app generates 48 random bytes at startup. It starts the runtime with
`--ipc-secret-stdin`, writes the secret to stdin as hex with a newline, and
closes stdin. The secret never appears in the environment or on the command
line. An environment-variable form exists for development, and the desktop app
does not use it. The runtime rejects secrets shorter than 32 bytes.

Every route passes the loopback and HMAC checks. The listener binds to
`127.0.0.1`, and requests with a non-loopback `Host` header are refused.

The runtime zeroes its `Secret` on exit. The desktop app zeroes its buffer after
a successful handshake check. On a mismatch it returns before that step, so the
buffer is not zeroed. These copies are never zeroed: the decoded byte slice and
stdin read buffer in the runtime, and the hex string the desktop app builds.

After binding, the runtime writes `<data-dir>/runtime.json` with `O_EXCL` and
mode 0600. It holds the PID, version, endpoint, start time and `secret_hash`,
the base64 SHA-256 of the secret. The runtime removes the file on exit. The
desktop app removes a stale copy before it starts the runtime.

Source: [`runtime-manager.ts`](../../apps/desktop/src/main/runtime-manager.ts),
[`handshake.go`](../../apps/runtime/internal/handshake/handshake.go).

## Clipboard

The main process polls the OS clipboard every 150 ms. It keeps the last text in
`lastClipText` until the clipboard changes and sends changes to the focused
console only. It writes no clipboard text to disk.

The renderer keeps a loop-suppression ledger. Each entry holds a direction, a
SHA-256 hash, a format and a time. It never holds the text.

The runtime proxy passes ClientCutText to the Prism console WebSocket unchanged
and only marks clipboard activity. Prism forwards the text to the agent over a
virtual serial channel. The runtime writes no clipboard text to disk and logs
only lengths.

In the guest, the Linux agent runs `xclip`, `xsel`, `wl-copy` or `wl-paste` with
a reduced environment and passes text on stdin. The Windows agent calls the
Win32 clipboard API. Logs record sizes, not text. The helper `clipboard.Wipe`
has no caller, so agent buffers are not zeroed through it.

The audit actions `clipboard.host_to_vm` and `clipboard.vm_to_host` are defined,
but nothing writes them. No clipboard transfer is recorded under those actions.
Bridge transfers can be posted to `/v1/agent/audit` as `bridge.clip_in` and
`bridge.clip_out`. That route has no text field and records the byte count,
direction and an optional SHA-256. It accepts only allowlisted actions.

Source: [`main.ts`](../../apps/desktop/src/main/main.ts),
[`Ledger.ts`](../../apps/desktop/src/renderer/clipboard/Ledger.ts),
[`proxy.go`](../../apps/runtime/internal/proxy/proxy.go).

## Audit log

The runtime writes `<data-dir>/audit.log` with `O_APPEND` and mode 0600. When
`security.audit_trail_enabled` is false, nothing is written.

Each line is a JSON object with `seq`, `ts` (UTC), `actor` (the OS user name),
`action`, `endpoint_id`, `session_id`, `vm_id`, `plugin_id`, `result`, `reason`,
`extra` and `prev_hash`. The `reason` value passes through the log scrubber.

`prev_hash` is the SHA-256 of the previous line. The first record hashes the
empty string. The chain has no key, so anyone who can write the file can rebuild
it. No other field is hashed.

Security-relevant actions are fsynced on each write. Clipboard, stream and
presence events are flushed by the next fsync or on close.

No code deletes, truncates or rotates the file. Size control is left to the
operator. On start, the runtime reads the whole file to find the last record.
`VerifyChain` can check the chain, but nothing calls it and there is no
command-line entry point for it.

Source: [`audit.go`](../../apps/runtime/internal/audit/audit.go).

## Logs

### Runtime log

The runtime writes JSON logs to stderr. The desktop app appends stdout and stderr
to `<data-dir>/runtime.log` without setting a file mode, so the file gets the
process default. The 0600 mode applies only when the runtime opens the file
itself with `--log-file`, which the desktop app does not pass. Nothing rotates
this file.

The runtime logger redacts in two ways:

- By key. Attributes named `password`, `secret`, `token`, `access_token`,
  `refresh_token`, `id_token`, `authorization`, `cookie`, `api_key`,
  `client_secret`, `ipc_secret`, `session_token` or `x-ntnx-prism-token`, or any
  name containing `secret`, `password` or `token`, become `***redacted***`.
- By value. Every string is scanned. Cookie and authorization headers, `Bearer`
  and `Basic` tokens, `NTNX_*_SESSION`, `JSESSIONID`, `PRISM_SESSION`,
  `igw_session` and `x-ntnx-prism-token` values become `[REDACTED]`.

Endpoint URLs and IDs, resource and session IDs, file paths, the audit actor
name and anything else that does not match these patterns are logged as is. Not
every log call was reviewed.

### Desktop log

electron-log writes `main.log` at level `info`. The console logs at `warn` in
production. Renderer log lines pass through `sanitizeLogField`, which replaces
control characters and caps length. This stops log forging but does not remove
secrets. Main-process log calls record the endpoint ID, cluster host and
certificate fingerprint, not cookie values.

### Agent log

| OS | Path |
| --- | --- |
| Windows | `%LOCALAPPDATA%\AHV Remote Console\hcbridge.log` |
| Linux | `$XDG_DATA_HOME/ahv-remote-console/hcbridge.log`, default `~/.local/share/ahv-remote-console/hcbridge.log` |

The file is append-only with mode 0600. A periodic task truncates it to the last
5 MiB. Clipboard messages record sizes, not text.

Source: [`logging.go`](../../apps/runtime/internal/logging/logging.go),
[`logSanitize.ts`](../../apps/desktop/src/main/logSanitize.ts).

## Telemetry

Runtime tracing is off by default. It starts only when `telemetry.enabled` is
true and an endpoint is set, or when `--otlp-endpoint` is given. The sample rate
defaults to 0.1. Spans go to the configured URL over OTLP/HTTP, and the URL is
logged at start.

Spans carry `plugin_id`, `session_id` and `resource_id` (the VM UUID). Error
spans carry the error text. No span found in the code carries clipboard text or
cookie values.

`packages/telemetry` has no network code. Its default sink discards events. The
renderer installs a sink that writes error events to the console only.

The desktop app, runtime and agent contain no crash reporter and no analytics
SDK.

Source: [`telemetry.go`](../../apps/runtime/internal/telemetry/telemetry.go),
[`packages/telemetry`](../../packages/telemetry/README.md).

## Diagnostics bundles

The operator starts an export from the menu or the tray. The renderer can also
start one through the `save-diagnostics` IPC handler. The desktop app opens a
save dialog with the default name
`ahv-remote-console-diagnostics-<timestamp>.zip`.

The runtime builds its part through a signed loopback call to
`GET /v1/diagnostics/bundle` and records `diagnostics.export` in the audit log.
It contains:

| File | Contents |
| --- | --- |
| `runtime-state.json` | Active sessions, plugin summaries, and a network snapshot with interface names, MAC and IP addresses |
| `config.json` | Config with `security.custom_ca_files` and every `pinned_cert_b64` cleared |
| `audit.log` | Last 256 KiB |
| `runtime.log` | Last 256 KiB |

The desktop app adds `main.log`, up to 4 MiB of `runtime.log`, and `about.txt`.
These are copied without the runtime's redaction. If the runtime is unreachable,
the desktop app writes a host-only bundle.

The app never uploads a bundle.

Source: [`diagnostics.ts`](../../apps/desktop/src/main/diagnostics.ts),
[`host.go`](../../apps/runtime/internal/plugins/host.go).

## Renderer storage

Local storage holds view state only:

| Key or store | Contents |
| --- | --- |
| Recent and favourite VMs | VM identifiers |
| `ahv-endpoint-meta` | VM count, last-connect time, last quality tier |
| `ahv-recent-vm-opens` | VM ID, VM name, endpoint ID, open time |
| `ahv-cluster-node-positions` | Node coordinates |
| `ahv-cluster-links` | Links between clusters |
| Folders | Custom folder map |
| Keyboard layout per VM | Layout code |
| Display fit per VM | Fit mode |

No cookie, token, password or clipboard text goes into these keys. IndexedDB and
sessionStorage are not used. The renderer holds SSO cookies briefly during
sign-in and the clipboard text it receives.

Source: [`recentStore.ts`](../../apps/desktop/src/renderer/app/recentStore.ts).

## Agent files in the guest

The agent makes no network calls. It talks to the host over a serial device.

| OS | Files |
| --- | --- |
| Linux | `~/.local/bin/ahv-remote-console-agent` (directory and binary mode 0700); systemd user unit `~/.config/systemd/user/ahv-remote-console-agent.service` (mode 0644) |
| Windows | `%LOCALAPPDATA%\AHV Remote Console\ahv-remote-console-agent.exe` and an HKCU Run value. The older name `hcbridge.exe` is recognised |

Both also write `hcbridge.log`. Run the agent with `-uninstall` to remove it. On
Linux this removes the unit and the binary. On Windows it removes the HKCU Run
value.

To mount the agent ISO, the runtime uploads the bundled ISO to the cluster Image
Service if it is not already there. The ISO holds no operator data.

## Outbound network connections

| Destination | Started by | What is sent | Controls |
| --- | --- | --- | --- |
| Prism REST over HTTPS | Runtime, AHV plugin | Cookie header; VM, image and task request bodies; ISO bytes | TLS 1.3 minimum, optional SPKI pin, custom CA files, optional proxy |
| Prism console WebSocket `wss://<endpoint>/vnc/vm/<uuid>/proxy` | Runtime, on operator request | Console stream, keystrokes, clipboard text | Same TLS settings; cookies on the upgrade |
| Prism and IdP pages | SSO window, on operator request | The operator's own sign-in traffic | Chromium certificate checks; cluster host pinned when a pin exists; permissions and popups denied |
| Update feed | Desktop updater, every 6 hours | GET of `latest*.yml` and a detached signature | Off unless `AHV_REMOTE_CONSOLE_AUTOUPDATE=1` and a feed URL are set; off in unpackaged builds |
| OTLP collector | Runtime | Trace spans | Off by default |
| External links | Operator click | The URL, handed to the OS | Only `https:` and `mailto:` |

The production content security policy allows connections to loopback only.
Renderer navigation outside the bundled page is blocked and handed to the OS.

Source: [`client.go`](../../plugins/ahv/internal/api/client.go),
[`autoUpdate.ts`](../../apps/desktop/src/main/autoUpdate.ts).

## Deleting data

| Data | How to remove it |
| --- | --- |
| Keychain cookies, all endpoints | Sign out |
| Keychain cookies, one endpoint | Removed automatically when Prism reports the session expired |
| Keychain cookies of a deleted endpoint | Sign out before you delete the endpoint. Deleting the endpoint alone leaves the entry |
| Any keychain entry | Remove entries under service `ahvRemoteConsole` in the OS credential manager |
| Endpoint records | Delete the endpoint in the UI, or call `DELETE /v1/config/endpoints/{id}` |
| `config.json` | Edit or delete the file. Defaults are written on the next start |
| Audit log | Delete the file by hand. Setting `security.audit_trail_enabled` to false only stops new records |
| Runtime and desktop logs | Delete the files by hand |
| Diagnostics bundle | Delete the zip |
| Agent and its log | Run the agent with `-uninstall`, then delete `hcbridge.log` by hand if needed |
| Linux open-at-login | Turn the setting off. The autostart file is removed |
| Renderer local storage | No in-app control. Remove the app's Chromium profile |
| SSO cookie partition | No in-app control. Remove the profile directory |
| Clipboard text | Nothing is persisted. Replaced when the clipboard changes |

> [!WARNING]
> Known gaps:
>
> - Deleting an endpoint leaves its Prism cookies in the keychain until sign-out.
> - The SSO cookie partition and renderer storage have no in-app clear.
> - The audit log and runtime log have no rotation or retention.
> - The plugin holds cookies as strings and only drops references. The configure
>   guide says it zeroes them.

## Open questions

These points are not settled by the code in this repository.

- The persistence scope of Windows Credential Manager entries.
- Store-level protection of keychain entries on each OS.
- Whether the SSO partition is encrypted on disk, and its path.
- The location of the desktop `main.log` and the Chromium profile directory.
- Whether the profile directory is ever cleared.
- How long the renderer keeps SSO cookies and received clipboard text.
- How long runtime proxy buffers hold a clipboard payload.
- The lifetime of agent clipboard buffers, including big-clipboard transfers.
- Whether an error string in a trace span can carry upstream response text.
- Whether `-uninstall` removes `hcbridge.log`.
- Whether the agent ISO is encrypted at rest in the Image Service.

## Related documentation

- [Threat model](threat-model.md)
- [Configuring endpoints](../operations/configure.md)
- [Clipboard](../architecture/clipboard.md)
- [Audit and logging](../architecture/audit-and-logging.md)
- [In-guest agent](../architecture/agent.md)
