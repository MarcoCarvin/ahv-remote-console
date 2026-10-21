# Troubleshooting

This page helps you when a console will not connect, the screen shows an error,
the clipboard stops working, or a VM will not live-migrate. Each problem lists
what you see, the cause and the fix. Start by capturing a diagnostics bundle.

## Contents

- [Capture a diagnostics bundle](#capture-a-diagnostics-bundle)
- [Console shows an error overlay](#console-shows-an-error-overlay)
- [Console shows "powered off" or "starting"](#console-shows-powered-off-or-starting)
- [Clipboard does not work](#clipboard-does-not-work)
- [Agent is not installed or the Mount control is missing](#agent-is-not-installed-or-the-mount-control-is-missing)
- [VM will not live-migrate](#vm-will-not-live-migrate)
- [Quick reference](#quick-reference)
- [Related documentation](#related-documentation)

## Capture a diagnostics bundle

A **diagnostics bundle** is a zip of redacted state and logs. Attach it to every
bug report or support request. You can export it even when the runtime is down.

1. Open the tray menu or the application menu and choose
   **Export Diagnostics…**. This works even when the main window is gone or
   stuck on a fatal error screen.
2. Choose where to save the file. The default name is
   `ahv-remote-console-diagnostics-<timestamp>.zip`.
3. Send that zip to support.

### What the bundle contains

From the runtime:

| File                 | Contents                                                                  |
| -------------------- | ------------------------------------------------------------------------- |
| `runtime-state.json` | Current sessions, plugin summaries, network state, UTC timestamp          |
| `config.json`        | Runtime config with custom CA file paths cleared and pinned certs blanked |
| `audit.log`          | Tail of the hash-chained audit log                                        |
| `runtime.log`        | Tail of the Go runtime log                                                |

From the desktop app, under `host-logs/`:

| File                    | Contents                                                        |
| ----------------------- | --------------------------------------------------------------- |
| `host-logs/main.log`    | Electron main-process log                                       |
| `host-logs/runtime.log` | Runtime stdout and stderr as captured by the desktop app        |
| `host-logs/about.txt`   | App version, platform and architecture, path of `runtime.log`   |

Size limits:

- Runtime logs are cut to the last 256 KiB, at a record boundary, so no JSONL
  line is half-truncated.
- Host logs are cut to the last 4 MiB each.
- A missing or unreadable log is recorded as a note in the bundle. It does not
  fail the export.

`host-logs/main.log` also holds the console window's warnings and errors,
prefixed `[console:<sessionId-prefix>]`, and clipboard reconciler lines tagged
`[reconciler.*]`. These lines are sanitized and length-capped. You do not need
DevTools open to capture them.

### What the bundle never contains

- No clipboard text. Logs and the audit log record only byte length, direction
  and SHA-256 of clipboard payloads.
- No credentials. Cookies live only in the OS keychain, and the config
  redaction blanks pinned certificates.

You can send the zip to support without leaking secrets.

### When the runtime is unreachable

If the runtime cannot be reached, for example behind the "cannot reach its local
runtime" fatal screen, the export still works. The desktop app writes a bundle
with `main.log`, `runtime.log` and an `about.txt` that states the runtime was
unreachable.

### For engineers

- The runtime serves the bundle at `GET /v1/diagnostics/bundle`. Every export,
  successful or not, is written to the audit log as `ActionDiagnosticsExport`.
- The main process signs the request with the IPC secret, like any other runtime
  call, and streams the response to disk. The SDK exposes only the bare URL.

Source: [`apps/runtime/internal/plugins/host.go`](../../apps/runtime/internal/plugins/host.go),
[`apps/desktop/src/main/diagnostics.ts`](../../apps/desktop/src/main/diagnostics.ts),
[`apps/desktop/src/main/runtime-manager.ts`](../../apps/desktop/src/main/runtime-manager.ts)

## Console shows an error overlay

When a console fails for good, it shows an overlay that names the cause and
offers only the recovery that can help. **Close** is always available.

The runtime proxy sends the cause as a string on a `fatal` control frame. An
unknown cause shows a neutral "lost connection" message.

| Cause             | Message you see                       | Button          |
| ----------------- | ------------------------------------- | --------------- |
| `session_expired` | "Please sign in again"                | **Sign in**     |
| `rbac_denied`     | "You don't have access to this VM"    | None            |
| `unreachable`     | "Lost connection to the VM"           | **Reconnect**   |
| `vm_not_found`    | "This VM is no longer available"      | None            |
| `config_error`    | "Can't connect securely to this VM"   | None            |
| `session_closed`  | "Connection to this VM dropped"       | **Reconnect**   |
| `frame_too_large` | "Display hiccup"                      | **Reconnect**   |

### "Please sign in again" (`session_expired`)

**Cause:** your Prism SSO sign-in has lapsed. The cookie is missing or no longer
valid, so a plain reconnect would fail again.

**Fix:**

1. Click **Sign in**. The SSO window opens. It can open on its own when the
   console window has focus.
2. Sign in to Prism. The console reconnects in place.

### "You don't have access to this VM" (`rbac_denied`)

**Cause:** you are signed in, but your Prism role does not allow you to open this
VM's console.

**Fix:**

1. Ask a Prism administrator to grant your role console access to the VM.
2. Open the console again.

### "Lost connection to the VM" (`unreachable`)

**Cause:** a network or VM problem, or the proxy used up its reconnect budget.
The proxy retries transient failures with backoff, up to 60 consecutive
attempts, before it reports `unreachable`.

**Fix:**

1. Click **Reconnect**.
2. If it fails again, check that the VM is running and that you can reach the
   cluster.
3. Check the runtime log in a diagnostics bundle for the dial errors.

A transient certificate problem, for example a load-balancer failover or clock
skew after wake, also shows as `unreachable`, not `config_error`.

### "This VM is no longer available" (`vm_not_found`)

**Cause:** the VM was deleted or moved to another cluster. Redialing cannot help.

**Fix:**

1. Click **Close**.
2. If the VM only moved, open it again from the VM list.

### "Can't connect securely to this VM" (`config_error`)

**Cause:** a TLS or pin fault that will happen on every attempt. Examples are an
SPKI pin mismatch, or `allow_insecure_tls` set without a valid pin. Only these
deterministic faults are terminal. Generic `x509` or certificate errors stay
retryable and show as `unreachable`.

**Fix:**

1. Click **Close**.
2. Correct the endpoint's pinned SPKI or TLS settings. See
   [Configuring endpoints](configure.md).
3. Open the console again.

### "Connection to this VM dropped" (`session_closed`)

**Cause:** the runtime ended the session. This happens when you close the tab,
when the session reaper removes it, or after a `DELETE` on the session.

**Fix:** click **Reconnect**. This creates a new session. It does not reuse the
old session ID.

### "Display hiccup" (`frame_too_large`)

**Cause:** the cluster sent a display frame larger than the client supports. This
protocol fault would happen again on the same session.

**Fix:** click **Reconnect**, which creates a new session, or close and reopen
the console.

### How fast errors appear

Terminal causes fail at once, on the first dial and on a reconnect. You do not
wait for the reconnect budget. A console that never receives a first connected
frame also fails fast instead of spinning.

Source: [`apps/runtime/internal/proxy/proxy.go`](../../apps/runtime/internal/proxy/proxy.go),
[`apps/desktop/src/renderer/components/console/status.ts`](../../apps/desktop/src/renderer/components/console/status.ts),
[`apps/desktop/src/renderer/components/console/ConsoleOverlays.tsx`](../../apps/desktop/src/renderer/components/console/ConsoleOverlays.tsx)

## Console shows "powered off" or "starting"

These are notices, not errors. They clear on their own.

### "Powered off" (`vm_powered_off`)

**Cause:** the VM is powered off. The proxy polls about every 3 seconds, with no
time limit, and the polls do not count against the reconnect budget.

**Fix:**

1. Power on the VM, from the notice or from Prism.
2. Wait. The notice clears when the VM's console comes back.

### "Starting" (`vm_starting`)

**Cause:** the VM is on, but its console is not ready yet. During that window the
cluster returns HTTP 500, 502, 503 or 504. The proxy keeps retrying with backoff.
This also applies to the first dial, so you can power on a VM and open its
console straight away.

**Fix:** wait for the console to connect.

### "Starting" never ends

**Cause:** the console is not coming up. Starting is bounded by the reconnect
budget, which lasts tens of minutes. After that the console shows `unreachable`.

**Fix:**

1. Check that the VM finished booting. Some guests stop at a firmware prompt or
   a boot menu.
2. Check the runtime log for repeated `HTTP 5xx` dial errors. They mean the
   cluster's VNC proxy is not serving this VM.
3. Check that your Prism role can open this VM's console. Mixed-fleet RBAC can
   show up in unexpected ways.

## Clipboard does not work

The clipboard runs over a `qemu-vdagent` SPICE channel on an ISA serial port
(COM3, `/dev/ttyS2` in a Linux guest). It does not use the guest network, so it
works through a full-tunnel VPN. See [Clipboard](../architecture/clipboard.md)
for the data path.

Work through these problems in order.

### A copy fails right after connecting or rebooting

**Cause:** a copy from the guest within about 30 seconds of a connect or
reconnect can be lost while the guest's vdagent is still starting. The console
also resets its clipboard state on every reconnect.

**Fix:** wait until the console has been connected for about half a minute, then
copy again.

### "A clipboard copy from the VM could not be delivered." (`clipboard_failed`)

**Cause:** the channel works, but one transfer failed after retries. The data was
dropped or could not be decoded. The notice appears so you do not paste stale
content.

**Fix:** copy again in the guest.

### Clipboard never works in either direction

**Cause:** the agent is not running in the guest, or the serial channel is not
attached.

> [!IMPORTANT]
> A green agent badge does not mean the clipboard is working. The desktop app has
> no handshake with the agent. The agent popover shows "connected" whenever the
> tab is active.

**Fix:**

1. Check in the guest that the agent is installed and running. See
   [Agent is not installed](#agent-is-not-installed-or-the-mount-control-is-missing).
2. Check that the libvirt hook is installed and armed on the host. See
   [VM will not live-migrate](#vm-will-not-live-migrate).

### The cluster cannot support the clipboard

**Cause:** the cluster's QEMU lacks `qemu-vdagent`, or the VM lacks the serial
devices it needs.

You see this diagnostic output: `NO — qemu-vdagent chardev NOT available`.

**Fix:**

1. Run the diagnostic from a CVM or AHV host:

   ```bash
   ssh root@<cvm-or-host> 'bash -s' < scripts/ahv-clipboard-diagnostic.sh <vm-uuid>
   ```

   The VM UUID is optional. The script checks the QEMU version, `qemu-vdagent`
   support, the VM's serial and channel devices, and spice, vdagent and NGT
   packages.
2. If it reports that the chardev is not available, the clipboard cannot work on
   this cluster. No client change can fix this.

### The clipboard works only after a few seconds, or only with a console open

**Cause:** this is expected. The libvirt hook switches COM3 to `qemu-vdagent`
only while a console is attached, about 3 seconds after it connects. It switches
back about 3 seconds after the last console closes. The agent retries and
renegotiates to handle this delay.

**Fix:** none needed. See [vdagent and live migration](../architecture/live-migration.md).

## Agent is not installed or the Mount control is missing

The in-guest agent is optional. You need it for clipboard sync and display
sizing. The desktop app cannot detect it, so check in the guest.

### Install the agent

1. In the console toolbar, open the agent popover and choose
   **Mount installer ISO**. The runtime uploads the agent ISO (about 11 MB) to
   the Prism Image Service and attaches it as a CD-ROM. The call takes about
   30 seconds. The upload is the slow step.
2. In the guest, run `ahv-remote-console-agent` once with no flags. It runs the
   installer, which is a GUI on Windows and the daemon elsewhere. It sets up
   per-user autostart, with `HKCU\Run` on Windows and a systemd `--user` unit
   on Linux.

AHV cannot hot-add the serial channel. If the VM was built without it, the mount
flow can offer a graceful power cycle to add it. This happens only when you
click **Restart now**.

See the [agent CLI reference](../reference/agent-cli.md) for all modes.

### The setup controls are missing (`vm_update_denied`)

You see: "Your Prism role doesn't allow VM configuration changes — ask an
administrator to run setup." The **Install prerequisites** control is absent.

**Cause:** your Prism role cannot change VM specs. The app hides a control only
after a read-only probe or a real Prism 403 proves the permission is missing. An
unknown permission is never treated as denied.

**Fix:** ask a Prism administrator to run the setup for this VM.

### Mount fails with "publish required" (`image_publish_required`)

You see: "ask your administrator to publish the agent image."

**Cause:** the agent image is not in the cluster's image catalog, and your role
cannot add images.

**Fix:** ask a Prism administrator to publish the agent image, then mount again.

### Mount fails with another error

**Cause:** the runtime reports a one-line reason inline.

**Fix:** read the reason, correct the problem, and click **Retry**.

### Check the serial uplink in a Windows guest

Run this in the guest:

```bash
ahv-remote-console-agent --uplink-probe
```

It runs for about 50 seconds and shows whether the serial uplink that the
clipboard needs works. It is Windows-only and returns an error on Linux and
macOS. See [`--uplink-probe`](../reference/agent-cli.md#--uplink-probe).

Source: [`apps/desktop/src/renderer/components/useAgent.ts`](../../apps/desktop/src/renderer/components/useAgent.ts),
[`apps/desktop/src/renderer/components/capabilityGate.ts`](../../apps/desktop/src/renderer/components/capabilityGate.ts)

## VM will not live-migrate

### A VM with an open console cannot migrate

**Cause:** `qemu-vdagent` holds an unconditional QEMU migration blocker while it
is active. A native fix is expected in AHV 12.0. The libvirt hook makes COM3 a
vdagent channel only while a console is attached. VMs without a console stay
migratable.

**Fix:**

1. Ask every operator who may have the VM open to close its console window. A
   console open on any one computer keeps the VM blocked.
2. Wait a few seconds. The hook switches COM3 back to a plain socket about
   3 seconds after the last console closes.
3. Migrate the VM. If you cannot reach every operator, power the VM off and use a
   cold migration, which is always possible.

To drain a host, make sure every operator has closed their console windows for the
VMs on it, or use the maintenance override in [Setting up AHV hosts](hypervisor-setup.md#maintenance-and-drain-override).
The override parks every VM, including VMs with a console open.

### VMs stay blocked, or the clipboard never arms

You see lines like `DRY-RUN would chardev-change …` in
`/var/log/libvirt/qemu/ahvrc-mgr.log`.

**Cause:** the hook's manager is not armed. Unarmed, it logs what it would do and
changes nothing. COM3 stays in whatever state it is in. If vdagent is active, the
VM stays blocked. If vdagent was never switched in, the clipboard does not work.

> [!WARNING]
> Check that the hook is installed and armed on every AHV host before you rely on
> clipboard sync or on VMs becoming migratable when their console closes.

**Fix:**

1. Arm the manager on the host. Set `AHVRC_MGR_ARMED=1`, or create the flag file:

   ```bash
   touch /etc/libvirt/hooks/ahvrc-mgr-armed
   ```

2. Follow [Setting up AHV hosts](hypervisor-setup.md) to install and verify the
   hook.

## Quick reference

| Symptom                                    | Section                                                                                         |
| ------------------------------------------ | ----------------------------------------------------------------------------------------------- |
| Need logs for support                      | [Capture a diagnostics bundle](#capture-a-diagnostics-bundle)                                   |
| Red error overlay on the console           | [Console shows an error overlay](#console-shows-an-error-overlay)                               |
| "Powered off" or "starting" notice         | [Console shows "powered off" or "starting"](#console-shows-powered-off-or-starting)             |
| Clipboard does not sync                    | [Clipboard does not work](#clipboard-does-not-work)                                             |
| Mount control missing or agent absent      | [Agent is not installed](#agent-is-not-installed-or-the-mount-control-is-missing)               |
| VM will not live-migrate with console open | [VM will not live-migrate](#vm-will-not-live-migrate)                                           |

## Related documentation

- [Runtime proxy](../architecture/runtime-proxy.md)
- [Clipboard](../architecture/clipboard.md)
- [Agent](../architecture/agent.md)
- [Agent CLI reference](../reference/agent-cli.md)
- [vdagent and live migration](../architecture/live-migration.md)
- [Setting up AHV hosts](hypervisor-setup.md)
- [Configuring endpoints](configure.md)
