//go:build linux

package ipc

import (
	"net"
	"syscall"

	"golang.org/x/sys/unix"
)

// readPeerPID extracts ucred from a Linux TCP loopback / UDS socket.
// SO_PEERCRED returns the PID/UID/GID of the process that called
// connect() (for stream sockets; for datagram sockets it returns the
// creator of the listen socket). On AF_INET loopback this is the
// connecting client's PID, which is what we want.
func readPeerPID(raw syscall.RawConn, _ net.Conn) (int, bool) {
	var ucred *unix.Ucred
	var serr error
	if err := raw.Control(func(fd uintptr) {
		ucred, serr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
	}); err != nil {
		return 0, false
	}
	if serr != nil || ucred == nil {
		return 0, false
	}
	return int(ucred.Pid), true
}
