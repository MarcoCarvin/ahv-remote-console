package ahv

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MarcoCarvin/ahv-remote-console/apps/runtime/pkg/sdk"
	"github.com/MarcoCarvin/ahv-remote-console/plugins/ahv/internal/api"
)

const cycleVMUUID = "11111111-2222-3333-4444-555555555555"

// prismFake is an httptest-backed Prism Central v3 fake with a VM power-state
// machine, covering everything MountAgentISO / AddSerialPort / the deferred
// apply touch: images (reuse path), the VM document GET/PUT, and tasks.
//
// PUT /vms classification: a body whose spec power_state differs from the
// fake's current state is a POWER op (the spec-edit paths round-trip the
// current state untouched); anything else is a SPEC edit. Power OFF asserts
// the graceful ACPI mechanism — a hard off fails the test immediately.
type prismFake struct {
	t  *testing.T
	mu sync.Mutex

	power      string // "ON" / "OFF"
	serial     []int  // serial port indexes in the spec
	cdromImage string // data_source_reference uuid in the single CD-ROM ("" = empty)
	specVer    int

	// acpiIgnored simulates a guest that never honors the ACPI shutdown: the
	// power-off PUT is accepted (task SUCCEEDED) but the state stays ON.
	acpiIgnored bool
	// specEditTaskFails forces every spec-edit task to FAILED (nothing applied).
	specEditTaskFails bool
	// powerPutForbidden / specPutForbidden simulate Prism RBAC: the matching
	// VM PUT is rejected with HTTP 403 and no state changes.
	powerPutForbidden bool
	specPutForbidden  bool
	// listForbidden 403s the image catalog list; catalogEmpty returns an
	// empty catalog (agent ISO not published).
	listForbidden bool
	catalogEmpty  bool
	// listGate, when non-nil, holds every catalog-list response until the
	// channel is closed — lets a test freeze a probe mid-flight.
	listGate chan struct{}

	imageName string
	imageDesc string

	tasks        map[string]string // task uuid -> status
	powerPuts    []string          // "OFF:ACPI", "ON:" ...
	events       []string          // ordered PUTs: "power:OFF", "spec(serial+cdrom)@OFF", ...
	specPuts     int
	imageLists   int // POST /images/list count
	imageCreates int // POST /images count (catalog entry creation)
	imageDeletes int // DELETE /images/{uuid} count (override/dedupe cleanup)
}

func newPrismFake(t *testing.T, power string, serial []int, isoBytes []byte) *prismFake {
	sum := sha256.Sum256(isoBytes)
	return &prismFake{
		t:         t,
		power:     power,
		serial:    append([]int(nil), serial...),
		specVer:   5,
		imageName: agentImageName(false, ""),
		imageDesc: "agent iso | sha256:" + hex.EncodeToString(sum[:]),
		tasks:     map[string]string{},
	}
}

func (f *prismFake) vmDocLocked() []byte {
	ports := make([]map[string]any, 0, len(f.serial))
	for _, idx := range f.serial {
		ports = append(ports, map[string]any{"index": idx, "is_connected": true})
	}
	cdrom := map[string]any{
		"device_properties": map[string]any{
			"device_type":  "CDROM",
			"disk_address": map[string]any{"adapter_type": "IDE", "device_index": 0},
		},
	}
	if f.cdromImage != "" {
		cdrom["data_source_reference"] = map[string]any{"kind": "image", "uuid": f.cdromImage}
	}
	doc := map[string]any{
		"metadata": map[string]any{"kind": "vm", "uuid": cycleVMUUID, "spec_version": f.specVer},
		"spec": map[string]any{
			"name": "cycle-vm",
			"resources": map[string]any{
				"power_state":      f.power,
				"disk_list":        []any{cdrom},
				"serial_port_list": ports,
				"num_sockets":      2,
			},
		},
		"status": map[string]any{
			"name":              "cycle-vm",
			"cluster_reference": map[string]any{"uuid": ""},
			"resources":         map[string]any{"power_state": f.power},
		},
	}
	b, _ := json.Marshal(doc)
	return b
}

func (f *prismFake) handlePutVM(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(io.LimitReader(r.Body, 32*1024*1024))
	var put struct {
		Spec struct {
			Resources struct {
				PowerState          string `json:"power_state"`
				PowerStateMechanism struct {
					Mechanism string `json:"mechanism"`
				} `json:"power_state_mechanism"`
				SerialPortList []struct {
					Index int `json:"index"`
				} `json:"serial_port_list"`
				DiskList []struct {
					DeviceProperties struct {
						DeviceType string `json:"device_type"`
					} `json:"device_properties"`
					DataSourceReference *struct {
						UUID string `json:"uuid"`
					} `json:"data_source_reference"`
				} `json:"disk_list"`
			} `json:"resources"`
		} `json:"spec"`
	}
	if err := json.Unmarshal(body, &put); err != nil {
		f.t.Errorf("vm PUT body did not parse: %v", err)
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	f.mu.Lock()
	taskID := fmt.Sprintf("task-%d", len(f.tasks)+1)
	if !strings.EqualFold(put.Spec.Resources.PowerState, f.power) {
		// POWER op.
		state := strings.ToUpper(put.Spec.Resources.PowerState)
		mech := put.Spec.Resources.PowerStateMechanism.Mechanism
		f.powerPuts = append(f.powerPuts, state+":"+mech)
		if f.powerPutForbidden {
			// Prism RBAC denies the power op: the attempt is recorded, the
			// state never changes.
			f.events = append(f.events, "power:"+state+":denied")
			f.mu.Unlock()
			w.WriteHeader(http.StatusForbidden)
			return
		}
		f.events = append(f.events, "power:"+state)
		if state == "OFF" {
			if mech != "ACPI" {
				f.t.Errorf("SAFETY VIOLATION: power-off without ACPI mechanism (got %q)", mech)
			}
			if !f.acpiIgnored {
				f.power = "OFF"
			}
		} else {
			f.power = "ON"
		}
		f.tasks[taskID] = "SUCCEEDED"
	} else {
		// SPEC edit.
		f.specPuts++
		if f.specPutForbidden {
			// Prism RBAC denies VM spec updates: attempt counted, no change.
			f.events = append(f.events, "spec:denied")
			f.mu.Unlock()
			w.WriteHeader(http.StatusForbidden)
			return
		}
		serialGrew := len(put.Spec.Resources.SerialPortList) > len(f.serial)
		newImage := ""
		for _, d := range put.Spec.Resources.DiskList {
			if d.DeviceProperties.DeviceType == "CDROM" && d.DataSourceReference != nil {
				newImage = d.DataSourceReference.UUID
				break
			}
		}
		cdromChanged := newImage != "" && newImage != f.cdromImage
		parts := []string{}
		if serialGrew {
			parts = append(parts, "serial")
		}
		if cdromChanged {
			parts = append(parts, "cdrom")
		}
		f.events = append(f.events, fmt.Sprintf("spec(%s)@%s", strings.Join(parts, "+"), f.power))
		switch {
		case f.specEditTaskFails:
			f.tasks[taskID] = "FAILED"
		case serialGrew && strings.EqualFold(f.power, "ON"):
			// AHV cannot hot-add a serial port to a running VM.
			f.tasks[taskID] = "FAILED"
		default:
			f.tasks[taskID] = "SUCCEEDED"
			if serialGrew {
				indexes := make([]int, 0, len(put.Spec.Resources.SerialPortList))
				for _, sp := range put.Spec.Resources.SerialPortList {
					indexes = append(indexes, sp.Index)
				}
				f.serial = indexes
			}
			if cdromChanged {
				f.cdromImage = newImage
			}
			f.specVer++
		}
	}
	f.mu.Unlock()

	resp := map[string]any{
		"metadata": map[string]any{"uuid": cycleVMUUID},
		"status":   map[string]any{"execution_context": map[string]any{"task_uuid": taskID}},
	}
	writeFakeJSON(w, http.StatusAccepted, resp)
}

func (f *prismFake) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/nutanix/v3/users/me", func(w http.ResponseWriter, _ *http.Request) {
		writeFakeJSON(w, http.StatusOK, map[string]any{})
	})
	mux.HandleFunc("/api/nutanix/v3/images/list", func(w http.ResponseWriter, _ *http.Request) {
		f.mu.Lock()
		f.imageLists++
		name, desc := f.imageName, f.imageDesc
		forbidden, empty := f.listForbidden, f.catalogEmpty
		gate := f.listGate
		f.mu.Unlock()
		if gate != nil {
			<-gate
		}
		if forbidden {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		if empty {
			writeFakeJSON(w, http.StatusOK, map[string]any{"entities": []map[string]any{}})
			return
		}
		writeFakeJSON(w, http.StatusOK, map[string]any{
			"entities": []map[string]any{{
				"metadata": map[string]any{"uuid": "img-1", "creation_time": time.Now().UTC().Format(time.RFC3339)},
				"spec":     map[string]any{"name": name, "description": desc, "resources": map[string]any{}},
				"status":   map[string]any{"state": "COMPLETE"},
			}},
		})
	})
	mux.HandleFunc("/api/nutanix/v3/images", func(w http.ResponseWriter, _ *http.Request) {
		// Catalog-entry creation. The capability tests only ever assert this
		// was NEVER reached, so a 403 (denied-role world) keeps any stray
		// caller honest without minting fake entries.
		f.mu.Lock()
		f.imageCreates++
		f.mu.Unlock()
		w.WriteHeader(http.StatusForbidden)
	})
	mux.HandleFunc("/api/nutanix/v3/images/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			// Override/dedupe cleanup. The create-denied tests assert this is
			// NEVER reached (a DELETE on an admin-published image would 403).
			f.mu.Lock()
			f.imageDeletes++
			f.mu.Unlock()
			w.WriteHeader(http.StatusForbidden)
			return
		}
		writeFakeJSON(w, http.StatusOK, map[string]any{"status": map[string]any{"state": "COMPLETE"}})
	})
	mux.HandleFunc("/api/nutanix/v3/tasks/", func(w http.ResponseWriter, r *http.Request) {
		id := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
		f.mu.Lock()
		st, ok := f.tasks[id]
		f.mu.Unlock()
		if !ok {
			st = "SUCCEEDED"
		}
		writeFakeJSON(w, http.StatusOK, map[string]any{"status": st, "progress_info": map[string]any{"percentage_complete": 100}})
	})
	mux.HandleFunc("/api/nutanix/v3/vms/"+cycleVMUUID, func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			f.mu.Lock()
			body := f.vmDocLocked()
			f.mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(body)
		case http.MethodPut:
			f.handlePutVM(w, r)
		default:
			http.Error(w, "method", http.StatusMethodNotAllowed)
		}
	})
	// v3 groups/arithmos query — the live VM stats source. Read-only POST; the
	// GetVMStats plugin test reads the canned sample below.
	mux.HandleFunc("/api/nutanix/v3/groups", func(w http.ResponseWriter, _ *http.Request) {
		gv := func(name, value string) map[string]any {
			return map[string]any{
				"name":   name,
				"values": []any{map[string]any{"values": []any{value}}},
			}
		}
		// Attribute names mirror internal/api/stats.go's canonical arithmos
		// constants (attrCPUUsagePpm…attrNetTxBps). Disk metrics are KBps on the
		// wire and scaled ×1024 to bytes/sec by GetVMStats, so 1/2 KBps here
		// becomes 1024/2048 B/s in the parsed sample.
		writeFakeJSON(w, http.StatusOK, map[string]any{
			"entity_type": "mh_vm",
			"group_results": []any{map[string]any{
				"entity_results": []any{map[string]any{
					"data": []any{
						gv("hypervisor_cpu_usage_ppm", "250000"),
						gv("memory_usage_ppm", "500000"),
						gv("controller_read_io_bandwidth_kBps", "1"),  // disk read → 1024 B/s
						gv("controller_write_io_bandwidth_kBps", "2"), // disk write → 2048 B/s
						gv("transmitted_rate_kBps", "4"),              // net tx KBps → 4096 B/s
						gv("received_rate_kBps", "8"),                 // net rx KBps → 8192 B/s
					},
				}},
			}},
		})
	})
	return mux
}

func writeFakeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// ---- locked accessors for test assertions ----

func (f *prismFake) Events() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.events...)
}
func (f *prismFake) PowerPuts() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.powerPuts...)
}
func (f *prismFake) Power() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.power
}
func (f *prismFake) SetPower(s string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.power = s
}
func (f *prismFake) Serial() []int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]int(nil), f.serial...)
}
func (f *prismFake) CDROMImage() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.cdromImage
}
func (f *prismFake) SpecPuts() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.specPuts
}
func (f *prismFake) ImageLists() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.imageLists
}
func (f *prismFake) ImageCreates() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.imageCreates
}
func (f *prismFake) ImageDeletes() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.imageDeletes
}

func hasSerialIndex(list []int, idx int) bool {
	for _, i := range list {
		if i == idx {
			return true
		}
	}
	return false
}

// newCyclePlugin wires a *Plugin against the fake with one signed-in
// endpoint, one open session on cycleVMUUID, and a tiny ACPI-wait budget.
func newCyclePlugin(t *testing.T, f *prismFake, isoBytes []byte) *Plugin {
	t.Helper()
	srv := httptest.NewServer(f.handler())
	t.Cleanup(srv.Close)

	isoPath := filepath.Join(t.TempDir(), "agent.iso")
	if err := os.WriteFile(isoPath, isoBytes, 0o600); err != nil {
		t.Fatalf("write iso: %v", err)
	}
	t.Setenv("AHV_REMOTE_CONSOLE_AGENT_ISO_PATH", isoPath)
	t.Setenv("AHV_REMOTE_CONSOLE_DEV", "")

	ep := sdk.EndpointRef{ID: "ep1", URL: srv.URL, PluginID: pluginID, AuthMethods: []sdk.AuthKind{sdk.AuthKindCookie}}
	p := &Plugin{
		deps: sdk.Dependencies{
			Logger:   testLogger{newDiscardLogger()},
			Keystore: emptyKeystore{},
			Config:   stubConfig{eps: []sdk.EndpointRef{ep}},
		},
		state:          map[string]*endpointState{},
		clients:        map[string]*api.Client{},
		sessions:       map[string]*sessionState{},
		cache:          newResourceCache(time.Second),
		clusterUUIDs:   map[string]consoleTarget{},
		pendingSerial:  map[string]pendingSerialAdd{},
		cycleOffBudget: 300 * time.Millisecond,
		cyclePollEvery: 10 * time.Millisecond,
	}
	p.bgCtx, p.bgCancel = context.WithCancel(context.Background())
	t.Cleanup(func() {
		p.bgCancel()
		waitTimeout(&p.bgWG, 5*time.Second)
	})

	client, err := api.NewClient(api.ClientOptions{BaseURL: srv.URL})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	p.clients["ep1"] = client
	p.sessions["sess1"] = &sessionState{
		EndpointID: "ep1",
		ResourceID: "ep1/" + cycleVMUUID,
		VMUUID:     cycleVMUUID,
		OpenedAt:   time.Now(),
	}
	return p
}

func (p *Plugin) deferredArmed(endpointID, vmUUID string) bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	_, ok := p.pendingSerial[deferredSerialKey(endpointID, vmUUID)]
	return ok
}

// TestMountAgentISO_PowerCycleHappyPath: consented cycle on a running VM
// without COM3 — strict order ACPI OFF → ONE merged spec PUT (serial+cdrom,
// applied while OFF) → power ON, and the result reports
// serial_port_added_cycled.
func TestMountAgentISO_PowerCycleHappyPath(t *testing.T) {
	iso := []byte("FAKEISO")
	f := newPrismFake(t, "ON", nil, iso)
	p := newCyclePlugin(t, f, iso)

	res, err := p.MountAgentISO(context.Background(), "sess1", sdk.ISOMountOptions{PowerCycle: true})
	if err != nil {
		t.Fatalf("mount: %v", err)
	}
	if res.SerialNote != "serial_port_added_cycled" {
		t.Errorf("serial note = %q, want serial_port_added_cycled", res.SerialNote)
	}
	if res.ImageUUID != "img-1" {
		t.Errorf("image uuid = %q, want img-1", res.ImageUUID)
	}
	wantEvents := []string{"power:OFF", "spec(serial+cdrom)@OFF", "power:ON"}
	if got := f.Events(); strings.Join(got, ",") != strings.Join(wantEvents, ",") {
		t.Errorf("event order = %v, want %v", got, wantEvents)
	}
	if f.Power() != "ON" {
		t.Errorf("VM left in power state %q, want ON", f.Power())
	}
	if !hasSerialIndex(f.Serial(), 2) {
		t.Errorf("COM3 not in spec after cycle: %v", f.Serial())
	}
	if f.CDROMImage() != "img-1" {
		t.Errorf("ISO not attached: %q", f.CDROMImage())
	}
	if p.deferredArmed("ep1", cycleVMUUID) {
		t.Errorf("deferred entry must not be armed after a successful cycle")
	}
}

// TestMountAgentISO_ACPITimeoutDefersWithoutForcing: the guest ignores ACPI.
// The cycle must abort with NO hard-off and NO power-on, leave power as
// found, attach the ISO hot, arm the deferred apply, and report
// serial_port_deferred.
func TestMountAgentISO_ACPITimeoutDefersWithoutForcing(t *testing.T) {
	iso := []byte("FAKEISO")
	f := newPrismFake(t, "ON", nil, iso)
	f.acpiIgnored = true
	p := newCyclePlugin(t, f, iso)

	res, err := p.MountAgentISO(context.Background(), "sess1", sdk.ISOMountOptions{PowerCycle: true})
	if err != nil {
		t.Fatalf("mount must still succeed (ISO attaches hot): %v", err)
	}
	if res.SerialNote != "serial_port_deferred" {
		t.Errorf("serial note = %q, want serial_port_deferred", res.SerialNote)
	}
	if got := f.PowerPuts(); len(got) != 1 || got[0] != "OFF:ACPI" {
		t.Errorf("power puts = %v, want exactly [OFF:ACPI] (no hard-off, no power-on)", got)
	}
	if f.Power() != "ON" {
		t.Errorf("power state must be left as found (ON), got %q", f.Power())
	}
	if hasSerialIndex(f.Serial(), 2) {
		t.Errorf("serial port must NOT be added while the VM is running")
	}
	if f.CDROMImage() != "img-1" {
		t.Errorf("ISO must still attach hot, got %q", f.CDROMImage())
	}
	if !p.deferredArmed("ep1", cycleVMUUID) {
		t.Errorf("deferred apply must be armed after an aborted cycle")
	}
}

// TestMountAgentISO_PostOffFailureStillPowersOn: the merged PUT fails after
// the VM reached OFF. The flow turned it off, so the flow MUST power it back
// on, and the original error is reported.
func TestMountAgentISO_PostOffFailureStillPowersOn(t *testing.T) {
	iso := []byte("FAKEISO")
	f := newPrismFake(t, "ON", nil, iso)
	f.specEditTaskFails = true
	p := newCyclePlugin(t, f, iso)

	_, err := p.MountAgentISO(context.Background(), "sess1", sdk.ISOMountOptions{PowerCycle: true})
	if err == nil {
		t.Fatal("expected the merged-update failure to surface")
	}
	got := f.Events()
	if len(got) != 3 || got[0] != "power:OFF" || !strings.HasPrefix(got[1], "spec(") || got[2] != "power:ON" {
		t.Errorf("event order = %v, want [power:OFF spec(...) power:ON]", got)
	}
	if f.Power() != "ON" {
		t.Errorf("VM must be powered back ON after a post-OFF failure, got %q", f.Power())
	}
}

// TestMountAgentISO_NoConsentNeverTouchesPower: power_cycle=false on a
// running VM without COM3 — zero power PUTs, hot ISO attach, the
// power_off_required note, and the deferred apply armed.
func TestMountAgentISO_NoConsentNeverTouchesPower(t *testing.T) {
	iso := []byte("FAKEISO")
	f := newPrismFake(t, "ON", nil, iso)
	p := newCyclePlugin(t, f, iso)

	res, err := p.MountAgentISO(context.Background(), "sess1", sdk.ISOMountOptions{})
	if err != nil {
		t.Fatalf("mount: %v", err)
	}
	if got := f.PowerPuts(); len(got) != 0 {
		t.Fatalf("power_cycle=false sent power ops: %v", got)
	}
	if res.SerialNote != "serial_port_power_off_required" {
		t.Errorf("serial note = %q, want serial_port_power_off_required", res.SerialNote)
	}
	if f.CDROMImage() != "img-1" {
		t.Errorf("ISO not attached: %q", f.CDROMImage())
	}
	if !p.deferredArmed("ep1", cycleVMUUID) {
		t.Errorf("deferred apply must be armed when the serial add needs a power cycle")
	}
}

// TestMountAgentISO_SerialPresentSkipsCycle: COM3 already present — even with
// consent, no power op fires and only the CD-ROM mutation is sent.
func TestMountAgentISO_SerialPresentSkipsCycle(t *testing.T) {
	iso := []byte("FAKEISO")
	f := newPrismFake(t, "ON", []int{2}, iso)
	p := newCyclePlugin(t, f, iso)

	res, err := p.MountAgentISO(context.Background(), "sess1", sdk.ISOMountOptions{PowerCycle: true})
	if err != nil {
		t.Fatalf("mount: %v", err)
	}
	if got := f.PowerPuts(); len(got) != 0 {
		t.Fatalf("COM3 present must skip the cycle, got power ops: %v", got)
	}
	if res.SerialNote != "serial_port_present" {
		t.Errorf("serial note = %q, want serial_port_present", res.SerialNote)
	}
	wantEvents := []string{"spec(cdrom)@ON"}
	if got := f.Events(); strings.Join(got, ",") != strings.Join(wantEvents, ",") {
		t.Errorf("events = %v, want %v", got, wantEvents)
	}
}

// TestMountAgentISO_MergedPUTWhenAlreadyOff: VM already OFF and COM3 absent —
// the merged single-PUT applies both edits with NO power op (no cycle needed).
func TestMountAgentISO_MergedPUTWhenAlreadyOff(t *testing.T) {
	iso := []byte("FAKEISO")
	f := newPrismFake(t, "OFF", nil, iso)
	p := newCyclePlugin(t, f, iso)

	res, err := p.MountAgentISO(context.Background(), "sess1", sdk.ISOMountOptions{PowerCycle: true})
	if err != nil {
		t.Fatalf("mount: %v", err)
	}
	if got := f.PowerPuts(); len(got) != 0 {
		t.Fatalf("an already-OFF VM must not get power ops, got: %v", got)
	}
	if res.SerialNote != "serial_port_added" {
		t.Errorf("serial note = %q, want serial_port_added", res.SerialNote)
	}
	wantEvents := []string{"spec(serial+cdrom)@OFF"}
	if got := f.Events(); strings.Join(got, ",") != strings.Join(wantEvents, ",") {
		t.Errorf("events = %v, want %v", got, wantEvents)
	}
	if f.Power() != "OFF" {
		t.Errorf("VM must stay OFF, got %q", f.Power())
	}
}
