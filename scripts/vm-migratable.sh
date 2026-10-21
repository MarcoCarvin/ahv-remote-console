#!/bin/bash
# Report whether a VM (or every running VM on this host) can be LIVE-migrated.
#
# Ground truth = QEMU's query-migrate "blocked-reasons" (what libvirt checks
# before a migration). Empty => migratable; any entry => blocked, with reason.
# For context it also shows the COM3 serial backend (qemu-vdagent blocks
# migration; a plain socket does not) and the VNC console client count, since
# under the console-following manager a VM is blocked exactly while consoled.
#
# Run as root on the AHV host:   sudo ./vm-migratable.sh [<vm-uuid> | all]
# From a CVM, all hosts at once:  hostssh "bash /tmp/vm-migratable.sh all"
usage() { echo "usage: $0 [<vm-uuid>|all]"; exit 1; }
TARGET="${1:-all}"

check_one() {
  local uuid="$1" name reasons be clients
  name=$(virsh domname "$uuid" 2>/dev/null)
  reasons=$(virsh qemu-monitor-command "$uuid" '{"execute":"query-migrate"}' 2>/dev/null \
    | python3 -c "import sys,json
try: r=json.load(sys.stdin)['return'].get('blocked-reasons',[])
except Exception: r=['<query-migrate failed: VM not here / not running?>']
print(' | '.join(r))" 2>/dev/null)
  be=$(virsh qemu-monitor-command "$uuid" '{"execute":"query-chardev"}' 2>/dev/null \
    | python3 -c "import sys,json
try: cs=[c for c in json.load(sys.stdin)['return'] if 'serial' in c['label']]
except Exception: cs=[]
print(','.join('%s=%s'%(c['label'],'vdagent' if 'vdagent' in c['filename'] else 'socket') for c in cs) or '-')" 2>/dev/null)
  clients=$(virsh qemu-monitor-command "$uuid" '{"execute":"query-vnc"}' 2>/dev/null \
    | python3 -c "import sys,json
try: print(len(json.load(sys.stdin)['return'].get('clients',[])))
except Exception: print('?')" 2>/dev/null)
  if [ -z "$reasons" ]; then
    printf "MIGRATABLE  %s (%s)  [serials: %s, console_clients=%s]\n" "$name" "$uuid" "$be" "$clients"
  else
    printf "BLOCKED     %s (%s)  reason: %s  [serials: %s, console_clients=%s]\n" "$name" "$uuid" "$reasons" "$be" "$clients"
  fi
}

if [ "$TARGET" = "all" ]; then
  for u in $(virsh list --uuid 2>/dev/null | grep .); do check_one "$u"; done
elif [ -n "$TARGET" ]; then
  check_one "$TARGET"
else
  usage
fi
