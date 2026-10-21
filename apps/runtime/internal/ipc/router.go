package ipc

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/gorilla/websocket"
	"go.opentelemetry.io/otel/attribute"

	"github.com/MarcoCarvin/ahv-remote-console/apps/runtime/internal/audit"
	"github.com/MarcoCarvin/ahv-remote-console/apps/runtime/internal/build"
	"github.com/MarcoCarvin/ahv-remote-console/apps/runtime/internal/config"
	"github.com/MarcoCarvin/ahv-remote-console/apps/runtime/internal/plugins"
	"github.com/MarcoCarvin/ahv-remote-console/apps/runtime/internal/presence"
	"github.com/MarcoCarvin/ahv-remote-console/apps/runtime/internal/proxy"
	"github.com/MarcoCarvin/ahv-remote-console/apps/runtime/internal/telemetry"
	"github.com/MarcoCarvin/ahv-remote-console/apps/runtime/pkg/sdk"
)

type router struct {
	opts Options
	log  *slog.Logger
}

func newRouter(opts Options) *router {
	return &router{opts: opts, log: opts.Logger}
}

func (r *router) attach(mux *http.ServeMux) {
	mux.HandleFunc("GET /healthz", r.healthz)
	mux.HandleFunc("GET /readyz", r.readyz)
	mux.HandleFunc("GET /metrics", r.metrics)
	mux.HandleFunc("GET /version", r.version)
	mux.HandleFunc("GET /v1/runtime/state", r.runtimeState)

	mux.HandleFunc("GET /v1/config/endpoints", r.listEndpoints)
	mux.HandleFunc("GET /v1/config/endpoints/{id}/health", r.endpointHealth)
	mux.HandleFunc("POST /v1/config/endpoints", r.upsertEndpoint)
	mux.HandleFunc("DELETE /v1/config/endpoints/{id}", r.deleteEndpoint)

	mux.HandleFunc("GET /v1/plugins", r.listPlugins)
	mux.HandleFunc("POST /v1/plugins/{id}/auth/start", r.pluginAuthStart)
	mux.HandleFunc("POST /v1/plugins/{id}/auth/complete", r.pluginAuthComplete)
	mux.HandleFunc("POST /v1/plugins/{id}/auth/refresh", r.pluginAuthRefresh)
	mux.HandleFunc("POST /v1/plugins/{id}/auth/logout", r.pluginLogout)

	mux.HandleFunc("GET /v1/plugins/{id}/whoami", r.pluginWhoAmI)

	mux.HandleFunc("GET /v1/plugins/{id}/resources", r.listResources)
	mux.HandleFunc("POST /v1/plugins/{id}/resources/refresh", r.refreshResources)

	mux.HandleFunc("POST /v1/sessions", r.openSession)
	mux.HandleFunc("GET /v1/sessions", r.listSessions)
	mux.HandleFunc("DELETE /v1/sessions/{sid}", r.closeSession)
	mux.HandleFunc("/v1/sessions/{sid}/stream", r.sessionStream)

	mux.HandleFunc("GET /v1/diagnostics/bundle", r.diagnosticsBundle)

	// Clipboard sync now flows in-band over the VNC stream via the
	// AHV Remote Console bridge daemon installed in the guest. No IPC
	// surface is needed for clipboard — the renderer handles
	// everything through the session WebSocket it already has.
	//
	// The bridge daemon doesn't speak to the runtime directly, so the
	// renderer (which owns the session context) emits bridge-lifecycle
	// audit events on the bridge's behalf through this endpoint.
	mux.HandleFunc("POST /v1/agent/audit", r.agentAudit)

	// Bridge installer ISO mount. The renderer asks the runtime to
	// upload the bundled bridge ISO to the cluster's Image Service
	// (idempotent — re-uses the existing image if it's already
	// present) and attach it as a CD-ROM to the session's VM. The
	// guest then sees `D:\Install.cmd` (or equivalent drive letter)
	// and the operator runs it once per VM — VMware-Tools-style.
	// Replaces the auto-typed install one-liner / HTTP binary
	// download path.
	mux.HandleFunc("POST /v1/agent/mount-iso", r.agentMountISO)
	mux.HandleFunc("POST /v1/vm/power", r.vmPowerAction)
	mux.HandleFunc("POST /v1/vm/serial-port", r.vmAddSerialPort)

	// VM snapshots (create / list / restore). Create + restore are POSTs;
	// restore is DESTRUCTIVE and writes a durable audit record. List is a GET
	// with plugin_id + resource_id on the query string so the signing
	// path-regex stays a fixed literal (mirrors vmStats / pluginWhoAmI).
	mux.HandleFunc("POST /v1/vm/snapshots", r.vmCreateSnapshot)
	mux.HandleFunc("GET /v1/vm/snapshots", r.vmListSnapshots)
	mux.HandleFunc("POST /v1/vm/snapshots/restore", r.vmRestoreSnapshot)

	// Live VM utilization (CPU / mem / disk / net) for the console stats panel.
	// Read-only: plugin_id + resource_id ride the query string (a GET) so the
	// signing path-regex stays a fixed literal. No audit append — like
	// listSessions / listResources, this is a high-frequency read poll and
	// appending per poll would flood the audit log.
	mux.HandleFunc("GET /v1/vm/stats", r.vmStats)

	// Long-running-task tray. The renderer polls these (read-only) to show
	// running/succeeded/failed power, serial-port, and agent-ISO ops with the
	// real upstream task state. listTasks aggregates across plugins; getTask
	// refreshes one handle. No audit append (read-only, like listSessions).
	mux.HandleFunc("GET /v1/tasks", r.listTasks)
	mux.HandleFunc("GET /v1/tasks/{id}", r.getTask)

	// Arbitrary catalog ISO mount + eject (beyond the hardwired agent ISO).
	// The renderer lists the cluster's Image-Service ISOs, mounts a chosen one
	// by UUID into the session VM's CD-ROM, and ejects the current media. The
	// session ID rides the JSON body (not the path) so the signing path-regex
	// stays a fixed literal — see apps/desktop/src/main/signable.ts.
	mux.HandleFunc("GET /v1/agent/images", r.listISOImages)
	mux.HandleFunc("POST /v1/vm/mount-iso", r.mountISO)
	mux.HandleFunc("POST /v1/vm/eject-iso", r.ejectISO)

	mux.HandleFunc("/v1/presence/ws", r.presenceWS)
	mux.HandleFunc("GET /v1/presence/peers", r.presencePeers)
	mux.HandleFunc("GET /v1/config/features", r.listFeatures)

	// (clipboard/stage-iso endpoint removed — v3 ships text inline
	// via the keystroke (host→guest) + pixel-strip (guest→host)
	// channels. The legacy ISO-based clipboard data plane is gone;
	// the agent installer ISO mount above is a separate path that
	// stays.)
}

// ---- /healthz, /readyz, /metrics, /version, /v1/runtime/state ----

// healthz is the LIVENESS probe (OBS-01): the process is up and the HTTP server
// is answering. It deliberately reports nothing about plugin/upstream state —
// that's readiness (/readyz). HMAC-exempt like /version.
func (r *router) healthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
}

// readyz is the READINESS probe (OBS-01): reports whether plugins have loaded
// and surfaces the last plugin-load error. A process can be live (healthz OK)
// but not yet ready (no plugin loaded / a plugin failed to init). Returns 503
// when no plugin is loaded or the last load reported an error, so an
// orchestrator can hold traffic until the runtime can actually serve sessions.
func (r *router) readyz(w http.ResponseWriter, _ *http.Request) {
	loaded, loadErr := r.opts.Plugins.Readiness()
	body := map[string]any{
		"plugins_loaded": loaded,
	}
	if loadErr != "" {
		body["last_plugin_load_error"] = loadErr
	}
	if loaded == 0 || loadErr != "" {
		body["status"] = "not_ready"
		writeJSON(w, http.StatusServiceUnavailable, body)
		return
	}
	body["status"] = "ready"
	writeJSON(w, http.StatusOK, body)
}

// metrics serves a minimal Prometheus text-exposition document (OBS-01) with
// the runtime-owned gauges/counters plus Go runtime stats. The active-session
// gauge is read from host-visible state at scrape time; the reconnect/clipboard
// counters and upstream RTT are published by the proxy (see telemetry.metrics).
func (r *router) metrics(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	telemetry.WriteMetrics(w, telemetry.MetricsSnapshot{
		ActiveSessions: r.opts.Plugins.ActiveSessions(),
	})
}

func (r *router) version(w http.ResponseWriter, _ *http.Request) {
	// This endpoint is HMAC-exempt (reachable without the IPC secret). Expose
	// only a coarse version string — never the exact commit/build date — so an
	// unauthenticated local or cross-origin caller cannot fingerprint the
	// precise build for targeting known-CVE exploits.
	writeJSON(w, http.StatusOK, map[string]any{
		"version": build.Version,
	})
}

func (r *router) runtimeState(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"version": build.Version,
		"commit":  build.Commit,
		"date":    build.Date,
		"plugins": r.opts.Plugins.Summaries(),
		"network": r.opts.NetState.Snapshot(),
		"observability": map[string]any{
			"traces_enabled": r.opts.Telemetry.Enabled(),
		},
	})
}

// ---- endpoint CRUD ----

type endpointWire struct {
	ID            string            `json:"id"`
	Label         string            `json:"label"`
	PluginID      string            `json:"plugin_id"`
	URL           string            `json:"url"`
	AuthMethods   []config.AuthKind `json:"auth_methods"`
	HasPinnedCert bool              `json:"has_pinned_cert"`
	// PinnedCertB64 is the operator-configured pinned certificate (base64
	// DER). It is the cluster's PUBLIC TLS certificate — it carries no
	// secret and is presented in every TLS handshake — so surfacing it to
	// the renderer is safe, and the desktop shell needs it to pin the
	// embedded SSO login window to the same certificate the runtime trusts.
	PinnedCertB64    string            `json:"pinned_cert_b64,omitempty"`
	AllowInsecureTLS bool              `json:"allow_insecure_tls"`
	Tags             map[string]string `json:"tags,omitempty"`
}

func wireFromConfig(e config.Endpoint) endpointWire {
	w := endpointWire{
		ID:               e.ID,
		Label:            e.Label,
		PluginID:         e.PluginID,
		URL:              e.URL,
		AuthMethods:      e.AuthMethods,
		HasPinnedCert:    e.PinnedCertB64 != "",
		PinnedCertB64:    e.PinnedCertB64,
		AllowInsecureTLS: e.AllowInsecureTLS,
		Tags:             e.Tags,
	}
	if len(w.AuthMethods) == 0 {
		w.AuthMethods = []config.AuthKind{config.AuthKindCookie}
	}
	return w
}

func (r *router) listEndpoints(w http.ResponseWriter, _ *http.Request) {
	cfg := r.opts.Config.Get()
	out := make([]endpointWire, 0, len(cfg.Endpoints))
	for _, e := range cfg.Endpoints {
		out = append(out, wireFromConfig(e))
	}
	writeJSON(w, http.StatusOK, map[string]any{"endpoints": out})
}

// endpointHealth returns the coarse reachability of ONE endpoint — the one the
// operator has selected in the connection manager. It deliberately probes only
// the requested endpoint (no fan-out to other clusters) and returns no cluster
// detail, so the pre-authentication login screen leaks nothing.
func (r *router) endpointHealth(w http.ResponseWriter, req *http.Request) {
	id := req.PathValue("id")
	cfg := r.opts.Config.Get()
	pluginID := ""
	for _, e := range cfg.Endpoints {
		if e.ID == id {
			pluginID = e.PluginID
			break
		}
	}
	if pluginID == "" {
		writeErr(w, http.StatusNotFound, "endpoint_not_found", "unknown endpoint: "+id)
		return
	}
	p, ok := r.opts.Plugins.Get(pluginID)
	if !ok {
		writeErr(w, http.StatusNotFound, "plugin_not_found", "unknown plugin: "+pluginID)
		return
	}
	hc, ok := p.(sdk.HealthCapable)
	if !ok {
		writeErr(w, http.StatusServiceUnavailable, "health_unsupported",
			"plugin does not report endpoint health")
		return
	}
	h, err := hc.EndpointHealth(req.Context(), id)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "health_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"health": h})
}

type upsertEndpointBody struct {
	ID               string            `json:"id"`
	Label            string            `json:"label"`
	PluginID         string            `json:"plugin_id"`
	URL              string            `json:"url"`
	AuthMethods      []config.AuthKind `json:"auth_methods"`
	PinnedCertB64    string            `json:"pinned_cert_b64,omitempty"`
	AllowInsecureTLS bool              `json:"allow_insecure_tls,omitempty"`
	Tags             map[string]string `json:"tags,omitempty"`
}

func (r *router) upsertEndpoint(w http.ResponseWriter, req *http.Request) {
	var b upsertEndpointBody
	if err := readJSON(req, &b); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_body", err.Error())
		return
	}
	if b.URL == "" || b.PluginID == "" {
		writeErr(w, http.StatusBadRequest, "missing_fields", "url and plugin_id are required")
		return
	}
	if len(b.AuthMethods) == 0 {
		writeErr(w, http.StatusBadRequest, "missing_fields", "at least one auth method is required")
		return
	}
	saved, err := r.opts.Config.UpsertEndpoint(config.Endpoint{
		ID:               b.ID,
		Label:            b.Label,
		PluginID:         b.PluginID,
		URL:              b.URL,
		AuthMethods:      b.AuthMethods,
		PinnedCertB64:    b.PinnedCertB64,
		AllowInsecureTLS: b.AllowInsecureTLS,
		Tags:             b.Tags,
	})
	if err != nil {
		r.opts.Audit.Append(audit.ActionEndpointUpsert, audit.Record{
			EndpointID: b.ID, PluginID: b.PluginID, Result: "error", Reason: err.Error(),
		})
		writeErr(w, http.StatusBadRequest, "save_failed", err.Error())
		return
	}
	r.opts.Audit.Append(audit.ActionEndpointUpsert, audit.Record{
		EndpointID: saved.ID, PluginID: saved.PluginID,
	})
	writeJSON(w, http.StatusOK, map[string]any{"endpoint": wireFromConfig(saved)})
}

func (r *router) deleteEndpoint(w http.ResponseWriter, req *http.Request) {
	id := req.PathValue("id")
	if err := r.opts.Config.DeleteEndpoint(id); err != nil {
		r.opts.Audit.Append(audit.ActionEndpointDelete, audit.Record{
			EndpointID: id, Result: "error", Reason: err.Error(),
		})
		writeErr(w, http.StatusNotFound, "delete_failed", err.Error())
		return
	}
	r.opts.Audit.Append(audit.ActionEndpointDelete, audit.Record{EndpointID: id})
	writeJSON(w, http.StatusNoContent, nil)
}

// ---- plugin discovery ----

func (r *router) listPlugins(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"plugins": r.opts.Plugins.Summaries(),
	})
}

func (r *router) pluginAuthStart(w http.ResponseWriter, req *http.Request) {
	id := req.PathValue("id")
	p, ok := r.opts.Plugins.Get(id)
	if !ok {
		writeErr(w, http.StatusNotFound, "plugin_not_found", "unknown plugin: "+id)
		return
	}
	var body json.RawMessage
	if err := readJSON(req, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_body", err.Error())
		return
	}
	resp, err := p.AuthStart(req.Context(), body)
	if err != nil {
		r.opts.Audit.Append(audit.ActionAuthStart, audit.Record{
			PluginID: id, Result: "error", Reason: err.Error(),
		})
		writeErr(w, http.StatusBadGateway, "auth_start_failed", err.Error())
		return
	}
	r.opts.Audit.Append(audit.ActionAuthStart, audit.Record{PluginID: id})
	writeJSON(w, http.StatusOK, resp)
}

func (r *router) pluginAuthComplete(w http.ResponseWriter, req *http.Request) {
	id := req.PathValue("id")
	p, ok := r.opts.Plugins.Get(id)
	if !ok {
		writeErr(w, http.StatusNotFound, "plugin_not_found", "unknown plugin: "+id)
		return
	}
	var body json.RawMessage
	if err := readJSON(req, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_body", err.Error())
		return
	}
	resp, err := p.AuthComplete(req.Context(), body)
	if err != nil {
		r.opts.Audit.Append(audit.ActionAuthComplete, audit.Record{
			PluginID: id, Result: "error", Reason: err.Error(),
		})
		writeErr(w, http.StatusBadGateway, "auth_complete_failed", err.Error())
		return
	}
	r.opts.Audit.Append(audit.ActionAuthComplete, audit.Record{PluginID: id})
	writeJSON(w, http.StatusOK, resp)
}

func (r *router) pluginAuthRefresh(w http.ResponseWriter, req *http.Request) {
	id := req.PathValue("id")
	p, ok := r.opts.Plugins.Get(id)
	if !ok {
		writeErr(w, http.StatusNotFound, "plugin_not_found", "unknown plugin: "+id)
		return
	}
	if err := p.AuthRefresh(req.Context()); err != nil {
		r.opts.Audit.Append(audit.ActionAuthRefresh, audit.Record{
			PluginID: id, Result: "error", Reason: err.Error(),
		})
		writeErr(w, http.StatusBadGateway, "auth_refresh_failed", err.Error())
		return
	}
	r.opts.Audit.Append(audit.ActionAuthRefresh, audit.Record{PluginID: id})
	writeJSON(w, http.StatusOK, map[string]any{"refreshed": true})
}

func (r *router) pluginLogout(w http.ResponseWriter, req *http.Request) {
	id := req.PathValue("id")
	p, ok := r.opts.Plugins.Get(id)
	if !ok {
		writeErr(w, http.StatusNotFound, "plugin_not_found", "unknown plugin: "+id)
		return
	}
	if err := p.Logout(req.Context()); err != nil {
		r.opts.Audit.Append(audit.ActionLogout, audit.Record{
			PluginID: id, Result: "error", Reason: err.Error(),
		})
		writeErr(w, http.StatusBadGateway, "logout_failed", err.Error())
		return
	}
	r.opts.Audit.Append(audit.ActionLogout, audit.Record{PluginID: id})
	writeJSON(w, http.StatusNoContent, nil)
}

// pluginWhoAmI reports which user an endpoint's stored session authenticates
// as (sdk.IdentityCapable), so the lobby account menu shows the operator.
func (r *router) pluginWhoAmI(w http.ResponseWriter, req *http.Request) {
	id := req.PathValue("id")
	p, ok := r.opts.Plugins.Get(id)
	if !ok {
		writeErr(w, http.StatusNotFound, "plugin_not_found", "unknown plugin: "+id)
		return
	}
	ic, ok := p.(sdk.IdentityCapable)
	if !ok {
		writeErr(w, http.StatusServiceUnavailable, "whoami_unsupported",
			"plugin does not report the signed-in user")
		return
	}
	endpointID := req.URL.Query().Get("endpoint_id")
	if endpointID == "" {
		writeErr(w, http.StatusBadRequest, "endpoint_required", "endpoint_id query param required")
		return
	}
	// Endpoint ownership is enforced HERE, not just inside the plugin: with a
	// second IdentityCapable plugin registered, a renderer could otherwise sign
	// GET /v1/plugins/A/whoami?endpoint_id=<B's endpoint> and rely on plugin A
	// to police plugin B's endpoints. Mirrors endpointHealth's config lookup.
	owned := false
	for _, e := range r.opts.Config.Get().Endpoints {
		if e.ID == endpointID {
			owned = e.PluginID == id
			break
		}
	}
	if !owned {
		writeErr(w, http.StatusNotFound, "endpoint_not_found",
			"endpoint does not exist or is not managed by plugin "+id)
		return
	}
	ident, err := ic.WhoAmI(req.Context(), endpointID)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "whoami_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"identity": ident})
}

// ---- resources (VMs etc) ----

func (r *router) listResources(w http.ResponseWriter, req *http.Request) {
	id := req.PathValue("id")
	p, ok := r.opts.Plugins.Get(id)
	if !ok {
		writeErr(w, http.StatusNotFound, "plugin_not_found", "unknown plugin: "+id)
		return
	}
	q := req.URL.Query()
	res, err := p.ListResources(req.Context(), plugins.ListQuery{
		Search:   q.Get("q"),
		Page:     atoiOr(q.Get("page"), 0),
		PageSize: atoiOr(q.Get("page_size"), 50),
	})
	if err != nil {
		writeErr(w, http.StatusBadGateway, "list_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (r *router) refreshResources(w http.ResponseWriter, req *http.Request) {
	id := req.PathValue("id")
	p, ok := r.opts.Plugins.Get(id)
	if !ok {
		writeErr(w, http.StatusNotFound, "plugin_not_found", "unknown plugin: "+id)
		return
	}
	if err := p.RefreshResources(req.Context()); err != nil {
		writeErr(w, http.StatusBadGateway, "refresh_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"refreshed": true})
}

// ---- sessions ----

type openSessionRequest struct {
	PluginID   string `json:"plugin_id"`
	ResourceID string `json:"resource_id"`
}

func (r *router) openSession(w http.ResponseWriter, req *http.Request) {
	var body openSessionRequest
	if err := readJSON(req, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_body", err.Error())
		return
	}
	// Control-plane span (OBS-01): carries plugin_id/endpoint; no-op when
	// telemetry is disabled.
	ctx, finish := r.opts.Telemetry.StartSpan(req.Context(), "ipc.openSession",
		attribute.String("plugin_id", body.PluginID),
		attribute.String("endpoint", "POST /v1/sessions"),
	)
	defer finish(nil)
	req = req.WithContext(ctx)
	p, ok := r.opts.Plugins.Get(body.PluginID)
	if !ok {
		writeErr(w, http.StatusNotFound, "plugin_not_found", "unknown plugin")
		return
	}
	sess, err := r.opts.Plugins.OpenSession(req.Context(), p, body.ResourceID)
	if err != nil {
		r.opts.Audit.Append(audit.ActionSessionOpen, audit.Record{
			PluginID: body.PluginID,
			VMID:     body.ResourceID,
			Result:   "error",
			Reason:   err.Error(),
		})
		writeErr(w, http.StatusBadGateway, "open_failed", err.Error())
		return
	}
	r.opts.Audit.Append(audit.ActionSessionOpen, audit.Record{
		PluginID:  body.PluginID,
		SessionID: sess.ID,
		VMID:      body.ResourceID,
	})
	writeJSON(w, http.StatusCreated, map[string]any{
		"session": sess,
	})
}

func (r *router) listSessions(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"sessions": r.opts.Plugins.Sessions(),
	})
}

func (r *router) closeSession(w http.ResponseWriter, req *http.Request) {
	sid := req.PathValue("sid")
	// Control-plane span (OBS-01): carries session_id/endpoint; no-op when
	// telemetry is disabled.
	ctx, finish := r.opts.Telemetry.StartSpan(req.Context(), "ipc.closeSession",
		attribute.String("session_id", sid),
		attribute.String("endpoint", "DELETE /v1/sessions/{sid}"),
	)
	defer finish(nil)
	req = req.WithContext(ctx)
	if err := r.opts.Plugins.CloseSession(req.Context(), sid); err != nil {
		r.opts.Audit.Append(audit.ActionSessionClose, audit.Record{
			SessionID: sid, Result: "error", Reason: err.Error(),
		})
		writeErr(w, http.StatusNotFound, "close_failed", err.Error())
		return
	}
	r.opts.Audit.Append(audit.ActionSessionClose, audit.Record{SessionID: sid})
	writeJSON(w, http.StatusNoContent, nil)
}

func (r *router) sessionStream(w http.ResponseWriter, req *http.Request) {
	sid := req.PathValue("sid")
	sess, ok := r.opts.Plugins.GetSession(sid)
	if !ok {
		writeErr(w, http.StatusNotFound, "session_not_found", "unknown session: "+sid)
		return
	}
	// Root the proxy's parent context in BOTH the request context (cancelled
	// when the renderer's loopback ws drops) AND the host's shutdown context
	// (cancelled at graceful shutdown). A hijacked WS request context is NOT
	// cancelled by http.Server.Shutdown, so without the host context an active
	// console would make shutdown wait out the full grace; with it, the proxy
	// pumps see ctx.Done() and tear down promptly (REL-02).
	ctx, cancel := context.WithCancel(req.Context())
	defer cancel()
	go func() {
		select {
		case <-r.opts.Plugins.BaseContext().Done():
			cancel()
		case <-ctx.Done():
		}
	}()
	req = req.WithContext(ctx)

	// Hand off to the proxy package which manages the upgrade,
	// heartbeats, and reconnect to the upstream plugin endpoint.
	// The proxy itself only sees RFB binary frames + minimal
	// ctrl-frame JSON (ping/pong, reconnect status); no agent
	// channel multiplexing anymore.
	//
	// When presence (multi-operator) is enabled, wire a RoleChecker
	// backed by the presence hub so the proxy strips a viewer's input
	// (KeyEvent / PointerEvent / QEMU client message) and only a
	// controller can drive the guest (AHV-02). When presence is
	// disabled there is a single operator who is always the controller,
	// so we keep today's behavior (nil role checker → isController).
	if r.opts.Presence != nil && r.presenceEnabled() {
		hub := r.opts.Presence
		proxy.HandleStreamWithRole(w, req, sess, r.log, func(sessionID string) string {
			return string(hub.RoleOf(sessionID))
		})
	} else {
		proxy.HandleStream(w, req, sess, r.log)
	}

	// The proxy run loop has ended — almost always because the renderer's ws
	// dropped, which is the TRANSIENT drop the renderer's self-heal recovers from
	// by reopening the SAME sessionId. We must NOT close the plugin session here:
	// destroying it would 404 that reopen (GetSession miss) and the console could
	// never reconnect — it would sit "reconnecting" forever (the AHV-03 regression).
	// The session deliberately persists so the renderer can re-attach. Cleanup of a
	// genuinely-abandoned session (no reopen) is handled by the idle reaper
	// (host: sessionIdleTTL, gated on !StreamAttached) and by the renderer's
	// explicit DELETE /v1/sessions/{sid} on window close/unmount.
}

// ---- diagnostics ----

func (r *router) diagnosticsBundle(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", `attachment; filename="ahv-remote-console-diagnostics.zip"`)
	if err := r.opts.Plugins.WriteDiagnostics(w); err != nil {
		r.log.Error("diagnostics export failed", "err", err)
		r.opts.Audit.Append(audit.ActionDiagnosticsExport, audit.Record{
			Result: "error", Reason: err.Error(),
		})
		return
	}
	r.opts.Audit.Append(audit.ActionDiagnosticsExport, audit.Record{})
}

// ---- bridge audit ----

// agentAuditRequest is the wire shape the renderer's useAgent hook
// posts after a bridge lifecycle event. The renderer holds the session
// context that the bridge daemon never sees directly; this endpoint
// stamps the event into the runtime's append-only audit log on the
// bridge's behalf.
//
// Compliance invariant: this body NEVER carries clipboard plaintext.
// Only direction, byte count, and an optional sha-256 hex digest of
// the payload (used by SIEMs to correlate transfers without learning
// content). The handler enforces the action allow-list so a
// compromised renderer can't pivot the endpoint into a general-purpose
// audit-record injector.
type agentAuditRequest struct {
	Action    string `json:"action"`
	SessionID string `json:"sessionId"`
	Bytes     int    `json:"bytes,omitempty"`
	Direction string `json:"direction,omitempty"`
	SHA256    string `json:"sha256,omitempty"`
}

// allowedAgentAuditActions is the closed set the renderer is allowed
// to emit through this endpoint. Any other action string is rejected
// with 400 so the route can't be co-opted into recording arbitrary
// events.
var allowedAgentAuditActions = map[audit.Action]struct{}{
	audit.ActionBridgeInstall:     {},
	audit.ActionBridgeUninstall:   {},
	audit.ActionBridgeConnect:     {},
	audit.ActionBridgeDisconnect:  {},
	audit.ActionBridgeClipIn:      {},
	audit.ActionBridgeClipOut:     {},
	audit.ActionBridgeProbeReject: {},
}

func (r *router) agentAudit(w http.ResponseWriter, req *http.Request) {
	var body agentAuditRequest
	if err := readJSON(req, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_body", err.Error())
		return
	}
	action := audit.Action(body.Action)
	if _, ok := allowedAgentAuditActions[action]; !ok {
		writeErr(w, http.StatusBadRequest, "invalid_action",
			"action must be one of bridge.install, bridge.uninstall, bridge.connect, bridge.disconnect, bridge.clip_in, bridge.clip_out, bridge.probe_reject")
		return
	}
	extra := agentAuditExtra(body)
	r.opts.Audit.Append(action, audit.Record{
		SessionID: body.SessionID,
		Extra:     extra,
	})
	writeJSON(w, http.StatusNoContent, nil)
}

// agentAuditExtra builds the Extra payload for a agent audit record.
// Only non-zero fields are emitted so the resulting JSON stays compact
// and downstream consumers don't trip over fields that don't apply to
// a particular action (e.g. connect has no bytes/direction).
func agentAuditExtra(body agentAuditRequest) map[string]any {
	extra := make(map[string]any, 3)
	if body.Bytes > 0 {
		extra["bytes"] = body.Bytes
	}
	if body.Direction != "" {
		extra["direction"] = body.Direction
	}
	if body.SHA256 != "" {
		extra["sha256"] = body.SHA256
	}
	if len(extra) == 0 {
		return nil
	}
	return extra
}

// ---- agent installer ISO mount ----

// vmPowerAction sends a power state change to a VM via the plugin.
// Takes plugin_id + resource_id so it works for off VMs without a session.
func (r *router) vmPowerAction(w http.ResponseWriter, req *http.Request) {
	var body struct {
		PluginID   string `json:"plugin_id"`
		ResourceID string `json:"resource_id"`
		Action     string `json:"action"`
	}
	if err := readJSON(req, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_body", err.Error())
		return
	}
	if body.PluginID == "" || body.ResourceID == "" || body.Action == "" {
		writeErr(w, http.StatusBadRequest, "missing_fields", "plugin_id, resource_id, and action are required")
		return
	}
	valid := map[string]bool{"ON": true, "OFF": true, "ACPI_SHUTDOWN": true, "ACPI_REBOOT": true, "RESET": true}
	if !valid[body.Action] {
		writeErr(w, http.StatusBadRequest, "invalid_action", "action must be ON, OFF, ACPI_SHUTDOWN, ACPI_REBOOT, or RESET")
		return
	}
	plug, ok := r.opts.Plugins.Get(body.PluginID)
	if !ok {
		writeErr(w, http.StatusNotFound, "plugin_not_found", "plugin not registered")
		return
	}
	cap, ok := plug.(sdk.PowerActionCapable)
	if !ok {
		writeErr(w, http.StatusServiceUnavailable, "power_unsupported", "plugin does not support power actions")
		return
	}
	if err := cap.PowerAction(req.Context(), body.ResourceID, body.Action); err != nil {
		writeErr(w, http.StatusBadGateway, "power_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "action": body.Action})
}

// vmAddSerialPort attaches the COM3 serial port (for the vdagent clipboard
// channel) to a VM via the plugin. Like vmPowerAction it takes
// plugin_id + resource_id so it works for a powered-OFF VM with no session —
// which is the only state in which AHV will actually attach the port.
func (r *router) vmAddSerialPort(w http.ResponseWriter, req *http.Request) {
	var body struct {
		PluginID   string `json:"plugin_id"`
		ResourceID string `json:"resource_id"`
	}
	if err := readJSON(req, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_body", err.Error())
		return
	}
	if body.PluginID == "" || body.ResourceID == "" {
		writeErr(w, http.StatusBadRequest, "missing_fields", "plugin_id and resource_id are required")
		return
	}
	plug, ok := r.opts.Plugins.Get(body.PluginID)
	if !ok {
		writeErr(w, http.StatusNotFound, "plugin_not_found", "plugin not registered")
		return
	}
	cap, ok := plug.(sdk.SerialPortCapable)
	if !ok {
		writeErr(w, http.StatusServiceUnavailable, "serial_port_unsupported",
			"plugin does not support serial-port provisioning")
		return
	}
	res, err := cap.AddSerialPort(req.Context(), body.ResourceID)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "serial_port_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// vmCreateSnapshot creates a named VM snapshot (sdk.SnapshotCapable). Takes
// plugin_id + resource_id + name so it works for any VM the endpoint manages.
func (r *router) vmCreateSnapshot(w http.ResponseWriter, req *http.Request) {
	var body struct {
		PluginID   string `json:"plugin_id"`
		ResourceID string `json:"resource_id"`
		Name       string `json:"name"`
	}
	if err := readJSON(req, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_body", err.Error())
		return
	}
	if body.PluginID == "" || body.ResourceID == "" || body.Name == "" {
		writeErr(w, http.StatusBadRequest, "missing_fields", "plugin_id, resource_id, and name are required")
		return
	}
	plug, ok := r.opts.Plugins.Get(body.PluginID)
	if !ok {
		writeErr(w, http.StatusNotFound, "plugin_not_found", "plugin not registered")
		return
	}
	cap, ok := plug.(sdk.SnapshotCapable)
	if !ok {
		writeErr(w, http.StatusServiceUnavailable, "snapshot_unsupported", "plugin does not support snapshots")
		return
	}
	info, err := cap.CreateSnapshot(req.Context(), body.ResourceID, body.Name)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "snapshot_create_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, info)
}

// vmListSnapshots lists a VM's snapshots (sdk.SnapshotCapable). Read-only:
// plugin_id + resource_id ride the query string (a GET) so the signing
// path-regex stays a fixed literal. No audit append — like vmStats / listTasks.
func (r *router) vmListSnapshots(w http.ResponseWriter, req *http.Request) {
	q := req.URL.Query()
	pluginID := q.Get("plugin_id")
	resourceID := q.Get("resource_id")
	if pluginID == "" || resourceID == "" {
		writeErr(w, http.StatusBadRequest, "missing_fields", "plugin_id and resource_id are required")
		return
	}
	plug, ok := r.opts.Plugins.Get(pluginID)
	if !ok {
		writeErr(w, http.StatusNotFound, "plugin_not_found", "plugin not registered")
		return
	}
	cap, ok := plug.(sdk.SnapshotCapable)
	if !ok {
		writeErr(w, http.StatusServiceUnavailable, "snapshot_unsupported", "plugin does not support snapshots")
		return
	}
	snaps, err := cap.ListSnapshots(req.Context(), resourceID)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "snapshot_list_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"snapshots": snaps})
}

// vmRestoreSnapshot reverts a VM to a snapshot (sdk.SnapshotCapable).
// DESTRUCTIVE — the renderer's mandatory confirmation gates this; the runtime
// records a durable audit entry (UUIDs only, no plaintext) for every attempt,
// mirroring how the catalog mount/eject ops audit a VM-affecting change.
func (r *router) vmRestoreSnapshot(w http.ResponseWriter, req *http.Request) {
	var body struct {
		PluginID   string `json:"plugin_id"`
		ResourceID string `json:"resource_id"`
		SnapshotID string `json:"snapshot_id"`
	}
	if err := readJSON(req, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_body", err.Error())
		return
	}
	if body.PluginID == "" || body.ResourceID == "" || body.SnapshotID == "" {
		writeErr(w, http.StatusBadRequest, "missing_fields", "plugin_id, resource_id, and snapshot_id are required")
		return
	}
	plug, ok := r.opts.Plugins.Get(body.PluginID)
	if !ok {
		writeErr(w, http.StatusNotFound, "plugin_not_found", "plugin not registered")
		return
	}
	cap, ok := plug.(sdk.SnapshotCapable)
	if !ok {
		writeErr(w, http.StatusServiceUnavailable, "snapshot_unsupported", "plugin does not support snapshots")
		return
	}
	err := cap.RestoreSnapshot(req.Context(), body.ResourceID, body.SnapshotID)
	if err != nil {
		r.opts.Audit.Append(audit.ActionVMSnapshotRestore, audit.Record{
			PluginID: body.PluginID,
			Result:   "error",
			Reason:   err.Error(),
			Extra:    map[string]any{"resource_id": body.ResourceID, "snapshot_id": body.SnapshotID},
		})
		writeErr(w, http.StatusBadGateway, "snapshot_restore_failed", err.Error())
		return
	}
	r.opts.Audit.Append(audit.ActionVMSnapshotRestore, audit.Record{
		PluginID: body.PluginID,
		Extra:    map[string]any{"resource_id": body.ResourceID, "snapshot_id": body.SnapshotID},
	})
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
}

// vmStats returns a live VM utilization sample (sdk.StatsCapable). Read-only:
// plugin_id + resource_id ride the query string (a GET) so the signing
// path-regex stays a fixed literal, and there is NO audit append — this is a
// high-frequency poll, mirroring listSessions / listResources. An upstream
// RBAC denial (HTTP 401/403) maps to 403 rbac_denied so the renderer can show
// targeted guidance; any other plugin failure is 502 stats_failed.
func (r *router) vmStats(w http.ResponseWriter, req *http.Request) {
	q := req.URL.Query()
	pluginID := q.Get("plugin_id")
	resourceID := q.Get("resource_id")
	if pluginID == "" || resourceID == "" {
		writeErr(w, http.StatusBadRequest, "missing_fields", "plugin_id and resource_id are required")
		return
	}
	plug, ok := r.opts.Plugins.Get(pluginID)
	if !ok {
		writeErr(w, http.StatusNotFound, "plugin_not_found", "plugin not registered")
		return
	}
	cap, ok := plug.(sdk.StatsCapable)
	if !ok {
		writeErr(w, http.StatusServiceUnavailable, "stats_unsupported",
			"plugin does not report VM stats")
		return
	}
	stats, err := cap.GetVMStats(req.Context(), resourceID)
	if err != nil {
		if isUpstreamUnauthorized(err) {
			writeErr(w, http.StatusForbidden, "rbac_denied", err.Error())
			return
		}
		writeErr(w, http.StatusBadGateway, "stats_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"stats": stats})
}

// isUpstreamUnauthorized reports whether a plugin error carries an upstream
// HTTP 401/403. The plugin's *api.APIError is internal to the AHV plugin and
// cannot be imported here, but its Error() string carries a stable "HTTP <code>"
// token (see plugins/ahv/internal/api APIError.Error), so we match on that —
// the same coarse classification the stats poll needs to offer re-auth vs a
// generic failure.
func isUpstreamUnauthorized(err error) bool {
	s := err.Error()
	return strings.Contains(s, "HTTP 401") || strings.Contains(s, "HTTP 403")
}

// listTasks aggregates the tracked long-running tasks across every registered
// plugin that implements sdk.TaskCapable, newest-first, for the renderer's
// Tasks tray. Read-only: it DISPLAYS in-flight ops, it never triggers them. A
// runtime with no task-capable plugin returns 503 task_unsupported (mirrors
// endpointHealth's capability-probe shape).
func (r *router) listTasks(w http.ResponseWriter, req *http.Request) {
	out := make([]sdk.TaskInfo, 0)
	supported := false
	for _, summary := range r.opts.Plugins.Summaries() {
		id, _ := summary["id"].(string)
		if id == "" {
			continue
		}
		plug, ok := r.opts.Plugins.Get(id)
		if !ok {
			continue
		}
		cap, ok := plug.(sdk.TaskCapable)
		if !ok {
			continue
		}
		supported = true
		tasks, err := cap.ListTasks(req.Context())
		if err != nil {
			// One plugin's listing failure must not blank the whole tray —
			// log-and-skip, return what the others have.
			r.log.Warn("list tasks failed", "plugin", id, "err", err)
			continue
		}
		out = append(out, tasks...)
	}
	if !supported {
		writeErr(w, http.StatusServiceUnavailable, "task_unsupported",
			"no plugin reports long-running tasks")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"tasks": out})
}

// getTask refreshes ONE tracked task by handle. The handle is opaque and plugin-
// minted, so the runtime asks each task-capable plugin in turn; the first that
// knows the handle answers. 404 task_not_found when no plugin recognizes it (the
// tray drops the row rather than erroring), 503 task_unsupported when no plugin
// tracks tasks at all.
func (r *router) getTask(w http.ResponseWriter, req *http.Request) {
	id := req.PathValue("id")
	if id == "" {
		writeErr(w, http.StatusBadRequest, "missing_id", "task id is required")
		return
	}
	supported := false
	for _, summary := range r.opts.Plugins.Summaries() {
		pid, _ := summary["id"].(string)
		if pid == "" {
			continue
		}
		plug, ok := r.opts.Plugins.Get(pid)
		if !ok {
			continue
		}
		cap, ok := plug.(sdk.TaskCapable)
		if !ok {
			continue
		}
		supported = true
		task, err := cap.GetTask(req.Context(), id)
		if err != nil {
			continue // unknown to this plugin — try the next
		}
		writeJSON(w, http.StatusOK, map[string]any{"task": task})
		return
	}
	if !supported {
		writeErr(w, http.StatusServiceUnavailable, "task_unsupported",
			"no plugin reports long-running tasks")
		return
	}
	writeErr(w, http.StatusNotFound, "task_not_found", "unknown task: "+id)
}

// agentMountISORequest is the POST body the renderer sends — the
// session ID identifying the active VM the bridge ISO should be
// attached to, plus the optional power-cycle consent flag.
type agentMountISORequest struct {
	SessionID string `json:"session_id"`
	// PowerCycle threads the operator's EXPLICIT consent to an orchestrated
	// power cycle (sdk.ISOMountOptions.PowerCycle) through to the plugin.
	// Optional; absent means false.
	PowerCycle bool `json:"power_cycle"`
}

// agentMountISO uploads the bundled agent installer ISO to the
// cluster's Image Service (no-op if already present), attaches it as a
// CD-ROM to the session's VM, and surfaces the resulting image UUID +
// drive letter hint back to the renderer. On success, an audit record
// is appended with the image UUID and a `reused` flag indicating
// whether the upload was actually performed or short-circuited; the ISO
// contents themselves are NEVER recorded.
func (r *router) agentMountISO(w http.ResponseWriter, req *http.Request) {
	var body agentMountISORequest
	if err := readJSON(req, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_body", err.Error())
		return
	}
	if body.SessionID == "" {
		writeErr(w, http.StatusBadRequest, "missing_session_id",
			"session_id is required")
		return
	}
	r.log.Info("bridge mount-iso request received", "session", body.SessionID)
	sess, ok := r.opts.Plugins.GetSession(body.SessionID)
	if !ok {
		r.log.Warn("bridge mount-iso: session not found", "session", body.SessionID)
		writeErr(w, http.StatusNotFound, "session_not_found",
			"unknown session: "+body.SessionID)
		return
	}
	plug, ok := r.opts.Plugins.Get(sess.PluginID)
	if !ok {
		r.log.Warn("bridge mount-iso: plugin not found", "plugin", sess.PluginID)
		writeErr(w, http.StatusNotFound, "plugin_not_found",
			"session's plugin is no longer registered: "+sess.PluginID)
		return
	}
	cap, ok := plug.(sdk.ISOMountCapable)
	if !ok {
		r.log.Warn("bridge mount-iso: plugin does not implement ISOMountCapable",
			"plugin", sess.PluginID)
		writeErr(w, http.StatusServiceUnavailable, "iso_mount_unsupported",
			"plugin "+sess.PluginID+" does not support installer ISO mount")
		return
	}
	r.log.Info("bridge mount-iso: invoking plugin",
		"plugin", sess.PluginID, "session", body.SessionID,
		"power_cycle", body.PowerCycle)
	res, err := cap.MountAgentISO(req.Context(), body.SessionID,
		sdk.ISOMountOptions{PowerCycle: body.PowerCycle})
	if err != nil {
		// 502 is the right bucket: the plugin's upstream (Prism
		// Image Service) failed. We deliberately surface the
		// plugin-side error message verbatim — the renderer's
		// install popover displays it inline so the operator can
		// see what Prism reported without having to crack the
		// runtime log.
		r.opts.Audit.Append(audit.ActionBridgeInstall, audit.Record{
			SessionID: body.SessionID,
			PluginID:  sess.PluginID,
			Result:    "error",
			Reason:    err.Error(),
			Extra: map[string]any{
				"power_cycle": body.PowerCycle,
			},
		})
		// Typed mount outcomes keep their wire code so the renderer can map
		// them to specific guidance instead of the generic failure line.
		code := "iso_mount_failed"
		if errors.Is(err, sdk.ErrImagePublishRequired) {
			code = "image_publish_required"
		}
		writeErr(w, http.StatusBadGateway, code, err.Error())
		return
	}
	// Audit BEFORE the response so an operator-visible success is
	// always preceded by a durable audit record (a writeJSON failure
	// after this point would lose the response but the audit entry
	// is already on disk).
	r.opts.Audit.Append(audit.ActionBridgeInstall, audit.Record{
		SessionID: body.SessionID,
		PluginID:  sess.PluginID,
		Extra: map[string]any{
			"image_uuid":  res.ImageUUID,
			"reused":      res.Reused,
			"power_cycle": body.PowerCycle,
			"serial_note": res.SerialNote,
		},
	})
	writeJSON(w, http.StatusOK, res)
}

// ---- arbitrary catalog ISO mount / eject ----

// catalogPlugin resolves a session_id to the plugin behind it and asserts the
// arbitrary-ISO catalog capability, writing the right error response and
// returning ok=false when anything is missing. Shared by the three catalog
// handlers (list / mount / eject), which all resolve the session the same way
// as agentMountISO.
func (r *router) catalogPlugin(w http.ResponseWriter, sessionID string) (sdk.ImageCatalogCapable, bool) {
	if sessionID == "" {
		writeErr(w, http.StatusBadRequest, "missing_session_id", "session_id is required")
		return nil, false
	}
	sess, ok := r.opts.Plugins.GetSession(sessionID)
	if !ok {
		writeErr(w, http.StatusNotFound, "session_not_found", "unknown session: "+sessionID)
		return nil, false
	}
	plug, ok := r.opts.Plugins.Get(sess.PluginID)
	if !ok {
		writeErr(w, http.StatusNotFound, "plugin_not_found",
			"session's plugin is no longer registered: "+sess.PluginID)
		return nil, false
	}
	cap, ok := plug.(sdk.ImageCatalogCapable)
	if !ok {
		writeErr(w, http.StatusServiceUnavailable, "iso_mount_unsupported",
			"plugin "+sess.PluginID+" does not support catalog ISO mount")
		return nil, false
	}
	return cap, true
}

// listISOImages returns the mountable ISO images in the session endpoint's
// catalog. session_id rides the query string (a GET) so the signing path-regex
// stays the fixed literal /v1/agent/images.
func (r *router) listISOImages(w http.ResponseWriter, req *http.Request) {
	sessionID := req.URL.Query().Get("session_id")
	cap, ok := r.catalogPlugin(w, sessionID)
	if !ok {
		return
	}
	images, err := cap.ListISOImages(req.Context(), sessionID)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "iso_catalog_failed", err.Error())
		return
	}
	// Never null on the wire — an empty catalog is [], not null.
	if images == nil {
		images = []sdk.CatalogImage{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"images": images})
}

// mountISO attaches a chosen catalog image into the session VM's CD-ROM.
func (r *router) mountISO(w http.ResponseWriter, req *http.Request) {
	var body struct {
		SessionID string `json:"session_id"`
		ImageUUID string `json:"image_uuid"`
	}
	if err := readJSON(req, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_body", err.Error())
		return
	}
	if body.ImageUUID == "" {
		writeErr(w, http.StatusBadRequest, "missing_image_uuid", "image_uuid is required")
		return
	}
	cap, ok := r.catalogPlugin(w, body.SessionID)
	if !ok {
		return
	}
	res, err := cap.MountISO(req.Context(), body.SessionID, body.ImageUUID)
	if err != nil {
		r.opts.Audit.Append(audit.ActionBridgeInstall, audit.Record{
			SessionID: body.SessionID,
			Result:    "error",
			Reason:    err.Error(),
			Extra:     map[string]any{"op": "mount", "image_uuid": body.ImageUUID},
		})
		writeErr(w, http.StatusBadGateway, "iso_mount_failed", err.Error())
		return
	}
	r.opts.Audit.Append(audit.ActionBridgeInstall, audit.Record{
		SessionID: body.SessionID,
		Extra: map[string]any{
			"op":         "mount",
			"image_uuid": res.ImageUUID,
			"reused":     res.Reused,
		},
	})
	writeJSON(w, http.StatusOK, res)
}

// ejectISO clears the media from the session VM's first CD-ROM drive.
func (r *router) ejectISO(w http.ResponseWriter, req *http.Request) {
	var body struct {
		SessionID string `json:"session_id"`
	}
	if err := readJSON(req, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_body", err.Error())
		return
	}
	cap, ok := r.catalogPlugin(w, body.SessionID)
	if !ok {
		return
	}
	res, err := cap.EjectISO(req.Context(), body.SessionID)
	if err != nil {
		r.opts.Audit.Append(audit.ActionBridgeInstall, audit.Record{
			SessionID: body.SessionID,
			Result:    "error",
			Reason:    err.Error(),
			Extra:     map[string]any{"op": "eject"},
		})
		writeErr(w, http.StatusBadGateway, "iso_eject_failed", err.Error())
		return
	}
	r.opts.Audit.Append(audit.ActionBridgeInstall, audit.Record{
		SessionID: body.SessionID,
		Extra:     map[string]any{"op": "eject", "ejected": res.Ejected},
	})
	writeJSON(w, http.StatusOK, res)
}

// ---- presence ----

var presenceUpgrader = websocket.Upgrader{ReadBufferSize: 4096, WriteBufferSize: 4096, CheckOrigin: func(r *http.Request) bool { return wsOriginIsLocal(r.Header.Get("Origin")) }, Subprotocols: []string{"hc.presence.v1"}, EnableCompression: false}

// wsOriginIsLocal mirrors proxy.originIsLocal: defense-in-depth against a
// remote website attempting cross-origin/DNS-rebinding access to the
// loopback runtime. The upgrade is HMAC-authenticated regardless, so we
// only reject clearly-remote http(s) origins and allow native/empty/file://
// origins used by the Electron shell.
func wsOriginIsLocal(origin string) bool {
	if origin == "" || origin == "null" {
		return true
	}
	rest, ok := strings.CutPrefix(origin, "http://")
	if !ok {
		rest, ok = strings.CutPrefix(origin, "https://")
	}
	if !ok {
		return true
	}
	if i := strings.IndexByte(rest, '/'); i >= 0 {
		rest = rest[:i]
	}
	host := rest
	if strings.HasPrefix(host, "[") {
		if end := strings.IndexByte(host, ']'); end >= 0 {
			host = host[:end+1]
		}
	} else if i := strings.LastIndexByte(host, ':'); i >= 0 {
		host = host[:i]
	}
	return host == "127.0.0.1" || host == "localhost" || host == "[::1]" || host == "::1"
}

func (r *router) presenceWS(w http.ResponseWriter, req *http.Request) {
	if r.opts.Presence == nil || !r.presenceEnabled() {
		writeErr(w, http.StatusServiceUnavailable, "presence_disabled", "presence features are not enabled")
		return
	}
	sessionID := req.URL.Query().Get("session_id")
	vmUUID := req.URL.Query().Get("vm_uuid")
	if sessionID == "" || vmUUID == "" {
		writeErr(w, http.StatusBadRequest, "missing_params", "session_id and vm_uuid required")
		return
	}
	conn, err := presenceUpgrader.Upgrade(w, req, nil)
	if err != nil {
		r.log.Warn("presence ws upgrade failed", "err", err)
		return
	}
	defer func() { _ = conn.Close() }()
	conn.SetReadLimit(16 * 1024)
	sub := r.opts.Presence.Subscribe(sessionID, vmUUID)
	defer func() { r.opts.Presence.Unsubscribe(sub); r.opts.Presence.RemovePeer(sessionID, vmUUID) }()
	r.log.Info("presence ws connected", "session", sessionID, "vm", vmUUID)
	r.opts.Audit.Append(audit.ActionPresenceJoin, audit.Record{SessionID: sessionID, VMID: vmUUID})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			_, data, err := conn.ReadMessage()
			if err != nil {
				return
			}
			r.opts.Presence.HandleMessage(data, sub)
		}
	}()
	for {
		select {
		case msg, ok := <-sub.Send():
			if !ok {
				return
			}
			_ = conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
			if err := conn.WriteMessage(websocket.TextMessage, msg); err != nil {
				return
			}
		case <-done:
			r.opts.Audit.Append(audit.ActionPresenceLeave, audit.Record{SessionID: sessionID, VMID: vmUUID})
			return
		}
	}
}

func (r *router) presencePeers(w http.ResponseWriter, req *http.Request) {
	if r.opts.Presence == nil || !r.presenceEnabled() {
		writeErr(w, http.StatusServiceUnavailable, "presence_disabled", "presence features are not enabled")
		return
	}
	vmUUID := req.URL.Query().Get("vm_uuid")
	if vmUUID == "" {
		writeErr(w, http.StatusBadRequest, "missing_params", "vm_uuid required")
		return
	}
	peers := r.opts.Presence.RoomPeers(vmUUID)
	if peers == nil {
		peers = []presence.Peer{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"peers": peers})
}

func (r *router) listFeatures(w http.ResponseWriter, _ *http.Request) {
	cfg := r.opts.Config.Get()
	writeJSON(w, http.StatusOK, map[string]any{"features": cfg.FeatureFlags})
}

func (r *router) presenceEnabled() bool { return r.opts.Config.Get().FeatureEnabled("presence") }

// ---- helpers ----

func readJSON(r *http.Request, dst any) error {
	if r.Body == nil {
		return errors.New("empty body")
	}
	defer func() { _ = r.Body.Close() }()
	dec := json.NewDecoder(io.LimitReader(r.Body, 4*1024*1024))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	return nil
}

func atoiOr(s string, fallback int) int {
	if s == "" {
		return fallback
	}
	var n int
	for _, c := range strings.TrimSpace(s) {
		if c < '0' || c > '9' {
			return fallback
		}
		n = n*10 + int(c-'0')
	}
	return n
}
