# Threat model

This page describes the security model of AHV Remote Console as the code
implements it. It is for reviewers and operators who need to know what the app
protects, against whom, and where the protection stops. Where the code does not
enforce a control that a comment or design document claims, the item is listed
under [Known gaps](#known-gaps).

## Scope and assets

In scope:

- The desktop app (`apps/desktop`): main process, sandboxed renderer, preload
  bridge, and the SSO window.
- The runtime (`apps/runtime`): loopback HTTP and WebSocket broker, RFB/VNC
  proxy, audit log, and logging.
- The AHV plugin (`plugins/ahv`): Prism REST and console WebSocket client, TLS
  pinning, and credential storage.
- The agent (`apps/ahv-remote-console-agent`): clipboard and display bridge over
  the guest serial port.

Out of scope: Prism Central and the AHV cluster software, the identity provider,
the guest and host operating systems, and the build and signing pipeline.

| Asset | Where it lives | Protection |
| --- | --- | --- |
| IPC HMAC secret (48 random bytes) | Main process and runtime memory | Never on disk, in argv, in env or in the renderer (T1) |
| Prism session cookies | OS keyring; SSO partition | Keyring only, no file fallback (T14) |
| Endpoint configuration and pins | Runtime config file | Validated on write (T6) |
| Clipboard text, host and guest | Renderer, main process, agent | Sanitized, size-capped, not logged (T10, T12) |
| Console stream and VM control | Runtime, Prism | Signing allowlist, loopback, HMAC (T2, T3) |
| Audit log | `audit.log` in the runtime data directory | Mode 0600, hash chain (T13) |
| Diagnostics bundle | Exported zip | Config redaction, log tails (T15) |

## Trust boundaries

```text
 Workstation, one OS user session
 +-------------------------------------------------------------+
 |  Renderer (sandboxed)              SSO window              |
 |  UI, VNC client, clipboard         Prism and IdP pages     |
 |      | preload bridge, invoke only     | own partition,    |
 |      |                                 | no preload        |
 |  ----+---- TB1: renderer -> main ------+-----------------  |
 |      v                                                     |
 |  Main process  (signs requests, keeps the secret)          |
 |      |  TB2: secret over stdin once; runtime.json (0600)   |
 |      v                                                     |
 |  Runtime on 127.0.0.1:<port>                               |
 |      loopback + Host check + HMAC, nonce, timestamp        |
 |      OS keyring (cookies), audit.log (0600)                |
 +------|------------------------------------------------------+
        |  TB3: TLS 1.3, SPKI pin or system roots
        v
   Prism Central / AHV cluster --- console WebSocket ---+
                                                         v
                     Guest VM: agent over serial  <-- TB4 (untrusted)
```

| Boundary | Separates | Enforcement |
| --- | --- | --- |
| TB1 | Renderer from main process | Privileged IPC handlers call `assertTrustedSender` first |
| TB2 | Desktop app from runtime | The secret crosses once, over stdin |
| TB3 | Runtime and SSO window from Prism | TLS with optional SPKI pin (T7) |
| TB4 | Host from guest | All VNC clipboard and serial input is untrusted (T10, T11) |

## Threat actors

| Actor | Assumed capability | Expected outcome |
| --- | --- | --- |
| Malicious guest VM | Controls VNC frames, clipboard payloads and every byte on the serial channel | Cannot reach the host clipboard unsanitized, exhaust memory, or reach the runtime (T10, T11) |
| Network attacker on the Prism path | Reads and modifies traffic, presents certificates | Cannot read credentials or the console stream once a pin is set. First connect to a self-signed cluster is exposed (T7) |
| Compromised renderer | Arbitrary JavaScript, for example via XSS or a bad dependency | Cannot get the secret or a privileged bridge. Can still use the signing oracle for allowlisted calls (T1, T2, T6, T9) |
| Malicious SSO content | Whatever Prism and IdP pages serve, in an isolated partition | Cannot reach IPC or broadcasts, or gain device permissions (T8) |
| Local unprivileged user | Another OS account on the same workstation | Cannot read keyring entries, the handshake file, the audit log or the secret (T16) |
| Operator error | Accepts a wrong fingerprint, enables insecure TLS, shares a bundle | Mitigated where the code can refuse the unsafe combination (T17) |

Same-user malware is outside the model. It can read the keyring, inspect
runtime memory and attach a debugger. A compromised hypervisor or host kernel is
also outside the model.

## Threats and controls

"Gap" in the residual-risk column points to [Known gaps](#known-gaps).

| ID | Threat | Controls | Residual risk |
| --- | --- | --- | --- |
| T1 | Renderer obtains the IPC secret | Secret generated in main; `Signer` holds a copy in a closure and zeroes it on `wipe()`; passed to the runtime over stdin as hex; bridge returns only signature headers or subprotocol | Secret is not readable. Signatures for allowlisted calls are (T2) |
| T2 | Compromised renderer uses the signing oracle | Sender check on both sign handlers; method and path allowlist with root-relative URI, no `..`, exact method; signing refused until runtime ready; diagnostics signed by main only | Allowlist limits paths, not parameters. See note |
| T3 | Forged or replayed runtime requests | Loopback peers only; HMAC-SHA256 over a canonical request, constant-time compare; timestamp within 60 s; nonce reuse rejected for 120 s; 8 MiB body cap; same scheme on WebSocket upgrades | Nonce recorded before MAC check. Gap G10 |
| T4 | DNS rebinding or cross-origin pages reach the runtime | `Host` must be `127.0.0.1`, `localhost` or `[::1]`; CORS allows only `file://` and the Vite dev origins, `null` gets nothing; responses `no-store` and `nosniff` | `/healthz`, `/readyz`, `/metrics`, `/version` skip HMAC by design. They expose no secrets |
| T5 | Spoofed runtime or handshake | Handshake file `O_EXCL`, mode 0600, directory 0700 when created; stale file deleted before spawn; file carries `base64url(sha256(secret))` for main to compare | Mismatch does not stop signing. Gap G1 |
| T6 | Renderer repoints an endpoint or pins an attacker certificate | Runtime validates writes: `https` only, pin must decode, `allow_insecure_tls` needs a pin; SSO window uses the stored endpoint | Stored endpoint is writable through the oracle. Gap G9 |
| T7 | Network attacker on the Prism path | Leaf SPKI must match the pinned SHA-256, plus expiry and hostname checks; bad pin or insecure-without-pin fails every handshake; system roots when unpinned; all three runtime TLS users share the config; SSO window pins the cluster host | First sign-in to a self-signed cluster trusts any certificate. See note |
| T8 | Malicious content in the SSO window | Partition per endpoint; no preload, sandbox, context isolation, no node integration, web security on; permissions denied; `window.open` denied; never a trusted sender, no broadcasts; cookies read from its own partition only | Every partition cookie reaches the renderer. Gap G7. Partition never cleared |
| T9 | Compromised renderer navigates, opens windows or loads outside content | Strict production CSP with loopback-only `connect-src`; `X-Frame-Options: DENY`, `nosniff`, `no-referrer`, restrictive Permissions Policy; permissions denied; `window.open` to system browser; navigation guards; external links only `https:` and `mailto:`; same guards on pop-outs | CSP on `file://` not verified. Gap G3. Prefix-match navigation guard. Gap G4 |
| T10 | Guest clipboard text executes or injects on the host | Control, bidi, zero-width and Unicode line-separator characters removed both ways; trailing line breaks stripped; size caps; host pasteboard writes checked | Interior newlines and printable text pass. No content inspection. See note |
| T11 | Oversized or malformed guest traffic exhausts memory | 8 MiB client WebSocket limit, 160 MiB upstream; 16 MiB agent reassembly with stall timeout; decompressed big-clipboard cap; 16 KiB presence limit; 8 MiB renderer bodies | One message per session can reach these caps. Total memory not verified |
| T12 | Clipboard plaintext reaches logs or the audit log | Renderer audit type has no text field; logs carry length, direction and a 12-character hash prefix; agent logs byte counts; renderer log text cleaned and capped; audit `Reason` scrubbed | Clipboard audit records are never written. Gap G2. Text stays in main memory. Gap G11 |
| T13 | Audit records tampered with or deleted | Mode 0600, `O_APPEND`; each record holds SHA-256 of the previous line; `VerifyChain`; security actions fsynced | Tamper-evident only. Write errors do not fail the operation. Gap G12 |
| T14 | Credentials at rest | OS keyring only, no file fallback; per-endpoint key; cookie encryption Fuse on | In-memory cookies cleared by GC only. SSO partition persists, on-disk state not verified |
| T15 | Secrets reach logs or diagnostics | Runtime logger redacts sensitive keys and values; upstream rejection bodies not logged; bundle redacts CA paths and pins, caps log tails at 256 KiB; host logs mode 0600 | Unknown keys not caught. No scrubber on the main-process log. Gap G6 |
| T16 | Local unprivileged user reads runtime state | Handshake, audit and log files 0600; data directories 0700 when created; secret never in argv or env; keyring is per OS user | Loopback port open to all users, exempt endpoints only. See note |
| T17 | Operator error on TLS and trust | An insecure endpoint without a pin refuses every connection; the fingerprint is confirmed before the pin is saved and before the session reaches the runtime; SSO TLS decision from the stored endpoint | First-use trust still depends on the operator checking the fingerprint out-of-band |
| T18 | Debug or developer switches left on in a release | DevTools only in debug or dev builds; desktop app uses `--ipc-secret-stdin`, not `--ipc-secret-env`; Fuses disable `RunAsNode`, `NODE_OPTIONS`, `--inspect` and force asar | Several build checks only warn. See note |

### Notes

- T2. A compromised renderer can call allowlisted operations with bodies it
chooses. These include `POST /v1/vm/power`, snapshot restore, ISO mount and
eject, endpoint upsert and delete, and session open and close. It can also read
the clipboard. The model accepts this. The renderer is trusted for UI actions,
and the controls aim to stop escalation beyond it.

- T3. Unsigned requests with a fresh timestamp can fill the nonce cache. Under
pressure the cache evicts the oldest quarter, which shortens the replay window
for a captured request. A capture needs loopback sniffing privileges that a
local unprivileged user does not have.

- T7. On first sign-in to a self-signed cluster, the SSO window accepts any
certificate for the cluster host and records it. The operator confirms the
fingerprint afterwards, so a man-in-the-middle at first contact can capture that
sign-in. Once a pin is set, a key change fails closed. The pin is an SPKI hash,
so rotation with the same key still works. Egress proxies (`proxyConfig`) are
not covered.

- T8. The SSO window follows IdP redirects and has no navigation guard, by
design. Sign-in resolves with every cookie the partition holds for the Prism
URL, not only session cookies. The renderer receives them and forwards them to
the runtime. Nothing calls `clearStorageData` on the `persist:` partition.

- T10. A guest can place a plausible command on the host clipboard. The
trailing-newline strip only prevents immediate execution on paste. The host
clipboard is shared with every application.

- T11. Several sessions can each hold a maximum-size message. A hostile Prism
endpoint is the source of the 160 MiB upstream case.

- T15. Redaction matches keys and known header and cookie shapes. A bare
secret under an unknown key passes. The diagnostics bundle adds host log files
without a second pass. Whether any main-process call writes a cookie is not
verified.

- T16. The per-user NSIS install leaves the runtime binary writable by the
installing user. The code does not change permissions on a data directory that
already exists.

- T17. The runtime refuses an insecure, unpinned endpoint. Because cookies are
sent before the pin is saved, the first sign-in to a self-signed cluster may
fail. This was not verified at runtime.

- T18. A build where the binary path does not match is skipped with a warning,
not a failure. Embedded asar integrity validation is off. The runtime binary
sits in `extraResources`, outside the asar, so that check would not cover it.
Codesign verification only warns.

Source: [`main.ts`](../../apps/desktop/src/main/main.ts),
[`signer.ts`](../../apps/desktop/src/main/signer.ts),
[`signable.ts`](../../apps/desktop/src/main/signable.ts),
[`trustedSenders.ts`](../../apps/desktop/src/main/trustedSenders.ts),
[`runtime-manager.ts`](../../apps/desktop/src/main/runtime-manager.ts),
[`sso-login.ts`](../../apps/desktop/src/main/sso-login.ts),
[`after-pack.js`](../../apps/desktop/scripts/after-pack.js),
[`ClipboardEngine.ts`](../../apps/desktop/src/renderer/clipboard/ClipboardEngine.ts),
[`server.go`](../../apps/runtime/internal/ipc/server.go),
[`audit.go`](../../apps/runtime/internal/audit/audit.go),
[`config.go`](../../apps/runtime/internal/config/config.go),
[`plugin.go`](../../plugins/ahv/plugin.go),
[`agent.go`](../../apps/ahv-remote-console-agent/internal/vdagent/agent.go).

## Non-goals

- Protection against same-user malware, debuggers or memory inspection.
- Protection against a compromised Prism, AHV cluster or identity provider. The
  app delegates identity to Prism and trusts its authorization decisions.
- Protection against a compromised hypervisor, host kernel or physical attacker.
- Clipboard confidentiality against other host applications. The OS clipboard
  is shared by design.
- Content inspection of clipboard text. Sanitization removes invisible and
  control characters only.
- Integrity of the update channel. `updateVerify.ts` exists but was not reviewed
  for this model.
- Release signing and notarization. The desktop architecture page says macOS
  release signing is not configured in this repository. Not verified.
- Egress proxy behaviour and the guest's own network.

## Known gaps

Each gap is a control that a comment or design document claims and the code does
not enforce, or enforces only in part. They are in order of impact.

| ID | Claim | What the code does |
| --- | --- | --- |
| G1 | On a handshake secret mismatch, main fails closed and stops the runtime | Main calls `stop()` and broadcasts `error`, but the runtime manager is already `ready` with the handshake cached. `isReady()` stays true, so signing and `fetchRuntimeEndpoints` continue against the handshake endpoint. Anyone who can write `runtime.json` could point those requests at their own server. The spawn secret is wiped only on success |
| G2 | Clipboard transfers are audited with direction, length and hash | Nothing emits `clipboard.*` or `bridge.clip_*` from the renderer or runtime. `ConsoleView` creates the engine without an audit emitter, and the agent-audit route accepts bridge actions only |
| G3 | The strict CSP applies to every response | The CSP is set only through `webRequest.onHeadersReceived`. The production page loads from `file://` and `index.html` has no `<meta>` CSP. Whether the header reaches `file://` is not verified |
| G4 | Navigation is limited to the Vite origin and the bundled `index.html` | `startsWith('http://localhost:5173')` also matches `http://localhost:5173.<other-host>`. Any `file://` URL passes |
| G5 | Every privileged handler calls `assertTrustedSender` first | `get-runtime-state` and `restart-runtime` do not. The SSO window has no preload, so it cannot reach them today |
| G6 | Renderer log lines are sanitized | Sanitization strips control characters only and does not redact secrets. Main-process logging has no value scrubber |
| G7 | Session credentials are captured and kept in main | Sign-in resolves with all cookies for the Prism URL, and they reach the renderer |
| G8 | Closed: cookies used to reach the runtime before fingerprint confirmation | The renderer now holds the captured session until you trust the certificate; cancelling discards it |
| G9 | A compromised renderer cannot aim the credential-bearing SSO window at an attacker host | The stored endpoint is writable through the signing oracle and accepts a new URL and pin. The renderer can then receive the operator's next credentials. The UI shows no out-of-band confirmation of the change |
| G10 | The nonce cache is an anti-replay control | The nonce is recorded before the MAC is verified. Unsigned requests fill the cache, and eviction shortens the replay window |
| G11 | Clipboard text is wiped from memory after use | Main keeps `lastClipText` until the next copy. Agent wiping is not verified |
| G12 | The audit log records every security-relevant operation | Clipboard and bridge clip records are absent (G2). Append errors are logged, not returned |

## Verification status

- Controls were checked against the source. No tests were run for this page.
- The Electron behaviours in T9 and T14 depend on the Electron version and were
  not run.
- Tests under `apps/runtime/internal/ipc` and `apps/desktop/src/main/__tests__`
  cover some controls and were not read in full.

## Reporting a vulnerability

Report vulnerabilities through [SECURITY.md](../../SECURITY.md). Do not open a
public issue.

## Related documentation

- [Data handling](data-handling.md)
- [Desktop architecture](../architecture/desktop.md)
- [Runtime and proxy](../architecture/runtime-proxy.md)
- [Clipboard](../architecture/clipboard.md)
- [Audit and logging](../architecture/audit-and-logging.md)
- [IPC reference](../reference/ipc.md)
