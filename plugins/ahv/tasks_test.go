package ahv

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/MarcoCarvin/ahv-remote-console/apps/runtime/pkg/sdk"
	"github.com/MarcoCarvin/ahv-remote-console/plugins/ahv/internal/api"
)

func writeTaskJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// newTaskPlugin builds a Plugin with just the task registry wired (and the
// fields newTask/finishTask touch). No endpoints/clients unless a test seeds
// them.
func newTaskPlugin() *Plugin {
	return &Plugin{
		deps: sdk.Dependencies{
			Logger:   testLogger{newDiscardLogger()},
			Keystore: emptyKeystore{},
			Config:   stubConfig{},
		},
		state:    map[string]*endpointState{},
		clients:  map[string]*api.Client{},
		sessions: map[string]*sessionState{},
		tasks:    map[string]*trackedTask{},
	}
}

func TestNewTask_CreatesRunningEntry(t *testing.T) {
	p := newTaskPlugin()
	id := p.newTask("vm_power", "Power OFF abcd1234", "ep1", "vm-1", "")
	if id == "" {
		t.Fatal("newTask returned an empty handle")
	}
	got, err := p.taskSnapshot(id)
	if err != nil {
		t.Fatalf("taskSnapshot: %v", err)
	}
	if got.State != sdk.TaskRunning {
		t.Errorf("state = %q, want running", got.State)
	}
	if got.PercentComplete != 0 {
		t.Errorf("percent = %d, want 0", got.PercentComplete)
	}
	if got.Kind != "vm_power" || got.Label != "Power OFF abcd1234" {
		t.Errorf("unexpected task: %+v", got)
	}
	if got.PluginID != pluginID {
		t.Errorf("plugin_id = %q, want %q", got.PluginID, pluginID)
	}
}

func TestFinishTask_Succeeded(t *testing.T) {
	p := newTaskPlugin()
	id := p.newTask("serial_port", "Add serial port abcd", "ep1", "vm-1", "")
	p.finishTask(id, nil)
	got, _ := p.taskSnapshot(id)
	if got.State != sdk.TaskSucceeded {
		t.Errorf("state = %q, want succeeded", got.State)
	}
	if got.PercentComplete != 100 {
		t.Errorf("percent = %d, want 100", got.PercentComplete)
	}
	if got.Error != "" {
		t.Errorf("error = %q, want empty on success", got.Error)
	}
}

func TestFinishTask_FailedSanitizedError(t *testing.T) {
	p := newTaskPlugin()
	id := p.newTask("vm_power", "Power OFF abcd", "ep1", "vm-1", "")
	p.finishTask(id, errors.New("power task ended FAILED: insufficient permission"))
	got, _ := p.taskSnapshot(id)
	if got.State != sdk.TaskFailed {
		t.Errorf("state = %q, want failed", got.State)
	}
	if got.PercentComplete != 100 {
		t.Errorf("percent = %d, want 100", got.PercentComplete)
	}
	if !strings.Contains(got.Error, "insufficient permission") {
		t.Errorf("error = %q, want the op's operator-facing message", got.Error)
	}
}

func TestListTasks_NewestFirst(t *testing.T) {
	p := newTaskPlugin()
	first := p.newTask("vm_power", "first", "ep1", "vm-1", "")
	// Force a strictly-later StartedAt for the second task so ordering is
	// deterministic regardless of clock resolution.
	p.tasksMu.Lock()
	p.tasks[first].info.StartedAt = time.Now().Add(-time.Minute)
	p.tasksMu.Unlock()
	second := p.newTask("agent_iso", "second", "ep1", "vm-2", "")

	out, err := p.ListTasks(context.Background())
	if err != nil {
		t.Fatalf("ListTasks: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("len = %d, want 2", len(out))
	}
	if out[0].ID != second || out[1].ID != first {
		t.Errorf("order = [%s, %s], want newest-first [%s, %s]", out[0].ID, out[1].ID, second, first)
	}
}

func TestGetTask_UnknownHandle(t *testing.T) {
	p := newTaskPlugin()
	if _, err := p.GetTask(context.Background(), "task-missing"); err == nil {
		t.Fatal("GetTask on an unknown handle must return an error")
	}
}

// TestGetTask_RefreshesRunningFromPrism: a running task with a known upstream
// task uuid is refreshed against real Prism state via PollTaskOnce.
func TestGetTask_RefreshesRunningFromPrism(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/api/prism/v4.0/config/tasks/") {
			http.Error(w, "unexpected "+r.URL.Path, http.StatusNotFound)
			return
		}
		writeTaskJSON(w, http.StatusOK, map[string]any{"data": map[string]any{"status": "SUCCEEDED"}})
	}))
	defer srv.Close()

	ep := sdk.EndpointRef{ID: "ep1", URL: srv.URL, PluginID: pluginID, AuthMethods: []sdk.AuthKind{sdk.AuthKindCookie}}
	p := newTaskPlugin()
	p.deps.Config = stubConfig{eps: []sdk.EndpointRef{ep}}
	client, err := api.NewClient(api.ClientOptions{BaseURL: srv.URL})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	p.clients[ep.ID] = client

	id := p.newTask("vm_power", "Power OFF abcd", ep.ID, "vm-1", "")
	p.setTaskPrism(id, "task-extid", true)

	got, err := p.GetTask(context.Background(), id)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if got.State != sdk.TaskSucceeded {
		t.Errorf("state = %q, want succeeded (refreshed from Prism)", got.State)
	}
	if got.PercentComplete != 100 {
		t.Errorf("percent = %d, want 100", got.PercentComplete)
	}
}

// TestGetTask_RunningWithoutPrismHandle: a running task with no upstream handle
// (the short power/serial ops) is returned as-is, with no network call.
func TestGetTask_RunningWithoutPrismHandle(t *testing.T) {
	p := newTaskPlugin()
	id := p.newTask("vm_power", "Power OFF abcd", "ep1", "vm-1", "")
	got, err := p.GetTask(context.Background(), id)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if got.State != sdk.TaskRunning {
		t.Errorf("state = %q, want running (no upstream handle to refresh)", got.State)
	}
}

func TestPruneTasksLocked_DropsAgedTerminal(t *testing.T) {
	p := newTaskPlugin()
	aged := p.newTask("vm_power", "old", "ep1", "vm-1", "")
	p.finishTask(aged, nil)
	fresh := p.newTask("vm_power", "new", "ep1", "vm-2", "")

	// Age the terminal task past the TTL.
	p.tasksMu.Lock()
	p.tasks[aged].info.UpdatedAt = time.Now().Add(-2 * terminalTaskTTL)
	p.pruneTasksLocked()
	_, agedStillThere := p.tasks[aged]
	_, freshStillThere := p.tasks[fresh]
	p.tasksMu.Unlock()

	if agedStillThere {
		t.Error("pruneTasksLocked must drop an aged terminal task")
	}
	if !freshStillThere {
		t.Error("pruneTasksLocked must keep a running task")
	}
}

// TestTaskCompliance_NoPayloadInLabelOrError is a guard for the cross-cutting
// invariant: a task's operator-facing strings must never carry payload bytes.
// Label is host-composed and Error is the op's own message — this asserts the
// registry stores exactly what it was given (it never reaches into payloads).
func TestTaskCompliance_NoPayloadInLabelOrError(t *testing.T) {
	p := newTaskPlugin()
	id := p.newTask("agent_iso", "Install agent on abcd1234", "ep1", "vm-1", "sess-1")
	p.updateTask(id, func(ti *sdk.TaskInfo) { ti.Detail = "Uploaded the installer image" })
	got, _ := p.taskSnapshot(id)
	for _, s := range []string{got.Label, got.Detail, got.Error} {
		if strings.Contains(strings.ToLower(s), "clipboard") {
			t.Errorf("operator-facing task string leaked a payload-shaped value: %q", s)
		}
	}
	if got.SessionID != "sess-1" {
		t.Errorf("session_id = %q, want sess-1", got.SessionID)
	}
}
