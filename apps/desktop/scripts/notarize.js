#!/usr/bin/env node
/* eslint-disable */
//
// electron-builder afterSign hook — macOS notarization + stapling (VEN-02).
//
// WHY: a signed-but-NOT-notarized app has no local notarization ticket, so on
// first launch macOS (syspolicyd/Gatekeeper) performs an ONLINE cdhash check
// against Apple's notary service before it will run. Under this app's
// full-tunnel VPN that round-trip stalls, which is the "took a while before
// actually starting" report. A *notarized + stapled* app carries the ticket
// locally, so Gatekeeper validates offline and launch is instant — and no
// quarantine-strip workaround is needed.
//
// SAFE BY DESIGN: this hook is a NO-OP unless the Apple App Store Connect API
// credentials are present in the environment, and it only `require()`s
// @electron/notarize inside that credentialed branch. So credential-less builds
// (local dev, the non-release CI) never notarize and never fail here — they
// just print a notice. A release build that HAS the creds notarizes and staples;
// if that legitimately fails, the build fails (we must not ship un-notarized
// while claiming otherwise).
//
// REQUIRED CI SECRETS (set on the macOS release runner):
//   APPLE_API_KEY     — filesystem PATH to the App Store Connect API key (.p8)
//   APPLE_API_KEY_ID  — the key's ID (the "AuthKey_<ID>.p8" <ID>)
//   APPLE_API_ISSUER  — the issuer UUID from App Store Connect → Users and Access → Keys
// The build must also sign with a "Developer ID Application" certificate
// (electron-builder picks it from the keychain); notarization rejects ad-hoc or
// unsigned bundles.
//
// NOTE: notarytool requires Xcode 13+ command-line tools on the build host
// (`xcrun notarytool` / `xcrun stapler`).

const { execFileSync } = require('node:child_process');

exports.default = async function notarizing(context) {
  const { electronPlatformName, appOutDir } = context;
  if (electronPlatformName !== 'darwin') return;

  const keyPath = process.env.APPLE_API_KEY;
  const keyId = process.env.APPLE_API_KEY_ID;
  const issuer = process.env.APPLE_API_ISSUER;
  if (!keyPath || !keyId || !issuer) {
    console.log(
      '[notarize] APPLE_API_KEY / APPLE_API_KEY_ID / APPLE_API_ISSUER not set — ' +
        'skipping notarization. The resulting build is signed but NOT notarized; ' +
        'first launch will be slow until a quarantine strip (see operator guide).',
    );
    return;
  }

  // Lazy require — only load the dep when we actually notarize, so a build
  // without the creds can never fail on a missing/!installed module.
  const { notarize } = require('@electron/notarize');
  const appName = context.packager.appInfo.productFilename;
  const appPath = `${appOutDir}/${appName}.app`;

  console.log(`[notarize] submitting ${appName}.app to Apple notary service (notarytool)…`);
  await notarize({
    tool: 'notarytool',
    appPath,
    appleApiKey: keyPath,
    appleApiKeyId: keyId,
    appleApiIssuer: issuer,
  });

  // notarytool only confirms acceptance; the ticket must be STAPLED into the
  // bundle so Gatekeeper can validate offline (the whole point — no VPN-stalled
  // round-trip on launch). Staple the .app here, before the DMG is assembled.
  console.log('[notarize] accepted — stapling ticket to the .app…');
  execFileSync('xcrun', ['stapler', 'staple', appPath], { stdio: 'inherit' });
  console.log('[notarize] stapled. First launch will validate offline.');
};
