#!/usr/bin/env node
/**
 * verify-icons.mjs — assert all icon assets exist with correct dimensions.
 */

import { existsSync, statSync } from 'node:fs';
import { join, dirname } from 'node:path';
import { fileURLToPath } from 'node:url';
import sharp from 'sharp';

const __dirname = dirname(fileURLToPath(import.meta.url));
const ROOT = join(__dirname, '..');
const ASSETS = join(ROOT, 'assets', 'icon');
const BUILD = join(ROOT, 'build', 'icons');

const SIZES = [16, 32, 48, 64, 128, 256, 512, 1024];
let failures = 0;

function check(path, label) {
  if (!existsSync(path)) {
    console.error(`FAIL: missing ${label} (${path})`);
    failures++;
    return false;
  }
  return true;
}

async function checkPng(path, expectedSize, label) {
  if (!check(path, label)) return;
  const meta = await sharp(path).metadata();
  if (meta.width !== expectedSize || meta.height !== expectedSize) {
    console.error(`FAIL: ${label} is ${meta.width}x${meta.height}, expected ${expectedSize}x${expectedSize}`);
    failures++;
  } else {
    console.log(`  OK  ${label} (${expectedSize}x${expectedSize})`);
  }
}

async function main() {
  console.log('Verifying icon assets...\n');

  // PNG sizes in assets/icon/
  for (const size of SIZES) {
    await checkPng(join(ASSETS, `icon-${size}.png`), size, `icon-${size}.png`);
  }

  // Bundle files
  check(join(ASSETS, 'icon.icns'), 'icon.icns');
  check(join(ASSETS, 'icon.ico'), 'icon.ico');
  check(join(ASSETS, 'source', 'icon-master.svg'), 'source/icon-master.svg');

  // Build dir
  check(join(BUILD, 'ahv-remote-console.icns'), 'build/ahv-remote-console.icns');
  check(join(BUILD, 'ahv-remote-console.ico'), 'build/ahv-remote-console.ico');
  check(join(BUILD, 'ahv-remote-console-512.png'), 'build/ahv-remote-console-512.png');

  // Tahoe modes
  for (const mode of ['default', 'dark', 'tinted-light', 'tinted-dark', 'clear-light', 'clear-dark']) {
    await checkPng(join(ASSETS, 'modes', `icon-${mode}.png`), 1024, `modes/icon-${mode}.png`);
  }

  console.log(`\n${failures === 0 ? 'ALL PASSED' : `${failures} FAILURE(S)`}`);
  process.exitCode = failures > 0 ? 1 : 0;
}

main().catch((err) => {
  console.error(err);
  process.exitCode = 1;
});
