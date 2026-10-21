#!/usr/bin/env node
/* eslint-disable */
//
// electron-builder afterPack hook.
//
// Hardens the packaged macOS bundle so first-launch is fast and the Go
// runtime is ready to execute.
//
// On macOS:
//   1. Strip com.apple.quarantine recursively from the .app bundle.
//      In practice the build host should not be applying quarantine to
//      its own outputs, but stray xattrs can be picked up from network
//      mounts, browser-downloaded resources, etc. Stripping at pack
//      time saves users the first-launch XProtect scan (1.8–3.4s).
//   2. chmod +x the bundled Go runtime binary (electron-builder copies
//      from extraResources preserving perms, but belt + braces).
//   3. codesign --verify the runtime binary to confirm the bundle was
//      signed correctly. This is a no-op for unsigned dev builds since
//      `codesign --verify` on an unsigned binary returns non-zero — we
//      treat that as a warning, not a failure, so local builds still
//      succeed.
//
// On ALL platforms it also flips Electron Fuses (below); the macOS quarantine /
// codesign steps are darwin-only.

'use strict';

const { spawnSync } = require('node:child_process');
const { existsSync } = require('node:fs');
const path = require('node:path');
const { flipFuses, FuseVersion, FuseV1Options } = require('@electron/fuses');

/**
 * @param {import('electron-builder').AfterPackContext} context
 */
exports.default = async function afterPack(context) {
  const { electronPlatformName, appOutDir, packager, arch } = context;
  const productName = packager.appInfo.productFilename;

  // ── Electron Fuses (ALL platforms) ──────────────────────────────────────
  // Seal the packaged Electron binary against its most dangerous runtime
  // toggles. afterPack runs BEFORE electron-builder signs, so the flipped fuse
  // state is covered by the app signature. This closes the
  // ELECTRON_RUN_AS_NODE / --inspect / NODE_OPTIONS relaunch that would
  // otherwise run arbitrary Node under the app's own signature + entitlements,
  // bypassing the sealed preload + the IPC signing-allowlist trust boundary.
  // We deliberately do NOT enable EnableEmbeddedAsarIntegrityValidation here —
  // it interacts with notarization ordering and should be turned on separately.
  const electronBinary = electronBinaryPath(electronPlatformName, appOutDir, productName);
  if (electronBinary && existsSync(electronBinary)) {
    console.log(`[after-pack] flipping Electron fuses on ${electronBinary}`);
    await flipFuses(electronBinary, {
      version: FuseVersion.V1,
      [FuseV1Options.RunAsNode]: false,
      [FuseV1Options.EnableNodeCliInspectArguments]: false,
      [FuseV1Options.EnableNodeOptionsEnvironmentVariable]: false,
      [FuseV1Options.OnlyLoadAppFromAsar]: true,
      [FuseV1Options.EnableCookieEncryption]: true,
      // Re-apply an ad-hoc macOS signature after flipping so an UNSIGNED dev
      // build still launches on Apple Silicon. A signed release re-signs after
      // this hook (afterPack runs pre-sign), overriding the ad-hoc signature.
      resetAdHocDarwinSignature: electronPlatformName === 'darwin',
    });
  } else {
    console.warn(
      `[after-pack] Electron binary not found for fuses (${electronBinary}); skipping fuse flip`,
    );
  }

  if (electronPlatformName !== 'darwin') {
    return;
  }

  const appBundlePath = path.join(appOutDir, `${productName}.app`);

  if (!existsSync(appBundlePath)) {
    console.warn(`[after-pack] .app bundle not found at ${appBundlePath}; skipping`);
    return;
  }

  // 1) Strip com.apple.quarantine from the entire .app bundle.
  console.log(`[after-pack] stripping com.apple.quarantine from ${appBundlePath}`);
  const xattrResult = spawnSync('xattr', ['-dr', 'com.apple.quarantine', appBundlePath], {
    stdio: 'inherit',
  });
  if (xattrResult.status !== 0) {
    // xattr returns non-zero if the attribute was not present, which is fine.
    console.warn(
      `[after-pack] xattr -dr com.apple.quarantine returned ${xattrResult.status} (likely no quarantine xattr present; continuing)`,
    );
  }

  // electron-builder uses 'mac' for x64 and 'mac-arm64' for arm64; our build
  // tree under build/runtime mirrors that convention via electron-builder's
  // `${arch}` token (arm64 / x64). Match the layout in electron-builder.yml.
  // arch is an enum-like value; map to our directory name.
  const archDir = archToRuntimeDir(arch);
  const runtimeBin = path.join(
    appBundlePath,
    'Contents',
    'Resources',
    'runtime',
    `mac-${archDir}`,
    'ahv-remote-console-runtime',
  );

  if (!existsSync(runtimeBin)) {
    console.warn(`[after-pack] runtime binary not found at ${runtimeBin}; skipping chmod/codesign`);
    return;
  }

  // 2) chmod +x to guarantee the runtime is executable.
  console.log(`[after-pack] chmod +x ${runtimeBin}`);
  const chmodResult = spawnSync('chmod', ['+x', runtimeBin], { stdio: 'inherit' });
  if (chmodResult.status !== 0) {
    throw new Error(`chmod +x failed for ${runtimeBin}`);
  }

  // 3) codesign --verify the runtime to confirm the signature was applied.
  //    For unsigned dev builds this will return non-zero; warn instead of
  //    failing so local packaging still works.
  console.log(`[after-pack] codesign --verify ${runtimeBin}`);
  const verifyResult = spawnSync('codesign', ['--verify', '--verbose=2', runtimeBin], {
    stdio: 'inherit',
  });
  if (verifyResult.status !== 0) {
    console.warn(
      `[after-pack] codesign --verify returned ${verifyResult.status} for ${runtimeBin}. ` +
        'This is expected for unsigned dev builds; signed release builds should pass.',
    );
  } else {
    console.log('[after-pack] runtime binary signature verified');
  }
};

/**
 * Resolve the packaged Electron executable to flip fuses on.
 *   macOS : <out>/<Product>.app/Contents/MacOS/<Product>
 *   win   : <out>/<Product>.exe
 *   linux : <out>/<sanitized-product>  (electron-builder lowercases + hyphenates
 *           the productName for the executable; fall back to the raw name)
 * Returns the first candidate that exists, or the primary candidate (the
 * caller's existsSync guard then warns + skips rather than failing the build).
 *
 * @param {string} platformName
 * @param {string} appOutDir
 * @param {string} productName
 * @returns {string|null}
 */
function electronBinaryPath(platformName, appOutDir, productName) {
  switch (platformName) {
    case 'darwin':
      return path.join(appOutDir, `${productName}.app`, 'Contents', 'MacOS', productName);
    case 'win32':
      return path.join(appOutDir, `${productName}.exe`);
    case 'linux': {
      const sanitized = productName.toLowerCase().replace(/[ _]+/g, '-');
      const candidates = [path.join(appOutDir, sanitized), path.join(appOutDir, productName)];
      return candidates.find((p) => existsSync(p)) || candidates[0];
    }
    default:
      return null;
  }
}

/**
 * Map electron-builder Arch enum (or string) to the runtime/mac-<arch>
 * directory name used in extraResources.
 *
 * electron-builder's Arch enum values:
 *   0 = ia32, 1 = x64, 2 = armv7l, 3 = arm64, 4 = universal
 *
 * We accept either the numeric enum or a string for forward compatibility.
 *
 * @param {number|string} arch
 * @returns {string}
 */
function archToRuntimeDir(arch) {
  if (typeof arch === 'string') {
    return arch;
  }
  switch (arch) {
    case 1:
      return 'x64';
    case 3:
      return 'arm64';
    case 4:
      return 'universal';
    default:
      return String(arch);
  }
}
