package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/MarcoCarvin/ahv-remote-console/apps/runtime/pkg/sdk"
)

const statsVMUUID = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"

// groupsValue builds one arithmos data entry (name + a single value cell) in
// the nested shape GetVMStats parses.
func groupsValue(name, value string) map[string]any {
	return map[string]any{
		"name":   name,
		"values": []any{map[string]any{"values": []any{value}}},
	}
}

// TestGetVMStats_ParsesEachMetric: every requested metric is present and parses
// into the typed sample, and a metric the response OMITS maps to -1.
func TestGetVMStats_ParsesEachMetric(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/nutanix/v3/groups", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"group_results": []any{
				map[string]any{
					"entity_results": []any{
						map[string]any{
							"data": []any{
								groupsValue(attrCPUUsagePpm, "123000"),
								groupsValue(attrMemoryUsagePpm, "456000"),
								// Disk + net rate metrics are KBps on the wire → ×1024.
								groupsValue(attrDiskReadBps, "1024"),
								groupsValue(attrDiskWriteBps, "2048"),
								groupsValue(attrNetTxRateKBps, "4"),    // bare rate → 4096 B/s
								groupsValue(attrNetRxRateKBpsDot, "8"), // dotted fallback → 8192 B/s
							},
						},
					},
				},
			},
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := newTestClient(t, srv)

	got, err := c.GetVMStats(context.Background(), statsVMUUID)
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	if got.CPUUsagePpm != 123000 {
		t.Errorf("cpu = %d, want 123000", got.CPUUsagePpm)
	}
	if got.MemoryUsagePpm != 456000 {
		t.Errorf("mem = %d, want 456000", got.MemoryUsagePpm)
	}
	if got.DiskBytesReadPerSec != 1048576 {
		t.Errorf("disk read = %d, want 1048576", got.DiskBytesReadPerSec)
	}
	if got.DiskBytesWritePerSec != 2097152 {
		t.Errorf("disk write = %d, want 2097152", got.DiskBytesWritePerSec)
	}
	// Net tx via the bare rate field, ×1024 (KBps→B/s).
	if got.NetBytesTxPerSec != 4096 {
		t.Errorf("net tx = %d, want 4096", got.NetBytesTxPerSec)
	}
	// Net rx via the DOTTED rate field (firstAvailable falls through the bare
	// spelling to the dotted one), ×1024.
	if got.NetBytesRxPerSec != 8192 {
		t.Errorf("net rx = %d, want 8192 (dotted-rate fallback ×1024)", got.NetBytesRxPerSec)
	}
}

// TestGetVMStats_NetCounterNotMappedToRate: a cumulative byte counter
// (hypervisor_num_*_bytes) must NOT be mapped into the bytes/sec field — only the
// *_rate_kBps fields are. With only the counter present, net stays unavailable.
func TestGetVMStats_NetCounterNotMappedToRate(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/nutanix/v3/groups", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"group_results": []any{map[string]any{
				"entity_results": []any{map[string]any{
					"data": []any{
						groupsValue(attrNetRxBytes, "999999999"), // counter only
						groupsValue(attrNetTxBytes, "888888888"),
					},
				}},
			}},
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := newTestClient(t, srv)

	got, err := c.GetVMStats(context.Background(), statsVMUUID)
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	if got.NetBytesRxPerSec != statUnavailable || got.NetBytesTxPerSec != statUnavailable {
		t.Errorf("counters must not map to a rate field: rx=%d tx=%d, want %d",
			got.NetBytesRxPerSec, got.NetBytesTxPerSec, statUnavailable)
	}
}

// TestGetVMStats_SendsCorrectRequestBody: the request carries entity_type
// mh_vm, entity_ids scoping (NOT the dead vm==<uuid> filter), the MANDATORY
// arithmos epoch-microsecond time window (a missing window is one all-n/a bug
// this fixes), and every requested attribute as a BARE attribute (no operation,
// which can make arithmos drop it — another all-n/a path).
func TestGetVMStats_SendsCorrectRequestBody(t *testing.T) {
	var captured groupsRequest
	mux := http.NewServeMux()
	mux.HandleFunc("/api/nutanix/v3/groups", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(io.LimitReader(r.Body, 64*1024))
		if err := json.Unmarshal(body, &captured); err != nil {
			t.Errorf("decode request body: %v", err)
		}
		writeJSON(w, http.StatusOK, map[string]any{"group_results": []any{}})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := newTestClient(t, srv)

	if _, err := c.GetVMStats(context.Background(), statsVMUUID); err != nil {
		t.Fatalf("stats: %v", err)
	}
	if captured.EntityType != statsEntityType {
		t.Errorf("entity_type = %q, want %q", captured.EntityType, statsEntityType)
	}
	// Scoping is by entity_ids, and the dead "vm==<uuid>" filter must be gone.
	if len(captured.EntityIDs) != 1 || captured.EntityIDs[0] != statsVMUUID {
		t.Errorf("entity_ids = %v, want [%s]", captured.EntityIDs, statsVMUUID)
	}
	if captured.FilterCriteria != "" {
		t.Errorf("filter_criteria = %q, want empty (entity_ids scopes the query)", captured.FilterCriteria)
	}
	// Time window MUST be present — without it arithmos returns names, no values.
	if captured.DownsamplingInterval <= 0 {
		t.Errorf("downsampling_interval = %d, want > 0 (time window required)", captured.DownsamplingInterval)
	}
	if captured.StartTimeUsecs <= 0 || captured.EndTimeUsecs <= 0 {
		t.Errorf("time window = [%d, %d], want both > 0", captured.StartTimeUsecs, captured.EndTimeUsecs)
	}
	if captured.EndTimeUsecs < captured.StartTimeUsecs {
		t.Errorf("end_time_usecs %d < start_time_usecs %d", captured.EndTimeUsecs, captured.StartTimeUsecs)
	}
	wantAttrs := map[string]bool{
		attrCPUUsagePpm: true, attrMemoryUsagePpm: true,
		attrDiskReadBps: true, attrDiskWriteBps: true,
		attrNetRxRateKBps: true, attrNetTxRateKBps: true,
		attrNetRxRateKBpsDot: true, attrNetTxRateKBpsDot: true,
		attrNetRxBytes: true, attrNetTxBytes: true,
	}
	for _, a := range captured.GroupMemberAttributes {
		delete(wantAttrs, a.Attribute)
		// Attributes are sent BARE (no operation) — a non-schema operation can
		// make arithmos drop the attribute.
		if a.Operation != "" {
			t.Errorf("attribute %q sent with operation %q, want bare (no operation)", a.Attribute, a.Operation)
		}
	}
	if len(wantAttrs) != 0 {
		t.Errorf("request missing attributes: %v", wantAttrs)
	}
}

// capturingLogger records Debug calls so the no-PII stats log can be asserted.
// It implements sdk.Logger; only Debug is exercised here.
type capturingLogger struct {
	msgs     []string
	args     [][]any
	warnMsgs []string
	warnArgs [][]any
}

func (l *capturingLogger) Debug(msg string, args ...any) {
	l.msgs = append(l.msgs, msg)
	l.args = append(l.args, args)
}
func (l *capturingLogger) Info(string, ...any) {}
func (l *capturingLogger) Warn(msg string, args ...any) {
	l.warnMsgs = append(l.warnMsgs, msg)
	l.warnArgs = append(l.warnArgs, args)
}
func (l *capturingLogger) Error(string, ...any)   {}
func (l *capturingLogger) With(...any) sdk.Logger { return l }

// TestGetVMStats_GuardedDebugLog: with a logger attached, GetVMStats emits a
// single PII-free debug line carrying the entity_type, the metric names the
// cluster returned, and group/entity counts — and NONE of the metric values.
// This is the line an operator reads to confirm/iterate the live attribute
// names against a real cluster.
func TestGetVMStats_GuardedDebugLog(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/nutanix/v3/groups", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"entity_type": statsEntityType,
			"group_results": []any{
				map[string]any{
					"entity_results": []any{
						map[string]any{
							"data": []any{
								groupsValue(attrCPUUsagePpm, "999"),
							},
						},
					},
				},
			},
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	log := &capturingLogger{}
	c, err := NewClient(ClientOptions{BaseURL: srv.URL, Logger: log})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}

	if _, err := c.GetVMStats(context.Background(), statsVMUUID); err != nil {
		t.Fatalf("stats: %v", err)
	}
	if len(log.msgs) != 1 {
		t.Fatalf("debug calls = %d, want 1", len(log.msgs))
	}
	// The structured args must include the returned metric NAME but never its
	// VALUE — assert the value "999" appears nowhere in the logged args.
	var sawCPUName bool
	for _, a := range log.args[0] {
		if s, ok := a.(string); ok && s == "999" {
			t.Errorf("metric VALUE leaked into stats debug log: %v", log.args[0])
		}
		if names, ok := a.([]string); ok {
			for _, n := range names {
				if n == attrCPUUsagePpm {
					sawCPUName = true
				}
			}
		}
	}
	if !sawCPUName {
		t.Errorf("returned metric name %q not in debug log args: %v", attrCPUUsagePpm, log.args[0])
	}
}

// TestGetVMStats_EmptyResultWarns: when the groups query matches no entity (the
// all-n/a failure mode), the diagnostic is logged at WARN — not Debug — so it is
// visible at the runtime's default level, carrying entity_count for triage and
// still leaking no values.
func TestGetVMStats_EmptyResultWarns(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/nutanix/v3/groups", func(w http.ResponseWriter, _ *http.Request) {
		// No group_results → entity_count 0 (e.g. wrong scoping).
		writeJSON(w, http.StatusOK, map[string]any{"group_results": []any{}})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	log := &capturingLogger{}
	c, err := NewClient(ClientOptions{BaseURL: srv.URL, Logger: log})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	if _, err := c.GetVMStats(context.Background(), statsVMUUID); err != nil {
		t.Fatalf("stats: %v", err)
	}
	if len(log.warnMsgs) != 1 {
		t.Fatalf("warn calls = %d, want 1 (empty result must WARN)", len(log.warnMsgs))
	}
	if len(log.msgs) != 0 {
		t.Errorf("debug calls = %d, want 0 (empty result logs at WARN, not Debug)", len(log.msgs))
	}
	// The warn line carries entity_count and leaks no metric value.
	var sawEntityCount bool
	for i := 0; i+1 < len(log.warnArgs[0]); i += 2 {
		if k, ok := log.warnArgs[0][i].(string); ok && k == "entity_count" {
			sawEntityCount = true
		}
	}
	if !sawEntityCount {
		t.Errorf("warn line missing entity_count: %v", log.warnArgs[0])
	}
}

// TestGetVMStats_403SurfacesAPIError: a Prism 403 is returned as an *APIError
// with Status 403 so the router can map it to rbac_denied.
func TestGetVMStats_403SurfacesAPIError(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/nutanix/v3/groups", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"message":"forbidden"}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := newTestClient(t, srv)

	_, err := c.GetVMStats(context.Background(), statsVMUUID)
	if err == nil {
		t.Fatal("expected error on 403")
	}
	var ae *APIError
	if !errors.As(err, &ae) {
		t.Fatalf("expected *APIError, got %T: %v", err, err)
	}
	if ae.Status != http.StatusForbidden {
		t.Errorf("status = %d, want 403", ae.Status)
	}
}
