package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestPollTaskOnce_V3 pins the single-shot v3 task poll the registry refresh and
// the waiter both delegate to: SUCCEEDED -> succeeded, FAILED -> failed (with
// the v3 error_detail), and an in-flight task -> running with the percent from
// progress_info.percentage_complete.
func TestPollTaskOnce_V3(t *testing.T) {
	cases := []struct {
		name        string
		status      string
		errorDetail string
		percent     int
		wantState   string
		wantPercent int
		wantErrSub  string
	}{
		{"succeeded", "SUCCEEDED", "", 100, "succeeded", 100, ""},
		{"failed", "FAILED", "disk attach blew up", 0, "failed", 100, "disk attach blew up"},
		{"running", "RUNNING", "", 42, "running", 42, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if !strings.HasPrefix(r.URL.Path, "/api/nutanix/v3/tasks/") {
					http.Error(w, "unexpected "+r.URL.Path, http.StatusNotFound)
					return
				}
				data := map[string]any{
					"status":        tc.status,
					"error_detail":  tc.errorDetail,
					"progress_info": map[string]any{"percentage_complete": tc.percent},
				}
				writePowerJSON(w, http.StatusOK, data)
			}))
			defer srv.Close()
			c := newTestClient(t, srv)

			snap, err := c.PollTaskOnce(context.Background(), "task-1", false)
			if err != nil {
				t.Fatalf("PollTaskOnce: %v", err)
			}
			if snap.State != tc.wantState {
				t.Errorf("state = %q, want %q", snap.State, tc.wantState)
			}
			if snap.PercentComplete != tc.wantPercent {
				t.Errorf("percent = %d, want %d", snap.PercentComplete, tc.wantPercent)
			}
			if tc.wantErrSub != "" && !strings.Contains(snap.Error, tc.wantErrSub) {
				t.Errorf("error = %q, want substring %q", snap.Error, tc.wantErrSub)
			}
		})
	}
}

// TestPollTaskOnce_V4 pins the v4 mapping: SUCCEEDED -> succeeded, and
// FAILED/ABORTED/CANCELED/CANCELLED -> failed carrying the v4 errorMessages
// detail. v4 reports no percent, so running stays at 0.
func TestPollTaskOnce_V4(t *testing.T) {
	cases := []struct {
		name       string
		status     string
		errMsg     string
		wantState  string
		wantErrSub string
	}{
		{"succeeded", "SUCCEEDED", "", "succeeded", ""},
		{"failed", "FAILED", "power op denied", "failed", "power op denied"},
		{"aborted", "ABORTED", "task aborted", "failed", "task aborted"},
		{"canceled", "CANCELED", "", "failed", ""},
		{"cancelled", "CANCELLED", "", "failed", ""},
		{"running", "RUNNING", "", "running", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if !strings.HasPrefix(r.URL.Path, "/api/prism/v4.0/config/tasks/") {
					http.Error(w, "unexpected "+r.URL.Path, http.StatusNotFound)
					return
				}
				data := map[string]any{"status": tc.status}
				if tc.errMsg != "" {
					data["errorMessages"] = []map[string]any{{"message": tc.errMsg}}
				}
				writePowerJSON(w, http.StatusOK, map[string]any{"data": data})
			}))
			defer srv.Close()
			c := newTestClient(t, srv)

			snap, err := c.PollTaskOnce(context.Background(), "task-extid", true)
			if err != nil {
				t.Fatalf("PollTaskOnce: %v", err)
			}
			if snap.State != tc.wantState {
				t.Errorf("state = %q, want %q", snap.State, tc.wantState)
			}
			if tc.wantErrSub != "" && !strings.Contains(snap.Error, tc.wantErrSub) {
				t.Errorf("error = %q, want substring %q", snap.Error, tc.wantErrSub)
			}
		})
	}
}

// TestPollTaskOnce_EmptyUUID: an empty task uuid short-circuits as a succeeded
// poll (the upstream op returned synchronously, nothing to wait on).
func TestPollTaskOnce_EmptyUUID(t *testing.T) {
	c := newTestClient(t, httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected request %s — empty uuid must not hit the network", r.URL.Path)
	})))
	snap, err := c.PollTaskOnce(context.Background(), "", true)
	if err != nil {
		t.Fatalf("PollTaskOnce: %v", err)
	}
	if snap.State != "succeeded" || snap.PercentComplete != 100 {
		t.Errorf("empty uuid snap = %+v, want succeeded/100", snap)
	}
}
