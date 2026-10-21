package ahv

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/MarcoCarvin/ahv-remote-console/apps/runtime/pkg/sdk"
	"github.com/MarcoCarvin/ahv-remote-console/plugins/ahv/internal/api"
)

// stubConfig is a minimal ConfigReader returning a fixed endpoint set.
type stubConfig struct{ eps []sdk.EndpointRef }

func (s stubConfig) Endpoints() []sdk.EndpointRef { return s.eps }
func (s stubConfig) CustomCAFiles() []string      { return nil }

// emptyKeystore has no stored credentials for any endpoint — Get always fails,
// so rehydrate() cannot reconstruct a session.
type emptyKeystore struct{}

func (emptyKeystore) Get(string) ([]byte, error) { return nil, errors.New("not found") }
func (emptyKeystore) Set(string, []byte) error   { return nil }
func (emptyKeystore) Delete(string) error        { return nil }

func newHealthPlugin(eps []sdk.EndpointRef) *Plugin {
	return &Plugin{
		deps: sdk.Dependencies{
			Logger:   testLogger{newDiscardLogger()},
			Keystore: emptyKeystore{},
			Config:   stubConfig{eps: eps},
		},
		state:    map[string]*endpointState{},
		clients:  map[string]*api.Client{},
		sessions: map[string]*sessionState{},
		cache:    newResourceCache(time.Second),
	}
}

// TestEndpointHealth_unreachableHost_isOfflineAndCarriesNoClusterData probes an
// endpoint whose host doesn't resolve/answer. The reachability ping is
// unauthenticated (no credentials sent, no body read), so the only outcomes are
// "healthy" (host answered) or "offline" (transport failure) — and the result
// carries no cluster fields, so nothing leaks regardless of sign-in state.
func TestEndpointHealth_unreachableHost_isOfflineAndCarriesNoClusterData(t *testing.T) {
	p := newHealthPlugin([]sdk.EndpointRef{{
		ID:    "ep1",
		Label: "Production",
		// RFC 6761 .invalid TLD never resolves — a deterministic transport
		// failure, so the probe must report offline without hanging.
		URL:         "https://prism.invalid:9440",
		PluginID:    pluginID,
		AuthMethods: []sdk.AuthKind{sdk.AuthKindCookie},
	}})

	h, err := p.EndpointHealth(context.Background(), "ep1")
	if err != nil {
		t.Fatalf("EndpointHealth returned error: %v", err)
	}
	if h.EndpointID != "ep1" {
		t.Fatalf("endpoint id mismatch: %q", h.EndpointID)
	}
	if h.Status != "offline" {
		t.Errorf("status = %q, want offline", h.Status)
	}
	if h.RTTMs != nil {
		t.Errorf("offline probe should not report latency, got %v", *h.RTTMs)
	}
}

// TestEndpointHealth_unknownEndpoint_errors confirms the plugin only reports
// endpoints it owns; an unknown id (or one owned by another plugin) errors
// rather than fabricating a status.
func TestEndpointHealth_unknownEndpoint_errors(t *testing.T) {
	p := newHealthPlugin([]sdk.EndpointRef{{
		ID:          "other",
		URL:         "https://other.local",
		PluginID:    "some-other-plugin",
		AuthMethods: []sdk.AuthKind{sdk.AuthKindCookie},
	}})
	if _, err := p.EndpointHealth(context.Background(), "other"); err == nil {
		t.Fatal("expected error for endpoint owned by another plugin, got nil")
	}
	if _, err := p.EndpointHealth(context.Background(), "does-not-exist"); err == nil {
		t.Fatal("expected error for unknown endpoint, got nil")
	}
}
