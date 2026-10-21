/**
 * Electron main process.
 *
 * Responsibilities:
 *   1. Spawn the Go runtime (`ahv-remote-console-runtime`) on launch and feed it the IPC
 *      secret over stdin. The runtime auto-binds an ephemeral loopback port
 *      and writes the address to a handshake file.
 *   2. Create the hardened BrowserWindow (sandbox, contextIsolation, no node
 *      integration, strict CSP, no remote module).
 *   3. Register the `ahv-remote-console://` URL scheme for deep-link callbacks.
 *   4. Provide IPC channels that proxy renderer requests to the runtime and
 *      expose the IPC secret to the renderer via a sealed contextBridge API.
 *   5. Run the auto-updater on a schedule.
 *   6. Build the system tray, app menu, and global hotkey.
 */

import type { Tray } from 'electron';
import {
  app,
  BrowserWindow,
  Menu,
  clipboard,
  ipcMain,
  nativeImage,
  shell,
  dialog,
  session,
  systemPreferences,
  type IpcMainInvokeEvent,
} from 'electron';
import log from 'electron-log';
import { randomBytes } from 'node:crypto';
import { createRequire } from 'node:module';
import { mkdirSync } from 'node:fs';
import { join } from 'node:path';
import { homedir, platform } from 'node:os';

import type { LockCombo } from '../preload/preload';

import { RuntimeManager, type RuntimeStateEvent, type RuntimeHandshake } from './runtime-manager';
import { Signer } from './signer';
import { buildAppMenu } from './menu';
import { getOpenAtLogin, setOpenAtLogin } from './login-item';
import { createTray } from './tray';
import { installOIDCCallbackHandler, registerProtocolSchemes } from './protocol';
import { openSSOLogin } from './sso-login';
import { assertSignableURI } from './signable';
import {
  registerTrustedWindow,
  assertTrustedSender,
  trustedBroadcastTargets,
  focusedBroadcastTarget,
} from './trustedSenders';
import { sanitizeLogField } from './logSanitize';
import {
  resolveStoredEndpointTLS,
  type EndpointFetcher,
  type ResolvedEndpointTLS,
} from './endpointLookup';
import { initAutoUpdate } from './autoUpdate';
import { mergeHostLogsIntoBundle, exportLocalLogsZip } from './diagnostics';
import type { Endpoint } from '@ahv-remote-console/protocol';

/**
 * Absolute path to the directory containing the app's PNG icons. Branches
 * on app.isPackaged because the relative layout differs between dev and
 * production builds:
 *
 *   - Dev: __dirname is `apps/desktop/dist/main`, four levels up is the
 *     repo root, so icons live at `<repo>/build/icons/*.png`.
 *   - Packaged: __dirname is `<App>/Contents/Resources/app.asar/dist/main`
 *     — four-levels-up landed at `<App>/Contents/`, which has no `build/`
 *     directory. electron-builder ships icons via extraResources under
 *     `process.resourcesPath/icons/`, which is the canonical Electron
 *     location for runtime-loaded image assets.
 *
 * (The build-time installer icons referenced by `mac.icon` / `win.icon` /
 * `linux.icon` in electron-builder.yml are baked into the .app/.exe by
 * electron-builder itself; those are unrelated to this lookup.)
 */
const ICON_DIR = app.isPackaged
  ? join(process.resourcesPath, 'icons')
  : join(__dirname, '..', '..', '..', '..', 'build', 'icons');

log.transports.file.level = 'info';
log.transports.console.level = process.env.NODE_ENV === 'production' ? 'warn' : 'debug';

const APP_ID = 'io.github.marcocarvin.ahv-remote-console';
app.setAppUserModelId(APP_ID);
app.name = 'AHV Remote Console';

// Dev perf-isolation mode: `AHV_REMOTE_CONSOLE_PERF=1 make dev` makes the dev
// build representative of shipping performance so console refresh / input lag
// can be benchmarked WITHOUT a full package build. It (a) skips the DevTools
// auto-open (Chromium repaint instrumentation is a heavy, dev-only tax) and
// (b) forwards perf=1 to the renderer so it drops React.StrictMode (which
// double-invokes the whole tree's renders + effects in dev; a no-op in prod).
// Default OFF — normal dev keeps DevTools + StrictMode's bug-catching.
const PERF_MODE = process.env.AHV_REMOTE_CONSOLE_PERF === '1';

// Disable Chromium's renderer-backgrounding optimisations.
//
// Background: Chromium throttles renderers it considers "not visible
// enough" — timers fire less often, rAF deprioritises, the scheduler
// moves the renderer to a low-priority CPU cgroup. For a remote-console
// app that's actively rendering framebuffer updates, polling clipboard,
// and forwarding pointer/key events, this throttling produces the
// "everything feels heavy" symptom in prod (because in dev, having
// DevTools open keeps the renderer foregrounded).
//
// Switches MUST be set before app.whenReady() resolves; see
// https://www.electronjs.org/docs/latest/api/command-line-switches
app.commandLine.appendSwitch('disable-renderer-backgrounding');
app.commandLine.appendSwitch('disable-background-timer-throttling');
app.commandLine.appendSwitch('disable-backgrounding-occluded-windows');

/**
 * Hardened BrowserWindow webPreferences shared verbatim by the main window and
 * every console pop-out. Both windows load the same bundled renderer over the
 * default session and must carry an identical security profile: sandboxed,
 * context-isolated, no node integration anywhere, strict web security, no
 * webview tag, and (per the command-line switches above) no background
 * throttling so framebuffer rendering / clipboard polling / input forwarding
 * never get deprioritized when the window is backgrounded. The per-window
 * `preload` path is spread in at each call site (it is computed identically
 * in both factories).
 */
const HARDENED_WEB_PREFERENCES = {
  contextIsolation: true,
  sandbox: true,
  nodeIntegration: false,
  nodeIntegrationInWorker: false,
  nodeIntegrationInSubFrames: false,
  webSecurity: true,
  allowRunningInsecureContent: false,
  webviewTag: false,
  spellcheck: false,
  autoplayPolicy: 'document-user-activation-required',
  backgroundThrottling: false,
} as const;

/** The native windowed keyboard-grab addon (main-process only). See
 *  `@ahv-remote-console/native-keyhook`. */
interface KeyhookModule {
  available(): boolean;
  arm(onCombo: (combo: LockCombo) => void): boolean;
  release(): void;
}

// Lazy, fault-tolerant load of the native keyhook addon. Resolved via a runtime
// require (NOT a static import) so a missing/unbuilt prebuild on a dev box or a
// toolchain-less runner degrades to Send-Keys instead of crashing main at
// startup. `null` is cached on failure so we don't retry every arm().
let keyhookCache: KeyhookModule | null | undefined;
function loadKeyhook(): KeyhookModule | null {
  if (keyhookCache !== undefined) return keyhookCache;
  try {
    const req = createRequire(__filename);
    keyhookCache = req('@ahv-remote-console/native-keyhook') as KeyhookModule;
  } catch {
    keyhookCache = null;
  }
  return keyhookCache;
}

// MUST run before app.whenReady() resolves — Electron rejects late calls.
registerProtocolSchemes();

// Single-instance lock.
if (!app.requestSingleInstanceLock()) {
  app.quit();
  process.exit(0);
}

class AHVRemoteConsoleApp {
  private mainWindow: BrowserWindow | null = null;
  // Tray reference must live for the OS to keep the icon visible; we read it
  // during shutdown to call .destroy() and let Electron clean up natively.
  private tray: Tray | null = null;
  private runtime: RuntimeManager;
  private readonly ipcSecret: Buffer;
  private readonly signer: Signer;
  private defaultMenu: Menu | null = null;
  // Set of webContents IDs (one per BrowserWindow) currently holding pointer
  // lock + keyboard capture. The app-menu accelerators are suspended whenever
  // this set is non-empty so any captured window — main or console — gets
  // pass-through behavior.
  private captureWindows = new Set<number>();
  // Pop-out console windows, keyed by session ID so we never spawn two for
  // the same VM tab.
  private consoleWindows = new Map<string, BrowserWindow>();
  // webContents id of the window that currently owns the native keyboard grab,
  // or null if no grab is armed. At most one window grabs at a time (the focused
  // console). Load-bearing for force-release: a stuck grab hijacks the whole
  // desktop, so it is released on the owner's blur/close and on app quit.
  private lockGrabWindowId: number | null = null;
  // Clipboard-bridge sequence + last-seen text. Maintained by the 150ms
  // pollClipboard loop (started in registerIPC) and read by the on-demand
  // clipboard-read handler. These were registerIPC-local closure vars before
  // the handler split; promoting them to fields lets the poll loop and the
  // read handler share the same state without changing any values.
  private clipSeq = 0;
  private lastClipText = '';

  constructor() {
    this.ipcSecret = randomBytes(48);
    // The Signer holds its own defensive copy of the secret so we can wipe
    // it independently. ipcSecret stays around only long enough to be
    // written to the runtime's stdin; we zero it explicitly after the
    // handshake check (see verifyAndSecure() below).
    this.signer = new Signer(this.ipcSecret);
    const dataDir = this.dataDir();
    mkdirSync(dataDir, { recursive: true });
    this.runtime = new RuntimeManager({
      dataDir,
      ipcSecret: this.ipcSecret,
      signer: this.signer,
      logger: log,
    });
  }

  async init(): Promise<void> {
    app.on('second-instance', () => this.focusMainWindow());
    app.on('window-all-closed', () => {
      if (platform() !== 'darwin') app.quit();
    });
    app.on('activate', () => {
      if (this.mainWindow === null) {
        void this.createMainWindow();
      }
    });
    app.on('before-quit', () => {
      // A native keyboard grab must never outlive the app — release it before
      // anything else so a crash-on-quit can't strand the desktop grabbed.
      this.releaseLockGrab();
      this.runtime.stop();
      this.tray?.destroy();
      this.tray = null;
      // Zero the in-memory IPC secret so it isn't recoverable from a
      // post-mortem heap snapshot or crash dump. After this point the
      // signer throws on every method.
      this.signer.wipe();
    });
    // Losing focus drops the grab: the operator alt-tabbed/cmd-tabbed away (the
    // very combo we grab), or a dialog stole focus. An app-level handler covers
    // every window (console + main) without per-window wiring.
    app.on('browser-window-blur', (_e, win) => {
      if (this.lockGrabWindowId != null && win.webContents.id === this.lockGrabWindowId) {
        this.releaseLockGrab();
      }
    });
    // Focus-push: when a console gains focus, hand it the CURRENT host clipboard.
    // With focus-scoped forwarding, a host copy made while this window was NOT
    // focused (e.g. copied from KeePass, then the operator switches to the VM)
    // never reached its guest — this delivers it on focus, so "the highlighted
    // guest has the host clipboard" holds without spraying every other VM. The
    // renderer's seq>baseline gate makes this a no-op when the window already has
    // the current clipboard, so refocusing without a new copy re-sends nothing.
    app.on('browser-window-focus', (_e, win) => {
      const target = focusedBroadcastTarget();
      if (target?.webContents.id === win.webContents.id) {
        target.webContents.send('ahv-remote-console:clipboard-changed', {
          text: this.lastClipText,
          seq: this.clipSeq,
        });
      }
    });
    // Per-window navigation guards are installed in createMainWindow and
    // createConsoleWindow. We deliberately do NOT install a global
    // web-contents-created handler — the embedded SSO login window
    // (sso-login.ts) legitimately needs to follow IdP redirects between
    // Prism and Okta / Authentik / etc., and a global guard would shove
    // every such redirect into the system browser, defeating the whole
    // point of an in-app login window.

    await app.whenReady();

    if (platform() === 'darwin' && app.dock) {
      // Squircle variant so the dock icon matches macOS's rounded convention
      // (the full-bleed square reads as out-of-place among other dock icons).
      const dockIcon = nativeImage.createFromPath(join(ICON_DIR, 'ahv-remote-console-dock.png'));
      if (!dockIcon.isEmpty()) app.dock.setIcon(dockIcon);
    }

    if (platform() === 'darwin') {
      // Disable macOS "press and hold" (the accent/diacritic popup) for THIS
      // app. In the VM console a held key must auto-repeat (hold 's' → 'ssss'),
      // but with press-and-hold on, Chromium fires keydown→keyup the instant the
      // accent popup appears, so the guest sees a single press and the key never
      // repeats. Off, a held key delivers normal repeats and the guest auto-
      // repeats the held keysym. App-scoped (our NSUserDefaults domain only) and
      // best-effort; may take a relaunch to take effect.
      try {
        systemPreferences.setUserDefault('ApplePressAndHoldEnabled', 'boolean', false);
      } catch {
        /* older Electron / sandboxed defaults — non-fatal */
      }
    }

    this.hardenSession();
    installOIDCCallbackHandler(({ url }) => {
      if (this.mainWindow) {
        this.mainWindow.webContents.send('ahv-remote-console:oidc-callback', { url });
      }
    });

    // ---- Decouple window creation from runtime startup ----
    //
    // Previously: `await this.runtime.start()` blocked window creation for
    // hundreds of ms (sometimes seconds) while we polled for the handshake
    // file. Operators saw a blank screen until the Go runtime finished
    // booting — the Electron core team's #1 documented startup anti-pattern.
    //
    // Now: kick off the runtime in the background, set up listeners that
    // forward state transitions to whichever windows are open, and create
    // the BrowserWindow immediately. The renderer renders a "connecting"
    // overlay while the runtime is `starting`, then drops it on `ready`.
    //
    // The handshake-secret verification still happens — just inside the
    // event handler below, before we flip the state to `ready`. If the
    // secret doesn't match we transition to `error` instead and the
    // renderer surfaces a fatal-error UI.
    this.wireRuntimeEvents();
    void this.runtime.start().catch((err) => {
      // The runtime emits `runtime:error` for handshake / spawn failures
      // already — this catch only exists to silence the unhandled-rejection
      // warning on the bare promise. The wired event handler is the
      // canonical surface for renderer notification.
      log.error('runtime start rejected', err);
    });

    await this.createMainWindow();
    this.tray = createTray({
      onOpen: () => this.focusMainWindow(),
      onQuit: () => app.quit(),
      onExportDiagnostics: () => void this.exportDiagnostics(),
      onOpenLogFolder: () => {
        try {
          shell.showItemInFolder(log.transports.file.getFile().path);
        } catch (err) {
          log.warn('open log folder failed', err);
        }
      },
    });
    this.defaultMenu = buildAppMenu({
      onPreferences: () => this.openPreferences(),
      onDiagnostics: () => this.exportDiagnostics(),
      onRevealLogFile: () => {
        try {
          const p = log.transports.file.getFile().path;
          shell.showItemInFolder(p);
        } catch (err) {
          log.warn('reveal log file failed', err);
        }
      },
      openAtLogin: getOpenAtLogin(),
      onToggleOpenAtLogin: (enabled) => {
        try {
          setOpenAtLogin(enabled);
        } catch (err) {
          log.warn('set open-at-login failed', err);
        }
      },
    });
    Menu.setApplicationMenu(this.defaultMenu);
    this.registerIPC();
    // In-app auto-update against the generic feed in AHV_REMOTE_CONSOLE_UPDATE_URL.
    // Gated to packaged builds —
    // there is no dev/debug update feed — and engineered to never block
    // or crash startup if the feed is unreachable.
    initAutoUpdate({ getMainWindow: () => this.mainWindow });
  }

  /**
   * Subscribe to runtime state transitions and forward them to every open
   * window via the `ahv-remote-console:runtime-state` IPC channel. The
   * renderer is expected to listen on this channel and drive its
   * "connecting" overlay / fatal-error UI from the payload.
   *
   * TODO(renderer): wire up `window.ahvRemoteConsole.onRuntimeState((s) => …)`
   * in preload + a React subscriber in the renderer shell to render the
   * connecting/error states. The IPC channel name
   * `ahv-remote-console:runtime-state` is the discoverable hook.
   */
  private wireRuntimeEvents(): void {
    const broadcast = (payload: RuntimeStateEvent) => {
      // Trusted app windows only — the SSO login window must never observe
      // runtime state (SEC-02/ELEC-03).
      for (const win of trustedBroadcastTargets()) {
        try {
          win.webContents.send('ahv-remote-console:runtime-state', payload);
        } catch (err) {
          log.warn('runtime-state broadcast failed', err);
        }
      }
    };
    this.runtime.on('runtime:starting', () => {
      broadcast({ state: 'starting' });
    });
    this.runtime.on('runtime:ready', (handshake: RuntimeHandshake) => {
      // Verify the runtime came up with the same secret we fed it on
      // stdin. The handshake file's secret_hash is the runtime's
      // confession; we compare against our locally-computed hash.
      // Mismatch ⇒ either the runtime was tampered with, an orphaned
      // old runtime crashed mid-write, or our spawn was raced. Fail
      // closed by transitioning the state machine to error.
      const expectedHash = this.signer.secretHash();
      if (handshake.secret_hash !== expectedHash) {
        log.error('runtime handshake secret_hash mismatch', {
          expected: expectedHash.slice(0, 8) + '…',
          received: handshake.secret_hash.slice(0, 8) + '…',
        });
        const msg = 'runtime IPC secret mismatch — refusing to continue';
        broadcast({ state: 'error', error: msg });
        this.runtime.stop();
        return;
      }
      // Wipe the ipcSecret buffer that was used to spawn the runtime.
      // The Signer keeps its own internal copy; this just stops the raw
      // secret from sitting in two places in V8 memory. Safe to call
      // multiple times — Buffer.fill is idempotent on zeroed memory.
      this.ipcSecret.fill(0);
      broadcast({ state: 'ready', handshake });
    });
    this.runtime.on('runtime:error', (err: Error) => {
      broadcast({ state: 'error', error: err.message });
    });
  }

  private async createMainWindow(): Promise<void> {
    const preloadPath = join(__dirname, '..', 'preload', 'preload.js');
    const appIcon = join(ICON_DIR, 'ahv-remote-console-512.png');
    this.mainWindow = new BrowserWindow({
      width: 1440,
      height: 900,
      minWidth: 960,
      minHeight: 600,
      show: false,
      // Matches the renderer's default dark canvas (--hc-bg-0 in
      // packages/ui tokens.css). We now show the window before React's
      // first paint (see the load block below), so this is the color the
      // operator sees for the first few frames — must NOT be white.
      backgroundColor: '#0f1b2a',
      icon: appIcon,
      frame: true,
      titleBarStyle: platform() === 'darwin' ? 'hiddenInset' : 'default',
      webPreferences: {
        preload: preloadPath,
        ...HARDENED_WEB_PREFERENCES,
      },
    });

    const isDev = process.env.AHV_REMOTE_CONSOLE_DEV === '1';
    // Debug mode (AHV_REMOTE_CONSOLE_DEBUG=1) is for profiling a
    // packaged build — opens DevTools and skips autoUpdater but
    // STILL loads the production renderer from file://. Operators
    // shouldn't set this env var; it's a developer escape hatch.
    const isDebug = isDev || process.env.AHV_REMOTE_CONSOLE_DEBUG === '1';

    // Mark this window trusted so it (and only it + console pop-outs) can call
    // privileged IPC and receive host broadcasts — never the SSO login window.
    registerTrustedWindow(this.mainWindow);
    installRendererNavGuards(this.mainWindow);

    if (isDev) {
      await this.mainWindow.loadURL('http://localhost:5173' + (PERF_MODE ? '/?perf=1' : ''));
    } else {
      const indexHTML = join(__dirname, '..', 'renderer', 'index.html');
      await this.mainWindow.loadFile(indexHTML, PERF_MODE ? { search: 'perf=1' } : undefined);
    }
    if (this.mainWindow.isDestroyed()) return;

    // Show as soon as the page has finished loading. loadFile/loadURL above
    // resolves on 'did-finish-load', which — unlike 'ready-to-show' — does NOT
    // wait for the compositor to produce a frame.
    //
    // This is the macOS cold-start fix. We previously deferred show() to a
    // `once('ready-to-show')` handler. On macOS the first compositor frame for
    // a HIDDEN (show:false) window is heavily deprioritized, so that event
    // could stall for many seconds — sometimes minutes — leaving the operator
    // staring at a bouncing dock icon. Opening DevTools forces the renderer
    // active, which produced the frame and made the window appear instantly:
    // that's precisely why `AHV_REMOTE_CONSOLE_DEBUG=1` builds "opened smoothly"
    // and pinned the cause. Showing on load breaks the dependency on that
    // frame entirely. backgroundColor matches the dark theme canvas, so the
    // first few pre-paint frames show the app's own background, not a flash.
    this.mainWindow.show();
    this.mainWindow.focus();

    if (isDev && !PERF_MODE) {
      // Dev (vite-served): dock DevTools to the right. Skipped in PERF_MODE so
      // the dev build isn't taxed by DevTools' repaint instrumentation.
      this.mainWindow.webContents.openDevTools({ mode: 'right' });
    } else if (isDebug && !PERF_MODE) {
      // Profile a packaged build without loading from Vite. DevTools opens as
      // a detached window so the prod CSS layout isn't disturbed by a panel.
      this.mainWindow.webContents.openDevTools({ mode: 'detach' });
    }

    this.mainWindow.on('closed', () => {
      this.mainWindow = null;
    });
  }

  /**
   * Spawn a console-only window for a single VM session. Same security
   * profile as the main window, but lacks the sidebar/tabbar — it loads the
   * renderer with `?mode=console&session=<id>&vm=<name>`, which the
   * renderer's bootstrap inspects to mount a minimal ConsoleWindow shell.
   *
   * Pop-out windows are independent: closing one only closes that VM's
   * session, the main window is unaffected. Each window has its own pointer
   * lock + keyboard capture; the capture set in this class is the union and
   * keeps menu accelerators suspended while any window has the keyboard.
   */
  private createConsoleWindow(sessionId: string, vmName: string): void {
    const existing = this.consoleWindows.get(sessionId);
    if (existing && !existing.isDestroyed()) {
      existing.show();
      existing.focus();
      return;
    }

    const preloadPath = join(__dirname, '..', 'preload', 'preload.js');
    const consoleIcon = join(ICON_DIR, 'ahv-remote-console-512.png');
    const win = new BrowserWindow({
      width: 1280,
      height: 800,
      minWidth: 640,
      minHeight: 400,
      show: false,
      backgroundColor: '#0f1b2a',
      icon: consoleIcon,
      frame: true,
      titleBarStyle: platform() === 'darwin' ? 'hiddenInset' : 'default',
      // The pop-out has its own custom banner, so the native in-window menu bar
      // (File/Edit/View, Windows/Linux) is redundant chrome. Auto-hiding it also
      // fixes a resize flicker: the keyboard-capture handler toggles the global
      // application menu (null while captured, defaultMenu on release), and a
      // resize releases capture — which on Windows re-added the menu row and
      // reflowed the banner + console on every drag. autoHideMenuBar keeps the
      // bar collapsed regardless of which application menu is set (no-op on
      // macOS, where the menu lives in the system bar). The native title bar and
      // its min/max/close controls are unaffected.
      autoHideMenuBar: true,
      title: `${vmName} — AHV Remote Console`,
      webPreferences: {
        preload: preloadPath,
        ...HARDENED_WEB_PREFERENCES,
      },
    });

    this.consoleWindows.set(sessionId, win);
    // Console pop-outs are trusted app windows (same bundled renderer, default
    // session) — register so they get broadcasts + can call privileged IPC.
    registerTrustedWindow(win);
    installRendererNavGuards(win);

    // Forward NATIVE window-fullscreen transitions (green button / F11 /
    // the toggle-fullscreen IPC's win.setFullScreen). The renderer's DOM
    // `fullscreenchange` never fires for these, and ConsoleView drops its
    // rail margins to go full-bleed in fullscreen — without this push the
    // guest keeps the windowed insets and fullscreen shows dead bands where
    // the rails would be. did-finish-load re-sends the current state so a
    // renderer reload lands in the right layout.
    const sendFullscreen = () => {
      if (!win.isDestroyed()) {
        win.webContents.send('ahv-remote-console:window-fullscreen', {
          fullscreen: win.isFullScreen(),
        });
      }
    };
    win.on('enter-full-screen', sendFullscreen);
    win.on('leave-full-screen', sendFullscreen);
    win.webContents.on('did-finish-load', sendFullscreen);

    // Persist the console renderer's warn/error output to the main-process log
    // (~/Library/Logs/<app>/main.log). The renderer console is otherwise only
    // visible in DevTools, so an RFB stream desync leaves no on-disk trace.
    // Mirroring it here makes a desync self-diagnosing: the decoder's
    // "[rfb.desync] bounds guard tripped on rect …" forensic dump (bad rect
    // header + recent rects + next bytes) and the "[rfb] protocol reset —
    // realigning stream: <reason>" line land in a readable file, naming the
    // exact encoding/rect that lost byte-alignment — without opening DevTools.
    win.webContents.on('console-message', (details) => {
      if (details.level !== 'warning' && details.level !== 'error') return;
      log.info(`[console:${sessionId.slice(0, 8)}] ${details.message}`);
    });

    const search =
      `mode=console&session=${encodeURIComponent(sessionId)}&vm=${encodeURIComponent(vmName)}` +
      (PERF_MODE ? '&perf=1' : '');
    if (process.env.AHV_REMOTE_CONSOLE_DEV === '1') {
      void win.loadURL(`http://localhost:5173/?${search}`);
    } else {
      const indexHTML = join(__dirname, '..', 'renderer', 'index.html');
      void win.loadFile(indexHTML, { search });
    }

    // No before-input-event interceptor here — preventDefault() in that hook
    // would block the renderer's keydown events too (which is what was
    // happening before the fix). Menu accelerators are now suppressed via
    // setIgnoreMenuShortcuts(true) in the set-capture-mode handler.

    // T5 pop-out fade-in (cannot bridge layoutId across BrowserWindows).
    // Window opens at opacity 0, then steps to 1 over ~200ms via
    // setOpacity. Easing approximation: 12 frames at ease-standard
    // (linear here — perceptually close enough at this duration).
    //
    // Trigger on 'did-finish-load', NOT 'ready-to-show': on macOS a hidden
    // window's first compositor frame is deprioritized, so 'ready-to-show'
    // can stall for seconds-to-minutes (same root cause as the main window's
    // cold-start fix above). 'did-finish-load' fires when the page is loaded,
    // independent of compositing — and the opacity-0 → 1 fade hides any
    // pre-paint frame regardless.
    win.webContents.once('did-finish-load', () => {
      if (win.isDestroyed()) return;
      win.setOpacity(0);
      win.show();
      const t0 = Date.now();
      const DURATION = 200;
      const tick = () => {
        if (win.isDestroyed()) return;
        const elapsed = Date.now() - t0;
        const p = Math.min(1, elapsed / DURATION);
        win.setOpacity(p);
        if (p < 1) setTimeout(tick, 16);
      };
      tick();
    });
    // Captured now — `closed` wipes webContents, so we can't read the id later.
    const consoleWcId = win.webContents.id;
    // A renderer crash does NOT emit 'closed' and a dead renderer can't send
    // setLockGrab(false) or fire a DOM blur — so without this a crash with the
    // window still focused would strand a system-wide keyboard grab with no owner
    // to release it. Drop the grab the moment the render process is gone.
    win.webContents.on('render-process-gone', () => {
      if (this.lockGrabWindowId === consoleWcId) this.releaseLockGrab();
    });
    win.on('closed', () => {
      this.consoleWindows.delete(sessionId);
      // A closed window that still owned the keyboard grab must release it, or
      // the OS stays grabbed with no window to drop it.
      if (this.lockGrabWindowId === consoleWcId) this.releaseLockGrab();
      // Drop any lingering capture-bit for this window's webContents.
      // (closed wipes the id, but be defensive in case the id was reused.)
      for (const id of this.captureWindows) {
        if (BrowserWindow.fromId(id) == null) {
          this.captureWindows.delete(id);
        }
      }
      if (this.captureWindows.size === 0) {
        Menu.setApplicationMenu(this.defaultMenu);
      }
    });
  }

  private focusMainWindow(): void {
    if (!this.mainWindow) {
      void this.createMainWindow();
      return;
    }
    if (this.mainWindow.isMinimized()) this.mainWindow.restore();
    this.mainWindow.show();
    this.mainWindow.focus();
  }

  private hardenSession(): void {
    const ses = session.defaultSession;
    const isDev = process.env.AHV_REMOTE_CONSOLE_DEV === '1';

    // Strict CSP in production. In dev we permit Vite's React Fast Refresh
    // preamble (inline + eval) and the HMR WebSocket to localhost:5173.
    const prodCSP = [
      "default-src 'self'",
      "script-src 'self'",
      "style-src 'self' 'unsafe-inline'",
      "img-src 'self' data: blob:",
      "font-src 'self' data:",
      "connect-src 'self' http://127.0.0.1:* ws://127.0.0.1:*",
      // Workers spawned from same-origin OR blob: URLs. The blob:
      // case is required by Chromium's native DecompressionStream
      // implementation (used by the RFB client for Tight-encoded
      // framebuffer chunks) — it transparently delegates to a
      // background worker built from an inlined blob.
      "worker-src 'self' blob:",
      "media-src 'none'",
      "object-src 'none'",
      "base-uri 'none'",
      "frame-src 'none'",
      "form-action 'none'",
      "frame-ancestors 'none'",
    ].join('; ');

    const devCSP = [
      "default-src 'self' http://localhost:5173",
      // Vite injects an inline preamble for React Fast Refresh and uses eval
      // for HMR module updates. Both are dev-only.
      "script-src 'self' 'unsafe-inline' 'unsafe-eval' http://localhost:5173",
      "style-src 'self' 'unsafe-inline' http://localhost:5173",
      "img-src 'self' data: blob: http://localhost:5173",
      "font-src 'self' data: http://localhost:5173",
      // Vite HMR uses ws://localhost:5173; runtime IPC stays on 127.0.0.1.
      "connect-src 'self' http://127.0.0.1:* ws://127.0.0.1:* http://localhost:5173 ws://localhost:5173",
      // Same rationale as prodCSP — plus Vite's plugin-react Fast
      // Refresh now spawns worker-based refresh helpers from blob:
      // URLs (the prior implementation was an inline preamble; the
      // new one is a blob worker, which is why this CSP suddenly
      // started blocking it).
      "worker-src 'self' blob: http://localhost:5173",
      "media-src 'none'",
      "object-src 'none'",
      "base-uri 'none'",
      "frame-src 'none'",
      "form-action 'none'",
      "frame-ancestors 'none'",
    ].join('; ');

    ses.webRequest.onHeadersReceived((details, cb) => {
      cb({
        responseHeaders: {
          ...details.responseHeaders,
          'Content-Security-Policy': [isDev ? devCSP : prodCSP],
          'X-Frame-Options': ['DENY'],
          'X-Content-Type-Options': ['nosniff'],
          'Referrer-Policy': ['no-referrer'],
          'Permissions-Policy': ['camera=(), microphone=(), geolocation=()'],
        },
      });
    });

    // Block all permission requests by default; we don't need any.
    ses.setPermissionRequestHandler((_wc, _perm, callback) => callback(false));
    ses.setPermissionCheckHandler(() => false);
  }

  /**
   * Wire each IPC channel name to its handler method, in the SAME order as
   * before (registration order is part of the public contract — the renderer
   * relies on every channel being live by the time the window finishes
   * loading). The clipboard poll loop is started inline at its original
   * position (between set-capture-mode and clipboard-read) because the
   * `clipboard-read` handler reads the seq/text it maintains; see the
   * this.clipSeq / this.lastClipText fields.
   */
  private registerIPC(): void {
    ipcMain.handle('ahv-remote-console:get-runtime-state', this.handleGetRuntimeState);
    ipcMain.handle('ahv-remote-console:restart-runtime', this.handleRestartRuntime);
    ipcMain.handle('ahv-remote-console:sign-request', this.handleSignRequest);
    ipcMain.handle('ahv-remote-console:sign-websocket', this.handleSignWebSocket);
    ipcMain.handle('ahv-remote-console:save-diagnostics', this.handleSaveDiagnostics);
    ipcMain.handle('ahv-remote-console:set-capture-mode', this.handleSetCaptureMode);
    ipcMain.handle('ahv-remote-console:set-lock-grab', this.handleSetLockGrab);
    // Clipboard bridge. Main-process polls the OS clipboard every 150ms
    // (not subject to Chromium's background-tab throttling) and pushes
    // changes to all renderer windows. This ensures clipboard sync is
    // instant even when the Electron window is in the background (e.g.,
    // operator copies from KeePass, switches to the app, and the text
    // is already in the guest by the time the window focuses).
    //
    // Cross-platform: Electron's `clipboard` module is uniform across
    // macOS NSPasteboard, Windows OpenClipboard/GetClipboardData, and
    // Linux X11/Wayland. No platform branching needed — the polling
    // loop below is the single host-side ingress point on every OS.
    //
    // 50ms → 150ms: at 20 Hz the macOS NSPasteboard probe was
    // visibly contributing to "everything feels heavy" prod load.
    // 150ms (~6.7 Hz) is still well below human paste-and-look
    // perception thresholds (~250 ms) and cuts main-process probe
    // load by 3×. Latency-sensitive paths can still call the
    // on-demand `ahv-remote-console:clipboard-read` IPC.
    const clipPollTimer = setInterval(this.pollClipboard, 150);
    app.on('before-quit', () => clearInterval(clipPollTimer));

    // On-demand read (still needed for manual "Push clipboard" button).
    ipcMain.handle('ahv-remote-console:clipboard-read', this.handleClipboardRead);
    ipcMain.handle('ahv-remote-console:toggle-fullscreen', this.handleToggleFullscreen);
    ipcMain.handle('ahv-remote-console:clipboard-write', this.handleClipboardWrite);
    // (clipboard-stage-iso IPC removed — v3 ships text inline via
    // the keystroke/pixel-strip path; the ISO-based host→guest
    // clipboard data plane no longer exists. Bridge installer ISO
    // mount uses a separate `mount-iso` handler below.)
    ipcMain.handle('ahv-remote-console:open-console-window', this.handleOpenConsoleWindow);
    ipcMain.handle('ahv-remote-console:close-console-windows', this.handleCloseConsoleWindows);
    ipcMain.handle('ahv-remote-console:hc-log', this.handleHcLog);
    ipcMain.handle('ahv-remote-console:open-sso-login', this.handleOpenSSOLogin);
  }

  // Close every open console pop-out. Called on sign-out: logout wipes the
  // credentials for every endpoint, so each pop-out is now running on dead
  // cookies (it would otherwise only discover this on its next re-dial, then sit
  // on a session_expired overlay). Closing them proactively avoids leaving stale
  // consoles behind a sign-out the operator believes is complete (#7).
  private handleCloseConsoleWindows = (e: IpcMainInvokeEvent) => {
    assertTrustedSender(e);
    for (const win of [...this.consoleWindows.values()]) {
      if (!win.isDestroyed()) win.close();
    }
  };

  // Renderer-side state query. Pairs with the `runtime-state` push
  // channel so a freshly-mounted React subscriber can read the
  // CURRENT state synchronously instead of waiting for the next
  // transition event to fire.
  private handleGetRuntimeState = () => {
    return this.runtime.getState();
  };

  // Operator-triggered restart from the renderer's fatal-error "Retry"
  // affordance. Returns the post-restart state snapshot so the renderer
  // can immediately reflect "starting" without waiting for the push.
  private handleRestartRuntime = () => {
    // Fire-and-forget the spawn; the renderer is already subscribed to
    // the `runtime-state` channel and will observe starting → ready/error.
    void this.runtime.restart().catch((err) => {
      log.warn('runtime restart failed', err);
    });
    return this.runtime.getState();
  };

  private handleSignRequest = (
    e: IpcMainInvokeEvent,
    args: { method: unknown; uri: unknown; body: unknown },
  ) => {
    assertTrustedSender(e);
    if (typeof args?.method !== 'string' || typeof args.uri !== 'string') {
      throw new Error('sign-request: invalid args');
    }
    // Only sign for (method, path) pairs the SDK legitimately calls.
    // Binding BOTH method and path blocks a compromised renderer from
    // minting auth for sensitive paths or for a method the SDK never
    // issues on an otherwise-allowlisted path (SEC-ORACLE-1).
    assertSignableURI(args.method, args.uri);
    // Sign-request only HMACs headers, it does not actually talk to
    // the runtime — so we COULD allow it pre-ready. But every
    // signed call the renderer makes hits the runtime over HTTP
    // immediately afterward and will fail with ECONNREFUSED. Fail
    // fast here so the renderer can show a clean retry banner
    // instead of a generic network error.
    if (!this.runtime.isReady()) {
      throw new Error('runtime starting, try again');
    }
    const body =
      typeof args.body === 'string'
        ? args.body
        : args.body == null
          ? null
          : (() => {
              throw new Error('sign-request: body must be string | null');
            })();
    // Signer is the only thing in this process that touches the IPC
    // secret. Renderer code only sees the resulting headers.
    return this.signer.signRequest(args.method, args.uri, body);
  };

  private handleSignWebSocket = (e: IpcMainInvokeEvent, uri: unknown) => {
    assertTrustedSender(e);
    if (typeof uri !== 'string') throw new Error('sign-websocket: uri required');
    // WebSocket upgrades are GET requests; the allowlist lists the WS paths
    // (stream, presence) under GET.
    assertSignableURI('GET', uri);
    if (!this.runtime.isReady()) {
      throw new Error('runtime starting, try again');
    }
    return this.signer.signWebSocket(uri);
  };

  private handleSaveDiagnostics = async (e: IpcMainInvokeEvent) => {
    assertTrustedSender(e);
    return this.exportDiagnostics();
  };

  private handleSetCaptureMode = (e: IpcMainInvokeEvent, captured: unknown) => {
    assertTrustedSender(e);
    const wcId = e.sender.id;
    const isCaptured = Boolean(captured);
    if (isCaptured) {
      this.captureWindows.add(wcId);
    } else {
      this.captureWindows.delete(wcId);
    }
    // Per-webContents: tell Chromium to ignore menu shortcuts (Cmd+Q,
    // Cmd+W, etc.) so they flow to the guest VM instead of triggering
    // app menu actions. This is the *correct* primitive — unlike
    // before-input-event's preventDefault, it does NOT block the
    // renderer's keydown/keyup events from firing.
    try {
      e.sender.setIgnoreMenuShortcuts(isCaptured);
    } catch {
      /* older Electron versions or destroyed sender — best-effort */
    }
    // RED-03: scope the application-menu drop. Previously we blanked the
    // menu globally whenever ANY window was captured, so one renderer
    // could suppress menu accelerators for every window. Only drop the
    // app menu when the FOCUSED window is itself a captured window;
    // otherwise restore the default menu. (setIgnoreMenuShortcuts above
    // is already per-webContents, so even with the menu present a captured
    // window won't trigger accelerators.)
    const focused = BrowserWindow.getFocusedWindow();
    const focusedCaptured = focused != null && this.captureWindows.has(focused.webContents.id);
    if (focusedCaptured) {
      Menu.setApplicationMenu(null);
    } else {
      Menu.setApplicationMenu(this.defaultMenu);
    }
  };

  // Arm/disarm the native WINDOWED keyboard grab for the calling window. Returns
  // true only if the grab actually armed, so the renderer never paints a
  // "grabbed" banner the OS can't honor. The combo list is owned by native+main;
  // the renderer never supplies keys — only a boolean crosses inbound, and only a
  // closed LockCombo enum crosses back out.
  private handleSetLockGrab = (e: IpcMainInvokeEvent, enabled: unknown): boolean => {
    assertTrustedSender(e); // FIRST — never let an untrusted (SSO) window grab
    const win = BrowserWindow.fromWebContents(e.sender);
    if (!win || BrowserWindow.getFocusedWindow() !== win) return false;
    if (!enabled) {
      this.releaseLockGrab();
      return false;
    }
    const senderId = e.sender.id;
    // If another window still owns the grab (focus moved console→console and its
    // blur/release hasn't been processed yet), tear it down FIRST. Without this,
    // the native arm() is a no-op-returns-true against the old grab, so the new
    // window would paint a "locked" banner while combos still route to the old,
    // now-unfocused owner — a silent dead grab.
    if (this.lockGrabWindowId != null && this.lockGrabWindowId !== senderId) {
      this.releaseLockGrab();
    }
    const keyhook = loadKeyhook();
    if (!keyhook?.available()) return false; // Wayland / TCC / absent → Send-Keys
    const armed = keyhook.arm((combo: LockCombo) => {
      // Fan a suppressed combo ONLY to the focused, grab-owning window.
      const target =
        this.lockGrabWindowId != null ? BrowserWindow.fromId(this.lockGrabWindowId) : null;
      if (
        target &&
        !target.isDestroyed() &&
        BrowserWindow.getFocusedWindow() === target &&
        target.webContents.id === senderId
      ) {
        target.webContents.send('ahv-remote-console:lock-combo', { combo });
      }
    });
    if (armed) this.lockGrabWindowId = senderId;
    return armed;
  };

  // Tear down the native grab (idempotent). A backgrounded/closed/crashed locked
  // window must NEVER leave the OS keyboard grabbed system-wide.
  private releaseLockGrab = (): void => {
    if (this.lockGrabWindowId == null) return;
    this.lockGrabWindowId = null;
    try {
      loadKeyhook()?.release();
    } catch {
      /* best-effort */
    }
  };

  private pollClipboard = () => {
    try {
      // Compare full text equality rather than a length+prefix
      // signature: long copies that share a prefix (file paths,
      // URLs) used to collide on `${length}:${first 64 chars}` and
      // drop legitimate operator copies. Storing the full string is
      // cheap — it's already in OS RAM.
      const text = clipboard.readText() ?? '';
      if (text !== this.lastClipText) {
        this.clipSeq++;
        this.lastClipText = text;
        // FOCUS-SCOPED host→guest: a host copy is forwarded ONLY into the guest
        // the operator is currently looking at, not fanned into every open
        // console (which would spray a copied secret across unrelated VMs —
        // matches the focused-session clipboard ownership of VMware/RDP/SPICE).
        // clipSeq/lastClipText are bumped unconditionally above so the
        // focus-push (browser-window-focus) and the on-demand clipboard-read can
        // hand the CURRENT clipboard to whichever console is focused next.
        // Trusted app windows only — never the untrusted SSO window (SEC-02).
        this.sendClipboardToFocusedConsole(text, this.clipSeq);
      }
    } catch {
      /* clipboard read failed */
    }
  };

  // Send a clipboard-changed event to ONLY the focused console (the highlighted
  // guest), or to nobody if focus is outside the app / on the SSO window. The
  // single ingress point for focus-scoped host→guest forwarding, shared by the
  // poll loop and the guest→host write path. The renderer's seq>baseline gate
  // means a window that already holds this seq ignores it (idempotent).
  private sendClipboardToFocusedConsole = (text: string, seq: number) => {
    const target = focusedBroadcastTarget();
    if (target) {
      target.webContents.send('ahv-remote-console:clipboard-changed', { text, seq });
    }
  };

  private handleClipboardRead = (e: IpcMainInvokeEvent) => {
    assertTrustedSender(e);
    return { text: this.lastClipText, seq: this.clipSeq };
  };

  private handleToggleFullscreen = (e: IpcMainInvokeEvent) => {
    assertTrustedSender(e);
    const win = BrowserWindow.fromWebContents(e.sender);
    if (win) win.setFullScreen(!win.isFullScreen());
  };

  private handleClipboardWrite = async (
    e: IpcMainInvokeEvent,
    text: unknown,
  ): Promise<{ ok: boolean; error?: string }> => {
    assertTrustedSender(e);
    if (typeof text !== 'string') return { ok: false, error: 'invalid argument' };
    // Report success/failure explicitly (was swallowed) — a guest→host
    // paste that fails silently means the operator pastes stale content
    // believing it worked. Windows OpenClipboard contention is routine and
    // transient, so retry once after a short delay before giving up; and
    // read back to confirm the write actually landed.
    const attempt = (): { ok: boolean; error?: string } => {
      try {
        clipboard.writeText(text);
        if (text !== this.lastClipText) {
          this.clipSeq++;
          this.lastClipText = text;
          // Same focus scoping as pollClipboard: a guest→host copy lands on the
          // HOST clipboard (writeText above) for every window's benefit, but its
          // host→guest re-forward is confined to the focused console — which is
          // the originating console, where the loop-suppression ledger drops it.
          // This stops a copy made in guest A from spraying into guest B.
          this.sendClipboardToFocusedConsole(text, this.clipSeq);
        }
        return { ok: true };
      } catch (err) {
        // The real failure mode is OS clipboard contention (another app
        // holding OpenClipboard), which throws. We deliberately do NOT
        // read back and compare — OSes normalize line endings on the
        // round-trip (CRLF↔LF), which would false-fail multi-line pastes.
        return { ok: false, error: String((err as Error)?.message ?? err) };
      }
    };
    let res = attempt();
    if (!res.ok) {
      await new Promise((r) => setTimeout(r, 60)); // contention usually clears fast
      res = attempt();
    }
    if (!res.ok) log.warn('clipboard-write failed', res.error);
    return res;
  };

  private handleOpenConsoleWindow = (
    e: IpcMainInvokeEvent,
    body: { sessionId: string; vmName: string },
  ) => {
    assertTrustedSender(e);
    if (typeof body?.sessionId !== 'string' || !body.sessionId) {
      throw new Error('sessionId required');
    }
    this.createConsoleWindow(body.sessionId, body.vmName ?? 'Console');
  };

  // Convergence-layer log mirror. The renderer's Reconciler emits
  // structured log lines (`[reconciler.tx]`, `[reconciler.rx]`,
  // `[reconciler] TIMEOUT`, etc.) at every state-machine
  // transition. These are visible in DevTools but operators rarely
  // have DevTools open during a real session — and when a failure
  // surfaces in the UI ("✕ clip: ack timeout") they need the
  // surrounding log lines on disk to diagnose. This handler
  // streams them through electron-log's file transport so they
  // land in the same logs folder as the main-process log.
  private handleHcLog = (
    e: IpcMainInvokeEvent,
    body: { level: unknown; tag: unknown; message: unknown },
  ) => {
    assertTrustedSender(e);
    const level = typeof body?.level === 'string' ? body.level : 'info';
    // RED-04/ELEC-06: sanitize renderer-controlled tag + message before
    // writing to the shared log file — strip CR/LF (log-forging) and
    // C0/C1 control chars, and cap length.
    const tag = sanitizeLogField(typeof body?.tag === 'string' ? body.tag : 'renderer', 64);
    const message = sanitizeLogField(typeof body?.message === 'string' ? body.message : '');
    const line = `[${tag}] ${message}`;
    switch (level) {
      case 'error':
        log.error(line);
        break;
      case 'warn':
        log.warn(line);
        break;
      case 'debug':
        log.debug(line);
        break;
      default:
        log.info(line);
    }
  };

  private handleOpenSSOLogin = async (
    e: IpcMainInvokeEvent,
    body: {
      endpointId: string;
      prismURL: string;
      // RED-01: allowInsecureTLS / pinnedCertB64 are deliberately NOT read
      // from the renderer here — the stored endpoint config is authoritative.
      allowInsecureTLS?: boolean;
      pinnedCertB64?: string;
    },
  ) => {
    assertTrustedSender(e);
    if (typeof body?.endpointId !== 'string' || !body.endpointId) {
      throw new Error('endpointId required');
    }
    // Require https for a credential-bearing login. Cleartext SSO would
    // hand the operator's Prism session to anyone on-path.
    if (typeof body?.prismURL !== 'string' || !/^https:/i.test(body.prismURL)) {
      throw new Error('prismURL must be an https:// URL');
    }
    // RED-01: derive host + TLS posture from the STORED endpoint config,
    // not from renderer-supplied values. Look the endpoint up in the
    // runtime (signed call), require the renderer's prismURL host to equal
    // the stored host, and use the STORED allow_insecure_tls / pinned cert.
    // A compromised renderer therefore cannot aim the credential-bearing
    // SSO window at an attacker host or silently disable TLS verification.
    let resolved: ResolvedEndpointTLS;
    try {
      const endpoints = await this.fetchRuntimeEndpoints();
      resolved = resolveStoredEndpointTLS(endpoints, body.endpointId, body.prismURL);
    } catch (err) {
      log.warn('open-sso-login: endpoint validation failed', err);
      throw err instanceof Error ? err : new Error(String(err));
    }
    // No hard refusal for insecure-without-pin: openSSOLogin handles that
    // case with Trust-On-First-Use (capture the cluster cert, accept it
    // once, and the renderer persists it as the endpoint pin). That gives
    // operators a working path for self-signed clusters while still ending
    // up pinned — instead of blindly disabling verification forever.
    const result = await openSSOLogin({
      endpointId: body.endpointId,
      prismURL: body.prismURL,
      allowInsecureTLS: resolved.allowInsecureTLS,
      pinnedCertB64: resolved.pinnedCertB64,
    });
    return result;
  };

  /**
   * Fetch the runtime's stored endpoint list via a signed loopback call.
   * Used by the SSO login handler (RED-01) to read the authoritative host +
   * TLS posture for an endpoint. Mirrors RuntimeManager.exportDiagnostics's
   * net.request + Signer pattern (the runtime isn't reachable until ready).
   */
  private fetchRuntimeEndpoints: EndpointFetcher = async (): Promise<Endpoint[]> => {
    const state = this.runtime.getState();
    if (state.state !== 'ready' || !state.handshake) {
      throw new Error('open-sso-login: runtime not ready');
    }
    const base = state.handshake.endpoint;
    const path = '/v1/config/endpoints';
    const { net } = await import('electron');
    const request = net.request({ method: 'GET', url: `${base}${path}` });
    const sigHeaders = this.signer.signRequest('GET', path, null);
    for (const [k, v] of Object.entries(sigHeaders)) request.setHeader(k, v);
    const payload = await new Promise<string>((resolve, reject) => {
      request.on('response', (response) => {
        if (response.statusCode < 200 || response.statusCode >= 300) {
          reject(new Error(`open-sso-login: endpoint list HTTP ${response.statusCode}`));
          return;
        }
        const chunks: Buffer[] = [];
        response.on('data', (chunk: Buffer) => chunks.push(chunk));
        response.on('end', () => resolve(Buffer.concat(chunks).toString('utf8')));
        response.on('error', reject);
      });
      request.on('error', reject);
      request.end();
    });
    const parsed = JSON.parse(payload) as { endpoints?: Endpoint[] };
    return Array.isArray(parsed.endpoints) ? parsed.endpoints : [];
  };

  private async exportDiagnostics(): Promise<string | null> {
    // Do NOT require a main window: this is reachable from the tray, which
    // an operator uses precisely when the window is on the fatal error
    // screen or closed. showSaveDialog works with or without a parent window.
    const opts = {
      title: 'Export AHV Remote Console diagnostics',
      defaultPath: `ahv-remote-console-diagnostics-${new Date().toISOString().replace(/[:.]/g, '-')}.zip`,
      filters: [{ name: 'Zip archive', extensions: ['zip'] }],
    };
    const result = this.mainWindow
      ? await dialog.showSaveDialog(this.mainWindow, opts)
      : await dialog.showSaveDialog(opts);
    if (result.canceled || !result.filePath) return null;
    try {
      await this.runtime.exportDiagnostics(result.filePath);
      // DX-S-03: the runtime bundle is now on disk, but it does NOT contain the
      // HOST-side logs (main.log + runtime.log). Always merge those in,
      // regardless of runtime reachability, so a renderer-triggered diagnostics
      // bundle always carries the Electron main log + the runtime's stdout/stderr
      // tail — the two things support most often asks for and that the runtime's
      // own /v1/diagnostics/bundle cannot include.
      mergeHostLogsIntoBundle(result.filePath, this.dataDir());
    } catch (err) {
      // The runtime is unreachable — which is exactly the state the fatal
      // "cannot reach its local runtime" screen is shown in, and the state in
      // which an operator most needs to export logs. Fall back to a
      // host-side log-only bundle so "Export Diagnostics" always produces
      // something useful instead of erroring.
      log.warn('runtime diagnostics unavailable; building local-log fallback', err);
      exportLocalLogsZip(result.filePath, this.dataDir());
    }
    return result.filePath;
  }

  private openPreferences(): void {
    this.mainWindow?.webContents.send('ahv-remote-console:navigate', { to: '/preferences' });
  }

  private dataDir(): string {
    switch (platform()) {
      case 'win32':
        return join(
          process.env.APPDATA ?? join(homedir(), 'AppData', 'Roaming'),
          'AHV Remote Console',
        );
      case 'darwin':
        return join(homedir(), 'Library', 'Application Support', 'AHV Remote Console');
      default:
        return join(
          process.env.XDG_DATA_HOME ?? join(homedir(), '.local', 'share'),
          'ahvRemoteConsole',
        );
    }
  }
}

/**
 * Install navigation guards on a renderer-hosting BrowserWindow (the main
 * window and any console pop-out). These windows should:
 *
 *   * never spawn additional Electron windows for web-initiated window.open
 *     calls — links go to the system browser instead.
 *   * never navigate away from the local Vite dev server / bundled
 *     index.html — outbound links also go to the system browser.
 *
 * The embedded SSO login window (sso-login.ts) deliberately does NOT
 * receive these guards; it must follow IdP redirects.
 */
function installRendererNavGuards(win: BrowserWindow): void {
  win.webContents.setWindowOpenHandler(({ url }) => {
    openExternalSafely(url);
    return { action: 'deny' };
  });
  // Shared allow-list body: only the local Vite dev server and the bundled
  // index.html may be navigated to; anything else is cancelled and (if safe)
  // handed to the system browser.
  const guard = (event: { preventDefault: () => void }, url: string) => {
    const allowed = url.startsWith('http://localhost:5173') || url.startsWith('file://');
    if (!allowed) {
      event.preventDefault();
      openExternalSafely(url);
    }
  };
  win.webContents.on('will-navigate', (event, url) => guard(event, url));
  // ELEC-05: also govern server-side / meta redirects and subframe navigations.
  // will-navigate alone misses a 30x redirect chain and any navigation that
  // originates inside a subframe, so an outbound redirect or a framed document
  // could escape the allow-list. will-frame-navigate covers both the main frame
  // and subframes (its event carries the target URL on `details.url`).
  win.webContents.on('will-redirect', (event, url) => guard(event, url));
  win.webContents.on('will-frame-navigate', (details) => guard(details, details.url));
}

// Only hand https/mailto URLs to the OS. shell.openExternal will otherwise
// launch ANY scheme the OS knows — file://, smb://, or a registered custom
// protocol handler — so an attacker who ever obtains renderer JS execution
// (a future decoder/plugin-HTML bug) could trigger an arbitrary local handler.
// Defense-in-depth: the prod CSP and absence of HTML-injection sinks make this
// unreachable today, but the gate is correct Electron practice and cheap.
function openExternalSafely(url: string): void {
  let scheme: string;
  try {
    scheme = new URL(url).protocol;
  } catch {
    return; // unparseable — drop
  }
  if (scheme === 'https:' || scheme === 'mailto:') {
    void shell.openExternal(url);
  }
}

const hcApp = new AHVRemoteConsoleApp();
hcApp.init().catch((err) => {
  log.error('fatal startup error', err);
  dialog.showErrorBox(
    'AHV Remote Console failed to start',
    err instanceof Error ? (err.stack ?? err.message) : String(err),
  );
  app.exit(1);
});
