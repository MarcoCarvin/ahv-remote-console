# ADR 0006: Clipboard uses qemu-vdagent on a serial port, not the guest network

## Status

Accepted

## Context

Host-to-guest clipboard sync is the main feature of the product. Nutanix Guest
Tools (NGT) does not provide a working host-to-guest clipboard. Browser-based
consoles that carry the clipboard over a separate channel break when the guest
is behind a full-tunnel VPN.

The clipboard must keep working when the guest's own network is fully tunnelled.
A relay over the guest network would be affected by a default-route VPN, so the
design avoids the guest IP stack. The repository does not record other
transports considered, such as a guest agent over TCP or a virtio channel.

## Decision

Clipboard data moves on two legs. Neither uses the guest IP stack.

1. Between the console window and the runtime, Extended Clipboard messages ride
   the existing VNC/RFB stream. The runtime translates them to and from legacy
   `ServerCutText`.
2. Between QEMU and the guest, the SPICE vdagent protocol runs over the ISA
   serial port the agent owns. That is COM3 on Windows and `/dev/ttyS2` on Linux.
   QEMU attaches a `qemu-vdagent` chardev to that port. The agent reads and
   writes the guest OS clipboard.

## Consequences

- Clipboard sync works under a full-tunnel VPN, with no guest TCP or UDP link.
- The serial link is slow. The agent caps a guest-to-host transfer at 256 KB so
  one transfer does not monopolise the link. The renderer accepts up to 10 MB.
- A `qemu-vdagent` chardev blocks QEMU live migration while it is attached. A
  libvirt hook follows the console. The port is a plain socket while idle, and
  the vdagent backend is attached only while a console is connected. The hook
  parks it about 3 seconds after the last console disconnects.
- Only text is transferred, not images or files.
- Both sides watch each other's clipboards, so loop suppression is needed. The
  renderer uses a 10-second window and the agent a 30-second echo marker.

## References

- [Clipboard](../clipboard.md)
- [Live migration](../live-migration.md)
- [`apps/ahv-remote-console-agent/internal/vdagent/protocol.go`](../../../apps/ahv-remote-console-agent/internal/vdagent/protocol.go)
- [`apps/runtime/internal/rfb/extclip.go`](../../../apps/runtime/internal/rfb/extclip.go)
- [`scripts/ahv-libvirt-hook-qemu.py`](../../../scripts/ahv-libvirt-hook-qemu.py)
