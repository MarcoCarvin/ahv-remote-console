import { app, Menu, Tray, nativeImage } from 'electron';
import { join } from 'node:path';

export interface TrayHooks {
  onOpen: () => void;
  onQuit: () => void;
  /** Export a diagnostics bundle (falls back to host-only logs if the
   *  runtime is down). Surfaced on the tray so it's reachable even when the
   *  main window is showing the fatal "cannot reach runtime" screen. */
  onExportDiagnostics: () => void;
  /** Reveal the log folder in Finder/Explorer. */
  onOpenLogFolder: () => void;
}

/**
 * Resolve the tray icon path. Same branch as ICON_DIR in main.ts — see
 * the comment there for the layout rationale. We don't import that
 * constant to keep tray.ts decoupled from the AHVRemoteConsoleApp class file.
 */
function trayIconDir(): string {
  return app.isPackaged
    ? join(process.resourcesPath, 'icons')
    : join(__dirname, '..', '..', '..', '..', 'build', 'icons');
}

export function createTray(hooks: TrayHooks): Tray {
  const iconPath = join(trayIconDir(), 'tray-iconTemplate.png');
  const icon = nativeImage.createFromPath(iconPath);
  const tray = new Tray(icon.isEmpty() ? nativeImage.createEmpty() : icon);
  tray.setToolTip('AHV Remote Console');
  const menu = Menu.buildFromTemplate([
    { label: 'Open AHV Remote Console', click: hooks.onOpen },
    { type: 'separator' },
    { label: 'Export Diagnostics…', click: hooks.onExportDiagnostics },
    { label: 'Open Log Folder', click: hooks.onOpenLogFolder },
    { type: 'separator' },
    { label: 'Quit', click: hooks.onQuit },
  ]);
  tray.setContextMenu(menu);
  tray.on('click', hooks.onOpen);
  return tray;
}
