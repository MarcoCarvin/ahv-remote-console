package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

// TestResolveProxy_DefaultsToEnvironment (OBS-02): with no explicit ProxyURL the
// client defaults to http.ProxyFromEnvironment so standard HTTP(S)_PROXY /
// NO_PROXY env vars are honored with zero config. (We assert identity with
// http.ProxyFromEnvironment rather than driving env behavior, because the
// stdlib caches the env-derived proxy config once per process — t.Setenv after
// start would not be observed and would make this flaky.)
func TestResolveProxy_DefaultsToEnvironment(t *testing.T) {
	fn, err := resolveProxy("", nil)
	if err != nil {
		t.Fatalf("resolveProxy: %v", err)
	}
	if fn == nil {
		t.Fatal("default proxy func must be non-nil")
	}
	if reflect.ValueOf(fn).Pointer() != reflect.ValueOf(http.ProxyFromEnvironment).Pointer() {
		t.Errorf("empty ProxyURL must default to http.ProxyFromEnvironment")
	}
}

// TestResolveProxy_ExplicitURL (OBS-02): an explicit ProxyURL is used for all
// hosts not in NoProxy.
func TestResolveProxy_ExplicitURL(t *testing.T) {
	fn, err := resolveProxy("http://corp-proxy.example:8080", nil)
	if err != nil {
		t.Fatalf("resolveProxy: %v", err)
	}
	req, _ := http.NewRequest(http.MethodGet, "https://prism.example:9440/x", nil)
	u, err := fn(req)
	if err != nil {
		t.Fatalf("proxy func: %v", err)
	}
	if u == nil || u.Host != "corp-proxy.example:8080" {
		t.Errorf("expected explicit proxy, got %v", u)
	}
}

// TestResolveProxy_NoProxyBypass (OBS-02): a host matching a NoProxy entry dials
// directly (proxy func returns nil), including suffix matches.
func TestResolveProxy_NoProxyBypass(t *testing.T) {
	fn, err := resolveProxy("http://corp-proxy.example:8080", []string{"prism.internal", ".corp.local"})
	if err != nil {
		t.Fatalf("resolveProxy: %v", err)
	}
	cases := []struct {
		target     string
		wantBypass bool
	}{
		{"https://prism.internal:9440/x", true},   // exact host
		{"https://node1.corp.local:9440/x", true}, // suffix match
		{"https://other.example:9440/x", false},   // not in no_proxy → proxied
	}
	for _, tc := range cases {
		req, _ := http.NewRequest(http.MethodGet, tc.target, nil)
		u, err := fn(req)
		if err != nil {
			t.Fatalf("proxy func(%s): %v", tc.target, err)
		}
		bypassed := u == nil
		if bypassed != tc.wantBypass {
			t.Errorf("%s: bypass=%v, want %v (proxy=%v)", tc.target, bypassed, tc.wantBypass, u)
		}
	}
}

func TestResolveProxy_BadURLErrors(t *testing.T) {
	if _, err := resolveProxy("://not a url", nil); err == nil {
		t.Errorf("expected error on unparseable proxy URL")
	}
}

// TestNewClient_HonorsExplicitProxy (OBS-02): an explicitly-configured proxy is
// wired into the REST transport AND surfaced via Proxy() so the WS dialer reuses
// it. We assert the transport actually routes a request through a stub proxy
// server.
func TestNewClient_HonorsExplicitProxy(t *testing.T) {
	proxied := false
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// A forward proxy sees the absolute-form request URI / full host.
		proxied = true
		w.WriteHeader(http.StatusOK)
	}))
	defer proxy.Close()

	c, err := NewClient(ClientOptions{
		BaseURL:  "http://prism.example:9440",
		ProxyURL: proxy.URL,
	})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	// Proxy() must expose the same function for the WS dialer.
	if c.Proxy() == nil {
		t.Fatal("Proxy() must never be nil")
	}
	req, _ := http.NewRequest(http.MethodGet, "http://prism.example:9440/api/nutanix/v3/users/me", nil)
	if pu, _ := c.Proxy()(req); pu == nil || !strings.HasPrefix(pu.String(), "http") {
		t.Fatalf("Proxy() did not return the configured proxy: %v", pu)
	}
	// Drive a real request through the transport so we prove it transits the proxy.
	_ = c.VerifyAuth(context.Background())
	if !proxied {
		t.Errorf("REST request did not transit the configured proxy")
	}
}

// TestNewClient_ProxyFromEnvironmentDefault (OBS-02): when no explicit proxy is
// set, Proxy() is http.ProxyFromEnvironment (non-nil) so both the REST transport
// and the WS dialer inherit env-var proxy behavior.
func TestNewClient_ProxyFromEnvironmentDefault(t *testing.T) {
	c, err := NewClient(ClientOptions{BaseURL: "https://prism.example:9440"})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	if c.Proxy() == nil {
		t.Fatal("Proxy() must default to a non-nil ProxyFromEnvironment")
	}
	if reflect.ValueOf(c.Proxy()).Pointer() != reflect.ValueOf(http.ProxyFromEnvironment).Pointer() {
		t.Errorf("Proxy() must default to http.ProxyFromEnvironment when no proxy is configured")
	}
}
