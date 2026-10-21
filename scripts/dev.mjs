#!/usr/bin/env node
/**
 * Cross-platform dev runner for AHV Remote Console.
 *
 * Mirrors what scripts/dev.sh has always done on Unix, but written
 * against Node's child_process API so it also runs on Windows (where
 * the Makefile's `SHELL := /usr/bin/env bash` line resolves to a
 * non-existent path and the bash shebang in dev.sh is meaningless).
 *
 * Steps:
 *   1. Build the Go runtime + AHV plugin (so the host-arch binary is
 *      fresh; the Electron main process will spawn it).
 *   2. Build the bridge ISO. release-keygen auto-mints a dev key on
 *      first run — no operator action needed; idempotent on re-run.
 *   3. Hand off to the @ahv-remote-console/desktop dev runner, which
 *      spins up Vite for the renderer and launches Electron with
 *      AHV_REMOTE_CONSOLE_DEV=1.
 *
 * The bridge ISO path is exported so the Electron main process can
 * forward it to the spawned runtime via env. Operator does NOT need
 * to set anything; the location of build/agent.iso is the
 * convention this script enforces.
 *
 * Production builds (electron-builder) bundle the ISO into the app's
 * Resources dir; the runtime resolves to that location automatically
 * when packaged. See apps/desktop/electron-builder.yml extraResources
 * + the main process's runtime-manager.ts agentIsoPath().
 */

import { spawnSync, spawn } from 'node:child_process';
import { dirname, join, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';
import { existsSync } from 'node:fs';
import { platform } from 'node:os';

const __dirname = dirname(fileURLToPath(import.meta.url));
const ROOT = resolve(__dirname, '..');
const IS_WINDOWS = platform() === 'win32';

function run(cmd, args, opts = {}) {
  const display = [cmd, ...args].join(' ');
  console.log(`\n→ ${display}\n`);
  const result = spawnSync(cmd, args, {
    stdio: 'inherit',
    cwd: ROOT,
    shell: IS_WINDOWS,
    ...opts,
  });
  if (result.error) {
    console.error(`\n✗ failed to spawn: ${display}\n${result.error.message}\n`);
    process.exit(1);
  }
  if (result.status !== 0) {
    console.error(`\n✗ exit ${result.status}: ${display}\n`);
    process.exit(result.status ?? 1);
  }
}

const makeCmd = process.env.MAKE ?? 'make';

// 1. Build the Go runtime + AHV plugin
run(makeCmd, ['-C', 'apps/runtime', 'build']);
run(makeCmd, ['-C', 'plugins/ahv', 'build']);

// 2. Build the bridge ISO
run(makeCmd, ['iso']);

// 3. Hand off to the desktop dev runner.
const agentIsoPath = join(ROOT, 'build', 'agent.iso');
if (!existsSync(agentIsoPath)) {
  console.error(`\n✗ bridge ISO did not appear at ${agentIsoPath} after make iso\n`);
  process.exit(1);
}

const env = {
  ...process.env,
  AHV_REMOTE_CONSOLE_AGENT_ISO_PATH: agentIsoPath,
};

console.log(`\n→ pnpm --filter @ahv-remote-console/desktop dev\n`);
const child = spawn('pnpm', ['--filter', '@ahv-remote-console/desktop', 'dev'], {
  stdio: 'inherit',
  cwd: ROOT,
  shell: IS_WINDOWS,
  env,
});
child.on('exit', (code) => process.exit(code ?? 0));
process.on('SIGINT', () => child.kill('SIGINT'));
process.on('SIGTERM', () => child.kill('SIGTERM'));
