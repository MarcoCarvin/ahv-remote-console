package plugins

import (
	"context"
	"log/slog"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"

	"github.com/MarcoCarvin/ahv-remote-console/apps/runtime/internal/config"
	"github.com/MarcoCarvin/ahv-remote-console/apps/runtime/internal/keystore"
	"github.com/MarcoCarvin/ahv-remote-console/apps/runtime/internal/telemetry"
	"github.com/MarcoCarvin/ahv-remote-console/apps/runtime/pkg/sdk"
)

// ---- Logger adapter ----

type slogAdapter struct{ l *slog.Logger }

func newSlogAdapter(l *slog.Logger) sdk.Logger { return &slogAdapter{l: l} }

func (s *slogAdapter) Debug(msg string, args ...any) { s.l.Debug(msg, args...) }
func (s *slogAdapter) Info(msg string, args ...any)  { s.l.Info(msg, args...) }
func (s *slogAdapter) Warn(msg string, args ...any)  { s.l.Warn(msg, args...) }
func (s *slogAdapter) Error(msg string, args ...any) { s.l.Error(msg, args...) }
func (s *slogAdapter) With(args ...any) sdk.Logger   { return &slogAdapter{l: s.l.With(args...)} }

// ---- Keystore adapter (per-plugin key prefix) ----

type ksAdapter struct {
	inner  keystore.Store
	prefix string
}

func newKeystoreAdapter(s keystore.Store, pluginID string) sdk.Keystore {
	return &ksAdapter{inner: s, prefix: "plugin/" + pluginID + "/"}
}

func (k *ksAdapter) Get(key string) ([]byte, error) {
	return k.inner.Get(k.prefix + key)
}
func (k *ksAdapter) Set(key string, value []byte) error {
	return k.inner.Set(k.prefix+key, value)
}
func (k *ksAdapter) Delete(key string) error {
	return k.inner.Delete(k.prefix + key)
}

// ---- Telemetry adapter ----

type telAdapter struct {
	t    *telemetry.Tracer
	name string
}

func newTelemetryAdapter(t *telemetry.Tracer, pluginID string) sdk.Telemetry {
	return &telAdapter{t: t, name: "plugin/" + pluginID}
}

func (a *telAdapter) StartSpan(ctx context.Context, name string) (context.Context, sdk.SpanFinish) {
	ctx, span := a.t.Tracer(a.name).Start(ctx, name)
	return ctx, func(err error) {
		if err != nil {
			span.SetStatus(codes.Error, err.Error())
			span.SetAttributes(attribute.String("error.message", err.Error()))
		}
		span.End()
	}
}

// ---- Config adapter ----

type cfgAdapter struct{ m *config.Manager }

func newConfigAdapter(m *config.Manager) sdk.ConfigReader { return &cfgAdapter{m: m} }

func (c *cfgAdapter) Endpoints() []sdk.EndpointRef {
	in := c.m.Get().Endpoints
	out := make([]sdk.EndpointRef, 0, len(in))
	for _, e := range in {
		methods := make([]sdk.AuthKind, 0, len(e.AuthMethods))
		for _, m := range e.AuthMethods {
			methods = append(methods, sdk.AuthKind(m))
		}
		if len(methods) == 0 {
			methods = []sdk.AuthKind{sdk.AuthKindCookie}
		}
		out = append(out, sdk.EndpointRef{
			ID:               e.ID,
			Label:            e.Label,
			URL:              e.URL,
			PluginID:         e.PluginID,
			AuthMethods:      methods,
			PinnedCertB64:    e.PinnedCertB64,
			AllowInsecureTLS: e.AllowInsecureTLS,
			Tags:             e.Tags,
		})
	}
	return out
}

// CustomCAFiles exposes the operator-configured CA bundle paths so
// plugins seeding TLS RootCAs honor the operator's internal CA. The
// returned slice is a fresh copy — modifications by the caller don't
// race with the config Manager's internal state.
func (c *cfgAdapter) CustomCAFiles() []string {
	in := c.m.Get().Security.CustomCAFiles
	out := make([]string, len(in))
	copy(out, in)
	return out
}

// ProxyURL exposes the operator-configured egress proxy (network.proxy_url).
// Plugins detect it by type assertion; empty means "use the standard
// HTTP(S)_PROXY / NO_PROXY environment variables".
func (c *cfgAdapter) ProxyURL() string {
	return c.m.Get().Network.ProxyURL
}

// NoProxy exposes network.no_proxy as a fresh copy, for the same reason as
// CustomCAFiles.
func (c *cfgAdapter) NoProxy() []string {
	in := c.m.Get().Network.NoProxy
	out := make([]string, len(in))
	copy(out, in)
	return out
}
