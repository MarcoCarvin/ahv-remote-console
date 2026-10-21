AHV Remote Console - Guest Agent
================================

What this CD-ROM is
-------------------

This CD-ROM installs the AHV Remote Console Guest Agent into this VM.
The agent lets the AHV Remote Console application on your workstation
sync the clipboard with this VM. It runs as the logged-in user — never
as SYSTEM/root and never machine-wide (on Windows via an HKCU Run
entry, on Linux via a per-user systemd unit) — and starts automatically
at each login.

How to install (Windows)
------------------------

Double-click "ahv-remote-console-agent.exe". It installs the agent and
offers to restart when it's done. That is the only step.

How to install (Linux)
----------------------

Desktop guests: open the linux/ folder and double-click the package for
your distribution - ahv-remote-console-agent.rpm (RHEL, Rocky, Fedora)
or ahv-remote-console-agent.deb (Ubuntu, Debian). Click Install, then
launch "AHV Remote Console Agent Setup" (the Launch button in Software,
or the application grid) and follow the dialogs.

Headless guests, or no admin rights: run the fallback script in a
terminal - it installs for your user only, no root needed:

    sh <mount-point>/linux/install.sh

Once setup completes you can eject this CD-ROM - the agent has been
copied into the system / your user profile and no longer needs it.

Files on this CD-ROM
--------------------

  ahv-remote-console-agent.exe
                       The Guest Agent for Windows. Double-click it to
                       install: it copies itself into your user profile,
                       sets it to start at login, then offers to restart.

  linux/ahv-remote-console-agent.rpm
  linux/ahv-remote-console-agent.deb
                       Distro packages for desktop Linux guests
                       (double-click to install, then run Agent Setup).

  linux/install.sh     Per-user fallback installer for headless Linux
                       guests or users without admin rights.

  linux/ahv-remote-console-agent-linux-amd64
                       The raw Linux agent binary install.sh installs.

  README.txt           This file.

  autorun.inf          Windows AutoPlay manifest pointing at the agent.

Uninstalling
------------

Windows - open a command prompt and run:

    "%LOCALAPPDATA%\AHV Remote Console\ahv-remote-console-agent.exe" --uninstall

Linux - run:

    ahv-remote-console-agent --uninstall

then remove the package (rpm -e / apt remove ahv-remote-console-agent)
if it was installed from one. The agent deregisters from login, stops,
and removes its per-user files.

More information
----------------

See docs/components/bridge-agent.md in the AHV Remote Console
repository for how the agent works. The agent stores no credentials or
secrets; clipboard contents are never logged (only byte length,
direction, and a SHA-256 digest).
