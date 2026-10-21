// Package audit implements an append-only audit log for AHV Remote Console.
//
// Records track every security-relevant operation: session opens and
// closes, clipboard transfers (lengths only, never contents), credential
// entry and removal, endpoint CRUD, autoUpdater runs, plugin load
// failures. Each line is a JSON object containing a SHA-256 hash chain
// linking it to its predecessor so a tamper at line N invalidates every
// line from N+1 onward.
//
// File layout
//
//	<dataDir>/audit.log     mode 0600, O_APPEND
//
// Each line is one JSON object terminated by '\n':
//
//	{"seq":N,"ts":"2026-05-21T10:00:00Z","actor":"alice",
//	 "action":"session.open","endpoint_id":"prism-1",
//	 "session_id":"...","vm_id":"...","result":"ok","prev_hash":"...",
//	 "extra":{}}
//
// prev_hash is `sha256(prev_line_bytes_without_trailing_newline)` hex.
// The first record has prev_hash = sha256(""). A tampered audit.log can
// be detected with a single linear scan that re-derives prev_hash for
// each record.
//
// Why JSONL + hashchain instead of a binary append-only format:
//
//   - JSONL is human-readable, ships through `grep`, and survives
//     mid-process crashes (each line is atomic relative to O_APPEND).
//   - The hashchain catches in-place tampering by anyone with write
//     access to the file. It does NOT defeat an attacker who can append
//     a new chain from scratch — that requires a key, which is out of
//     scope for v1.
//
// The package is goroutine-safe. All file writes go through a single
// mutex so seq numbers and the hash chain stay consistent.
package audit

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/user"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/MarcoCarvin/ahv-remote-console/apps/runtime/internal/logging"
)

// Action is a stable identifier for the kind of event being recorded.
// New values are added by extending the const block — existing values
// never get renamed so log analytics keep working across versions.
type Action string

const (
	ActionRuntimeStart       Action = "runtime.start"
	ActionRuntimeStop        Action = "runtime.stop"
	ActionSessionOpen        Action = "session.open"
	ActionSessionClose       Action = "session.close"
	ActionSessionStreamOpen  Action = "session.stream.open"
	ActionSessionStreamClose Action = "session.stream.close"
	ActionAuthStart          Action = "auth.start"
	ActionAuthComplete       Action = "auth.complete"
	ActionAuthRefresh        Action = "auth.refresh"
	ActionAuthInvalidate     Action = "auth.invalidate"
	ActionLogout             Action = "auth.logout"
	ActionEndpointUpsert     Action = "endpoint.upsert"
	ActionEndpointDelete     Action = "endpoint.delete"
	ActionClipboardHostToVM  Action = "clipboard.host_to_vm"
	ActionClipboardVMToHost  Action = "clipboard.vm_to_host"
	ActionDiagnosticsExport  Action = "diagnostics.export"
	ActionPluginLoadFailure  Action = "plugin.load_failure"
	ActionUpdaterApply       Action = "updater.apply"

	// Bridge daemon (hcbridge.exe) lifecycle actions. Emitted by the
	// renderer via the /v1/agent/audit IPC endpoint — the bridge
	// daemon itself doesn't talk to the runtime directly; it speaks
	// in-band through the VNC session, and the renderer (which holds
	// the session context) records the audit events on its behalf.
	ActionBridgeInstall   Action = "bridge.install"   // first-time install of hcbridge.exe in the guest
	ActionBridgeUninstall Action = "bridge.uninstall" // operator removed the bridge from the guest

	// ActionBridgeConnect / ActionBridgeDisconnect mark the bridge
	// handshake completing or tearing down on a particular session.
	ActionBridgeConnect    Action = "bridge.connect"
	ActionBridgeDisconnect Action = "bridge.disconnect"

	// ActionBridgeClipIn (guest -> host) and ActionBridgeClipOut
	// (host -> guest) record clipboard transfers crossing the bridge.
	//
	// COMPLIANCE INVARIANT: these records carry ONLY
	//
	//   - direction  ("host_to_vm" | "vm_to_host"),
	//   - bytes      (length of the payload in bytes),
	//   - sha256     (hex of the SHA-256 of the payload, for dedupe
	//                 correlation across audit consumers).
	//
	// They MUST NEVER carry clipboard plaintext. This is the load-bearing constraint here:
	// the audit log is reviewed by ops/security teams and may be
	// forwarded to SIEMs; any plaintext leak through this channel
	// would defeat the whole point of the bridge being an "operator
	// productivity feature with audit-trail oversight" rather than a
	// "covert data exfiltration vector".
	ActionBridgeClipIn  Action = "bridge.clip_in"  // guest -> host clipboard transfer (length only)
	ActionBridgeClipOut Action = "bridge.clip_out" // host -> guest clipboard transfer (length only)

	// ActionBridgeProbeReject fires when a probe envelope fails
	// signature/AEAD verification. Indicates either an unsigned
	// downgrade attempt or a wrong-session collision; in either case
	// the bridge refused the operation and ops should investigate.
	ActionBridgeProbeReject Action = "bridge.probe_reject"

	ActionPresenceJoin       Action = "presence.join"
	ActionPresenceLeave      Action = "presence.leave"
	ActionPresenceRoleChange Action = "presence.role_change"

	// VM snapshot lifecycle. ActionVMSnapshotRestore is DESTRUCTIVE — the
	// restore reverts the VM and is gated behind a mandatory operator
	// confirmation; the durable audit entry records WHO reverted WHAT, so it is
	// written for every restore attempt. These records carry only snapshot/VM
	// UUIDs and names (non-sensitive identifiers) — never clipboard or
	// credential content.
	ActionVMSnapshotCreate  Action = "vm.snapshot.create"
	ActionVMSnapshotRestore Action = "vm.snapshot.restore"
)

// Record is one entry in the audit log. Fields that don't apply to a
// particular action are emitted as the JSON zero value (empty string,
// empty map) — never `null`, so downstream consumers don't have to
// handle option types.
type Record struct {
	Seq        uint64         `json:"seq"`
	Timestamp  time.Time      `json:"ts"`
	Actor      string         `json:"actor"`
	Action     Action         `json:"action"`
	EndpointID string         `json:"endpoint_id,omitempty"`
	SessionID  string         `json:"session_id,omitempty"`
	VMID       string         `json:"vm_id,omitempty"`
	PluginID   string         `json:"plugin_id,omitempty"`
	Result     string         `json:"result"` // "ok" | "error" | "denied"
	Reason     string         `json:"reason,omitempty"`
	Extra      map[string]any `json:"extra,omitempty"`
	PrevHash   string         `json:"prev_hash"`
}

// Logger is the append-only audit log. Construct via Open; close via
// Close on process shutdown. Safe for concurrent use.
type Logger struct {
	mu       sync.Mutex
	f        *os.File
	enabled  bool
	actor    string // OS username, cached at Open time
	seq      atomic.Uint64
	prevHash string
	slog     *slog.Logger
}

// Open creates or appends to the audit log at `path`. If `enabled` is
// false, the returned Logger is a no-op (every Append returns nil
// without writing). This honors the SecuritySettings.AuditTrailEnabled
// config flag while keeping call sites at the emitters short and
// always-on.
func Open(path string, enabled bool, slogger *slog.Logger) (*Logger, error) {
	l := &Logger{enabled: enabled, slog: slogger}
	if !enabled {
		slogger.Info("audit log disabled by config", "path", path)
		return l, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("audit: mkdir: %w", err)
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, fmt.Errorf("audit: open: %w", err)
	}
	// Ensure mode is 0600 even if the file pre-existed with looser perms.
	if err := os.Chmod(path, 0o600); err != nil {
		slogger.Warn("audit: chmod failed (continuing)", "err", err)
	}
	l.f = f
	if u, err := user.Current(); err == nil {
		l.actor = u.Username
	}
	// Reconstruct the seq counter and prev_hash by tailing the existing
	// file. Cheap on small files (typical) and bounded for ops who
	// rotate the log.
	if err := l.recover(path); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("audit: recover: %w", err)
	}
	slogger.Info("audit log opened", "path", path, "starting_seq", l.seq.Load()+1)
	return l, nil
}

// recover reads the existing file (if any) to seed the seq counter and
// the prev_hash. Reading the entire file is fine — audit logs grow
// linearly with operator activity and rotation is the right cure for
// size, not in-process truncation.
func (l *Logger) recover(path string) error {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) || len(data) == 0 {
		l.prevHash = hashHex(nil)
		return nil
	}
	if err != nil {
		return err
	}
	// Find the last non-empty line.
	lastLineStart := -1
	for i := len(data) - 1; i >= 0; i-- {
		if data[i] == '\n' {
			if i == len(data)-1 {
				continue // trailing newline
			}
			lastLineStart = i + 1
			break
		}
	}
	if lastLineStart < 0 {
		lastLineStart = 0
	}
	lastLine := data[lastLineStart:]
	if n := len(lastLine); n > 0 && lastLine[n-1] == '\n' {
		lastLine = lastLine[:n-1]
	}
	if len(lastLine) == 0 {
		l.prevHash = hashHex(nil)
		return nil
	}
	var rec Record
	if err := json.Unmarshal(lastLine, &rec); err != nil {
		return fmt.Errorf("audit: tail unparseable, refusing to continue: %w", err)
	}
	l.seq.Store(rec.Seq)
	l.prevHash = hashHex(lastLine)
	return nil
}

// Append writes one record. The seq, ts, actor, and prev_hash are
// filled in by the logger; the caller provides everything else.
// Returns nil immediately if the logger is disabled. Errors are
// logged via the slog instance — they do NOT bubble up to the
// emitter, because audit-log failure should not break the user
// operation it was recording. Operators who care about audit
// completeness should monitor the slog for "audit append failed".
func (l *Logger) Append(action Action, fields Record) {
	if !l.enabled {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()

	fields.Seq = l.seq.Add(1)
	fields.Timestamp = time.Now().UTC()
	if fields.Actor == "" {
		fields.Actor = l.actor
	}
	fields.Action = action
	fields.PrevHash = l.prevHash
	if fields.Result == "" {
		fields.Result = "ok"
	}
	// Reason is free-form and frequently set from err.Error(), which can wrap an
	// upstream response or header value. Scrub secret-looking content (cookies,
	// auth headers) before it lands in the on-disk hash chain — the same
	// defense-in-depth the slog handler applies. (The primary cred-reflection
	// vector, APIError.Error()'s body, is already removed at the source.)
	if fields.Reason != "" {
		fields.Reason = logging.Scrub(fields.Reason)
	}

	line, err := json.Marshal(&fields)
	if err != nil {
		l.slog.Error("audit marshal failed", "action", action, "err", err)
		return
	}
	if _, err := l.f.Write(append(line, '\n')); err != nil {
		l.slog.Error("audit append failed", "action", action, "err", err)
		return
	}
	// Durability: fsync immediately for security-critical events so a kernel
	// panic / OS shutdown can't lose them. High-frequency low-stakes events
	// (clipboard, presence, stream churn) skip the per-event fsync — they are
	// still written to the file and made durable by the next critical event's
	// fsync (which flushes ALL buffered writes) or by Close(). Losing the last
	// few low-stakes records across a hard crash is acceptable; an fsync on the
	// hot path is not (audit OBS-03).
	if fsyncImmediately(action) {
		if err := l.f.Sync(); err != nil {
			l.slog.Warn("audit fsync failed", "err", err)
		}
	}
	l.prevHash = hashHex(line)
}

// fsyncImmediately reports whether an action is durability-critical enough to
// fsync on every write. High-frequency, low-stakes events skip it (see Append).
// The default is true so any new/unknown action is fsynced until deliberately
// classified as hot-path.
func fsyncImmediately(a Action) bool {
	switch a {
	case ActionClipboardHostToVM, ActionClipboardVMToHost,
		ActionBridgeClipIn, ActionBridgeClipOut,
		ActionBridgeConnect, ActionBridgeDisconnect,
		ActionSessionStreamOpen, ActionSessionStreamClose,
		ActionPresenceJoin, ActionPresenceLeave, ActionPresenceRoleChange:
		return false
	default:
		return true
	}
}

// Close flushes and closes the file. Safe to call on a disabled
// logger.
func (l *Logger) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f == nil {
		return nil
	}
	// Flush any deferred low-stakes records to disk before closing.
	if err := l.f.Sync(); err != nil {
		l.slog.Warn("audit fsync on close failed", "err", err)
	}
	err := l.f.Close()
	l.f = nil
	return err
}

// VerifyChain re-derives prev_hash for every record in the file and
// returns the first seq number where the chain breaks (or 0 if intact).
// Operations teams call this from a CLI subcommand to confirm
// tamper-evidence on demand.
func VerifyChain(path string) (badSeq uint64, err error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	data, err := io.ReadAll(f)
	if err != nil {
		return 0, err
	}
	expected := hashHex(nil)
	start := 0
	for i := 0; i < len(data); i++ {
		if data[i] != '\n' {
			continue
		}
		line := data[start:i]
		start = i + 1
		if len(line) == 0 {
			continue
		}
		var rec Record
		if err := json.Unmarshal(line, &rec); err != nil {
			return rec.Seq, fmt.Errorf("audit: malformed record: %w", err)
		}
		if rec.PrevHash != expected {
			return rec.Seq, errors.New("audit: prev_hash mismatch")
		}
		expected = hashHex(line)
	}
	return 0, nil
}

func hashHex(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}
