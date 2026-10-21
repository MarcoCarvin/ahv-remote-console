package ipc

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gorilla/websocket"

	"github.com/MarcoCarvin/ahv-remote-console/apps/runtime/internal/audit"
	"github.com/MarcoCarvin/ahv-remote-console/apps/runtime/internal/plugins"
	"github.com/MarcoCarvin/ahv-remote-console/apps/runtime/pkg/sdk"
)

// The two stub plugins are registered ONCE (init below) so the host's LoadAll
// instantiates them; each test sets the per-test hook vars the registered
// instances read. (sdk.Register is process-global and additive, and a type
// either implements ImageCatalogCapable or not — so we register one capable and
// one plain plugin with distinct IDs rather than toggling at runtime.)
const (
	catalogStubID = "stub-catalog"
	plainStubID   = "stub-plain"
)

var (
	stubListFn  func(sessionID string) ([]sdk.CatalogImage, error)
	stubMountFn func(sessionID, imageUUID string) (sdk.ISOMountResult, error)
	stubEjectFn func(sessionID string) (sdk.EjectISOResult, error)
)

func init() {
	sdk.Register(func() sdk.Plugin { return &catalogStubPlugin{id: catalogStubID} })
	sdk.Register(func() sdk.Plugin { return &plainStubPlugin{id: plainStubID} })
}

// catalogStubPlugin implements sdk.Plugin + sdk.ImageCatalogCapable for the
// router handler tests. It delegates to the per-test hook vars.
type catalogStubPlugin struct {
	id string
}

func (p *catalogStubPlugin) Manifest() sdk.Manifest {
	return sdk.Manifest{ID: p.id, Version: "test"}
}
func (p *catalogStubPlugin) Init(context.Context, sdk.Dependencies) error { return nil }
func (p *catalogStubPlugin) Shutdown(context.Context) error               { return nil }
func (p *catalogStubPlugin) AuthStart(context.Context, json.RawMessage) (any, error) {
	return nil, nil
}
func (p *catalogStubPlugin) AuthComplete(context.Context, json.RawMessage) (any, error) {
	return nil, nil
}
func (p *catalogStubPlugin) AuthRefresh(context.Context) error { return nil }
func (p *catalogStubPlugin) Logout(context.Context) error      { return nil }
func (p *catalogStubPlugin) ListResources(context.Context, sdk.ListQuery) (sdk.ListResult, error) {
	return sdk.ListResult{}, nil
}
func (p *catalogStubPlugin) RefreshResources(context.Context) error { return nil }
func (p *catalogStubPlugin) OpenSession(_ context.Context, resourceID string) (sdk.SessionInfo, error) {
	return sdk.SessionInfo{ResourceID: resourceID, PluginID: p.id}, nil
}
func (p *catalogStubPlugin) OpenStream(context.Context, string) (*websocket.Conn, error) {
	return nil, errors.New("not used")
}
func (p *catalogStubPlugin) CloseSession(context.Context, string) error { return nil }

func (p *catalogStubPlugin) ListISOImages(_ context.Context, sessionID string) ([]sdk.CatalogImage, error) {
	return stubListFn(sessionID)
}
func (p *catalogStubPlugin) MountISO(_ context.Context, sessionID, imageUUID string) (sdk.ISOMountResult, error) {
	return stubMountFn(sessionID, imageUUID)
}
func (p *catalogStubPlugin) EjectISO(_ context.Context, sessionID string) (sdk.EjectISOResult, error) {
	return stubEjectFn(sessionID)
}

// plainStubPlugin is a Plugin that does NOT implement ImageCatalogCapable, used
// to exercise the 503 iso_mount_unsupported path. It deliberately does NOT
// embed catalogStubPlugin (which would promote the catalog methods).
type plainStubPlugin struct{ id string }

func (p *plainStubPlugin) Manifest() sdk.Manifest {
	return sdk.Manifest{ID: p.id, Version: "test"}
}
func (p *plainStubPlugin) Init(context.Context, sdk.Dependencies) error { return nil }
func (p *plainStubPlugin) Shutdown(context.Context) error               { return nil }
func (p *plainStubPlugin) AuthStart(context.Context, json.RawMessage) (any, error) {
	return nil, nil
}
func (p *plainStubPlugin) AuthComplete(context.Context, json.RawMessage) (any, error) {
	return nil, nil
}
func (p *plainStubPlugin) AuthRefresh(context.Context) error { return nil }
func (p *plainStubPlugin) Logout(context.Context) error      { return nil }
func (p *plainStubPlugin) ListResources(context.Context, sdk.ListQuery) (sdk.ListResult, error) {
	return sdk.ListResult{}, nil
}
func (p *plainStubPlugin) RefreshResources(context.Context) error { return nil }
func (p *plainStubPlugin) OpenSession(_ context.Context, resourceID string) (sdk.SessionInfo, error) {
	return sdk.SessionInfo{ResourceID: resourceID, PluginID: p.id}, nil
}
func (p *plainStubPlugin) OpenStream(context.Context, string) (*websocket.Conn, error) {
	return nil, errors.New("not used")
}
func (p *plainStubPlugin) CloseSession(context.Context, string) error { return nil }

var _ sdk.ImageCatalogCapable = (*catalogStubPlugin)(nil)

// catalogTestRouter builds a router over a real plugins.Host (LoadAll loads the
// two stub plugins registered above) with one session opened on pluginID.
// Returns the router and the open session ID.
func catalogTestRouter(t *testing.T, pluginID string) (*router, string) {
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
	plug, ok := host.Get(pluginID)
	if !ok {
		t.Fatalf("plugin %q not loaded", pluginID)
	}
	info, err := host.OpenSession(context.Background(), plug, pluginID+"/vm-1")
	if err != nil {
		t.Fatalf("open session: %v", err)
	}
	r := newRouter(Options{Logger: logger, Plugins: host, Audit: auditLog})
	return r, info.ID
}

func decodeErrCode(t *testing.T, body []byte) string {
	t.Helper()
	var parsed struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		t.Fatalf("decode error body %q: %v", body, err)
	}
	return parsed.Error.Code
}

func TestListISOImagesHandler_OK(t *testing.T) {
	stubListFn = func(string) ([]sdk.CatalogImage, error) {
		return []sdk.CatalogImage{{UUID: "iso-1", Name: "win.iso", State: "COMPLETE"}}, nil
	}
	r, sid := catalogTestRouter(t, catalogStubID)

	req := httptest.NewRequest(http.MethodGet, "/v1/agent/images?session_id="+sid, nil)
	w := httptest.NewRecorder()
	r.listISOImages(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var resp struct {
		Images []sdk.CatalogImage `json:"images"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Images) != 1 || resp.Images[0].UUID != "iso-1" {
		t.Errorf("unexpected images: %+v", resp.Images)
	}
}

func TestListISOImagesHandler_UnknownSession(t *testing.T) {
	r, _ := catalogTestRouter(t, catalogStubID)
	req := httptest.NewRequest(http.MethodGet, "/v1/agent/images?session_id=nope", nil)
	w := httptest.NewRecorder()
	r.listISOImages(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", w.Code)
	}
	if code := decodeErrCode(t, w.Body.Bytes()); code != "session_not_found" {
		t.Errorf("error code = %q, want session_not_found", code)
	}
}

func TestMountISOHandler_MissingImageUUID(t *testing.T) {
	r, sid := catalogTestRouter(t, catalogStubID)
	body := `{"session_id":"` + sid + `"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/vm/mount-iso", strings.NewReader(body))
	w := httptest.NewRecorder()
	r.mountISO(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", w.Code, w.Body.String())
	}
	if code := decodeErrCode(t, w.Body.Bytes()); code != "missing_image_uuid" {
		t.Errorf("error code = %q, want missing_image_uuid", code)
	}
}

func TestMountISOHandler_PluginError(t *testing.T) {
	stubMountFn = func(string, string) (sdk.ISOMountResult, error) {
		return sdk.ISOMountResult{}, errors.New("prism rejected the attach")
	}
	r, sid := catalogTestRouter(t, catalogStubID)
	body := `{"session_id":"` + sid + `","image_uuid":"img-1"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/vm/mount-iso", strings.NewReader(body))
	w := httptest.NewRecorder()
	r.mountISO(w, req)
	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502; body=%s", w.Code, w.Body.String())
	}
	if code := decodeErrCode(t, w.Body.Bytes()); code != "iso_mount_failed" {
		t.Errorf("error code = %q, want iso_mount_failed", code)
	}
}

func TestMountISOHandler_Unsupported(t *testing.T) {
	// A plugin that does NOT implement ImageCatalogCapable → 503.
	r, sid := catalogTestRouter(t, plainStubID)
	body := `{"session_id":"` + sid + `","image_uuid":"img-1"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/vm/mount-iso", strings.NewReader(body))
	w := httptest.NewRecorder()
	r.mountISO(w, req)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; body=%s", w.Code, w.Body.String())
	}
	if code := decodeErrCode(t, w.Body.Bytes()); code != "iso_mount_unsupported" {
		t.Errorf("error code = %q, want iso_mount_unsupported", code)
	}
}

func TestEjectISOHandler_OK(t *testing.T) {
	stubEjectFn = func(string) (sdk.EjectISOResult, error) {
		return sdk.EjectISOResult{Ejected: true}, nil
	}
	r, sid := catalogTestRouter(t, catalogStubID)
	body := `{"session_id":"` + sid + `"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/vm/eject-iso", strings.NewReader(body))
	w := httptest.NewRecorder()
	r.ejectISO(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var resp sdk.EjectISOResult
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !resp.Ejected {
		t.Errorf("expected ejected=true")
	}
}

func TestEjectISOHandler_UnknownSession(t *testing.T) {
	r, _ := catalogTestRouter(t, catalogStubID)
	body := `{"session_id":"does-not-exist"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/vm/eject-iso", strings.NewReader(body))
	w := httptest.NewRecorder()
	r.ejectISO(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", w.Code)
	}
	if code := decodeErrCode(t, w.Body.Bytes()); code != "session_not_found" {
		t.Errorf("error code = %q, want session_not_found", code)
	}
}
