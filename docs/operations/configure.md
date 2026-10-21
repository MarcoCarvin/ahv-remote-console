# Configuring endpoints

This page shows you how to add a Prism Central endpoint, trust its TLS certificate,
and sign in. It ends with reference tables for the configuration file and the
environment variables. It is for operators setting up the desktop app and engineers
who manage its configuration.

An **endpoint** is one Prism Central you connect through. Each endpoint has an `id`,
a `url`, a friendly `label`, an auth method, optional TLS settings and optional
grouping tags. The app has no identity of its own. You sign in with your Prism
credentials, and the endpoint records only where to sign in. It stores no password.

## Add an endpoint

1. In the connection manager, choose to add a cluster. The **Connect to Prism**
   panel opens.
2. Fill in the form:

   | Field                         | Required | Notes                                                  |
   | ----------------------------- | -------- | ------------------------------------------------------ |
   | **Prism Central URL**         | Yes      | Must start with `https://`. A trailing slash is removed. |
   | **Friendly name**             | No       | Defaults to the URL host name.                         |
   | **Customer**, **Environment** | No       | Grouping tags. They sort the cluster in the connection list and the map. |
   | **Advanced TLS**              | No       | Pinned certificate and the insecure-TLS option. See [Trust the cluster certificate](#trust-the-cluster-certificate). |

3. Save. An empty URL or a URL without a scheme is rejected with a message.
4. Sign in through the SSO window with whatever identity provider Prism uses: local
   account, AD/LDAP or SSO.

To edit an endpoint, open it in the same panel. Its saved sign-in is kept. If you
change the URL, you may need to sign in again.

### How a save reaches the runtime

The renderer is sandboxed and cannot reach the runtime directly.

1. The SDK's `upsertEndpoint` sends `POST /v1/config/endpoints`.
2. The main process signs the request because it is on the signing allowlist. It
   refuses anything that is not. See the [IPC reference](../reference/ipc.md).
3. The runtime requires `url`, `plugin_id` and at least one auth method.
4. `config.Manager.UpsertEndpoint` fills defaults, derives an `id` from the label or
   URL if none is given, validates the endpoint and saves it.
5. Every upsert and delete is written to the tamper-evident audit log.

Listing and deleting use the same signed path: `GET /v1/config/endpoints` and
`DELETE /v1/config/endpoints/{id}`.

## Trust the cluster certificate

By default the app checks the cluster certificate against the system's trusted
certificate authorities and requires TLS 1.3. If your machine already trusts the
Prism Central certificate, leave **Advanced TLS** alone.

Use **Advanced TLS** when the cluster has a self-signed or private certificate that
your machine does not trust. Pin the certificate. Pinning trusts exactly that
certificate's key, which is stronger than turning verification off.

To trust an enterprise CA for every endpoint instead, use
[`security.custom_ca_files`](#custom-ca-bundles).

### Pin a certificate

1. Get the cluster's leaf certificate as base64 DER. Port 9440 is the Prism default.
   Change it to match your URL.

   ```bash
   openssl s_client -connect <prism-host>:9440 -servername <prism-host> </dev/null 2>/dev/null \
     | openssl x509 -outform DER | base64
   ```

   From a `.pem` or `.cer` file, the base64 is the block between the `BEGIN` and
   `END CERTIFICATE` lines. Whitespace is allowed.

2. In **Advanced TLS**, paste it into **Pinned leaf certificate (base64)**.
3. Save.

On every handshake the app then checks that:

- the SHA-256 of the leaf's SubjectPublicKeyInfo (SPKI) equals the pin's,
- the leaf is inside its validity window,
- the leaf's host name matches the endpoint URL host.

A man-in-the-middle with a different key is rejected, even with an otherwise valid
certificate. Because the pin is the public key, a certificate rotation that keeps
the same key pair needs no new pin. A new key needs a new pin.

The config stores the full certificate in `pinned_cert_b64`. The runtime pins the
SPKI hash taken from it.

### First connection to a self-signed cluster

Use **Trust the certificate at first sign-in** under **Advanced TLS** when the
cluster has a self-signed certificate and you do not have its base64 certificate
to paste as a pin. The option sets `allow_insecure_tls` on the endpoint.

1. Save the endpoint with the option checked. The runtime refuses every connection
   to it until a pin is stored, so nothing connects without verification.
2. Sign in. The SSO window accepts the cluster certificate once, for the cluster
   host only, and captures it. External identity-provider hosts keep normal
   certificate checks.
3. The app shows the certificate's SHA-256 fingerprint. Check it with your cluster
   administrator.
4. Select **Trust and sign in**. The app stores the certificate as the pin, clears
   `allow_insecure_tls`, and only then sends the session to the runtime.
   Select **Cancel sign-in** to discard the session instead. Nothing reaches the
   runtime.
5. From then on the SSO window and the runtime both verify against the pin.

### A broken pin disables the endpoint

TLS fails closed. The runtime never falls back to the system roots when pinning is
misconfigured. The endpoint refuses every connection until you fix it.

| Configuration                                | Result                                                    |
| -------------------------------------------- | --------------------------------------------------------- |
| No pin, no insecure flag                     | Verified against system roots and custom CAs, TLS 1.3     |
| Valid pin                                    | Connects only if SPKI, validity and host name match       |
| `allow_insecure_tls` without a pin           | Waiting for first-use trust. Every handshake is refused until a pin is stored. |
| Pin that is not valid base64                 | Refused. Every handshake fails.                           |
| Pin that is base64 but not an X.509 certificate | Refused. Every handshake fails.                        |

At save time, a pin that decodes to fewer than 100 bytes is rejected as malformed.
A real DER certificate is usually over 300 bytes. A pin that decodes but is not a
certificate passes save but fails at connect time, so a bad pin never acts as no pin.

The proxy treats a pin mismatch or a bare insecure flag as a permanent failure. It
does not retry, so the connection is refused instead of looping.

Source: [`plugins/ahv/plugin.go`](../../plugins/ahv/plugin.go),
[`apps/desktop/src/main/sso-login.ts`](../../apps/desktop/src/main/sso-login.ts).

## Credentials

Credentials never touch disk and never appear in the configuration file.
Authentication is delegated to Prism.

- You sign in in the SSO window, a sandboxed `BrowserWindow` pointed at Prism's
  `/console/` login.
- The runtime probes Prism with the captured session cookies. Only on success does
  it store them in the OS keychain: Windows Credential Manager, macOS Keychain or
  Linux libsecret. The entry is `endpoint/<id>/sso_cookies_json`.
- Every REST call and the VNC WebSocket replay the cookies. Rotated `Set-Cookie`
  values, including from the WebSocket upgrade, are saved back to the keychain.
- The cookies are held in memory as Go strings. Logout drops the references but
  cannot overwrite the values, so they remain until garbage collection.
- Logout clears the keychain entry for every known endpoint, not only the loaded
  ones.

The app does not refresh sessions in the background. `AuthRefresh` is a no-op. When
a session expires, the console shows a `session_expired` error and lets you sign in
again from the console. See [Runtime proxy](../architecture/runtime-proxy.md).

## Configuration file

The runtime keeps its configuration as JSON at `<data-dir>/config.json`. The file is
written with mode `0600` in a `0700` directory.

| OS      | Data directory                                                    |
| ------- | ----------------------------------------------------------------- |
| Windows | `%APPDATA%\AHV Remote Console`                                    |
| macOS   | `~/Library/Application Support/AHV Remote Console`                |
| Linux   | `$XDG_DATA_HOME/ahvRemoteConsole`, default `~/.local/share/ahvRemoteConsole` |

### Endpoint keys

Each entry in `endpoints` has these keys:

| Key                  | Description                                                        |
| -------------------- | ------------------------------------------------------------------ |
| `id`                 | Unique, non-empty. Derived from the label or URL if blank on save. |
| `label`              | Friendly name                                                      |
| `plugin_id`          | The plugin that handles the endpoint                               |
| `url`                | An `https://` URL with a host. `http://` is rejected.              |
| `auth_methods`       | Only `cookie` (the SSO window). An empty list becomes `["cookie"]`. Legacy `oidc` and `basic` entries become `cookie` on load. |
| `pinned_cert_b64`    | The cluster's public certificate, base64 DER. Must decode to at least 100 bytes. |
| `allow_insecure_tls` | Trust the certificate at first sign-in. Without a pin, connections are refused until the pin is stored. |
| `tags`               | Grouping tags, for example `customer` and `environment`            |

The pinned certificate is public. The cluster presents it on every handshake, so it
is safe to store and to pass to the renderer, which pins the SSO window to the same
certificate. The pin and the custom CA paths are still removed from diagnostics
bundles.

### Security keys

| Key                        | Description                                                       |
| -------------------------- | ----------------------------------------------------------------- |
| `security.enforce_tls13`   | Require TLS 1.3. On by default.                                   |
| `security.custom_ca_files` | List of PEM bundle paths. See [Custom CA bundles](#custom-ca-bundles). |

### Network keys

| Key                | Description                                                          |
| ------------------ | -------------------------------------------------------------------- |
| `network.proxy_url` | Egress proxy for REST and the VNC WebSocket. Must parse as a URL.   |
| `network.no_proxy`  | Hosts or suffixes that bypass the proxy.                            |

When both are empty, the AHV plugin uses the `HTTP_PROXY`, `HTTPS_PROXY` and
`NO_PROXY` environment variables.

### Custom CA bundles

To trust an enterprise CA for every endpoint, list its PEM bundle paths in
`security.custom_ca_files`. The runtime adds them to the system roots for all
endpoints. The setting is global, not per endpoint, and the Add Endpoint panel does
not show it. Each path must be non-empty.

### Validation

`config.Endpoint.Validate` checks every endpoint, both on save and when the runtime
loads the file. `Config.Validate` also checks that IDs are unique.

- Validation reports every problem in one pass, with messages scoped to the field,
  for example `endpoints[2].url: …`.
- On save, `UpsertEndpoint` rejects an invalid endpoint.
- At startup, problems are logged at WARN and the runtime keeps running, so an
  unusual but working config does not lock you out.

Source: [`apps/runtime/internal/config/config.go`](../../apps/runtime/internal/config/config.go).

## Environment variables

### Auto-update feed (optional)

The desktop app reads these at startup. Auto-update is off unless you set them.

| Variable                               | Meaning                                                    |
| -------------------------------------- | ---------------------------------------------------------- |
| `AHV_REMOTE_CONSOLE_AUTOUPDATE`        | `1` turns the updater on. Any other value, or unset, leaves it off. |
| `AHV_REMOTE_CONSOLE_UPDATE_URL`        | Base URL of your generic update feed. The updater appends `latest.yml`, `latest-mac.yml` or `latest-linux.yml`. Unset means no check. |
| `AHV_REMOTE_CONSOLE_UPDATE_PUBKEY_B64` | Base64 Ed25519 release public key. Required on Linux: an update is refused unless `latest-linux.yml.minisig` verifies against it. Without it, Linux updates fail closed. |

The updater checks at startup and every 6 hours. It does nothing in dev, debug or
unpackaged builds. For a feed behind a private CA, see
[Auto-update feed](install.md#auto-update-feed).

Source: [`apps/desktop/src/main/autoUpdate.ts`](../../apps/desktop/src/main/autoUpdate.ts),
[`apps/desktop/src/main/updateVerify.ts`](../../apps/desktop/src/main/updateVerify.ts).

### Other variables

| Variable                            | Meaning                                                   |
| ----------------------------------- | --------------------------------------------------------- |
| `AHV_REMOTE_CONSOLE_AGENT_ISO_PATH` | Path of the agent ISO the runtime mounts. Default `build/agent.iso`. |
| `AHV_REMOTE_CONSOLE_DEV`            | Set to `1` by `make dev` when it starts Electron.          |
| `NODE_EXTRA_CA_CERTS`               | Set by the updater to the private update CA bundle.        |
| `APPDATA`, `XDG_DATA_HOME`          | Locate the data directory on Windows and Linux.            |
| `HTTP_PROXY`, `HTTPS_PROXY`, `NO_PROXY` | Egress proxy when `network.proxy_url` is empty.   |

## Troubleshooting

For TLS errors, expired sessions and diagnostics bundles, see
[Troubleshooting](troubleshooting.md).

## Related documentation

- [IPC reference](../reference/ipc.md)
- [AHV plugin](../architecture/ahv-plugin.md)
- [Runtime proxy](../architecture/runtime-proxy.md)
- [Architecture overview](../architecture/overview.md)
- [Update integrity](../architecture/update-integrity.md)
- [Installing and building](install.md)
