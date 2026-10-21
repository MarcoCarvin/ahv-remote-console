package ahv

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/MarcoCarvin/ahv-remote-console/apps/runtime/pkg/sdk"
	"github.com/MarcoCarvin/ahv-remote-console/plugins/ahv/internal/api"
)

const catalogVMUUID = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"

// catalogImage is one entry the fake catalog returns from /images/list.
type catalogImage struct {
	uuid      string
	name      string
	imageType string
	state     string
}

// catalogFake is an httptest-backed Prism v3 fake serving the endpoints the
// ImageCatalogCapable methods touch: /images/list, the VM GET/PUT, and tasks.
type catalogFake struct {
	t       *testing.T
	images  []catalogImage
	vmDisks []api.VMDisk

	putBody []byte
	putHit  int
}

func (f *catalogFake) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/nutanix/v3/images/list", func(w http.ResponseWriter, _ *http.Request) {
		type entity struct {
			Metadata struct {
				UUID string `json:"uuid"`
			} `json:"metadata"`
			Spec struct {
				Name      string `json:"name"`
				Resources struct {
					ImageType string `json:"image_type"`
				} `json:"resources"`
			} `json:"spec"`
			Status struct {
				State string `json:"state"`
			} `json:"status"`
		}
		var resp struct {
			Entities []entity `json:"entities"`
			Metadata struct {
				TotalMatches int `json:"total_matches"`
			} `json:"metadata"`
		}
		for _, im := range f.images {
			var e entity
			e.Metadata.UUID = im.uuid
			e.Spec.Name = im.name
			e.Spec.Resources.ImageType = im.imageType
			e.Status.State = im.state
			resp.Entities = append(resp.Entities, e)
		}
		resp.Metadata.TotalMatches = len(resp.Entities)
		writeCatalogJSON(w, http.StatusOK, resp)
	})
	mux.HandleFunc("/api/nutanix/v3/vms/"+catalogVMUUID, func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			body := map[string]any{
				"metadata": map[string]any{"kind": "vm", "uuid": catalogVMUUID, "spec_version": 3},
				"spec": map[string]any{
					"name":      "catalog-vm",
					"resources": map[string]any{"disk_list": f.vmDisks},
				},
			}
			writeCatalogJSON(w, http.StatusOK, body)
		case http.MethodPut:
			b, _ := io.ReadAll(io.LimitReader(r.Body, 32*1024*1024))
			f.putBody = b
			f.putHit++
			out := map[string]any{
				"metadata": map[string]any{"uuid": catalogVMUUID},
				"status": map[string]any{
					"execution_context": map[string]any{"task_uuid": "task-1"},
				},
			}
			writeCatalogJSON(w, http.StatusAccepted, out)
		default:
			http.Error(w, "method", http.StatusMethodNotAllowed)
		}
	})
	mux.HandleFunc("/api/nutanix/v3/tasks/", func(w http.ResponseWriter, _ *http.Request) {
		writeCatalogJSON(w, http.StatusOK, map[string]any{"status": "SUCCEEDED"})
	})
	return mux
}

func writeCatalogJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// newCatalogPlugin builds a Plugin wired to the catalog fake with one
// authenticated endpoint + one open session on catalogVMUUID.
func newCatalogPlugin(t *testing.T, f *catalogFake) *Plugin {
	t.Helper()
	srv := httptest.NewServer(f.handler())
	t.Cleanup(srv.Close)

	ep := sdk.EndpointRef{ID: "ep1", URL: srv.URL, PluginID: pluginID, AuthMethods: []sdk.AuthKind{sdk.AuthKindCookie}}
	p := &Plugin{
		deps: sdk.Dependencies{
			Logger:   testLogger{newDiscardLogger()},
			Keystore: emptyKeystore{},
			Config:   stubConfig{eps: []sdk.EndpointRef{ep}},
		},
		state:        map[string]*endpointState{},
		clients:      map[string]*api.Client{},
		sessions:     map[string]*sessionState{},
		cache:        newResourceCache(time.Second),
		clusterUUIDs: map[string]consoleTarget{},
		capabilities: map[string]*endpointCapabilities{},
		capEpochs:    map[string]uint64{},
	}
	client, err := api.NewClient(api.ClientOptions{BaseURL: srv.URL})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	p.clients["ep1"] = client
	p.sessions["sess1"] = &sessionState{
		EndpointID: "ep1",
		ResourceID: "ep1/" + catalogVMUUID,
		VMUUID:     catalogVMUUID,
		OpenedAt:   time.Now(),
	}
	return p
}

// TestListISOImages_FiltersToISOAndUsableState: only ISO_IMAGE + usable-state
// entries are offered; DISK_IMAGE and non-COMPLETE rows are dropped.
func TestListISOImages_FiltersToISOAndUsableState(t *testing.T) {
	f := &catalogFake{
		t: t,
		images: []catalogImage{
			{uuid: "iso-ok", name: "win.iso", imageType: "ISO_IMAGE", state: "COMPLETE"},
			{uuid: "disk-1", name: "golden.qcow2", imageType: "DISK_IMAGE", state: "COMPLETE"},
			{uuid: "iso-pending", name: "half.iso", imageType: "ISO_IMAGE", state: "PENDING"},
			{uuid: "iso-empty-state", name: "older.iso", imageType: "ISO_IMAGE", state: ""},
		},
	}
	p := newCatalogPlugin(t, f)

	images, err := p.ListISOImages(context.Background(), "sess1")
	if err != nil {
		t.Fatalf("list iso images: %v", err)
	}
	got := map[string]bool{}
	for _, im := range images {
		got[im.UUID] = true
	}
	if !got["iso-ok"] {
		t.Errorf("COMPLETE ISO_IMAGE should be offered")
	}
	if !got["iso-empty-state"] {
		t.Errorf("empty-state ISO_IMAGE (older PC build) should be offered")
	}
	if got["disk-1"] {
		t.Errorf("DISK_IMAGE must NOT be offered")
	}
	if got["iso-pending"] {
		t.Errorf("non-COMPLETE ISO_IMAGE must NOT be offered")
	}
	// image_read evidence should be recorded as allowed after a successful list.
	if v := p.capabilityValue("ep1", capImageRead); v != capAllowed {
		t.Errorf("image_read = %q, want allowed after a successful list", v)
	}
}

func TestListISOImages_UnknownSession(t *testing.T) {
	f := &catalogFake{t: t}
	p := newCatalogPlugin(t, f)
	if _, err := p.ListISOImages(context.Background(), "nope"); err == nil {
		t.Errorf("expected error on unknown session")
	}
}

// TestMountISO_RejectsNonUUID guards the defense-in-depth UUID check.
func TestMountISO_RejectsNonUUID(t *testing.T) {
	f := &catalogFake{t: t}
	p := newCatalogPlugin(t, f)
	if _, err := p.MountISO(context.Background(), "sess1", "../etc/passwd"); err == nil {
		t.Errorf("expected error on non-UUID image id")
	}
	if f.putHit != 0 {
		t.Errorf("no VM PUT should fire for an invalid image id, got %d", f.putHit)
	}
}

// TestMountISO_AttachesWithoutSerial: the mount issues a CD-ROM media change
// and NEVER mutates serial_port_list (arbitrary ISOs don't add COM3).
func TestMountISO_AttachesWithoutSerial(t *testing.T) {
	f := &catalogFake{
		t: t,
		vmDisks: []api.VMDisk{{
			DeviceProperties: api.VMDeviceProperties{
				DeviceType:  "CDROM",
				DiskAddress: api.VMDiskAddress{AdapterType: "SATA", DeviceIndex: 0},
			},
		}},
	}
	p := newCatalogPlugin(t, f)

	imageUUID := "12345678-1234-1234-1234-123456789abc"
	res, err := p.MountISO(context.Background(), "sess1", imageUUID)
	if err != nil {
		t.Fatalf("mount iso: %v", err)
	}
	if res.ImageUUID != imageUUID {
		t.Errorf("result image uuid = %q, want %q", res.ImageUUID, imageUUID)
	}
	if res.SerialNote != "" {
		t.Errorf("arbitrary ISO mount must not set SerialNote, got %q", res.SerialNote)
	}
	if f.putHit != 1 {
		t.Fatalf("expected one VM PUT (CD-ROM media change), got %d", f.putHit)
	}
	// The PUT body must NOT carry a serial_port_list mutation.
	if strings.Contains(string(f.putBody), "serial_port_list") {
		t.Errorf("mount-iso PUT body should not touch serial_port_list; body: %s", f.putBody)
	}
	if !strings.Contains(string(f.putBody), imageUUID) {
		t.Errorf("mount-iso PUT body should reference the image UUID")
	}
}

// TestEjectISO_ResolvesSessionAndDetaches: an eject clears the CD-ROM media and
// reports ejected=true.
func TestEjectISO_ResolvesSessionAndDetaches(t *testing.T) {
	f := &catalogFake{
		t: t,
		vmDisks: []api.VMDisk{{
			DeviceProperties: api.VMDeviceProperties{
				DeviceType:  "CDROM",
				DiskAddress: api.VMDiskAddress{AdapterType: "SATA", DeviceIndex: 0},
			},
			DataSourceReference: &api.VMDataSourceRef{Kind: "image", UUID: "mounted"},
		}},
	}
	p := newCatalogPlugin(t, f)

	res, err := p.EjectISO(context.Background(), "sess1")
	if err != nil {
		t.Fatalf("eject iso: %v", err)
	}
	if !res.Ejected {
		t.Errorf("expected ejected=true when media was present")
	}
	if f.putHit != 1 {
		t.Errorf("expected one VM PUT for the eject, got %d", f.putHit)
	}
}

// TestEjectISO_NoMediaIsNoop: ejecting an empty drive issues no PUT and reports
// ejected=false.
func TestEjectISO_NoMediaIsNoop(t *testing.T) {
	f := &catalogFake{
		t: t,
		vmDisks: []api.VMDisk{{
			DeviceProperties: api.VMDeviceProperties{
				DeviceType:  "CDROM",
				DiskAddress: api.VMDiskAddress{AdapterType: "SATA", DeviceIndex: 0},
			},
		}},
	}
	p := newCatalogPlugin(t, f)

	res, err := p.EjectISO(context.Background(), "sess1")
	if err != nil {
		t.Fatalf("eject iso: %v", err)
	}
	if res.Ejected {
		t.Errorf("expected ejected=false on an already-empty drive")
	}
	if f.putHit != 0 {
		t.Errorf("expected zero PUTs on a no-op eject, got %d", f.putHit)
	}
}

func TestEjectISO_UnknownSession(t *testing.T) {
	f := &catalogFake{t: t}
	p := newCatalogPlugin(t, f)
	if _, err := p.EjectISO(context.Background(), "nope"); err == nil {
		t.Errorf("expected error on unknown session")
	}
}
