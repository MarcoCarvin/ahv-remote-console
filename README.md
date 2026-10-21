# AHV Remote Console

A desktop client for Nutanix AHV VM consoles, with host-to-guest clipboard sync that
keeps working when the guest is on a full-tunnel VPN.

## Features

- Sign in to Prism, pick a VM and open its console in its own window.
- Copy and paste between your machine and the guest in both directions. Nutanix Guest
  Tools does not provide this for the console.
- The clipboard travels over the console channel, not the guest network, so a
  full-tunnel VPN in the guest does not break it.
- The console reconnects after network drops, VM reboots and power cycles. When it
  cannot, it shows the cause: session expired, access denied or VM unreachable.
- Sign-in is delegated to Prism, with local, directory or SSO accounts. Your Prism
  permissions are your permissions in the app.
- An optional host hook turns the clipboard channel on only while a console is open,
  so VMs stay live-migratable.

## How it works

The desktop app starts a local Go runtime that listens on `127.0.0.1` only. The
runtime contains the AHV plugin, which signs in to Prism, lists VMs and proxies the
VM's VNC stream to the app. Every request from the app to the runtime is signed with
HMAC-SHA256 using a secret created at each launch.

The clipboard uses a SPICE `qemu-vdagent` channel on a serial port inside the guest:
`COM3` on Windows, `/dev/ttyS2` on Linux. A small in-guest agent bridges that port to
the guest clipboard. The console works without the agent; the agent adds clipboard
sync and display sizing. You install it from the agent ISO that the build produces.

```text
desktop app --signed HTTP/WS--> runtime + AHV plugin --REST/VNC--> Prism
                                                                     |
                                     guest agent <--serial port-- VM console
```

See the [architecture overview](docs/architecture/overview.md) for the full picture.

## Requirements

- A Nutanix AHV cluster managed through Prism, and an account that can open VM
  consoles.
- A workstation running Windows, macOS or Linux. See the
  [support matrix](docs/operations/support-matrix.md) for architectures and
  installers.
- For clipboard sync: the in-guest agent on a Windows or Linux guest.
- To build from source: Go 1.25 or later, Node 24 or later, and pnpm 11 or later.

Known limits of the AHV console itself:

- The frame rate is capped at about 30 fps by QEMU's VNC refresh.
- JPEG encoding is not available in AHV's QEMU. The app offers a 16-bit low-colour
  mode to save bandwidth instead.
- The console cannot be resized from the client. The agent sets the guest resolution.

## Installation

Install the packaged desktop app for your platform. The runtime is bundled with it.
See the [installation guide](docs/operations/install.md), then
[configure an endpoint](docs/operations/configure.md).

## Quick start

Build and run from source:

```bash
git clone https://github.com/MarcoCarvin/ahv-remote-console.git
cd ahv-remote-console
make install
make dev
```

`make install` installs the pnpm and Go dependencies. `make dev` runs the app in
development mode. Only the renderer reloads on change. Restart after changes to the
Electron main process or preload.

Build and check:

```bash
make build
make test
make lint
make typecheck
```

Build the agent ISO at `build/agent.iso`. This always rebuilds the agent binaries
first:

```bash
make iso
```

Package an installer for the current platform with `make package`, or run the full
production build with `make prod`.

> [!NOTE]
> `make package-mac` needs a Developer ID signing identity and refuses to run
> without one. Use `make package-mac-dev` for a local unsigned build that you cannot
> ship.

See [Building from source](docs/development/build.md) and
[Testing](docs/development/testing.md) for all targets.

## Documentation

| Document | Description |
| --- | --- |
| [Documentation index](docs/README.md) | All documents, by section |
| [Architecture overview](docs/architecture/overview.md) | Components and connection flow |
| [Clipboard](docs/architecture/clipboard.md) | How clipboard sync works |
| [Live migration](docs/architecture/live-migration.md) | The clipboard channel and VM migration |
| [Connect and use the clipboard](docs/operations/connect-and-clipboard.md) | Walkthrough for operators |
| [Troubleshooting](docs/operations/troubleshooting.md) | Common failures and fixes |
| [Protocol reference](docs/reference/protocol.md) | Runtime API and control frames |
| [Threat model](docs/security/threat-model.md) | Security boundaries and controls |

## Contributing

Contributions are welcome. Read [CONTRIBUTING.md](CONTRIBUTING.md) and the
[Code of Conduct](CODE_OF_CONDUCT.md). For help, see [SUPPORT.md](SUPPORT.md).

## Security

Report vulnerabilities privately as described in [SECURITY.md](SECURITY.md). Do not
open a public issue.

## License

Apache License 2.0. See [LICENSE](LICENSE). Changes are listed in
[CHANGELOG.md](CHANGELOG.md).

## Trademarks

AHV Remote Console is an integration with the Nutanix AHV stack. It is not a Nutanix
product and is not endorsed or supported by Nutanix, Inc. See [NOTICE](NOTICE) for
the full statement and trademark notice.
