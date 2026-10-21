package ahv

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MarcoCarvin/ahv-remote-console/apps/runtime/pkg/sdk"
	"github.com/MarcoCarvin/ahv-remote-console/plugins/ahv/internal/api"
)

const cacheTestVMUUID = "11111111-2222-3333-4444-555555555555"

// newClusterCachePlugin builds a plugin literal (no Init) with the
// cluster-UUID cache map initialized, mirroring newRotationPlugin.
func newClusterCachePlugin(eps []sdk.EndpointRef, ks sdk.Keystore) *Plugin {
	p := newRotationPlugin(eps, ks)
	p.clusterUUIDs = map[string]consoleTarget{}
	return p
}

// registerCookieClient wires a ready-to-use API client for the endpoint so
// clientFor never goes to the keystore/network during the test.
func registerCookieClient(t *testing.T, p *Plugin, ep sdk.EndpointRef) {
	t.Helper()
	cookies := []api.SessionCookie{{Name: "NTNX_MERCURY_IAM_SESSION", Value: "v"}}
	client, err := p.newCookieClient(ep, cookies)
	if err != nil {
		t.Fatalf("newCookieClient: %v", err)
	}
	p.replaceClient(ep.ID, client, &endpointState{method: sdk.AuthKindCookie, sessionCookies: cookies})
}

func cacheTestEndpoint(srvURL string) sdk.EndpointRef {
	return sdk.EndpointRef{
		ID:          "ep1",
		URL:         srvURL,
		PluginID:    pluginID,
		AuthMethods: []sdk.AuthKind{sdk.AuthKindCookie},
	}
}

// getVMJSON is the v3 single-VM read payload the tests serve, carrying the
// cluster UUID OpenSession needs.
const getVMJSON = `{
	"metadata": {"uuid": "` + cacheTestVMUUID + `"},
	"status": {
		"name": "vm-fresh",
		"cluster_reference": {"kind": "cluster", "uuid": "cluster-fresh"},
		"resources": {"power_state": "ON"}
	}
}`

// newGetVMServer serves GET /api/nutanix/v3/vms/<uuid> and counts hits so
// tests can assert whether OpenSession paid the round-trip.
func newGetVMServer(t *testing.T, hits *atomic.Int32) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/api/nutanix/v3/vms/"+cacheTestVMUUID {
			hits.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(getVMJSON))
			return
		}
		http.Error(w, "unexpected "+r.Method+" "+r.URL.Path, http.StatusNotFound)
	}))
}

// A warm cluster-UUID cache must make OpenSession skip GetVM entirely and
// stamp the session (metadata + sessionState) from the cached values.
func TestOpenSession_CacheHitSkipsGetVM(t *testing.T) {
	var hits atomic.Int32
	srv := newGetVMServer(t, &hits)
	defer srv.Close()

	ep := cacheTestEndpoint(srv.URL)
	p := newClusterCachePlugin([]sdk.EndpointRef{ep}, newRecordingKeystore())
	registerCookieClient(t, p, ep)
	p.storeConsoleTarget(ep.ID, cacheTestVMUUID, consoleTarget{ClusterUUID: "cluster-cached", VMName: "vm-cached"})

	info, err := p.OpenSession(context.Background(), ep.ID+"/"+cacheTestVMUUID)
	if err != nil {
		t.Fatalf("OpenSession: %v", err)
	}
	if n := hits.Load(); n != 0 {
		t.Errorf("GetVM called %d times on a cache hit, want 0", n)
	}
	if got := info.Metadata["cluster_uuid"]; got != "cluster-cached" {
		t.Errorf("metadata cluster_uuid = %q, want cluster-cached", got)
	}
	if got := info.Metadata["vm_name"]; got != "vm-cached" {
		t.Errorf("metadata vm_name = %q, want vm-cached", got)
	}
	p.mu.RLock()
	st := p.sessions[info.ID]
	p.mu.RUnlock()
	if st == nil || st.ClusterUUID != "cluster-cached" {
		t.Errorf("sessionState cluster UUID = %+v, want cluster-cached", st)
	}
}

// A cold cache must fall back to GetVM — and the fetched value must land in
// the cache so the next open for the same VM is a hit.
func TestOpenSession_CacheMissFallsBackToGetVM(t *testing.T) {
	var hits atomic.Int32
	srv := newGetVMServer(t, &hits)
	defer srv.Close()

	ep := cacheTestEndpoint(srv.URL)
	p := newClusterCachePlugin([]sdk.EndpointRef{ep}, newRecordingKeystore())
	registerCookieClient(t, p, ep)

	info, err := p.OpenSession(context.Background(), ep.ID+"/"+cacheTestVMUUID)
	if err != nil {
		t.Fatalf("OpenSession: %v", err)
	}
	if n := hits.Load(); n != 1 {
		t.Fatalf("GetVM called %d times on a cache miss, want 1", n)
	}
	if got := info.Metadata["cluster_uuid"]; got != "cluster-fresh" {
		t.Errorf("metadata cluster_uuid = %q, want cluster-fresh", got)
	}
	if got := info.Metadata["vm_name"]; got != "vm-fresh" {
		t.Errorf("metadata vm_name = %q, want vm-fresh", got)
	}

	// Second open: the fallback fetch must have primed the cache.
	if _, err := p.OpenSession(context.Background(), ep.ID+"/"+cacheTestVMUUID); err != nil {
		t.Fatalf("second OpenSession: %v", err)
	}
	if n := hits.Load(); n != 1 {
		t.Errorf("GetVM called %d times across two opens, want 1 (second open should hit the cache)", n)
	}
}

// An empty cached cluster UUID must be a MISS: caching "" would build the
// proxyClusterUuid-less WS URL that 404s on Prism Central.
func TestOpenSession_EmptyCachedClusterUUIDIsMiss(t *testing.T) {
	var hits atomic.Int32
	srv := newGetVMServer(t, &hits)
	defer srv.Close()

	ep := cacheTestEndpoint(srv.URL)
	p := newClusterCachePlugin([]sdk.EndpointRef{ep}, newRecordingKeystore())
	registerCookieClient(t, p, ep)
	// Seed the map directly — storeConsoleTarget itself refuses empty UUIDs.
	p.mu.Lock()
	p.clusterUUIDs[ep.ID+"/"+cacheTestVMUUID] = consoleTarget{ClusterUUID: "", VMName: "stale"}
	p.mu.Unlock()

	info, err := p.OpenSession(context.Background(), ep.ID+"/"+cacheTestVMUUID)
	if err != nil {
		t.Fatalf("OpenSession: %v", err)
	}
	if n := hits.Load(); n != 1 {
		t.Errorf("GetVM called %d times with an empty cached UUID, want 1 (empty = miss)", n)
	}
	if got := info.Metadata["cluster_uuid"]; got != "cluster-fresh" {
		t.Errorf("metadata cluster_uuid = %q, want cluster-fresh", got)
	}
}

// A real vms/list fetch must populate the cluster-UUID cache from the list
// payload — that is what lets the subsequent OpenSession skip GetVM.
func TestListResources_PopulatesClusterUUIDCache(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/api/nutanix/v3/vms/list" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{
				"entities": [{
					"metadata": {"uuid": "` + cacheTestVMUUID + `"},
					"status": {
						"name": "vm-list",
						"description": "",
						"cluster_reference": {"kind": "cluster", "name": "cl1", "uuid": "cluster-from-list"},
						"resources": {"power_state": "ON", "nic_list": []}
					}
				}]
			}`))
			return
		}
		http.Error(w, "unexpected "+r.Method+" "+r.URL.Path, http.StatusNotFound)
	}))
	defer srv.Close()

	ep := cacheTestEndpoint(srv.URL)
	p := newClusterCachePlugin([]sdk.EndpointRef{ep}, newRecordingKeystore())
	registerCookieClient(t, p, ep)

	if _, err := p.ListResources(context.Background(), sdk.ListQuery{Page: 0, PageSize: 50}); err != nil {
		t.Fatalf("ListResources: %v", err)
	}
	target, ok := p.cachedConsoleTarget(ep.ID, cacheTestVMUUID)
	if !ok {
		t.Fatal("cluster-UUID cache not populated by ListResources")
	}
	if target.ClusterUUID != "cluster-from-list" || target.VMName != "vm-list" {
		t.Errorf("cached target = %+v, want {cluster-from-list vm-list}", target)
	}
}

// newPreflightTLSServer rejects the VNC WS upgrade with an HTTP status (the
// stale-cluster-UUID signature) and serves the GetVM preflight per getVM.
func newPreflightTLSServer(t *testing.T, getVM http.HandlerFunc) *httptest.Server {
	t.Helper()
	return httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/vnc/vm/"):
			w.WriteHeader(http.StatusServiceUnavailable)
		case r.Method == http.MethodGet && r.URL.Path == "/api/nutanix/v3/vms/"+cacheTestVMUUID:
			getVM(w, r)
		default:
			http.Error(w, "unexpected "+r.Method+" "+r.URL.Path, http.StatusNotFound)
		}
	}))
}

func newPreflightPlugin(t *testing.T, srv *httptest.Server) (*Plugin, sdk.EndpointRef) {
	t.Helper()
	ep := cacheTestEndpoint(srv.URL)
	ep.PinnedCertB64 = base64.StdEncoding.EncodeToString(srv.Certificate().Raw)
	p := newClusterCachePlugin([]sdk.EndpointRef{ep}, newRecordingKeystore())
	registerCookieClient(t, p, ep)
	p.mu.Lock()
	p.sessions["sid"] = &sessionState{
		EndpointID:  ep.ID,
		ResourceID:  ep.ID + "/" + cacheTestVMUUID,
		VMUUID:      cacheTestVMUUID,
		ClusterUUID: "cluster-stale",
		OpenedAt:    time.Now(),
	}
	p.mu.Unlock()
	p.storeConsoleTarget(ep.ID, cacheTestVMUUID, consoleTarget{ClusterUUID: "cluster-stale", VMName: "vm-stale"})
	return p, ep
}

// When the WS upgrade is rejected with an HTTP status and the preflight GetVM
// succeeds, the preflight must refresh BOTH the cluster-UUID cache and the
// session's cluster UUID so the next dial routes with current data.
func TestOpenStream_PreflightRefreshesStaleClusterUUID(t *testing.T) {
	srv := newPreflightTLSServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(getVMJSON))
	})
	defer srv.Close()
	p, ep := newPreflightPlugin(t, srv)

	_, err := p.OpenStream(context.Background(), "sid")
	if err == nil || !strings.Contains(err.Error(), "HTTP 503") {
		t.Fatalf("OpenStream error = %v, want the HTTP 503 dial failure", err)
	}
	target, ok := p.cachedConsoleTarget(ep.ID, cacheTestVMUUID)
	if !ok || target.ClusterUUID != "cluster-fresh" {
		t.Errorf("cache after preflight = %+v, %v; want refreshed to cluster-fresh", target, ok)
	}
	p.mu.RLock()
	got := p.sessions["sid"].ClusterUUID
	p.mu.RUnlock()
	if got != "cluster-fresh" {
		t.Errorf("sessionState cluster UUID after preflight = %q, want cluster-fresh", got)
	}
}

// When the preflight GetVM itself fails, the stale cache entry must still be
// gone — the next OpenSession then refetches instead of reusing bad routing.
func TestOpenStream_PreflightFailureInvalidatesClusterUUID(t *testing.T) {
	srv := newPreflightTLSServer(t, func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"message":"not found"}`, http.StatusNotFound)
	})
	defer srv.Close()
	p, ep := newPreflightPlugin(t, srv)

	_, err := p.OpenStream(context.Background(), "sid")
	if err == nil || !strings.Contains(err.Error(), "vm not found") {
		t.Fatalf("OpenStream error = %v, want vm not found", err)
	}
	if target, ok := p.cachedConsoleTarget(ep.ID, cacheTestVMUUID); ok {
		t.Errorf("stale cache entry survived a failed preflight: %+v", target)
	}
}
