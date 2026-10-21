//go:build windows

package ipc

import "net"

// peerPID on Windows is currently a no-op for TCP loopback. The
// matching primitive (GetExtendedTcpTable scanned by remote four-tuple
// to find owning PID) is heavy and racy. The full UDS-equivalent
// migration (named pipe with GetNamedPipeClientProcessId) is tracked
// as v2 work.
func peerPID(_ net.Conn) (int, bool) {
	return 0, true
}
