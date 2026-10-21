package ahv

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/MarcoCarvin/ahv-remote-console/apps/runtime/pkg/sdk"
	"github.com/MarcoCarvin/ahv-remote-console/plugins/ahv/internal/api"
)

// TestCapabilityProbe_SetsImageReadAndCatalogPresence: the Layer-1 probe rides
// the keep-alive tick, runs ONCE per auth epoch, and one read-only catalog
// list answers both image_read and agent-ISO presence.
func TestCapabilityProbe_SetsImageReadAndCatalogPresence(t *testing.T) {
	iso := []byte("FAKEISO")
	f := newPrismFake(t, "ON", nil, iso)
	p := newCyclePlugin(t, f, iso)

	p.keepAliveTick(context.Background())
	caps := p.capabilitiesSnapshot("ep1")
	if caps.ImageRead != capAllowed {
		t.Errorf("image_read = %q, want allowed", caps.ImageRead)
	}
	if !caps.AgentISOInCatalog {
		t.Error("agent_iso_in_catalog = false, want true (fake catalog carries the agent image name)")
	}
	// Probes are read-only and never write.
	if f.SpecPuts() != 0 || len(f.PowerPuts()) != 0 || f.ImageCreates() != 0 {
		t.Errorf("probe issued writes: spec=%d power=%v creates=%d",
			f.SpecPuts(), f.PowerPuts(), f.ImageCreates())
	}
	// Once per epoch: a second tick must NOT re-list.
	lists := f.ImageLists()
	p.keepAliveTick(context.Background())
	if got := f.ImageLists(); got != lists {
		t.Errorf("second tick re-probed: lists %d -> %d", lists, got)
	}
	// Evidence-only fields stay unknown — no write probes of any kind.
	if caps.ImageCreate != capUnknown || caps.VMUpdate != capUnknown || caps.VMPower != capUnknown {
		t.Errorf("write capabilities must stay unknown after probing: %+v", caps)
	}
}

// TestCapabilityProbe_CatalogList403MarksImageReadDenied: a 403 on the list
// probe is definitive evidence — image_read denied, probe not retried within
// the epoch, catalog flag stays false.
func TestCapabilityProbe_CatalogList403MarksImageReadDenied(t *testing.T) {
	iso := []byte("FAKEISO")
	f := newPrismFake(t, "ON", nil, iso)
	f.listForbidden = true
	p := newCyclePlugin(t, f, iso)

	p.keepAliveTick(context.Background())
	caps := p.capabilitiesSnapshot("ep1")
	if caps.ImageRead != capDenied {
		t.Errorf("image_read = %q, want denied", caps.ImageRead)
	}
	if caps.AgentISOInCatalog {
		t.Error("agent_iso_in_catalog must stay false when the catalog is unreadable")
	}
	lists := f.ImageLists()
	p.keepAliveTick(context.Background())
	if got := f.ImageLists(); got != lists {
		t.Errorf("denied probe must not retry within the epoch: lists %d -> %d", lists, got)
	}
}

// TestNoteCapabilityOutcome_EvidenceRules: success marks allowed; a 403 from
// the SPECIFIC operation marks denied; 401 NEVER touches the mirror; a 403
// from a flow's internal read step never misattributes to a write capability.
func TestNoteCapabilityOutcome_EvidenceRules(t *testing.T) {
	p := newHealthPlugin(nil)

	p.noteCapabilityOutcome("ep1", capVMUpdate, nil, p.capEpoch("ep1"))
	if got := p.capabilityValue("ep1", capVMUpdate); got != capAllowed {
		t.Errorf("after success: vm_update = %q, want allowed", got)
	}

	putErr := fmt.Errorf("update vm: %w",
		&api.APIError{Method: http.MethodPut, Path: "/api/nutanix/v3/vms/x", Status: 403})
	p.noteCapabilityOutcome("ep1", capVMUpdate, putErr, p.capEpoch("ep1"))
	if got := p.capabilityValue("ep1", capVMUpdate); got != capDenied {
		t.Errorf("after PUT 403: vm_update = %q, want denied", got)
	}

	// 401 is session expiry, not an authorization signal.
	p.noteCapabilityOutcome("ep1", capVMPower,
		&api.APIError{Method: http.MethodPut, Path: "/api/nutanix/v3/vms/x", Status: 401},
		p.capEpoch("ep1"))
	if got := p.capabilityValue("ep1", capVMPower); got != capUnknown {
		t.Errorf("after 401: vm_power = %q, want unknown (401 never touches the mirror)", got)
	}

	// A 403 on the VM GET inside a power flow is a read denial, not power.
	p.noteCapabilityOutcome("ep1", capVMPower, fmt.Errorf("get vm: %w",
		&api.APIError{Method: http.MethodGet, Path: "/api/nutanix/v3/vms/x", Status: 403}),
		p.capEpoch("ep1"))
	if got := p.capabilityValue("ep1", capVMPower); got != capUnknown {
		t.Errorf("GET 403 misattributed to vm_power: %q", got)
	}

	// v4 power $actions 403 IS power evidence.
	p.noteCapabilityOutcome("ep1", capVMPower, fmt.Errorf("power ON: %w",
		&api.APIError{Method: http.MethodPost, Path: "/api/vmm/v4.0/ahv/config/vms/x/$actions/power-on", Status: 403}),
		p.capEpoch("ep1"))
	if got := p.capabilityValue("ep1", capVMPower); got != capDenied {
		t.Errorf("after v4 $actions 403: vm_power = %q, want denied", got)
	}

	// Image create evidence: the catalog POST and the bytes PUT both count.
	p.noteCapabilityOutcome("ep1", capImageCreate, fmt.Errorf("create image: %w",
		&api.APIError{Method: http.MethodPost, Path: "/api/nutanix/v3/images", Status: 403}),
		p.capEpoch("ep1"))
	if got := p.capabilityValue("ep1", capImageCreate); got != capDenied {
		t.Errorf("after images POST 403: image_create = %q, want denied", got)
	}
	p.noteCapabilityOutcome("ep2", capImageCreate, fmt.Errorf("upload bytes: %w",
		&api.APIError{Method: http.MethodPut, Path: "/api/nutanix/v3/images/u-1/file", Status: 403}),
		p.capEpoch("ep2"))
	if got := p.capabilityValue("ep2", capImageCreate); got != capDenied {
		t.Errorf("after bytes PUT 403: image_create = %q, want denied", got)
	}
}

// TestCapabilityEpoch_InvalidateEndpointClears: endpoint invalidation ends the
// auth epoch — every field returns to unknown.
func TestCapabilityEpoch_InvalidateEndpointClears(t *testing.T) {
	p := newHealthPlugin(nil)
	p.setCapability("ep1", capVMUpdate, false, p.capEpoch("ep1"))
	p.setAgentISOInCatalog("ep1", true, p.capEpoch("ep1"))

	p.invalidateEndpoint("ep1")

	caps := p.capabilitiesSnapshot("ep1")
	if caps.VMUpdate != capUnknown || caps.AgentISOInCatalog {
		t.Errorf("invalidate must clear the mirror, got %+v", caps)
	}
}

// TestCapabilityEpoch_StaleProbeDropped: a Layer-1 probe that stalls across an
// auth-epoch boundary must not write into the fresh epoch — its results
// describe the OLD credentials. The late completion leaves the new epoch
// all-unknown AND probePending true, so the fresh epoch still probes.
func TestCapabilityEpoch_StaleProbeDropped(t *testing.T) {
	iso := []byte("FAKEISO")
	f := newPrismFake(t, "ON", nil, iso)
	f.listGate = make(chan struct{})
	p := newCyclePlugin(t, f, iso)

	// The probe captures its epoch at kick time, then stalls inside the
	// catalog list (the fake holds the response until listGate closes).
	p.kickCapabilityProbe("ep1", p.clients["ep1"])

	// Auth-epoch boundary while the probe is in flight.
	p.mu.Lock()
	p.resetCapabilitiesLocked("ep1")
	p.mu.Unlock()

	close(f.listGate)
	waitTimeout(&p.bgWG, 5*time.Second)

	caps := p.capabilitiesSnapshot("ep1")
	if caps.ImageRead != capUnknown || caps.AgentISOInCatalog {
		t.Errorf("stale probe wrote into the fresh epoch: %+v", caps)
	}
	if !p.capabilityProbePending("ep1") {
		t.Error("probePending must stay true so the fresh epoch re-probes")
	}
}

// TestCapabilityEpoch_StaleEvidenceDropped: evidence captured under epoch N
// and applied after an epoch boundary is a no-op — and a stale write after
// invalidateEndpoint must not auto-vivify the deleted mirror entry.
func TestCapabilityEpoch_StaleEvidenceDropped(t *testing.T) {
	p := newHealthPlugin(nil)
	epoch := p.capEpoch("ep1")
	putErr := fmt.Errorf("update vm: %w",
		&api.APIError{Method: http.MethodPut, Path: "/api/nutanix/v3/vms/x", Status: 403})

	// Epoch boundary between the operation's start and its completion.
	p.mu.Lock()
	p.resetCapabilitiesLocked("ep1")
	p.mu.Unlock()

	p.noteCapabilityOutcome("ep1", capVMUpdate, putErr, epoch)
	if got := p.capabilityValue("ep1", capVMUpdate); got != capUnknown {
		t.Errorf("stale 403 applied across the epoch boundary: vm_update = %q, want unknown", got)
	}
	if !p.capabilityProbePending("ep1") {
		t.Error("probePending must stay true after a dropped stale write")
	}

	// Stale SUCCESS after the entry was deleted outright.
	epoch = p.capEpoch("ep1")
	p.invalidateEndpoint("ep1")
	p.noteCapabilityOutcome("ep1", capVMUpdate, nil, epoch)
	p.mu.RLock()
	_, vivified := p.capabilities["ep1"]
	p.mu.RUnlock()
	if vivified {
		t.Error("stale write after invalidateEndpoint must not re-create the mirror entry")
	}
}

// TestCapabilityEpoch_CookieRotationClears: a captured Set-Cookie rotation is
// an auth-epoch boundary — evidence from the previous cookie set is dropped
// and the Layer-1 probes re-arm.
func TestCapabilityEpoch_CookieRotationClears(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Add("Set-Cookie", "NTNX_MERCURY_IAM_SESSION=rotated-value; Path=/; HttpOnly")
		writeFakeJSON(w, http.StatusOK, map[string]any{})
	}))
	defer srv.Close()

	ep := sdk.EndpointRef{ID: "ep1", URL: srv.URL, PluginID: pluginID, AuthMethods: []sdk.AuthKind{sdk.AuthKindCookie}}
	p := newRotationPlugin([]sdk.EndpointRef{ep}, newRecordingKeystore())
	cookies := []api.SessionCookie{{Name: "NTNX_MERCURY_IAM_SESSION", Value: "initial-value"}}
	client, err := p.newCookieClient(ep, cookies)
	if err != nil {
		t.Fatalf("newCookieClient: %v", err)
	}
	p.replaceClient(ep.ID, client, &endpointState{method: sdk.AuthKindCookie, sessionCookies: cookies})

	// Evidence learned in this epoch...
	p.setCapability("ep1", capVMPower, false, p.capEpoch("ep1"))

	// ...is wiped when a request captures a cookie rotation.
	if err := client.VerifyAuth(context.Background()); err != nil {
		t.Fatalf("VerifyAuth: %v", err)
	}
	caps := p.capabilitiesSnapshot("ep1")
	if caps.VMPower != capUnknown {
		t.Errorf("rotation must reset evidence: vm_power = %q, want unknown", caps.VMPower)
	}
	if !p.capabilityProbePending("ep1") {
		t.Error("rotation must re-arm the Layer-1 probes")
	}
}

// TestMountAgentISO_PowerCycleDenied403: the consented cycle hits a 403 on
// the power PUT. The cycle aborts with the VM exactly as found, exactly ONE
// power attempt (no retry), the deferred apply armed, the ISO attached hot,
// SerialNote=serial_port_power_denied, and vm_power mirrored as denied.
func TestMountAgentISO_PowerCycleDenied403(t *testing.T) {
	iso := []byte("FAKEISO")
	f := newPrismFake(t, "ON", nil, iso)
	f.powerPutForbidden = true
	p := newCyclePlugin(t, f, iso)

	res, err := p.MountAgentISO(context.Background(), "sess1", sdk.ISOMountOptions{PowerCycle: true})
	if err != nil {
		t.Fatalf("mount must still succeed (ISO attaches hot): %v", err)
	}
	if res.SerialNote != "serial_port_power_denied" {
		t.Errorf("serial note = %q, want serial_port_power_denied", res.SerialNote)
	}
	if got := f.PowerPuts(); len(got) != 1 || got[0] != "OFF:ACPI" {
		t.Errorf("power puts = %v, want exactly one denied [OFF:ACPI] attempt", got)
	}
	if f.Power() != "ON" {
		t.Errorf("VM must be left as found (ON), got %q", f.Power())
	}
	if hasSerialIndex(f.Serial(), 2) {
		t.Error("serial port must NOT be added while the VM is running")
	}
	if f.CDROMImage() != "img-1" {
		t.Errorf("ISO must still attach hot, got %q", f.CDROMImage())
	}
	if !p.deferredArmed("ep1", cycleVMUUID) {
		t.Error("deferred apply must be armed after a power-denied cycle")
	}
	if got := p.capabilityValue("ep1", capVMPower); got != capDenied {
		t.Errorf("vm_power = %q, want denied", got)
	}
	// The hot attach is a successful spec PUT — vm_update evidence.
	if got := p.capabilityValue("ep1", capVMUpdate); got != capAllowed {
		t.Errorf("vm_update = %q, want allowed (hot attach succeeded)", got)
	}
}

// TestDeferredSerial_RBACDeniedDropsEntry: the deferred apply's spec PUT gets
// a 403 — the entry is dropped (no point retrying every power-off until
// roles change) and vm_update is mirrored as denied.
func TestDeferredSerial_RBACDeniedDropsEntry(t *testing.T) {
	iso := []byte("FAKEISO")
	f := newPrismFake(t, "OFF", nil, iso)
	f.specPutForbidden = true
	p := newCyclePlugin(t, f, iso)
	p.armDeferredSerialAdd("ep1", cycleVMUUID)

	p.applyDeferredSerial(context.Background())

	if p.deferredArmed("ep1", cycleVMUUID) {
		t.Error("entry must be dropped after an RBAC-denied apply")
	}
	if got := p.capabilityValue("ep1", capVMUpdate); got != capDenied {
		t.Errorf("vm_update = %q, want denied", got)
	}
	if hasSerialIndex(f.Serial(), 2) {
		t.Error("denied PUT must not have changed the spec")
	}
	if got := f.PowerPuts(); len(got) != 0 {
		t.Errorf("the deferred path must NEVER send power ops: %v", got)
	}
}

// TestMountAgentISO_CatalogPresentSkipsCreateWhenDenied: image_create is
// proven denied but the agent ISO is already published — the mount reuses it
// end-to-end with ZERO image-create calls (the full flow works for
// VM-update-only operators).
func TestMountAgentISO_CatalogPresentSkipsCreateWhenDenied(t *testing.T) {
	iso := []byte("FAKEISO")
	f := newPrismFake(t, "ON", []int{2}, iso)
	p := newCyclePlugin(t, f, iso)
	p.setCapability("ep1", capImageCreate, false, p.capEpoch("ep1"))

	res, err := p.MountAgentISO(context.Background(), "sess1", sdk.ISOMountOptions{})
	if err != nil {
		t.Fatalf("mount must succeed via the published image: %v", err)
	}
	if res.ImageUUID != "img-1" {
		t.Errorf("image uuid = %q, want img-1 (reused)", res.ImageUUID)
	}
	if f.ImageCreates() != 0 {
		t.Errorf("image create calls = %d, want 0", f.ImageCreates())
	}
	caps := p.capabilitiesSnapshot("ep1")
	if !caps.AgentISOInCatalog {
		t.Error("agent_iso_in_catalog must be true after a reuse mount")
	}
	if caps.ImageCreate != capDenied {
		t.Errorf("image_create = %q, must stay denied (no create was exercised)", caps.ImageCreate)
	}
	if caps.VMUpdate != capAllowed {
		t.Errorf("vm_update = %q, want allowed (attach PUT succeeded)", caps.VMUpdate)
	}
}

// TestMountAgentISO_ImagePublishRequired: catalog missing + image_create
// denied surfaces the typed image_publish_required outcome WITHOUT attempting
// the upload — zero image-create calls, zero VM spec writes.
func TestMountAgentISO_ImagePublishRequired(t *testing.T) {
	iso := []byte("FAKEISO")
	f := newPrismFake(t, "ON", nil, iso)
	f.catalogEmpty = true
	p := newCyclePlugin(t, f, iso)
	p.setCapability("ep1", capImageCreate, false, p.capEpoch("ep1"))

	_, err := p.MountAgentISO(context.Background(), "sess1", sdk.ISOMountOptions{})
	if err == nil {
		t.Fatal("expected image_publish_required error")
	}
	if !errors.Is(err, sdk.ErrImagePublishRequired) {
		t.Errorf("error %q must wrap sdk.ErrImagePublishRequired (the router maps it to the wire code)", err)
	}
	if f.ImageCreates() != 0 {
		t.Errorf("image create calls = %d, want 0", f.ImageCreates())
	}
	// The single setup control subsumes the old standalone channel add: the
	// blocked image half must NOT block the channel half, so exactly one
	// spec PUT happens — the best-effort COM3 ensure, never the attach.
	if f.SpecPuts() != 1 {
		t.Errorf("spec puts = %d, want 1 (the best-effort channel ensure only)", f.SpecPuts())
	}
	// VM is ON in this fake, so the ensure ends PowerCycleRequired and the
	// deferred add must be armed despite the typed image failure.
	if !p.deferredArmed("ep1", cycleVMUUID) {
		t.Error("deferred channel add must be armed by the best-effort ensure on a running VM")
	}
	if caps := p.capabilitiesSnapshot("ep1"); caps.AgentISOInCatalog {
		t.Error("agent_iso_in_catalog must be false after the presence re-check")
	}
}

// TestMountAgentISO_CreateDeniedReusesDespiteStaleDescription: image_create is
// proven denied and the catalog carries a COMPLETE name-matched entry whose
// description SHA does NOT match the local ISO (an administrator published an
// older build). The mount must reuse THAT entry's UUID — never the
// delete/create machinery (a DELETE on the admin's image and the POST /images
// would both 403 and burn the retry budget).
func TestMountAgentISO_CreateDeniedReusesDespiteStaleDescription(t *testing.T) {
	iso := []byte("FAKEISO")
	f := newPrismFake(t, "ON", []int{2}, iso)
	f.imageDesc = "agent iso | sha256:" + strings.Repeat("0", 64)
	p := newCyclePlugin(t, f, iso)
	p.setCapability("ep1", capImageCreate, false, p.capEpoch("ep1"))

	res, err := p.MountAgentISO(context.Background(), "sess1", sdk.ISOMountOptions{})
	if err != nil {
		t.Fatalf("mount must reuse the published image despite the stale description: %v", err)
	}
	if res.ImageUUID != "img-1" {
		t.Errorf("image uuid = %q, want img-1 (the published entry)", res.ImageUUID)
	}
	if f.ImageCreates() != 0 {
		t.Errorf("image create calls = %d, want 0", f.ImageCreates())
	}
	if f.ImageDeletes() != 0 {
		t.Errorf("image delete calls = %d, want 0 (never delete the admin's image)", f.ImageDeletes())
	}
	if f.CDROMImage() != "img-1" {
		t.Errorf("ISO not attached from the published image: %q", f.CDROMImage())
	}
}

// TestEndpointHealth_CarriesCapabilities: the health payload serializes the
// capability mirror exactly as frozen — even when the host is offline (the
// mirror is local cached state, not an upstream probe).
func TestEndpointHealth_CarriesCapabilities(t *testing.T) {
	p := newHealthPlugin([]sdk.EndpointRef{{
		ID:          "ep1",
		URL:         "https://prism.invalid:9440",
		PluginID:    pluginID,
		AuthMethods: []sdk.AuthKind{sdk.AuthKindCookie},
	}})
	p.setCapability("ep1", capVMPower, false, p.capEpoch("ep1"))
	p.setAgentISOInCatalog("ep1", true, p.capEpoch("ep1"))

	h, err := p.EndpointHealth(context.Background(), "ep1")
	if err != nil {
		t.Fatalf("EndpointHealth: %v", err)
	}
	raw, err := json.Marshal(h)
	if err != nil {
		t.Fatalf("marshal health: %v", err)
	}
	want := `"capabilities":{"image_read":"unknown","image_create":"unknown",` +
		`"vm_update":"unknown","vm_power":"denied","agent_iso_in_catalog":true}`
	if !strings.Contains(string(raw), want) {
		t.Errorf("health payload = %s, want it to contain %s", raw, want)
	}
}
