# Guest agent

The guest agent is the program that runs inside each VM to provide copy and paste
and automatic display sizing. This page explains what it is, what it does, every
change it makes to a VM, and how to install and remove it. Read it before you
install the agent, and share it with your endpoint security team.

The agent is optional. Without it, AHV Remote Console still works as a remote
console. Copy and paste also needs the host module on the AHV hosts. See
[Setting up AHV hosts](hypervisor-setup.md).

> [!IMPORTANT]
> The agent is not a driver. It installs no kernel driver, no device driver, no
> Windows service and no system daemon. It is an ordinary program that runs as the
> signed-in user and starts at login.

## Contents

- [What the agent does](#what-the-agent-does)
- [What the agent does not do](#what-the-agent-does-not-do)
- [Changes made to the VM](#changes-made-to-the-vm)
- [Code signing and endpoint security](#code-signing-and-endpoint-security)
- [Install on Windows](#install-on-windows)
- [Install on Linux](#install-on-linux)
- [Uninstall](#uninstall)
- [Logs](#logs)
- [Related documentation](#related-documentation)

## What the agent does

| Function | How it works |
| --- | --- |
| Clipboard sync | Reads and writes the guest clipboard. It exchanges clipboard text with QEMU over a virtual serial port (`COM3` on Windows, `/dev/ttyS2` on Linux) using the SPICE vdagent protocol. |
| Display sizing | Sets the guest's display resolution at login and keeps it there. The target comes from `--default-resolution`: `1920x1080` on Windows and `max` on Linux by default. |
| Keep-warm (Windows) | While a console window is attached, it keeps the console picture updating. A 1x1 transparent window changes one pixel every 25 ms, and every standard system cursor is replaced with an invisible one. The cursors are restored when the console detaches or the agent stops. |
| Single instance | Only one copy runs per user session. |

The full technical description is in [In-guest agent](../architecture/agent.md), and
every command-line flag is in the [agent CLI reference](../reference/agent-cli.md).

## What the agent does not do

- It makes no network connections. It talks only over the virtual serial port.
- It needs no administrator rights to run. One optional install step does, and is
  described below.
- It does not run machine-wide. Each user who wants clipboard sync installs it for
  their own account.

## Changes made to the VM

Preparing a VM and installing the agent changes the following. Use this list for
change records and for your EDR allow list.

| Change | Where | Admin rights | When |
| --- | --- | --- | --- |
| Agent ISO uploaded to the cluster image service | Prism | Prism permissions | **Install prerequisites** in the console |
| CD-ROM with the agent ISO attached to the VM | VM configuration, through Prism | Prism permissions | **Install prerequisites** |
| Serial port added to the VM | VM configuration, through Prism | Prism permissions | **Install prerequisites**. Can need a VM power cycle. |
| Agent binary copied | Windows: `%LOCALAPPDATA%\AHV Remote Console\ahv-remote-console-agent.exe`. Linux: `~/.local/bin/ahv-remote-console-agent` | No | Install |
| Autostart entry | Windows: value `AHV Remote Console Guest Agent` under `HKCU\Software\Microsoft\Windows\CurrentVersion\Run`, starting the agent with `--daemon`. Linux: `systemd --user` unit `ahv-remote-console-agent.service` | No | Install |
| Firmware boot resolution | The VM's UEFI variable store: OVMF's `PlatformConfig` variable | Yes. Windows shows a UAC prompt. | Windows installer, only when the display is not already at the target resolution. Linux only if you run `--apply-display` as root. |
| Serial-port group membership | Linux: `usermod -aG <group> <user>` through `pkexec` | Yes, a password prompt | Linux setup, only if the user cannot open the serial port |
| Linux package files | `/usr/bin/ahv-remote-console-agent` and an **Agent Setup** launcher in `/usr/share/applications` | Yes, through the package manager | Only if you install the `.rpm` or `.deb` |

## Code signing and endpoint security

> [!WARNING]
> EDR and antivirus products can flag, block or quarantine the agent. It is an
> executable that starts at login, uses the clipboard, opens a serial port, replaces
> the system cursors while a console is attached and, on Windows, can ask for
> elevation to write a firmware variable. The agent ISO also contains an
> `autorun.inf`, which some security policies block. These behaviours look
> suspicious to security tools, especially when the binary is not signed.

To deploy the agent in a managed environment:

1. Build it yourself and sign the Windows binaries with your organisation's
   Authenticode certificate. The agent Makefile's `sign-windows` target signs them
   when `signtool` and a certificate are configured. Without them the binaries are
   unsigned and Windows SmartScreen warns on first run. See
   [Packaging and release](../development/release.md).
2. Record the file hashes of the binaries you ship.
3. Give your security team the [Changes made to the VM](#changes-made-to-the-vm)
   table and this page, and add the agent to your EDR allow list according to your
   security policy.
4. Test the agent on one VM with your EDR active before you roll it out.

## Install on Windows

1. Open the VM's console. In the CD/ISO menu, select **Install prerequisites**. This
   attaches the agent ISO and adds the serial port. If the console says a power
   cycle is needed, power the VM off and on.
2. Inside the VM, open the new CD-ROM drive. AutoPlay may offer to start the agent
   because the disc has an `autorun.inf`.
3. Run `ahv-remote-console-agent.exe`. The installer window opens. If the WebView2
   runtime is missing, for example on older Windows Server editions, it installs
   quietly without a window.
4. The installer copies the agent to your profile, adds the autostart entry and
   starts the agent. This step needs no administrator rights.
5. If the display is not already at the target resolution, the installer starts
   itself again with elevation to write the firmware boot resolution. Windows shows
   a UAC prompt for **AHV Remote Console Guest Agent**. This is the only step that
   needs administrator rights. If you decline it, the install still completes, and
   the agent still sets the resolution at each login.
6. The installer offers to restart the VM. A restart applies the boot resolution.

> [!NOTE]
> The installer window does not mention the firmware resolution change, and the UAC
> prompt names the agent rather than a display tool. This page is the full
> description of what the elevated step does.

## Install on Linux

Use one of these paths:

- **Desktop guests.** Open the `linux/` folder on the CD-ROM and install the package
  for your distribution: `ahv-remote-console-agent.rpm` for RHEL, Rocky and Fedora,
  or `ahv-remote-console-agent.deb` for Ubuntu and Debian. Then start **AHV Remote
  Console Agent Setup** from the application menu and follow the dialogs. It asks
  for a target resolution and installs the agent for your user.
- **Headless guests, or no admin rights.** Run the fallback script. It installs for
  your user only and needs no root:

  ```bash
  sh <mount-point>/linux/install.sh
  ```

  Pass `none`, `max` or a size such as `1920x1080` to choose the resolution. The
  default is `max`.

If your user cannot open the serial port, setup offers to add you to the port's
group through `pkexec`. You must log in again afterwards. If the serial port does not
exist, power the VM off, add the port with **Install prerequisites**, and power it
on.

## Uninstall

Run the agent with `--uninstall`, as the user who installed it. No administrator
rights are needed.

| Platform | What is removed |
| --- | --- |
| Windows | The autostart value, the installed binary, and leftover entries from older versions |
| Linux | The `systemd --user` unit (stopped and disabled), the binary, and leftover entries from older versions |

The uninstall does not change the firmware boot resolution back, and it does not
remove the CD-ROM or the serial port from the VM. Remove those through Prism if you
no longer need them. If you installed the Linux package, remove it with your package
manager.

## Logs

| Platform | Log file |
| --- | --- |
| Windows | `%LOCALAPPDATA%\AHV Remote Console\hcbridge.log` |
| Linux | `$XDG_DATA_HOME/ahv-remote-console/hcbridge.log`, by default `~/.local/share/ahv-remote-console/hcbridge.log` |

The agent never writes clipboard contents to its log. It logs message types and
sizes only.

## Related documentation

- [Connect and use the clipboard](connect-and-clipboard.md)
- [Setting up AHV hosts](hypervisor-setup.md)
- [In-guest agent](../architecture/agent.md)
- [Agent CLI reference](../reference/agent-cli.md)
- [Data handling](../security/data-handling.md)
