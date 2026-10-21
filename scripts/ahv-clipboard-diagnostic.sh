#!/bin/bash
# AHV Clipboard Diagnostic Script
#
# Run this from the CVM (Controller VM):
#   ssh root@<cvm-or-host> 'bash -s' < ahv-clipboard-diagnostic.sh
#
# Or SSH to the AHV host directly and run it there.
#
# This script checks everything needed to determine if transparent
# clipboard sharing is possible on this AHV cluster.

set -euo pipefail

VM_UUID="${1:-}"

echo "============================================"
echo "AHV Clipboard Diagnostic"
echo "============================================"
echo ""

# 1. QEMU version
echo "--- 1. QEMU VERSION ---"
QEMU_BIN=$(which qemu-kvm 2>/dev/null || which qemu-system-x86_64 2>/dev/null || echo "/usr/libexec/qemu-kvm")
if [ -x "$QEMU_BIN" ]; then
    $QEMU_BIN --version 2>&1 | head -3
else
    echo "qemu-kvm not found at standard paths"
    find / -name "qemu-kvm" -o -name "qemu-system-x86_64" 2>/dev/null | head -5
fi
echo ""

# 2. Check if qemu-vdagent chardev is compiled in
echo "--- 2. QEMU VDAGENT CHARDEV SUPPORT ---"
if [ -x "$QEMU_BIN" ]; then
    if $QEMU_BIN -chardev help 2>&1 | grep -q "qemu-vdagent"; then
        echo "YES — qemu-vdagent chardev is available"
    else
        echo "NO — qemu-vdagent chardev NOT available"
        echo "Available chardev types:"
        $QEMU_BIN -chardev help 2>&1 | head -20
    fi
fi
echo ""

# 3. Check if vmport is enabled (it should be by default)
echo "--- 3. VMPORT STATUS ---"
if [ -x "$QEMU_BIN" ]; then
    $QEMU_BIN -machine help 2>&1 | grep -i "vmport" || echo "vmport not in machine help (probably auto/enabled)"
fi
echo ""

# 4. If a VM UUID was provided, check its specific config
if [ -n "$VM_UUID" ]; then
    echo "--- 4. VM-SPECIFIC CONFIG (${VM_UUID}) ---"

    # Find the QEMU process
    QEMU_PID=$(ps aux | grep "qemu" | grep "$VM_UUID" | grep -v grep | awk '{print $2}' | head -1)
    if [ -n "$QEMU_PID" ]; then
        echo "QEMU PID: $QEMU_PID"
        echo ""

        # Full command line
        echo "--- 4a. FULL QEMU COMMAND LINE ---"
        cat /proc/$QEMU_PID/cmdline | tr '\0' '\n' | head -100
        echo ""

        # Check for serial ports
        echo "--- 4b. SERIAL PORT CONFIG ---"
        cat /proc/$QEMU_PID/cmdline | tr '\0' '\n' | grep -i "serial\|chardev\|virtio-serial\|vdagent\|spice" || echo "No serial/chardev/vdagent args found"
        echo ""

        # Check for virtio-serial bus
        echo "--- 4c. VIRTIO-SERIAL DEVICES ---"
        cat /proc/$QEMU_PID/cmdline | tr '\0' '\n' | grep -i "virtio-serial\|virtserialport\|virtioserial" || echo "No virtio-serial devices found"
        echo ""

        # Check QMP socket
        echo "--- 4d. QMP MONITOR SOCKET ---"
        cat /proc/$QEMU_PID/cmdline | tr '\0' '\n' | grep -i "monitor\|qmp\|chardev.*monitor" || echo "No QMP socket found in cmdline"
        ls -la /var/lib/libvirt/qemu/domain-*${VM_UUID}*/monitor.sock 2>/dev/null || echo "No monitor socket found in /var/lib/libvirt/qemu/"
        echo ""
    else
        echo "No running QEMU process found for UUID $VM_UUID"
        echo "Running QEMU processes:"
        ps aux | grep qemu-kvm | grep -v grep | head -5
    fi

    # Check libvirt domain XML
    echo "--- 4e. LIBVIRT DOMAIN XML (serial/channel devices) ---"
    virsh dumpxml "$VM_UUID" 2>/dev/null | grep -A5 "serial\|channel\|console\|virtio-serial\|com.redhat" || \
    virsh list --all 2>/dev/null | head -10 && echo "(provide domain name or UUID)"
    echo ""

    # Try QMP query
    echo "--- 4f. QMP DEVICE QUERY ---"
    MONITOR_SOCK=$(ls /var/lib/libvirt/qemu/domain-*${VM_UUID}*/monitor.sock 2>/dev/null | head -1)
    if [ -n "$MONITOR_SOCK" ]; then
        echo "Attempting QMP query on $MONITOR_SOCK..."
        # Query chardevs
        virsh qemu-monitor-command "$VM_UUID" --pretty '{"execute":"query-chardev"}' 2>/dev/null | head -50 || echo "QMP query failed"
    fi
    echo ""
else
    echo "--- 4. SKIP VM-SPECIFIC (no UUID provided) ---"
    echo "Usage: $0 <vm-uuid>"
    echo ""
    echo "Running VMs:"
    virsh list 2>/dev/null || ps aux | grep qemu-kvm | grep -v grep | head -5
fi

echo ""

# 5. Check for spice-vdagent or NGT on the host
echo "--- 5. NGT / VDAGENT ON HOST ---"
rpm -qa 2>/dev/null | grep -i "spice\|vdagent\|ngt\|guest-tool" || echo "No spice/vdagent/NGT packages found"
echo ""

# 6. Check available virtio-serial infrastructure
echo "--- 6. SYSTEM VIRTIO-SERIAL CHECK ---"
lsmod 2>/dev/null | grep -i "virtio" || echo "No virtio modules loaded (expected on hypervisor host)"
echo ""

# 7. Network connectivity check (can the operator reach AHV host?)
echo "--- 7. HOST NETWORK ---"
ip addr show 2>/dev/null | grep "inet " | grep -v "127.0.0.1" | head -5
echo ""

echo "============================================"
echo "DIAGNOSTIC COMPLETE"
echo ""
echo "KEY QUESTIONS ANSWERED:"
echo "  1. Is QEMU >= 6.1? (check version above)"
echo "  2. Does QEMU have qemu-vdagent chardev? (check section 2)"
echo "  3. Does the VM have virtio-serial? (check section 4c)"
echo "  4. Does the VM have serial ports? (check section 4b)"
echo "  5. Is QMP accessible? (check section 4f)"
echo "============================================"
