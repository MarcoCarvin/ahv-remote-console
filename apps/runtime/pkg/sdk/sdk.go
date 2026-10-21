// Package sdk is the public plugin SDK for AHV Remote Console. Plugins import this
// package to implement the Plugin interface and to register themselves with
// the runtime via Register().
//
// SDK stability: types and the Plugin interface in this package are part of
// the public API and follow semantic versioning. Breaking changes require a
// major-version bump.
package sdk

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// Plugin is the contract every protocol implementation must satisfy.
//
// All methods may be called concurrently. Plugins are expected to be
// thread-safe and to honor ctx cancellation.
type Plugin interface {
	// Manifest returns plugin metadata. Called once during host startup.
	Manifest() Manifest

	// Init wires plugin-runtime dependencies. Returning an error leaves the
	// plugin unregistered; the runtime continues with other plugins.
	Init(ctx context.Context, deps Dependencies) error

	// Shutdown releases resources. Best-effort, with a host-imposed timeout.
	Shutdown(ctx context.Context) error

	// AuthStart begins a credential acquisition flow. The body is plugin-
	// specific JSON.
	AuthStart(ctx context.Context, body json.RawMessage) (any, error)

	// AuthComplete finalizes the flow with the IdP redirect callback or the
	// device-flow result. On success, tokens are persisted via the SDK
	// Keystore.
	AuthComplete(ctx context.Context, body json.RawMessage) (any, error)

	// AuthRefresh is invoked proactively before access tokens expire and
	// reactively after upstream 401s.
	AuthRefresh(ctx context.Context) error

	// Logout removes persisted credentials.
	Logout(ctx context.Context) error

	// ListResources returns the page of resources matching q.
	ListResources(ctx context.Context, q ListQuery) (ListResult, error)

	// RefreshResources forces an upstream refresh, invalidating cached pages.
	RefreshResources(ctx context.Context) error

	// OpenSession is called once when the operator chooses a resource. The
	// returned SessionInfo is returned to the renderer; the runtime uses
	// SessionInfo.ID as the canonical handle.
	OpenSession(ctx context.Context, resourceID string) (SessionInfo, error)

	// OpenStream is called once per upstream connection (initial connect and
	// every reconnect after a failure). The plugin is responsible for
	// dialing the upstream WebSocket with proper TLS pinning, custom CA
	// trust, and authentication. The returned conn must already be in
	// console-ready state (post-handshake).
	OpenStream(ctx context.Context, sessionID string) (*websocket.Conn, error)

	// CloseSession is called when the renderer closes the console tab or the
	// runtime is shutting down.
	CloseSession(ctx context.Context, sessionID string) error
}

// Manifest is plugin metadata.
type Manifest struct {
	ID           string   `json:"id"`
	Name         string   `json:"name"`
	Version      string   `json:"version"`
	Description  string   `json:"description"`
	Vendor       string   `json:"vendor"`
	Capabilities []string `json:"capabilities"`
}

// Dependencies are the host-provided integration points.
type Dependencies struct {
	Logger    Logger
	Keystore  Keystore
	NetState  NetStateSubscriber
	Telemetry Telemetry
	Config    ConfigReader
	// Version is the runtime build version (build.Version: a git-describe
	// string, or "dev" for go run). Plugins use it for human-facing version
	// tags — e.g. the AHV plugin embeds it in the dev agent-ISO image name.
	Version string
	// OnSessionsExpired (optional) lets a plugin proactively tell the runtime
	// that the given sessions' credentials just went invalid (e.g. the keep-alive
	// observed a 401), so the runtime drives each session's console into the
	// re-auth arm BEFORE the live stream drops — the circular keep-alive loop.
	// Sessions are NOT closed by this: a re-auth refreshes the endpoint cookies
	// and the SAME session reconnects. nil-safe — plugins call it only when set.
	OnSessionsExpired func(sessionIDs []string)
}

type Logger interface {
	Debug(msg string, args ...any)
	Info(msg string, args ...any)
	Warn(msg string, args ...any)
	Error(msg string, args ...any)
	With(args ...any) Logger
}

type Keystore interface {
	Get(key string) ([]byte, error)
	Set(key string, value []byte) error
	Delete(key string) error
}

type NetStateSubscriber interface {
	Subscribe() (<-chan NetEvent, func())
}

type NetEvent struct {
	Type      string    `json:"type"`
	Interface string    `json:"interface"`
	Time      time.Time `json:"time"`
}

type Telemetry interface {
	StartSpan(ctx context.Context, name string) (context.Context, SpanFinish)
}

type SpanFinish func(err error)

type ConfigReader interface {
	Endpoints() []EndpointRef
	// CustomCAFiles returns the list of operator-configured CA bundle
	// file paths. Plugins seeding TLS RootCAs should call this so the
	// operator's internal CAs are honored on every upstream dial.
	// Previously this config was declared but unused (audit HIGH
	// finding) — implementations now must return real paths.
	CustomCAFiles() []string
}

// AuthKind names an authentication mechanism. The platform ships exactly one:
//
//   - AuthKindCookie  — VMRC-style SSO: AHV Remote Console opens Prism's login
//     page in a sandboxed embedded BrowserWindow, the
//     user authenticates via whatever IdP Prism is
//     configured with (Okta, Authentik, ...), and the
//     desktop shell hands the resulting session cookies
//     to the plugin for API replay.
type AuthKind string

const (
	AuthKindCookie AuthKind = "cookie"
)

type EndpointRef struct {
	ID               string            `json:"id"`
	Label            string            `json:"label"`
	URL              string            `json:"url"`
	PluginID         string            `json:"plugin_id"`
	AuthMethods      []AuthKind        `json:"auth_methods"`
	PinnedCertB64    string            `json:"pinned_cert_b64,omitempty"`
	AllowInsecureTLS bool              `json:"allow_insecure_tls,omitempty"`
	Tags             map[string]string `json:"tags,omitempty"`
}

// Allows reports whether the endpoint permits the given auth method.
func (e EndpointRef) Allows(kind AuthKind) bool {
	if len(e.AuthMethods) == 0 {
		return kind == AuthKindCookie
	}
	for _, k := range e.AuthMethods {
		if k == kind {
			return true
		}
	}
	return false
}

type ListQuery struct {
	Search   string
	Page     int
	PageSize int
}

type Resource struct {
	ID          string            `json:"id"`
	Label       string            `json:"label"`
	Status      string            `json:"status"`
	Description string            `json:"description,omitempty"`
	Cluster     string            `json:"cluster,omitempty"`
	IP          string            `json:"ip,omitempty"`
	Tags        map[string]string `json:"tags,omitempty"`
}

type ListResult struct {
	Resources []Resource `json:"resources"`
	Total     int        `json:"total"`
	Page      int        `json:"page"`
	PageSize  int        `json:"page_size"`
}

type SessionInfo struct {
	ID           string            `json:"id"`
	PluginID     string            `json:"plugin_id"`
	ResourceID   string            `json:"resource_id"`
	OpenedAt     time.Time         `json:"opened_at"`
	Capabilities []string          `json:"capabilities,omitempty"`
	Metadata     map[string]string `json:"metadata,omitempty"`
}

// ---- Registration ----

var (
	regMu      sync.Mutex
	registered []func() Plugin
)

// Register makes a plugin available to the runtime. Call this from an init()
// in the plugin package. The builder is invoked once during host startup.
func Register(builder func() Plugin) {
	if builder == nil {
		return
	}
	regMu.Lock()
	defer regMu.Unlock()
	registered = append(registered, builder)
}

// Registered returns a snapshot of the registered builders. The runtime calls
// this once at startup; subsequent registrations are ignored.
func Registered() []func() Plugin {
	regMu.Lock()
	defer regMu.Unlock()
	out := make([]func() Plugin, len(registered))
	copy(out, registered)
	return out
}

// ErrImagePublishRequired is returned (wrapped) by MountAgentISO when the
// agent image is not in the endpoint's catalog AND the operator's upstream
// role cannot create images. The IPC layer maps it to the wire error code
// "image_publish_required" so the renderer can show the
// ask-an-administrator-to-publish notice instead of a generic mount failure.
var ErrImagePublishRequired = errors.New("image_publish_required")

// ISOMountCapable is an optional plugin capability. Plugins that
// can attach installer ISOs to a running VM implement this
// interface; the runtime uses it to drive the "Install bridge"
// UX without knowing plugin-specific identifiers.
type ISOMountCapable interface {
	// MountAgentISO uploads the agent installer ISO (if not
	// already in the platform's image catalog) and attaches it as
	// a CD-ROM to the session's VM. Idempotent: re-calls return
	// the existing image's UUID with `Reused=true` and a no-op
	// attach if a CD-ROM with that image is already attached.
	MountAgentISO(ctx context.Context, sessionID string, opts ISOMountOptions) (ISOMountResult, error)
}

// ImageCatalogCapable is an optional plugin capability for platforms whose
// image service exposes a catalog of mountable ISOs. Plugins implement it so
// the runtime can drive the "Mount ISO from catalog" + "Eject CD-ROM" UX
// without knowing plugin-specific identifiers.
//
// All three methods resolve a console session (the same session resolution as
// ISOMountCapable.MountAgentISO) to the underlying VM. MountISO / EjectISO are
// VM-spec updates (they produce vm_update capability evidence). Unlike
// MountAgentISO, an arbitrary catalog ISO NEVER provisions the COM3 vdagent
// serial port — that channel is specific to the agent ISO.
type ImageCatalogCapable interface {
	// ListISOImages returns the mountable ISO images in the session's
	// endpoint catalog (image_type == ISO_IMAGE, in a usable state).
	ListISOImages(ctx context.Context, sessionID string) ([]CatalogImage, error)

	// MountISO attaches the catalog image identified by imageUUID into the
	// session VM's first CD-ROM drive (media change, like MountAgentISO's
	// attach), without touching the serial port. Idempotent.
	MountISO(ctx context.Context, sessionID, imageUUID string) (ISOMountResult, error)

	// EjectISO clears the media from the session VM's first CD-ROM drive.
	// Idempotent: an already-empty (or absent) CD-ROM reports Ejected=false.
	EjectISO(ctx context.Context, sessionID string) (EjectISOResult, error)
}

// CatalogImage is one entry the catalog picker offers the operator. Minimal by
// design — the picker lists by name and mounts by UUID.
type CatalogImage struct {
	UUID        string `json:"uuid"`
	Name        string `json:"name"`
	State       string `json:"state"`
	Description string `json:"description,omitempty"`
}

// EjectISOResult is the return shape from EjectISO. Ejected is false when the
// drive was already empty / no CD-ROM device exists (idempotent no-op).
type EjectISOResult struct {
	Ejected bool `json:"ejected"`
}

// ISOMountOptions carries operator-consented options for MountAgentISO.
type ISOMountOptions struct {
	// PowerCycle is true ONLY when the operator explicitly consented to an
	// orchestrated power cycle for a running VM that still needs the COM3
	// serial port: graceful ACPI guest shutdown → merged ISO+COM3 spec
	// update → power back on. A plugin MUST NOT change VM power state
	// unless this is set — and even then never via a hard power-off (an
	// ACPI shutdown that doesn't complete defers the serial add instead).
	// Optional on the wire; absent means false.
	PowerCycle bool `json:"power_cycle"`
}

// ISOMountResult is the return shape from MountAgentISO. The
// renderer surfaces these fields in the install UX.
type ISOMountResult struct {
	// ImageUUID is the Prism (or equivalent) image catalog UUID
	// the ISO landed at. Useful for audit and re-use.
	ImageUUID string `json:"image_uuid"`
	// DriveLetterHint is a best-guess at the drive letter the
	// guest OS will assign (e.g. "D:" on Windows). Empty if the
	// plugin can't make a reasonable guess.
	DriveLetterHint string `json:"drive_letter_hint"`
	// Reused is true when the upload step was skipped because an
	// image with this filename + SHA-256 was already in the
	// catalog. Renderer can show "Existing installer attached" vs
	// "Installer uploaded + attached" copy on this signal.
	Reused bool `json:"reused"`
	// SerialNote signals the COM3 serial port provisioning outcome:
	//   ""                              — no provisioning attempted
	//   "serial_port_present"           — port was already present, nothing to do
	//   "serial_port_added"             — port added, VM is off, appears on next boot
	//   "serial_port_power_off_required"— VM is running; AHV can't hot-add a serial
	//                                     port. Operator must power the VM fully OFF
	//                                     and back ON (an ACPI reboot is NOT enough).
	//   "serial_port_added_cycled"      — port added via the operator-consented
	//                                     orchestrated power cycle (the VM is back ON).
	//   "serial_port_deferred"          — a consented cycle was attempted but the
	//                                     ACPI shutdown did not reach OFF in budget;
	//                                     the add is pending the next OBSERVED
	//                                     power-off (no power state was forced).
	//   "serial_port_power_denied"      — the operator consented to a cycle but the
	//                                     upstream denied the power op (HTTP 403);
	//                                     the cycle aborted with the VM left as
	//                                     found, no retry, and the add deferred to
	//                                     the next OBSERVED power-off.
	//   "serial_port_error"             — failed to add (permissions, API error, …)
	SerialNote string `json:"serial_note,omitempty"`
	// TaskID is the Tasks-tray handle for THIS install op (see TaskCapable). The
	// renderer polls GetTask(TaskID) to drive the real install progress instead
	// of a fabricated timer. The same handle is already discoverable via
	// ListTasks by session_id, so it is also populated before this result
	// returns; surfacing it here lets the caller poll it directly.
	TaskID string `json:"task_id,omitempty"`
}

// TaskState is the lifecycle state of a long-running task surfaced to the
// renderer's Tasks tray. Deliberately coarse — the tray shows running /
// succeeded / failed, with PercentComplete driving the in-progress bar.
type TaskState string

const (
	TaskRunning   TaskState = "running"
	TaskSucceeded TaskState = "succeeded"
	TaskFailed    TaskState = "failed"
)

// TaskInfo is one long-running operation (power action, serial-port add, agent
// ISO install) surfaced to the renderer's Tasks tray. The plugin mints a handle
// when the op starts and updates it as the op progresses; GetTask refreshes the
// running ones against the real upstream task state.
//
// HARD INVARIANT (cross-cutting compliance): Label/Detail/Error are
// operator-facing strings only and MUST NEVER carry clipboard plaintext or
// credentials. Label is host-composed ("Power OFF <vmShort>"); Error comes from
// the upstream task's own operator-facing failure detail; Detail names the
// current phase. No payload bytes ever land here.
type TaskInfo struct {
	ID              string    `json:"id"`
	PluginID        string    `json:"plugin_id"`
	Kind            string    `json:"kind"` // "vm_power" | "serial_port" | "agent_iso"
	Label           string    `json:"label"`
	ResourceID      string    `json:"resource_id,omitempty"`
	SessionID       string    `json:"session_id,omitempty"`
	State           TaskState `json:"state"`
	PercentComplete int       `json:"percent_complete"`
	Detail          string    `json:"detail,omitempty"` // operator-facing current-step text
	Error           string    `json:"error,omitempty"`  // terminal failure reason; NEVER clipboard/credential data
	StartedAt       time.Time `json:"started_at"`
	UpdatedAt       time.Time `json:"updated_at"`
}

// TaskCapable is an optional plugin capability. Plugins that track long-running
// operations (power/serial/ISO) implement it so the runtime can serve the
// renderer's Tasks tray at GET /v1/tasks and GET /v1/tasks/{id} without knowing
// plugin-specific task identifiers.
//
// The task list is read-only: it DISPLAYS in-flight ops; it never triggers
// them. Creating a task stays gated on the same capability mirror + explicit
// in-UI confirmation the destructive actions already require.
type TaskCapable interface {
	// ListTasks returns a snapshot of this plugin's tracked tasks, newest
	// first. Terminal tasks linger briefly so the tray can show a just-
	// finished result before they age out.
	ListTasks(ctx context.Context) ([]TaskInfo, error)

	// GetTask refreshes ONE task against the upstream (for a running task with
	// a known upstream handle) and returns its current state. It returns an
	// error only when the handle is unknown to this plugin (the tray drops the
	// row rather than erroring the UI).
	GetTask(ctx context.Context, id string) (TaskInfo, error)
}

// PowerActionCapable is an optional plugin capability for VMs that
// support power state changes (on, off, reboot, reset). The runtime
// uses this to expose power controls in the console and lobby UI.
type PowerActionCapable interface {
	// PowerAction sends a power state change to a VM. resourceID is
	// the same compound ID used in ListResources (e.g., "endpoint/vmUUID").
	// action is one of: "ON", "OFF", "ACPI_SHUTDOWN", "ACPI_REBOOT", "RESET".
	PowerAction(ctx context.Context, resourceID string, action string) error
}

// SerialPortCapable is an optional plugin capability for platforms that can
// attach a serial port (COM3 for the vdagent clipboard channel) to a VM via
// their management API.
//
// This is exposed as a DEDICATED operator action — separate from installer-ISO
// mount — because on AHV a serial port can only be added while the VM is
// powered OFF (it cannot be hot-added to a running VM, and an in-guest reboot
// does not restart QEMU). The intended flow is: power the VM OFF, add the
// serial port (confirming the result here), power the VM back ON, then mount
// the installer ISO and run Install.cmd.
type SerialPortCapable interface {
	// AddSerialPort ensures the VM identified by resourceID has the serial
	// port the vdagent clipboard channel needs (COM3 / index 2). Idempotent.
	// resourceID is the same compound ID used by PowerAction
	// (e.g. "endpoint/vmUUID"), so it works for a powered-off VM with no
	// active console session.
	AddSerialPort(ctx context.Context, resourceID string) (SerialPortAddResult, error)
}

// StatsCapable is an optional plugin capability for platforms that can report a
// VM's live utilization (CPU / memory / disk / network). The runtime exposes it
// at GET /v1/vm/stats so the console can render a light-polling stats panel.
//
// READ-ONLY capability: GetVMStats reads a point-in-time sample and mutates
// nothing — so, unlike PowerActionCapable / ISOMountCapable, it has NO
// capability-mirror / evidence wiring (a read never proves vm_power or vm_update)
// and needs no operator confirmation or capability gate.
type StatsCapable interface {
	// GetVMStats returns a live utilization sample for the VM identified by
	// resourceID — the same compound ID used by PowerAction / AddSerialPort
	// (e.g. "endpoint/vmUUID"), so it works for any VM the endpoint manages.
	GetVMStats(ctx context.Context, resourceID string) (VMStats, error)
}

// VMStats is one point-in-time VM utilization sample surfaced to the console
// stats panel. CPUUsagePpm / MemoryUsagePpm are in parts-per-million (ppm;
// /10000 == percent); the disk/net fields are bytes per second. Any field equal
// to -1 means the upstream did not report that metric for this sample (the
// renderer shows it as "n/a" rather than a misleading 0).
type VMStats struct {
	CPUUsagePpm    int64 `json:"cpu_usage_ppm"`
	MemoryUsagePpm int64 `json:"memory_usage_ppm"`
	DiskReadBps    int64 `json:"disk_read_bps"`
	DiskWriteBps   int64 `json:"disk_write_bps"`
	NetRxBps       int64 `json:"net_rx_bps"`
	NetTxBps       int64 `json:"net_tx_bps"`
	// SampledAt is when the runtime read the sample (host clock), not an
	// upstream timestamp — it lets the panel show sample freshness.
	SampledAt time.Time `json:"sampled_at"`
}

// SnapshotCapable is an optional plugin capability for platforms that can
// create, list, and restore VM snapshots via their management API. The runtime
// exposes it at POST/GET /v1/vm/snapshots and POST /v1/vm/snapshots/restore so
// the console can show a Snapshots panel.
//
// resourceID is the same compound "<endpointId>/<vmUUID>" used by
// PowerAction / AddSerialPort, so it works for any VM the endpoint manages.
type SnapshotCapable interface {
	// CreateSnapshot takes a named snapshot of the VM. A snapshot create is a
	// VM-config write (vm_update evidence). Returns the new snapshot.
	CreateSnapshot(ctx context.Context, resourceID, name string) (SnapshotInfo, error)
	// ListSnapshots returns the VM's snapshots, newest-first. Read-only.
	ListSnapshots(ctx context.Context, resourceID string) ([]SnapshotInfo, error)
	// RestoreSnapshot restores the given snapshot (recovery point).
	//
	// On v3 this CREATES A NEW VM from the recovery point — it is NOT a
	// guaranteed in-place revert of the source VM. The plugin does NOT prompt —
	// the runtime/renderer MUST have obtained explicit operator confirmation
	// before calling it.
	RestoreSnapshot(ctx context.Context, resourceID, snapshotID string) error
}

// SnapshotInfo is one VM recovery point ("snapshot") surfaced to the console
// Snapshots panel. Fields reflect what v3 recovery points actually provide.
type SnapshotInfo struct {
	UUID      string `json:"uuid"`
	Name      string `json:"name"`
	CreatedAt string `json:"created_at,omitempty"`
	// ExpiresAt is the recovery point's garbage-collection time, empty when it
	// never expires.
	ExpiresAt string `json:"expires_at,omitempty"`
	// State is the recovery point's lifecycle state (e.g. COMPLETE).
	State string `json:"state,omitempty"`
	// SnapshotType is the recovery_point_type (CRASH_CONSISTENT /
	// APPLICATION_CONSISTENT / LIVE).
	SnapshotType string `json:"snapshot_type,omitempty"`
}

// SnapshotListResult is the wire shape of GET /v1/vm/snapshots.
type SnapshotListResult struct {
	Snapshots []SnapshotInfo `json:"snapshots"`
}

// IdentityCapable is an optional plugin capability. Plugins that can report
// which user an endpoint's stored session authenticates as implement it; the
// runtime exposes it at GET /v1/plugins/{id}/whoami?endpoint_id= so the lobby
// account menu can show the signed-in user (not just the cluster).
type IdentityCapable interface {
	// WhoAmI returns the authenticated user behind endpointID's stored
	// session. Returns an error when the endpoint is unknown, has no stored
	// credentials, or the upstream identity probe fails.
	WhoAmI(ctx context.Context, endpointID string) (Identity, error)
}

// Identity is the signed-in user surfaced to the lobby account menu.
type Identity struct {
	Username    string `json:"username"`
	DisplayName string `json:"display_name,omitempty"`
}

// HealthCapable is an optional plugin capability. Plugins that can probe a
// single endpoint's reachability implement it; the runtime exposes it at
// GET /v1/config/endpoints/{id}/health so the cluster map can show a status dot
// per node.
//
// Implementations MUST be a cheap, UNAUTHENTICATED reachability probe (a single
// short request, bounded by a timeout) — they MUST NOT send credentials, read
// response bodies, or disclose any cluster detail. Reachability + latency is
// all the map needs; richer telemetry belongs to the post-authentication lobby.
type HealthCapable interface {
	// EndpointHealth probes one endpoint the plugin owns and returns its
	// reachability. Returns an error if the endpoint id is not managed by this
	// plugin.
	EndpointHealth(ctx context.Context, endpointID string) (EndpointHealth, error)
}

// EndpointHealth is the reachability of a single endpoint, surfaced to the
// cluster-map status dot. Deliberately minimal — host up/down + latency, no
// cluster information — so probing every node leaks nothing and needs no auth.
type EndpointHealth struct {
	EndpointID string `json:"endpoint_id"`
	// Status is "healthy" (the host answered) or "offline" (transport failure).
	Status string `json:"status"`
	// RTTMs is the reachability round-trip in ms (nil when offline). It is
	// network latency to the host, not cluster telemetry.
	RTTMs *int `json:"rtt_ms,omitempty"`
	// Capabilities mirrors the plugin's probe/evidence-derived view of what
	// the CURRENT operator session can do on this endpoint (nil when the
	// plugin doesn't track capabilities). It is locally cached state — the
	// reachability probe above stays unauthenticated and sends nothing extra.
	Capabilities *EndpointCapabilities `json:"capabilities,omitempty"`
}

// EndpointCapabilities is the per-endpoint upstream-permission mirror riding
// the health payload. Each string field is "unknown" | "allowed" | "denied":
// "unknown" means no evidence either way (the renderer treats it as normal —
// only PROVEN denials, i.e. an upstream 403 from a read-only probe or a real
// attempt, degrade the UI). AgentISOInCatalog reports whether the agent
// installer ISO is already published in the endpoint's image catalog; when
// true, ImageCreate is not required for the full setup flow.
type EndpointCapabilities struct {
	ImageRead         string `json:"image_read"`
	ImageCreate       string `json:"image_create"`
	VMUpdate          string `json:"vm_update"`
	VMPower           string `json:"vm_power"`
	AgentISOInCatalog bool   `json:"agent_iso_in_catalog"`
}

// SerialPortAddResult is the return shape from AddSerialPort. The renderer
// maps Note to operator-facing copy (see serialCopy.ts).
type SerialPortAddResult struct {
	// Note uses the same vocabulary as ISOMountResult.SerialNote:
	//   "serial_port_present"            — already present, nothing to do
	//   "serial_port_added"              — added; appears on the next boot
	//   "serial_port_power_off_required" — VM is running; power it fully OFF then ON
	//   "serial_port_error"              — failed to add
	Note string `json:"note"`
	// PowerState is the VM power state at call time ("ON"/"OFF"), so the
	// renderer can tailor guidance (e.g. "you can power on now").
	PowerState string `json:"power_state,omitempty"`
}
