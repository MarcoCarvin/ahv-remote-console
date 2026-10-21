// Package telemetry wraps OpenTelemetry tracing for the runtime. Tracing is
// optional; when no OTLP endpoint is configured we install a no-op tracer
// provider so callers can use the API unconditionally.
package telemetry

import (
	"context"
	"log/slog"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"
)

type Tracer struct {
	provider trace.TracerProvider
	enabled  bool
	shutdown func(context.Context) error
}

type Options struct {
	ServiceName    string
	ServiceVersion string
	// Enabled gates tracing. When false (the default), New returns a no-op
	// tracer regardless of OTLPEndpoint, so an operator can leave an endpoint
	// configured but turn collection off without editing it out. When true an
	// OTLPEndpoint is required; an empty endpoint with Enabled=true still falls
	// back to the no-op tracer (validation upstream rejects that combination).
	Enabled      bool
	OTLPEndpoint string
	// SampleRate is the head-based trace sampling ratio in [0.0, 1.0]. 0 (the
	// zero value) is treated as the historical default of 0.1 so a config that
	// enables telemetry without setting a rate keeps the prior behavior; a rate
	// of exactly 0 is therefore not expressible (use Enabled=false to disable).
	SampleRate float64
	Logger     *slog.Logger
}

func Noop() *Tracer {
	return &Tracer{
		provider: noop.NewTracerProvider(),
		enabled:  false,
		shutdown: func(context.Context) error { return nil },
	}
}

func New(opts Options) (*Tracer, error) {
	// Honor config.Telemetry: tracing is on only when explicitly Enabled AND an
	// endpoint is configured. Previously New keyed solely off a non-empty
	// endpoint and ignored Enabled, and the sampler was hardcoded to 0.1.
	if !opts.Enabled || opts.OTLPEndpoint == "" {
		return Noop(), nil
	}
	ctx := context.Background()
	exp, err := otlptracehttp.New(ctx, otlptracehttp.WithEndpointURL(opts.OTLPEndpoint))
	if err != nil {
		return nil, err
	}
	res, err := resource.Merge(resource.Default(), resource.NewSchemaless(
		semconv.ServiceName(opts.ServiceName),
		semconv.ServiceVersion(opts.ServiceVersion),
	))
	if err != nil {
		return nil, err
	}
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exp),
		sdktrace.WithResource(res),
		sdktrace.WithSampler(sdktrace.ParentBased(samplerFor(opts.SampleRate))),
	)
	otel.SetTracerProvider(tp)
	if opts.Logger != nil {
		opts.Logger.Info("telemetry enabled", "endpoint", opts.OTLPEndpoint, "sample_rate", effectiveSampleRate(opts.SampleRate))
	}
	return &Tracer{
		provider: tp,
		enabled:  true,
		shutdown: tp.Shutdown,
	}, nil
}

// effectiveSampleRate clamps the configured rate into [0,1] and treats the zero
// value as the historical 0.1 default (see Options.SampleRate).
func effectiveSampleRate(rate float64) float64 {
	if rate <= 0 {
		return 0.1
	}
	if rate > 1 {
		return 1
	}
	return rate
}

// samplerFor builds the head sampler from the effective rate. A rate of 1.0
// uses AlwaysSample so a "trace everything" config doesn't get TraceID-ratio
// rounding surprises.
func samplerFor(rate float64) sdktrace.Sampler {
	r := effectiveSampleRate(rate)
	if r >= 1 {
		return sdktrace.AlwaysSample()
	}
	return sdktrace.TraceIDRatioBased(r)
}

func (t *Tracer) Enabled() bool { return t.enabled }

func (t *Tracer) Provider() trace.TracerProvider {
	if t == nil {
		return noop.NewTracerProvider()
	}
	return t.provider
}

func (t *Tracer) Tracer(name string) trace.Tracer {
	return t.Provider().Tracer(name)
}

// StartSpan opens a span on the named tracer and returns a finish func that
// records an error (if any) and ends the span. It is the control-plane analogue
// of the plugin-facing adapter in plugins/adapters.go: when telemetry is
// disabled the provider is a no-op so the span work is effectively free. Used by
// the ipc router and plugin host to instrument OpenSession/CloseSession and
// request handling.
func (t *Tracer) StartSpan(ctx context.Context, name string, attrs ...attribute.KeyValue) (context.Context, func(error)) {
	ctx, span := t.Tracer("runtime").Start(ctx, name)
	if len(attrs) > 0 {
		span.SetAttributes(attrs...)
	}
	return ctx, func(err error) {
		if err != nil {
			span.SetStatus(codes.Error, err.Error())
			span.SetAttributes(attribute.String("error.message", err.Error()))
		}
		span.End()
	}
}

func (t *Tracer) Shutdown(ctx context.Context) error {
	if t == nil || t.shutdown == nil {
		return nil
	}
	return t.shutdown(ctx)
}
