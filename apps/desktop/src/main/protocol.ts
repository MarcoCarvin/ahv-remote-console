import { app, protocol } from 'electron';
import { platform } from 'node:os';

export interface ProtocolHandlerArgs {
  url: string;
}

// MUST match electron-builder.yml `protocols[].schemes` and the Linux
// `x-scheme-handler/...` MimeType (both `ahv-remote-console`), or OIDC deep-link
// callbacks won't route back to the app. Kebab-case is the canonical value used
// everywhere else; this was previously camelCase and silently mismatched (ELEC-01).
const SCHEME = 'ahv-remote-console';

/**
 * Register the custom URL scheme as privileged. MUST be called before
 * `app.whenReady()` resolves — Electron enforces this at the API level.
 */
export function registerProtocolSchemes(): void {
  protocol.registerSchemesAsPrivileged([
    { scheme: SCHEME, privileges: { secure: true, standard: true } },
  ]);
}

/**
 * Install the deep-link handler for the `ahv-remote-console://` scheme. Safe to call after
 * `app.whenReady()`; in fact must come after, so a `BrowserWindow` exists to
 * forward URLs into.
 */
export function installOIDCCallbackHandler(onURL: (args: ProtocolHandlerArgs) => void): void {
  if (platform() === 'win32' && process.argv.length >= 2) {
    app.setAsDefaultProtocolClient(SCHEME, process.execPath, [process.argv[1]!]);
  } else {
    app.setAsDefaultProtocolClient(SCHEME);
  }

  app.on('open-url', (event, url) => {
    event.preventDefault();
    if (url.startsWith(`${SCHEME}://`)) {
      onURL({ url });
    }
  });

  // Windows: subsequent launches arrive via second-instance with the URL
  // appended to argv by the OS.
  app.on('second-instance', (_event, argv) => {
    const link = argv.find((a) => a.startsWith(`${SCHEME}://`));
    if (link) onURL({ url: link });
  });
}
