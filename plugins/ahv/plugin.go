// Package ahv implements the AHV Remote Console plugin for Nutanix AHV via Prism
// Central. The plugin supports two authentication mechanisms:
//
//   - Local credentials — username/password verified by Prism's
//     /api/nutanix/v3/users/me. Covers local accounts, AD/LDAP, and
//     directory-backed users (Prism resolves which transparently).
//   - Embedded SSO — for clusters wired to SAML/OIDC IdPs like Okta or
//     Authentik, the desktop shell opens Prism's /console/ login page in
//     a sandboxed Electron BrowserWindow and hands the resulting session
//     cookies back to the plugin. We replay those cookies on every API
//     request and never touch the IdP ourselves.
//
// Secrets never live on disk. Prism SSO session cookies are stored
// in the OS keychain (Credential Manager / Keychain / libsecret).
package ahv

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"

	"github.com/MarcoCarvin/ahv-remote-console/apps/runtime/pkg/sdk"
	"github.com/MarcoCarvin/ahv-remote-console/plugins/ahv/internal/api"
)

const pluginID = "nutanix-ahv"

func init() {
	sdk.Register(func() sdk.Plugin { return &Plugin{} })
}

type Plugin struct {
	deps          sdk.Dependencies
	mu            sync.RWMutex
	state         map[string]*endpointState
	clients       map[string]*api.Client
	sessions      map[string]*sessionState
	cache         *resourceCache
	clusterUUIDs  map[string]consoleTarget
	stopKeepAlive context.CancelFunc
	// pendingSerial tracks VMs whose COM3 add ended PowerCycleRequired, so
	// the keep-alive tick can apply it at the next OBSERVED power-off
	// (applyDeferredSerial). Keyed endpointID/vmUUID, guarded by mu.
	// Deliberately in-memory only: a plugin restart loses the set, which is
	// acceptable because the renderer's sticky power_off_required notice and
	// the "Add serial port" button remain the durable operator-facing
	// surface — re-running either re-arms the entry.
	pendingSerial map[string]pendingSerialAdd
	// cycleOffBudget / cyclePollEvery bound the consented power cycle's
	// ACPI-shutdown wait (fields, not consts, only so tests can shrink them;
	// Init sets the production defaults).
	cycleOffBudget time.Duration
	cyclePollEvery time.Duration
	// Plugin-lifetime context + waitgroup for DETACHED background goroutines
	// (the mount-ISO GC sweep): they survive the request that spawned them but
	// are cancelled and awaited by Shutdown so authenticated Prism work never
	// outlives a believed-complete teardown (#24).
	bgCtx    context.Context
	bgCancel context.CancelFunc
	bgWG     sync.WaitGroup
	// capabilities mirrors probe/evidence-derived upstream permission state
	// per endpoint for the health payload (see capabilities.go). Guarded by
	// mu; reset at every auth-epoch boundary.
	capabilities map[string]*endpointCapabilities
	// capEpochs is the per-endpoint auth-epoch counter behind those resets:
	// probes/operations capture it at start, and a mirror write whose captured
	// value no longer matches is dropped (see capabilities.go). Guarded by mu;
	// monotonic, entries are never deleted.
	capEpochs map[string]uint64
	// tasks is the in-memory long-running-task registry backing sdk.TaskCapable
	// (the renderer's Tasks tray): power/serial/ISO ops mint a handle here and
	// finish it on return; GetTask refreshes a running handle against the real
	// upstream task. Guarded by tasksMu (a separate lock from mu so a task poll
	// never contends with the session/client hot paths). See tasks.go.
	tasksMu sync.Mutex
	tasks   map[string]*trackedTask
}

// endpointState holds credentials for one endpoint. Sensitive fields
// are []byte so we can explicitly zero them on logout/invalidate —
// Go strings are immutable and would leave cleartext copies on the
// heap waiting for GC, recoverable from a crash dump or heap snapshot
// any time before then.
type endpointState struct {
	method         sdk.AuthKind
	sessionCookies []api.SessionCookie
}

// wipe zeroes every credential-bearing field in the state. Call on
// logout, invalidateEndpoint, or any path that retires the state.
func (s *endpointState) wipe() {
	if s == nil {
		return
	}
	// sessionCookies are []SessionCookie{Name, Value string}. Best we
	// can do for strings is drop the reference and let GC eventually
	// reclaim — the cleartext lives until then. The keystore-side
	// storage is the durable copy; once we delete that, no copies
	// outlive the GC cycle.
	s.sessionCookies = nil
}

type sessionState struct {
	EndpointID  string
	ResourceID  string
	VMUUID      string
	ClusterUUID string
	OpenedAt    time.Time
}

// consoleTarget is what OpenSession needs from a VM read: the cluster UUID
// that routes the PC-side VNC WS upgrade, plus the display name stamped into
// session metadata. Cached per endpoint/VM (p.clusterUUIDs) from every real
// vms/list or GetVM payload so a console open normally skips the GetVM
// round-trip.
type consoleTarget struct {
	ClusterUUID string
	VMName      string
}

func consoleTargetKey(endpointID, vmUUID string) string {
	return endpointID + "/" + vmUUID
}

// cachedConsoleTarget returns the cached routing info for a VM. An empty
// cluster UUID is reported as a MISS: it would build the proxyClusterUuid-less
// WS URL that 404s on Prism Central, so the caller must refetch instead.
func (p *Plugin) cachedConsoleTarget(endpointID, vmUUID string) (consoleTarget, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	t, ok := p.clusterUUIDs[consoleTargetKey(endpointID, vmUUID)]
	if !ok || t.ClusterUUID == "" {
		return consoleTarget{}, false
	}
	return t, true
}

// storeConsoleTarget refuses empty cluster UUIDs so a payload missing
// cluster_reference.uuid can never clobber a previously good entry.
func (p *Plugin) storeConsoleTarget(endpointID, vmUUID string, t consoleTarget) {
	if t.ClusterUUID == "" {
		return
	}
	p.mu.Lock()
	p.clusterUUIDs[consoleTargetKey(endpointID, vmUUID)] = t
	p.mu.Unlock()
}

func (p *Plugin) dropConsoleTarget(endpointID, vmUUID string) {
	p.mu.Lock()
	delete(p.clusterUUIDs, consoleTargetKey(endpointID, vmUUID))
	p.mu.Unlock()
}

func (p *Plugin) Manifest() sdk.Manifest {
	return sdk.Manifest{
		ID:          pluginID,
		Name:        "Nutanix AHV",
		Version:     "1.0.0",
		Description: "Remote console for Nutanix AHV virtual machines via Prism Central. Supports local accounts and embedded SSO.",
		Vendor:      "The AHV Remote Console Authors",
		Capabilities: []string{
			"console.vnc",
			"console.resize",
			"console.clipboard",
			"auth.local",
			"auth.sso",
			"discovery.vms",
		},
	}
}

func (p *Plugin) Init(_ context.Context, deps sdk.Dependencies) error {
	p.deps = deps
	p.state = make(map[string]*endpointState)
	p.clients = make(map[string]*api.Client)
	p.sessions = make(map[string]*sessionState)
	p.cache = newResourceCache(30 * time.Second)
	p.clusterUUIDs = make(map[string]consoleTarget)
	p.pendingSerial = make(map[string]pendingSerialAdd)
	p.capabilities = make(map[string]*endpointCapabilities)
	p.capEpochs = make(map[string]uint64)
	p.tasks = make(map[string]*trackedTask)
	p.cycleOffBudget = acpiOffBudget
	p.cyclePollEvery = acpiOffPollEvery
	ctx, cancel := context.WithCancel(context.Background())
	p.stopKeepAlive = cancel
	p.bgCtx, p.bgCancel = context.WithCancel(context.Background())
	go p.sessionKeepAlive(ctx)
	deps.Logger.Info("ahv plugin initialized")
	return nil
}

func (p *Plugin) Shutdown(_ context.Context) error {
	if p.stopKeepAlive != nil {
		p.stopKeepAlive()
	}
	if p.bgCancel != nil {
		p.bgCancel()
	}
	// Bounded wait for detached background goroutines (mount-ISO GC) to observe
	// the cancel and exit BEFORE we close the clients below, so no authenticated
	// Prism delete outlives teardown or hits an already-closed client (#24).
	waitTimeout(&p.bgWG, 5*time.Second)
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, c := range p.clients {
		c.Close()
	}
	return nil
}

// waitTimeout waits for wg, but no longer than d. A detached background sweep
// that is mid-Prism-task can't always exit instantly on cancel, so Shutdown
// bounds the wait rather than blocking teardown indefinitely.
func waitTimeout(wg *sync.WaitGroup, d time.Duration) {
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(d):
	}
}

// keepAliveInterval is how often we ping Prism to refresh the server-side
// session cookie. It MUST be comfortably shorter than the cluster's SSO/
// session idle-validity window, or the cookie lapses in the gap between
// pings — the next API call (a VM list or a console WS upgrade) then gets a
// 401, which trips invalidateEndpoint and surfaces as "reconnecting" on every
// VM. Some clusters (esp. Authentik-fronted SSO) run very short idle windows,
// so the previous 5-minute interval lost the race constantly. 30s keeps even
// a short-lived session warm; the ping is tiny (~200 B req / ~500 B resp).
const keepAliveInterval = 30 * time.Second

// acpiOffBudget is how long the consented mount power cycle waits for the
// guest to actually reach OFF after the graceful ACPI shutdown request. A
// guest that hasn't halted by then is treated as having ignored ACPI: the
// cycle ABORTS (never a hard power-off) and the COM3 add is deferred to the
// next observed power-off. acpiOffPollEvery is the power-state poll cadence.
const (
	acpiOffBudget    = 120 * time.Second
	acpiOffPollEvery = 3 * time.Second
)

// sessionKeepAlive pings Prism's /users/me endpoint every keepAliveInterval
// for each active endpoint, refreshing the session cookie timer so Prism does
// not expire the session while the operator sits in a console (where no other
// Prism API call would occur).
func (p *Plugin) sessionKeepAlive(ctx context.Context) {
	ticker := time.NewTicker(keepAliveInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			p.keepAliveTick(ctx)
		}
	}
}

// keepAliveTick pings each active endpoint once. On an auth failure it
// proactively invalidates the endpoint so a dead session surfaces immediately
// (NUTA-07) rather than lingering until the next API call trips a 401.
func (p *Plugin) keepAliveTick(ctx context.Context) {
	p.mu.RLock()
	clients := make(map[string]*api.Client, len(p.clients))
	for id, c := range p.clients {
		clients[id] = c
	}
	p.mu.RUnlock()
	for id, c := range clients {
		// Bound each probe: the loop is serialized on one goroutine on an
		// un-timed background ctx, so a stalled probe (otherwise capped only by
		// the 60s client timeout) would eat one or two whole expiry-detection
		// windows during a network spike. 10s sits under the 30s keep-alive tick.
		pctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		err := c.VerifyAuth(pctx)
		cancel()
		if err == nil {
			p.deps.Logger.Debug("keep-alive ping OK", "endpoint", id)
			// The Layer-1 capability probes ride the keep-alive (off every
			// request hot path), at most once per auth epoch per endpoint
			// (see capabilities.go).
			if p.capabilityProbePending(id) {
				p.probeCapabilities(ctx, id, c, p.capEpoch(id))
			}
			continue
		}
		// NUTA-07: a keep-alive auth failure means the session is dead — surface
		// it immediately by invalidating, so the renderer routes back to sign-in
		// instead of staring at a live-looking console until the next API call.
		// But ONLY a 401 is "session expired". A 403 is per-resource RBAC or a
		// transient WAF/LB hiccup on the probe path — wiping the whole endpoint's
		// session on a 403 would log the operator out of a perfectly live
		// session. The WS-upgrade (OpenStream) path already treats 403 as
		// non-expiry; match that here. Anything else (network/stall/timeout) is
		// transient → Debug and keep the session.
		switch api.HTTPStatus(err) {
		case 401:
			p.deps.Logger.Warn("keep-alive ping 401 — session expired, invalidating endpoint",
				"endpoint", id)
			p.invalidateEndpoint(id)
		case 403:
			p.deps.Logger.Warn("keep-alive ping 403 — keeping session (per-resource RBAC / transient)",
				"endpoint", id)
		default:
			p.deps.Logger.Debug("keep-alive ping failed", "endpoint", id, "err", err)
		}
	}
	// Piggyback the deferred COM3 watcher on the same tick: GET-only power
	// observation plus an idempotent serial add once a pending VM is OFF.
	p.applyDeferredSerial(ctx)
}

// ----- deferred COM3 serial add (apply at next observed power-off) -----

// pendingSerialAdd identifies one VM whose COM3 add is deferred to the next
// observed power-off.
type pendingSerialAdd struct {
	EndpointID string
	VMUUID     string
}

func deferredSerialKey(endpointID, vmUUID string) string {
	return endpointID + "/" + vmUUID
}

// armDeferredSerialAdd records that the VM needs COM3 added the next time it
// is OBSERVED powered OFF. Armed whenever a serial add ends
// PowerCycleRequired (the mount flow, the standalone button, and an aborted
// consent cycle). Idempotent.
func (p *Plugin) armDeferredSerialAdd(endpointID, vmUUID string) {
	key := deferredSerialKey(endpointID, vmUUID)
	p.mu.Lock()
	_, exists := p.pendingSerial[key]
	if !exists {
		p.pendingSerial[key] = pendingSerialAdd{EndpointID: endpointID, VMUUID: vmUUID}
	}
	p.mu.Unlock()
	if !exists {
		p.deps.Logger.Info("serial add deferred — will apply at the next observed power-off",
			"endpoint", endpointID, "vm", vmUUID)
	}
}

func (p *Plugin) dropDeferredSerialAdd(key string) {
	p.mu.Lock()
	delete(p.pendingSerial, key)
	p.mu.Unlock()
}

// applyDeferredSerial processes the pending set on each keep-alive tick: for
// every pending VM, observe (GET-only) its power state; once OFF, add COM3
// via the idempotent EnsureSerialPort — which re-checks absence at apply
// time, so a port the operator added manually clears the entry with NO PUT.
//
// This path NEVER issues a power operation: it strictly waits for a
// power-off that happened for other reasons (operator action, guest
// shutdown). Per-tick work is bounded — the set is tiny (one entry per
// deferred VM) and processed sequentially, no goroutine fan-out.
func (p *Plugin) applyDeferredSerial(ctx context.Context) {
	p.mu.RLock()
	pending := make(map[string]pendingSerialAdd, len(p.pendingSerial))
	for k, v := range p.pendingSerial {
		pending[k] = v
	}
	p.mu.RUnlock()
	if len(pending) == 0 {
		return
	}
	for key, e := range pending {
		ep, ok := p.endpoint(e.EndpointID)
		if !ok {
			// Endpoint removed from config — the entry can never apply.
			p.dropDeferredSerialAdd(key)
			continue
		}
		client, err := p.clientFor(ctx, ep)
		if err != nil {
			continue // no credentials right now; retry on a later tick
		}
		vm, err := client.GetVM(ctx, e.VMUUID)
		if err != nil {
			if api.HTTPStatus(err) == 404 {
				p.dropDeferredSerialAdd(key) // VM deleted
			}
			continue
		}
		if !strings.EqualFold(vm.PowerState, "OFF") {
			continue // still running — keep watching
		}
		epoch := p.capEpoch(e.EndpointID)
		res, err := client.EnsureSerialPort(ctx, e.VMUUID, 2)
		if err != nil {
			if denies403(err, capVMUpdate) {
				// Upstream RBAC denied the spec PUT: retrying at every observed
				// power-off is pointless until roles change (a re-auth resets
				// the mirror and the operator can re-run setup). Record the
				// evidence and drop the entry.
				p.setCapability(e.EndpointID, capVMUpdate, false, epoch)
				p.deps.Logger.Warn("deferred serial add denied by upstream RBAC — dropping pending entry",
					"vm", e.VMUUID)
				p.dropDeferredSerialAdd(key)
				continue
			}
			p.deps.Logger.Warn("deferred serial add failed — will retry on a later tick",
				"vm", e.VMUUID, "err", err)
			continue
		}
		if res.AlreadyPresent || res.Added {
			if res.Added {
				p.setCapability(e.EndpointID, capVMUpdate, true, epoch)
			}
			p.deps.Logger.Info("deferred serial add applied at observed power-off",
				"vm", e.VMUUID, "already_present", res.AlreadyPresent)
			p.dropDeferredSerialAdd(key)
		}
	}
}

// ----- auth -----

type authStartBody struct {
	EndpointID string       `json:"endpoint_id"`
	Method     sdk.AuthKind `json:"method"`
	// Used when Method == "cookie". The desktop shell captured these from an
	// embedded Prism login window's isolated partition.
	Cookies []api.SessionCookie `json:"cookies,omitempty"`
}

type authStartResp struct {
	Method        sdk.AuthKind `json:"method"`
	Authenticated bool         `json:"authenticated"`
}

func (p *Plugin) AuthStart(ctx context.Context, raw json.RawMessage) (any, error) {
	var b authStartBody
	if err := json.Unmarshal(raw, &b); err != nil {
		return nil, fmt.Errorf("decode body: %w", err)
	}
	if b.Method == "" {
		return nil, errors.New(`method required ("cookie")`)
	}
	ep, ok := p.endpoint(b.EndpointID)
	if !ok {
		return nil, fmt.Errorf("unknown endpoint: %s", b.EndpointID)
	}
	if !ep.Allows(b.Method) {
		return nil, fmt.Errorf("auth method %q is not enabled on endpoint %s", b.Method, b.EndpointID)
	}

	switch b.Method {
	case sdk.AuthKindCookie:
		return p.authStartCookie(ctx, ep, b)
	default:
		return nil, fmt.Errorf("unsupported auth method: %s", b.Method)
	}
}

// authStartCookie consumes session cookies captured by the desktop shell
// from an embedded Prism login window, probes Prism with them, and persists
// them in the OS keychain.
func (p *Plugin) authStartCookie(ctx context.Context, ep sdk.EndpointRef, b authStartBody) (any, error) {
	if len(b.Cookies) == 0 {
		return nil, errors.New("at least one session cookie is required")
	}
	probe, err := p.newCookieClient(ep, b.Cookies)
	if err != nil {
		return nil, err
	}
	if err := probe.VerifyAuth(ctx); err != nil {
		probe.Close()
		return nil, fmt.Errorf("session not authenticated: %w", err)
	}
	// Persist the POST-rotation cookies, not the captured pre-rotation set.
	// VerifyAuth's users/me triggers Prism Mercury's first session rotation; the
	// probe jar now holds the rotated (valid) cookies, while b.Cookies are the
	// captured pre-rotation set that rotation may have already invalidated.
	// Writing b.Cookies here would clobber the rotated entry onRotate just
	// persisted (and seed endpointState with the dead cookie) — so a later cold
	// rehydrate (e.g. after a runtime restart) would 401. Snapshot falls back to
	// b.Cookies only if the client somehow has no jar.
	cookies := probe.SessionCookies()
	if len(cookies) == 0 {
		cookies = b.Cookies
	}
	encoded, err := json.Marshal(cookies)
	if err != nil {
		probe.Close()
		return nil, fmt.Errorf("encode cookies: %w", err)
	}
	if err := p.deps.Keystore.Set(keySSOCookies(ep.ID), encoded); err != nil {
		probe.Close()
		return nil, fmt.Errorf("persist cookies: %w", err)
	}
	p.replaceClient(ep.ID, probe, &endpointState{
		method:         sdk.AuthKindCookie,
		sessionCookies: cookies,
	})
	// Fresh auth epoch: run the Layer-1 capability probes now, detached — the
	// login response must not wait on a catalog walk (see capabilities.go).
	p.kickCapabilityProbe(ep.ID, probe)
	return authStartResp{Method: sdk.AuthKindCookie, Authenticated: true}, nil
}

// AuthComplete exists for compatibility with the SDK contract but is a no-op
// for the AHV plugin: the cookie (embedded SSO) flow completes in AuthStart.
// A future plugin that does an IdP redirect would use this.
func (p *Plugin) AuthComplete(_ context.Context, _ json.RawMessage) (any, error) {
	return nil, errors.New("AHV plugin does not require AuthComplete; both methods finish in AuthStart")
}

// AuthRefresh is currently a no-op. The Prism session cookie is refreshed by
// re-running the embedded SSO flow when the user hits a 401.
func (p *Plugin) AuthRefresh(_ context.Context) error { return nil }

func (p *Plugin) Logout(ctx context.Context) error {
	p.mu.Lock()
	// Iterate over every endpoint we know from config, not just the
	// ones currently in p.state — entries can be evicted by
	// invalidateEndpoint while the keystore copy survives, and
	// previously Logout missed those.
	knownIDs := make(map[string]struct{})
	for _, ep := range p.deps.Config.Endpoints() {
		knownIDs[ep.ID] = struct{}{}
	}
	for id := range p.state {
		knownIDs[id] = struct{}{}
	}
	for id := range knownIDs {
		_ = p.deps.Keystore.Delete(keySSOCookies(id))
	}
	// Zero every cached credential before dropping the state map.
	for _, st := range p.state {
		st.wipe()
	}
	p.state = make(map[string]*endpointState)
	for _, c := range p.clients {
		c.Close()
	}
	// Logout ends every auth epoch — bump each endpoint's epoch so in-flight
	// probes/operations from the old credentials can no longer write, then
	// drop all capability evidence with it.
	for id := range p.clients {
		p.bumpCapEpochLocked(id)
	}
	for id := range p.capabilities {
		if _, ok := p.clients[id]; !ok {
			p.bumpCapEpochLocked(id)
		}
	}
	p.clients = make(map[string]*api.Client)
	p.capabilities = make(map[string]*endpointCapabilities)
	p.mu.Unlock()
	_ = ctx
	return nil
}

// ----- resources -----

// WhoAmI implements sdk.IdentityCapable: reports which Prism user the
// endpoint's stored session authenticates as (/users/me), so the lobby
// account menu shows the operator, not the cluster.
func (p *Plugin) WhoAmI(ctx context.Context, endpointID string) (sdk.Identity, error) {
	ep, ok := p.endpoint(endpointID)
	if !ok {
		return sdk.Identity{}, fmt.Errorf("unknown endpoint: %s", endpointID)
	}
	client, err := p.clientFor(ctx, ep)
	if err != nil {
		return sdk.Identity{}, err
	}
	self, err := client.UsersMe(ctx)
	if err != nil {
		return sdk.Identity{}, err
	}
	return sdk.Identity{Username: self.Username, DisplayName: self.DisplayName}, nil
}

func (p *Plugin) ListResources(ctx context.Context, q sdk.ListQuery) (sdk.ListResult, error) {
	endpoints := p.deps.Config.Endpoints()
	// Always return a non-nil slice so the renderer never sees null.
	out := sdk.ListResult{Page: q.Page, PageSize: q.PageSize, Resources: []sdk.Resource{}}

	relevant := 0
	authFailed := 0
	var lastAuthErr error

	for _, ep := range endpoints {
		if ep.PluginID != pluginID {
			continue
		}
		relevant++
		if cached, ok := p.cache.get(ep.ID); ok && q.Search == "" {
			out.Resources = append(out.Resources, cached...)
			continue
		}
		client, err := p.clientFor(ctx, ep)
		if err != nil {
			// "no credentials" is the EXPECTED state for an endpoint the operator
			// has not signed into — ListResources fans out across ALL endpoints
			// (the lobby lists VMs cluster-wide), so an unauthenticated endpoint
			// shows up on every refresh. Log that at debug; reserve WARN for
			// genuine init failures (TLS pin mismatch, network) worth surfacing.
			if strings.Contains(err.Error(), "no credentials available") {
				p.deps.Logger.Debug("skipping endpoint without credentials", "endpoint", ep.ID)
			} else {
				p.deps.Logger.Warn("client init failed", "endpoint", ep.ID, "err", err)
			}
			authFailed++
			lastAuthErr = err
			continue
		}
		vms, err := client.ListVMs(ctx, api.ListVMsOptions{
			Search: q.Search,
			Offset: q.Page * q.PageSize,
			Length: q.PageSize,
		})
		if err != nil {
			// Treat 401/403 as an expired session: drop the cached client and
			// the persisted credentials so the renderer is forced back to
			// sign-in. Counting it as authFailed makes the loop's final check
			// surface "no credentials" to the caller.
			if api.IsUnauthorized(err) {
				p.deps.Logger.Warn("upstream session expired", "endpoint", ep.ID)
				p.invalidateEndpoint(ep.ID)
				authFailed++
				lastAuthErr = err
				continue
			}
			p.deps.Logger.Warn("list VMs failed", "endpoint", ep.ID, "err", err)
			continue
		}
		page := make([]sdk.Resource, 0, len(vms))
		for _, vm := range vms {
			// The list payload already carries status.cluster_reference.uuid;
			// remembering it here is what lets OpenSession skip its GetVM
			// round-trip for VMs the lobby has listed.
			p.storeConsoleTarget(ep.ID, vm.UUID, consoleTarget{ClusterUUID: vm.ClusterUUID, VMName: vm.Name})
			page = append(page, sdk.Resource{
				ID:          fmt.Sprintf("%s/%s", ep.ID, vm.UUID),
				Label:       vm.Name,
				Status:      vm.PowerState,
				Description: vm.Description,
				Cluster:     vm.ClusterName,
				IP:          vm.PrimaryIP,
				Tags: map[string]string{
					"endpoint": ep.Label,
					"uuid":     vm.UUID,
				},
			})
		}
		p.cache.set(ep.ID, page)
		out.Resources = append(out.Resources, page...)
	}
	out.Total = len(out.Resources)

	// If every relevant endpoint failed auth (no credentials, expired
	// session, ...), bubble the error so the renderer routes to sign-in
	// instead of presenting an empty VM list as if everything succeeded.
	if relevant > 0 && authFailed == relevant {
		if lastAuthErr != nil {
			return out, fmt.Errorf("no credentials: %w", lastAuthErr)
		}
		return out, errors.New("no credentials available — sign in first")
	}
	return out, nil
}

func (p *Plugin) RefreshResources(_ context.Context) error {
	p.cache.purge()
	return nil
}

// ----- health -----

// healthProbeTimeout bounds the single reachability probe so an unreachable
// cluster can't hang the request.
const healthProbeTimeout = 5 * time.Second

// EndpointHealth implements sdk.HealthCapable: an UNAUTHENTICATED reachability
// ping for the cluster-map status dot. It builds a throwaway bare client (no
// stored cookies) honoring the endpoint's TLS pinning and does one short GET;
// any HTTP response means the host is up, only a transport failure is "offline".
// No credentials are sent and no body is read, so probing every node leaks
// nothing and doesn't depend on being signed in — auth is handled later by the
// Connect flow. Returns reachability + round-trip latency.
func (p *Plugin) EndpointHealth(ctx context.Context, endpointID string) (sdk.EndpointHealth, error) {
	ep, ok := p.endpoint(endpointID)
	if !ok {
		return sdk.EndpointHealth{}, fmt.Errorf("unknown endpoint: %s", endpointID)
	}
	h := sdk.EndpointHealth{EndpointID: ep.ID, Status: "offline"}
	// The capability mirror rides the same payload. It is locally cached
	// probe/evidence state (see capabilities.go) — the reachability ping
	// below stays unauthenticated and sends nothing extra.
	h.Capabilities = p.capabilitiesSnapshot(ep.ID)

	proxyURL, noProxy := p.proxyConfig()
	client, err := api.NewClient(api.ClientOptions{
		BaseURL:   ep.URL,
		TLSConfig: p.tlsConfigFor(ep),
		Logger:    p.deps.Logger,
		ProxyURL:  proxyURL,
		NoProxy:   noProxy,
	})
	if err != nil {
		return h, nil
	}
	defer client.Close()

	pctx, cancel := context.WithTimeout(ctx, healthProbeTimeout)
	defer cancel()
	if reachable, rtt := client.Reachable(pctx); reachable {
		h.Status = "healthy"
		ms := int(rtt.Milliseconds())
		h.RTTMs = &ms
	}
	return h, nil
}

// ----- session lifecycle -----

func (p *Plugin) OpenSession(ctx context.Context, resourceID string) (sdk.SessionInfo, error) {
	endpointID, vmUUID, err := splitResourceID(resourceID)
	if err != nil {
		return sdk.SessionInfo{}, err
	}
	ep, ok := p.endpoint(endpointID)
	if !ok {
		return sdk.SessionInfo{}, fmt.Errorf("unknown endpoint: %s", endpointID)
	}
	client, err := p.clientFor(ctx, ep)
	if err != nil {
		return sdk.SessionInfo{}, err
	}
	// Look up the cluster UUID. Prism Central is multi-cluster and the
	// VNC WS endpoint requires both UUIDs in the query string to route
	// the upgrade to the right hypervisor; without them the proxy returns
	// a Tomcat 404. The vms/list payload the lobby fetched already carried
	// the value, so this is normally a cache hit and the open pays no REST
	// round-trip; GetVM remains as the cold-path fallback.
	target, ok := p.cachedConsoleTarget(endpointID, vmUUID)
	if !ok {
		vm, err := client.GetVM(ctx, vmUUID)
		if err != nil {
			return sdk.SessionInfo{}, fmt.Errorf("fetch VM metadata: %w", err)
		}
		target = consoleTarget{ClusterUUID: vm.ClusterUUID, VMName: vm.Name}
		p.storeConsoleTarget(endpointID, vmUUID, target)
	}
	// COM3 serial port provisioning is handled by the "Install
	// prerequisites" action (MountAgentISO) so the operator gets
	// clear feedback on what was done and whether a reboot is needed.
	//
	// NUTA-06 (GO-04): the per-open AcquireConsoleProxyToken round-trip and
	// the unconditional UsersMe / FetchConsolePage diagnostic chain were
	// removed. The proxy token was never used on the dial (auth is cookie
	// replay only), and the diagnostics added latency + log-hygiene risk on
	// every console open. Only the GetVM fallback above (for cluster_uuid,
	// on a cluster-UUID cache miss) remains.

	// Generate the session ID here so the plugin's session map and the
	// host-side session wrapper agree. If we left info.ID empty the host
	// would mint a fresh UUID for the wire response while p.sessions stayed
	// keyed by "", and proxy.OpenStream would later look up the host UUID
	// in p.sessions and miss.
	sid := uuid.NewString()
	info := sdk.SessionInfo{
		ID:         sid,
		ResourceID: resourceID,
		PluginID:   pluginID,
		OpenedAt:   time.Now().UTC(),
		Capabilities: []string{
			"vnc.rfb38",
			"vnc.extended_clipboard",
			"vnc.extended_desktop_size",
		},
		Metadata: map[string]string{
			"endpoint_id":  endpointID,
			"vm_uuid":      vmUUID,
			"cluster_uuid": target.ClusterUUID,
			"vm_name":      target.VMName,
		},
	}
	p.mu.Lock()
	p.sessions[sid] = &sessionState{
		EndpointID:  endpointID,
		ResourceID:  resourceID,
		VMUUID:      vmUUID,
		ClusterUUID: target.ClusterUUID,
		OpenedAt:    info.OpenedAt,
	}
	p.mu.Unlock()
	return info, nil
}

// OpenStream dials the hypervisor-side VNC WebSocket directly. There is no
// API-level "ticket" exchange in Prism — the WS path is stable and auth is
// carried by replaying our Prism session cookies on the upgrade request
// (the Cookie header; see api.WSDialHeader).
func (p *Plugin) OpenStream(ctx context.Context, sessionID string) (*websocket.Conn, error) {
	// Snapshot the routing fields under the lock: the preflight below may
	// rewrite st.ClusterUUID concurrently with a later re-dial.
	p.mu.RLock()
	st, ok := p.sessions[sessionID]
	var endpointID, vmUUID, clusterUUID string
	if ok {
		endpointID, vmUUID, clusterUUID = st.EndpointID, st.VMUUID, st.ClusterUUID
	}
	p.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("unknown session: %s", sessionID)
	}
	ep, ok := p.endpoint(endpointID)
	if !ok {
		return nil, fmt.Errorf("endpoint vanished: %s", endpointID)
	}

	client, err := p.clientFor(ctx, ep)
	if err != nil {
		return nil, err
	}
	header, err := client.WSDialHeader(ctx)
	if err != nil {
		return nil, fmt.Errorf("build WS auth header: %w", err)
	}
	wsURL := client.ConsoleWebSocketURL(vmUUID, clusterUUID)

	dialer := &websocket.Dialer{
		HandshakeTimeout: 20 * time.Second,
		// Intentionally NO Subprotocols. The HAR capture against PC 7.5.1.4
		// shows Prism's web UI sends no Sec-WebSocket-Protocol on the
		// upgrade; offering one ("binary") makes PC's WS handler 403 the
		// request as a non-canonical client.
		TLSClientConfig: p.tlsConfigFor(ep),

		// VNC is already heavily compressed via Tight (zlib + JPEG).
		// Enabling WebSocket permessage-deflate on top adds significant CPU
		// overhead, increases latency via the flate window buffering, and
		// often INCREASES payload size on already-entropic data. Disable it.
		EnableCompression: false,

		// The default Gorilla buffers are 4KB. During video playback, a single
		// Framebuffer Update can be hundreds of KB. A 1MB buffer allows the
		// frame to be parsed and written to TCP in a single syscall rather than
		// fragmenting into hundreds, drastically reducing CPU/context-switch
		// overhead and smoothing out downstream latency.
		ReadBufferSize:  1024 * 1024,
		WriteBufferSize: 1024 * 1024,

		// OBS-02: route the WS upgrade through the same egress proxy / air-gap
		// config the REST transport uses (defaults to ProxyFromEnvironment), so
		// the console works in proxied/air-gapped deployments.
		Proxy: client.Proxy(),
	}
	conn, resp, err := dialer.DialContext(ctx, wsURL, header)
	if err != nil {
		status := 0
		var body string
		bodyLen := 0
		if resp != nil {
			status = resp.StatusCode
			if resp.Body != nil {
				buf := make([]byte, 1024)
				n, _ := resp.Body.Read(buf)
				body = strings.TrimSpace(string(buf[:n]))
				bodyLen = n
				_ = resp.Body.Close()
			}
		}
		// IMPORTANT: do NOT log the upstream rejection body. Prism's WAF
		// echoes request headers (including Cookie / Authorization values)
		// back in 4xx bodies, and the slog redactor matches by attribute
		// key — "body" is not on the list so the cleartext credential
		// would land in logs.
		p.deps.Logger.Warn("upstream WS upgrade rejected",
			"endpoint", ep.ID,
			"url", wsURL,
			"status", status,
			"body_bytes", bodyLen,
			"err", err.Error(),
		)
		if status == 401 {
			p.invalidateEndpoint(ep.ID)
			return nil, fmt.Errorf("session expired: upstream returned HTTP 401 on WS upgrade")
		}
		if status == 403 {
			// 403 is "this user lacks permission for this resource", distinct
			// from "session expired". Do not wipe the session. Return a
			// generic message rather than the upstream body (which may
			// contain reflected credentials).
			_ = body // intentionally not returned
			return nil, fmt.Errorf("not authorized to open console for this VM")
		}
		if status == 0 {
			// A TRANSIENT network failure (TCP reset / timeout), not an HTTP
			// rejection. Do NOT run the v3 preflight here: it rides the same
			// impaired path that just failed and would only burn its timeout
			// before falling through to this exact error — on a flapping link
			// that stretches every reconnect cycle. Retry promptly; a genuinely
			// expired cookie is still caught by the 30s keep-alive's VerifyAuth.
			return nil, fmt.Errorf("dial VNC websocket: %w", err)
		}
		// status != 0: an HTTP rejection whose cause Prism's WAF hides in the
		// (credential-reflecting) body we never surface. Preflight the v3 API to
		// disambiguate powered-off / deleted / dead-cookie into a STABLE cause the
		// proxy classifier keys off (vm_powered_off / vm_not_found / session
		// expired). 3s cap — a healthy GetVM is ~tens of ms; an impaired one fails
		// well inside that, so the preflight never materially delays recovery.
		// A stale cached cluster UUID manifests exactly as this HTTP-status
		// rejection, so drop the cache entry up front; a successful read below
		// repopulates it AND this session's routing, making the next dial
		// self-healing.
		p.dropConsoleTarget(endpointID, vmUUID)
		pctx, cancel := context.WithTimeout(ctx, 3*time.Second)
		vm, verr := client.GetVM(pctx, vmUUID)
		cancel()
		if verr == nil {
			p.storeConsoleTarget(endpointID, vmUUID, consoleTarget{ClusterUUID: vm.ClusterUUID, VMName: vm.Name})
			if vm.ClusterUUID != "" {
				p.mu.Lock()
				st.ClusterUUID = vm.ClusterUUID
				p.mu.Unlock()
			}
		}
		switch {
		case verr == nil && strings.EqualFold(vm.PowerState, "OFF"):
			return nil, fmt.Errorf("vm powered off: console upgrade rejected while VM power_state=OFF")
		case api.HTTPStatus(verr) == 404:
			return nil, fmt.Errorf("vm not found: %s", vmUUID)
		case api.HTTPStatus(verr) == 401:
			p.invalidateEndpoint(ep.ID)
			return nil, fmt.Errorf("session expired: VM read returned HTTP 401 after WS upgrade failure")
		case api.HTTPStatus(verr) == 403:
			return nil, fmt.Errorf("not authorized to open console for this VM")
		}
		// Do NOT embed the upstream body — Prism's WAF echoes request headers
		// (Cookie/Authorization) into 4xx/5xx bodies, and unlike logs (scrubbed)
		// and the 401/403 paths (which already suppress it), this error flows
		// verbatim into ctrlFrame.Reason and is rendered on-screen. body was read
		// only to advance/close resp.Body.
		_ = body // intentionally not returned
		return nil, fmt.Errorf("dial VNC websocket: HTTP %d", status)
	}
	// NUTA-05: Prism Mercury IAM can rotate the session cookie on the WS upgrade
	// response too (Set-Cookie), but that response never flows through the REST
	// transport's cookie-capture, so the rotation was being dropped — the jar
	// (and keystore) kept replaying the older cookie. Feed it back in so memory +
	// keystore stay current with what the upgrade just handed us.
	if resp != nil {
		client.RotateCookies(resp.Header["Set-Cookie"])
	}
	return conn, nil
}

func (p *Plugin) CloseSession(_ context.Context, sessionID string) error {
	p.mu.Lock()
	delete(p.sessions, sessionID)
	p.mu.Unlock()
	return nil
}

// ----- helpers -----

func (p *Plugin) endpoint(id string) (sdk.EndpointRef, bool) {
	for _, e := range p.deps.Config.Endpoints() {
		if e.ID == id && e.PluginID == pluginID {
			return e, true
		}
	}
	return sdk.EndpointRef{}, false
}

// clientFor returns a ready-to-use API client. It rehydrates credentials
// from the OS keychain on cold start.
func (p *Plugin) clientFor(ctx context.Context, ep sdk.EndpointRef) (*api.Client, error) {
	p.mu.RLock()
	if c, ok := p.clients[ep.ID]; ok {
		p.mu.RUnlock()
		return c, nil
	}
	st := p.state[ep.ID]
	p.mu.RUnlock()

	if st == nil {
		rehydrated, err := p.rehydrate(ep)
		if err != nil {
			return nil, err
		}
		st = rehydrated
	}

	if st.method != sdk.AuthKindCookie || len(st.sessionCookies) == 0 {
		return nil, errors.New("session cookies missing — sign in first")
	}
	c, err := p.newCookieClient(ep, st.sessionCookies)
	if err != nil {
		return nil, err
	}
	p.replaceClient(ep.ID, c, st)
	_ = ctx
	return c, nil
}

// newCookieClient builds an API client for the cookie (embedded SSO) path whose
// session cookies ROTATE. Prism Central Mercury IAM refreshes the session
// server-side (using the refresh token) and returns the renewed session cookie
// via Set-Cookie; the jar captures it, replays the latest on every REST call
// and the VNC WS upgrade, and persists the refreshed set back to the keystore.
// This is what keeps a session alive instead of dying on the captured cookie's
// absolute schedule — the root cause of the "constant reconnecting".
func (p *Plugin) newCookieClient(ep sdk.EndpointRef, cookies []api.SessionCookie) (*api.Client, error) {
	jar := api.NewCookieJar(cookies, func(rotated []api.SessionCookie) {
		// Persist the refreshed set so a cold-start rehydrate uses the latest.
		// The keystore is the durable source of truth; the running client reads
		// its live cookies from the jar.
		if enc, err := json.Marshal(rotated); err == nil {
			_ = p.deps.Keystore.Set(keySSOCookies(ep.ID), enc)
		}
		// NUTA-03: keep in-memory endpointState in sync with the keystore. The
		// rotated set was persisted above but the in-process state still held the
		// stale captured cookies — so an in-process client rebuild (clientFor →
		// newCookieClient) reseeded the jar from the OLD set, undoing the
		// rotation. Update the live state under the plugin lock so memory +
		// keystore agree and a rebuild seeds from the latest cookies.
		//
		// NOTE: this persists EVERY rotation unconditionally. A prior attempt to
		// gate it on "is this still the active client" (audit #17, guarding a
		// superseded client's late rotation from clobbering fresher cookies) broke
		// LOGIN: authStartCookie verifies the probe (VerifyAuth → users/me, which
		// triggers Mercury's first cookie rotation) BEFORE replaceClient stores it,
		// so the gate dropped that first legitimate rotation and the keystore kept
		// the now-invalidated captured cookie → next request 401'd. The #17 race is
		// rare; correctness of the common login path wins. Re-do #17 only via a
		// Close()-time onRotate detach, never an active-client identity check here.
		p.mu.Lock()
		if st, ok := p.state[ep.ID]; ok && st != nil {
			st.sessionCookies = append([]api.SessionCookie(nil), rotated...)
		}
		// A captured cookie rotation is an auth-epoch boundary: the mirror
		// only reflects evidence from the CURRENT cookie set (roles can
		// change across a session refresh), so reset it and re-arm the
		// Layer-1 probes — they re-run on the next keep-alive tick.
		p.resetCapabilitiesLocked(ep.ID)
		p.mu.Unlock()
	})
	proxyURL, noProxy := p.proxyConfig()
	return api.NewClient(api.ClientOptions{
		BaseURL:   ep.URL,
		TLSConfig: p.tlsConfigFor(ep),
		Logger:    p.deps.Logger,
		CookieJar: jar,
		ProxyURL:  proxyURL,
		NoProxy:   noProxy,
	})
}

// proxyConfigReader is an OPTIONAL extension of sdk.ConfigReader: a config
// adapter that also surfaces the operator's air-gap/egress-proxy settings
// (OBS-02). The shipped sdk.ConfigReader interface exposes only Endpoints() and
// CustomCAFiles(), so we probe for this via a type assertion and, when present,
// honor the explicit proxy_url / no_proxy. When they are empty (or the reader
// lacks these methods) api.NewClient defaults to http.ProxyFromEnvironment, so
// the standard HTTP(S)_PROXY / NO_PROXY environment variables apply.
type proxyConfigReader interface {
	ProxyURL() string
	NoProxy() []string
}

// proxyConfig returns the operator-configured egress proxy URL and no-proxy
// list when the config adapter exposes them, else empty (env-var default).
func (p *Plugin) proxyConfig() (string, []string) {
	if pc, ok := p.deps.Config.(proxyConfigReader); ok {
		return pc.ProxyURL(), pc.NoProxy()
	}
	return "", nil
}

func (p *Plugin) replaceClient(endpointID string, c *api.Client, st *endpointState) {
	p.mu.Lock()
	if prev, ok := p.clients[endpointID]; ok {
		prev.Close()
	}
	p.clients[endpointID] = c
	p.state[endpointID] = st
	// A new client is a new auth epoch: capability evidence from the previous
	// credential set no longer applies. Back to unknown, probes re-armed.
	p.resetCapabilitiesLocked(endpointID)
	p.mu.Unlock()
}

// invalidateEndpoint drops the cached client and persisted credentials for a
// single endpoint without touching any others. Used when the upstream tells
// us the session expired (HTTP 401 / 403) — the operator needs to sign in
// again for that endpoint specifically.
func (p *Plugin) invalidateEndpoint(endpointID string) {
	p.mu.Lock()
	if c, ok := p.clients[endpointID]; ok {
		c.Close()
		delete(p.clients, endpointID)
	}
	if st, ok := p.state[endpointID]; ok {
		// Zero the credential bytes before dropping the state map
		// entry. Go's GC will eventually collect the backing arrays,
		// but a heap snapshot taken in the interval would otherwise
		// expose them.
		st.wipe()
		delete(p.state, endpointID)
	}
	// The auth epoch ended — capability evidence dies with it, and the epoch
	// bump voids any in-flight probe/operation captured under it (a stale
	// write must not re-create the entry just deleted).
	delete(p.capabilities, endpointID)
	p.bumpCapEpochLocked(endpointID)
	// Collect the live sessions on this endpoint so the runtime can proactively
	// drive their consoles into the re-auth arm BEFORE the live stream drops
	// (SH2). Sessions are deliberately NOT deleted: a re-auth refreshes the
	// endpoint cookies and the SAME session id reconnects.
	var expired []string
	for sid, st := range p.sessions {
		if st.EndpointID == endpointID {
			expired = append(expired, sid)
		}
	}
	p.mu.Unlock()
	// Stale VMs would otherwise survive in the resource cache after the
	// session was invalidated.
	p.cache.delete(endpointID)
	if len(expired) > 0 && p.deps.OnSessionsExpired != nil {
		p.deps.OnSessionsExpired(expired)
	}
	// Delete persisted material so a stale cookie does not get picked up by
	// the next rehydrate. Errors are logged-and-ignored; we never want to
	// fail the invalidate path because secret deletion stuttered.
	if err := p.deps.Keystore.Delete(keySSOCookies(endpointID)); err != nil {
		p.deps.Logger.Debug("keystore delete failed", "key", keySSOCookies(endpointID), "err", err)
	}
}

// rehydrate reconstructs the endpoint's auth state from the keystore on
// cold start by reading the persisted Prism session cookies.
func (p *Plugin) rehydrate(ep sdk.EndpointRef) (*endpointState, error) {
	if ep.Allows(sdk.AuthKindCookie) {
		if raw, err := p.deps.Keystore.Get(keySSOCookies(ep.ID)); err == nil && len(raw) > 0 {
			var cookies []api.SessionCookie
			if json.Unmarshal(raw, &cookies) == nil && len(cookies) > 0 {
				return &endpointState{method: sdk.AuthKindCookie, sessionCookies: cookies}, nil
			}
		}
	}
	return nil, errors.New("no credentials available — sign in first")
}

// tlsConfigFor builds a TLS configuration honoring per-endpoint pinning,
// the explicit AllowInsecureTLS override, and any operator-provided
// custom CA bundles.
//
// # Pinning model
//
// We pin the SubjectPublicKeyInfo (SPKI) of the leaf certificate, not
// the full DER. SPKI pinning survives certificate rotation as long as
// the operator regenerates the cert with the same key — common
// practice when a cert hits its expiry but the underlying keypair is
// still trusted. The old leaf-DER pin forced operators to re-pin every
// rotation. Storage: ep.PinnedCertB64 is still base64 of the DER cert
// (operator pastes their .pem/.cer in the UI), but we extract and
// store the SPKI hash internally.
//
// # Verification path
//
// When a pin is set we set InsecureSkipVerify=true and do the full
// chain validation manually inside VerifyConnection. We use
// VerifyConnection (not VerifyPeerCertificate) because it is called
// for both fresh handshakes and resumed TLS sessions. With
// InsecureSkipVerify and a manual verifier, the pin replaces system
// roots as the trust anchor — the cert presented must be the one whose
// SPKI matches, full stop.
//
// # Fail-closed semantics
//
// If PinnedCertB64 fails to base64-decode, we return a TLS config that
// refuses every handshake. Previously this silently fell through to
// the system roots — pinning had effectively no effect for misconfigured
// pins.
//
// # CustomCAFiles
//
// SecuritySettings.CustomCAFiles is loaded into RootCAs so operators
// who configure their internal CA actually get their internal CA
// trusted. Was previously dead config — declared, surfaced in the UI,
// but never read.
func (p *Plugin) tlsConfigFor(ep sdk.EndpointRef) *tls.Config {
	cfg := &tls.Config{
		MinVersion:         tls.VersionTLS13,
		ClientSessionCache: tls.NewLRUClientSessionCache(64),
	}
	if u, err := url.Parse(ep.URL); err == nil {
		cfg.ServerName = u.Hostname()
	}

	// CustomCAFiles override / extend the system roots.
	if pool := p.customRootPool(); pool != nil {
		cfg.RootCAs = pool
	}

	if ep.AllowInsecureTLS && ep.PinnedCertB64 == "" {
		// FAIL-CLOSED. allow_insecure_tls without a pin marks an endpoint that is
		// waiting for trust on first use: the SSO window captures the cluster's
		// certificate and, once the operator confirms its fingerprint, the pin is
		// stored and allow_insecure_tls cleared. Until then this plugin never skips
		// verification, because that would make the credentials and the console
		// stream MITM-able. Refuse the connection instead. (Mirrors the
		// unparseable-pin handling below: a VerifyConnection that always errors,
		// with InsecureSkipVerify=true only so our verifier runs in place of the
		// default one.)
		p.deps.Logger.Warn("refusing endpoint: waiting for its certificate to be trusted on first sign-in",
			"endpoint", ep.ID)
		rejectErr := fmt.Errorf("endpoint %q: certificate not trusted yet — sign in and confirm the fingerprint", ep.ID)
		cfg.VerifyConnection = func(_ tls.ConnectionState) error { return rejectErr }
		cfg.InsecureSkipVerify = true
		return cfg
	}

	if ep.PinnedCertB64 == "" {
		return cfg
	}

	raw, err := base64.StdEncoding.DecodeString(ep.PinnedCertB64)
	if err != nil {
		// Fail-closed: an unparseable pin disables the endpoint until
		// the operator fixes their config. Previously this silently
		// dropped through to "trust system roots", which meant a
		// configured-but-broken pin was operationally identical to "no
		// pin at all" — false sense of security.
		p.deps.Logger.Error("pinned cert is not valid base64 — disabling endpoint",
			"endpoint", ep.ID, "err", err)
		rejectErr := fmt.Errorf("endpoint %q has an invalid pinned cert configuration", ep.ID)
		cfg.VerifyConnection = func(_ tls.ConnectionState) error {
			return rejectErr
		}
		cfg.InsecureSkipVerify = true // skip default verifier so ours runs
		return cfg
	}
	pinCert, err := x509.ParseCertificate(raw)
	if err != nil {
		p.deps.Logger.Error("pinned cert does not parse as x509 — disabling endpoint",
			"endpoint", ep.ID, "err", err)
		rejectErr := fmt.Errorf("endpoint %q has an invalid pinned cert configuration", ep.ID)
		cfg.VerifyConnection = func(_ tls.ConnectionState) error {
			return rejectErr
		}
		cfg.InsecureSkipVerify = true
		return cfg
	}
	pinnedSPKI := sha256.Sum256(pinCert.RawSubjectPublicKeyInfo)

	// VerifyConnection covers both fresh and resumed TLS sessions.
	cfg.InsecureSkipVerify = true
	cfg.VerifyConnection = func(cs tls.ConnectionState) error {
		if len(cs.PeerCertificates) == 0 {
			return errors.New("no peer certificate")
		}
		leaf := cs.PeerCertificates[0]
		got := sha256.Sum256(leaf.RawSubjectPublicKeyInfo)
		if got != pinnedSPKI {
			return fmt.Errorf("pinned SPKI mismatch for %s", ep.URL)
		}
		now := time.Now()
		if now.Before(leaf.NotBefore) {
			return fmt.Errorf("peer cert not yet valid (NotBefore=%s)", leaf.NotBefore)
		}
		if now.After(leaf.NotAfter) {
			return fmt.Errorf("peer cert expired (NotAfter=%s)", leaf.NotAfter)
		}
		if cfg.ServerName != "" {
			if err := leaf.VerifyHostname(cfg.ServerName); err != nil {
				return fmt.Errorf("peer cert hostname: %w", err)
			}
		}
		return nil
	}
	return cfg
}

// customRootPool returns a *x509.CertPool seeded with the operator's
// CustomCAFiles AND the system roots, or nil if none configured. Read
// errors are logged but don't fail the dial — they get logged once
// per endpoint open instead of poisoning every TLS handshake. Pool
// is rebuilt per call so live config changes take effect on the next
// session open.
func (p *Plugin) customRootPool() *x509.CertPool {
	if p.deps.Config == nil {
		return nil
	}
	files := p.deps.Config.CustomCAFiles()
	if len(files) == 0 {
		return nil
	}
	pool, err := x509.SystemCertPool()
	if err != nil || pool == nil {
		// Fresh pool: better than failing the dial. Operators on
		// systems without a usable system pool (rare) get *only*
		// their CustomCAFiles, which is what they configured.
		pool = x509.NewCertPool()
	}
	for _, path := range files {
		pem, err := os.ReadFile(path)
		if err != nil {
			p.deps.Logger.Warn("custom CA file unreadable",
				"path", path, "err", err)
			continue
		}
		if !pool.AppendCertsFromPEM(pem) {
			p.deps.Logger.Warn("custom CA file contained no parseable certs",
				"path", path)
			continue
		}
		p.deps.Logger.Info("custom CA loaded", "path", path)
	}
	return pool
}

// ----- keystore key namespacing -----

func keySSOCookies(endpointID string) string {
	return "endpoint/" + endpointID + "/sso_cookies_json"
}

// ----- resource ID helpers -----

// vmShort returns a short, operator-facing form of a VM UUID for task labels
// (the first segment of the canonical 8-4-4-4-12 UUID). The VM name isn't
// always known on the resource-based ops (PowerAction/AddSerialPort run without
// a session), so the short UUID is the stable label. Carries no secret.
func vmShort(vmUUID string) string {
	if i := strings.IndexByte(vmUUID, '-'); i > 0 {
		return vmUUID[:i]
	}
	if len(vmUUID) > 8 {
		return vmUUID[:8]
	}
	return vmUUID
}

func splitResourceID(s string) (string, string, error) {
	for i := 0; i < len(s); i++ {
		if s[i] == '/' {
			pluginID, vmUUID := s[:i], s[i+1:]
			// Defense-in-depth: the vmUUID segment is interpolated into
			// upstream Prism URLs. Reject anything that is not a plain
			// UUID so a crafted resource ID cannot smuggle path/query
			// characters even if a future call site forgets to escape.
			if !isUUID(vmUUID) {
				return "", "", fmt.Errorf("invalid VM UUID in resource ID: %q", vmUUID)
			}
			return pluginID, vmUUID, nil
		}
	}
	return "", "", fmt.Errorf("invalid resource ID: %q", s)
}

// isUUID reports whether s is a canonical 8-4-4-4-12 hex UUID. Nutanix VM
// UUIDs always take this form; restricting to it blocks path/query
// injection through the resource-ID parser.
func isUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, r := range s {
		switch i {
		case 8, 13, 18, 23:
			if r != '-' {
				return false
			}
		default:
			isHex := (r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F')
			if !isHex {
				return false
			}
		}
	}
	return true
}

// ----- in-memory resource cache -----

type resourceCache struct {
	mu  sync.Mutex
	ttl time.Duration
	v   map[string]cachedResources
}

type cachedResources struct {
	resources []sdk.Resource
	until     time.Time
}

func newResourceCache(ttl time.Duration) *resourceCache {
	return &resourceCache{ttl: ttl, v: make(map[string]cachedResources)}
}

func (c *resourceCache) get(key string) ([]sdk.Resource, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	cv, ok := c.v[key]
	if !ok || time.Now().After(cv.until) {
		return nil, false
	}
	return cv.resources, true
}

func (c *resourceCache) set(key string, r []sdk.Resource) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.v[key] = cachedResources{resources: r, until: time.Now().Add(c.ttl)}
}

func (c *resourceCache) purge() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.v = make(map[string]cachedResources)
}

func (c *resourceCache) delete(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.v, key)
}

// MountAgentISO implements sdk.ISOMountCapable.
//
// Looks up the session's VM, reads the agent ISO from
// AHV_REMOTE_CONSOLE_AGENT_ISO_PATH (default `build/agent.iso`),
// uploads it to the cluster's Prism Image Service (deduping by
// name + SHA-256), then attaches it as a SATA CD-ROM to the
// session's VM. Idempotent end-to-end: re-calls just return the
// cached UUIDs.
//
// Name convention for the catalog entry (see agentImageName):
//
//	production: ahv-remote-console-agent.iso              (static)
//	dev:        ahv-remote-console-agent-dev-<version>.iso
//
// The content SHA is NOT in the name — it travels in the image's
// spec.description, where UploadImage uses it to reuse an unchanged
// image or OVERRIDE a changed one (a new prod build with different
// bytes replaces the old static-named image). See images.go.

// agentImageName returns the Prism catalog name for the agent installer ISO.
// Production is a fixed, version-free name so a new build with different bytes
// overrides the old one in place (override is by content SHA in the
// description, not the name). Dev tags the name with the build version so a
// developer's concurrent builds don't collide on one catalog entry. The
// version is sanitized to a filename-safe token; an empty/"dev" version yields
// the plain "-dev" suffix.
func agentImageName(dev bool, version string) string {
	if !dev {
		return "ahv-remote-console-agent.iso"
	}
	v := sanitizeVersionTag(version)
	if v == "" || v == "dev" {
		return "ahv-remote-console-agent-dev.iso"
	}
	return "ahv-remote-console-agent-dev-" + v + ".iso"
}

// sanitizeVersionTag keeps only filename-safe characters from a build version
// (git-describe output like "v1.2.3-5-gabc1234-dirty" is already safe); any
// other rune becomes '-'. Bounds the length so a pathological version can't
// produce an unwieldy catalog name.
func sanitizeVersionTag(version string) string {
	const maxLen = 48
	var b strings.Builder
	for _, r := range version {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
		if b.Len() >= maxLen {
			break
		}
	}
	return strings.Trim(b.String(), "-")
}

// PowerAction implements sdk.PowerActionCapable. Takes a resourceID
// (endpoint/vmUUID) so it works for off VMs without an active session.
func (p *Plugin) PowerAction(ctx context.Context, resourceID string, action string) error {
	endpointID, vmUUID, err := splitResourceID(resourceID)
	if err != nil {
		return err
	}
	ep, ok := p.endpoint(endpointID)
	if !ok {
		return fmt.Errorf("unknown endpoint: %s", endpointID)
	}
	client, err := p.clientFor(ctx, ep)
	if err != nil {
		return fmt.Errorf("api client: %w", err)
	}
	p.deps.Logger.Info("power action", "vm", vmUUID, "action", action)
	// Register the op in the Tasks tray. PowerAction is synchronous (it polls
	// the Prism task to terminal before returning), so the handle flips
	// running -> succeeded/failed on return — real Prism state, just no
	// mid-flight percent for the short power task.
	taskID := p.newTask("vm_power", "Power "+action+" "+vmShort(vmUUID), endpointID, vmUUID, "")
	epoch := p.capEpoch(endpointID)
	perr := client.PowerAction(ctx, vmUUID, action)
	p.finishTask(taskID, perr)
	// Layer-2 evidence: success or a 403 on the v4 $actions verb is vm_power
	// proof; the ETag GET / task polls inside never misattribute (method+path
	// matched, see capabilities.go).
	p.noteCapabilityOutcome(endpointID, capVMPower, perr, epoch)
	if perr != nil {
		// The renderer toast is transient — the log is the durable record of
		// WHY (waitForTaskV4 carries Prism's task failure detail).
		p.deps.Logger.Warn("power action failed", "vm", vmUUID, "action", action, "err", perr)
	}
	return perr
}

// GetVMStats implements sdk.StatsCapable. Takes a resourceID
// (endpoint/vmUUID) so it works for any VM the endpoint manages, with or
// without an active session.
//
// READ-ONLY: it reads one live utilization sample via the v3 groups/arithmos
// query and mutates nothing — so, unlike PowerAction, it does NOT register a
// task or record capability evidence (a read proves neither vm_power nor
// vm_update).
func (p *Plugin) GetVMStats(ctx context.Context, resourceID string) (sdk.VMStats, error) {
	endpointID, vmUUID, err := splitResourceID(resourceID)
	if err != nil {
		return sdk.VMStats{}, err
	}
	ep, ok := p.endpoint(endpointID)
	if !ok {
		return sdk.VMStats{}, fmt.Errorf("unknown endpoint: %s", endpointID)
	}
	client, err := p.clientFor(ctx, ep)
	if err != nil {
		return sdk.VMStats{}, fmt.Errorf("api client: %w", err)
	}
	sample, err := client.GetVMStats(ctx, vmUUID)
	if err != nil {
		return sdk.VMStats{}, err
	}
	return sdk.VMStats{
		CPUUsagePpm:    sample.CPUUsagePpm,
		MemoryUsagePpm: sample.MemoryUsagePpm,
		DiskReadBps:    sample.DiskBytesReadPerSec,
		DiskWriteBps:   sample.DiskBytesWritePerSec,
		NetRxBps:       sample.NetBytesRxPerSec,
		NetTxBps:       sample.NetBytesTxPerSec,
		SampledAt:      time.Now(),
	}, nil
}

// serialNoteFromResult maps an api.EnsureSerialPort outcome to the SerialNote
// vocabulary shared by the SDK (sdk.ISOMountResult.SerialNote /
// sdk.SerialPortAddResult.Note) and the renderer (serialCopy.ts). Shared by
// MountAgentISO and the dedicated AddSerialPort action so the two never drift.
// A genuine failure (err != nil) maps to "serial_port_error" without touching
// res, which may be nil in that case.
func serialNoteFromResult(res *api.SerialPortResult, err error) string {
	switch {
	case err != nil:
		return "serial_port_error"
	case res == nil:
		return ""
	case res.PowerCycleRequired:
		// VM is powered ON — AHV can't hot-add a serial port. The operator
		// must fully power the VM OFF and back ON (an ACPI reboot is NOT
		// enough). The renderer renders this as actionable guidance.
		return "serial_port_power_off_required"
	case res.AlreadyPresent:
		return "serial_port_present"
	case res.Added:
		return "serial_port_added"
	default:
		return ""
	}
}

// AddSerialPort implements sdk.SerialPortCapable. It ensures the VM has the
// COM3 serial port (index 2) the vdagent clipboard channel needs. Resource-
// based (not session-based) so the operator can run it while the VM is
// powered OFF, with no active console session — which is exactly when AHV
// will actually attach the port.
func (p *Plugin) AddSerialPort(ctx context.Context, resourceID string) (sdk.SerialPortAddResult, error) {
	endpointID, vmUUID, err := splitResourceID(resourceID)
	if err != nil {
		return sdk.SerialPortAddResult{}, err
	}
	ep, ok := p.endpoint(endpointID)
	if !ok {
		return sdk.SerialPortAddResult{}, fmt.Errorf("unknown endpoint: %s", endpointID)
	}
	client, err := p.clientFor(ctx, ep)
	if err != nil {
		return sdk.SerialPortAddResult{}, fmt.Errorf("api client: %w", err)
	}
	// Track the op in the Tasks tray. EnsureSerialPort is synchronous (it polls
	// the spec-update task to terminal internally), so the handle flips
	// running -> succeeded/failed on return.
	taskID := p.newTask("serial_port", "Add serial port "+vmShort(vmUUID), endpointID, vmUUID, "")
	// COM3 == serial port index 2.
	epoch := p.capEpoch(endpointID)
	res, spErr := client.EnsureSerialPort(ctx, vmUUID, 2)
	// AlreadyPresent means EnsureSerialPort never PUT — no vm_update evidence.
	p.noteVMUpdateEvidence(endpointID, spErr != nil || res == nil || !res.AlreadyPresent, spErr, epoch)
	if spErr != nil {
		// Genuine failure (network / permission / task failed on a powered-OFF
		// VM). Surface it — the router maps a non-nil error to a 502 with the
		// message, which the renderer shows inline.
		p.finishTask(taskID, spErr)
		p.deps.Logger.Warn("add serial port: failed", "err", spErr, "vm", vmUUID)
		return sdk.SerialPortAddResult{}, spErr
	}
	p.finishTask(taskID, nil)
	note := serialNoteFromResult(res, nil)
	if res.PowerCycleRequired {
		// The add couldn't take effect on the running VM — watch for the
		// next observed power-off and complete it then (Part of the merged
		// COM3 lifecycle; never a power op).
		p.armDeferredSerialAdd(endpointID, vmUUID)
	} else {
		// Definitive success — clear any deferred entry from earlier attempts.
		p.dropDeferredSerialAdd(deferredSerialKey(endpointID, vmUUID))
	}
	p.deps.Logger.Info("add serial port", "vm", vmUUID, "note", note, "power", res.PowerState)
	return sdk.SerialPortAddResult{Note: note, PowerState: res.PowerState}, nil
}

// CreateSnapshot implements sdk.SnapshotCapable. Resource-based
// (endpoint/vmUUID) like PowerAction / AddSerialPort. Creating a snapshot is a
// VM-config write, so it records vm_update evidence.
func (p *Plugin) CreateSnapshot(ctx context.Context, resourceID, name string) (sdk.SnapshotInfo, error) {
	endpointID, vmUUID, err := splitResourceID(resourceID)
	if err != nil {
		return sdk.SnapshotInfo{}, err
	}
	ep, ok := p.endpoint(endpointID)
	if !ok {
		return sdk.SnapshotInfo{}, fmt.Errorf("unknown endpoint: %s", endpointID)
	}
	client, err := p.clientFor(ctx, ep)
	if err != nil {
		return sdk.SnapshotInfo{}, fmt.Errorf("api client: %w", err)
	}
	taskID := p.newTask("vm_snapshot", "Create snapshot "+vmShort(vmUUID), endpointID, vmUUID, "")
	epoch := p.capEpoch(endpointID)
	_, snapUUID, cErr := client.CreateSnapshot(ctx, vmUUID, name)
	p.finishTask(taskID, cErr)
	// A snapshot create always issues a write (no idempotent skip), so a success
	// is vm_update evidence and a 403 (were the verb path to match) is a denial.
	p.noteVMUpdateEvidence(endpointID, true, cErr, epoch)
	if cErr != nil {
		p.deps.Logger.Warn("create snapshot failed", "vm", vmUUID, "err", cErr)
		return sdk.SnapshotInfo{}, cErr
	}
	// The v3 create (procedural_response) carries only a task UUID, so snapUUID
	// is empty here; the panel re-lists to surface the new recovery point.
	p.deps.Logger.Info("create snapshot", "vm", vmUUID, "snapshot", snapUUID)
	return sdk.SnapshotInfo{UUID: snapUUID, Name: name, SnapshotType: "CRASH_CONSISTENT"}, nil
}

// ListSnapshots implements sdk.SnapshotCapable. READ-ONLY: it lists the VM's
// snapshots and mutates nothing, so — like GetVMStats — it records NO capability
// evidence (there is no snapshot_read capability in the mirror) and registers no
// task.
func (p *Plugin) ListSnapshots(ctx context.Context, resourceID string) ([]sdk.SnapshotInfo, error) {
	endpointID, vmUUID, err := splitResourceID(resourceID)
	if err != nil {
		return nil, err
	}
	ep, ok := p.endpoint(endpointID)
	if !ok {
		return nil, fmt.Errorf("unknown endpoint: %s", endpointID)
	}
	client, err := p.clientFor(ctx, ep)
	if err != nil {
		return nil, fmt.Errorf("api client: %w", err)
	}
	snaps, err := client.ListSnapshots(ctx, vmUUID)
	if err != nil {
		return nil, err
	}
	out := make([]sdk.SnapshotInfo, 0, len(snaps))
	for _, s := range snaps {
		out = append(out, sdk.SnapshotInfo{
			UUID:         s.UUID,
			Name:         s.Name,
			CreatedAt:    s.CreatedAt,
			ExpiresAt:    s.ExpiresAt,
			State:        s.State,
			SnapshotType: s.SnapshotType,
		})
	}
	return out, nil
}

// RestoreSnapshot implements sdk.SnapshotCapable. On v3 a restore CREATES A NEW
// VM from the recovery point — it is NOT a guaranteed in-place revert of the
// source VM. The plugin does NOT prompt — the renderer's mandatory confirmation
// dialog gates this. The Info log is the durable record of the op (UUIDs only —
// no plaintext).
func (p *Plugin) RestoreSnapshot(ctx context.Context, resourceID, snapshotID string) error {
	endpointID, vmUUID, err := splitResourceID(resourceID)
	if err != nil {
		return err
	}
	ep, ok := p.endpoint(endpointID)
	if !ok {
		return fmt.Errorf("unknown endpoint: %s", endpointID)
	}
	client, err := p.clientFor(ctx, ep)
	if err != nil {
		return fmt.Errorf("api client: %w", err)
	}
	taskID := p.newTask("vm_snapshot", "Restore snapshot "+vmShort(vmUUID), endpointID, vmUUID, "")
	epoch := p.capEpoch(endpointID)
	_, rErr := client.RestoreSnapshot(ctx, vmUUID, snapshotID)
	p.finishTask(taskID, rErr)
	p.noteVMUpdateEvidence(endpointID, true, rErr, epoch)
	if rErr != nil {
		p.deps.Logger.Warn("restore snapshot failed", "vm", vmUUID, "snapshot", snapshotID, "err", rErr)
		return rErr
	}
	p.deps.Logger.Info("restore snapshot", "vm", vmUUID, "snapshot", snapshotID)
	return nil
}

func (p *Plugin) MountAgentISO(ctx context.Context, sessionID string, opts sdk.ISOMountOptions) (result sdk.ISOMountResult, retErr error) {
	// Detach from the HTTP request context (which Go's net/http
	// cancels as soon as the response returns) and put a generous
	// upper bound on the whole operation. The PUT-bytes step alone
	// can run for minutes against a busy Prism over VPN; killing
	// the upload halfway leaves a half-baked catalog entry the
	// next attempt has to clean up.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Minute)
	defer cancel()

	// 1. Resolve the session → endpoint + VM UUID.
	p.mu.RLock()
	st, ok := p.sessions[sessionID]
	p.mu.RUnlock()
	if !ok {
		return sdk.ISOMountResult{}, fmt.Errorf("ahv: mount agent iso: unknown session: %s", sessionID)
	}
	ep, ok := p.endpoint(st.EndpointID)
	if !ok {
		return sdk.ISOMountResult{}, fmt.Errorf("ahv: mount agent iso: endpoint vanished: %s", st.EndpointID)
	}
	client, err := p.clientFor(ctx, ep)
	if err != nil {
		return sdk.ISOMountResult{}, fmt.Errorf("ahv: mount agent iso: api client: %w", err)
	}

	// Register the install in the Tasks tray. Minted now (op start) so the
	// tray's useTasks discovers it by session_id immediately — it doesn't wait
	// for the synchronous return. updateTask bumps percent at each real phase
	// boundary; the deferred finishTask records the terminal state, and stamps
	// the returned task_id into the result so the renderer polls THIS task.
	taskID := p.newTask("agent_iso", "Install agent on "+vmShort(st.VMUUID), st.EndpointID, st.VMUUID, sessionID)
	defer func() { p.finishTask(taskID, retErr) }()

	// 2. Read the agent ISO bytes from disk.
	isoPath := os.Getenv("AHV_REMOTE_CONSOLE_AGENT_ISO_PATH")
	if isoPath == "" {
		// Legacy override name, kept so an operator/CI that still exports the
		// old variable doesn't silently fall back to the default path.
		isoPath = os.Getenv("AHV_REMOTE_CONSOLE_BRIDGE_ISO_PATH")
	}
	if isoPath == "" {
		isoPath = "build/agent.iso"
	}
	isoPath = filepath.Clean(isoPath)
	isoBytes, err := os.ReadFile(isoPath) // #nosec G304 -- path from trusted env var or hardcoded default
	if err != nil {
		return sdk.ISOMountResult{}, fmt.Errorf("ahv: mount agent iso: read %s: %w", isoPath, err)
	}
	if len(isoBytes) == 0 {
		return sdk.ISOMountResult{}, fmt.Errorf("ahv: mount agent iso: %s is empty", isoPath)
	}
	p.deps.Logger.Info("mount-iso: ISO loaded",
		"path", isoPath, "bytes", len(isoBytes))

	// 3. Build the catalog image name. Production uses a STATIC name; dev tags
	//    it with the build version so concurrent dev builds are distinguishable.
	//    The content SHA is NOT in the name anymore — it travels in the image's
	//    spec.description, where UploadImage uses it to reuse-or-override (see
	//    images.go). Dev vs prod is the app's own signal (AHV_REMOTE_CONSOLE_DEV,
	//    inherited by the runtime); the version is the runtime build version.
	dev := os.Getenv("AHV_REMOTE_CONSOLE_DEV") == "1"
	imageName := agentImageName(dev, p.deps.Version)

	// Evidence epoch for every capability-mirror write this mount produces:
	// a mount straddling an auth-epoch boundary deposits nothing.
	mountEpoch := p.capEpoch(ep.ID)

	// 3a. Capability gate (catalog before upload). When image CREATE is
	//     already PROVEN denied, the mount must never enter UploadImage's
	//     reuse-or-override machinery (its description-SHA mismatch path
	//     deletes same-name entries and re-creates — both guaranteed 403s
	//     for this role). Instead resolve a published entry by NAME alone,
	//     even on a content mismatch (a possibly-stale published ISO beats
	//     a guaranteed failure), or fail typed: sdk.ErrImagePublishRequired
	//     becomes the wire code "image_publish_required" (router mapping),
	//     which the renderer maps to the ask-an-administrator notice.
	var imageUUID string
	var reused bool
	publishedOnly := false
	if p.capabilityValue(ep.ID, capImageCreate) == capDenied {
		uuid, stale, rerr := client.ResolvePublishedImage(ctx, imageName, isoBytes, st.ClusterUUID, p.deps.Version)
		switch {
		case rerr != nil && errors.Is(rerr, api.ErrImageNotPlaced):
			// Placement resolution would need a checkout (an image create) —
			// actionable by an admin, not by retrying here.
			return sdk.ISOMountResult{}, fmt.Errorf("ahv: mount agent iso: %w", rerr)
		case rerr != nil:
			// Couldn't verify presence; fall through — the upload path hits
			// the same wall and now fails fast on its first 403.
			p.noteCapabilityOutcome(ep.ID, capImageRead, rerr, mountEpoch)
		case uuid == "":
			p.setAgentISOInCatalog(ep.ID, false, mountEpoch)
			// The image half is blocked, but the channel half need not be:
			// the single setup control also subsumes the old standalone
			// channel add, so ensure COM3 best-effort before failing typed —
			// an agent delivered by other means keeps its channel, and a
			// running VM arms the deferred add as usual.
			spRes, spErr := client.EnsureSerialPort(ctx, st.VMUUID, 2)
			p.noteVMUpdateEvidence(ep.ID, spErr != nil || spRes == nil || !spRes.AlreadyPresent, spErr, mountEpoch)
			switch {
			case spErr != nil:
				p.deps.Logger.Warn("mount-iso: best-effort channel add failed", "err", spErr, "vm", st.VMUUID)
			case spRes.PowerCycleRequired:
				p.armDeferredSerialAdd(ep.ID, st.VMUUID)
			default:
				p.dropDeferredSerialAdd(deferredSerialKey(ep.ID, st.VMUUID))
			}
			p.deps.Logger.Warn("mount-iso: agent image not in catalog and image create is denied — publish required",
				"endpoint", ep.ID, "name", imageName)
			return sdk.ISOMountResult{}, fmt.Errorf(
				"ahv: mount agent iso: %w: the agent image %q is not in the image catalog and this role cannot create images — ask an administrator to publish the agent image",
				sdk.ErrImagePublishRequired, imageName)
		default:
			imageUUID, reused, publishedOnly = uuid, true, true
			p.setCapability(ep.ID, capImageRead, true, mountEpoch)
			p.setAgentISOInCatalog(ep.ID, true, mountEpoch)
			if stale {
				p.deps.Logger.Warn("mount-iso: published agent image content differs from this build — using it anyway (ask an administrator to publish an updated image)",
					"endpoint", ep.ID, "name", imageName, "image_uuid", uuid)
			}
		}
	}

	p.deps.Logger.Info("mount-iso: uploading to Prism Image Service",
		"name", imageName, "dev", dev, "endpoint", ep.ID)

	// 4. Upload (or reuse). Prism's Image Service has been observed
	//    returning intermittent HTTP 500 on the byte-PUT step and
	//    intermittent HTTP 500 on the dedupe-list step that
	//    UploadImage performs internally. Retry with linear backoff
	//    so a transient cluster hiccup doesn't block the operator
	//    from deploying the agent installer ISO.
	//
	//    Cap at MOUNT_ISO_UPLOAD_ATTEMPTS=3 — beyond that the cluster
	//    is genuinely sick and the operator should escalate.
	const mountIsoUploadAttempts = 3
	mountIsoBackoffMs := []time.Duration{750 * time.Millisecond, 2 * time.Second}
	var lastErr error
	for attempt := 1; attempt <= mountIsoUploadAttempts && !publishedOnly; attempt++ {
		uploadStart := time.Now()
		imageUUID, reused, lastErr = client.UploadImage(ctx, imageName, isoBytes, st.ClusterUUID, p.deps.Version)
		if lastErr == nil {
			p.deps.Logger.Info("mount-iso: upload complete",
				"image_uuid", imageUUID, "reused", reused,
				"attempt", attempt,
				"elapsed", time.Since(uploadStart))
			break
		}
		// image-not-placed (NO_POLICY_COMPLIANT_CLUSTER) is NOT transient: the
		// bytes landed on a different cluster, so retrying just re-uploads 11 MB
		// and fails again. Surface the actionable error immediately rather than
		// burning the upload-retry budget.
		if errors.Is(lastErr, api.ErrImageNotPlaced) {
			p.deps.Logger.Warn("mount-iso: image not placed on VM cluster — not retrying",
				"err", lastErr, "vm", st.VMUUID)
			return sdk.ISOMountResult{}, fmt.Errorf("ahv: mount agent iso: %w", lastErr)
		}
		// An RBAC 403 is equally non-transient: record the evidence and fail
		// fast instead of burning the retry budget (and a ctx cancel during
		// backoff would otherwise lose the evidence entirely).
		if denies403(lastErr, capImageCreate) || denies403(lastErr, capImageRead) {
			p.noteCapabilityOutcome(ep.ID, capImageCreate, lastErr, mountEpoch)
			p.noteCapabilityOutcome(ep.ID, capImageRead, lastErr, mountEpoch)
			p.deps.Logger.Warn("mount-iso: upload denied by upstream RBAC — not retrying",
				"err", lastErr, "vm", st.VMUUID)
			return sdk.ISOMountResult{}, fmt.Errorf("ahv: mount agent iso: upload: %w", lastErr)
		}
		if attempt == mountIsoUploadAttempts {
			// Layer-2 evidence: a 403 from the catalog-entry POST or the
			// bytes PUT proves image_create denied; one from the dedupe list
			// proves image_read denied (matched on method+path, see
			// capabilities.go).
			p.noteCapabilityOutcome(ep.ID, capImageCreate, lastErr, mountEpoch)
			p.noteCapabilityOutcome(ep.ID, capImageRead, lastErr, mountEpoch)
			p.deps.Logger.Error("mount-iso: upload exhausted retries",
				"err", lastErr, "attempts", attempt,
				"elapsed", time.Since(uploadStart))
			return sdk.ISOMountResult{}, fmt.Errorf("ahv: mount agent iso: upload: %w", lastErr)
		}
		backoff := mountIsoBackoffMs[attempt-1]
		p.deps.Logger.Warn("mount-iso: upload failed — retrying",
			"err", lastErr, "attempt", attempt,
			"backoff", backoff,
			"elapsed", time.Since(uploadStart))
		select {
		case <-time.After(backoff):
		case <-ctx.Done():
			return sdk.ISOMountResult{}, ctx.Err()
		}
	}
	// Layer-2 evidence from the successful upload/reuse: the dedupe walk
	// inside UploadImage listed the catalog (image_read), a fresh upload
	// exercised create (reuse made no create call, so it proves nothing
	// there), and either way the agent ISO is now in the catalog. The
	// published-only path recorded its own evidence at the gate.
	if !publishedOnly {
		p.setCapability(ep.ID, capImageRead, true, mountEpoch)
		if !reused {
			p.setCapability(ep.ID, capImageCreate, true, mountEpoch)
		}
		p.setAgentISOInCatalog(ep.ID, true, mountEpoch)
	}

	// Tasks-tray progress: the image half is done (uploaded or reused). The
	// attach (and any consented power cycle) is the remaining work.
	imageDetail := "Uploaded the installer image"
	if reused {
		imageDetail = "Reusing the published installer image"
	}
	p.updateTask(taskID, func(ti *sdk.TaskInfo) {
		ti.PercentComplete = 70
		ti.Detail = imageDetail
	})

	// 5+6. Attach the ISO and ensure the COM3 serial port. When COM3 is
	//    absent and the VM is OFF (including mid consented power cycle) both
	//    edits ride ONE merged spec round-trip — one spec_version bump, one
	//    Prism task, no 409 window between them. When COM3 is already
	//    present only the CD-ROM media change is sent. On a running VM
	//    without power-cycle consent the behavior is unchanged (hot attach +
	//    the serial probe that reports power_off_required), plus the
	//    deferred apply is armed for the next observed power-off.
	attachStart := time.Now()
	driveLetter, attachReused, serialNote, mountErr := p.attachWithSerial(ctx, client, st.EndpointID, st.VMUUID, imageUUID, opts)
	if mountErr != nil {
		p.deps.Logger.Error("mount-iso: attach failed",
			"err", mountErr, "vm", st.VMUUID, "image_uuid", imageUUID,
			"elapsed", time.Since(attachStart))
		return sdk.ISOMountResult{}, fmt.Errorf("ahv: mount agent iso: %w", mountErr)
	}
	p.deps.Logger.Info("mount-iso: attach complete",
		"drive_letter_hint", driveLetter, "attach_reused", attachReused,
		"serial_note", serialNote,
		"elapsed", time.Since(attachStart))

	// Tasks-tray progress: the CD-ROM is attached (and COM3/power-cycle, if
	// any, is done). The deferred finishTask stamps it to 100% / succeeded.
	p.updateTask(taskID, func(ti *sdk.TaskInfo) {
		ti.PercentComplete = 90
		ti.Detail = "Attached the installer CD-ROM"
	})

	// "Reused" in the result is the end-to-end flag the renderer
	// keys its copy on: true means we did nothing new (image was
	// already in the catalog AND already attached to the VM).
	// If the image was reused but the CD-ROM wasn't yet attached,
	// the operator still saw a state change, so we flip Reused to
	// false to surface that.
	endToEndReused := reused && attachReused

	p.deps.Logger.Info("agent setup complete",
		"session", sessionID,
		"vm_uuid", st.VMUUID,
		"image_uuid", imageUUID,
		"image_reused", reused,
		"attach_reused", attachReused,
		"drive_letter_hint", driveLetter,
		"serial_note", serialNote,
	)

	// 7. Best-effort garbage collection of superseded plugin images.
	//    The mount above already attached the LIVE build, so passing its
	//    canonical name as the keep prunes every older bridge build hash
	//    while leaving the current one (and its duplicates) intact. GC is
	//    owner-scoped (only the plugin's own name prefixes) and
	//    reference-safe (an in-use image is skipped, never force-detached).
	//    Errors are logged, never propagated — a catalog-cleanup hiccup must
	//    not fail an otherwise-successful mount. clipMaxAge is 1h: clipboard
	//    payloads are single-use churn and short-lived.
	// Run the sweep DETACHED in the background. The mount has already
	// succeeded and must return promptly; pruning a large pre-existing
	// duplicate backlog can take a while (each delete waits on a Prism task),
	// so blocking the response here would stall the very first post-fix mount
	// for minutes. context.WithoutCancel keeps auth/values but survives the
	// request return; the 10m cap bounds a stuck sweep. client is the
	// long-lived cached API client, safe to use after this handler returns.
	p.bgWG.Add(1)
	go func(keep string) {
		defer p.bgWG.Done()
		// Detached from the REQUEST ctx (the mount must return promptly) but tied
		// to the PLUGIN lifetime via bgCtx, so Shutdown cancels + awaits it (#24)
		// instead of it being orphaned mid-Prism-delete. The 10m cap still bounds
		// a stuck sweep.
		gcCtx, cancel := context.WithTimeout(p.bgCtx, 10*time.Minute)
		defer cancel()
		if gcErr := client.GarbageCollect(gcCtx, []string{keep}, time.Hour); gcErr != nil {
			p.deps.Logger.Warn("mount-iso: background image GC returned error (non-fatal)", "err", gcErr)
		}
	}(imageName)

	return sdk.ISOMountResult{
		ImageUUID:       imageUUID,
		DriveLetterHint: driveLetter,
		Reused:          endToEndReused,
		SerialNote:      serialNote,
		TaskID:          taskID,
	}, nil
}

// attachWithSerial picks the spec-update strategy for the mount:
//
//   - COM3 already present       → plain CD-ROM-only mutation.
//   - COM3 absent + VM OFF       → ONE merged spec PUT (ISO + COM3).
//   - COM3 absent + VM ON + consent → the orchestrated power cycle.
//   - COM3 absent + VM ON, no consent → hot attach + the serial add that
//     reports power_off_required (unchanged behavior), and the deferred
//     apply is armed for the next observed power-off.
func (p *Plugin) attachWithSerial(ctx context.Context, client *api.Client, endpointID, vmUUID, imageUUID string, opts sdk.ISOMountOptions) (driveLetter string, attachReused bool, serialNote string, err error) {
	// Any outcome that proves COM3 is there makes an earlier deferred entry
	// stale — without this, a consent cycle following a no-consent mount
	// leaves the entry burning a power-state GET per keep-alive tick until
	// the VM is next seen off.
	defer func() {
		switch serialNote {
		case "serial_port_present", "serial_port_added", "serial_port_added_cycled":
			p.dropDeferredSerialAdd(deferredSerialKey(endpointID, vmUUID))
		}
	}()
	// Evidence epoch for every mirror write this attach produces.
	epoch := p.capEpoch(endpointID)
	serialPresent, vmPower, probeErr := client.SerialPortState(ctx, vmUUID, 2)
	switch {
	case probeErr != nil:
		// Can't read the serial/power state — still do the plain attach so
		// the ISO lands; the serial outcome is unknowable (error note),
		// matching the old best-effort EnsureSerialPort failure semantics.
		p.deps.Logger.Warn("mount-iso: serial state probe failed", "err", probeErr, "vm", vmUUID)
		_, driveLetter, attachReused, err = client.AttachCDROM(ctx, vmUUID, imageUUID)
		p.noteVMUpdateEvidence(endpointID, !attachReused, err, epoch)
		if err != nil {
			return "", false, "", fmt.Errorf("attach cdrom: %w", err)
		}
		return driveLetter, attachReused, "serial_port_error", nil
	case serialPresent:
		_, driveLetter, attachReused, err = client.AttachCDROM(ctx, vmUUID, imageUUID)
		p.noteVMUpdateEvidence(endpointID, !attachReused, err, epoch)
		if err != nil {
			return "", false, "", fmt.Errorf("attach cdrom: %w", err)
		}
		return driveLetter, attachReused, "serial_port_present", nil
	case !strings.EqualFold(vmPower, "ON"):
		// VM is OFF: the merged single-PUT adds the ISO and COM3 together.
		res, mErr := client.AttachCDROMAndSerialPort(ctx, vmUUID, imageUUID, 2)
		if mErr != nil {
			p.noteVMUpdateEvidence(endpointID, true, mErr, epoch)
			return "", false, "", fmt.Errorf("attach cdrom+serial: %w", mErr)
		}
		p.noteVMUpdateEvidence(endpointID, !(res.AttachReused && res.SerialAlreadyPresent), nil, epoch)
		note := "serial_port_added"
		if res.SerialAlreadyPresent {
			note = "serial_port_present"
		}
		return res.DriveLetterHint, res.AttachReused, note, nil
	case opts.PowerCycle:
		// Explicit operator consent arrived with the request: orchestrate
		// ACPI OFF → merged PUT → ON. This is the ONLY path that ever
		// changes power state.
		return p.mountWithPowerCycle(ctx, client, endpointID, vmUUID, imageUUID)
	default:
		// Running VM, no consent: hot attach, then the idempotent serial add
		// that reports power_off_required — and arm the deferred apply so
		// the next OBSERVED power-off completes COM3 without operator action.
		_, driveLetter, attachReused, err = client.AttachCDROM(ctx, vmUUID, imageUUID)
		p.noteVMUpdateEvidence(endpointID, !attachReused, err, epoch)
		if err != nil {
			return "", false, "", fmt.Errorf("attach cdrom: %w", err)
		}
		spResult, spErr := client.EnsureSerialPort(ctx, vmUUID, 2)
		// AlreadyPresent means EnsureSerialPort never PUT — no evidence then.
		p.noteVMUpdateEvidence(endpointID, spErr != nil || spResult == nil || !spResult.AlreadyPresent, spErr, epoch)
		if spErr != nil {
			p.deps.Logger.Warn("setup: failed to ensure COM3", "err", spErr, "vm", vmUUID)
		}
		if spResult != nil && spResult.PowerCycleRequired {
			p.armDeferredSerialAdd(endpointID, vmUUID)
		}
		return driveLetter, attachReused, serialNoteFromResult(spResult, spErr), nil
	}
}

// mountWithPowerCycle runs the operator-consented COM3 power cycle:
// graceful ACPI guest shutdown → wait for OFF (bounded by cycleOffBudget) →
// merged ISO+COM3 spec PUT → power back ON.
//
// Safety rails (operator-only design, violations are blockers):
//   - A hard power-off is NEVER issued. If ACPI doesn't get the guest to OFF
//     within the budget, the cycle aborts: the ISO attaches hot, the COM3 add
//     is deferred to the next observed power-off, and the power state is left
//     exactly as found.
//   - Reached-OFF is the point of no return for the power-on guarantee:
//     whatever fails after it, the flow that turned the VM off turns it back
//     on, and the original error is reported.
func (p *Plugin) mountWithPowerCycle(ctx context.Context, client *api.Client, endpointID, vmUUID, imageUUID string) (string, bool, string, error) {
	// Evidence epoch captured at cycle start: writes from a cycle straddling
	// an auth-epoch boundary are dropped by the mirror.
	epoch := p.capEpoch(endpointID)
	p.deps.Logger.Info("mount-iso: operator-consented power cycle — requesting ACPI guest shutdown",
		"vm", vmUUID, "budget", p.cycleOffBudget)
	offErr := client.SetPowerStateV3(ctx, vmUUID, "OFF")
	if denies403(offErr, capVMPower) {
		// Upstream RBAC denied the consented power op (403 on the power PUT):
		// record the evidence, abort the cycle with the VM exactly as found
		// (the PUT was rejected — nothing moved), attach the ISO hot, and
		// defer COM3 to the next observed power-off. NO retry: the denial is
		// role-based and consent can't cure it.
		p.setCapability(endpointID, capVMPower, false, epoch)
		p.deps.Logger.Warn("mount-iso: consented power cycle denied by upstream RBAC — aborting cycle, deferring COM3 add",
			"vm", vmUUID)
		p.armDeferredSerialAdd(endpointID, vmUUID)
		_, driveLetter, attachReused, aErr := client.AttachCDROM(ctx, vmUUID, imageUUID)
		p.noteVMUpdateEvidence(endpointID, !attachReused, aErr, epoch)
		if aErr != nil {
			return "", false, "", fmt.Errorf("attach cdrom: %w", aErr)
		}
		return driveLetter, attachReused, "serial_port_power_denied", nil
	}
	if offErr == nil {
		// The power PUT was accepted — that alone is vm_power evidence.
		p.setCapability(endpointID, capVMPower, true, epoch)
		offErr = client.WaitForPowerState(ctx, vmUUID, "OFF", p.cycleOffBudget, p.cyclePollEvery)
	}
	if offErr != nil {
		// The guest did not reach OFF gracefully. NO hard-off and NO
		// power-on (the flow never observed its own OFF): abort the cycle,
		// attach the ISO hot, defer COM3 to the next observed power-off.
		p.deps.Logger.Warn("mount-iso: ACPI shutdown did not reach OFF — aborting cycle, deferring COM3 add",
			"err", offErr, "vm", vmUUID)
		p.armDeferredSerialAdd(endpointID, vmUUID)
		_, driveLetter, attachReused, aErr := client.AttachCDROM(ctx, vmUUID, imageUUID)
		p.noteVMUpdateEvidence(endpointID, !attachReused, aErr, epoch)
		if aErr != nil {
			return "", false, "", fmt.Errorf("attach cdrom: %w", aErr)
		}
		return driveLetter, attachReused, "serial_port_deferred", nil
	}

	p.deps.Logger.Info("mount-iso: VM reached OFF — applying merged ISO attach + COM3", "vm", vmUUID)
	res, mErr := client.AttachCDROMAndSerialPort(ctx, vmUUID, imageUUID, 2)
	if mErr != nil {
		p.noteVMUpdateEvidence(endpointID, true, mErr, epoch)
	} else {
		p.noteVMUpdateEvidence(endpointID, !(res.AttachReused && res.SerialAlreadyPresent), nil, epoch)
	}

	// This flow turned the VM off and observed it complete — it powers the
	// VM back on even when the merged update failed.
	onErr := p.powerOnAfterCycle(ctx, client, vmUUID)
	p.noteCapabilityOutcome(endpointID, capVMPower, onErr, epoch)
	if mErr != nil {
		if onErr != nil {
			p.deps.Logger.Error("mount-iso: power-on after failed merged update also failed — VM left OFF",
				"err", onErr, "vm", vmUUID)
		}
		return "", false, "", fmt.Errorf("attach cdrom+serial (mid power cycle): %w", mErr)
	}
	if onErr != nil {
		return "", false, "", fmt.Errorf("ISO and COM3 applied but power-on failed — power the VM on manually: %w", onErr)
	}
	p.deps.Logger.Info("mount-iso: power cycle complete — COM3 added, VM powered back on", "vm", vmUUID)
	return res.DriveLetterHint, res.AttachReused, "serial_port_added_cycled", nil
}

// powerOnAfterCycle powers the VM back on after a flow-initiated OFF. If the
// mount's own context already died (which can be exactly why we are cleaning
// up), it switches to a short detached context — the power-on must still be
// attempted (same idiom as the image deleteOrphan cleanup).
func (p *Plugin) powerOnAfterCycle(ctx context.Context, client *api.Client, vmUUID string) error {
	if ctx.Err() != nil {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(context.WithoutCancel(ctx), 3*time.Minute)
		defer cancel()
	}
	return client.SetPowerStateV3(ctx, vmUUID, "ON")
}

// ----- arbitrary catalog ISO mount / eject (sdk.ImageCatalogCapable) -----

// catalogSession resolves a console session to its endpoint + cached API
// client, the common prologue for all three catalog methods. Mirrors the
// session resolution in MountAgentISO.
func (p *Plugin) catalogSession(ctx context.Context, op, sessionID string) (*sessionState, sdk.EndpointRef, *api.Client, error) {
	p.mu.RLock()
	st, ok := p.sessions[sessionID]
	p.mu.RUnlock()
	if !ok {
		return nil, sdk.EndpointRef{}, nil, fmt.Errorf("ahv: %s: unknown session: %s", op, sessionID)
	}
	ep, ok := p.endpoint(st.EndpointID)
	if !ok {
		return nil, sdk.EndpointRef{}, nil, fmt.Errorf("ahv: %s: endpoint vanished: %s", op, st.EndpointID)
	}
	client, err := p.clientFor(ctx, ep)
	if err != nil {
		return nil, sdk.EndpointRef{}, nil, fmt.Errorf("ahv: %s: api client: %w", op, err)
	}
	return st, ep, client, nil
}

// ListISOImages implements sdk.ImageCatalogCapable: returns the mountable ISO
// images in the session endpoint's catalog. Filters to image_type == ISO_IMAGE
// (empty type tolerated for older PC builds) AND a usable state (COMPLETE, or
// empty on older builds), so the picker never offers a half-baked or
// disk-image entry. Records image_read capability evidence from the list call.
func (p *Plugin) ListISOImages(ctx context.Context, sessionID string) ([]sdk.CatalogImage, error) {
	_, ep, client, err := p.catalogSession(ctx, "list iso images", sessionID)
	if err != nil {
		return nil, err
	}
	epoch := p.capEpoch(ep.ID)
	images, err := client.ListImages(ctx)
	p.noteCapabilityOutcome(ep.ID, capImageRead, err, epoch)
	if err != nil {
		return nil, fmt.Errorf("ahv: list iso images: %w", err)
	}
	p.setCapability(ep.ID, capImageRead, true, epoch)
	out := make([]sdk.CatalogImage, 0, len(images))
	for _, e := range images {
		isISO := e.ImageType == "" || strings.EqualFold(e.ImageType, "ISO_IMAGE")
		usable := e.State == "" || strings.EqualFold(e.State, "COMPLETE")
		if !isISO || !usable {
			continue
		}
		out = append(out, sdk.CatalogImage{
			UUID:        e.UUID,
			Name:        e.Name,
			State:       e.State,
			Description: e.Description,
		})
	}
	return out, nil
}

// MountISO implements sdk.ImageCatalogCapable: attaches the chosen catalog
// image into the session VM's first CD-ROM drive. Reuses AttachCDROM (media
// change) — NOT AttachCDROMAndSerialPort: an arbitrary ISO never provisions the
// COM3 vdagent channel. Records vm_update evidence.
func (p *Plugin) MountISO(ctx context.Context, sessionID, imageUUID string) (sdk.ISOMountResult, error) {
	// Defense-in-depth: imageUUID is interpolated into the upstream VM spec.
	// Reject anything that isn't a canonical UUID, mirroring splitResourceID's
	// guard on the VM UUID.
	if !isUUID(imageUUID) {
		return sdk.ISOMountResult{}, fmt.Errorf("ahv: mount iso: invalid image UUID: %q", imageUUID)
	}
	// Detach from the HTTP request context and bound the whole attach (a media
	// change waits on a Prism task), mirroring MountAgentISO.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Minute)
	defer cancel()

	st, ep, client, err := p.catalogSession(ctx, "mount iso", sessionID)
	if err != nil {
		return sdk.ISOMountResult{}, err
	}
	epoch := p.capEpoch(ep.ID)
	taskUUID, drive, reused, err := client.AttachCDROM(ctx, st.VMUUID, imageUUID)
	p.noteVMUpdateEvidence(ep.ID, !reused, err, epoch)
	if err != nil {
		return sdk.ISOMountResult{}, fmt.Errorf("ahv: mount iso: %w", err)
	}
	p.deps.Logger.Info("mount-iso (catalog): attach complete",
		"vm", st.VMUUID, "image_uuid", imageUUID, "task", taskUUID,
		"reused", reused, "drive_letter_hint", drive)
	// SerialNote intentionally left "" — arbitrary ISOs don't add the COM3
	// serial channel (that is the agent ISO's concern).
	return sdk.ISOMountResult{
		ImageUUID:       imageUUID,
		DriveLetterHint: drive,
		Reused:          reused,
	}, nil
}

// EjectISO implements sdk.ImageCatalogCapable: clears the media from the
// session VM's first CD-ROM drive (never a disk). Records vm_update evidence.
func (p *Plugin) EjectISO(ctx context.Context, sessionID string) (sdk.EjectISOResult, error) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Minute)
	defer cancel()

	st, ep, client, err := p.catalogSession(ctx, "eject iso", sessionID)
	if err != nil {
		return sdk.EjectISOResult{}, err
	}
	epoch := p.capEpoch(ep.ID)
	taskUUID, ejected, err := client.DetachCDROM(ctx, st.VMUUID)
	// A no-op eject (already empty / no CD-ROM) issues no PUT, so it proves
	// nothing about vm_update — only an actual eject (ejected==true) or an
	// error does.
	p.noteVMUpdateEvidence(ep.ID, ejected, err, epoch)
	if err != nil {
		return sdk.EjectISOResult{}, fmt.Errorf("ahv: eject iso: %w", err)
	}
	p.deps.Logger.Info("eject-iso (catalog): complete",
		"vm", st.VMUUID, "task", taskUUID, "ejected", ejected)
	return sdk.EjectISOResult{Ejected: ejected}, nil
}

// Compile-time check that the AHV plugin actually satisfies the
// optional ISOMountCapable capability. The IPC layer will assert
// against this interface; failing the cast at startup is more
// useful than a runtime mount-button no-op.
var _ sdk.ISOMountCapable = (*Plugin)(nil)

// Compile-time check that the AHV plugin satisfies the arbitrary-ISO catalog
// capability (list / mount-by-uuid / eject).
var _ sdk.ImageCatalogCapable = (*Plugin)(nil)

// Compile-time check that the AHV plugin reports per-endpoint health.
var _ sdk.HealthCapable = (*Plugin)(nil)

// Compile-time check that the AHV plugin reports the signed-in user.
var _ sdk.IdentityCapable = (*Plugin)(nil)

// Compile-time check that the AHV plugin exposes its long-running tasks to the
// renderer's Tasks tray (see tasks.go).
var _ sdk.TaskCapable = (*Plugin)(nil)
