//go:build windows

package proxy

import (
	"log/slog"
	"net"
)

// setDSCPEF — Windows no-op. DSCP setting on Windows requires the
// QoS Packet Scheduler service and an Administrator-installed
// policy; setting IP_TOS via the socket layer has been a no-op
// since Windows XP SP2. The runtime is fine without it; Windows
// guests typically aren't on QoS-managed links anyway.
func setDSCPEF(_ net.Conn, _ *slog.Logger) {}

// setQuickACK — Windows no-op. The TCP_QUICKACK socket option is
// Linux-specific. Windows uses its own ACK-frequency tuning.
func setQuickACK(_ net.Conn, _ *slog.Logger) {}
