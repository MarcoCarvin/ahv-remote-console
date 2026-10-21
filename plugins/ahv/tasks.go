package ahv

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sort"
	"time"

	"github.com/MarcoCarvin/ahv-remote-console/apps/runtime/pkg/sdk"
	"github.com/MarcoCarvin/ahv-remote-console/plugins/ahv/internal/api"
)

// In-plugin task registry backing sdk.TaskCapable (the renderer's Tasks tray).
//
// It introduces NO new Prism RBAC surface: a tracked task records the upstream
// task UUID the existing power/serial/mount ops already obtain, and GetTask
// refreshes it via the read-only tasks GET those ops already poll. The registry
// is in-memory and per-runtime-process — tasks vanish on a runtime restart, the
// same lifecycle as sessions — and bounded by pruneTasksLocked.
//
// COMPLIANCE: every operator-facing string on a tracked task (Label, Detail,
// Error) is host-composed or sourced from the upstream task's own
// operator-facing failure detail. Clipboard plaintext and credentials NEVER
// land here.

// terminalTaskTTL is how long a finished (succeeded/failed) task lingers in the
// registry so the tray can show a just-completed result before it ages out.
const terminalTaskTTL = 10 * time.Minute

// trackedTask is one registry entry: the wire-facing TaskInfo plus the upstream
// routing the registry needs to refresh it.
type trackedTask struct {
	info          sdk.TaskInfo
	endpointID    string
	vmUUID        string
	prismTaskUUID string // upstream task handle once an op returns one
	isV4          bool   // which tasks GET PollTaskOnce should issue
}

// newTaskID mints a registry handle (crypto/rand hex, like other id minting in
// the repo — gosec-clean, not math/rand).
func newTaskID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand failure is catastrophic and effectively never happens;
		// fall back to a time-seeded id so the op can still be tracked.
		return fmt.Sprintf("task-%d", time.Now().UnixNano())
	}
	return "task-" + hex.EncodeToString(b[:])
}

// newTask registers a running task and returns its handle. kind is the task
// taxonomy ("vm_power" | "serial_port" | "agent_iso"); label is the operator-
// facing one-liner. The entry starts running/percent=0.
func (p *Plugin) newTask(kind, label, endpointID, vmUUID, sessionID string) string {
	id := newTaskID()
	now := time.Now().UTC()
	t := &trackedTask{
		endpointID: endpointID,
		vmUUID:     vmUUID,
		info: sdk.TaskInfo{
			ID:              id,
			PluginID:        pluginID,
			Kind:            kind,
			Label:           label,
			ResourceID:      endpointID + "/" + vmUUID,
			SessionID:       sessionID,
			State:           sdk.TaskRunning,
			PercentComplete: 0,
			StartedAt:       now,
			UpdatedAt:       now,
		},
	}
	p.tasksMu.Lock()
	if p.tasks == nil {
		p.tasks = make(map[string]*trackedTask)
	}
	p.pruneTasksLocked()
	p.tasks[id] = t
	p.tasksMu.Unlock()
	return id
}

// setTaskPrism records the upstream task uuid (and its API generation) once an
// op obtains it, so GetTask can refresh real progress mid-flight.
func (p *Plugin) setTaskPrism(id, prismUUID string, isV4 bool) {
	p.tasksMu.Lock()
	if t, ok := p.tasks[id]; ok {
		t.prismTaskUUID = prismUUID
		t.isV4 = isV4
		t.info.UpdatedAt = time.Now().UTC()
	}
	p.tasksMu.Unlock()
}

// updateTask mutates a tracked task's TaskInfo under lock and bumps UpdatedAt.
// Used at real phase boundaries (e.g. ISO uploaded -> percent 70).
func (p *Plugin) updateTask(id string, fn func(*sdk.TaskInfo)) {
	p.tasksMu.Lock()
	if t, ok := p.tasks[id]; ok {
		fn(&t.info)
		t.info.UpdatedAt = time.Now().UTC()
	}
	p.tasksMu.Unlock()
}

// finishTask marks a task terminal: succeeded (err == nil) or failed (err !=
// nil, Error = err.Error()). Percent jumps to 100 either way. err.Error() is the
// op's own operator-facing message — it never carries payload bytes.
func (p *Plugin) finishTask(id string, err error) {
	p.tasksMu.Lock()
	if t, ok := p.tasks[id]; ok {
		if err != nil {
			t.info.State = sdk.TaskFailed
			t.info.Error = err.Error()
		} else {
			t.info.State = sdk.TaskSucceeded
			t.info.Error = ""
		}
		t.info.PercentComplete = 100
		t.info.UpdatedAt = time.Now().UTC()
	}
	p.tasksMu.Unlock()
}

// pruneTasksLocked drops terminal tasks older than terminalTaskTTL so the
// registry stays bounded. Caller holds tasksMu.
func (p *Plugin) pruneTasksLocked() {
	cutoff := time.Now().Add(-terminalTaskTTL)
	for id, t := range p.tasks {
		if t.info.State != sdk.TaskRunning && t.info.UpdatedAt.Before(cutoff) {
			delete(p.tasks, id)
		}
	}
}

// ListTasks implements sdk.TaskCapable: a newest-first snapshot of tracked
// tasks. Read-only — it never refreshes upstream (the tray's getTask poll does
// that for the running subset).
func (p *Plugin) ListTasks(_ context.Context) ([]sdk.TaskInfo, error) {
	p.tasksMu.Lock()
	p.pruneTasksLocked()
	out := make([]sdk.TaskInfo, 0, len(p.tasks))
	for _, t := range p.tasks {
		out = append(out, t.info)
	}
	p.tasksMu.Unlock()
	sort.Slice(out, func(i, j int) bool {
		return out[i].StartedAt.After(out[j].StartedAt)
	})
	return out, nil
}

// GetTask implements sdk.TaskCapable: look up a handle and, if it is still
// running with a known upstream task uuid, refresh State/PercentComplete/Error
// from real Prism state via the read-only PollTaskOnce. Returns an error only
// when the handle is unknown to this plugin.
func (p *Plugin) GetTask(ctx context.Context, id string) (sdk.TaskInfo, error) {
	p.tasksMu.Lock()
	t, ok := p.tasks[id]
	if !ok {
		p.tasksMu.Unlock()
		return sdk.TaskInfo{}, fmt.Errorf("unknown task: %s", id)
	}
	// Snapshot the routing under the lock; do the network refresh unlocked.
	running := t.info.State == sdk.TaskRunning
	prismUUID := t.prismTaskUUID
	isV4 := t.isV4
	endpointID := t.endpointID
	if !running || prismUUID == "" {
		info := t.info
		p.tasksMu.Unlock()
		return info, nil
	}
	p.tasksMu.Unlock()

	ep, ok := p.endpoint(endpointID)
	if !ok {
		return p.taskSnapshot(id)
	}
	client, err := p.clientFor(ctx, ep)
	if err != nil {
		return p.taskSnapshot(id)
	}
	snap, err := client.PollTaskOnce(ctx, prismUUID, isV4)
	if err != nil {
		// A transient poll failure must not flip the task to failed — keep the
		// last-known state and let the next poll catch up.
		return p.taskSnapshot(id)
	}
	p.applySnapshot(id, snap)
	return p.taskSnapshot(id)
}

// applySnapshot folds a PollTaskOnce result into a tracked task. Only a running
// task is advanced; a task already finished by its op's return is left as-is so
// a richer op-side error isn't clobbered by the bare task detail.
func (p *Plugin) applySnapshot(id string, snap api.TaskSnapshot) {
	p.tasksMu.Lock()
	defer p.tasksMu.Unlock()
	t, ok := p.tasks[id]
	if !ok || t.info.State != sdk.TaskRunning {
		return
	}
	switch snap.State {
	case "succeeded":
		t.info.State = sdk.TaskSucceeded
		t.info.PercentComplete = 100
	case "failed":
		t.info.State = sdk.TaskFailed
		t.info.PercentComplete = 100
		if snap.Error != "" {
			t.info.Error = snap.Error
		}
	default:
		if snap.PercentComplete > t.info.PercentComplete {
			t.info.PercentComplete = snap.PercentComplete
		}
	}
	t.info.UpdatedAt = time.Now().UTC()
}

// taskSnapshot returns the current TaskInfo for a handle, or an error if it
// vanished (pruned) between calls.
func (p *Plugin) taskSnapshot(id string) (sdk.TaskInfo, error) {
	p.tasksMu.Lock()
	defer p.tasksMu.Unlock()
	t, ok := p.tasks[id]
	if !ok {
		return sdk.TaskInfo{}, fmt.Errorf("unknown task: %s", id)
	}
	return t.info, nil
}
