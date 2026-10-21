# AHV Remote Console

AHV Remote Console is a desktop remote console for virtual machines on Nutanix AHV.
It opens VM consoles through Prism, each in its own window, on Windows, macOS and
Linux. It can also copy and paste between your machine and the guest. Copy and paste
needs extra components on the AHV hosts and inside each guest. See
[What you install](#what-you-install).

## Features

Remote console. This works with the desktop app alone:

- Sign in to Prism, pick a VM and open its console in its own window.
- Keyboard and mouse input, a Send Keys menu, fullscreen, and ISO mounting.
- The console reconnects after network drops, VM reboots and power cycles. When it
  cannot, it shows the cause: session expired, access denied or VM unreachable.
- Sign-in is delegated to Prism, with local, directory or SSO accounts. Your Prism
  permissions are your permissions in the app.

Clipboard sync. This needs the host module and the guest agent:

- Copy and paste between your machine and the guest in both directions. Nutanix Guest
  Tools does not provide this for the console.
- The clipboard travels over the console channel, not the guest network, so a
  full-tunnel VPN in the guest does not break it.
- The host module turns the clipboard channel on only while a console is open, so
  VMs stay live-migratable.
- The guest agent also sets the guest's display resolution to fit the window.

## Demo

https://github.com/user-attachments/assets/6746a5ae-08a8-4155-9c62-a0b1fe41981b

The demo runs 1:39 and covers:

- **Sign-in:** signing in through SSO to a live Prism Central, and how the VMs you
  see change with your permissions.
- **Organisation:** folders and tags to arrange VMs the way you work.
- **Console access:** the VM console opened through an active VPN tunnel, even when
  the VM cannot be reached over the network.
- **Clipboard:** copy and paste in both directions between the VM and your
  workstation.

## What you install

| Component | Where it runs | Needed for | What it changes |
| --- | --- | --- | --- |
| Desktop app, with the runtime bundled | Your workstation | Everything | Installs the app. The runtime listens on `127.0.0.1` only. |
| [Host module](docs/operations/hypervisor-setup.md) (a libvirt hook) | Every AHV host that runs the VMs | Clipboard sync | Installs `/etc/libvirt/hooks/qemu` as root. Its manager process switches each VM's `COM3` serial port to a `qemu-vdagent` channel while a console is open. |
| [Guest agent](docs/operations/guest-agent.md) (an executable, not a driver) | Inside each Windows or Linux VM | Clipboard sync, display sizing | Installs per user and starts at login. Uses the serial port and the clipboard. On Windows it changes the system cursors while a console is attached. |

To prepare a VM for the agent, the app uploads the agent ISO to the cluster's image
service, attaches it as a CD-ROM and adds a serial port to the VM through Prism.
Adding the serial port can need a VM power cycle.

> [!NOTE]
> Without the host module and the guest agent, the app works as a remote console.
> Only copy and paste and automatic display sizing need them.

> [!CAUTION]
> **The host module is installed at your own risk.** It modifies Nutanix AHV
> hypervisor hosts. It is not part of AHV, and Nutanix does not support or endorse
> it. Installing it can affect your Nutanix support, and AHV upgrades can remove or
> break it. Use it only in environments you own or are authorised to change, test it
> in a lab first, and keep the
> [roll-back steps](docs/operations/hypervisor-setup.md#roll-back) at hand. The
> software is provided "as is", without warranty. You are responsible for any outage,
> data loss or other damage that results from installing or running it. See sections
> 7 and 8 of the [LICENSE](LICENSE).

> [!WARNING]
> **Unsigned binaries and endpoint security.** The guest agent is an executable that
> starts at login, reads and writes the clipboard, opens a serial port and, on
> Windows, changes the system cursors. EDR and antivirus products can flag, block or
> quarantine that behaviour, especially when the binary is not code-signed. Builds
> from this repository are signed only when you provide a signing certificate. Sign
> the guest agent and the desktop app with your organisation's certificate, and
> allow them in your EDR according to your security policy. Unsigned installers also
> trigger SmartScreen on Windows and Gatekeeper on macOS. See
> [Guest agent](docs/operations/guest-agent.md) for every change the agent makes, and
> [Packaging and release](docs/development/release.md) for signing.

## Live migration and maintenance windows

Copy and paste relies on the VM's serial port. While any operator has a console
window open for a VM, that port carries the `qemu-vdagent` channel, and QEMU blocks
live migration of the VM.

> [!IMPORTANT]
> Before a maintenance window in which VMs are live-migrated automatically, such as
> host patching or an upgrade that drains hosts, every operator must close their
> console windows for the affected VMs. QEMU sees the console disconnect, and the
> host module switches the serial port back automatically about 3 seconds later.
> Live migration then works again. The rest of the app can stay open.

- Cold migration, of a powered-off VM, is always possible.
- HA restarts are not affected.
- If you control the drain, the host module's maintenance marker parks every VM on a
  host even with consoles open. See
  [Maintenance and drain override](docs/operations/hypervisor-setup.md#maintenance-and-drain-override).
- Details: [vdagent and live migration](docs/architecture/live-migration.md).

## How it works

The desktop app starts a local Go runtime that listens on `127.0.0.1` only. The
runtime contains the AHV plugin, which signs in to Prism, lists VMs and proxies the
VM's VNC stream to the app. Every request from the app to the runtime is signed with
HMAC-SHA256 using a secret created at each launch.

The clipboard uses a SPICE `qemu-vdagent` channel on a serial port inside the guest:
`COM3` on Windows, `/dev/ttyS2` on Linux. The host module provides that channel on the
AHV host, and the guest agent bridges the port to the guest clipboard. You install the
agent from the agent ISO that the build produces.

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
- For clipboard sync only: the host module on the AHV hosts, and the guest agent in a
  Windows or Linux guest. See [What you install](#what-you-install).
- To build from source: Go 1.26 or later, Node 24 or later, and pnpm 11 or later.

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
| [Guest agent](docs/operations/guest-agent.md) | What the in-guest executable does, how to install and remove it |
| [Setting up AHV hosts](docs/operations/hypervisor-setup.md) | Install and roll back the host module |
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
