# Update integrity

This page explains how the desktop app decides to accept or refuse an in-app
update. It covers the opt-in gate, the update feed, the checks electron-updater
makes, the extra signature check the app runs on Linux, and the gaps in that
path. It is for developers and for operators who run an update feed.

## How it works

Auto-update is off unless you turn it on. When it is on, electron-updater
downloads the update from a feed you host and installs it. On macOS and
Windows, the OS code-signing check decides whether the publisher is trusted.
Linux has no such check for AppImages, so the app checks an Ed25519 signature
over the feed manifest itself, and refuses the update when the check fails or
no key is set.

```text
 feed (your server)
   latest-linux.yml  ----> electron-updater: download, sha512 check
   latest-linux.yml.minisig
          |                      |
          v                      v
   app: Ed25519 check  ---> pass: show install dialog
                            fail: log error, no dialog
```

The repo has the verifier but not the signer. Creating the `.minisig` file is a
release step outside this repo.

## Opt-in gate

`initAutoUpdate()` checks these conditions in order. The first one that fails
stops it, and no updater is created.

| Order | Condition | When it fails |
| --- | --- | --- |
| 1 | `AHV_REMOTE_CONSOLE_AUTOUPDATE` is exactly `1` | logs "disabled (default)" |
| 2 | the app is packaged and not in debug mode (`AHV_REMOTE_CONSOLE_DEV=1` or `AHV_REMOTE_CONSOLE_DEBUG=1` means debug) | logs "skipped" |
| 3 | `AHV_REMOTE_CONSOLE_UPDATE_URL` is set | logs "no feed" |

The flag is off by default because auto-update needs two things the code does
not test: signed builds, and a release version that sorts above the installed
one. The flag is the only control.

Once the gate passes, the updater runs at startup and every six hours. The
timer does not keep the app alive and is cleared on quit. Every entry point
catches its errors, so an unreachable feed is logged and does not block
startup.

Setting these variables is described in
[Configuring](../operations/configure.md#auto-update-feed-optional).

Source: [`apps/desktop/src/main/autoUpdate.ts`](../../apps/desktop/src/main/autoUpdate.ts)

## Feed URL

- The app reads `AHV_REMOTE_CONSOLE_UPDATE_URL` from the process environment.
  No feed URL is built into the installer; `electron-builder.yml` has no
  `publish:` block.
- electron-updater gets it as a generic provider with channel `latest`. It
  appends the platform manifest name: `latest.yml`, `latest-mac.yml` or
  `latest-linux.yml`.
- The Linux verifier builds its own URLs from the same base, with any trailing
  `/` removed: `<base>/latest-linux.yml` and `<base>/latest-linux.yml.minisig`.
- The release workflow does not publish a feed. It attaches the installers and
  `latest*.yml` files to a GitHub Release. You host your own feed.

## What electron-updater checks

The app uses electron-updater 6.8.9. The points below come from reading the
library's code.

- Payload digest. The library compares the sha512 of a cached download with
  the sha512 in the manifest, and builds a digest check for differential
  downloads from the same value.
- Version. It offers only a version greater than the installed one by
  semver.
- Windows signature. `NsisUpdater` checks the installer signature against a
  `publisherName` from the installed app config, when that name is set.
- macOS signature. Squirrel.Mac refuses an unsigned app, according to the
  app's code comment.
- Linux AppImage. `AppImageUpdater` applies the AppImage and checks no
  publisher signature.

The app relies on the library for the payload digest, and on Windows and macOS
for the signature check. It does not repeat either.

electron-updater uses Node's TLS stack. Before the first check, the app points
`NODE_EXTRA_CA_CERTS` at an operator CA bundle if that file exists. Without it,
a feed signed by a private CA fails TLS and the error is logged.

## Linux manifest signature

### What is signed

The raw bytes of `latest-linux.yml` are signed. The signature is a detached
minisign-format file, `latest-linux.yml.minisig`. The AppImage itself is not
signed by this scheme. The manifest carries its sha512, which electron-updater
checks.

### Key format

- The public key is a raw 32-byte Ed25519 key, base64-encoded.
- The app wraps it in SPKI DER by adding the standard 12-byte Ed25519 SPKI
  prefix.
- A key that is not exactly 32 bytes is rejected.

### Signature file format

`parseMinisig()` reads the file as LF-separated lines:

| Line | Contents | Used |
| --- | --- | --- |
| 0 | untrusted comment | no |
| 1 | base64 of `algorithm[2] \|\| key_id[8] \|\| ed25519_sig[64]`, exactly 74 bytes | yes |
| 2 | trusted comment | no |
| 3 | signature over the trusted comment | no |

The algorithm must be the ASCII string `Ed`. The prehashed variant `ED` is
refused. The signature is bytes 10 to 74 of the decoded line.

### Verification steps

`verifyUpdateManifest(manifest, minisigContents, publicKeyB64)` returns false
at the first failure:

1. The key is empty.
2. The key does not decode from base64.
3. The signature file has a structural error.
4. `verifyEd25519` fails. The key must be 32 bytes and the signature 64 bytes,
   and Node's `crypto.verify(null, manifest, key, signature)` must succeed. Any
   exception counts as a failure.

Unit tests sign a test manifest with a generated key and check acceptance,
tampering, a wrong key and a missing key.

Source: [`apps/desktop/src/main/updateVerify.ts`](../../apps/desktop/src/main/updateVerify.ts),
[`apps/desktop/src/main/__tests__/updateVerify.test.ts`](../../apps/desktop/src/main/__tests__/updateVerify.test.ts)

### On failure

`verifyLinuxUpdateSignature()` returns false when:

- no key is set
- no feed URL is set
- either fetch fails or returns a non-2xx status
- the signature is bad
- anything throws

`onUpdateDownloaded()` then logs an error and returns. The renderer gets no
`update-downloaded` event, no dialog appears, and this code does not call
`quitAndInstall()`.

### No key, no update

`AHV_REMOTE_CONSOLE_UPDATE_PUBKEY_B64` is empty by default. A Linux build without
it never shows the install dialog. This rule has a gap; see
[Install on quit skips the Linux check](#install-on-quit-skips-the-linux-check).

## Platform behaviour

| Platform | Installer | App check | Library or OS check |
| --- | --- | --- | --- |
| Linux x64, arm64 | AppImage (updates), deb | Ed25519 manifest check, fails closed | sha512 of the payload |
| macOS arm64, x64 | dmg | none | Squirrel.Mac refuses unsigned apps |
| Windows x64, ia32 | NSIS exe | none | publisher check, when a publisher name is configured |

The app's own check runs only when `platform() === 'linux'`.

Signing in the release builds, as configured in the repo:

- macOS. The `afterSign` notarization step does nothing without Apple API
  credentials. The public release workflow has no macOS desktop job.
  `make package-mac` refuses to build without a Developer ID identity.
- Windows. The release workflow passes `CSC_LINK` and `CSC_KEY_PASSWORD` from
  the `WIN_CERT_BASE64` and `WIN_CERT_PASSWORD` secrets.
  `signAndEditExecutable: true` is set in `electron-builder.yml`.

## Key lifecycle

| Step | Where it happens |
| --- | --- |
| Create the Ed25519 key pair | outside the repo; code comments name `minisign -G` or libsodium |
| Store the secret key | not in the repo, and not described in it |
| Sign `latest-linux.yml` and publish the `.minisig` | not in the repo; the release workflow uploads `latest*.yml` with no signing step |
| Give the app the public key | environment variable `AHV_REMOTE_CONSOLE_UPDATE_PUBKEY_B64`, read once when `updateVerify.ts` loads; no build step injects it |
| Check the key | only the empty-key check; no fingerprint or key ID comparison |

The key is a runtime setting, not a value built into the app. As a result:

- Whoever can set the app's environment can set the trusted key and the feed
  URL together. The code does not pin a key fingerprint.
- Changing the key means changing the environment of every installed app. The
  code has no key ID check and no rotation procedure.

No script in `scripts/` and no Makefile target signs a manifest. The only files
that mention `minisig`, `minisign` or `ed25519` are `autoUpdate.ts`,
`updateVerify.ts`, its test, and the configure, install and release docs.

## Known gaps

Each of these comes from reading the code and can be checked with a test.

### Install on quit skips the Linux check

> [!WARNING]
> On Linux, a downloaded update that fails the signature check can still be
> installed when the app quits.

`autoInstallOnAppQuit` is `true`. electron-updater registers its own quit
handler, which calls `install()` when an installer path and download info are
set. Neither depends on the app's check. A failed check only stops the dialog
and the renderer event. A fix needs `autoInstallOnAppQuit = false` on Linux
until the check passes, or a check inside the quit path.

### The manifest is fetched twice

electron-updater fetches `latest-linux.yml` to get the AppImage URL and sha512.
The verifier fetches it again. The check covers what the verifier read, not
what electron-updater used. A feed that returns different bytes the second time
passes.

### Only one signature is checked

The trusted-comment signature on line 3 is not read, and the key ID on line 1 is
not compared with the key. The check is a single signature over the manifest
bytes.

### The manifest fetch uses Chromium networking

`httpGet` uses `electron.net`, not Node. Node honours `NODE_EXTRA_CA_CERTS` and
Chromium does not. A private-CA feed can therefore fail the manifest fetch and
the update is refused. This was not tested.

### No freshness check

A signed manifest stays valid for any version. The only version rule is the
library's semver check. An old manifest with a valid signature is not rejected.

### No size limit on the fetch

`httpGet` keeps every chunk of the manifest and signature responses.

### Windows publisher check is not configured

`electron-builder.yml` sets no `publisherName`, and no file in the repo does.
Whether the Windows check runs in a released build depends on what
electron-builder writes at package time. This was not verified.

### Not verified

- The full-download path in electron-updater was not traced, so this page does
  not claim that every download is digest-checked.
- The macOS signature check in `MacUpdater` was not read. The claim rests on
  the app's code comment.

## Threats

| Threat | Control | Gap |
| --- | --- | --- |
| Feed or proxy serves a malicious AppImage | Ed25519 manifest check; payload sha512 from the manifest | install on quit; two fetches |
| TLS interception with a private CA | operator CA bundle for Node TLS; manifest signature on Linux | macOS and Windows have only transport and OS checks |
| Feed serves an old, signed, vulnerable build | library semver rule | no freshness or minimum-version check |
| Trusted key changed through the environment | OS process isolation only | no fingerprint pin |
| Unsigned build auto-updates and fails silently | off by default | signing is not tested by code |
| Update events reach an untrusted window | sent only to trusted app windows | none seen |
| Feed outage | all errors caught and logged | none seen |
| Dev or unpackaged build reaches a feed | debug and packaged gates | none seen |
| Large or hostile manifest response | none | no size limit |
| Key lost or leaked | none in the repo | storage and rotation not described |

## Related documentation

- [Configuring the update feed](../operations/configure.md#auto-update-feed-optional)
- [Installing](../operations/install.md)
- [Release process](../development/release.md#auto-update)
- [Threat model](../security/threat-model.md)
