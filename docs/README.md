# Documentation

This is the index of the AHV Remote Console documentation, for operators who run the
app and engineers who build or change it. For a project introduction, see the
[repository README](../README.md).

If you operate the app, start with [Installing](operations/install.md) and
[Connect and use the clipboard](operations/connect-and-clipboard.md). If you work on
the code, start with the [architecture overview](architecture/overview.md). Terms are
defined in the [glossary](reference/glossary.md).

## Architecture

How the desktop app, runtime, AHV plugin and agent fit together.

| Document | Description |
| --- | --- |
| [overview.md](architecture/overview.md) | Components, trust boundary and connection flow. Start here. |
| [desktop.md](architecture/desktop.md) | The Electron app, IPC secret handoff, signing allowlist and Electron hardening |
| [runtime-proxy.md](architecture/runtime-proxy.md) | The Go runtime and the RFB/VNC proxy loop, control frames and network tuning |
| [ahv-plugin.md](architecture/ahv-plugin.md) | Prism sign-in with SSO cookies, VM listing, VNC and the agent ISO mount |
| [agent.md](architecture/agent.md) | Agent modes, per-user autostart, clipboard, display sizing, keep-warm |
| [clipboard.md](architecture/clipboard.md) | Clipboard sync end to end over the `qemu-vdagent` serial channel |
| [live-migration.md](architecture/live-migration.md) | The vdagent migration blocker and the host hook that removes it |
| [audit-and-logging.md](architecture/audit-and-logging.md) | Audit log events, hash chain and storage, and application logs |
| [update-integrity.md](architecture/update-integrity.md) | Opt-in updates and the Ed25519 manifest check |

### Architecture decision records

| Document | Description |
| --- | --- |
| [adr/README.md](architecture/adr/README.md) | Index of decision records and their status |
| [0001](architecture/adr/0001-electron-desktop-shell.md) | Electron is the desktop shell |
| [0002](architecture/adr/0002-separate-go-runtime-daemon.md) | A separate Go runtime holds the Prism and VNC logic |
| [0003](architecture/adr/0003-loopback-hmac-ipc.md) | Loopback HTTP and WebSocket with per-request HMAC |
| [0004](architecture/adr/0004-renderer-never-holds-secret.md) | The sandboxed renderer never holds the IPC secret |
| [0005](architecture/adr/0005-ahv-plugin-statically-linked.md) | The AHV plugin is linked into the runtime binary |
| [0006](architecture/adr/0006-clipboard-over-vdagent-serial.md) | Clipboard over `qemu-vdagent` on a serial port |
| [0007](architecture/adr/0007-agent-iso-per-user-install.md) | The agent ships as an ISO and installs per user |
| [0008](architecture/adr/0008-auto-update-opt-in.md) | Auto-update is opt-in and off by default |
| [0009](architecture/adr/0009-single-go-module.md) | One Go module for the whole repository |
| [0010](architecture/adr/0010-single-pnpm-workspace.md) | One pnpm workspace for the TypeScript side |
| [0011](architecture/adr/0011-prism-delegated-authentication.md) | No app-side identity system; Prism handles sign-in |

## Operations

Installing, configuring and running the app, and the host-side setup.

| Document | Description |
| --- | --- |
| [install.md](operations/install.md) | Install the desktop app on each platform, or build from source |
| [configure.md](operations/configure.md) | Add a Prism endpoint, pin its TLS certificate, store credentials |
| [connect-and-clipboard.md](operations/connect-and-clipboard.md) | Connect to a VM, install the agent, use the clipboard and keyboard layout |
| [troubleshooting.md](operations/troubleshooting.md) | Console, clipboard and migration failures and their causes |
| [hypervisor-setup.md](operations/hypervisor-setup.md) | Install, arm and check the libvirt hook on AHV hosts |
| [scripts.md](operations/scripts.md) | Scripts in `scripts/` and `cmd/`: who runs them and what they change |
| [support-matrix.md](operations/support-matrix.md) | Supported workstation and guest systems, and toolchain versions |

## Reference

Contracts, interfaces and terms.

| Document | Description |
| --- | --- |
| [protocol.md](reference/protocol.md) | Runtime HTTP/WS API, control frames and the RFB 3.8 subset |
| [ipc.md](reference/ipc.md) | IPC secret, signing allowlist and HMAC-SHA256 headers |
| [agent-cli.md](reference/agent-cli.md) | Agent command-line modes and their behaviour on Windows and Linux |
| [keyboard.md](reference/keyboard.md) | How keys reach the guest and how to set the guest layout |
| [glossary.md](reference/glossary.md) | Terms used across the documentation |

## Development

Building, testing and releasing from source.

| Document | Description |
| --- | --- |
| [build.md](development/build.md) | `make` targets, toolchain and the development loop |
| [testing.md](development/testing.md) | Tests, lint, type checks and the CI pipeline |
| [release.md](development/release.md) | Packaging, signing, notarization and update publishing |
| [plugin-sdk.md](development/plugin-sdk.md) | Write a runtime plugin against the SDK |
| [versioning.md](development/versioning.md) | Version numbers, release trigger and changelog rules |

## Security

Threats, controls and data handling. See the [security index](security/README.md).

| Document | Description |
| --- | --- |
| [README.md](security/README.md) | Index of the security documents |
| [threat-model.md](security/threat-model.md) | Trust boundaries, threats, controls and known gaps |
| [data-handling.md](security/data-handling.md) | What data the app stores, for how long, and what leaves the machine |

## Design

Specifications of the current desktop app interface. Start with the
[design overview](design/overview.md).

| Document | Description |
| --- | --- |
| [README.md](design/README.md) | Index of the design specifications |
| [overview.md](design/overview.md) | Map of every window, screen and UI surface |
| [design-system.md](design/design-system.md) | Tokens, themes, type scale, spacing and global conventions |
| [motion.md](design/motion.md) | Animation durations, easing and animation layers |
| [icons.md](design/icons.md) | The SVG `Icon` component |
| [shared-ui.md](design/shared-ui.md) | Shared UI components in `packages/ui` |
| [app-shell.md](design/app-shell.md) | Main window, navigation, toasts and command palette |
| [connection-manager.md](design/connection-manager.md) | The cluster selection screens before sign-in |
| [cluster-map.md](design/cluster-map.md) | The cluster map and its inspectors |
| [sso-login-window.md](design/sso-login-window.md) | The embedded Prism sign-in window |
| [lobby.md](design/lobby.md) | The VM grid shown after sign-in |
| [console-view.md](design/console-view.md) | The VNC viewport and the controls over it |
| [console-chrome.md](design/console-chrome.md) | Console rails overlaid on the guest screen |
| [console-controls.md](design/console-controls.md) | Console menus and the shortcuts overlay |
| [console-overlays.md](design/console-overlays.md) | Connecting, error and other full-screen states |
| [console-motion-visuals.md](design/console-motion-visuals.md) | Latency ring and remote cursor visuals |
| [capture-and-input.md](design/capture-and-input.md) | Keyboard lock banner and input indicators |
| [console-popout.md](design/console-popout.md) | Pop-out console windows |

## Related documentation

- [Repository README](../README.md)
- [Contributing](../CONTRIBUTING.md)
- [Security policy](../SECURITY.md)
- [Changelog](../CHANGELOG.md)
