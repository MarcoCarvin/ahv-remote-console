'use strict';

// Pinned-SHA-256 integrity verification for the native keyhook binary, run in
// the Electron MAIN process before the .node is require()'d (dlopen'd). The
// addon executes native code in the secret-holding main process, so a planted
// binary in app.asar.unpacked is a real (same-user, defense-in-depth) concern on
// Windows/Linux; macOS additionally has Library Validation + the ASAR-integrity
// fuse. This module is the SHA-256 layer of that defense.
//
// THREAT MODEL / LIMITS: the pin manifest (integrity.json) is packed INSIDE the
// asar (it is not a .node, so asarUnpack leaves it packed), while the .node lives
// in app.asar.unpacked — so an attacker who swaps the unpacked .node does NOT
// automatically control the manifest. An attacker who can rewrite the whole asar
// can still bypass this (detected by the macOS ASAR-integrity fuse; unprotected
// same-user on Win/Linux, where this never crosses a privilege boundary anyway).
// This raises the bar; it is not a sandbox.

const fs = require('node:fs');
const path = require('node:path');
const crypto = require('node:crypto');

/** Hex SHA-256 of a file's bytes. Throws if the file can't be read. */
function sha256File(file) {
  return crypto.createHash('sha256').update(fs.readFileSync(file)).digest('hex');
}

/**
 * Decide which native binary to load, enforcing the pin on SHIPPED builds.
 * Returns an absolute path to require(), or null to fall back to Send-Keys.
 *
 * SHIPPED build = an integrity.json manifest is present beside the package. In
 * that mode ONLY the manifest-pinned prebuild is trusted: a missing entry, a
 * missing file, or a hash mismatch returns null, and we NEVER fall through to an
 * unpinned binary an attacker could plant.
 *
 * DEV build = no manifest → permissive (prebuild or local node-gyp output),
 * since there is no pin to verify against.
 *
 * @param {string} baseDir  package root (the dir containing index.js)
 * @param {string} platform process.platform (e.g. 'win32', 'linux', 'darwin')
 * @param {string} arch     process.arch (e.g. 'x64', 'arm64')
 * @returns {string|null}
 */
function resolveVerifiedBinary(baseDir, platform, arch) {
  const key = `${platform}-${arch}`;
  const prebuild = path.join(baseDir, 'prebuilds', key, 'node.napi.node');

  let manifest = null;
  try {
    manifest = JSON.parse(fs.readFileSync(path.join(baseDir, 'integrity.json'), 'utf8'));
  } catch {
    /* no manifest → dev mode */
  }

  if (manifest) {
    const expected = manifest[key];
    if (typeof expected !== 'string' || expected.length === 0) return null; // no pin for this arch
    if (!fs.existsSync(prebuild)) return null;
    let actual;
    try {
      actual = sha256File(prebuild);
    } catch {
      return null;
    }
    // Constant-time compare on equal-length hex; length check first.
    if (actual.length !== expected.length) return null;
    if (!crypto.timingSafeEqual(Buffer.from(actual), Buffer.from(expected))) return null;
    return prebuild;
  }

  // Dev / unbuilt: nothing to verify against. Prefer a prebuild, then node-gyp.
  for (const f of [
    prebuild,
    path.join(baseDir, 'build', 'Release', 'keyhook.node'),
    path.join(baseDir, 'build', 'Debug', 'keyhook.node'),
  ]) {
    if (fs.existsSync(f)) return f;
  }
  return null;
}

module.exports = { sha256File, resolveVerifiedBinary };
