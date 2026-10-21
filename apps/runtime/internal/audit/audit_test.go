package audit

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newTestLogger(t *testing.T) (*Logger, string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "audit.log")
	l, err := Open(path, true, slog.New(slog.NewJSONHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = l.Close() })
	return l, path
}

func TestAppend_writesJSONLAndHashChainSurvivesRestart(t *testing.T) {
	l, path := newTestLogger(t)
	l.Append(ActionSessionOpen, Record{PluginID: "ahv", SessionID: "s-1"})
	l.Append(ActionSessionClose, Record{SessionID: "s-1"})
	_ = l.Close()

	// Re-open: seq counter should resume from 2.
	l2, err := Open(path, true, slog.New(slog.NewJSONHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("re-Open: %v", err)
	}
	l2.Append(ActionRuntimeStop, Record{})
	_ = l2.Close()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("expected 3 lines, got %d: %s", len(lines), string(data))
	}
	if !strings.Contains(lines[0], `"seq":1`) {
		t.Errorf("line 0 has wrong seq: %s", lines[0])
	}
	if !strings.Contains(lines[1], `"seq":2`) {
		t.Errorf("line 1 has wrong seq: %s", lines[1])
	}
	if !strings.Contains(lines[2], `"seq":3`) {
		t.Errorf("line 2 has wrong seq: %s", lines[2])
	}

	bad, err := VerifyChain(path)
	if err != nil {
		t.Fatalf("VerifyChain: %v (bad seq %d)", err, bad)
	}
	if bad != 0 {
		t.Errorf("expected intact chain, got bad seq %d", bad)
	}
}

func TestVerifyChain_detectsTamper(t *testing.T) {
	l, path := newTestLogger(t)
	l.Append(ActionAuthStart, Record{PluginID: "ahv"})
	l.Append(ActionAuthComplete, Record{PluginID: "ahv"})
	l.Append(ActionSessionOpen, Record{SessionID: "s-1"})
	_ = l.Close()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	// Flip a byte in the middle record's payload (not the seq, to keep
	// JSON valid). Replace "ahv" with "evil" in the second line — same
	// JSON shape, different content, so prev_hash of the third line
	// should mismatch.
	tampered := strings.Replace(string(data), `"action":"auth.complete","plugin_id":"ahv"`, `"action":"auth.complete","plugin_id":"evl"`, 1)
	if tampered == string(data) {
		t.Fatalf("test setup: substitution didn't match")
	}
	if err := os.WriteFile(path, []byte(tampered), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	bad, err := VerifyChain(path)
	if err == nil {
		t.Fatalf("expected tamper detection, got nil error")
	}
	if bad != 3 {
		t.Errorf("expected break at seq 3, got %d", bad)
	}
}

func TestDisabled_isNoop(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "audit.log")
	l, err := Open(path, false, slog.New(slog.NewJSONHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	l.Append(ActionSessionOpen, Record{PluginID: "ahv"})
	_ = l.Close()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("expected no audit file when disabled, got err=%v", err)
	}
}

// OBS-03: security-critical actions fsync on every write; high-frequency
// low-stakes actions skip the hot-path fsync; unknown actions default to fsync.
func TestFsyncImmediately(t *testing.T) {
	for _, a := range []Action{
		ActionAuthComplete, ActionSessionOpen, ActionEndpointDelete,
		ActionUpdaterApply, ActionRuntimeStart, ActionBridgeInstall, ActionBridgeProbeReject,
		Action("some.new.action"),
	} {
		if !fsyncImmediately(a) {
			t.Errorf("action %q should fsync immediately", a)
		}
	}
	for _, a := range []Action{
		ActionClipboardHostToVM, ActionClipboardVMToHost, ActionBridgeClipIn, ActionBridgeClipOut,
		ActionPresenceJoin, ActionPresenceRoleChange, ActionSessionStreamOpen,
	} {
		if fsyncImmediately(a) {
			t.Errorf("high-frequency action %q must NOT fsync on the hot path", a)
		}
	}
}

// OBS-03: deferring fsync for low-stakes events must not affect correctness —
// records are still written and the hash chain stays intact, and Close flushes
// the trailing deferred record.
func TestAppend_DeferredLowStakesStillChains(t *testing.T) {
	l, path := newTestLogger(t)
	l.Append(ActionClipboardHostToVM, Record{SessionID: "s-1"}) // deferred fsync
	l.Append(ActionAuthComplete, Record{EndpointID: "e-1"})     // critical fsync flushes both
	l.Append(ActionPresenceJoin, Record{SessionID: "s-1"})      // deferred (flushed by Close)
	if err := l.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if bad, err := VerifyChain(path); err != nil {
		t.Fatalf("VerifyChain broke at seq %d: %v", bad, err)
	}
	data, _ := os.ReadFile(path)
	if !strings.Contains(string(data), `"clipboard.host_to_vm"`) {
		t.Error("deferred clipboard record was not persisted")
	}
	if !strings.Contains(string(data), `"presence.join"`) {
		t.Error("trailing deferred presence record was not flushed by Close")
	}
}
