package ipc

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gorilla/websocket"

	"github.com/MarcoCarvin/ahv-remote-console/apps/runtime/internal/audit"
	"github.com/MarcoCarvin/ahv-remote-console/apps/runtime/internal/plugins"
	"github.com/MarcoCarvin/ahv-remote-console/apps/runtime/pkg/sdk"
)

const statsStubID = "stub-stats"

// stubStatsFn is the per-test hook the registered stats stub delegates to.
var stubStatsFn func(resourceID string) (sdk.VMStats, error)

func init() {
	sdk.Register(func() sdk.Plugin { return &statsStubPlugin{id: statsStubID} })
}

// statsStubPlugin implements sdk.Plugin + sdk.StatsCapable for the vmStats
// handler tests, delegating GetVMStats to stubStatsFn.
type statsStubPlugin struct{ id string }

func (p *statsStubPlugin) Manifest() sdk.Manifest                       { return sdk.Manifest{ID: p.id, Version: "test"} }
func (p *statsStubPlugin) Init(context.Context, sdk.Dependencies) error { return nil }
func (p *statsStubPlugin) Shutdown(context.Context) error               { return nil }
func (p *statsStubPlugin) AuthStart(context.Context, json.RawMessage) (any, error) {
	return nil, nil
}
func (p *statsStubPlugin) AuthComplete(context.Context, json.RawMessage) (any, error) {
	return nil, nil
}
func (p *statsStubPlugin) AuthRefresh(context.Context) error { return nil }
func (p *statsStubPlugin) Logout(context.Context) error      { return nil }
func (p *statsStubPlugin) ListResources(context.Context, sdk.ListQuery) (sdk.ListResult, error) {
	return sdk.ListResult{}, nil
}
func (p *statsStubPlugin) RefreshResources(context.Context) error { return nil }
func (p *statsStubPlugin) OpenSession(_ context.Context, resourceID string) (sdk.SessionInfo, error) {
	return sdk.SessionInfo{ResourceID: resourceID, PluginID: p.id}, nil
}
func (p *statsStubPlugin) OpenStream(context.Context, string) (*websocket.Conn, error) {
	return nil, errors.New("not used")
}
func (p *statsStubPlugin) CloseSession(context.Context, string) error { return nil }

func (p *statsStubPlugin) GetVMStats(_ context.Context, resourceID string) (sdk.VMStats, error) {
	return stubStatsFn(resourceID)
}

var _ sdk.StatsCapable = (*statsStubPlugin)(nil)

// statsTestRouter builds a router over a real plugins.Host with the stub
// plugins loaded (the stats stub + the plain stub registered in iso_catalog_test).
func statsTestRouter(t *testing.T) *router {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	auditLog, err := audit.Open("", false, logger)
	if err != nil {
		t.Fatalf("audit open: %v", err)
	}
	host, err := plugins.NewHost(plugins.Options{Logger: logger})
	if err != nil {
		t.Fatalf("new host: %v", err)
	}
	t.Cleanup(func() { host.Shutdown(context.Background()) })
	if err := host.LoadAll(); err != nil {
		t.Fatalf("load all: %v", err)
	}
	return newRouter(Options{Logger: logger, Plugins: host, Audit: auditLog})
}

func TestVMStatsHandler_OK(t *testing.T) {
	stubStatsFn = func(string) (sdk.VMStats, error) {
		return sdk.VMStats{
			CPUUsagePpm: 250000, MemoryUsagePpm: 500000,
			DiskReadBps: 1024, DiskWriteBps: 2048,
			NetRxBps: 8192, NetTxBps: 4096,
		}, nil
	}
	r := statsTestRouter(t)

	req := httptest.NewRequest(http.MethodGet,
		"/v1/vm/stats?plugin_id="+statsStubID+"&resource_id=ep1/vm-1", nil)
	w := httptest.NewRecorder()
	r.vmStats(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var resp struct {
		Stats sdk.VMStats `json:"stats"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Stats.CPUUsagePpm != 250000 || resp.Stats.NetRxBps != 8192 {
		t.Errorf("unexpected stats: %+v", resp.Stats)
	}
}

func TestVMStatsHandler_MissingParams(t *testing.T) {
	r := statsTestRouter(t)
	// resource_id omitted.
	req := httptest.NewRequest(http.MethodGet, "/v1/vm/stats?plugin_id="+statsStubID, nil)
	w := httptest.NewRecorder()
	r.vmStats(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
	if code := decodeErrCode(t, w.Body.Bytes()); code != "missing_fields" {
		t.Errorf("error code = %q, want missing_fields", code)
	}
}

func TestVMStatsHandler_Unsupported(t *testing.T) {
	// plainStubPlugin (from iso_catalog_test.go) does NOT implement StatsCapable.
	r := statsTestRouter(t)
	req := httptest.NewRequest(http.MethodGet,
		"/v1/vm/stats?plugin_id="+plainStubID+"&resource_id=ep1/vm-1", nil)
	w := httptest.NewRecorder()
	r.vmStats(w, req)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; body=%s", w.Code, w.Body.String())
	}
	if code := decodeErrCode(t, w.Body.Bytes()); code != "stats_unsupported" {
		t.Errorf("error code = %q, want stats_unsupported", code)
	}
}

func TestVMStatsHandler_RBACDenied(t *testing.T) {
	stubStatsFn = func(string) (sdk.VMStats, error) {
		return sdk.VMStats{}, errors.New("POST /api/nutanix/v3/groups: HTTP 403: forbidden")
	}
	r := statsTestRouter(t)
	req := httptest.NewRequest(http.MethodGet,
		"/v1/vm/stats?plugin_id="+statsStubID+"&resource_id=ep1/vm-1", nil)
	w := httptest.NewRecorder()
	r.vmStats(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body=%s", w.Code, w.Body.String())
	}
	if code := decodeErrCode(t, w.Body.Bytes()); code != "rbac_denied" {
		t.Errorf("error code = %q, want rbac_denied", code)
	}
}
