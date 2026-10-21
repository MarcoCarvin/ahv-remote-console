#!/usr/bin/env bash
# Deploy the console-following vdagent manager to AHV hosts.
#
# For each host: validates the staged hook (syntax + selftest + md5), backs up
# the ORIGINAL /etc/libvirt/hooks/qemu to qemu.ahvrc-orig (once), installs the
# committed hook, arms it (ahvrc-mgr-armed flag), installs+enables the systemd
# unit, clears any test-rig manager so systemd owns the singleton, and prints a
# verification + read-only report. Idempotent and re-runnable.
#
# Usage:  ./scripts/deploy-vdagent-manager.sh [host ...]    (default: .90-.95)
#   key: ~/.ssh/nutanix ; sudo password read from /tmp/test.txt (this env).
#
# ROLLBACK (per host, from the CVM or via ssh):
#   systemctl disable --now ahvrc-vdagent-manager
#   rm -f /etc/libvirt/hooks/ahvrc-mgr-armed
#   cp /etc/libvirt/hooks/qemu.ahvrc-orig /etc/libvirt/hooks/qemu
#   # then re-attach vdagent to any VM left parked, or just let the next boot
#   # re-inject it via the restored original hook.
set -euo pipefail
HOSTS=("$@"); [ ${#HOSTS[@]} -gt 0 ] || { echo "usage: $0 <host> [host...]" >&2; exit 2; }
KEY="${AHVRC_SSH_KEY:-$HOME/.ssh/nutanix}"
DIR="$(cd "$(dirname "$0")" && pwd)"
HOOK="$DIR/ahv-libvirt-hook-qemu.py"
ONEHOST="$DIR/ahvrc-deploy-onehost.sh"
PW="$(cat /tmp/test.txt)"
MD5="$(md5sum "$HOOK" | cut -d' ' -f1)"
python3 "$HOOK" --manager-selftest >/dev/null
for h in "${HOSTS[@]}"; do
  echo "########## $h ##########"
  cat "$HOOK"    | ssh -i "$KEY" "nutanix@$h" "cat > /tmp/ahvrc-newhook.py"
  cat "$ONEHOST" | ssh -i "$KEY" "nutanix@$h" "cat > /tmp/ahvrc-deploy.sh"
  ssh -i "$KEY" "nutanix@$h" "echo \"$PW\" | sudo -S -p '' bash /tmp/ahvrc-deploy.sh $MD5 2>/dev/null"
done
