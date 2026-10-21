#!/usr/bin/python3
#
# Copyright 2023 Nutanix, Inc.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.
#

import io
import json
import os
import re
import time
import logging
import subprocess
import sys
import traceback
from typing import Any, Dict, Optional
import xml.etree.cElementTree as etree

# The Nutanix hook framework is present only on AHV hosts. The console-following
# manager (--manager / --manager-selftest) uses none of it (stdlib only), so the
# import is made tolerant: standard hook dispatch on a host imports it exactly
# as before; the manager and its selftest run standalone (CI, dev) without it.
try:
    from ahv.libvirt_hook import migrate, precreate, predefine, prepare, \
        release, startcpus, timeout, undefine
    import ahv.libvirt_hook.log as log
except ImportError:  # manager-only invocation off-host
    migrate = precreate = predefine = prepare = release = None
    startcpus = timeout = undefine = None

    class _StderrLog:
        @staticmethod
        def inf(msg):
            print(msg, file=sys.stderr)
        logger = None
    log = _StderrLog()

# If strip is True, all XML namespaces are stripped from the incoming XML.
# If xml is True, we expect the xml from stdin.
# Empty when the Nutanix framework is absent (watcher-only off-host run); the
# watcher never dispatches through HOOKS.
HOOKS = {} if migrate is None else {
    "migrate":   {"handler": migrate.hook,   "strip": False, "xml": True},
    "predefine": {"handler": predefine.hook, "strip": False, "xml": True,
                  "on_error": predefine.on_error},
    "precreate": {"handler": precreate.hook, "strip": False, "xml": True},
    "prepare":   {"handler": prepare.hook,   "strip": True,  "xml": True},
    "startcpus": {"handler": startcpus.hook, "strip": True,  "xml": True},
    "release":   {"handler": release.hook,   "strip": True,  "xml": True},
    "timeout":   {"handler": timeout.hook,   "strip": False, "xml": False},
    "undefine":  {"handler": undefine.hook,  "strip": True,  "xml": True},
}


def run_hook(
        hook: Dict[str, Any],
        domain: str,
        xml: Optional[etree.Element],
        extra: str,
) -> int:
    try:
        hook["handler"](domain, xml, extra)
    except Exception as e:
        log.err(traceback.format_exc())
        sys.stderr.write(str(e))
        on_error = hook.get("on_error", None)
        if on_error is not None:
            try:
                on_error(domain, xml, extra)
            except Exception as e:
                log.err("Exception during error cleanup")
                log.err(traceback.format_exc())
                sys.stderr.write(str(e))
        return 1
    return 0


# --- AHV Remote Console: vdagent clipboard via chardev-change ---
#
# Swap COM3's chardev backend (the dedicated agent port, isa-serial iobase
# 0x3E8 / index 2) from socket to qemu-vdagent with clipboard=on. This converts
# COM3 into a native QEMU clipboard channel — no virtio-serial needed, no PCI
# hotplug needed. COM1 (iobase 0x3F8) belongs to Nutanix Guest Tools and is
# never touched.
#
# The bridge daemon inside the guest opens COM3 (/dev/ttyS2) and speaks the
# SPICE agent protocol to exchange clipboard data bidirectionally.
#
# This function is NO LONGER called at boot (a vdagent chardev blocks live
# migration for its lifetime). It is retained as the canonical reference for
# the validated chardev-change command ("qemu-vdagent,clipboard=on") and its
# COM3-targeting rationale; the console-following manager below reuses exactly
# that command to attach vdagent only while a console is connected.
def _com3_charserial(xml):
    """Return the QEMU chardev id (charserialN) that backs COM3, or None.

    libvirt assigns charserialN by the Nth <serial> element in DOCUMENT ORDER
    (qemu_alias.c) — independent of the COM index — so charserial0 is whichever
    serial Acropolis emits first, NOT necessarily COM3. We therefore locate COM3
    by its STABLE isa-serial identity: iobase 0x3E8 (equivalently target
    port='2'), which is invariant under reordering, and return
    charserial<document-position>. Both attributes are present in the config XML
    the hook receives; we don't rely on the runtime <alias>.
    """
    if xml is None:
        return None
    for pos, s in enumerate(xml.findall(".//devices/serial")):
        addr = s.find("address")
        tgt = s.find("target")
        iobase = ((addr.get("iobase") if addr is not None else "") or "").lower()
        port = (tgt.get("port") if tgt is not None else "") or ""
        if iobase == "0x3e8" or port == "2":
            return "charserial%d" % pos
    return None


def inject_vdagent_clipboard(domain, xml=None):
    if domain == "-":
        return
    # Prefer UUID over domain name — immune to name-reuse race during
    # the 5-second background sleep window.
    vm_uuid = None
    if xml is not None:
        uuid_elem = xml.find("uuid")
        if uuid_elem is not None and uuid_elem.text:
            vm_uuid = uuid_elem.text.strip()
    try:
        # Spawn a background process that waits 3 seconds (for the hook
        # to return and libvirtd to release the QMP monitor), then issues
        # the chardev-change via virsh. This avoids the deadlock where
        # virsh/QMP hangs because libvirtd is blocked waiting for the hook.
        # HMP chardev-change applies clipboard=on correctly: its parse path
        # (vdagent_chr_parse, QEMU 9.1 ui/vdagent.c) sets has_clipboard=true
        # for us. QMP works too, but only if the caller passes the optional
        # member clipboard:true explicitly in the ChardevQemuVDAgent backend
        # (omit it and the backend comes up with clipboard off). We use HMP
        # so the flag can't be forgotten.
        #
        # Target the chardev that ACTUALLY backs COM3 (iobase 0x3E8), found by
        # parsing the domain XML — not a greedy "try charserial0/1/2, break on
        # first success" probe. chardev-change succeeds on ANY chardev (empty
        # HMP response), so the old probe always swapped charserial0; that was
        # only correct when Acropolis happened to emit COM3 first. When the
        # operator adds COM3 AFTER the existing port, COM3 is charserial1/2 and
        # charserial0 is NGT's reserved COM1 (or the system console) — so
        # vdagent landed on the wrong port (clobbering NGT's channel) and
        # host->guest clipboard was silently dead until a re-setup happened to
        # re-emit COM3 first. Keying on iobase 0x3E8 is order-invariant and
        # never touches COM1/NGT. Empty HMP response = success; non-empty = error.
        com3_id = _com3_charserial(xml)
        if com3_id is None:
            log.inf(
                f"vdagent clipboard: COM3 (iobase 0x3e8 / port 2) not in domain XML "
                f"for {domain}; skipping (VM has no agent serial port)"
            )
            return
        import shlex
        safe_domain = shlex.quote(domain)
        target = shlex.quote(vm_uuid) if vm_uuid else safe_domain
        log_path = "/var/log/libvirt/qemu/vdagent-inject.log"
        safe_log = shlex.quote(log_path)
        safe_id = shlex.quote(com3_id)
        # 10s virsh timeout (zombie guard if QMP hangs) + 0-5s jitter on the 5s
        # base sleep so many VMs booting at once don't hammer libvirtd in
        # lockstep. We change ONLY COM3's chardev — never the system console.
        script = (
            f'sleep $((5 + RANDOM % 6)); '
            f'RESULT=$(timeout 10 virsh qemu-monitor-command {target} --hmp '
            f'  "chardev-change {safe_id} qemu-vdagent,clipboard=on" 2>&1); '
            f'RC=$?; '
            f'echo "$(date) domain={safe_domain} uuid={shlex.quote(vm_uuid or "unknown")} {safe_id} rc=$RC: [$RESULT]" >> {safe_log}; '
            f'if [ $RC -eq 0 ] && [ -z "$RESULT" ]; then '
            f'  echo "$(date) SUCCESS on {safe_id} (COM3)" >> {safe_log}; '
            f'else '
            f'  echo "$(date) FAILED on {safe_id}: rc=$RC [$RESULT]" >> {safe_log}; '
            f'fi'
        )
        subprocess.Popen(
            ["bash", "-c", script],
            stdout=subprocess.DEVNULL,
            stderr=subprocess.DEVNULL,
            start_new_session=True,
        )
        log.inf(f"vdagent clipboard: background injection launched for {domain} (uuid={vm_uuid})")
    except Exception as e:
        log.inf(f"vdagent clipboard: launch failed for {domain}: {e}")
# --- end vdagent clipboard ---


# --- AHV Remote Console: console-following vdagent manager (migration-safe) ---
#
# QEMU 9.1 blocks live migration for the LIFETIME of a qemu-vdagent chardev
# (ui/vdagent.c: migrate_add_blocker in chr_open, removed only in chr_fini; no
# host-side knob on AHV's build; vendor-confirmed,
# native support planned for AHV 12.0). A plain socket-backed serial migrates
# fine — proven live (only the vdagent chardev contributed a blocker).
#
# So vdagent is the TRANSIENT state: COM3 is a plain unix socket while idle,
# and only becomes qemu-vdagent while a console (VNC client) is actually
# attached. An idle VM is therefore migratable on the FIRST TRY with no
# detection and no failure path. The manager follows QEMU's own
# VNC_CONNECTED / VNC_DISCONNECTED events (via virsh qemu-monitor-event) and
# converges each VM's COM3 backend to match its live console refcount, with a
# periodic reconcile loop as the safety net for missed events / manager
# restart. The reconcile pass also performs the one-time deployment transition
# (parks idle vdagent VMs left over from the old always-on inject).
#
# On a (re)attach the manager uses the SAME validated chardev-change command as
# the boot inject above (qemu-vdagent,clipboard=on); the in-guest agent's caps
# heartbeat restores the clipboard channel within its interval. On the
# destination after a migration, the console's reconnect re-attaches via this
# same manager.
#
# Singleton loop, spawned by the hook itself (ensure_manager below) — the
# deployed artifact stays ONE file. DRY-RUN unless armed (AHVRC_MGR_ARMED env
# or the arm flag file), so a fresh deploy LOGS its decisions and swaps nothing
# until an operator arms it.

MGR_LOCK = "/var/run/ahvrc-mgr.lock"
MGR_ARM_FLAG = "/etc/libvirt/hooks/ahvrc-mgr-armed"
MGR_LOG = "/var/log/libvirt/qemu/ahvrc-mgr.log"
MGR_RECONCILE_S = 30   # safety-net reconcile cadence (backstop only)
MGR_DEBOUNCE_S = 3     # ignore transient connect probes before attaching
MGR_GRACE_S = 3        # few seconds: close-console -> migratable ETA ~grace+park
# Deliberately a few seconds: the real workflow is "close the console so I can
# migrate" — the VM must become migratable within seconds of the close, not at
# the next reconcile. The grace still absorbs a quick accidental close+reopen
# / brief network blip. The park is event-driven (see the disconnect handler),
# so park latency is ~MGR_GRACE_S, independent of the reconcile cadence.
# Anti-thrash floor only — MUST stay <= MGR_GRACE_S. The event-driven park fires
# at close+grace, when since_last_swap == session_duration + grace; if min_swap
# exceeded grace, a short session (open, quick copy-paste, close) would have the
# park blocked by hysteresis and fall through to the 30s reconcile — breaking the
# "migratable within seconds of close" guarantee regardless of how briefly the
# console was open. At 2s it still dedups the event/reconcile race (those fire ms
# apart) but never blocks a real debounce/grace-cadenced swap.
MGR_MIN_SWAP_S = 2     # hysteresis floor: no two swaps within this window
MGR_VIRSH_TIMEOUT_S = 10
# `virsh qemu-monitor-event --loop --timestamp` prints lines like:
#   2026-06-12 ...: event VNC_CONNECTED for domain '<name>': {...}
# virsh emits ONLY the single-quoted domain name — there is NO parenthesised
# UUID (an earlier pattern assumed `domain <name> (<uuid>)` and so matched
# nothing, silently disabling the whole event path and leaving the 30s reconcile
# as the only mover). On AHV the domain name IS the VM UUID (Acropolis names
# domains by UUID), so it lines up with the `virsh list --uuid` the reconcile
# keys on, and virsh accepts it for every qemu-monitor-command we issue. The
# selftest asserts this matches a real sample so it can't silently rot again.
MGR_EVENT_RE = re.compile(
    r"event (VNC_CONNECTED|VNC_DISCONNECTED) for domain '([^']+)'")


def _mgr_log(msg):
    import datetime
    try:
        # Bounded by truncation, not logrotate: the host's logrotate config is
        # not ours to extend.
        if os.path.exists(MGR_LOG) and os.path.getsize(MGR_LOG) > 10 << 20:
            os.truncate(MGR_LOG, 0)
        with open(MGR_LOG, "a") as f:
            f.write("%s %s\n" % (datetime.datetime.now().isoformat(), msg))
    except OSError:
        pass


def _virsh(args, timeout_s=MGR_VIRSH_TIMEOUT_S):
    """Run virsh under a hard `timeout` wrapper. Never raises: a hung or
    missing QMP monitor returns (1, <reason>) so the loop survives one bad VM."""
    try:
        p = subprocess.run(
            ["timeout", str(timeout_s), "virsh"] + args,
            stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True,
        )
        return p.returncode, p.stdout
    except Exception as e:
        return 1, str(e)


def _qmp(uuid, command):
    rc, out = _virsh(["qemu-monitor-command", uuid,
                      json.dumps({"execute": command})])
    if rc != 0:
        return None
    try:
        return json.loads(out).get("return")
    except (json.JSONDecodeError, AttributeError):
        return None


def _mgr_com3(uuid):
    """(charserialN, socket_path) from the LIVE domain XML, or (None, None).
    Same iobase-0x3E8/port-2 identity as _com3_charserial above. socket_path is
    the source path libvirt created for COM3, needed to park back to a socket."""
    rc, out = _virsh(["dumpxml", uuid])
    if rc != 0:
        return None, None
    try:
        xml = etree.fromstring(out)
    except etree.ParseError:
        return None, None
    cid = _com3_charserial(xml)
    if cid is None:
        return None, None
    for s in xml.findall(".//devices/serial"):
        addr = s.find("address")
        if addr is not None and (addr.get("iobase") or "").lower() == "0x3e8":
            src = s.find("source")
            return cid, (src.get("path") if src is not None else None)
        tgt = s.find("target")
        if tgt is not None and (tgt.get("port") or "") == "2":
            src = s.find("source")
            return cid, (src.get("path") if src is not None else None)
    return cid, None


def _mgr_backend_is_vdagent(uuid, cid):
    """True/False if COM3's live backend is qemu-vdagent, or None if unknown
    (QMP unreachable / label missing) — callers must never act on None."""
    chardevs = _qmp(uuid, "query-chardev")
    if not chardevs:
        return None
    for cd in chardevs:
        if cd.get("label") == cid:
            return "vdagent" in (cd.get("filename") or "")
    return None


def _mgr_client_count(uuid):
    """Number of attached VNC clients, or None if query-vnc is unreachable."""
    vnc = _qmp(uuid, "query-vnc")
    if vnc is None:
        return None
    return len(vnc.get("clients") or [])


def _mgr_migrating(uuid):
    """True if the VM is mid-migration (a vdagent swap mustn't race a migration,
    and a swap is pointless on a domain that's already moving). Best-effort:
    unknown -> False so we don't freeze the manager on a transient QMP miss."""
    job = _qmp(uuid, "query-migrate")
    if not job:
        return False
    return job.get("status") in ("setup", "active", "pre-switchover",
                                 "device", "postcopy-active")


def _mgr_armed():
    return os.environ.get("AHVRC_MGR_ARMED") == "1" or os.path.exists(MGR_ARM_FLAG)


def manager_decide(clients, backend_is_vdagent, unattached_for_s,
                   since_last_swap_s, maintenance):
    """Pure decision core: 'attach' | 'park' | None.

    - maintenance forces 'park' (migratable now) whenever a vdagent is live,
      regardless of console state — covers live-console VMs during a host drain.
    - a connected client attaches immediately (subject to hysteresis);
    - the last client leaving parks only after the grace period;
    - never two swaps within the hysteresis window;
    - no-op when COM3 is already in the target backend.
    """
    if maintenance:
        return "park" if backend_is_vdagent else None
    if since_last_swap_s < MGR_MIN_SWAP_S:
        return None
    if clients > 0 and not backend_is_vdagent:
        return "attach"
    if clients == 0 and backend_is_vdagent and unattached_for_s >= MGR_GRACE_S:
        return "park"
    return None


def _mgr_swap(uuid, cid, action, socket_path, armed):
    """Apply a converge decision via HMP chardev-change. Empty HMP response =
    success (the validated rule). Returns True on success (or in DRY-RUN)."""
    if action == "attach":
        spec = "qemu-vdagent,clipboard=on"  # the validated inject command
    else:
        if not socket_path:
            _mgr_log("park %s: no COM3 socket path in live XML; skipping" % uuid)
            return False
        # Restore the exact socket backend libvirt created at start so the live
        # state matches the domain XML again — and the chardev blocker is gone.
        spec = "socket,path=%s,server=on,wait=off" % socket_path
    if not armed:
        _mgr_log("DRY-RUN would chardev-change %s %s -> %s" % (uuid, cid, spec))
        return True
    rc, out = _virsh(["qemu-monitor-command", uuid, "--hmp",
                      "chardev-change %s %s" % (cid, spec)])
    ok = rc == 0 and not out.strip()  # empty HMP response = success
    _mgr_log("%s %s %s rc=%d [%s]" % (
        "SWAP" if ok else "SWAP-FAILED", uuid, action, rc, out.strip()[:200]))
    return ok


def attach(uuid, state, armed, lock):
    """Converge COM3 to qemu-vdagent for a console'd VM. Guarded against a
    VM that is migrating or already migration-blocked.

    The backend re-check + hysteresis + swap + last_swap write run under `lock`
    so the event thread and the reconcile thread can never both pass the guard
    and issue two concurrent chardev-change on the same COM3."""
    if _mgr_migrating(uuid):
        _mgr_log("attach %s: VM migrating; deferring" % uuid)
        return
    cid, sock = _mgr_com3(uuid)
    if not cid:
        return  # no COM3 — not ours
    state.setdefault(uuid, {})["sock"] = sock
    with lock:
        backend_vd = _mgr_backend_is_vdagent(uuid, cid)
        if backend_vd is None or backend_vd:
            return  # unknown -> never act; already vdagent -> nothing to do
        now = time.time()
        if now - state[uuid].get("last_swap", 0) < MGR_MIN_SWAP_S:
            return
        if _mgr_swap(uuid, cid, "attach", sock, armed):
            state[uuid]["last_swap"] = now


def park(uuid, state, armed, lock):
    """Converge COM3 back to a plain socket so the VM is live-migratable. Same
    cross-thread swap serialization as attach (see its docstring)."""
    cid, sock = _mgr_com3(uuid)
    if not cid:
        return
    state.setdefault(uuid, {})["sock"] = sock
    with lock:
        backend_vd = _mgr_backend_is_vdagent(uuid, cid)
        if backend_vd is None or not backend_vd:
            return  # unknown -> never act; already socket -> nothing to do
        now = time.time()
        if now - state[uuid].get("last_swap", 0) < MGR_MIN_SWAP_S:
            return
        if _mgr_swap(uuid, cid, "park", sock, armed):
            state[uuid]["last_swap"] = now


def maintenance_active():
    """Best-effort: is THIS host entering/under maintenance (drain)? When True,
    park ALL local VMs immediately so even live-console VMs become migratable.

    NOTE: the authoritative maintenance signal on AHV is not finalized here —
    the exact source (an Acropolis host-state field, an ADS marker, or a local
    sentinel file) MUST be confirmed during host bring-up. Until then we probe
    a local marker file and fall through to a conservative False so the manager
    never parks a console'd VM on a false positive. `virsh nodeinfo` succeeding
    only proves libvirt is alive, not that the host is draining, so it is NOT
    used as a maintenance signal."""
    try:
        if os.path.exists("/etc/libvirt/hooks/ahvrc-maintenance"):
            return True
    except OSError:
        pass
    return False


def _mgr_reconcile(state, armed, lock):
    """Safety-net pass: converge every running VM with a COM3 to its live
    console refcount. Also performs the deployment transition (parks idle
    vdagent VMs, keeps console'd ones) and honours the maintenance override.

    Decides via the pure manager_decide core, then EXECUTES through the locked
    attach/park — the single, race-free swap path shared with the event thread
    (which re-check backend + hysteresis under `lock`)."""
    now = time.time()
    rc, out = _virsh(["list", "--uuid"])
    domains = [l.strip() for l in out.splitlines() if l.strip()] if rc == 0 else []
    for u in list(state):
        if u not in domains:
            state.pop(u, None)
    drain = maintenance_active()
    for uuid in domains:
        cid, sock = _mgr_com3(uuid)
        if not cid:
            continue  # no COM3 — not ours
        st = state.setdefault(uuid, {})
        st["sock"] = sock
        clients = _mgr_client_count(uuid)
        if clients is None:
            continue  # unknown — never act on doubt
        if clients > 0:
            st["last_seen_attached"] = now
        backend_vd = _mgr_backend_is_vdagent(uuid, cid)
        if backend_vd is None:
            continue
        unattached_for = now - st.get("last_seen_attached", now if clients > 0 else 0)
        action = manager_decide(clients, backend_vd, unattached_for,
                                now - st.get("last_swap", 0), drain)
        if action == "attach":
            attach(uuid, state, armed, lock)
        elif action == "park":
            park(uuid, state, armed, lock)


def _mgr_event_loop(state, lock):
    """Stream QEMU's own VNC_CONNECTED / VNC_DISCONNECTED events through libvirt
    (no second monitor) and converge COM3 to the LIVE console state. Runs in its
    own thread; the main thread drives the periodic reconcile backstop.

    Two design rules that the "close console -> migratable in a few seconds,
    every time" guarantee depends on:

    1. Gate on the LIVE client count (query-vnc), never an events-derived
       refcount. A refcount desyncs — the Prism proxy can open more than one TCP
       client per console, connect/disconnect counts arrive asymmetrically, and
       events are missed whenever the reader is busy — and a stale-high refcount
       silently strands a VM vdagent-attached (i.e. unmigratable) until the 30s
       reconcile catches it. query-vnc is always the truth.
    2. Never block the reader. Each event schedules its debounce/grace wait in a
       short-lived thread, so a slow virsh call or another (often non-COM3) VM's
       event can't stall COM3 handling — the previous inline time.sleep() let one
       VM's grace delay every other VM's park. Swaps still go through the locked
       attach()/park(), which re-check backend + hysteresis under `lock`, so the
       handler threads and the reconcile thread can never issue racing swaps.

    We re-spawn the monitor on EOF so a libvirtd blip doesn't permanently blind
    the manager."""
    import threading

    def _debounce_attach(uuid):
        # Ignore transient connect probes: attach only if a live client is still
        # present after the debounce.
        time.sleep(MGR_DEBOUNCE_S)
        if maintenance_active():
            return  # drain in progress — never attach
        # query-vnc returns None on a QMP miss. `None > 0` raises TypeError in
        # Python 3, which would kill THIS debounce thread (no handler) and leave
        # the VM parked on the socket — clipboard dead in both directions until
        # the 30s reconcile. That miss is far likelier when two consoles open at
        # once (two VNC_CONNECTED events race query-vnc), so guard it: a miss
        # means "don't act on doubt" — the reconcile backstop retries. Mirrors
        # the `clients is None` guards in _mgr_reconcile / manager_report.
        clients = _mgr_client_count(uuid)
        if clients is not None and clients > 0:
            attach(uuid, state, _mgr_armed(), lock)

    def _grace_park(uuid):
        # Park only if the VM is genuinely client-free after the grace, so a
        # reconnect within the grace cancels the park. Independent of how long
        # the console was open: latency from close to migratable is ~the grace.
        time.sleep(MGR_GRACE_S)
        # None (QMP miss) is NOT == 0, so a miss already declines to park here —
        # "never act on doubt". Spelled out so the asymmetry with the attach
        # guard above is intentional, not an oversight.
        if _mgr_client_count(uuid) == 0:
            park(uuid, state, _mgr_armed(), lock)

    while True:
        try:
            # Stream ALL monitor events (no --event filter): this virsh build
            # rejects a second --event ("option --event already seen"), so we
            # filter to VNC_CONNECTED/VNC_DISCONNECTED in line_re below. The
            # extra events are cheap and ignored.
            proc = subprocess.Popen(
                ["virsh", "qemu-monitor-event", "--loop", "--timestamp"],
                stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True,
            )
        except Exception as e:
            _mgr_log("event stream spawn failed: %s; retrying in 5s" % e)
            time.sleep(5)
            continue
        try:
            for raw in proc.stdout:
                m = MGR_EVENT_RE.search(raw)
                if not m:
                    continue
                event, uuid = m.group(1), m.group(2).lower()
                state.setdefault(uuid, {})["last_seen_attached"] = time.time()
                handler = _debounce_attach if event == "VNC_CONNECTED" else _grace_park
                threading.Thread(target=handler, args=(uuid,), daemon=True).start()
        except Exception as e:
            _mgr_log("event stream read error: %s" % e)
        finally:
            try:
                proc.terminate()
            except Exception:
                pass
        _mgr_log("event stream ended; re-spawning")
        time.sleep(1)


def manager_main():
    import fcntl
    import threading
    lockf = open(MGR_LOCK, "w")
    try:
        fcntl.flock(lockf, fcntl.LOCK_EX | fcntl.LOCK_NB)
    except OSError:
        return 0  # another manager holds the singleton
    armed = _mgr_armed()
    _mgr_log("manager started: reconcile=%ds debounce=%ds grace=%ds "
             "min_swap=%ds armed=%s"
             % (MGR_RECONCILE_S, MGR_DEBOUNCE_S, MGR_GRACE_S,
                MGR_MIN_SWAP_S, armed))
    # Shared per-VM state: uuid -> {last_seen_attached, last_swap, sock}.
    # The event handler threads and the reconcile thread BOTH read-check-then-swap
    # and both write last_swap, so the swap critical section (backend re-check +
    # hysteresis + chardev-change + last_swap) is serialized by this lock —
    # otherwise two concurrent chardev-change could hit the same COM3.
    state = {}
    swap_lock = threading.Lock()
    t = threading.Thread(target=_mgr_event_loop, args=(state, swap_lock),
                         daemon=True)
    t.start()
    # Reconcile immediately (deployment transition) then on the safety cadence.
    while True:
        try:
            _mgr_reconcile(state, _mgr_armed(), swap_lock)
        except Exception as e:
            _mgr_log("reconcile error: %s" % e)  # never crash on one bad pass
        time.sleep(MGR_RECONCILE_S)


def manager_selftest():
    G = MGR_GRACE_S
    # (clients, backend_is_vdagent, unattached_for_s, since_last_swap_s,
    #  maintenance) -> expected
    cases = [
        (1, False, 0, 999, False, "attach"),   # operator arrived: attach
        (1, True, 0, 999, False, None),        # already attached
        (0, True, G - 1, 999, False, None),    # inside grace: hold vdagent
        (0, True, G + 1, 999, False, "park"),  # grace elapsed: park (migratable)
        (0, False, 999, 999, False, None),     # idle and already parked
        (1, False, 0, 1, False, None),         # hysteresis blocks attach
        (0, True, 999, 1, False, None),        # hysteresis blocks park
        (0, True, G + 1, G, False, "park"),    # short session: park still fires
                                               # at grace (since_last_swap==grace
                                               # >= min_swap) — the regression
                                               # guard for the 30s-stall bug
        (1, True, 0, 999, True, "park"),       # maintenance forces park (console'd)
        (0, True, 0, 999, True, "park"),       # maintenance parks idle vdagent
        (1, False, 0, 5, True, None),          # maintenance, already socket: no-op
        (0, False, 999, 999, True, None),      # maintenance, already socket: no-op
    ]
    for i, (c, v, un, sw, mnt, want) in enumerate(cases):
        got = manager_decide(c, v, un, sw, mnt)
        if got != want:
            print("manager selftest case %d FAILED: got %r want %r"
                  % (i, got, want))
            return 1
    # The event-stream regex must match REAL virsh output (this exact format,
    # captured live, silently disabled the event path once). uuid == domain name.
    sample = ("2026-06-12 08:09:42.501+0000: event VNC_DISCONNECTED for domain "
              "'7a9c24c9-5874-4c50-7c9a-b76ede570850': {\"server\":{}}")
    m = MGR_EVENT_RE.search(sample)
    if not m or m.group(1) != "VNC_DISCONNECTED" or \
            m.group(2) != "7a9c24c9-5874-4c50-7c9a-b76ede570850":
        print("manager selftest FAILED: MGR_EVENT_RE no longer matches virsh "
              "qemu-monitor-event output: %r" % (m and m.groups()))
        return 1
    if _mgr_swap_race_selftest() != 0:
        return 1
    print("manager selftest OK (%d cases + swap-race)" % len(cases))
    return 0


def _mgr_swap_race_selftest():
    """Drive attach() from many threads against one VM with the swap lock and
    assert exactly ONE chardev-change happens — the cross-thread hysteresis
    must hold. Stubs the virsh-touching helpers so it runs off-host."""
    import threading
    import builtins
    g = globals()
    saved = {k: g.get(k) for k in
             ("_mgr_migrating", "_mgr_com3", "_mgr_backend_is_vdagent", "_mgr_swap")}
    swaps = {"n": 0}
    swaps_lock = threading.Lock()

    def fake_swap(uuid, cid, action, socket_path, armed):
        # Count swaps; the small sleep widens the race window so an unlocked
        # implementation would reliably double-swap.
        with swaps_lock:
            swaps["n"] += 1
        time.sleep(0.02)
        return True

    backend = {"vd": False}

    def fake_backend(uuid, cid):
        return backend["vd"]

    g["_mgr_migrating"] = lambda uuid: False
    g["_mgr_com3"] = lambda uuid: ("charserial0", "/tmp/x.sock")
    g["_mgr_backend_is_vdagent"] = fake_backend
    g["_mgr_swap"] = fake_swap
    try:
        state = {}
        lock = threading.Lock()
        # First swap flips the backend so re-checks see vdagent (no second swap).
        orig_fake_swap = fake_swap

        def flip_swap(uuid, cid, action, socket_path, armed):
            r = orig_fake_swap(uuid, cid, action, socket_path, armed)
            backend["vd"] = True
            return r
        g["_mgr_swap"] = flip_swap
        threads = [threading.Thread(target=attach,
                                    args=("vm-1", state, True, lock))
                   for _ in range(8)]
        for t in threads:
            t.start()
        for t in threads:
            t.join()
        if swaps["n"] != 1:
            print("manager swap-race selftest FAILED: %d swaps, want 1"
                  % swaps["n"])
            return 1
        return 0
    finally:
        for k, v in saved.items():
            g[k] = v
        _ = builtins  # keep import used if linters fuss


def manager_report():
    """One-shot, READ-ONLY diagnostic: print the live state and the decision
    the manager WOULD make for every running VM that has a COM3 — including
    the no-op cases the daemon log omits. Never swaps regardless of armed
    state. Use to monitor/validate the console-following logic on a host."""
    drain = maintenance_active()
    now = time.time()
    rc, out = _virsh(["list", "--uuid"])
    domains = [l.strip() for l in out.splitlines() if l.strip()] if rc == 0 else []
    print("manager-report: maintenance=%s, %d running domains" % (drain, len(domains)))
    found = 0
    for uuid in domains:
        cid, _sock = _mgr_com3(uuid)
        if not cid:
            continue  # no COM3 — not ours
        found += 1
        clients = _mgr_client_count(uuid)
        backend_vd = _mgr_backend_is_vdagent(uuid, cid)
        migrating = _mgr_migrating(uuid)
        if clients is None or backend_vd is None:
            print("  %s %s: clients=%s backend_vd=%s -> SKIP (unknown)"
                  % (uuid, cid, clients, backend_vd))
            continue
        # Fresh-state report: treat an idle VM as past grace so the steady
        # decision is visible; a live daemon honours the real grace timer.
        unattached_for = 0 if clients > 0 else (MGR_GRACE_S + 1)
        action = manager_decide(clients, backend_vd, unattached_for, 1e9, drain)
        print("  %s %s: clients=%d backend=%s migrating=%s -> %s"
              % (uuid, cid, clients, "vdagent" if backend_vd else "socket",
                 migrating, action or "no-op (already correct)"))
    print("manager-report: %d VM(s) with a COM3" % found)
    _ = now
    return 0


def ensure_manager():
    """Spawn the singleton manager if none is running. Called on every hook
    event — a non-blocking flock probe, microseconds when one is alive."""
    import fcntl
    try:
        probe = open(MGR_LOCK, "w")
        try:
            fcntl.flock(probe, fcntl.LOCK_EX | fcntl.LOCK_NB)
        except OSError:
            return  # manager alive
        fcntl.flock(probe, fcntl.LOCK_UN)
        probe.close()
        subprocess.Popen(
            ["setsid", sys.executable, os.path.abspath(__file__), "--manager"],
            stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL,
            start_new_session=True,
        )
        log.inf("vdagent manager: singleton spawned")
    except Exception as e:
        log.inf(f"vdagent manager: spawn failed: {e}")
# --- end console-following vdagent manager ---


def main(argv):
    # Console-following manager modes: dispatch BEFORE the host logfile handler
    # (the manager has its own log and must run off-host for selftest/CI).
    if len(argv) > 1 and argv[1] == "--manager":
        return manager_main()
    if len(argv) > 1 and argv[1] == "--manager-selftest":
        return manager_selftest()
    if len(argv) > 1 and argv[1] == "--manager-report":
        return manager_report()

    trace_handler = logging.FileHandler("/var/log/libvirt/qemu/hook.log")
    trace_handler.setFormatter(logging.Formatter("%(asctime)s: %(name)s:"
                                                 "%(levelname)s: %(message)s"))
    log.logger.addHandler(trace_handler)

    domain = argv[1]
    action = argv[2]
    subaction = argv[3]
    extra = argv[4]
    xml = None

    ensure_manager()

    hook = HOOKS.get(action)
    if not hook:
        return 0

    if hook["xml"]:
        with io.TextIOWrapper(sys.stdin.buffer, encoding='utf-8') \
              as stdin_utf8:
            xmlstr = stdin_utf8.read()

    log.inf(f"Domain {domain}, action {action}, subaction {subaction}, extra "
            f"{extra}")

    if hook["xml"]:
        xml = etree.fromstring(xmlstr)

        if hook["strip"]:
            for elem in xml.iter():
                elem.tag = elem.tag.rpartition("}")[2]

    rc = run_hook(hook, domain, xml, extra)

    # NOTE: COM3 is intentionally LEFT as a plain socket at boot — a VM with no
    # vdagent chardev live-migrates on the first try. The console-following
    # manager (ensure_manager above) attaches qemu-vdagent only while a console
    # is connected, reusing inject_vdagent_clipboard's validated chardev-change
    # command. The boot inject is therefore no longer called here.

    return rc


if __name__ == "__main__":
    sys.exit(main(sys.argv))
