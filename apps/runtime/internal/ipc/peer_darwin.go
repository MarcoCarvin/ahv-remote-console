//go:build darwin

package ipc

import (
	"net"
	"syscall"
)

// readPeerPID on macOS: LOCAL_PEERPID works for AF_UNIX sockets. For
// AF_INET TCP loopback there is no reliable kernel API to fetch the
// peer's PID — the system holds the four-tuple but not the owning
// process. We return (0, true) which the caller interprets as
// "unknown — do not block."
//
// The full UDS migration (v2 work) closes
// this gap by switching the listener to AF_UNIX, where LOCAL_PEERPID
// is reliable.
func readPeerPID(_ syscall.RawConn, _ net.Conn) (int, bool) {
	return 0, true
}
