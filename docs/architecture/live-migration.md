# vdagent and live migration

The clipboard needs a `qemu-vdagent` backend on the guest's serial port, and
that backend blocks QEMU live migration. This page explains the blocker, the
host-side hook that works around it, and how to check that the hook works. It
is for developers and for the administrators who run the AHV hosts.

The hook attaches `qemu-vdagent` only while a console is open. The rest of the
time the serial port is a plain socket, which does not block migration.

```text
 console opens        console open              console closes
 VNC_CONNECTED   -->  COM3 = qemu-vdagent  -->  VNC_DISCONNECTED
 wait ~3 s            clipboard works           wait ~3 s
 arm                  VM cannot migrate         park: COM3 = socket
                                                VM can migrate again
```

For operators, this means:

- A VM with an open console cannot live-migrate.
- About 3 s after you close the console, the VM can migrate again.
- Idle VMs, and VMs that never had a console open, can migrate from power-on.
- When a host enters maintenance, the hook parks every VM so the drain can
  proceed. See [Maintenance override](#maintenance-override).
- You need no host access and no change in how you work.

## 1. The blocker

The clipboard attaches a `qemu-vdagent` backend to the agent serial port:
`COM3` on Windows, `/dev/ttyS2` on Linux. On the host, the hook finds `COM3` by
its ISA I/O base `0x3E8` (libvirt target `port='2'`), never by its
`charserialN` index. `COM1` (I/O base `0x3F8`) belongs to Nutanix Guest Tools
and is never touched.

In AHV's QEMU 9.1, `qemu-vdagent` registers a live-migration blocker for the
whole life of the backend. `vdagent_chr_open` calls `migrate_add_blocker`, and
only `vdagent_chr_fini` lifts it. AHV's build has no setting to turn it off.
Nutanix has confirmed this and plans native migration support in AHV 12.0. A
plain socket-backed serial port adds no blocker.

While `COM3` has a `qemu-vdagent` backend, the VM cannot live-migrate.

### Effect on cluster operations

- **Host maintenance.** A drain must live-migrate every running VM off the host.
  One VM with an armed backend stops the drain. This is why the hook parks all
  local VMs when the host enters maintenance.
- **Acropolis Dynamic Scheduler (ADS).** ADS migrates on a best-effort basis and
  skips a blocked VM.
- **HA failover.** Not affected. HA powers the VM on again on a new host and
  migrates no live state.

### Approaches that do not work

- AHV's libvirt has no source-side pre-migration hook that could remove the
  backend right before a migration starts.
- AHV's `qemu-vdagent` has no setting to suppress the blocker.
- `qemu-vdagent` cannot be replaced by an external relay. It is the bridge
  inside QEMU between the guest agent and the VNC clipboard. A process outside
  QEMU has no way to deliver text into the VNC session.

## 2. Console-following vdagent

The hook makes `qemu-vdagent` a short-lived state that exists only while a
console is connected. At all other times `COM3` is a plain Unix-socket serial
port. An idle VM, or one that never had a console open, migrates on the first
try with no detection step and no failure path.

All of this runs on the host, in one file:
[`scripts/ahv-libvirt-hook-qemu.py`](../../scripts/ahv-libvirt-hook-qemu.py).
It is both the libvirt hook and, with `--manager`, a long-running control loop
called the **manager**.

### No vdagent at boot

The hook does not attach `qemu-vdagent` at boot. `COM3` stays a socket from
power-on. The function `inject_vdagent_clipboard` remains in the file as a
reference for the `chardev-change` command, but the hook does not call it. On
every libvirt event, the hook checks with a non-blocking `flock` that the
single manager process is running, and starts it if not.

### The trigger is the proxy's own VNC session

The manager follows QEMU's own VNC client events, `VNC_CONNECTED` and
`VNC_DISCONNECTED`. It reads them through libvirt's existing monitor connection
with `virsh qemu-monitor-event --loop --timestamp`. It opens no second QMP
monitor.

That VNC client is the runtime proxy of the desktop app:

- When you open a console, the proxy dials Prism's VNC WebSocket (`OpenStream`
  in [`plugins/ahv/plugin.go`](../../plugins/ahv/plugin.go)). QEMU sees a new VNC
  client and emits `VNC_CONNECTED`.
- When the console closes, or the proxy reconnects after an upstream failure,
  the proxy closes the upstream connection at the end of its run-loop
  iteration. QEMU emits `VNC_DISCONNECTED`. A reconnect produces a disconnect
  followed by a connect.

The desktop app knows nothing about vdagent, parking or migration. Opening and
closing a console is the trigger.

### Event-line parsing

`MGR_EVENT_RE` matches each event line. This `virsh` build prints:

```text
... : event VNC_CONNECTED for domain '<name>': {json}
```

The domain is a single-quoted name with no UUID in parentheses. On AHV the
domain name is the VM UUID, so it matches the `virsh list --uuid` output the
reconcile loop uses. The selftest checks `MGR_EVENT_RE` against a real captured
line, because a pattern that matches nothing turns off the whole event path
without an error.

### Live client count, not an event count

The manager does not count clients from events. Such a count drifts: the proxy
can open more than one client per console, connect and disconnect events can
arrive out of balance, and events are missed during restarts. A wrong count
would leave a VM armed and unable to migrate.

Instead, each event starts a short-lived thread for its wait, so one VM never
delays another. After the wait, the decision uses the live client count from
`query-vnc`.

### The swap commands

Both swaps are HMP `chardev-change` commands on the `COM3` backend id
(`_mgr_swap`):

| Swap | Command argument |
| --- | --- |
| Arm (socket to vdagent) | `qemu-vdagent,clipboard=on` |
| Park (vdagent to socket) | `socket,path=<COM3 socket path>,server=on,wait=off` |

HMP is used because it sets `clipboard=on` through `vdagent_chr_parse`. QMP
needs the option passed explicitly. Park restores the socket libvirt created at
start, using the path from the live domain XML, so the running state matches
the domain XML again.

An empty HMP response means success. Any other response is an error.

## 3. Lifecycle

### Console connect (arm)

1. `VNC_CONNECTED` arrives. A handler thread waits the debounce,
   `MGR_DEBOUNCE_S` (about 3 s).
2. A short probe connection that closes during the debounce leaves the live
   count at 0, so nothing is armed.
3. After the debounce, the handler arms only if the `query-vnc` count is above 0.
4. If the VM is migrating, the arm is skipped, so the blocker never returns on a
   moving VM. The arm is also skipped while the host is in maintenance.
5. The manager runs `chardev-change <com3> qemu-vdagent,clipboard=on`.
6. The agent's capabilities heartbeat finds the new backend and the clipboard
   starts. See [section 4](#4-the-agent-restores-the-clipboard).

Measured time from console open to armed: about 3.3 s, which is the debounce
plus the `chardev-change` round trip.

### Console disconnect (park)

1. `VNC_DISCONNECTED` arrives. A handler thread waits the grace, `MGR_GRACE_S`
   (about 3 s).
2. After the grace, it checks the live `query-vnc` count again. A reconnect in
   that time, such as a network blip or a pop-out window re-attaching, keeps
   the count above 0 and cancels the park.
3. If the count is still 0, the manager parks `COM3` back to a socket.
4. Removing the vdagent backend removes the migration blocker. The VM can
   migrate.

The grace is short on purpose. The usual reason to close a console is to
migrate the VM, so it must become migratable within seconds. Park time is about
the grace, whatever the session length. The minimum gap between swaps,
`MGR_MIN_SWAP_S`, is kept at or below the grace. A short session (open, paste,
close) therefore parks on time instead of waiting for the 30 s reconcile.

### Decision rules

The pure function `manager_decide` returns `attach`, `park` or nothing. A table
in the selftest covers it.

- Attach at once when a client is present and `COM3` is a socket, subject to
  the minimum swap gap.
- Park only when no client has been present for at least `MGR_GRACE_S`.
- Do nothing if `query-vnc` or `query-chardev` returns an unknown value.
- Never swap twice within `MGR_MIN_SWAP_S`, so a flapping connection cannot
  thrash the backend.
- Do nothing when `COM3` already has the target backend.
- Never touch `COM1`. Only the `0x3E8` backend is swapped.

The swap itself (re-check the backend, check the swap gap, run `chardev-change`,
record the time) runs under one shared lock. Event threads and the reconcile
thread therefore cannot send competing commands to the same port. A selftest
calls `attach` from eight threads and checks that exactly one swap happens.

### Maintenance override

When the host enters maintenance, the manager parks all local VMs at once,
without the grace, including VMs with an open console. During a drain,
migration matters more than the clipboard. A VM with a console that moves to
another host gets its clipboard back there when its console reconnects. A
console that connects during the drain does not arm.

> [!IMPORTANT]
> The source of the maintenance signal on AHV is not settled. The current
> `maintenance_active` checks for a local marker file,
> `/etc/libvirt/hooks/ahvrc-maintenance`, and returns false otherwise, so a
> false positive never parks a console session. Confirm the right source (an
> Acropolis host-state field, an ADS marker or a marker file) when you bring up
> a host.

### Reconcile loop

Every 30 s (`MGR_RECONCILE_S`), a loop runs `query-vnc` and `query-chardev` for
each VM, decides with `manager_decide`, and acts through the same locked
`attach` and `park`. It covers missed events, dropped monitor streams and
manager restarts.

- A VM with VNC clients and a socket backend is armed.
- A VM with no clients past the grace and a vdagent backend is parked.
- Everything else is left alone.

### Dry run until armed

The manager makes no changes unless it is armed, either with the environment
variable `AHVRC_MGR_ARMED=1` or with the flag file
`/etc/libvirt/hooks/ahvrc-mgr-armed`. Unarmed, it logs each decision it would
make and swaps nothing. You can watch a new deployment before it changes
anything.

## 4. The agent restores the clipboard

Nothing on the host restores the clipboard after an arm. The agent inside the
guest does it. It sends its capabilities every 5 s until the clipboard is
negotiated, then every 15 s for as long as it runs. Each message asks the host
for its capabilities. A new vdagent peer, after an arm, after a manager
restart, or on a migration target, is therefore answered within that interval,
with no agent restart.

A swap can interrupt a read in progress. The agent treats a stall inside a
frame as the backend going away, resets reassembly and carries on. Details are
in [In-guest agent](agent.md#capabilities-and-backend-swaps).

## 5. Components

| Component | Role |
| --- | --- |
| libvirt hook (`scripts/ahv-libvirt-hook-qemu.py`) | On every libvirt event, makes sure the manager is running. Leaves `COM3` a socket at boot. |
| Manager (`--manager`, same file) | Reads VNC events, gates on the live `query-vnc` count, arms and parks with debounce, grace, migration guard and maintenance override, and runs the 30 s reconcile. One instance per host, held by `flock`. |
| Runtime proxy (`apps/runtime/internal/proxy`, `plugins/ahv`) | Its upstream VNC connection is the QEMU VNC client whose events drive the manager. It knows nothing about vdagent. |
| Agent (`ahv-remote-console-agent --daemon`) | Sends capabilities on a permanent heartbeat and restores the clipboard whenever the backend appears. |

## 6. Deployment

Copy `scripts/ahv-libvirt-hook-qemu.py` into the host's libvirt hooks directory
as the `qemu` hook. No other file ships. Running VMs move to the new model with
no reboot:

1. The hook starts the manager on the next libvirt event, or you start it
   directly.
2. On its first pass, the reconcile loop parks every VM that has a vdagent
   backend and no VNC client. VMs with an open console stay armed.
3. From then on, each console session follows the arm and park lifecycle.

To make the manager act instead of dry-run, arm it. See
[Dry run until armed](#dry-run-until-armed). Full host steps are in
[Hypervisor setup](../operations/hypervisor-setup.md).

## 7. Verifying the hook

Run the selftest. It covers the decision table, the event pattern and the swap
race, and exits non-zero on failure.

```bash
python3 scripts/ahv-libvirt-hook-qemu.py --manager-selftest
```

On a host, print the live state and the decision for each running VM with a
`COM3`. This command is read-only and never swaps, armed or not.

```bash
python3 /etc/libvirt/hooks/qemu --manager-report
```

```text
manager-report: maintenance=False, N running domains
  <uuid> charserialN: clients=1 backend=vdagent migrating=False -> no-op (already correct)
  <uuid> charserialN: clients=0 backend=socket migrating=False -> no-op (already correct)
manager-report: M VM(s) with a COM3
```

A VM with `clients>0` should settle on `backend=vdagent`. A VM with `clients=0`
should settle on `backend=socket`. `-> SKIP (unknown)` means the manager could
not read the state and will not act.

Check the manager process and its log:

```bash
pgrep -f 'hooks/qemu --manager'
tail -n 40 /var/log/libvirt/qemu/ahvrc-mgr.log
```

In dry-run mode the log shows `DRY-RUN would ...` lines.

End-to-end check:

1. Open a console to a VM that has the agent and a `COM3`.
2. Within about 3.3 s, `--manager-report` shows `backend=vdagent`.
3. Copy and paste in both directions. This proves the agent found the new
   backend.
4. Close the console. Within about 3 s, `--manager-report` shows
   `backend=socket`.
5. Optionally, live-migrate the VM.

## 8. Native support in AHV 12.0

Nutanix plans native migration of the vdagent device in AHV 12.0. QEMU would
then migrate the device state instead of blocking. When that ships, `COM3` can
keep `qemu-vdagent` all the time, and the arm and park lifecycle, the parking
logic and the maintenance override can be removed.

> [!WARNING]
> Do not remove the hook until you have confirmed the AHV 12.0 behaviour on
> your deployed AHV build and tested migration with the native path live.

## Keep-warm uses the same channel

The proxy also sends keep-warm tokens (`__AHVRC_KEEPWARM__`) over the vdagent
channel to counter QEMU's idle refresh backoff. Keep-warm has no effect on the
migration blocker. It runs on Windows guests only and does nothing on Linux.
See [In-guest agent](agent.md#keep-warm-windows-only).

## Related documentation

- [Hypervisor setup](../operations/hypervisor-setup.md)
- [Clipboard](clipboard.md)
- [In-guest agent](agent.md)
- [Runtime proxy](runtime-proxy.md)
