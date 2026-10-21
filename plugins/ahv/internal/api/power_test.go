package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// powerMock simulates the Prism Central v4 VMM power-action surface:
//
//	GET  /api/vmm/v4.0/ahv/config/vms/{extId}            -> 200 + ETag header
//	POST /api/vmm/v4.0/ahv/config/vms/{extId}/$actions/* -> 202 + TaskReference
//	GET  /api/prism/v4.0/config/tasks/{extId}            -> 200 + task status
//
// It records the action verb, the If-Match / Ntnx-Request-Id headers, and the
// POST body so tests can assert the exact v4 contract per action.
type powerMock struct {
	vmUUID     string
	etag       string
	taskStatus string // default SUCCEEDED
	taskErrMsg string // errorMessages[0].message on the task poll (optional)

	getHit     int
	actionHit  int
	taskHit    int
	probeHit   int
	actionVerb string
	ifMatch    string
	requestID  string
	actionBody []byte
}

func (m *powerMock) handler() http.HandlerFunc {
	vmPath := "/api/vmm/v4.0/ahv/config/vms/" + m.vmUUID
	return func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		switch {
		case p == vmmV4Root && r.Method == http.MethodGet:
			// NUTA-01 capability probe: the VMM v4 collection root. A
			// v4-capable PC answers non-404 here; the mock simulates v4 present.
			m.probeHit++
			w.WriteHeader(http.StatusOK)
		case strings.HasPrefix(p, "/api/prism/v4.0/config/tasks/"):
			m.taskHit++
			st := m.taskStatus
			if st == "" {
				st = "SUCCEEDED"
			}
			data := map[string]any{"status": st}
			if m.taskErrMsg != "" {
				data["errorMessages"] = []map[string]any{{"message": m.taskErrMsg}}
			}
			writePowerJSON(w, http.StatusOK, map[string]any{"data": data})
		case strings.Contains(p, "/$actions/"):
			m.actionHit++
			m.actionVerb = p[strings.LastIndex(p, "/")+1:]
			m.ifMatch = r.Header.Get("If-Match")
			m.requestID = r.Header.Get("Ntnx-Request-Id")
			m.actionBody, _ = io.ReadAll(io.LimitReader(r.Body, 1<<20))
			writePowerJSON(w, http.StatusAccepted, map[string]any{"data": map[string]any{"extId": "task-1"}})
		case p == vmPath && r.Method == http.MethodGet:
			m.getHit++
			w.Header().Set("ETag", m.etag)
			writePowerJSON(w, http.StatusOK, map[string]any{"data": map[string]any{"extId": m.vmUUID}})
		default:
			http.Error(w, "unexpected "+r.Method+" "+p, http.StatusNotFound)
		}
	}
}

func writePowerJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// TestPowerAction_V4 pins the per-action v4 contract: each operator action maps
// to the correct $actions verb, echoes the GET ETag as If-Match, sends a
// Ntnx-Request-Id, attaches the guest transition body only for the graceful
// actions, and polls the returned task to success.
func TestPowerAction_V4(t *testing.T) {
	cases := []struct {
		action    string
		wantVerb  string
		guestBody bool
	}{
		{"ON", "power-on", false},
		{"OFF", "power-off", false},
		{"ACPI_SHUTDOWN", "guest-shutdown", true},
		{"ACPI_REBOOT", "guest-reboot", true},
		{"RESET", "reset", false},
	}
	for _, tc := range cases {
		t.Run(tc.action, func(t *testing.T) {
			mock := &powerMock{vmUUID: "vm-1", etag: "etag-abc"}
			srv := httptest.NewServer(mock.handler())
			defer srv.Close()
			c := newTestClient(t, srv)

			if err := c.PowerAction(context.Background(), "vm-1", tc.action); err != nil {
				t.Fatalf("PowerAction(%s): %v", tc.action, err)
			}
			if mock.getHit != 1 {
				t.Errorf("expected exactly 1 ETag GET, got %d", mock.getHit)
			}
			if mock.actionVerb != tc.wantVerb {
				t.Errorf("action verb = %q, want %q", mock.actionVerb, tc.wantVerb)
			}
			if mock.ifMatch != "etag-abc" {
				t.Errorf("If-Match = %q, want the GET ETag %q", mock.ifMatch, "etag-abc")
			}
			if mock.requestID == "" {
				t.Errorf("Ntnx-Request-Id header missing (idempotency key)")
			}
			if mock.taskHit < 1 {
				t.Errorf("expected the returned task to be polled, got %d polls", mock.taskHit)
			}
			if tc.guestBody {
				var cfg map[string]any
				if err := json.Unmarshal(mock.actionBody, &cfg); err != nil {
					t.Fatalf("guest transition body parse: %v (body=%q)", err, mock.actionBody)
				}
				if v, ok := cfg["shouldEnableScriptExec"]; !ok || v != false {
					t.Errorf("guest body must disable script exec, got %v (body=%q)", v, mock.actionBody)
				}
			} else if n := len(strings.TrimSpace(string(mock.actionBody))); n != 0 {
				t.Errorf("hard action %s must send no body, got %q", tc.action, mock.actionBody)
			}
		})
	}
}

// TestPowerAction_V4_TaskFailed: a FAILED task must surface as an error
// CARRYING Prism's failure detail and a clean task id — "ended FAILED" with a
// raw ergon composite id ("ZXJnb24=:<uuid>") tells the operator nothing.
func TestPowerAction_V4_TaskFailed(t *testing.T) {
	mock := &powerMock{vmUUID: "vm-1", etag: "e", taskStatus: "FAILED",
		taskErrMsg: "No host has enough available resources for VM"}
	srv := httptest.NewServer(mock.handler())
	defer srv.Close()
	c := newTestClient(t, srv)

	err := c.PowerAction(context.Background(), "vm-1", "ACPI_REBOOT")
	if err == nil {
		t.Fatal("expected an error when the power task ends FAILED")
	}
	if !strings.Contains(err.Error(), "No host has enough available resources") {
		t.Errorf("error %q must carry the task's failure detail", err)
	}
	if strings.Contains(err.Error(), "ZXJnb24=") {
		t.Errorf("error %q must not leak the ergon composite prefix", err)
	}
}

// TestPowerAction_V4_TaskFailedNoDetail: a detail-less failure still points
// the operator somewhere actionable instead of a bare status word.
func TestPowerAction_V4_TaskFailedNoDetail(t *testing.T) {
	mock := &powerMock{vmUUID: "vm-1", etag: "e", taskStatus: "FAILED"}
	srv := httptest.NewServer(mock.handler())
	defer srv.Close()
	c := newTestClient(t, srv)

	err := c.PowerAction(context.Background(), "vm-1", "ACPI_REBOOT")
	if err == nil {
		t.Fatal("expected an error when the power task ends FAILED")
	}
	if !strings.Contains(err.Error(), "no failure detail") {
		t.Errorf("error %q must carry the no-detail fallback guidance", err)
	}
}

// TestTaskDisplayID pins the ergon-prefix strip ("ZXJnb24=:<uuid>" -> uuid)
// and the passthrough for already-bare ids.
func TestTaskDisplayID(t *testing.T) {
	if got := taskDisplayID("ZXJnb24=:d09b8cc4-630b-5e23-882f-865d73adb26e"); got != "d09b8cc4-630b-5e23-882f-865d73adb26e" {
		t.Errorf("taskDisplayID composite = %q", got)
	}
	if got := taskDisplayID("task-1"); got != "task-1" {
		t.Errorf("taskDisplayID bare = %q", got)
	}
}

// TestPowerAction_V4_UnsupportedAction: an action outside the known set is
// rejected locally and never touches the API.
func TestPowerAction_V4_UnsupportedAction(t *testing.T) {
	mock := &powerMock{vmUUID: "vm-1", etag: "e"}
	srv := httptest.NewServer(mock.handler())
	defer srv.Close()
	c := newTestClient(t, srv)

	if err := c.PowerAction(context.Background(), "vm-1", "POWERCYCLE"); err == nil {
		t.Fatal("expected an error for an unsupported action")
	}
	if mock.getHit != 0 || mock.actionHit != 0 {
		t.Errorf("unsupported action must not hit the API (get=%d action=%d)", mock.getHit, mock.actionHit)
	}
}

// noV4Mock simulates a v3-only Prism Central: the VMM v4 namespace is NOT
// routed, so EVERY /api/vmm/v4.0/... path (including the capability-probe
// collection root and the per-VM ETag GET) returns 404.
type noV4Mock struct {
	probeHit int
	etagHit  int
}

func (m *noV4Mock) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		if p == vmmV4Root {
			m.probeHit++
		} else if strings.HasPrefix(p, vmmV4Root+"/") {
			m.etagHit++
		}
		http.Error(w, "not found", http.StatusNotFound)
	}
}

// TestPowerAction_NoV4_ActionableError (NUTA-01): a PC without the VMM v4
// namespace must NOT dead-end on a raw HTTP 404 from getETag. The capability
// probe detects the absent namespace and PowerAction returns an actionable
// "requires a Prism Central with VMM v4; this PC reports v3" error — and never
// even attempts the ETag GET (the probe short-circuits first).
func TestPowerAction_NoV4_ActionableError(t *testing.T) {
	mock := &noV4Mock{}
	srv := httptest.NewServer(mock.handler())
	defer srv.Close()
	c := newTestClient(t, srv)

	err := c.PowerAction(context.Background(), "vm-1", "ON")
	if err == nil {
		t.Fatal("expected an error on a v3-only PC")
	}
	if !strings.Contains(err.Error(), "VMM v4") || !strings.Contains(err.Error(), "v3") {
		t.Errorf("error must be actionable about missing VMM v4, got %q", err.Error())
	}
	// A raw 404 must NOT leak through.
	if strings.Contains(err.Error(), "404") || strings.Contains(err.Error(), "HTTP 404") {
		t.Errorf("raw 404 leaked into the operator-facing error: %q", err.Error())
	}
	if mock.probeHit != 1 {
		t.Errorf("expected exactly one capability probe, got %d", mock.probeHit)
	}
	if mock.etagHit != 0 {
		t.Errorf("must not attempt the ETag GET once v4 is known absent, got %d", mock.etagHit)
	}
}

// TestHasVMMv4_CachedOncePerClient (NUTA-01): the capability probe runs exactly
// once per Client and the result is cached across calls.
func TestHasVMMv4_CachedOncePerClient(t *testing.T) {
	mock := &noV4Mock{}
	srv := httptest.NewServer(mock.handler())
	defer srv.Close()
	c := newTestClient(t, srv)

	for i := 0; i < 3; i++ {
		if c.HasVMMv4(context.Background()) {
			t.Fatalf("expected HasVMMv4=false against a v3-only PC")
		}
	}
	if mock.probeHit != 1 {
		t.Errorf("capability probe must be cached (one HTTP probe), got %d", mock.probeHit)
	}
}

// noTaskRefMock answers the capability probe + ETag GET like a v4 PC but returns
// a 202 Accepted with an EMPTY task extId on the action POST.
type noTaskRefMock struct {
	vmUUID string
}

func (m *noTaskRefMock) handler() http.HandlerFunc {
	vmPath := "/api/vmm/v4.0/ahv/config/vms/" + m.vmUUID
	return func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		switch {
		case p == vmmV4Root && r.Method == http.MethodGet:
			w.WriteHeader(http.StatusOK)
		case strings.Contains(p, "/$actions/"):
			// 202 Accepted (async) but NO task reference — the bug NUTA-04 guards.
			writePowerJSON(w, http.StatusAccepted, map[string]any{"data": map[string]any{"extId": ""}})
		case p == vmPath && r.Method == http.MethodGet:
			w.Header().Set("ETag", "etag-x")
			writePowerJSON(w, http.StatusOK, map[string]any{"data": map[string]any{"extId": m.vmUUID}})
		default:
			http.Error(w, "unexpected", http.StatusNotFound)
		}
	}
}

// TestPowerAction_V4_NoTaskRefOn202IsError (NUTA-04): an HTTP 202 (async) with
// an empty task extId is NOT success — there is no task to confirm the action
// ran. It must surface as "no task reference", never a silent success.
func TestPowerAction_V4_NoTaskRefOn202IsError(t *testing.T) {
	mock := &noTaskRefMock{vmUUID: "vm-1"}
	srv := httptest.NewServer(mock.handler())
	defer srv.Close()
	c := newTestClient(t, srv)

	err := c.PowerAction(context.Background(), "vm-1", "ON")
	if err == nil {
		t.Fatal("expected an error when a 202 carries no task reference")
	}
	if !strings.Contains(err.Error(), "no task reference") {
		t.Errorf("expected a 'no task reference' error, got %q", err.Error())
	}
}

// syncSuccessMock answers like a v4 PC but the action POST returns 200 OK with
// no body (synchronous success) — the only case where an empty extId is OK.
type syncSuccessMock struct {
	vmUUID  string
	taskHit int
}

func (m *syncSuccessMock) handler() http.HandlerFunc {
	vmPath := "/api/vmm/v4.0/ahv/config/vms/" + m.vmUUID
	return func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		switch {
		case p == vmmV4Root && r.Method == http.MethodGet:
			w.WriteHeader(http.StatusOK)
		case strings.HasPrefix(p, "/api/prism/v4.0/config/tasks/"):
			m.taskHit++
			writePowerJSON(w, http.StatusOK, map[string]any{"data": map[string]any{"status": "SUCCEEDED"}})
		case strings.Contains(p, "/$actions/"):
			// 200 OK, no body — a synchronous success. No task to poll.
			w.WriteHeader(http.StatusOK)
		case p == vmPath && r.Method == http.MethodGet:
			w.Header().Set("ETag", "etag-y")
			writePowerJSON(w, http.StatusOK, map[string]any{"data": map[string]any{"extId": m.vmUUID}})
		default:
			http.Error(w, "unexpected", http.StatusNotFound)
		}
	}
}

// TestPowerAction_V4_SyncSuccessShortCircuits (NUTA-04): a 200 OK with no task
// extId is the legitimate synchronous-success case — it short-circuits without
// polling any task and returns nil.
func TestPowerAction_V4_SyncSuccessShortCircuits(t *testing.T) {
	mock := &syncSuccessMock{vmUUID: "vm-1"}
	srv := httptest.NewServer(mock.handler())
	defer srv.Close()
	c := newTestClient(t, srv)

	if err := c.PowerAction(context.Background(), "vm-1", "ON"); err != nil {
		t.Fatalf("a 200 sync success must succeed, got %v", err)
	}
	if mock.taskHit != 0 {
		t.Errorf("sync success must not poll a task, got %d polls", mock.taskHit)
	}
}
