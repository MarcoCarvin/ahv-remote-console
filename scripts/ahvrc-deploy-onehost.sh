#!/bin/bash
# Per-host deploy of the console-following vdagent hook (HOOK-ONLY: no systemd;
# the hook's own ensure_manager spawns/respawns the manager on libvirt events).
# Validates the staged hook before touching production, backs up the original
# once, removes any previously-installed systemd unit, installs the hook, arms
# it, kicks the manager once, and verifies.
NEW=/tmp/ahvrc-newhook.py
EXPECT_MD5="$1"
python3 -c "import ast; ast.parse(open('$NEW').read())" || { echo "ABORT $(hostname): staged hook syntax invalid"; exit 1; }
python3 "$NEW" --manager-selftest >/dev/null 2>&1 || { echo "ABORT $(hostname): selftest failed"; exit 1; }
GOTMD5=$(md5sum "$NEW" | cut -d' ' -f1)
[ "$GOTMD5" = "$EXPECT_MD5" ] || { echo "ABORT $(hostname): md5 $GOTMD5 != expected $EXPECT_MD5"; exit 1; }
[ -f /etc/libvirt/hooks/qemu.ahvrc-orig ] || cp /etc/libvirt/hooks/qemu /etc/libvirt/hooks/qemu.ahvrc-orig
# Remove the prior systemd supervisor (now hook-only).
systemctl disable --now ahvrc-vdagent-manager >/dev/null 2>&1
rm -f /etc/systemd/system/ahvrc-vdagent-manager.service
systemctl daemon-reload >/dev/null 2>&1
# Install hook + arm.
install -m 0755 "$NEW" /etc/libvirt/hooks/qemu
touch /etc/libvirt/hooks/ahvrc-mgr-armed
# Restart the manager on the fresh code: kill the old one + stale lock, then
# kick the first spawn (ensure_manager keeps it alive on future hook events).
pkill -9 -f "hooks/qemu --manager" 2>/dev/null
pkill -9 -f "ahvrc-mgr.py --manager" 2>/dev/null
sleep 1; rm -f /var/run/ahvrc-mgr.lock
setsid python3 /etc/libvirt/hooks/qemu --manager </dev/null >/dev/null 2>&1 &
sleep 6
rm -f /tmp/ahvrc-mgr.py /tmp/mon90.sh /tmp/qstat.sh /tmp/snap.sh /tmp/story.sh /tmp/status90.sh /tmp/setup_armed.sh /tmp/runmgr.sh /tmp/start90.sh /tmp/evttest.sh /tmp/chk93.sh /tmp/ahvrc-newhook.py /tmp/ahvrc-deploy.sh /tmp/ahvrc-vdagent-manager.service 2>/dev/null
echo "=== $(hostname) ==="
echo "hookmd5=$(md5sum /etc/libvirt/hooks/qemu | cut -d' ' -f1) backup=$([ -f /etc/libvirt/hooks/qemu.ahvrc-orig ] && echo Y) armed=$([ -f /etc/libvirt/hooks/ahvrc-mgr-armed ] && echo Y) systemd=$(systemctl is-active ahvrc-vdagent-manager 2>/dev/null) mgr_pid=$(pgrep -f 'hooks/qemu --manager' | head -1)"
python3 /etc/libvirt/hooks/qemu --manager-report 2>/dev/null | head -8
