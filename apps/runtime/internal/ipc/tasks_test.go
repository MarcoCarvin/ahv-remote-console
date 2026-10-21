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

// taskStubID is a Plugin that implements sdk.TaskCapable; plainStubID (from
// iso_catalog_test.go) does NOT, used for the 503 task_unsupported path.
const taskStubID = "stub-tasks"

var (
	stubListTasksFn func() ([]sdk.TaskInfo, error)
	stubGetTaskFn   func(id string) (sdk.TaskInfo, error)
)

func init() {
	sdk.Register(func() sdk.Plugin { return &taskStubPlugin{id: taskStubID} })
}

// taskStubPlugin implements sdk.Plugin + sdk.TaskCapable, delegating to the
// per-test hook vars.
type taskStubPlugin struct{ id string }

func (p *taskStubPlugin) Manifest() sdk.Manifest                       { return sdk.Manifest{ID: p.id, Version: "test"} }
func (p *taskStubPlugin) Init(context.Context, sdk.Dependencies) error { return nil }
func (p *taskStubPlugin) Shutdown(context.Context) error               { return nil }
func (p *taskStubPlugin) AuthStart(context.Context, json.RawMessage) (any, error) {
	return nil, nil
}
func (p *taskStubPlugin) AuthComplete(context.Context, json.RawMessage) (any, error) {
	return nil, nil
}
func (p *taskStubPlugin) AuthRefresh(context.Context) error { return nil }
func (p *taskStubPlugin) Logout(context.Context) error      { return nil }
func (p *taskStubPlugin) ListResources(context.Context, sdk.ListQuery) (sdk.ListResult, error) {
	return sdk.ListResult{}, nil
}
func (p *taskStubPlugin) RefreshResources(context.Context) error { return nil }
func (p *taskStubPlugin) OpenSession(_ context.Context, resourceID string) (sdk.SessionInfo, error) {
	return sdk.SessionInfo{ResourceID: resourceID, PluginID: p.id}, nil
}
func (p *taskStubPlugin) OpenStream(context.Context, string) (*websocket.Conn, error) {
	return nil, errors.New("not used")
}
func (p *taskStubPlugin) CloseSession(context.Context, string) error { return nil }

func (p *taskStubPlugin) ListTasks(context.Context) ([]sdk.TaskInfo, error) {
	return stubListTasksFn()
}
func (p *taskStubPlugin) GetTask(_ context.Context, id string) (sdk.TaskInfo, error) {
	return stubGetTaskFn(id)
}

var _ sdk.TaskCapable = (*taskStubPlugin)(nil)

// tasksTestRouter builds a router over a real plugins.Host (LoadAll loads the
// registered stubs).
func tasksTestRouter(t *testing.T) *router {
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

func TestListTasksHandler_OK(t *testing.T) {
	stubListTasksFn = func() ([]sdk.TaskInfo, error) {
		return []sdk.TaskInfo{
			{ID: "task-1", PluginID: taskStubID, Kind: "vm_power", State: sdk.TaskRunning, PercentComplete: 30},
		}, nil
	}
	r := tasksTestRouter(t)

	req := httptest.NewRequest(http.MethodGet, "/v1/tasks", nil)
	w := httptest.NewRecorder()
	r.listTasks(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var resp struct {
		Tasks []sdk.TaskInfo `json:"tasks"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// The host also loads the AHV plugin (TaskCapable, empty registry) and the
	// catalog stub (not TaskCapable). The stub's one task must be present.
	found := false
	for _, task := range resp.Tasks {
		if task.ID == "task-1" {
			found = true
		}
	}
	if !found {
		t.Errorf("listTasks did not aggregate the stub's task: %+v", resp.Tasks)
	}
}

func TestGetTaskHandler_OK(t *testing.T) {
	stubGetTaskFn = func(id string) (sdk.TaskInfo, error) {
		if id != "task-1" {
			return sdk.TaskInfo{}, errors.New("unknown")
		}
		return sdk.TaskInfo{ID: "task-1", PluginID: taskStubID, State: sdk.TaskSucceeded, PercentComplete: 100}, nil
	}
	r := tasksTestRouter(t)

	req := httptest.NewRequest(http.MethodGet, "/v1/tasks/task-1", nil)
	req.SetPathValue("id", "task-1")
	w := httptest.NewRecorder()
	r.getTask(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var resp struct {
		Task sdk.TaskInfo `json:"task"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Task.ID != "task-1" || resp.Task.State != sdk.TaskSucceeded {
		t.Errorf("unexpected task: %+v", resp.Task)
	}
}

func TestGetTaskHandler_NotFound(t *testing.T) {
	stubGetTaskFn = func(string) (sdk.TaskInfo, error) {
		return sdk.TaskInfo{}, errors.New("unknown task")
	}
	r := tasksTestRouter(t)

	req := httptest.NewRequest(http.MethodGet, "/v1/tasks/nope", nil)
	req.SetPathValue("id", "nope")
	w := httptest.NewRecorder()
	r.getTask(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%s", w.Code, w.Body.String())
	}
	if code := decodeErrCode(t, w.Body.Bytes()); code != "task_not_found" {
		t.Errorf("error code = %q, want task_not_found", code)
	}
}
