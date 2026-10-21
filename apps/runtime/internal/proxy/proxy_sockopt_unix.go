//go:build !windows

package proxy

import (
	"crypto/tls"
	"log/slog"
	"net"
	"syscall"
)

// setDSCPEF marks the socket with DSCP Expedited Forwarding (46).
// TOS byte = DSCP << 2 = 0xB8. This tells switches/routers to treat
// the connection as low-latency, low-jitter traffic — same class
// used by VoIP. Best-effort: errors are silently ignored.
func setDSCPEF(conn net.Conn, log *slog.Logger) {
	setTOS := func(fd int) {
		_ = syscall.SetsockoptInt(fd, syscall.IPPROTO_IP, syscall.IP_TOS, 0xB8)
	}
	switch c := conn.(type) {
	case *net.TCPConn:
		raw, err := c.SyscallConn()
		if err != nil {
			return
		}
		_ = raw.Control(func(fd uintptr) { setTOS(int(fd)) })
		log.Debug("proxy: DSCP EF set on TCP conn")
	case *tls.Conn:
		if tc, ok := c.NetConn().(*net.TCPConn); ok {
			raw, err := tc.SyscallConn()
			if err != nil {
				return
			}
			_ = raw.Control(func(fd uintptr) { setTOS(int(fd)) })
			log.Debug("proxy: DSCP EF set on TLS conn")
		}
	}
}

// setQuickACK disables delayed ACKs on the connection. This saves up
// to 40ms per ACK on Linux. The kernel resets TCP_QUICKACK after
// each ACK, but setting it once is still beneficial for the initial
// handshake burst. On macOS the constant 12 won't match a valid
// socket option and the call silently fails — harmless.
func setQuickACK(conn net.Conn, log *slog.Logger) {
	const tcpQuickACK = 12 // Linux TCP_QUICKACK; no-op on macOS

	setOpt := func(fd int) {
		// Ignore errors — Linux-only, best-effort.
		_ = syscall.SetsockoptInt(fd, syscall.IPPROTO_TCP, tcpQuickACK, 1)
	}
	switch c := conn.(type) {
	case *net.TCPConn:
		raw, err := c.SyscallConn()
		if err != nil {
			return
		}
		_ = raw.Control(func(fd uintptr) { setOpt(int(fd)) })
		log.Debug("proxy: TCP_QUICKACK set on TCP conn")
	case *tls.Conn:
		if tc, ok := c.NetConn().(*net.TCPConn); ok {
			raw, err := tc.SyscallConn()
			if err != nil {
				return
			}
			_ = raw.Control(func(fd uintptr) { setOpt(int(fd)) })
			log.Debug("proxy: TCP_QUICKACK set on TLS conn")
		}
	}
}
