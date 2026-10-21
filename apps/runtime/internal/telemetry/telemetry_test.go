package telemetry

import (
	"bytes"
	"context"
	"strings"
	"testing"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// OBS-01: telemetry.New must honor config.Telemetry — specifically the Enabled
// gate. Previously New keyed only off a non-empty OTLPEndpoint and ignored
// Enabled, so an operator who set an endpoint but flipped Enabled=false still
// got tracing.
func TestNew_EnabledGate(t *testing.T) {
	// Enabled=false with an endpoint set must still be a no-op tracer.
	tr, err := New(Options{
		ServiceName:  "test",
		Enabled:      false,
		OTLPEndpoint: "http://collector.example:4318",
		SampleRate:   1.0,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if tr.Enabled() {
		t.Fatal("Enabled=false must yield a disabled (no-op) tracer even with an endpoint set")
	}

	// Enabled=true but no endpoint also falls back to no-op (validation upstream
	// rejects this combination; the constructor must not panic / try to dial).
	tr2, err := New(Options{ServiceName: "test", Enabled: true, OTLPEndpoint: ""})
	if err != nil {
		t.Fatalf("New (enabled, no endpoint): %v", err)
	}
	if tr2.Enabled() {
		t.Fatal("Enabled=true with empty endpoint must yield a disabled tracer")
	}
}

// OBS-01: the configured SampleRate must drive the head sampler instead of the
// old hardcoded 0.1. We assert via the sampler description, which encodes the
// ratio for TraceIDRatioBased and the AlwaysSample short-circuit at 1.0.
func TestSamplerFor_HonorsSampleRate(t *testing.T) {
	cases := []struct {
		name      string
		rate      float64
		wantDescr string
	}{
		{"zero defaults to 0.1", 0, sdktrace.TraceIDRatioBased(0.1).Description()},
		{"half", 0.5, sdktrace.TraceIDRatioBased(0.5).Description()},
		{"one is always-sample", 1.0, sdktrace.AlwaysSample().Description()},
		{"over-one clamps to always", 2.0, sdktrace.AlwaysSample().Description()},
		{"negative defaults to 0.1", -1, sdktrace.TraceIDRatioBased(0.1).Description()},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := samplerFor(tc.rate).Description()
			if got != tc.wantDescr {
				t.Fatalf("samplerFor(%v).Description() = %q, want %q", tc.rate, got, tc.wantDescr)
			}
		})
	}
}

func TestEffectiveSampleRate(t *testing.T) {
	cases := []struct {
		in, want float64
	}{
		{0, 0.1}, {-0.3, 0.1}, {0.25, 0.25}, {1, 1}, {1.5, 1},
	}
	for _, c := range cases {
		if got := effectiveSampleRate(c.in); got != c.want {
			t.Errorf("effectiveSampleRate(%v) = %v, want %v", c.in, got, c.want)
		}
	}
}

// A no-op tracer's StartSpan must be safe (no panic, valid finish func) so the
// control-plane instrumentation is free when telemetry is disabled.
func TestStartSpan_NoopSafe(t *testing.T) {
	tr := Noop()
	ctx, finish := tr.StartSpan(context.Background(), "x")
	if ctx == nil || finish == nil {
		t.Fatal("StartSpan must return a usable ctx + finish")
	}
	finish(nil) // must not panic
}

// OBS-01: /metrics exposition contains all the required gauges/counters and the
// caller-supplied active-session gauge is rendered.
func TestWriteMetrics_ExposesGauges(t *testing.T) {
	// Drive the package-global counters so the rendered values are non-zero and
	// we can assert they flow through.
	IncReconnectAttempt()
	IncReconnectAttempt()
	IncClipboardFailed()
	SetUpstreamRTTMillis(42)

	var buf bytes.Buffer
	WriteMetrics(&buf, MetricsSnapshot{ActiveSessions: 3})
	out := buf.String()

	for _, want := range []string{
		"ahv_runtime_active_sessions 3",
		"ahv_runtime_reconnect_attempts_total",
		"ahv_runtime_clipboard_failed_total",
		"ahv_runtime_upstream_rtt_ms 42",
		"ahv_runtime_goroutines",
		"ahv_runtime_heap_alloc_bytes",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("metrics output missing %q\n---\n%s", want, out)
		}
	}
	// TYPE/HELP lines must be present for at least the counters.
	if !strings.Contains(out, "# TYPE ahv_runtime_reconnect_attempts_total counter") {
		t.Errorf("missing TYPE line for reconnect counter\n%s", out)
	}
}
