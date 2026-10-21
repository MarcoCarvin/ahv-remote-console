package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// consoleTokenMock simulates the v4 generate-console-token flow:
//
//	GET  /api/vmm/v4.0/ahv/config/vms/{extId}                       -> 200 + ETag
//	POST /api/vmm/v4.0/ahv/config/vms/{extId}/$actions/generate-console-token
//	                                                                -> 202 + task ref
//	GET  /api/prism/v4.0/config/tasks/{extId}                       -> 200 + token in completionDetails
//
// NEEDS-LIVE-VALIDATION (AHV-04): mirrors the documented contract; the real wire
// shapes must be confirmed against a live Prism Central before GA.
type consoleTokenMock struct {
	vmUUID     string
	token      string // value returned in the completed task
	taskStatus string // default SUCCEEDED
	noExtID    bool   // when true, the 202 action carries no task extId
	emptyToken bool   // when true, a SUCCEEDED task carries no token value

	etagHit   int
	actionHit int
	taskHit   int
	ifMatch   string
}

func (m *consoleTokenMock) handler() http.HandlerFunc {
	vmPath := "/api/vmm/v4.0/ahv/config/vms/" + m.vmUUID
	return func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		switch {
		case strings.HasPrefix(p, "/api/prism/v4.0/config/tasks/"):
			m.taskHit++
			st := m.taskStatus
			if st == "" {
				st = "SUCCEEDED"
			}
			details := []map[string]any{}
			if !m.emptyToken {
				details = append(details, map[string]any{"name": "vmConsoleToken", "value": m.token})
			}
			writePowerJSON(w, http.StatusOK, map[string]any{
				"data": map[string]any{"status": st, "completionDetails": details},
			})
		case strings.HasSuffix(p, "/$actions/generate-console-token"):
			m.actionHit++
			m.ifMatch = r.Header.Get("If-Match")
			ext := "console-task-1"
			if m.noExtID {
				ext = ""
			}
			writePowerJSON(w, http.StatusAccepted, map[string]any{"data": map[string]any{"extId": ext}})
		case p == vmPath && r.Method == http.MethodGet:
			m.etagHit++
			w.Header().Set("ETag", "etag-ct")
			writePowerJSON(w, http.StatusOK, map[string]any{"data": map[string]any{"extId": m.vmUUID}})
		default:
			http.Error(w, "unexpected "+r.Method+" "+p, http.StatusNotFound)
		}
	}
}

// TestAcquireConsoleTokenV4_HappyPath (AHV-04): the v4 token flow GETs the ETag,
// POSTs generate-console-token with If-Match, polls the task, and returns the
// minted JWT from the task's completion details.
func TestAcquireConsoleTokenV4_HappyPath(t *testing.T) {
	mock := &consoleTokenMock{vmUUID: "vm-9", token: "jwt-abc.def.ghi"}
	srv := httptest.NewServer(mock.handler())
	defer srv.Close()
	c := newTestClient(t, srv)

	tok, err := c.AcquireConsoleTokenV4(context.Background(), "vm-9")
	if err != nil {
		t.Fatalf("AcquireConsoleTokenV4: %v", err)
	}
	if tok != "jwt-abc.def.ghi" {
		t.Errorf("token = %q, want the minted JWT", tok)
	}
	if mock.etagHit != 1 {
		t.Errorf("expected one ETag GET, got %d", mock.etagHit)
	}
	if mock.ifMatch != "etag-ct" {
		t.Errorf("If-Match = %q, want the GET ETag", mock.ifMatch)
	}
	if mock.taskHit < 1 {
		t.Errorf("expected the token task to be polled, got %d", mock.taskHit)
	}
}

// TestAcquireConsoleTokenV4_NoTaskRef (AHV-04 + NUTA-04 parity): a 202 with no
// task extId is an error, never a silent empty-token success.
func TestAcquireConsoleTokenV4_NoTaskRef(t *testing.T) {
	mock := &consoleTokenMock{vmUUID: "vm-9", noExtID: true}
	srv := httptest.NewServer(mock.handler())
	defer srv.Close()
	c := newTestClient(t, srv)

	if _, err := c.AcquireConsoleTokenV4(context.Background(), "vm-9"); err == nil {
		t.Fatal("expected an error when the action returns no task reference")
	}
}

// TestAcquireConsoleTokenV4_TaskFailed (AHV-04): a FAILED token task surfaces an
// error.
func TestAcquireConsoleTokenV4_TaskFailed(t *testing.T) {
	mock := &consoleTokenMock{vmUUID: "vm-9", taskStatus: "FAILED"}
	srv := httptest.NewServer(mock.handler())
	defer srv.Close()
	c := newTestClient(t, srv)

	if _, err := c.AcquireConsoleTokenV4(context.Background(), "vm-9"); err == nil {
		t.Fatal("expected an error when the token task ends FAILED")
	}
}

// TestAcquireConsoleTokenV4_SucceededButNoToken (AHV-04): a SUCCEEDED task that
// somehow carries no token value is an error, not an empty token.
func TestAcquireConsoleTokenV4_SucceededButNoToken(t *testing.T) {
	mock := &consoleTokenMock{vmUUID: "vm-9", emptyToken: true}
	srv := httptest.NewServer(mock.handler())
	defer srv.Close()
	c := newTestClient(t, srv)

	if _, err := c.AcquireConsoleTokenV4(context.Background(), "vm-9"); err == nil {
		t.Fatal("expected an error when a succeeded task carries no token")
	}
}

// TestConsoleWebSocketURLV4 (AHV-04): the v4 launch URL embeds the escaped VM
// UUID in the path and the token in the VmConsoleToken query param.
func TestConsoleWebSocketURLV4(t *testing.T) {
	c, err := NewClient(ClientOptions{BaseURL: "https://pc.example:9440"})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	got := c.ConsoleWebSocketURLV4("vm 9", "jwt+with/special=chars")
	if !strings.HasPrefix(got, "wss://pc.example:9440/console/launch/") {
		t.Errorf("unexpected base/path: %q", got)
	}
	if !strings.Contains(got, "VmConsoleToken=jwt%2Bwith%2Fspecial%3Dchars") {
		t.Errorf("token not URL-encoded into query: %q", got)
	}
	// The path segment must be escaped (no raw space).
	if strings.Contains(got, "launch/vm 9") {
		t.Errorf("VM UUID path segment not escaped: %q", got)
	}
}

// TestConsoleWebSocketURL_V3StillDefault: the v3 cookie-only dial URL is
// unchanged — the v4 path is opt-in and must not regress the working default.
func TestConsoleWebSocketURL_V3StillDefault(t *testing.T) {
	c, err := NewClient(ClientOptions{BaseURL: "https://pc.example:9440"})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	got := c.ConsoleWebSocketURL("vm-1", "cluster-1")
	if got != "wss://pc.example:9440/vnc/vm/vm-1/proxy?proxyClusterUuid=cluster-1" {
		t.Errorf("v3 default WS URL changed: %q", got)
	}
}
