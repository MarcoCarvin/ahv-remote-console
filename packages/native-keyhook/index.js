'use strict';

// Loader for the native keyhook addon. CommonJS so the Electron main process
// can `require()` it (via createRequire) from the asar-unpacked tree.
//
// The addon is OPTIONAL: if no prebuilt/compiled binary is present (a dev box
// without a C/C++ toolchain, a CI lint job, the unbuilt sandbox), every export
// degrades to a no-op that reports "unavailable" so the app transparently falls
// back to the Send-Keys menu. Loading this module NEVER throws.

const { resolveVerifiedBinary } = require('./integrity');

// Native combo ids (must match the LockCombo enum order in src/keyhook.h).
const COMBO_NAMES = ['AltTab', 'AltShiftTab', 'AltEsc', 'Super'];

function loadNative() {
  // Pin-verify the unpacked .node before dlopen on shipped builds; permissive on
  // dev (no manifest). Returns null → we degrade to Send-Keys, never load an
  // unverified binary into the main process.
  const file = resolveVerifiedBinary(__dirname, process.platform, process.arch);
  if (!file) return null;
  try {
    return require(file);
  } catch {
    return null;
  }
}

const native = loadNative();

/** @returns {boolean} */
function available() {
  if (!native) return false;
  try {
    return Boolean(native.available());
  } catch {
    return false;
  }
}

/**
 * @param {(combo: string) => void} onCombo
 * @returns {boolean}
 */
function arm(onCombo) {
  if (!native || typeof onCombo !== 'function') return false;
  try {
    return Boolean(
      native.arm((id) => {
        const name = COMBO_NAMES[id];
        if (name) onCombo(name);
      }),
    );
  } catch {
    return false;
  }
}

function release() {
  if (!native) return;
  try {
    native.release();
  } catch {
    /* best-effort */
  }
}

module.exports = { available, arm, release };
