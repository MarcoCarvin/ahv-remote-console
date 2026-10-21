package ipc

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/MarcoCarvin/ahv-remote-console/apps/runtime/internal/plugins"
	"github.com/MarcoCarvin/ahv-remote-console/apps/runtime/internal/telemetry"
)

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

// testRouter builds a router with a real (empty) plugin host and a no-op tracer
// so the OBS-01 observability endpoints can be exercised over httptest.
func testRouter(t *testing.T) (*router, func()) {
	t.Helper()
	host, err := plugins.NewHost(plugins.Options{Logger: quietLogger()})
	if err != nil {
		t.Fatalf("NewHost: %v", err)
	}
	r := newRouter(Options{
		Logger:    quietLogger(),
		Telemetry: telemetry.Noop(),
		Plugins:   host,
	})
	return r, func() { host.Shutdown(t.Context()) }
}

// OBS-01: /readyz is readiness, not liveness — a fresh host with no plugins
// loaded reports not_ready (503) with plugins_loaded=0.
func TestReadyz_NotReadyWhenNoPlugins(t *testing.T) {
	r, cleanup := testRouter(t)
	defer cleanup()

	mux := http.NewServeMux()
	r.attach(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/readyz")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("readyz with no plugins: got %d, want 503", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), `"plugins_loaded":0`) {
		t.Fatalf("readyz body missing plugins_loaded:0, got %s", body)
	}
	if !strings.Contains(string(body), `"not_ready"`) {
		t.Fatalf("readyz body missing not_ready status, got %s", body)
	}
}

// Liveness (/healthz) must stay green regardless of plugin/readiness state.
func TestHealthz_LiveEvenWhenNotReady(t *testing.T) {
	r, cleanup := testRouter(t)
	defer cleanup()

	mux := http.NewServeMux()
	r.attach(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("healthz should be 200 (liveness) even when not ready, got %d", resp.StatusCode)
	}
}

// OBS-01: /metrics serves the required gauges in Prometheus text exposition.
func TestMetricsEndpoint_ServesGauges(t *testing.T) {
	r, cleanup := testRouter(t)
	defer cleanup()

	mux := http.NewServeMux()
	r.attach(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("metrics: got %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Fatalf("metrics content-type = %q, want text/plain exposition", ct)
	}
	body, _ := io.ReadAll(resp.Body)
	for _, want := range []string{
		"ahv_runtime_active_sessions",
		"ahv_runtime_reconnect_attempts_total",
		"ahv_runtime_clipboard_failed_total",
		"ahv_runtime_upstream_rtt_ms",
		"ahv_runtime_goroutines",
		"ahv_runtime_heap_alloc_bytes",
	} {
		if !strings.Contains(string(body), want) {
			t.Errorf("metrics output missing %q\n%s", want, body)
		}
	}
}

// OBS-01: /readyz and /metrics must be HMAC-exempt (probe/scrape endpoints
// carry no secret). Verify the auth middleware passes them through unsigned.
func TestProbesBypassHMAC(t *testing.T) {
	secret := NewSecret([]byte("0123456789abcdef0123456789abcdef"))
	defer secret.Wipe()

	r, cleanup := testRouter(t)
	defer cleanup()
	mux := http.NewServeMux()
	r.attach(mux)
	handler := hmacAuth(secret, quietLogger(), mux)
	srv := httptest.NewServer(handler)
	defer srv.Close()

	for _, path := range []string{"/readyz", "/metrics"} {
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		// Not 401: the probe bypassed HMAC. (readyz may be 503 not_ready, metrics 200.)
		if resp.StatusCode == http.StatusUnauthorized {
			t.Errorf("%s returned 401 — probe should bypass HMAC", path)
		}
		resp.Body.Close()
	}
}
