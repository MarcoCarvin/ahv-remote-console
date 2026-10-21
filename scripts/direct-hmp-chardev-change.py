#!/usr/bin/env python3
"""
Send HMP chardev-change directly to QEMU's QMP socket,
bypassing virsh and libvirtd. This tests whether the HMP
path correctly applies clipboard=on when virsh is not involved.

Usage: python3 direct-hmp-chardev-change.py <qmp-socket-path> <chardev-id>
Example: python3 direct-hmp-chardev-change.py /var/lib/libvirt/qemu/domain-<uuid>/monitor.sock charserial0
"""
import json
import socket
import sys
import glob

def qmp_send(sock, cmd):
    sock.sendall((json.dumps(cmd) + "\n").encode())
    resp = b""
    while b"\n" not in resp:
        resp += sock.recv(4096)
    return json.loads(resp.decode().strip())

def main():
    if len(sys.argv) < 3:
        # Auto-discover socket
        domain = sys.argv[1] if len(sys.argv) > 1 else "*"
        patterns = [
            f"/var/lib/libvirt/qemu/domain-{domain}/monitor.sock",
            f"/var/lib/libvirt/qemu/domain-*{domain}*/monitor.sock",
        ]
        sock_path = None
        for pat in patterns:
            matches = glob.glob(pat)
            if matches:
                sock_path = matches[0]
                break
        if not sock_path:
            print(f"Usage: {sys.argv[0]} <qmp-socket-path> <chardev-id>")
            print(f"   or: {sys.argv[0]} <vm-uuid>")
            sys.exit(1)
        chardev_id = "charserial0"
    else:
        sock_path = sys.argv[1]
        chardev_id = sys.argv[2]

    print(f"Connecting to QMP socket: {sock_path}")
    sock = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
    sock.settimeout(10)
    sock.connect(sock_path)

    # Read greeting
    greeting = b""
    while b"\n" not in greeting:
        greeting += sock.recv(4096)
    print(f"Greeting: {greeting.decode().strip()}")

    # Send qmp_capabilities
    resp = qmp_send(sock, {"execute": "qmp_capabilities"})
    print(f"Capabilities: {resp}")

    # Method 1: HMP via human-monitor-command
    print(f"\n=== Method 1: HMP via human-monitor-command ===")
    hmp_cmd = f"chardev-change {chardev_id} qemu-vdagent,clipboard=on"
    print(f"Sending HMP: {hmp_cmd}")
    resp = qmp_send(sock, {
        "execute": "human-monitor-command",
        "arguments": {"command-line": hmp_cmd}
    })
    print(f"Response: {resp}")

    # Query chardev to verify
    print(f"\n=== Verify chardev ===")
    resp = qmp_send(sock, {"execute": "query-chardev"})
    for cd in resp.get("return", []):
        if chardev_id in cd.get("label", ""):
            print(f"  {cd['label']}: {cd['filename']}")

    # Method 2: Also try QMP chardev-change for comparison
    print(f"\n=== Method 2: QMP chardev-change (for comparison) ===")
    resp = qmp_send(sock, {
        "execute": "chardev-change",
        "arguments": {
            "id": chardev_id,
            "backend": {
                "type": "qemu-vdagent",
                "data": {"clipboard": True}
            }
        }
    })
    print(f"QMP Response: {resp}")

    sock.close()
    print("\nDone. Check bridge logs for clipboard_by_demand in caps exchange.")

if __name__ == "__main__":
    main()
