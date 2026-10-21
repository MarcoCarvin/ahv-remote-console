// Per-endpoint Prism capability mirror.
//
// Auth is fully delegated to Prism SSO, so the app never KNOWS the operator's
// role — it can only infer it two ways:
//
//   - Layer 1, read-only probes: run once per endpoint per auth epoch, off
//     every request hot path (the keep-alive tick, or detached right after a
//     successful auth). One catalog list answers both image_read (403 means
//     denied, success means allowed) and agent-ISO presence (the existing
//     by-name convention). NO write probes of any kind.
//   - Layer 2, evidence: HTTP 403s from REAL operations the flows ran anyway
//     (image create / bytes PUT, VM spec PUTs, power ops) prove denied;
//     successes prove allowed. A 401 is session expiry — re-auth, never an
//     authorization signal — and NEVER touches the mirror.
//
// The mirror is surfaced on the existing per-endpoint health payload
// (sdk.EndpointHealth.Capabilities) so the renderer can gate setup UI on
// PROVEN denials only — "unknown" is never treated as denied.
//
// Epochs: the mirror only reflects evidence from the CURRENT credential set.
// Every auth-epoch boundary — successful (re-)auth via replaceClient, a
// captured cookie rotation, invalidateEndpoint, Logout — resets the fields to
// unknown and re-arms the Layer-1 probes (roles may have changed; stale
// evidence must not keep gating the UI).

package ahv

import (
	"context"
	"errors"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/MarcoCarvin/ahv-remote-console/apps/runtime/pkg/sdk"
	"github.com/MarcoCarvin/ahv-remote-console/plugins/ahv/internal/api"
)

// Capability states — the values of sdk.EndpointCapabilities string fields.
const (
	capUnknown = "unknown"
	capAllowed = "allowed"
	capDenied  = "denied"
)

// Capability names. They double as the health JSON keys
// (sdk.EndpointCapabilities) — keep both in sync.
const (
	capImageRead   = "image_read"
	capImageCreate = "image_create"
	capVMUpdate    = "vm_update"
	capVMPower     = "vm_power"
)

// endpointCapabilities is one endpoint's mirror. Guarded by Plugin.mu.
type endpointCapabilities struct {
	imageRead         string
	imageCreate       string
	vmUpdate          string
	vmPower           string
	agentISOInCatalog bool
	// probePending re-arms the Layer-1 probes: set at every epoch reset,
	// cleared once a probe reaches a definitive answer (allowed or denied).
	// Transport/5xx/401 failures keep it set so a later keep-alive tick
	// retries.
	probePending bool
}

func newEndpointCapabilities() *endpointCapabilities {
	return &endpointCapabilities{
		imageRead:    capUnknown,
		imageCreate:  capUnknown,
		vmUpdate:     capUnknown,
		vmPower:      capUnknown,
		probePending: true,
	}
}

// field maps a capability name to its slot, nil for an unknown name.
func (c *endpointCapabilities) field(name string) *string {
	switch name {
	case capImageRead:
		return &c.imageRead
	case capImageCreate:
		return &c.imageCreate
	case capVMUpdate:
		return &c.vmUpdate
	case capVMPower:
		return &c.vmPower
	}
	return nil
}

// capsLocked returns the endpoint's mirror entry, creating an all-unknown one
// if missing. Caller must hold p.mu (write).
func (p *Plugin) capsLocked(endpointID string) *endpointCapabilities {
	if p.capabilities == nil {
		p.capabilities = make(map[string]*endpointCapabilities)
	}
	c, ok := p.capabilities[endpointID]
	if !ok {
		c = newEndpointCapabilities()
		p.capabilities[endpointID] = c
	}
	return c
}

// resetCapabilitiesLocked starts a fresh evidence epoch for the endpoint:
// every field back to unknown, catalog flag false, Layer-1 probes re-armed,
// and the epoch counter bumped so in-flight work from the old epoch can no
// longer write. Caller must hold p.mu (write).
func (p *Plugin) resetCapabilitiesLocked(endpointID string) {
	if p.capabilities == nil {
		p.capabilities = make(map[string]*endpointCapabilities)
	}
	p.bumpCapEpochLocked(endpointID)
	p.capabilities[endpointID] = newEndpointCapabilities()
}

// capEpoch returns the endpoint's current auth-epoch counter. Capture it when
// a probe or evidence-producing operation STARTS and pass it to the mirror
// writes: a write whose captured epoch no longer matches is dropped, so work
// that completes after an auth-epoch boundary (re-auth, cookie rotation,
// invalidateEndpoint, Logout) can never deposit stale evidence into the fresh
// epoch — or re-create an entry an invalidate just deleted.
func (p *Plugin) capEpoch(endpointID string) uint64 {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.capEpochs[endpointID]
}

// bumpCapEpochLocked advances the endpoint's epoch so every previously
// captured value mismatches from now on. Counters are only ever incremented,
// never deleted — a deleted-then-recreated counter would restart at zero and
// collide with an old capture. Caller must hold p.mu (write).
func (p *Plugin) bumpCapEpochLocked(endpointID string) {
	if p.capEpochs == nil {
		p.capEpochs = make(map[string]uint64)
	}
	p.capEpochs[endpointID]++
}

// setCapability records a PROVEN outcome for one capability. epoch is the
// counter captured when the evidencing operation started (capEpoch); a
// mismatch means the auth epoch turned over mid-flight and the write is
// dropped.
func (p *Plugin) setCapability(endpointID, name string, allowed bool, epoch uint64) {
	v := capDenied
	if allowed {
		v = capAllowed
	}
	p.mu.Lock()
	if p.capEpochs[endpointID] != epoch {
		p.mu.Unlock()
		return
	}
	slot := p.capsLocked(endpointID).field(name)
	changed := slot != nil && *slot != v
	if slot != nil {
		*slot = v
	}
	p.mu.Unlock()
	if changed && !allowed {
		p.deps.Logger.Info("capability denied by upstream RBAC",
			"endpoint", endpointID, "capability", name)
	}
}

// capabilityValue returns the current state for one capability ("unknown"
// when no evidence exists yet).
func (p *Plugin) capabilityValue(endpointID, name string) string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if c, ok := p.capabilities[endpointID]; ok {
		if slot := c.field(name); slot != nil {
			return *slot
		}
	}
	return capUnknown
}

// setAgentISOInCatalog records whether the agent ISO is published in the
// endpoint's image catalog (refreshed by the Layer-1 probe, the mount-time
// presence re-check, and any mount that uploads/reuses the image). Epoch-
// gated like setCapability.
func (p *Plugin) setAgentISOInCatalog(endpointID string, present bool, epoch uint64) {
	p.mu.Lock()
	if p.capEpochs[endpointID] == epoch {
		p.capsLocked(endpointID).agentISOInCatalog = present
	}
	p.mu.Unlock()
}

// capabilitiesSnapshot returns the wire shape for the health payload. Always
// non-nil; all-unknown when nothing has been learned this epoch.
func (p *Plugin) capabilitiesSnapshot(endpointID string) *sdk.EndpointCapabilities {
	out := &sdk.EndpointCapabilities{
		ImageRead:   capUnknown,
		ImageCreate: capUnknown,
		VMUpdate:    capUnknown,
		VMPower:     capUnknown,
	}
	p.mu.RLock()
	if c, ok := p.capabilities[endpointID]; ok {
		out.ImageRead = c.imageRead
		out.ImageCreate = c.imageCreate
		out.VMUpdate = c.vmUpdate
		out.VMPower = c.vmPower
		out.AgentISOInCatalog = c.agentISOInCatalog
	}
	p.mu.RUnlock()
	return out
}

// capabilityProbePending reports whether the Layer-1 probes still need to run
// for the endpoint. A live client with no mirror entry is exactly a fresh
// epoch, so a missing entry reads as pending.
func (p *Plugin) capabilityProbePending(endpointID string) bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if c, ok := p.capabilities[endpointID]; ok {
		return c.probePending
	}
	return true
}

// noteCapabilityOutcome records Layer-2 evidence from a REAL Prism operation
// a flow ran anyway: success proves the capability allowed; an HTTP 403
// carried by the specific operation that exercises it proves denied. A 401
// (session expiry — re-auth, not authorization) and transport/5xx failures
// NEVER touch the mirror. epoch is the counter captured when the operation
// started (capEpoch).
func (p *Plugin) noteCapabilityOutcome(endpointID, name string, err error, epoch uint64) {
	if err == nil {
		p.setCapability(endpointID, name, true, epoch)
		return
	}
	if denies403(err, name) {
		p.setCapability(endpointID, name, false, epoch)
	}
}

// noteVMUpdateEvidence records vm_update evidence from a VM spec-update call.
// putIssued=false (an idempotent no-op that never PUT — e.g. an attach whose
// image was already in the drive) proves nothing on success, so it must not
// flip an earlier denial; a 403 carried by a vms PUT proves denied regardless.
func (p *Plugin) noteVMUpdateEvidence(endpointID string, putIssued bool, err error, epoch uint64) {
	if err == nil && !putIssued {
		return
	}
	p.noteCapabilityOutcome(endpointID, capVMUpdate, err, epoch)
}

// denies403 reports whether err carries an upstream HTTP 403 from the
// SPECIFIC Prism operation that evidences the named capability, matched on
// the APIError's method+path. Matching the operation — not just the status —
// keeps a flow's read/poll steps from misattributing their 403s: e.g. the VM
// GET inside SetPowerStateV3 is a read denial, not power. The v3 spec PUT and
// the v3 power PUT share one wire path (PUT /vms/{uuid}); the CALLER asserts
// which operation it issued via name.
func denies403(err error, name string) bool {
	var ae *api.APIError
	if !errors.As(err, &ae) || ae.Status != http.StatusForbidden {
		return false
	}
	switch name {
	case capImageRead:
		return ae.Method == http.MethodPost && ae.Path == "/api/nutanix/v3/images/list"
	case capImageCreate:
		if ae.Method == http.MethodPost && ae.Path == "/api/nutanix/v3/images" {
			return true
		}
		return ae.Method == http.MethodPut &&
			strings.HasPrefix(ae.Path, "/api/nutanix/v3/images/") &&
			strings.HasSuffix(ae.Path, "/file")
	case capVMUpdate:
		return ae.Method == http.MethodPut && strings.HasPrefix(ae.Path, "/api/nutanix/v3/vms/")
	case capVMPower:
		// v3 spec-driven power PUT (the consent cycle) or a v4 $actions verb
		// (the operator power buttons).
		if ae.Method == http.MethodPut && strings.HasPrefix(ae.Path, "/api/nutanix/v3/vms/") {
			return true
		}
		return ae.Method == http.MethodPost && strings.Contains(ae.Path, "/$actions/")
	}
	return false
}

// agentISOCatalogName is the by-name catalog key shared by the presence probe
// and the mount-time gate (same dev/prod convention as MountAgentISO).
func (p *Plugin) agentISOCatalogName() string {
	return agentImageName(os.Getenv("AHV_REMOTE_CONSOLE_DEV") == "1", p.deps.Version)
}

// catalogHasImage reports whether the catalog carries a usable (COMPLETE, or
// state-less on older PC builds) entry under name. Presence is by NAME only:
// content updates ride the same static name (see images.go), and a name hit
// is what removes the image_create requirement for VM-update-only operators.
func catalogHasImage(imgs []api.ImageEntry, name string) bool {
	for _, e := range imgs {
		if e.Name == name && (e.State == "" || strings.EqualFold(e.State, "COMPLETE")) {
			return true
		}
	}
	return false
}

// probeCapabilities runs the Layer-1 probes for one endpoint: a single
// read-only catalog list answers BOTH image_read (403 means denied, success
// means allowed) and agent-ISO presence. Runs off the request hot path at
// most once per auth epoch; a transport/5xx/401 failure leaves everything
// unknown AND pending so a later keep-alive tick retries. epoch is the
// counter captured when the probe started (capEpoch) — a probe that
// completes after an epoch boundary writes nothing, leaving the fresh epoch
// all-unknown with the probes still armed.
func (p *Plugin) probeCapabilities(ctx context.Context, endpointID string, client *api.Client, epoch uint64) {
	imgs, err := client.ListImages(ctx)
	if err != nil {
		if denies403(err, capImageRead) {
			p.setCapability(endpointID, capImageRead, false, epoch)
			p.mu.Lock()
			if p.capEpochs[endpointID] == epoch {
				p.capsLocked(endpointID).probePending = false
			}
			p.mu.Unlock()
			return
		}
		p.deps.Logger.Debug("capability probe failed — will retry on a later tick",
			"endpoint", endpointID, "err", err)
		return
	}
	present := catalogHasImage(imgs, p.agentISOCatalogName())
	p.mu.Lock()
	applied := p.capEpochs[endpointID] == epoch
	if applied {
		c := p.capsLocked(endpointID)
		c.imageRead = capAllowed
		c.agentISOInCatalog = present
		c.probePending = false
	}
	p.mu.Unlock()
	if !applied {
		p.deps.Logger.Debug("capability probe finished after an auth-epoch boundary — dropped",
			"endpoint", endpointID)
		return
	}
	p.deps.Logger.Debug("capability probe complete",
		"endpoint", endpointID, "agent_iso_in_catalog", present)
}

// kickCapabilityProbe runs probeCapabilities detached from the calling
// request (bounded, tied to the plugin lifetime so Shutdown cancels and
// awaits it like the other background work). The epoch is captured HERE, at
// probe start, so a reset that lands while the probe is in flight voids its
// writes. No-op when the background context is absent (bare test
// constructions).
func (p *Plugin) kickCapabilityProbe(endpointID string, client *api.Client) {
	if p.bgCtx == nil {
		return
	}
	epoch := p.capEpoch(endpointID)
	p.bgWG.Add(1)
	go func() {
		defer p.bgWG.Done()
		ctx, cancel := context.WithTimeout(p.bgCtx, 2*time.Minute)
		defer cancel()
		p.probeCapabilities(ctx, endpointID, client, epoch)
	}()
}
