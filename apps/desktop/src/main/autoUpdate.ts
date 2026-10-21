/**
 * Auto-update wiring extracted verbatim from main.ts (FE-05). The control flow,
 * env gating, event-handler registration, 6-hour poll, internal-CA trust, and
 * Linux manifest signature verification are unchanged — only the enclosing
 * `this.*` references were rebound to the `deps` passed in by main.ts.
 *
 * NOTE on __dirname: this module is bundled into the single dist/main/main.js
 * by vite (rollup), so __dirname here resolves to dist/main — identical to
 * where resolveCaBundlePath() ran before, keeping the dev CA-bundle path the
 * same four-levels-up anchor.
 */

import type { BrowserWindow } from 'electron';
import { app, dialog } from 'electron';
import log from 'electron-log';
import { autoUpdater } from 'electron-updater';
import { existsSync } from 'node:fs';
import { join } from 'node:path';
import { platform } from 'node:os';

import { trustedBroadcastTargets } from './trustedSenders';
import { verifyUpdateManifest, UPDATE_PUBKEY_B64 } from './updateVerify';

/**
 * Dependencies main.ts injects. `getMainWindow` is read lazily (only when the
 * update-downloaded dialog needs a parent), preserving the original timing of
 * the `this.mainWindow` read.
 */
export interface AutoUpdateDeps {
  getMainWindow: () => BrowserWindow | null;
}

/**
 * Wire electron-updater against a generic update feed given at runtime by
 * AHV_REMOTE_CONSOLE_UPDATE_URL. No feed is compiled into the app. With the
 * variable unset the updater does nothing. electron-updater appends the
 * per-platform channel manifest (latest.yml / latest-mac.yml /
 * latest-linux.yml) to that URL automatically.
 *
 * Hard requirements baked in here:
 *   - PACKAGED-ONLY. We skip entirely for dev (`AHV_REMOTE_CONSOLE_DEV=1`)
 *     and debug (`AHV_REMOTE_CONSOLE_DEBUG=1`) builds, and defensively for
 *     `!app.isPackaged`. There is no dev feed and electron-updater throws
 *     if app-update.yml is absent (it only ships in packaged installers).
 *   - NEVER blocks or crashes startup. Every entry point is wrapped so a
 *     network failure / unreachable feed / DNS error is logged and
 *     swallowed. `error` and `update-not-available` are logged quietly.
 *   - PRIVATE CA. electron-updater downloads over Node TLS, which trusts
 *     the OS store and would reject a feed signed by a private CA.
 *     We point NODE_EXTRA_CA_CERTS at the operator-supplied CA bundle BEFORE
 *     the updater runs (see resolveCaBundlePath()).
 */
export function initAutoUpdate(deps: AutoUpdateDeps): void {
  const isDev = process.env.AHV_REMOTE_CONSOLE_DEV === '1';
  const isDebug = isDev || process.env.AHV_REMOTE_CONSOLE_DEBUG === '1';
  // DEFAULT OFF. Auto-update is wired but must not run until two
  // preconditions are met, neither of which holds today:
  //   1. Code-signing — electron-updater REFUSES to apply an update to an
  //      unsigned app on macOS (Squirrel.Mac) and Windows; we currently
  //      ship unsigned builds, so an update would download then fail to
  //      install. Leaving the updater live would mean a packaged demo build
  //      tries (and silently fails) every 6h.
  //   2. A monotonic release version — the release feed publishes
  //      "<ver>-build.<pipeline>", which is semver-LESS than the installed
  //      "<ver>", so no update is ever detected anyway.
  // Until both land, the updater stays dormant unless explicitly opted in
  // (AHV_REMOTE_CONSOLE_AUTOUPDATE=1) for staged testing.
  if (process.env.AHV_REMOTE_CONSOLE_AUTOUPDATE !== '1') {
    log.info('auto-update: disabled (default) — pending code-signing + release-version cutover');
    return;
  }
  // Skip in dev/debug (no dev feed) and for any non-packaged build.
  // app-update.yml only exists inside a packaged installer; running the
  // updater without it throws, so app.isPackaged is the authoritative
  // gate and the env vars are belt-and-braces for `AHV_REMOTE_CONSOLE_DEBUG`
  // profiling of a packaged build.
  if (isDebug || !app.isPackaged) {
    log.info('auto-update: skipped (dev/debug or unpackaged build)');
    return;
  }
  const feedUrl = process.env.AHV_REMOTE_CONSOLE_UPDATE_URL;
  if (!feedUrl) {
    log.info('auto-update: no feed (AHV_REMOTE_CONSOLE_UPDATE_URL is not set)');
    return;
  }

  try {
    // --- Internal CA trust (must be set before any updater TLS) ---
    // electron-updater's HTTP client uses Node's TLS stack, which honors
    // NODE_EXTRA_CA_CERTS but NOT Electron/Chromium's own cert handling.
    // A feed signed by a private CA needs the bundle below, or
    // this the very first checkForUpdates() rejects with a self-signed /
    // unknown-issuer TLS error. Setting it here, before checkForUpdates(),
    // is sufficient: Node lazily reads NODE_EXTRA_CA_CERTS when it first
    // initializes its default TLS context, which has not happened yet at
    // app-ready time for an app that has made no outbound TLS connections.
    const caPath = resolveCaBundlePath();
    if (caPath && existsSync(caPath)) {
      process.env.NODE_EXTRA_CA_CERTS = caPath;
      log.info(`auto-update: NODE_EXTRA_CA_CERTS set to ${caPath}`);
    } else {
      // Don't abort — the feed may present a publicly-trusted
      // cert, or the operator's machine may already trust the internal CA
      // via the system store. Log loudly so a TLS failure is diagnosable.
      log.warn(
        `auto-update: CA bundle not found (looked at ${caPath ?? '<unresolved>'}); ` +
          'updates from a private-CA feed will fail TLS verification',
      );
    }

    // Route electron-updater's logs through the same electron-log instance
    // as the rest of main. The type cast is because electron-updater types
    // its logger as a structural subset of electron-log's Logger.
    autoUpdater.logger = log;
    autoUpdater.setFeedURL({ provider: 'generic', url: feedUrl, channel: 'latest' });
    // Download the update in the background as soon as one is found, then
    // install it on the next quit (or immediately if the operator clicks
    // "Restart now" in the update-downloaded dialog below).
    autoUpdater.autoDownload = true;
    autoUpdater.autoInstallOnAppQuit = true;

    autoUpdater.on('update-available', (info) => {
      log.info(`auto-update: update available ${info?.version ?? ''}`);
      // Trusted app windows only — never the untrusted SSO login window
      // (SEC-02/ELEC-03).
      for (const win of trustedBroadcastTargets()) {
        try {
          win.webContents.send('ahv-remote-console:update-available', info);
        } catch {
          /* window torn down mid-broadcast — best-effort */
        }
      }
    });
    autoUpdater.on('update-not-available', () => {
      // Quiet: this is the steady-state on a fully-updated install.
      log.info('auto-update: already up to date');
    });
    autoUpdater.on('error', (err) => {
      // Feed unreachable / TLS / parse failures land here. NEVER surface
      // a dialog or rethrow — a down feed must not degrade the app.
      log.warn('auto-update: check/download failed', err);
    });
    autoUpdater.on('update-downloaded', (info) => {
      void onUpdateDownloaded(deps, info);
    });

    // Initial check + a recurring 6-hour poll. checkForUpdates() returns a
    // promise that rejects on network errors; the `error` handler above
    // also fires, but we catch the rejection too so it never becomes an
    // unhandled rejection that could be escalated by a strict runtime.
    const check = () => {
      autoUpdater.checkForUpdates().catch((err) => {
        log.warn('auto-update: checkForUpdates rejected', err);
      });
    };
    check();
    const SIX_HOURS = 6 * 60 * 60 * 1000;
    const timer = setInterval(check, SIX_HOURS);
    // Don't let the poll timer keep the process alive on quit.
    timer.unref?.();
    app.on('before-quit', () => clearInterval(timer));
  } catch (err) {
    // Belt-and-braces: any synchronous setup failure (e.g. electron-updater
    // throwing because app-update.yml is somehow missing) must not crash
    // startup. The rest of the app is already up by this point.
    log.warn('auto-update: initialization failed; continuing without updates', err);
  }
}

/**
 * Show a native dialog offering to install a downloaded update now. On
 * "Restart now" we quitAndInstall(); on "Later" the update installs on the
 * next app quit (autoInstallOnAppQuit). Broadcast to the renderer too so a
 * future in-app affordance can mirror the state.
 */
async function onUpdateDownloaded(deps: AutoUpdateDeps, info: { version?: string }): Promise<void> {
  log.info(`auto-update: update downloaded ${info?.version ?? ''}`);
  // SECU-03: Linux AppImage updates carry no OS-enforced publisher signature
  // (unlike Squirrel.Mac / Windows NSIS), so a feed/TLS compromise could
  // push a malicious AppImage. Enforce an application-level Ed25519/minisign
  // signature over the update manifest before applying. Fails closed: if the
  // manifest can't be fetched or the signature can't be verified (e.g. no
  // signing key has been provisioned yet — see the SECU-03 needs-ops note),
  // we do NOT install. macOS/Windows keep their OS signature gate and are
  // unaffected by this branch.
  if (platform() === 'linux') {
    const verified = await verifyLinuxUpdateSignature();
    if (!verified) {
      log.error('auto-update: Linux manifest signature verification FAILED — refusing update');
      return;
    }
    log.info('auto-update: Linux manifest signature verified');
  }
  // Trusted app windows only (SEC-02/ELEC-03).
  for (const win of trustedBroadcastTargets()) {
    try {
      win.webContents.send('ahv-remote-console:update-downloaded', info);
    } catch {
      /* best-effort */
    }
  }
  try {
    const version = info?.version ? ` (${info.version})` : '';
    const opts = {
      type: 'info' as const,
      buttons: ['Restart now', 'Later'],
      defaultId: 0,
      cancelId: 1,
      title: 'Update ready',
      message: `A new version of AHV Remote Console${version} has been downloaded.`,
      detail:
        'Restart the app to apply the update, or it will be installed the next time you quit.',
    };
    const mainWindow = deps.getMainWindow();
    const result = mainWindow
      ? await dialog.showMessageBox(mainWindow, opts)
      : await dialog.showMessageBox(opts);
    if (result.response === 0) {
      // quitAndInstall tears down windows and relaunches the installed
      // build. autoInstallOnAppQuit already guarantees the "Later" path
      // installs on the next quit, so nothing else to do for response 1.
      autoUpdater.quitAndInstall();
    }
  } catch (err) {
    log.warn('auto-update: update-downloaded dialog failed', err);
  }
}

/**
 * SECU-03: fetch the Linux update manifest (`latest-linux.yml`) and its
 * detached minisign signature (`latest-linux.yml.minisig`) from the same
 * generic feed electron-updater uses, then verify the Ed25519 signature with
 * the embedded release public key (UPDATE_PUBKEY_B64).
 *
 * LIMITATION (Linux integrity-only): this proves the manifest was signed by
 * the holder of the release key, which gates which AppImage URL/hash the
 * updater will accept — but the AppImage payload itself is unsigned at the OS
 * level on Linux (no Authenticode / Squirrel.Mac equivalent). Manifest
 * signing is the strongest portable guarantee available here; verifying the
 * downloaded AppImage's sha512 against the (now-trusted) manifest is handled
 * by electron-updater itself.
 *
 * Returns false (fail-closed) on any error, including: no signing key
 * provisioned (the current default — see needs-ops), feed unreachable, or a
 * bad signature. NEEDS-LIVE-VALIDATION: exercised end-to-end only once a
 * real key + a published `.minisig` exist; unit tests cover the verify math.
 */
async function verifyLinuxUpdateSignature(): Promise<boolean> {
  if (!UPDATE_PUBKEY_B64) {
    log.warn(
      'auto-update: no update signing key provisioned (AHV_REMOTE_CONSOLE_UPDATE_PUBKEY_B64) — ' +
        'cannot verify Linux manifest signature; refusing update (SECU-03 needs-ops)',
    );
    return false;
  }
  try {
    const feed = autoUpdater.getFeedURL();
    if (!feed) {
      log.warn('auto-update: no feed URL available for manifest verification');
      return false;
    }
    const base = feed.replace(/\/$/, '');
    const manifestURL = `${base}/latest-linux.yml`;
    const sigURL = `${manifestURL}.minisig`;
    const [manifest, minisig] = await Promise.all([httpGet(manifestURL), httpGet(sigURL)]);
    return verifyUpdateManifest(manifest, minisig.toString('utf8'), UPDATE_PUBKEY_B64);
  } catch (err) {
    log.warn('auto-update: Linux manifest signature fetch/verify failed', err);
    return false;
  }
}

/** Minimal GET helper over electron.net for the update-manifest verify path. */
async function httpGet(url: string): Promise<Buffer> {
  const { net } = await import('electron');
  const request = net.request({ method: 'GET', url });
  return new Promise<Buffer>((resolve, reject) => {
    request.on('response', (response) => {
      if (response.statusCode < 200 || response.statusCode >= 300) {
        reject(new Error(`GET ${url} -> HTTP ${response.statusCode}`));
        return;
      }
      const chunks: Buffer[] = [];
      response.on('data', (chunk: Buffer) => chunks.push(chunk));
      response.on('end', () => resolve(Buffer.concat(chunks)));
      response.on('error', reject);
    });
    request.on('error', reject);
    request.end();
  });
}

/**
 * Resolve the on-disk path of the optional operator-supplied CA bundle for the
 * current build flavor:
 *
 *   - Packaged: `process.resourcesPath/certs/update-ca-bundle.pem`, if an
 *     operator placed one there (no bundle ships with this repository).
 *   - Dev/source: at `<repo>/scripts/certs/update-ca-bundle.pem`. From the
 *     compiled main bundle (`apps/desktop/dist/main`) the repo root is four
 *     levels up — the same anchor ICON_DIR uses for its dev branch.
 *
 * Returns null only if neither candidate is constructible (it never
 * throws — callers must tolerate a missing bundle).
 */
function resolveCaBundlePath(): string | null {
  if (app.isPackaged) {
    return join(process.resourcesPath, 'certs', 'update-ca-bundle.pem');
  }
  return join(__dirname, '..', '..', '..', '..', 'scripts', 'certs', 'update-ca-bundle.pem');
}
