import { Menu, type MenuItemConstructorOptions, app, shell } from 'electron';
import { platform } from 'node:os';

export interface AppMenuHooks {
  onPreferences: () => void;
  onDiagnostics: () => void | Promise<unknown>;
  onRevealLogFile?: () => void;
  /** Current "launch at login" state, used as the checkbox's initial value. */
  openAtLogin: boolean;
  /** Toggle "launch at login"; receives the new (post-click) checkbox state. */
  onToggleOpenAtLogin: (enabled: boolean) => void;
}

export function buildAppMenu(hooks: AppMenuHooks): Menu {
  const isMac = platform() === 'darwin';

  // Launch-at-login toggle. Electron flips the checkmark on click, so
  // `item.checked` is already the new state when the handler runs.
  const openAtLoginItem: MenuItemConstructorOptions = {
    label: 'Open at Login',
    type: 'checkbox',
    checked: hooks.openAtLogin,
    click: (item) => hooks.onToggleOpenAtLogin(item.checked),
  };

  const macAppMenu: MenuItemConstructorOptions = {
    label: app.name,
    submenu: [
      { role: 'about' },
      { type: 'separator' },
      { label: 'Preferences…', accelerator: 'Cmd+,', click: () => hooks.onPreferences() },
      openAtLoginItem,
      { type: 'separator' },
      { role: 'services' },
      { type: 'separator' },
      { role: 'hide' },
      { role: 'hideOthers' },
      { role: 'unhide' },
      { type: 'separator' },
      { role: 'quit' },
    ],
  };

  const fileMenu: MenuItemConstructorOptions = {
    label: 'File',
    submenu: [
      ...(isMac
        ? []
        : ([
            { label: 'Preferences…', accelerator: 'Ctrl+,', click: () => hooks.onPreferences() },
            openAtLoginItem,
            { type: 'separator' },
          ] as MenuItemConstructorOptions[])),
      {
        label: 'Export Diagnostics…',
        click: () => void hooks.onDiagnostics(),
      },
      {
        // Reveals the live log file (electron-log target) in
        // Finder / Explorer. Critical for troubleshooting because
        // it surfaces convergence-layer state transitions WITHOUT
        // DevTools open. The renderer's [reconciler] log lines
        // are mirrored to this file via the hcLog IPC channel.
        label: 'Open Log File',
        click: () => hooks.onRevealLogFile?.(),
      },
      { type: 'separator' },
      ...(isMac
        ? ([{ role: 'close' }] as MenuItemConstructorOptions[])
        : ([{ role: 'quit' }] as MenuItemConstructorOptions[])),
    ],
  };

  const editMenu: MenuItemConstructorOptions = {
    label: 'Edit',
    submenu: [
      { role: 'undo' },
      { role: 'redo' },
      { type: 'separator' },
      { role: 'cut' },
      { role: 'copy' },
      { role: 'paste' },
      ...(isMac
        ? ([{ role: 'pasteAndMatchStyle' }, { role: 'selectAll' }] as MenuItemConstructorOptions[])
        : ([
            { role: 'delete' },
            { type: 'separator' },
            { role: 'selectAll' },
          ] as MenuItemConstructorOptions[])),
    ],
  };

  // DevTools is gated behind AHV_REMOTE_CONSOLE_DEV=1: in production
  // builds it would let any operator (or anyone with brief physical
  // access) inspect renderer state — including diagnostics globals
  // like window.__hcRfbClientGetter that expose the active RFBClient.
  // The IPC secret itself is no longer reachable (signRequest pattern,
  // apps/desktop/src/main/signer.ts), but defence-in-depth matters.
  // Dev = vite-served renderer. Debug = packaged renderer with
  // DevTools enabled (env-gated, not exposed to operators).
  const isDev = process.env.AHV_REMOTE_CONSOLE_DEV === '1';
  const isDebug = isDev || process.env.AHV_REMOTE_CONSOLE_DEBUG === '1';
  const viewMenu: MenuItemConstructorOptions = {
    label: 'View',
    submenu: [
      { role: 'reload' },
      { role: 'forceReload' },
      ...(isDebug ? ([{ role: 'toggleDevTools' }] as MenuItemConstructorOptions[]) : []),
      { type: 'separator' },
      { role: 'resetZoom' },
      { role: 'zoomIn' },
      { role: 'zoomOut' },
      { type: 'separator' },
      { role: 'togglefullscreen' },
    ],
  };

  const windowMenu: MenuItemConstructorOptions = {
    label: 'Window',
    submenu: [
      { role: 'minimize' },
      { role: 'zoom' },
      ...(isMac
        ? ([
            { type: 'separator' },
            { role: 'front' },
            { type: 'separator' },
            { role: 'window' },
          ] as MenuItemConstructorOptions[])
        : ([{ role: 'close' }] as MenuItemConstructorOptions[])),
    ],
  };

  const helpMenu: MenuItemConstructorOptions = {
    role: 'help',
    submenu: [
      {
        label: 'Report a security issue',
        click: () => void shell.openExternal('https://github.com/MarcoCarvin/ahv-remote-console/security/advisories/new'),
      },
    ],
  };

  const template: MenuItemConstructorOptions[] = isMac
    ? [macAppMenu, fileMenu, editMenu, viewMenu, windowMenu, helpMenu]
    : [fileMenu, editMenu, viewMenu, windowMenu, helpMenu];

  return Menu.buildFromTemplate(template);
}
