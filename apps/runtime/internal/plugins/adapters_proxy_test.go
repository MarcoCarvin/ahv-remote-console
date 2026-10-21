package plugins

import (
	"io"
	"log/slog"
	"path/filepath"
	"slices"
	"testing"

	"github.com/MarcoCarvin/ahv-remote-console/apps/runtime/internal/config"
)

// The AHV plugin reads the egress proxy through a type assertion on the config
// reader it is given. These tests pin that the runtime's adapter satisfies that
// shape and passes network.proxy_url / network.no_proxy through.
func TestCfgAdapter_ExposesProxySettings(t *testing.T) {
	m, err := config.Load(filepath.Join(t.TempDir(), "config.json"), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	if err := m.Update(func(c *config.Config) {
		c.Network.ProxyURL = "http://proxy.example:3128"
		c.Network.NoProxy = []string{".example.internal", "10.0.0.0/8"}
	}); err != nil {
		t.Fatalf("Update: %v", err)
	}

	var reader any = &cfgAdapter{m: m}
	pc, ok := reader.(interface {
		ProxyURL() string
		NoProxy() []string
	})
	if !ok {
		t.Fatal("cfgAdapter does not expose ProxyURL/NoProxy")
	}
	if got := pc.ProxyURL(); got != "http://proxy.example:3128" {
		t.Errorf("ProxyURL() = %q", got)
	}
	got := pc.NoProxy()
	if !slices.Equal(got, []string{".example.internal", "10.0.0.0/8"}) {
		t.Errorf("NoProxy() = %v", got)
	}
	got[0] = "mutated"
	if m.Get().Network.NoProxy[0] != ".example.internal" {
		t.Error("NoProxy() must return a copy, not the manager's slice")
	}
}
