# Changelog

All notable changes to AHV Remote Console are listed in this file.

The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and the
project uses [Semantic Versioning](https://semver.org/spec/v2.0.0.html). Released
builds are attached to GitHub Releases. See the
[release process](docs/development/release.md).

## [Unreleased]

These changes apply to macOS, Windows and Linux unless noted.

### Added

- Automatic keyboard grab on Windows. Click into a console and Alt+Tab, the Windows
  key and Alt+Esc go to the VM, with an on-screen banner. Press Ctrl+Alt or click
  away to release. Ctrl+Alt+Del and Win+L stay with Windows and remain on the
  **Send Keys** menu.
- Power VMs from the lobby. Start, shut down or restart a VM from its tile without
  opening its console. Shutdown and restart ask for confirmation. The controls are
  hidden for accounts without permission.
- Create and restore VM snapshots from the console. Both ask for confirmation.
  Restoring creates a new VM from the recovery point and leaves the running VM as it
  is.
- Mount any ISO from the cluster image catalog as a CD-ROM, and eject it.
- A tasks tray that shows real progress for agent install, ISO mount and snapshots.
- Live VM resource stats in the console: CPU, memory, disk, and network where the
  cluster reports it.
- Live latency indicator in the status rail.
- A "VM is starting" state during power-on instead of an error.
- Support for many consoles at once, 10 or more at 1080p or higher, each in its own
  window.
- A one-click guest agent installer. It installs per user, starts at login, and
  syncs the clipboard over the VNC and serial channel.
- A published data-handling and privacy statement.
- Third-party open-source notices for every target platform, with a CI check that
  fails the build when they are out of date.

### Changed

- Fullscreen on macOS is `Ctrl+Cmd+F`, the macOS standard. `Cmd+Shift+F` now reaches
  the guest. `F11` is unchanged on Windows and Linux.
- Cluster search uses `Cmd+F` or `Ctrl+F` in both the list and the map. `Cmd+K` or
  `Ctrl+K` opens only the command palette.
- The **Send Keys** menu follows the usual VM console set: system key combinations,
  Win, Alt+Tab, Alt+Shift+Tab, Alt+Esc, Alt+F4, keys a host intercepts or a Mac lacks,
  function keys and Linux virtual terminal switching. Mac Option-layer symbols appear
  only on macOS hosts.
- Rendering matches the Nutanix AHV web console, with sharp output on HiDPI displays
  and smoother pointer movement.
- Pop-out consoles use the whole window for the guest screen. The toolbar appears when
  you hover at the top edge, so the guest taskbar at the bottom stays reachable.
- On a Mac, Command shortcuts reach the guest as their Ctrl equivalents. Command or
  Option with the arrow keys map to Home, End, word movement and document start and
  end. Shift extends the selection. Option+Delete deletes a word.
- The agent image is uploaded to the cluster once and reused. A new build replaces the
  previous image. Installing the agent again is safe.
- The standard guest display resolution is applied at boot and at every login.
- Clipboard sync no longer shows a status pop-up for each copy.
- VM power actions use the current Nutanix API and show clear errors.
- Console menus and panels open as centred overlays that close on Esc or a click
  outside.
- Updated console chrome and status rail with consistent status indicators.
- Signed installers and auto-update with increasing version numbers on all platforms.

### Fixed

- Consoles stay in sync through guest reboots, short network drops and VPN changes.
  The screen no longer freezes, loses sync or goes black, including on VMs at 1080p
  and above.
- A dropped console recovers on its own, even after the local runtime restarts or the
  VM session is lost. When it cannot recover, it shows a plain message with the next
  step: **Reconnect**, **Sign in** or **Power on**.
- Signing in again after a Prism session expires reconnects the VM in place instead
  of asking to sign in again and again.
- A short latency spike no longer freezes the latency reading, drops a healthy
  console, or leaves the picture in reduced colour. Full colour returns when the link
  recovers. A stuck connection is detected and reconnected faster.
- The first click or key press after the screen has been idle responds sooner.
- Resizing a pop-out console on Windows no longer flickers.
- Symbol keys type correctly when the host and guest layouts differ. For example, `/`
  on an Italian keyboard no longer types `?`. Italian, French AZERTY, German and
  similar layouts work.
- Holding a key repeats it inside the VM on every host platform.
- IME and CJK text input reaches the guest correctly.
- The clipboard handshake survives the AHV serial backend swap, and serial port
  (COM3) setup is more reliable.
- The shortcut reference (`?`) no longer lists `←` and `/`, which did nothing.

### Removed

- The system-wide `Cmd+Shift+H` or `Ctrl+Shift+H` hotkey, which blocked that
  combination in every other application. Use the dock, taskbar or tray.
- Ordinary Windows shortcuts from the **Send Keys** menu: Win+D, Win+E, Win+R, Win+L
  and similar, Win+arrows, Alt+Space, Shift+F10, Menu, Alt+PrintScreen, Escape and
  Ctrl+Alt+Esc. Type them directly while the keyboard is captured.

### Security

- Hardened authentication, local IPC, TLS certificate pinning and signed update
  verification.
- Clipboard contents are never written to logs, only length and hash. Credentials
  stay in the OS keychain.
- The bundled runtime is built with a Go standard library that includes the latest
  security fixes.

### Known limitations

- Automatic keyboard grab is not available on macOS and Linux. Use the **Send Keys**
  menu for host-reserved combinations there. On macOS, Command+Tab, Command+Space and
  Mission Control always stay with macOS. Use Option+Tab inside the guest.

## [1.0.0] - 2026-06

First release: a desktop console for Nutanix AHV VMs with the clipboard sync that
Nutanix Guest Tools does not provide.

### Added

- VNC/RFB console with Tight encoding, `ExtendedDesktopSize` and `ServerCutText`
  clipboard support.
- Clipboard sync in both directions over the agent's serial bridge. Text is sanitised
  on the host before it is written to the guest.
- The in-guest agent and its installer, guest display resolution control, and an
  ISO9660 and Joliet image that you mount to install the agent.
- Prism sign-in through the cluster's own login page with local, directory or SSO
  accounts. Includes re-authentication in the console (F5), Prism session cookie
  rotation, and credential storage in macOS Keychain, Windows Credential Manager or
  Linux Secret Service.
- VM power actions through the Nutanix v4 `$actions` API,
  capability probing and clear power error messages.
- Reconnecting sessions with typed disconnect causes, visible reconnect progress,
  backoff with jitter, fast failure on authentication errors, detection of half-open
  upstream connections, graceful drain and cleanup of idle sessions.
- Signed desktop packages for macOS, Windows and Linux, built with electron-builder,
  with optional auto-update.
- Generated `THIRD_PARTY_NOTICES.md` and a data-handling and privacy statement.

### Security

- Fixes for RFB and authentication denial-of-service and information leaks.
- The `null` CORS origin is no longer accepted.
- IPC nonces are evicted oldest first.
- The audit log syncs to disk only for security-critical events.
