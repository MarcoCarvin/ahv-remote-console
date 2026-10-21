# ADR 0001: Electron is the desktop shell

## Status

Accepted (inferred from code; confirm)

## Context

AHV Remote Console is a desktop client for Nutanix AHV VM consoles. It needs a
windowed UI for the lobby and console views, pop-out console windows, an
embedded Prism SSO window, a system tray and menus. It also needs packaged
installers for macOS, Windows and Linux.

The architecture overview names the desktop tier as the Electron, React and
TypeScript layer. That tier owns the UI and the trust boundary.

The repository does not record why Electron was chosen, or which alternatives
were considered, such as a native toolkit or a browser-hosted UI. The reasoning
here is reconstructed from the security configuration.

## Decision

The desktop app is one Electron application. The main process runs Node.js. The
renderer is a React and TypeScript bundle that runs with `contextIsolation: true`,
`sandbox: true`, `nodeIntegration: false` and `webSecurity: true`.
electron-builder produces the packaged builds, and the Electron Fuses are set at
pack time.

## Consequences

- The security model depends on Electron's process split. Only the main process
  has Node, the filesystem and the IPC secret. The renderer is treated as
  potentially hostile. See [ADR 0004](0004-renderer-never-holds-secret.md).
- Packaged builds turn off the RunAsNode, NodeCliInspect and NodeOptions fuses,
  so a relaunch cannot bypass the sealed preload. See
  [ADR 0008](0008-auto-update-opt-in.md) for packaging and updates.
- Every build carries the Chromium and Node runtime. The repository does not
  discuss the size or update cost of that runtime.
- Window handling, permission handlers and navigation guards are part of the
  security surface and are tested in the desktop app.

## References

- [Architecture overview](../overview.md)
- [Desktop app](../desktop.md)
- [`apps/desktop/scripts/after-pack.js`](../../../apps/desktop/scripts/after-pack.js)
