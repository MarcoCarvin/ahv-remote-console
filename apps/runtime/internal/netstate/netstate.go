// Package netstate observes OS-level network state. Every 2 seconds it
// snapshots net.Interfaces() and exposes the latest snapshot via Snapshot().
//
// The implementation is intentionally polling-based for portability — it avoids
// the platform-specific complexity of netlink/route sockets while still
// reflecting interface/address changes within a few seconds. The snapshot is
// surfaced in the runtime-state and diagnostics endpoints.
//
// (The event-subscription machinery — Subscribe/diffAndEmit/Event — was removed:
// no plugin consumed NetState events, the only subscriber send-path raced a
// closed channel, and the polling goroutine never stopped. See GO-01/GO-R-05.)
package netstate

import (
	"context"
	"log/slog"
	"net"
	"sort"
	"strings"
	"sync"
	"time"
)

type Observer struct {
	log     *slog.Logger
	mu      sync.RWMutex
	current Snapshot
}

type Snapshot struct {
	CapturedAt time.Time   `json:"captured_at"`
	Interfaces []Interface `json:"interfaces"`
}

type Interface struct {
	Name      string   `json:"name"`
	MAC       string   `json:"mac,omitempty"`
	Up        bool     `json:"up"`
	Loopback  bool     `json:"loopback"`
	Addresses []string `json:"addresses"`
}

func New(log *slog.Logger) *Observer {
	return &Observer{
		log: log.With("component", "netstate"),
	}
}

func (o *Observer) Run(ctx context.Context) {
	t := time.NewTicker(2 * time.Second)
	defer t.Stop()
	if snap, err := capture(); err == nil {
		o.mu.Lock()
		o.current = snap
		o.mu.Unlock()
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			next, err := capture()
			if err != nil {
				o.log.Warn("capture failed", "err", err)
				continue
			}
			o.mu.Lock()
			o.current = next
			o.mu.Unlock()
		}
	}
}

func (o *Observer) Snapshot() Snapshot {
	o.mu.RLock()
	defer o.mu.RUnlock()
	return o.current
}

func capture() (Snapshot, error) {
	ifs, err := net.Interfaces()
	if err != nil {
		return Snapshot{}, err
	}
	snap := Snapshot{CapturedAt: time.Now()}
	for _, i := range ifs {
		addrs, _ := i.Addrs()
		addrStrs := make([]string, 0, len(addrs))
		for _, a := range addrs {
			addrStrs = append(addrStrs, a.String())
		}
		sort.Strings(addrStrs)
		snap.Interfaces = append(snap.Interfaces, Interface{
			Name:      i.Name,
			MAC:       i.HardwareAddr.String(),
			Up:        (i.Flags & net.FlagUp) != 0,
			Loopback:  (i.Flags & net.FlagLoopback) != 0,
			Addresses: addrStrs,
		})
	}
	sort.Slice(snap.Interfaces, func(a, b int) bool {
		return strings.Compare(snap.Interfaces[a].Name, snap.Interfaces[b].Name) < 0
	})
	return snap, nil
}
