//go:build linux || darwin

package ipc

import (
	"net"
	"syscall"
)

// peerPID returns the OS process ID of the peer connected to `conn`.
// Returns (0, true) on platforms where peer identity is not reliably
// recoverable for the given socket type, so callers don't treat a
// missing PID as an attacker. Returns (pid, true) on success and
// (0, false) on a hard error.
//
// Linux: SO_PEERCRED on AF_UNIX returns ucred (pid, uid, gid). On
// AF_INET TCP loopback the same call also returns ucred since kernel
// 3.0+; we use this for our 127.0.0.1 listener.
//
// macOS: LOCAL_PEERPID via getsockopt(SOL_LOCAL) works on AF_UNIX. For
// AF_INET TCP loopback we currently return (0, true) — the system does
// not expose peer PID over TCP, and we don't want to false-positive an
// "unknown" peer as malicious. The full UDS migration (v2 work) closes this gap.
func peerPID(conn net.Conn) (pid int, ok bool) {
	type rawConner interface {
		SyscallConn() (syscall.RawConn, error)
	}
	rc, ok := conn.(rawConner)
	if !ok {
		return 0, true
	}
	raw, err := rc.SyscallConn()
	if err != nil {
		return 0, false
	}
	pid, ok = readPeerPID(raw, conn)
	return pid, ok
}
