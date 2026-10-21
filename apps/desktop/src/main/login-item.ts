/**
 * Cross-platform "open at login" (launch the app when the user signs in to
 * their workstation).
 *
 * macOS + Windows use Electron's native login-item API, which persists the
 * setting in the OS itself (Login Items / the Run registry key) — so we store
 * nothing and the toggle survives app updates and reinstalls.
 *
 * Linux has no `app.setLoginItemSettings` support, so we manage an XDG
 * autostart `.desktop` entry by hand: its presence under
 * `$XDG_CONFIG_HOME/autostart` (default `~/.config/autostart`) IS the state.
 */
import { app } from 'electron';
import { existsSync, mkdirSync, writeFileSync, rmSync } from 'node:fs';
import { dirname, join } from 'node:path';
import { homedir } from 'node:os';

const LINUX_DESKTOP_FILE = 'ahv-remote-console.desktop';

function linuxAutostartFile(): string {
  const base = process.env.XDG_CONFIG_HOME?.trim()
    ? join(process.env.XDG_CONFIG_HOME, 'autostart')
    : join(homedir(), '.config', 'autostart');
  return join(base, LINUX_DESKTOP_FILE);
}

function linuxExecPath(): string {
  // For a packaged AppImage, process.execPath points inside the ephemeral
  // mount; APPIMAGE holds the stable path the user actually launched, which is
  // the one that must be re-run at login.
  // eslint-disable-next-line @typescript-eslint/prefer-nullish-coalescing -- intentional falsy check: an empty/whitespace APPIMAGE must fall through to execPath, not be treated as a valid path
  return process.env.APPIMAGE?.trim() || process.execPath;
}

/** Whether the app is currently registered to launch at login. */
export function getOpenAtLogin(): boolean {
  if (process.platform === 'linux') return existsSync(linuxAutostartFile());
  return app.getLoginItemSettings().openAtLogin;
}

/** Enable or disable launching the app at login. */
export function setOpenAtLogin(enabled: boolean): void {
  if (process.platform === 'linux') {
    const file = linuxAutostartFile();
    if (enabled) {
      mkdirSync(dirname(file), { recursive: true });
      writeFileSync(
        file,
        [
          '[Desktop Entry]',
          'Type=Application',
          `Name=${app.name}`,
          `Exec=${linuxExecPath()}`,
          'Terminal=false',
          'X-GNOME-Autostart-enabled=true',
          '',
        ].join('\n'),
        'utf8',
      );
    } else if (existsSync(file)) {
      rmSync(file);
    }
    return;
  }
  // macOS + Windows: the operator chose to see the window on launch, so we
  // leave openAsHidden (macOS) at its default false.
  app.setLoginItemSettings({ openAtLogin: enabled });
}
