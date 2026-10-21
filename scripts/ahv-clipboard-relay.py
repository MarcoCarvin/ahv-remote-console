#!/usr/bin/env python3
"""
Clipboard relay: bridges VNC clipboard ↔ serial port.

Connects to QEMU's VNC server as a second client AND to the
serial port's Unix socket. When the operator copies text (the
first VNC client sends ClientCutText), QEMU sends ServerCutText
to us. We forward it to the bridge via the serial socket. When
the bridge sends clipboard data from the guest, we forward it
as ClientCutText to VNC, which QEMU delivers as ServerCutText
to the operator's client.

No chardev-change, no vdagent, no clipboard=true flag.
Just a VNC client + serial socket relay.

Protocol on the serial socket (bridge ↔ relay):
  [4 bytes LE uint32: length] [N bytes: UTF-8 text]

Started by the libvirt hook as a background process.
"""

import glob
import json
import os
import select
import socket
import struct
import sys
import time
import threading


def find_vnc_port(domain):
    """Find the VNC port from the QEMU command line."""
    import subprocess
    try:
        result = subprocess.run(
            ["virsh", "qemu-monitor-command", domain, "--hmp", "info vnc"],
            capture_output=True, text=True, timeout=5
        )
        for line in result.stdout.splitlines():
            # Look for "127.0.0.1:59xx" or similar
            if "127.0.0.1:" in line:
                parts = line.split("127.0.0.1:")
                if len(parts) > 1:
                    port_str = parts[1].split()[0].strip()
                    return int(port_str)
    except Exception:
        pass
    return None


def find_serial_socket(domain):
    """Find the serial socket for our COM port (index=2, acli)."""
    patterns = [
        f"/var/lib/libvirt/qemu/{domain}.serial.2",
        f"/var/lib/libvirt/qemu/{domain}.serial.0",
    ]
    for pat in patterns:
        if os.path.exists(pat):
            return pat
    # Try glob
    for suffix in [".serial.2", ".serial.0"]:
        matches = glob.glob(f"/var/lib/libvirt/qemu/{domain}{suffix}")
        if matches:
            return matches[0]
    return None


class VNCClipboardClient:
    """Minimal VNC client that only handles clipboard messages."""

    def __init__(self, host, port):
        self.sock = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
        self.sock.connect((host, port))
        self.sock.settimeout(1.0)
        self._handshake()

    def _recv_exact(self, n):
        buf = b""
        while len(buf) < n:
            chunk = self.sock.recv(n - len(buf))
            if not chunk:
                raise ConnectionError("VNC connection closed")
            buf += chunk
        return buf

    def _handshake(self):
        # Server sends version
        version = self._recv_exact(12)
        # Send same version back
        self.sock.sendall(b"RFB 003.008\n")
        # Security types
        num_types = self._recv_exact(1)[0]
        types = self._recv_exact(num_types)
        # Choose type 1 (None)
        self.sock.sendall(bytes([1]))
        # Security result
        result = self._recv_exact(4)
        if struct.unpack(">I", result)[0] != 0:
            raise ConnectionError("VNC auth failed")
        # ClientInit: shared=1
        self.sock.sendall(bytes([1]))
        # ServerInit: read and discard
        header = self._recv_exact(24)
        name_len = struct.unpack(">I", header[20:24])[0]
        if name_len > 0:
            self._recv_exact(name_len)

    def send_client_cut_text(self, text):
        """Send ClientCutText (message type 6)."""
        encoded = text.encode("utf-8") if isinstance(text, str) else text
        msg = struct.pack(">BxxxI", 6, len(encoded)) + encoded
        self.sock.sendall(msg)

    def read_server_message(self):
        """Read one server message. Returns (type, data) or None on timeout."""
        try:
            msg_type_bytes = self.sock.recv(1)
            if not msg_type_bytes:
                return None
            msg_type = msg_type_bytes[0]

            if msg_type == 0:  # FramebufferUpdate
                self._recv_exact(1)  # padding
                n_rects = struct.unpack(">H", self._recv_exact(2))[0]
                for _ in range(n_rects):
                    self._skip_rect()
                return ("fbu", None)
            elif msg_type == 1:  # SetColourMapEntries
                self._recv_exact(1)  # padding
                first = struct.unpack(">H", self._recv_exact(2))[0]
                n_colours = struct.unpack(">H", self._recv_exact(2))[0]
                self._recv_exact(n_colours * 6)
                return ("colourmap", None)
            elif msg_type == 2:  # Bell
                return ("bell", None)
            elif msg_type == 3:  # ServerCutText
                self._recv_exact(3)  # padding
                length = struct.unpack(">I", self._recv_exact(4))[0]
                if length > 10 * 1024 * 1024:
                    return None
                text = self._recv_exact(length)
                return ("clipboard", text)
            else:
                return ("unknown", msg_type)
        except socket.timeout:
            return None
        except Exception:
            return None

    def _skip_rect(self):
        """Skip one FramebufferUpdate rectangle."""
        header = self._recv_exact(12)
        x, y, w, h = struct.unpack(">HH", header[0:4])[0], 0, struct.unpack(">H", header[4:6])[0], struct.unpack(">H", header[6:8])[0]
        encoding = struct.unpack(">i", header[8:12])[0]
        w = struct.unpack(">H", header[4:6])[0]
        h = struct.unpack(">H", header[6:8])[0]
        if encoding == 0:  # Raw
            # Assume 4 bpp
            self._recv_exact(w * h * 4)
        elif encoding == 1:  # CopyRect
            self._recv_exact(4)
        # Other encodings: just skip what we can
        # For a clipboard-only client, we shouldn't receive many FBUs

    def request_no_updates(self):
        """Send FramebufferUpdateRequest with incremental=1 for 0x0 area."""
        msg = struct.pack(">BbHHHH", 3, 1, 0, 0, 1, 1)
        self.sock.sendall(msg)

    def fileno(self):
        return self.sock.fileno()

    def close(self):
        self.sock.close()


def relay_loop(domain, log_file="/tmp/clipboard-relay.log"):
    """Main relay loop."""
    def log(msg):
        ts = time.strftime("%Y-%m-%d %H:%M:%S")
        line = f"{ts} {msg}\n"
        with open(log_file, "a") as f:
            f.write(line)

    log(f"Starting clipboard relay for {domain}")

    # Find VNC port
    vnc_port = find_vnc_port(domain)
    if not vnc_port:
        log("ERROR: Could not find VNC port")
        return
    log(f"VNC port: {vnc_port}")

    # Find serial socket
    serial_path = find_serial_socket(domain)
    if not serial_path:
        log("ERROR: Could not find serial socket")
        return
    log(f"Serial socket: {serial_path}")

    # Connect to VNC
    try:
        vnc = VNCClipboardClient("127.0.0.1", vnc_port)
        log("VNC connected and handshake complete")
    except Exception as e:
        log(f"ERROR: VNC connection failed: {e}")
        return

    # Connect to serial socket
    try:
        serial = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        serial.settimeout(1.0)
        serial.connect(serial_path)
        log("Serial socket connected")
    except Exception as e:
        log(f"ERROR: Serial socket connection failed: {e}")
        vnc.close()
        return

    # Request minimal VNC updates (we only care about clipboard)
    vnc.request_no_updates()

    serial_buf = b""
    last_host_clip = ""
    last_guest_clip = ""

    log("Relay loop starting")

    while True:
        try:
            # Read from VNC (ServerCutText = host clipboard)
            msg = vnc.read_server_message()
            if msg and msg[0] == "clipboard":
                text = msg[1].decode("utf-8", errors="replace")
                if text != last_host_clip and text != last_guest_clip:
                    last_host_clip = text
                    # Forward to bridge via serial socket
                    encoded = text.encode("utf-8")
                    frame = struct.pack("<I", len(encoded)) + encoded
                    try:
                        serial.sendall(frame)
                        log(f"Host→Guest: {len(encoded)} bytes")
                    except Exception as e:
                        log(f"Serial write error: {e}")

            # Read from serial socket (bridge clipboard)
            try:
                chunk = serial.recv(4096)
                if chunk:
                    serial_buf += chunk
                    # Parse framed messages: [4-byte LE length][text]
                    while len(serial_buf) >= 4:
                        msg_len = struct.unpack("<I", serial_buf[:4])[0]
                        if msg_len > 1024 * 1024:
                            serial_buf = b""
                            break
                        if len(serial_buf) < 4 + msg_len:
                            break
                        text = serial_buf[4:4+msg_len].decode("utf-8", errors="replace")
                        serial_buf = serial_buf[4+msg_len:]
                        if text != last_guest_clip and text != last_host_clip:
                            last_guest_clip = text
                            vnc.send_client_cut_text(text)
                            log(f"Guest→Host: {len(text)} bytes")
            except socket.timeout:
                pass

        except KeyboardInterrupt:
            break
        except Exception as e:
            log(f"Relay error: {e}")
            time.sleep(1)

    vnc.close()
    serial.close()
    log("Relay stopped")


if __name__ == "__main__":
    domain = sys.argv[1] if len(sys.argv) > 1 else ""
    if not domain:
        print("Usage: ahv-clipboard-relay.py <domain-uuid>")
        sys.exit(1)
    relay_loop(domain)
