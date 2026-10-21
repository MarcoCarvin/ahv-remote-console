/**
 * Preload script — runs in an isolated world with limited Node APIs and
 * exposes a tightly-scoped API to the renderer via contextBridge.
 *
 * Everything exposed here is the renderer's entire surface to the main
 * process. The renderer never has direct access to ipcRenderer, fs, or any
 * Node module.
 */

import { contextBridge, ipcRenderer } from 'electron';

/**
 * Lifecycle state of the Go runtime supervisor. The main process
 * forwards every transition over `runtime-state` so the renderer can
 * gate UI affordances (e.g. show a "connecting" overlay while
 * `state === 'starting'`, a fatal error banner while `state === 'error'`).
 *
 * TODO(renderer): the corresponding subscriber/component to consume
 * `onRuntimeState` should render the connecting overlay and an
 * error-with-retry path. The IPC contract is fixed by this preload type.
 */
export interface RuntimeStateEvent {
  state: 'starting' | 'ready' | 'error';
  handshake?: RuntimeHandshake;
  error?: string;
}

/**
 * The fixed, closed set of host-reserved key combinations the native windowed
 * keyboard grab can suppress on the host and forward to the guest. This enum is
 * the ONLY key data that crosses the bridge for the grab — never raw keysyms or
 * accelerator strings. The renderer maps each value to a local keysym chord
 * (the same one the Send Keys menu uses). Mirrors
 * `@ahv-remote-console/native-keyhook`'s `LockCombo`.
 */
export type LockCombo = 'AltTab' | 'AltShiftTab' | 'AltEsc' | 'Super';

export interface ExposedAPI {
  /** Synchronously read the current supervisor state (see RuntimeStateEvent). */
  getRuntimeState: () => Promise<RuntimeStateEvent>;
  /**
   * Subscribe to runtime state transitions. The callback fires for every
   * `starting → ready` (or `ready → starting → ready` on restart) cycle
   * and on terminal `error` states. Returns an unsubscribe function.
   */
  onRuntimeState: (cb: (state: RuntimeStateEvent) => void) => () => void;
  /**
   * Operator-triggered runtime restart (the renderer's "Retry" button on
   * the fatal-error screen). Resets the supervisor's restart budget and
   * re-spawns even if it had given up. Returns the immediate post-call
   * state snapshot; subsequent transitions arrive via `onRuntimeState`.
   */
  restartRuntime: () => Promise<RuntimeStateEvent>;
  /**
   * Ask main to sign a runtime-bound HTTP request. The renderer never
   * sees the IPC secret; only the resulting HMAC headers cross the
   * contextBridge. Caller passes the request body as a string (or null
   * for body-less calls) — main hashes it before signing.
   *
   * Replaces the previous `getIPCSecretHex` which leaked the secret to
   * the renderer's V8 isolate.
   */
  signRequest: (
    method: string,
    uri: string,
    body: string | null,
  ) => Promise<Record<string, string>>;
  /** Ask main to compute the Sec-WebSocket-Protocol subprotocol for an HMAC-authenticated WS upgrade. */
  signWebSocket: (uri: string) => Promise<string>;
  saveDiagnostics: () => Promise<string | null>;
  /**
   * Notify the main process that a console view has captured the keyboard.
   * The main process suspends app-menu accelerators (Cmd+Q, Ctrl+W, …) so
   * the keys flow to the guest VM instead of triggering local actions.
   * `false` releases capture.
   */
  setCaptureMode: (captured: boolean) => Promise<void>;
  /**
   * Renderer→main: arm/disarm the native WINDOWED keyboard grab for THIS window
   * (VMware-style auto-grab). Main owns the hardcoded combo list and installs the
   * per-OS hook; the renderer never supplies keys. Resolves `true` only if the
   * grab actually armed — `false` on Wayland / macOS-TCC-denied / addon absent —
   * so the renderer never shows a "grabbed" banner the OS can't honor (it keeps
   * the Send Keys menu for those combos instead). `false` releases the grab.
   */
  setLockGrab: (enabled: boolean) => Promise<boolean>;
  /**
   * Main→renderer ONLY: a host-reserved combo the native hook suppressed on the
   * host, delivered as a closed {@link LockCombo} enum. The renderer maps it to
   * the same keysym chord the Send Keys menu uses and injects it into the guest.
   * Fanned only to the focused, trusted, grab-owning window. Returns unsubscribe.
   */
  onLockCombo: (cb: (data: { combo: LockCombo }) => void) => () => void;
  /**
   * Pop the given VM session out into its own dedicated console window. The
   * main process tracks open windows by session ID; calling again with the
   * same ID focuses the existing window instead of spawning a duplicate.
   */
  openConsoleWindow: (sessionId: string, vmName: string) => Promise<void>;
  /** Close every open console pop-out (used on sign-out so stale consoles don't
   *  linger on dead credentials). */
  closeConsoleWindows: () => Promise<void>;
  /**
   * Open Prism's /console/ login page in a sandboxed window on an isolated
   * partition. Resolves with the captured session cookies once Prism reports
   * the session as authenticated. Rejects if the operator closes the window
   * before completing the flow.
   */
  openSSOLogin: (args: {
    endpointId: string;
    prismURL: string;
    allowInsecureTLS?: boolean;
    /** base64-DER pinned cert; when set the SSO window trusts only this cert. */
    pinnedCertB64?: string;
  }) => Promise<{
    cookies: { name: string; value: string }[];
    /** base64-DER cluster cert captured via Trust-On-First-Use (no prior pin),
     *  for the renderer to persist as the endpoint's pinned_cert_b64. */
    capturedCertB64?: string;
    capturedCertSha256?: string;
  }>;
  onOIDCCallback: (cb: (args: { url: string }) => void) => () => void;
  onNavigate: (cb: (args: { to: string }) => void) => () => void;
  onUpdateAvailable: (cb: (info: UpdateInfo) => void) => () => void;
  onUpdateDownloaded: (cb: (info: UpdateInfo) => void) => () => void;
  toggleFullscreen: () => Promise<void>;
  /**
   * Subscribe to NATIVE window-fullscreen transitions (the macOS green
   * button / toggleFullscreen's win.setFullScreen path). The DOM
   * `fullscreenchange` event does not fire for those, and the console
   * layout goes full-bleed in fullscreen, so main forwards the
   * BrowserWindow enter/leave-full-screen events. Returns unsubscribe.
   */
  onWindowFullscreen: (cb: (data: { fullscreen: boolean }) => void) => () => void;
  onClipboardChanged: (cb: (data: { text: string; seq: number }) => void) => () => void;
  platform: NodeJS.Platform;
  /**
   * Read the host OS clipboard. Bridged through Electron's main-process
   * `clipboard` module rather than `navigator.clipboard`, which in
   * sandboxed renderers is permission-gated and tends to silently fail.
   */
  clipboardRead: () => Promise<{ text: string; seq: number }>;
  /** Write text to the host OS clipboard. */
  clipboardWrite: (text: string) => Promise<{ ok: boolean; error?: string }>;
  /**
   * Append a structured renderer log line to the main-process log
   * file (electron-log writes to ~/Library/Logs/<APP>/main.log on
   * macOS, %APPDATA%/<APP>/logs/main.log on Windows). Use this to
   * mirror critical convergence-layer state transitions to disk so
   * operators can troubleshoot WITHOUT DevTools open.
   *
   *   level: 'info' | 'warn' | 'error' | 'debug'
   *   tag:   short channel ID, e.g. 'reconciler' / 'rfb.tx'
   *   message: pre-formatted text
   */
  hcLog: (level: 'info' | 'warn' | 'error' | 'debug', tag: string, message: string) => void;
}

interface RuntimeHandshake {
  version: string;
  pid: number;
  endpoint: string;
  secret_hash: string;
  started_at: string;
}

interface UpdateInfo {
  version: string;
  releaseDate?: string;
  releaseName?: string;
  releaseNotes?: string;
}

function listener<T>(channel: string, cb: (payload: T) => void): () => void {
  const wrapped = (_e: Electron.IpcRendererEvent, payload: T) => cb(payload);
  ipcRenderer.on(channel, wrapped);
  return () => ipcRenderer.off(channel, wrapped);
}

const api: ExposedAPI = {
  getRuntimeState: () => ipcRenderer.invoke('ahv-remote-console:get-runtime-state'),
  onRuntimeState: (cb) => listener('ahv-remote-console:runtime-state', cb),
  restartRuntime: () => ipcRenderer.invoke('ahv-remote-console:restart-runtime'),
  signRequest: (method, uri, body) =>
    ipcRenderer.invoke('ahv-remote-console:sign-request', { method, uri, body }),
  signWebSocket: (uri) => ipcRenderer.invoke('ahv-remote-console:sign-websocket', uri),
  saveDiagnostics: () => ipcRenderer.invoke('ahv-remote-console:save-diagnostics'),
  setCaptureMode: (captured) => ipcRenderer.invoke('ahv-remote-console:set-capture-mode', captured),
  setLockGrab: (enabled) => ipcRenderer.invoke('ahv-remote-console:set-lock-grab', enabled),
  onLockCombo: (cb) => listener('ahv-remote-console:lock-combo', cb),
  openConsoleWindow: (sessionId, vmName) =>
    ipcRenderer.invoke('ahv-remote-console:open-console-window', { sessionId, vmName }),
  closeConsoleWindows: () => ipcRenderer.invoke('ahv-remote-console:close-console-windows'),
  openSSOLogin: (args) => ipcRenderer.invoke('ahv-remote-console:open-sso-login', args),
  onOIDCCallback: (cb) => listener('ahv-remote-console:oidc-callback', cb),
  onNavigate: (cb) => listener('ahv-remote-console:navigate', cb),
  onUpdateAvailable: (cb) => listener('ahv-remote-console:update-available', cb),
  onUpdateDownloaded: (cb) => listener('ahv-remote-console:update-downloaded', cb),
  toggleFullscreen: () => ipcRenderer.invoke('ahv-remote-console:toggle-fullscreen'),
  onWindowFullscreen: (cb) => listener('ahv-remote-console:window-fullscreen', cb),
  onClipboardChanged: (cb) => listener('ahv-remote-console:clipboard-changed', cb),
  platform: process.platform,
  clipboardRead: () => ipcRenderer.invoke('ahv-remote-console:clipboard-read'),
  clipboardWrite: (text) => ipcRenderer.invoke('ahv-remote-console:clipboard-write', text),
  hcLog: (level, tag, message) => {
    // Fire-and-forget — the renderer must not block on the log
    // mirror landing on disk. Convergence state transitions fire at
    // ≤ 1 Hz so the IPC volume is trivial.
    void ipcRenderer.invoke('ahv-remote-console:hc-log', {
      level,
      tag,
      message,
    });
  },
};

contextBridge.exposeInMainWorld('ahvRemoteConsole', api);

// Type guard for renderers that want a typed view.
declare global {
  interface Window {
    ahvRemoteConsole: ExposedAPI;
  }
}
