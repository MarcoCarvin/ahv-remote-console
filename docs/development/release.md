# Releasing

This page covers how a build becomes a signed installer, how a release is published,
and how auto-update works. It is for maintainers who cut releases. For the everyday
build and dev loop, see [Building from source](build.md).

The release path:

```text
make prod[-mac|-win|-linux]
  -> build runtime, plugin, agent ISO and icons
  -> stage the runtime where electron-builder expects it
  -> electron-builder packs the installer
       afterPack: flip Electron Fuses; on macOS, strip
                  quarantine and check the runtime
       afterSign: on macOS, notarize and staple
release.yml (on a v*.*.* tag)
  -> attaches installers to a GitHub Release
desktop app (if auto-update is enabled)
  -> checks the configured feed for a newer version
```

## Versioning

The version in `apps/desktop/package.json` is the version the installer reports and
the version an update feed is compared against. Each release must have a strictly
greater SemVer version than the one before. Do not add a prerelease suffix to a
release version, because a prerelease sorts below the plain version.

See [Versioning](versioning.md) for the full rules.

## Packaging

electron-builder reads [`apps/desktop/electron-builder.yml`](../../apps/desktop/electron-builder.yml).

| Setting | Value |
| --- | --- |
| App ID | `io.github.marcocarvin.ahv-remote-console` |
| Product name | `AHV Remote Console` |
| Artifact name | `ahv-remote-console-${version}-${os}-${arch}.${ext}` |

### Targets

| Platform | Targets | Architectures |
| --- | --- | --- |
| macOS | `dmg` | `arm64`, `x64` |
| Windows | `nsis` | `x64`, `ia32` |
| Linux | `AppImage`, `deb` | `x64`, `arm64` |

- **Windows** uses NSIS. MSI is not built because WiX fails under Wine on the Linux
  build host. The NSIS installer is not one-click. It installs per user
  (`perMachine: false`), allows elevation and a choice of install folder, and offers
  English and Italian.
- **Linux** ships AppImage and deb. RPM is not built because the packaging tool
  breaks on the space in the product name. On RHEL or Fedora, run the AppImage or
  repack the deb. The deb declares its GTK, NSS, Xss and Xtst dependencies.
- **macOS** builds one DMG per architecture, so both runtime architectures must be
  staged.

`make package-mac-dev` builds `arm64` only, and `make prod-win-local` builds Windows
x64 only. Both are local test builds. See [Building from source](build.md#packaging).

### Installer contents

The asar archive holds only `dist/**` and `package.json`, without source maps. Files
that must stay outside the asar ship as `extraResources`:

| Resource | Installed as | Purpose |
| --- | --- | --- |
| Runtime binary | `runtime/${os}-${arch}/ahv-remote-console-runtime` | Started by the main process |
| `agent.iso` | `agent.iso` | The agent ISO, read from `process.resourcesPath/agent.iso` |
| `THIRD_PARTY_NOTICES.md` | `THIRD_PARTY_NOTICES.md` | Licence notices for bundled dependencies |
| `build/icons/` | `icons/` | Dock, window and tray icons |

There is no plugin library to ship. The AHV plugin is linked into the runtime.

The configuration also has an `asarUnpack: [resources/runtime/**]` entry. It has no
effect, because nothing under that path is packed into the asar. The runtime is on
disk because it is an `extraResource`.

### Staging the runtime

electron-builder copies the runtime from `build/runtime/${os}-${arch}/`. The packaged
app starts a binary named exactly `ahv-remote-console-runtime`, with no OS or
architecture suffix.

`make stage-runtime` builds the runtime, clears the destination folder and copies the
binary there under that name. On macOS it builds both `arm64` and `x64`. `make package`
runs `build` and `stage-runtime` before electron-builder. The `prod*` targets also run
`install`, `agent-bins`, `iso` and the icon build first.

## Post-pack hardening

[`apps/desktop/scripts/after-pack.js`](../../apps/desktop/scripts/after-pack.js) runs as
the `afterPack` hook, before signing.

### Electron Fuses

On every platform, the hook sets these fuses on the packaged Electron binary. Because
it runs before signing, the signature covers the fuse state.

| Fuse | Value | Effect |
| --- | --- | --- |
| `RunAsNode` | `false` | `ELECTRON_RUN_AS_NODE` cannot relaunch the app as plain Node |
| `EnableNodeCliInspectArguments` | `false` | `--inspect` and debugger attach are disabled |
| `EnableNodeOptionsEnvironmentVariable` | `false` | `NODE_OPTIONS` is ignored |
| `OnlyLoadAppFromAsar` | `true` | Code loads only from the asar |
| `EnableCookieEncryption` | `true` | The cookie store is encrypted with an OS key |

Together these stop anyone from running arbitrary Node code under the app's signature
and entitlements, which would bypass the preload and the IPC signing allowlist. See
[Desktop app](../architecture/desktop.md) and the [IPC reference](../reference/ipc.md).

`EnableEmbeddedAsarIntegrityValidation` is not enabled. It interacts with the order of
notarization steps.

After setting the fuses on macOS, the hook applies an ad-hoc signature, so an unsigned
build still launches on Apple Silicon. A signed release is signed again after the
hook, which replaces the ad-hoc signature.

### macOS checks

On macOS the hook also:

1. Removes the `com.apple.quarantine` attribute from the app bundle. This avoids a
   first-launch XProtect scan. A missing attribute is not an error.
2. Sets the executable bit on
   `Contents/Resources/runtime/mac-<arch>/ahv-remote-console-runtime`. Failure stops
   the build.
3. Runs `codesign --verify` on the runtime. On an unsigned build this fails and is
   logged as a warning. A signed release is expected to pass.

## Signing

### Windows

The desktop installer has `signAndEditExecutable: true`. In `release.yml`, the
`desktop` job passes two repository secrets to electron-builder:

| Secret | Passed as |
| --- | --- |
| `WIN_CERT_BASE64` | `CSC_LINK` (base64 PFX) |
| `WIN_CERT_PASSWORD` | `CSC_KEY_PASSWORD` |

The Windows installer builds on the `windows-2022` runner. There is no separate
Windows post-sign hook.

The agent Makefile signs the Windows agent binaries with Authenticode through its
`sign-windows` target, which `all-platforms` runs. It signs only when `signtool.exe`
is on `PATH` and both `CODESIGN_CERT_PATH` and `CODESIGN_CERT_PASS_FILE` are set:

| Variable | Meaning |
| --- | --- |
| `CODESIGN_CERT_PATH` | `.pfx` or `.p12` file with the signing key |
| `CODESIGN_CERT_PASS_FILE` | File that holds the `.pfx` password |
| `CODESIGN_TIMESTAMP_URL` | RFC 3161 timestamp server. Default `http://timestamp.digicert.com` |
| `SIGNTOOL` | Path to `signtool.exe`. Default `signtool.exe` |

Without a certificate it prints a warning and continues. Unsigned agent binaries
trigger SmartScreen warnings in the guest.

### macOS

The `mac` block sets `hardenedRuntime: true` and `gatekeeperAssess: false`, and uses
`build/entitlements.mac.plist` for both `entitlements` and `entitlementsInherit`.

There are two entitlement profiles:

| Profile | File | Contents |
| --- | --- | --- |
| Production | [`build/entitlements.mac.plist`](../../build/entitlements.mac.plist) | `allow-jit`, `allow-unsigned-executable-memory`, `network.client`, `files.user-selected.read-write` |
| Development | [`build/entitlements.mac.dev.plist`](../../build/entitlements.mac.dev.plist) | The same, plus `disable-library-validation` |

The production profile does not disable library validation. The runtime is a
separately signed child process, not a library loaded into Electron, so library
validation can stay on.

With the hardened runtime on, library validation rejects the Electron Framework unless
it shares the app's Team ID. An unsigned build therefore needs the development
profile, and a Developer ID build must not use it.

`make package-mac` and `make prod-mac` refuse to run without a Developer ID
Application certificate in the keychain, or `CSC_LINK` or `CSC_NAME` set.
`make package-mac-dev` uses the development profile, turns off identity discovery
(`CSC_IDENTITY_AUTO_DISCOVERY=false`) and names the artifact `DEV-UNSIGNED`. It is
not for distribution.

### Notarization

[`apps/desktop/scripts/notarize.js`](../../apps/desktop/scripts/notarize.js) runs as
the `afterSign` hook. It does nothing unless these App Store Connect API values are
set:

| Variable | Meaning |
| --- | --- |
| `APPLE_API_KEY` | Path to the `.p8` key file |
| `APPLE_API_KEY_ID` | Key ID |
| `APPLE_API_ISSUER` | Issuer ID |

When they are set, it submits the app with `notarytool`, then staples the ticket with
`xcrun stapler` before the DMG is built. It loads `@electron/notarize` only in that
case, so a build without credentials does not need the module.

Stapling matters for this app. Without a stapled ticket, Gatekeeper checks the app
online at first launch, and that check stalls behind a full-tunnel VPN. A stapled app
is checked offline.

### macOS release gate

`make verify-mac-release` checks a packaged `.app` under `apps/desktop/release/`.
`make prod-mac` and `make package-mac` run it at the end. Run it on its own before you
ship an app built earlier. It fails unless:

1. `codesign --verify --deep --strict` passes.
2. A Team ID is present (`TeamIdentifier=[A-Z0-9]{10}`), so the app is not ad-hoc or
   self-signed.
3. The hardened runtime flag is present.
4. The entitlements do not include `disable-library-validation`.

To ship a macOS release, you need your own Developer ID Application certificate, in
the keychain or through `CSC_LINK` or `CSC_NAME`, and the three notarization values
above.

### Linux

AppImage and deb packages carry no OS-enforced publisher signature. Auto-update
verifies Linux update manifests separately. See [Linux update integrity](#linux-update-integrity).

## Release workflow

Pushing a tag that matches `v*.*.*` starts `.github/workflows/release.yml`.

| Job | Runs on | What it does |
| --- | --- | --- |
| `runtime` | `ubuntu-22.04` | Builds the runtime for `linux/amd64`, `linux/arm64`, `windows/amd64` and `darwin/arm64`, stamped with the tag as its version |
| `desktop` | `ubuntu-22.04`, `windows-2022` | Builds and packages the desktop app. Uploads installers and the `latest*.yml` update manifests |
| `publish` | `ubuntu-22.04` | Creates the GitHub Release with the desktop artifacts |

The release notes come from the `CHANGELOG.md` section whose heading matches the tag
without the `v`. If there is no such section, the workflow uses `[Unreleased]`. GitHub's
generated commit list is appended.

To cut a release:

1. Set the version in `apps/desktop/package.json`.
2. Add a `CHANGELOG.md` section for that version.
3. Push a tag such as `v1.2.3`.

## Auto-update

The desktop app uses electron-updater, wired in
[`apps/desktop/src/main/autoUpdate.ts`](../../apps/desktop/src/main/autoUpdate.ts). It
is off by default. No feed is built into the installer.

### Configuration

| Variable | Effect |
| --- | --- |
| `AHV_REMOTE_CONSOLE_AUTOUPDATE` | Set to `1` to enable auto-update |
| `AHV_REMOTE_CONSOLE_UPDATE_URL` | Base URL of the update feed. Required |
| `AHV_REMOTE_CONSOLE_UPDATE_PUBKEY_B64` | Ed25519 public key for Linux manifest checks. Empty by default |

The updater also skips:

- dev builds (`AHV_REMOTE_CONSOLE_DEV=1`) and debug builds
  (`AHV_REMOTE_CONSOLE_DEBUG=1`);
- unpackaged runs, because `app-update.yml` exists only in a packaged installer.

Before you enable it, two conditions must hold:

1. The app is signed. electron-updater will not apply an update to an unsigned app on
   macOS or Windows.
2. Release versions increase strictly. Otherwise the feed's version is not newer and
   no update is found.

### Feed layout

The updater uses the `generic` provider with channel `latest`. It appends the
per-platform manifest to the feed URL and downloads the artifact it names:

| Platform | Manifest |
| --- | --- |
| Windows | `latest.yml` |
| macOS | `latest-mac.yml` |
| Linux | `latest-linux.yml` |

### Behaviour when enabled

- Before any update request, it sets `NODE_EXTRA_CA_CERTS` to an operator CA bundle,
  if one exists, so a feed signed by a private CA is trusted. Packaged builds use
  `process.resourcesPath/certs/update-ca-bundle.pem`. Source runs use
  `scripts/certs/update-ca-bundle.pem` at the repository root.
- It downloads updates automatically and installs them on quit.
- It checks once at start, then every 6 hours. The timer does not keep the process
  alive.
- Errors never block or crash startup. An unreachable feed is logged as a warning.
- When an update is downloaded, it shows a **Restart now** / **Later** dialog.
  **Later** installs on the next quit.
- It sends update events only to trusted app windows, never to the SSO window.

### Linux update integrity

Before it applies a Linux update, the app downloads `latest-linux.yml` and
`latest-linux.yml.minisig`. It verifies the Ed25519 minisign signature over the
manifest with the key in `AHV_REMOTE_CONSOLE_UPDATE_PUBKEY_B64`.

> [!WARNING]
> This check fails closed. If no key is set, or the manifest or signature cannot be
> fetched or verified, the update is refused. With the default empty key, Linux
> updates are always refused.

The signature covers the manifest, which names the AppImage URL and hash.
electron-updater then checks the payload's `sha512`. See
[Update integrity](../architecture/update-integrity.md).

## Known limitations

- The public workflows build no macOS package and do not sign or notarize one. The
  release workflow builds only a `darwin/arm64` runtime.
- No Developer ID certificate is configured by default. Local macOS builds are
  unsigned development builds.
- The public workflows do not run the Playwright tests against the packaged app.
- The Linux update signature check is covered by unit tests of the verification
  logic. It has not been tested end to end with a real key and a published
  `.minisig`.

## Related documentation

- [Building from source](build.md)
- [Testing and CI](testing.md)
- [Versioning](versioning.md)
- [Update integrity](../architecture/update-integrity.md)
- [Desktop app](../architecture/desktop.md)
- [IPC reference](../reference/ipc.md)
- [Documentation index](../README.md)
