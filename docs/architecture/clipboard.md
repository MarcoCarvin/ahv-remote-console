# Host and guest clipboard

The clipboard lets you copy text on your workstation and paste it in the VM, and
the reverse. This page explains how text travels in each direction, how loops
and oversized payloads are stopped, and why clipboard text never reaches a log.
It is for developers and for operators who want to know what happens to their
data.

## How it works

The clipboard never uses the guest's network. Text travels inside the console
connection you already have open, then over a virtual serial port into the
guest. Four components carry it:

1. The **desktop app main process** reads and writes the workstation clipboard.
2. The renderer's **`ClipboardEngine`** sends and receives clipboard text as RFB
   messages, cleans it, and stops echoes.
3. The **runtime proxy** sits between the renderer and Prism's VNC connection.
   It translates QEMU's Extended Clipboard messages into the plain form the
   renderer reads.
4. The **agent** inside the VM speaks the SPICE vdagent protocol over the serial
   port and reads and writes the guest clipboard.

QEMU sits in the middle. Its clipboard subsystem connects RFB messages on the
VNC side to vdagent messages on the serial side.

```text
 workstation clipboard                               guest clipboard
        |                                                   |
   main process --IPC-- renderer --WS-- runtime proxy       agent
                                            |                 |
                                    Prism VNC --> QEMU --serial (COM3)
```

What you need on each VM:

- The agent running inside the VM. See [In-guest agent](agent.md).
- The agent serial port (`COM3` on Windows, `/dev/ttyS2` on Linux). The AHV
  host's libvirt hook arms the `qemu-vdagent` backend on it while a console is
  connected. See [Hypervisor setup](../operations/hypervisor-setup.md).

What you will notice:

- Only text is transferred. Images and files are not.
- Copying the same text again a few seconds later still transfers. Echo
  suppression is limited in time.
- Text from the guest arrives on your workstation without a trailing newline.
  A compromised guest therefore cannot run a command the moment you paste into
  a terminal. You press Enter yourself.
- A failed transfer, for example one that is too large or a locked OS
  clipboard, shows an error. Your copy is not lost silently.

## Host to guest

```text
workstation clipboard
  -> main process poll (150 ms) -> IPC clipboard-changed {text, seq}
  -> ClipboardEngine.forwardHostToVM
  -> RFB ClientCutText over the loopback WebSocket
  -> runtime proxy (passes it through)
  -> Prism VNC -> QEMU clipboard -> qemu-vdagent on COM3
  -> agent -> guest clipboard
```

1. The main process sees the change. A 150 ms timer reads the clipboard
   with Electron's `clipboard` module. On a text change it increments a sequence
   number and sends `ahv-remote-console:clipboard-changed` with `{text, seq}`.
   It sends this only to trusted app windows, never to the SSO window.
2. The renderer forwards it. `startHostPoll` first reads the current `seq` as
   a baseline. It then forwards every event whose `seq` is greater than that
   baseline, including your first copy.
3. The renderer cleans and checks the text. `forwardHostToVM`:
   - converts CRLF and CR to LF, so both directions share one form
   - removes dangerous invisible characters with `sanitizeGuestText`
   - rejects text over 10 MB and reports an error
   - records an echo-suppression hash before sending
4. The renderer sends RFB `ClientCutText`. `sendClientCutText` builds a
   standard positive-length `ClientCutText` (message type 6). Payloads over 2 KB
   go as chunked frames, because one `ClientCutText` over 900 KiB makes QEMU
   drop the console session. The RFB client also rejects a single frame over
   900 KiB. The renderer never sends the negative-length Extended Clipboard
   form. Only the proxy uses that format.
5. The proxy passes it through. It notes clipboard activity, which delays
   keep-warm messages, and forwards the frame unchanged.
6. QEMU hands it to the guest. It turns the `ClientCutText` into a vdagent
   `CLIPBOARD_GRAB`, then sends the `CLIPBOARD` data when the agent asks.
7. The agent writes the guest clipboard. It checks that the type is UTF-8
   text and sanitizes the text again (`sanitizeClipboardText`). It records an
   echo marker, then writes the clipboard, with up to 3 attempts for Windows
   `OpenClipboard` contention. If the write fails, it clears the marker and
   returns an error.

## Guest to host

```text
guest clipboard change
  -> agent sends CLIPBOARD_GRAB over COM3
  -> QEMU -> Extended Clipboard NOTIFY on VNC
  -> proxy sends REQUEST; QEMU asks the agent; agent replies
  -> QEMU -> Extended Clipboard PROVIDE (zlib)
  -> proxy turns it into legacy ServerCutText
  -> renderer -> ClipboardEngine.handleGuestClipboard
  -> IPC clipboard-write -> workstation clipboard
```

1. The agent sees the guest copy. Each guest clipboard backend has a
   `Watch` channel. Windows uses `AddClipboardFormatListener`. Linux on Wayland
   uses `wl-paste --watch`. Both fall back to polling when the native path is
   missing. `pollClipboard` reads that channel.
2. The agent drops echoes. It reads the clipboard with a 2 s timeout. It
   drops the text if it equals the last text, or if its hash matches the marker
   from its own last write. That marker is used once and expires after 30 s.
3. The agent announces the text. It stores the text in a grab cache and
   sends `CLIPBOARD_GRAB` (type 7). When `CLIPBOARD_REQUEST` (type 8) arrives,
   it answers from the cache. Reading the OS clipboard a second time can stall
   for about 2 s on Windows or return nothing. The cache is used once. It is
   wiped on use, on the next watch event, on read-loop recovery, and after 10 s.
4. The proxy asks for the data. QEMU reports the grab as an Extended
   Clipboard `NOTIFY`. The proxy answers with a `REQUEST` and sets a retry
   deadline. QEMU fetches the text from the agent and sends it back as a
   `PROVIDE`: a `ServerCutText` with a negative length and a zlib payload.
5. The proxy translates it. `TranslateExtClipServerCutText` checks that the
   message is a PROVIDE with text, decompresses it (16 MB cap), and builds a
   plain positive-length `ServerCutText` for the renderer.
6. The renderer decodes it. The RFB client decodes the text as strict UTF-8
   and rejects truncated or corrupt payloads, so no replacement characters land
   on your clipboard. `ConsoleView` passes the text to
   `ClipboardEngine.handleGuestClipboard`.
7. The engine cleans and writes it. `handleGuestClipboard`:
   - rejects payloads over 10 MB
   - converts CRLF and CR to LF and strips trailing line breaks, including
     U+0085, U+2028 and U+2029, so `curl evil | sh\n` cannot run on paste
   - sanitizes with `sanitizeGuestText` before hashing, so hashes match across
     both directions
   - drops the text if the opposite direction recorded the same hash recently
   - writes with `clipboardWrite` and reports an error on failure. The main
     process retries once on contention.

## Extended Clipboard and vdagent

Two wire protocols meet at QEMU.

### VNC side

AHV's QEMU 9.1 sends clipboard data with the **Extended Clipboard
pseudo-encoding** (`0xC0A1E5CE`), not with legacy `ServerCutText`. The renderer's
RFB client does not speak it, so the proxy translates.

- When the renderer sends `SetEncodings`, the proxy adds `0xC0A1E5CE`. Without
  it, QEMU does not use Extended Clipboard.
- Right after that, the proxy sends an Extended Clipboard `CAPS` message once
  per connection. It advertises text with a 10 MB maximum. QEMU may never send
  `CAPS` first, and without ours a guest copy never reaches you.
- The message type is in the high bits of a 4-byte flags word: `CAPS` (`1<<24`),
  `REQUEST` (`1<<25`), `PEEK` (`1<<26`), `NOTIFY` (`1<<27`), `PROVIDE`
  (`1<<28`). The only data type is `TEXT` (`1<<0`).
- A `NOTIFY` means new data. The proxy replies with a `REQUEST`. The `PROVIDE`
  carries the zlib-compressed text.
- If a `PROVIDE` fails to decode, the proxy sends the renderer a
  `clipboard_failed` control frame and counts it in telemetry. If no `PROVIDE`
  arrives by the deadline, the proxy sends the `REQUEST` again a limited number
  of times, then sends `clipboard_failed`.
- AHV splits large Tight framebuffer updates across WebSocket frames. To avoid
  mistaking one of those for clipboard data, the proxy treats raw data as
  Extended Clipboard only when the 3-byte padding is zero and the whole message
  is in the frame. Anything uncertain is forwarded unchanged.

### Serial side

The agent speaks the SPICE vdagent protocol over a plain ISA serial port. QEMU's
vdagent backend does not depend on the serial frontend, so no virtio-serial
device and no PCI hotplug are needed.

- The port is `COM3` on Windows and `/dev/ttyS2` on Linux, opened raw at
  115200 8N1 with exclusive access. `COM1` belongs to Nutanix Guest Tools and is
  never touched.
- Each message is an 8-byte chunk header plus a 20-byte message header and the
  payload, all little-endian. Reassembly timeouts limit the gap between chunks,
  not the total time, so a large paste over a slow link is not dropped.
- The agent advertises `CapClipboardByDemand` (bit 5) and
  `CapClipboardSelection` (bit 6). Clipboard is on only when the host replies
  with by-demand. Because the agent advertises selection support, it echoes the
  host's selection byte in its replies.
- The libvirt hook swaps the `COM3` backend from a socket to `qemu-vdagent` a few
  seconds after a console attaches. This interrupts the serial stream. The
  agent backs off on read errors, resets reassembly and the grab cache, and
  sends its capabilities again. It repeats them every 5 s until negotiated and
  every 15 s after that, so a new peer after a swap or a migration is answered
  without a restart.

The full wire format is in [In-guest agent](agent.md#wire-format).

## Why it works under a full-tunnel VPN

Inside the guest, the agent exchanges bytes over a serial port. QEMU emulates
that port on the host. It is not a network interface, so the guest's routes,
VPN client and firewall do not affect it. A VPN that captures all guest IP
traffic leaves the serial channel alone.

From QEMU to you, the text rides the VNC console stream that also carries the
screen. That stream runs from the AHV host through Prism to the runtime. It does
not start inside the guest OS. There is no network connection between host and
guest for a VPN to break.

## Echo suppression

Without suppression, a two-way clipboard echoes forever. A host write changes
the guest clipboard, the guest watcher fires and sends it back, the host poll
fires, and so on. Two layers stop this.

- **Agent.** The agent drops a guest change whose hash matches the one-shot
  marker from its own last write. The marker expires after 30 s.
- **Renderer.** The `Ledger` records each transfer as direction, SHA-256 hash,
  format and time. It reports a loop when the opposite direction recorded the
  same hash within 10 s. It stores hashes, never text, and keeps at most 128
  entries. It is cleared when the session reconnects.

The 10 s window means a deliberate re-copy of the same text a little later
still goes through. Both directions hash the same canonical form: sanitized,
LF line endings, trailing newlines removed. A guest echo of host text is
therefore recognised.

Source: [`apps/desktop/src/renderer/clipboard/Ledger.ts`](../../apps/desktop/src/renderer/clipboard/Ledger.ts)

## No clipboard text in logs or audit

Clipboard contents are never written to a log or an audit record. Any record
about a clipboard transfer may carry only the byte length, the direction and a
SHA-256 hash.

> [!NOTE]
> Clipboard audit records are not written in this release. The runtime defines
> the actions `clipboard.host_to_vm` and `clipboard.vm_to_host`, but no code
> appends them. See [Audit log and application logs](audit-and-logging.md).

How the rule is held in code:

- The renderer type `ClipboardMeta` has no text, payload or content field. It
  has only `direction`, `hash`, `bytes`, `format`, `ts`, `sessionId` and
  `truncated`.
- The agent logs only the message type and payload length. A clipboard
  message can hold a pasted password, and the guest log is readable inside the
  guest. Capability handling logs decoded flags, not raw bytes.
- The agent zeroes clipboard buffers after use (`wipeBytes`, `wipeGrabCache`).
  The grab cache is used once and expires after 10 s. The guest clipboard
  backends zero temporary copies and never write to disk.
- When the renderer strips dangerous characters, it logs a scrubbed preview of
  at most 32 characters for diagnostics. This is the only place a fragment of
  clipboard text can reach a log, the renderer console log.

## Control tokens

The clipboard channel also carries a few control tokens. They are not operator
data and never reach either clipboard. They are caught before any clipboard
write and are not passed to the renderer.

| Token | Purpose |
| --- | --- |
| `__AHVRC_RESIZE__<WxH>` | display resize, handled by the agent |
| `__AHVRC_KEEPWARM__<ttl_s>` | Windows keep-warm lease |
| `__AHVRC_KEEPWARM_ACK__` | the agent's reply to the keep-warm probe |

The proxy sends a lease only after the agent acknowledges the probe, so an
older agent never receives one. Keep-warm is Windows-only and does nothing on
Linux guests. The proxy holds keep-warm messages back while real clipboard
traffic is recent, so they never compete with a copy. Details are in
[In-guest agent](agent.md#keep-warm-windows-only).

## Live migration

The `qemu-vdagent` backend blocks QEMU live migration for as long as it exists.
The libvirt hook keeps `COM3` a plain socket while no console is connected and
arms `qemu-vdagent` only during a console session. It parks the port again
about 3 s after the last console closes. Idle VMs and VMs with closed consoles
can therefore migrate. The agent's capabilities heartbeat restores the
clipboard on each attach, including on the migration target. See
[vdagent and live migration](live-migration.md).

## Size limits

| Limit | Value |
| --- | --- |
| Host to guest text, renderer | 10 MB |
| Single RFB `ClientCutText` frame | 900 KiB |
| Host to guest frame before chunking | 2 KB |
| Guest to host text, renderer | 10 MB |
| Renderer to runtime WebSocket message (session drops above it) | 8 MB |
| Extended Clipboard decompressed size | 16 MB |
| Guest to host text, agent | 256 KB |
| Inbound clipboard payload, agent | 10 MB |
| Inbound vdagent message, agent | 16 MB |
| Renderer echo window | 10 s |
| Agent echo marker | 30 s, used once |
| Agent grab cache | 10 s |

The agent caps a single guest-to-host transfer at 256 KB
(`maxOutboundClipboard`). That is much smaller than the renderer's 10 MB limit,
because a larger transfer would occupy the serial link for too long.

Source: [`ClipboardEngine.ts`](../../apps/desktop/src/renderer/clipboard/ClipboardEngine.ts),
[`extclip.go`](../../apps/runtime/internal/rfb/extclip.go),
[`proxy.go`](../../apps/runtime/internal/proxy/proxy.go),
[`agent.go`](../../apps/ahv-remote-console-agent/internal/vdagent/agent.go)

## Related documentation

- [In-guest agent](agent.md)
- [vdagent and live migration](live-migration.md)
- [Runtime proxy](runtime-proxy.md)
- [Audit log and application logs](audit-and-logging.md)
- [Connect and clipboard](../operations/connect-and-clipboard.md)
- [Data handling](../security/data-handling.md)
