#!/usr/bin/env node
/* eslint-disable */
// Coordinated dev runner.
//
// What we run, and why:
//
//   1. One *synchronous* build of the preload and main bundles. Both
//      rarely change during a normal dev session — when they do, the
//      operator restarts `make dev`. We tried watch-builds earlier and
//      they made the laptop sweat by pinning two extra Node processes
//      pegged on filesystem polling for no real benefit.
//   2. One Vite dev server for the renderer (the only piece that
//      actually changes minute-to-minute, and the one that needs HMR).
//   3. One Electron process — which itself spawns the Go runtime via
//      RuntimeManager. The Electron main process owns the runtime
//      lifecycle so no other supervisor is needed.

import { spawn, spawnSync } from 'node:child_process';
import { fileURLToPath } from 'node:url';
import { dirname, join } from 'node:path';
import { existsSync } from 'node:fs';

const __dirname = dirname(fileURLToPath(import.meta.url));
const root = join(__dirname, '..');

const procs = [];

function run(cmd, args, opts = {}) {
  const child = spawn(cmd, args, { stdio: 'inherit', cwd: root, shell: false, ...opts });
  procs.push(child);
  child.on('exit', (code) => {
    if (code !== 0 && !child.killed) {
      console.error(`${cmd} ${args.join(' ')} exited with code ${code}`);
      shutdown(code ?? 1);
    }
  });
  return child;
}

function buildOnce(cfg) {
  const result = spawnSync(pkgRunner, ['exec', 'vite', 'build', '--config', cfg], {
    cwd: root,
    stdio: 'inherit',
    shell: false,
  });
  if (result.status !== 0) {
    console.error(`Build failed: ${cfg}`);
    process.exit(result.status ?? 1);
  }
}

function shutdown(code = 0) {
  for (const p of procs) {
    try { p.kill('SIGTERM'); } catch { /* noop */ }
  }
  process.exit(code);
}

process.on('SIGINT', () => shutdown(0));
process.on('SIGTERM', () => shutdown(0));

const pkgRunner = process.platform === 'win32' ? 'pnpm.cmd' : 'pnpm';

const APP_NAME = 'AHV Remote Console';

// macOS dev DX: a dev run is `electron .` against the prebuilt Electron.app,
// whose bundle Info.plist names it "Electron" — so the Dock tile and menu show
// "Electron", and app.setName() in the main process CANNOT rename a running
// macOS bundle (the name is read from Info.plist at launch). Relabel the LOCAL
// dev Electron bundle's display-name keys so `make dev` shows the real product
// name. Idempotent + best-effort: it mutates only THIS repo's node_modules copy
// (reverted on reinstall, re-applied here every launch), never touches
// CFBundleExecutable (which must keep matching the actual binary), and never
// blocks dev on failure. Packaged builds are unaffected — electron-builder sets
// the name from `productName` in electron-builder.yml.
async function relabelDevElectron() {
  if (process.platform !== 'darwin') return;
  try {
    const electronPath = (await import('electron')).default;
    if (typeof electronPath !== 'string') return;
    // electronPath = .../Electron.app/Contents/MacOS/Electron → Info.plist is
    // two levels up, in Contents/.
    const infoPlist = join(dirname(electronPath), '..', 'Info.plist');
    if (!existsSync(infoPlist)) return;
    for (const key of ['CFBundleName', 'CFBundleDisplayName']) {
      const set = spawnSync('/usr/libexec/PlistBuddy', ['-c', `Set :${key} ${APP_NAME}`, infoPlist], {
        stdio: 'ignore',
      });
      // CFBundleDisplayName is optional and may be absent — add it if Set fails.
      if (set.status !== 0) {
        spawnSync('/usr/libexec/PlistBuddy', ['-c', `Add :${key} string ${APP_NAME}`, infoPlist], {
          stdio: 'ignore',
        });
      }
    }
    console.log(`[dev] relabeled dev Electron bundle → "${APP_NAME}"`);
  } catch {
    /* best-effort: dev still works, the Dock just shows the default "Electron" */
  }
}

console.log('[dev] building preload + main (one-shot)...');
buildOnce('vite.preload.config.ts');
buildOnce('vite.main.config.ts');

if (!existsSync(join(root, 'dist/main/main.js')) || !existsSync(join(root, 'dist/preload/preload.js'))) {
  console.error('[dev] preload/main bundles did not appear after build');
  process.exit(1);
}

console.log('[dev] starting Vite dev server for renderer...');
const vite = run(pkgRunner, ['exec', 'vite', '--config', 'vite.renderer.config.ts']);

// Wait for Vite to print its "ready" message before launching Electron, with
// a generous fallback so a slow dep optimization doesn't block us forever.
await waitForVite(vite, 60_000);

await relabelDevElectron();

console.log('[dev] starting Electron...');
const env = { ...process.env, AHV_REMOTE_CONSOLE_DEV: '1' };
run(pkgRunner, ['exec', 'electron', '.'], { env });

async function waitForVite(child, timeoutMs) {
  // Vite's stdout is inherited so we cannot reliably parse it. Instead, poll
  // localhost:5173 with HEAD requests until it responds (or timeout).
  const url = 'http://localhost:5173/';
  const deadline = Date.now() + timeoutMs;
  while (Date.now() < deadline) {
    try {
      const res = await fetch(url, { method: 'HEAD' });
      if (res.ok || res.status === 404) return;
    } catch {
      /* not ready yet */
    }
    await new Promise((r) => setTimeout(r, 250));
  }
  console.warn('[dev] Vite did not become ready within the timeout; launching Electron anyway');
  void child;
}
