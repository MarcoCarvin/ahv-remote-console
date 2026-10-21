package telemetry

// Metrics are runtime-owned process counters/gauges exposed in a tiny
// Prometheus-style text exposition at /metrics (see ipc.router). We deliberately
// use only stdlib sync/atomic (no expvar global namespace, no new deps) so the
// values are addressable from the control-plane code that owns them without
// pulling in a metrics SDK.
//
// Counter wiring split (OBS-01):
//   - active_sessions / upstream_rtt_ms are GAUGES fed from host-visible state
//     by the /metrics handler at scrape time (the host knows the live session
//     count; the proxy publishes its last-measured RTT here).
//   - reconnect_attempts_total / clipboard_failed_total are COUNTERS. The events
//     that drive them live in the proxy hot path (proxy.go), which is owned by
//     the proxy-rfb cluster and must NOT be edited from here. The counters are
//     published so that cluster can increment them from one line each; until
//     then they read 0. See the package-level Inc* helpers below.

import (
	"fmt"
	"io"
	"runtime"
	"sync/atomic"
)

// Process-global counters. Package-level (not per-Tracer) because the proxy
// pumps that drive reconnect/clipboard events don't carry a Tracer handle and
// run independently of whether tracing is enabled. They are monotonic.
var (
	reconnectAttemptsTotal atomic.Uint64
	clipboardFailedTotal   atomic.Uint64
	// upstreamRTTMillis is the last-observed runtime<->upstream round-trip in
	// milliseconds, published by the proxy. A gauge: latest write wins.
	upstreamRTTMillis atomic.Int64
)

// IncReconnectAttempt records one upstream reconnect attempt. Safe for
// concurrent use. Intended to be called from the proxy reconnect loop
// (proxy-rfb cluster wiring).
func IncReconnectAttempt() { reconnectAttemptsTotal.Add(1) }

// IncClipboardFailed records one failed clipboard transfer. Safe for concurrent
// use. Intended to be called from the proxy clipboard path (proxy-rfb cluster
// wiring).
func IncClipboardFailed() { clipboardFailedTotal.Add(1) }

// SetUpstreamRTTMillis publishes the last-measured upstream round-trip latency
// (milliseconds). Intended to be called from the proxy ping/pong handler
// (proxy-rfb cluster wiring).
func SetUpstreamRTTMillis(ms int64) { upstreamRTTMillis.Store(ms) }

// MetricsSnapshot is the point-in-time view the /metrics handler renders. The
// caller supplies the gauges it can see (active session count); the package
// fills in the process-global counters and Go runtime stats.
type MetricsSnapshot struct {
	ActiveSessions int
}

// WriteMetrics renders a minimal Prometheus text-exposition document combining
// the caller-supplied gauges, the runtime-owned counters, and Go runtime stats
// (goroutines + heap). Stdlib only; no client_golang dependency.
func WriteMetrics(w io.Writer, snap MetricsSnapshot) {
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)

	lines := []struct {
		name, typ, help string
		val             any
	}{
		{"ahv_runtime_active_sessions", "gauge", "Open console sessions tracked by the plugin host.", snap.ActiveSessions},
		{"ahv_runtime_reconnect_attempts_total", "counter", "Upstream console reconnect attempts since start.", reconnectAttemptsTotal.Load()},
		{"ahv_runtime_clipboard_failed_total", "counter", "Failed clipboard transfers since start.", clipboardFailedTotal.Load()},
		{"ahv_runtime_upstream_rtt_ms", "gauge", "Last measured runtime-to-upstream round-trip latency in milliseconds.", upstreamRTTMillis.Load()},
		{"ahv_runtime_goroutines", "gauge", "Number of goroutines currently running.", runtime.NumGoroutine()},
		{"ahv_runtime_heap_alloc_bytes", "gauge", "Bytes of allocated heap objects.", ms.HeapAlloc},
	}
	for _, m := range lines {
		fmt.Fprintf(w, "# HELP %s %s\n", m.name, m.help)
		fmt.Fprintf(w, "# TYPE %s %s\n", m.name, m.typ)
		fmt.Fprintf(w, "%s %v\n", m.name, m.val)
	}
}
