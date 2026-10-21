package plugins

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MarcoCarvin/ahv-remote-console/apps/runtime/internal/config"
	"github.com/MarcoCarvin/ahv-remote-console/apps/runtime/internal/netstate"
)

// OBS-01: Readiness reports the live plugin count and the last load error,
// distinct from liveness. White-box because LoadAll keys off the global sdk
// registry (which already carries the real ahv plugin in this test binary), so
// we drive the host's internal state directly to get deterministic states.
func TestReadiness_ReflectsLoadState(t *testing.T) {
	h := newTestHost(t)

	// Fresh host: no plugins, no error → not ready.
	if n, e := h.Readiness(); n != 0 || e != "" {
		t.Fatalf("fresh host: got (%d, %q), want (0, \"\")", n, e)
	}

	// Simulate a successful load of one plugin.
	h.mu.Lock()
	h.plugins["ahv"] = &fakePlugin{id: "ahv"}
	h.loadErr = nil
	h.mu.Unlock()
	if n, e := h.Readiness(); n != 1 || e != "" {
		t.Fatalf("after load: got (%d, %q), want (1, \"\")", n, e)
	}

	// Simulate a load error being recorded.
	h.mu.Lock()
	h.loadErr = errors.New("plugin ahv init: boom")
	h.mu.Unlock()
	_, e := h.Readiness()
	if !strings.Contains(e, "boom") {
		t.Fatalf("Readiness last error = %q, want it to contain the load error", e)
	}
}

// OBS-01: ActiveSessions tracks the host's live session count, the gauge the
// /metrics endpoint scrapes.
func TestActiveSessions(t *testing.T) {
	h := newTestHost(t)
	fp := &fakePlugin{id: "ahv"}
	if got := h.ActiveSessions(); got != 0 {
		t.Fatalf("fresh host active sessions = %d, want 0", got)
	}
	info, err := h.OpenSession(context.Background(), fp, "vm-1")
	if err != nil {
		t.Fatalf("OpenSession: %v", err)
	}
	if got := h.ActiveSessions(); got != 1 {
		t.Fatalf("after open active sessions = %d, want 1", got)
	}
	if err := h.CloseSession(context.Background(), info.ID); err != nil {
		t.Fatalf("CloseSession: %v", err)
	}
	if got := h.ActiveSessions(); got != 0 {
		t.Fatalf("after close active sessions = %d, want 0", got)
	}
}

// diagHost builds a host with a config manager + netstate so WriteDiagnostics
// (which reads both) works, plus the supplied log paths (DX-S-03).
func diagHost(t *testing.T, auditPath, runtimePath string) *Host {
	t.Helper()
	cfgMgr, err := config.Load(filepath.Join(t.TempDir(), "config.json"),
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	h, err := NewHost(Options{
		Logger:         slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError})),
		Config:         cfgMgr,
		NetState:       netstate.New(slog.New(slog.NewTextHandler(io.Discard, nil))),
		AuditLogPath:   auditPath,
		RuntimeLogPath: runtimePath,
	})
	if err != nil {
		t.Fatalf("NewHost: %v", err)
	}
	t.Cleanup(func() { h.cancelBase(); <-h.reaperDone })
	return h
}

// unzip returns the named entry's bytes from a zip archive, or fails the test.
func unzipEntry(t *testing.T, archive []byte, name string) ([]byte, bool) {
	t.Helper()
	zr, err := zip.NewReader(bytes.NewReader(archive), int64(len(archive)))
	if err != nil {
		t.Fatalf("zip.NewReader: %v", err)
	}
	for _, f := range zr.File {
		if f.Name == name {
			rc, err := f.Open()
			if err != nil {
				t.Fatalf("open %s: %v", name, err)
			}
			defer func() { _ = rc.Close() }()
			data, err := io.ReadAll(rc)
			if err != nil {
				t.Fatalf("read %s: %v", name, err)
			}
			return data, true
		}
	}
	return nil, false
}

// DX-S-03: the diagnostics bundle must tail-include the runtime-owned audit log
// so the bundle isn't logless. Fail-before/pass-after: before the fix the
// bundle held only runtime-state.json + config.json.
func TestWriteDiagnostics_IncludesAuditLog(t *testing.T) {
	dir := t.TempDir()
	auditPath := filepath.Join(dir, "audit.log")
	runtimePath := filepath.Join(dir, "runtime.log")
	const auditLine = `{"seq":1,"action":"session.open","session_id":"s-77"}`
	if err := os.WriteFile(auditPath, []byte(auditLine+"\n"), 0o600); err != nil {
		t.Fatalf("seed audit log: %v", err)
	}
	if err := os.WriteFile(runtimePath, []byte("level=INFO msg=\"runtime up\"\n"), 0o600); err != nil {
		t.Fatalf("seed runtime log: %v", err)
	}

	h := diagHost(t, auditPath, runtimePath)
	var buf bytes.Buffer
	if err := h.WriteDiagnostics(&buf); err != nil {
		t.Fatalf("WriteDiagnostics: %v", err)
	}

	auditEntry, ok := unzipEntry(t, buf.Bytes(), "audit.log")
	if !ok {
		t.Fatal("diagnostics bundle is missing audit.log")
	}
	if !strings.Contains(string(auditEntry), "s-77") {
		t.Fatalf("audit.log entry in bundle did not include the seeded record, got: %s", auditEntry)
	}
	if _, ok := unzipEntry(t, buf.Bytes(), "runtime.log"); !ok {
		t.Fatal("diagnostics bundle is missing runtime.log")
	}
	// The pre-existing entries must still be present.
	if _, ok := unzipEntry(t, buf.Bytes(), "runtime-state.json"); !ok {
		t.Fatal("diagnostics bundle is missing runtime-state.json")
	}
	if _, ok := unzipEntry(t, buf.Bytes(), "config.json"); !ok {
		t.Fatal("diagnostics bundle is missing config.json")
	}
}

// DX-S-03: a missing/empty log path is skipped (logs to stderr / audit
// disabled) without failing the export — the rest of the bundle still emits.
func TestWriteDiagnostics_SkipsMissingLogs(t *testing.T) {
	h := diagHost(t, "", "") // no audit, no runtime log file
	var buf bytes.Buffer
	if err := h.WriteDiagnostics(&buf); err != nil {
		t.Fatalf("WriteDiagnostics with no log paths: %v", err)
	}
	if _, ok := unzipEntry(t, buf.Bytes(), "audit.log"); ok {
		t.Fatal("audit.log should be absent when no path is configured")
	}
	if _, ok := unzipEntry(t, buf.Bytes(), "runtime-state.json"); !ok {
		t.Fatal("runtime-state.json must still be present")
	}
}
