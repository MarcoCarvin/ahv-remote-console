# Setting up AHV hosts

This runbook is for operators who install and run the console-following libvirt
hook on AHV hosts. It covers prerequisites, install, arming, verification,
host drains and roll-back. For why the hook exists, read
[vdagent and live migration](../architecture/live-migration.md).

## Contents

- [How the hook works](#how-the-hook-works)
- [Prerequisites](#prerequisites)
- [Validate the hook before you deploy](#validate-the-hook-before-you-deploy)
- [Install the hook](#install-the-hook)
- [Arm the manager](#arm-the-manager)
- [Verify a deployment](#verify-a-deployment)
- [Maintenance and drain override](#maintenance-and-drain-override)
- [Roll back](#roll-back)
- [Troubleshooting](#troubleshooting)
- [Reference](#reference)
- [Known limitations](#known-limitations)
- [Related documentation](#related-documentation)

## How the hook works

> [!IMPORTANT]
> A `qemu-vdagent` chardev holds an unconditional QEMU live-migration blocker for
> its whole lifetime. A native fix is planned for AHV 12.0. The hook exists to
> work around this blocker. See
> [The blocker](../architecture/live-migration.md#1-the-blocker).

The hook is one Python file,
[`scripts/ahv-libvirt-hook-qemu.py`](../../scripts/ahv-libvirt-hook-qemu.py). It
is installed as `/etc/libvirt/hooks/qemu`. On every libvirt event it starts or
reuses a single **manager** process, which does the work. No other file ships to
the host, and there is no systemd unit.

COM3 is a plain unix-socket serial port at boot and whenever no console is
attached. It becomes a `qemu-vdagent` chardev only while a VNC console is
connected. So an idle VM live-migrates on the first try.

The manager:

- Reads QEMU's `VNC_CONNECTED` and `VNC_DISCONNECTED` events through
  `virsh qemu-monitor-event`. Opening a console in the desktop app produces
  these events.
- Arms vdagent 3 seconds after a connect, if the live `query-vnc` client count
  is still above zero.
- Parks COM3 back to a socket 3 seconds after the last client leaves, if the
  count is still zero. A reconnect within that time cancels the park.
- Bases every decision on the live `query-vnc` count, never on a count derived
  from events.
- Runs a reconcile pass every 30 seconds to catch missed events and restarts. On
  first run it also parks idle VMs that still have vdagent attached.
- Runs as a singleton, held by a lock on `/var/run/ahvrc-mgr.lock`.

Key properties for operators:

- No guest reboot, no VM downtime and no change in how operators work. Running
  VMs move onto the model live.
- The hook only touches COM3, the agent serial port at iobase `0x3E8`. It never
  touches COM1 or Nutanix Guest Tools.
- The manager is in dry-run mode until you arm it.

## Prerequisites

- Root access to each AHV host.
- `/usr/bin/python3` and `virsh` on each host.
- `python3` and `md5sum` on the workstation you deploy from.
- For the scripted install: SSH access to each host as the `nutanix` user.
- VMs that should get the clipboard need a COM3 serial port. The manager skips
  VMs without one.

## Validate the hook before you deploy

Both deploy scripts run these checks. Run them yourself if you deploy by hand.

1. Check that the file parses:

   ```bash
   python3 -c "import ast; ast.parse(open('scripts/ahv-libvirt-hook-qemu.py').read())"
   ```

2. Run the selftest. It must exit 0:

   ```bash
   python3 scripts/ahv-libvirt-hook-qemu.py --manager-selftest
   ```

3. Record the checksum you will ship:

   ```bash
   md5sum scripts/ahv-libvirt-hook-qemu.py
   ```

The selftest checks the decision table, checks that the VNC event regex matches
a real captured `virsh` line, and checks that only one swap fires when eight
threads attach at once.

> [!CAUTION]
> Treat a selftest failure as a hard stop. A regex mismatch disables the whole
> event path without any other sign.

## Install the hook

You can install with the deploy script or by hand. The per-host installer,
[`scripts/ahvrc-deploy-onehost.sh`](../../scripts/ahvrc-deploy-onehost.sh), is the
source of truth for the steps.

> [!WARNING]
> The standard installer arms the manager in the same pass. It is not a dry-run
> deploy. To watch the manager's decisions before it changes anything, follow
> [Arm the manager](#arm-the-manager) and install without the arm flag.

### Scripted install

[`scripts/deploy-vdagent-manager.sh`](../../scripts/deploy-vdagent-manager.sh)
runs the selftest locally, then copies the hook and the per-host installer to each
host and runs the installer with `sudo`. It is idempotent.

1. Prepare credentials. The script uses the SSH key `~/.ssh/nutanix`, or the path
   in `AHVRC_SSH_KEY`. It reads the sudo password from `/tmp/test.txt`.
2. Run the script with an explicit host list. With no hosts it prints usage and
   exits:

   ```bash
   ./scripts/deploy-vdagent-manager.sh <host-1> <host-2>
   ```

3. Read the verification output that each host prints.

> [!WARNING]
> The SSH key path and the password file are conventions of the lab environment
> the script was written for. Adapt them to your own credential handling before
> you use the script against production hosts.

> [!NOTE]
> The header of `deploy-vdagent-manager.sh` says it installs a systemd unit. That
> comment is out of date. The installer it runs is hook-only and removes any
> systemd unit. Trust the per-host installer.

### Manual install

These are the steps the per-host installer runs, in order. Run them as root on
each host, with the validated hook staged at `<staged-hook>`.

1. Validate the staged file: parse it, run `--manager-selftest`, and compare its
   MD5 with the one you recorded. Stop on any failure.
2. Back up the original hook, once:

   ```bash
   [ -f /etc/libvirt/hooks/qemu.ahvrc-orig ] || cp /etc/libvirt/hooks/qemu /etc/libvirt/hooks/qemu.ahvrc-orig
   ```

3. Remove any earlier systemd supervisor:

   ```bash
   systemctl disable --now ahvrc-vdagent-manager
   rm -f /etc/systemd/system/ahvrc-vdagent-manager.service
   systemctl daemon-reload
   ```

4. Install the hook:

   ```bash
   install -m 0755 <staged-hook> /etc/libvirt/hooks/qemu
   ```

5. Arm the manager. Skip this step for a dry-run soak:

   ```bash
   touch /etc/libvirt/hooks/ahvrc-mgr-armed
   ```

6. Restart the manager on the new code. Stop the old manager, remove the stale
   lock and start one manager. After this, the hook keeps it running on libvirt
   events:

   ```bash
   pkill -9 -f "hooks/qemu --manager"
   rm -f /var/run/ahvrc-mgr.lock
   setsid python3 /etc/libvirt/hooks/qemu --manager </dev/null >/dev/null 2>&1 &
   ```

7. Verify. See [Verify a deployment](#verify-a-deployment).

## Arm the manager

The manager has two states.

- **Unarmed (dry-run):** the manager runs its full logic and logs every swap it
  would make to `/var/log/libvirt/qemu/ahvrc-mgr.log`. It changes no chardev.
  This is the safe state for a first deploy.
- **Armed:** the manager swaps COM3 between socket and vdagent.

Either signal arms it. The manager checks on every swap:

- the environment variable `AHVRC_MGR_ARMED=1` on the manager process
- the flag file `/etc/libvirt/hooks/ahvrc-mgr-armed`

The flag file survives restarts and is what the deploy scripts use.

### First deployment

1. Install the hook without the arm flag. Either install by hand and skip step 5,
   or run the scripted install, then remove the flag and restart the manager:

   ```bash
   rm /etc/libvirt/hooks/ahvrc-mgr-armed
   ```

2. Open and close a console on a test VM. Watch the log for dry-run lines:

   ```bash
   tail -f /var/log/libvirt/qemu/ahvrc-mgr.log
   ```

   Expect lines like:

   ```text
   DRY-RUN would chardev-change <uuid> charserialN -> qemu-vdagent,clipboard=on
   ```

3. Check that `--manager-report` shows the decisions you expect. See
   [Verify a deployment](#verify-a-deployment).
4. Arm the manager:

   ```bash
   touch /etc/libvirt/hooks/ahvrc-mgr-armed
   ```

   The flag is read on every decision, so you do not need to restart the manager.
5. Open and close a console again. Check that the log now shows `SWAP …` lines
   and that `--manager-report` shows the live backend.

## Verify a deployment

1. Run the read-only report. It never swaps, armed or not:

   ```bash
   python3 /etc/libvirt/hooks/qemu --manager-report
   ```

   It prints one line per running VM with a COM3:

   ```text
   manager-report: maintenance=False, N running domains
     <uuid> charserialN: clients=1 backend=vdagent migrating=False -> no-op (already correct)
     <uuid> charserialN: clients=0 backend=socket migrating=False -> no-op (already correct)
   manager-report: M VM(s) with a COM3
   ```

   - `clients` is the live `query-vnc` count.
   - `backend` is `vdagent` or `socket`, from `query-chardev`.
   - The arrow shows the steady-state decision. An idle VM is treated as past
     the grace time.
   - A VM with `clients` above 0 should reach `backend=vdagent`. A VM with
     `clients=0` should reach `backend=socket`.
   - `-> SKIP (unknown)` means `clients` or the backend could not be read. The
     manager never acts on doubt.

2. Check the installation:

   ```bash
   md5sum /etc/libvirt/hooks/qemu
   ls -l /etc/libvirt/hooks/qemu.ahvrc-orig
   ls -l /etc/libvirt/hooks/ahvrc-mgr-armed
   pgrep -f 'hooks/qemu --manager'
   tail -n 40 /var/log/libvirt/qemu/ahvrc-mgr.log
   ```

   The MD5 must match what you shipped. The arm flag is present, unless you are
   running a dry-run soak. The per-host installer prints the same facts when it
   finishes.

3. Run an end-to-end check:
   1. Pick a test VM with the in-guest agent installed and a COM3.
   2. Open a console in the desktop app. Within about 3.3 seconds,
      `--manager-report` shows `backend=vdagent`. That is the debounce plus the
      `chardev-change` round trip.
   3. Copy and paste between host and guest. This proves the agent renegotiated
      with the new backend.
   4. Close the console. Within about the 3-second grace, `--manager-report`
      shows `backend=socket`. The VM can live-migrate again. You can confirm
      with a live migration.

## Maintenance and drain override

A host drain must live-migrate every running VM off the host. A VM with an armed
vdagent chardev would refuse to migrate and stall the drain.

While the host is in maintenance, the manager parks every local VM at once,
including VMs with a console attached. It skips the grace time and refuses to arm
new consoles.

The maintenance signal is a local marker file. To drain a host:

1. Create the marker:

   ```bash
   touch /etc/libvirt/hooks/ahvrc-maintenance
   ```

2. Check that it took effect. The report header shows `maintenance=True`, and
   VMs with a console show the decision `park`:

   ```bash
   python3 /etc/libvirt/hooks/qemu --manager-report
   ```

3. Drain the host or put it in maintenance through Prism as usual.
4. Remove the marker when you are done. Consoles re-arm through their own VNC
   events:

   ```bash
   rm -f /etc/libvirt/hooks/ahvrc-maintenance
   ```

The manager applies the override on the next reconcile pass, within 30 seconds,
and on every event while the marker exists. A VM moved off the host with a
console open gets vdagent back on the destination host when its console
reconnects.

> [!WARNING]
> The marker file is the only maintenance signal. You must create it yourself, or
> through your drain automation, before you drain a host that has VMs with open
> consoles. See [Known limitations](#known-limitations).

## Roll back

To pause swapping without uninstalling, remove the arm flag. The manager keeps
running and logs in dry-run mode:

```bash
rm -f /etc/libvirt/hooks/ahvrc-mgr-armed
```

To uninstall, run these steps on each host:

1. Disable any systemd unit, if one was ever installed:

   ```bash
   systemctl disable --now ahvrc-vdagent-manager 2>/dev/null
   ```

2. Disarm the manager. It returns to dry-run at once:

   ```bash
   rm -f /etc/libvirt/hooks/ahvrc-mgr-armed
   ```

3. Restore the original hook from the backup:

   ```bash
   cp /etc/libvirt/hooks/qemu.ahvrc-orig /etc/libvirt/hooks/qemu
   ```

4. Reattach vdagent to any VM the manager left parked, or let the next boot
   inject it through the restored hook.

> [!CAUTION]
> The original hook attaches vdagent at all times. Restoring it brings back the
> migration blocker on idle VMs. Roll back only if you mean to give up the
> workaround.

## Troubleshooting

| Symptom                                 | Likely cause                                                  | Where to look                                                                 |
| --------------------------------------- | ------------------------------------------------------------- | ----------------------------------------------------------------------------- |
| Console opens, clipboard never works    | Manager unarmed, or VM has no COM3                            | `DRY-RUN would …` in `ahvrc-mgr.log`; `--manager-report`                       |
| Report lists 0 VMs with a COM3          | VMs lack the agent serial port (iobase `0x3E8`)               | `--manager-report`; provision COM3 (see [agent CLI](../reference/agent-cli.md)) |
| VM won't migrate after console closes   | Park not firing: manager dead, unarmed, or QMP unreachable    | `pgrep -f 'hooks/qemu --manager'`; `ahvrc-mgr.log`; report `backend=`          |
| Manager not running                     | No recent libvirt event to start it; stale lock               | `setsid python3 /etc/libvirt/hooks/qemu --manager &`; `/var/run/ahvrc-mgr.lock` |
| Decisions look wrong, events ignored    | `query-vnc` or `query-chardev` unknown; event regex mismatch  | `ahvrc-mgr.log`; rerun `--manager-selftest`                                   |
| Maintenance override does not park      | Marker file missing or at the wrong path                      | `ls /etc/libvirt/hooks/ahvrc-maintenance`; report header `maintenance=`       |

Two behaviours to keep in mind:

- The manager never acts on doubt. If `query-vnc` or `query-chardev` returns
  nothing, it does nothing. A VM shown as `SKIP (unknown)` is left alone on
  purpose.
- COM3 is found by its isa-serial identity, iobase `0x3E8` or target
  `port='2'`, not by its `charserialN` index. Reordering serial ports cannot make
  the manager touch COM1 or NGT.

## Reference

### Command modes

| Mode                       | Effect                                                                 |
| -------------------------- | ---------------------------------------------------------------------- |
| `<domain> <action> …`      | Normal libvirt hook call, then start the manager if needed             |
| `--manager`                | Run the singleton manager: event thread and reconcile loop             |
| `--manager-report`         | Read-only report of live state and decision per COM3 VM. Never swaps   |
| `--manager-selftest`       | Run the decision, regex and swap-race selftests. Non-zero on failure   |

The manager modes run without the Nutanix hook framework, so you can run the
selftest in CI, on a workstation or on a CVM.

### Paths and settings

| Setting                 | Value                                    |
| ----------------------- | ---------------------------------------- |
| Installed hook          | `/etc/libvirt/hooks/qemu`                |
| Original hook backup    | `/etc/libvirt/hooks/qemu.ahvrc-orig`     |
| Arm flag file           | `/etc/libvirt/hooks/ahvrc-mgr-armed`     |
| Arm environment variable | `AHVRC_MGR_ARMED=1`                     |
| Maintenance marker      | `/etc/libvirt/hooks/ahvrc-maintenance`   |
| Manager lock            | `/var/run/ahvrc-mgr.lock`                |
| Manager log             | `/var/log/libvirt/qemu/ahvrc-mgr.log`    |
| Hook trace log          | `/var/log/libvirt/qemu/hook.log`         |
| `MGR_RECONCILE_S`       | 30 s reconcile interval                  |
| `MGR_DEBOUNCE_S`        | 3 s arm debounce                         |
| `MGR_GRACE_S`           | 3 s park grace                           |
| `MGR_MIN_SWAP_S`        | 2 s hysteresis floor. Must stay at or below the grace |
| `MGR_VIRSH_TIMEOUT_S`   | 10 s timeout for each `virsh` call       |

The manager truncates its own log at 10 MiB. It does not use logrotate.

## Known limitations

- The authoritative AHV maintenance signal is not finalized in code. It could be
  an Acropolis host-state field, an ADS marker or the marker file. Confirm and
  wire it during host bring-up. Until then the marker file is the only override.
- The maintenance check is conservative. When in doubt it reports no
  maintenance, so it never parks a VM with a console on a false positive. A
  successful `virsh nodeinfo` only proves libvirt is alive and is not treated as
  a maintenance signal.

## Related documentation

- [vdagent and live migration](../architecture/live-migration.md)
- [Clipboard](../architecture/clipboard.md)
- [Agent](../architecture/agent.md)
- [Agent CLI reference](../reference/agent-cli.md)
- [Runtime proxy](../architecture/runtime-proxy.md)
- [AHV plugin](../architecture/ahv-plugin.md)
- [Scripts and tools](scripts.md)
- [Troubleshooting](troubleshooting.md)
- [Documentation index](../README.md#operations)
