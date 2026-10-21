package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// imagesMock is a minimal httptest.Server-backed Prism v3 mock
// that covers the endpoints UploadImage and the GC path exercise:
//
//	POST   /api/nutanix/v3/images/list
//	POST   /api/nutanix/v3/images
//	PUT    /api/nutanix/v3/images/{uuid}/file
//	DELETE /api/nutanix/v3/images/{uuid}
//	GET    /api/nutanix/v3/tasks/{uuid}
//
// Tests adjust the seeded catalog and the task progression to
// drive each path.
//
// IMPORTANT — checksum fidelity: real Prism does NOT back-populate
// spec.resources.checksum.checksum_value for an operator byte-PUT.
// So /images/list returns an EMPTY checksum_value for images the
// plugin created. The mock honors ImageEntry.SHA256 only when a test
// EXPLICITLY sets it (to simulate an inter-cluster-copied image that
// happens to carry a checksum); the production case is SHA256=="".
// An earlier version of this mock hand-injected the content checksum
// onto every entry, which masked the dead checksum-gated dedupe — the
// very bug this suite now guards against. Do not reintroduce that.
type imagesMock struct {
	t  *testing.T
	mu sync.Mutex
	// catalog is the initial /images/list contents. Tests preseed
	// to drive the dedupe path.
	catalog []ImageEntry
	// createdCount tracks POST /images calls. Tests assert this
	// is zero on the dedupe path and one on the upload path.
	createdCount int
	// createdBody captures the body of the last POST /images so tests
	// can assert the spec (e.g. initial_placement_ref_list, checksum).
	createdBody []byte
	// uploadedBytes captures the body of PUT /images/{uuid}/file.
	uploadedBytes []byte
	// taskStatus is what GET /tasks/{uuid} should return on the
	// next call. Tests can preset to FAILED to drive the error
	// path.
	taskStatus string
	// taskError is the error_detail accompanying a FAILED status.
	taskError string
	// deletedUUIDs records every successful DELETE /images/{uuid}, in
	// call order, so GC tests can assert exactly which entries were
	// pruned (and which were kept/skipped).
	deletedUUIDs []string
	// inUse marks image UUIDs that DELETE must reject with Prism's
	// "image in use" 409. Drives the GC reference-safety / skip path.
	inUse map[string]bool
	// listCount tracks POST /images/list calls (UploadImage lists
	// twice — dedupe walk + pre-POST race re-check).
	listCount int
	// imageNotPlaced, when true, makes GET /images/{uuid} report an EMPTY
	// current_cluster_reference_list — simulating Prism landing the bytes on a
	// cluster OTHER than the one requested (drives the ErrImageNotPlaced path).
	// Default (false) reports the image ACTIVE on whatever cluster the create
	// requested via initial_placement_ref_list, so happy-path tests pass.
	imageNotPlaced bool
}

func (m *imagesMock) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/nutanix/v3/images/list", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method", http.StatusMethodNotAllowed)
			return
		}
		m.mu.Lock()
		defer m.mu.Unlock()
		m.listCount++
		var resp prismImageListResp
		for _, e := range m.catalog {
			entity := struct {
				Metadata struct {
					UUID         string `json:"uuid"`
					CreationTime string `json:"creation_time"`
				} `json:"metadata"`
				Spec struct {
					Name        string `json:"name"`
					Description string `json:"description"`
					Resources   struct {
						ImageType string `json:"image_type"`
						Checksum  struct {
							ChecksumAlgorithm string `json:"checksum_algorithm"`
							ChecksumValue     string `json:"checksum_value"`
						} `json:"checksum"`
					} `json:"resources"`
				} `json:"spec"`
				Status struct {
					State string `json:"state"`
				} `json:"status"`
			}{}
			entity.Metadata.UUID = e.UUID
			entity.Spec.Name = e.Name
			entity.Spec.Description = e.Description
			entity.Spec.Resources.ImageType = e.ImageType
			// Mirror real Prism: checksum_value is whatever the catalog actually
			// holds. For plugin-created images that is EMPTY (Prism does not
			// echo our byte-PUT checksum); tests set SHA256 only to simulate an
			// image that genuinely carries one.
			if e.SHA256 != "" {
				entity.Spec.Resources.Checksum.ChecksumAlgorithm = "SHA_256"
				entity.Spec.Resources.Checksum.ChecksumValue = e.SHA256
			}
			if !e.CreatedAt.IsZero() {
				entity.Metadata.CreationTime = e.CreatedAt.UTC().Format(time.RFC3339)
			}
			// Default to the active state so dedupe tests (which don't set
			// State) reuse as before.
			state := e.State
			if state == "" {
				state = "COMPLETE"
			}
			entity.Status.State = state
			resp.Entities = append(resp.Entities, entity)
		}
		resp.Metadata.TotalMatches = len(resp.Entities)
		writeJSON(w, http.StatusOK, resp)
	})
	mux.HandleFunc("/api/nutanix/v3/images", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method", http.StatusMethodNotAllowed)
			return
		}
		body, _ := io.ReadAll(io.LimitReader(r.Body, 64*1024))
		m.mu.Lock()
		m.createdCount++
		m.createdBody = body
		m.mu.Unlock()
		out := prismCreateImageResp{}
		out.Metadata.UUID = "image-uuid-from-create"
		out.Status.ExecutionContext.TaskUUID = "task-create"
		writeJSON(w, http.StatusOK, out)
	})
	mux.HandleFunc("/api/nutanix/v3/images/", func(w http.ResponseWriter, r *http.Request) {
		// Two sub-paths share this prefix:
		//   PUT    /api/nutanix/v3/images/{uuid}/file  — byte upload
		//   DELETE /api/nutanix/v3/images/{uuid}       — GC / orphan cleanup
		if r.Method == http.MethodDelete {
			// {uuid} is the last path segment.
			uuid := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
			m.mu.Lock()
			if m.inUse[uuid] {
				m.mu.Unlock()
				// Prism's "image is in use, cannot delete" — 409 CONFLICT with
				// a descriptive body. Drives GC's reference-safe skip path.
				http.Error(w, `{"message":"image is in use and cannot be deleted"}`, http.StatusConflict)
				return
			}
			m.deletedUUIDs = append(m.deletedUUIDs, uuid)
			// Drop it from the catalog so a subsequent list reflects the delete.
			kept := m.catalog[:0]
			for _, e := range m.catalog {
				if e.UUID != uuid {
					kept = append(kept, e)
				}
			}
			m.catalog = kept
			m.mu.Unlock()
			out := prismDeleteImageResp{}
			out.Status.ExecutionContext.TaskUUID = "task-delete"
			writeJSON(w, http.StatusAccepted, out)
			return
		}

		// GET /api/nutanix/v3/images/{uuid} — image detail. Used by the
		// post-upload placement check (imageActiveOnCluster). By default report
		// the image ACTIVE on the cluster(s) the create requested; set
		// imageNotPlaced to report none (PC put the bytes elsewhere).
		if r.Method == http.MethodGet && !strings.HasSuffix(r.URL.Path, "/file") {
			m.mu.Lock()
			notPlaced := m.imageNotPlaced
			created := m.createdBody
			m.mu.Unlock()
			clusters := []map[string]string{}
			if !notPlaced {
				var cr struct {
					Spec struct {
						Resources struct {
							InitialPlacementRefList []struct {
								UUID string `json:"uuid"`
							} `json:"initial_placement_ref_list"`
						} `json:"resources"`
					} `json:"spec"`
				}
				_ = json.Unmarshal(created, &cr)
				for _, x := range cr.Spec.Resources.InitialPlacementRefList {
					clusters = append(clusters, map[string]string{"uuid": x.UUID})
				}
			}
			writeJSON(w, http.StatusOK, map[string]any{
				"status": map[string]any{
					"state":     "COMPLETE",
					"resources": map[string]any{"current_cluster_reference_list": clusters},
				},
			})
			return
		}

		// Path is /api/nutanix/v3/images/{uuid}/file.
		if !strings.HasSuffix(r.URL.Path, "/file") {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		if r.Method != http.MethodPut {
			http.Error(w, "method", http.StatusMethodNotAllowed)
			return
		}
		body, _ := io.ReadAll(io.LimitReader(r.Body, 32*1024*1024))
		m.mu.Lock()
		m.uploadedBytes = body
		m.mu.Unlock()
		// Return the task UUID in the body (matches Prism's PC
		// 7.5+ behavior). Tests for the Location-header branch
		// can swap this out via a separate mock.
		out := prismCreateImageResp{}
		out.Status.ExecutionContext.TaskUUID = "task-upload"
		writeJSON(w, http.StatusAccepted, out)
	})
	mux.HandleFunc("/api/nutanix/v3/tasks/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method", http.StatusMethodNotAllowed)
			return
		}
		m.mu.Lock()
		defer m.mu.Unlock()
		status := m.taskStatus
		if status == "" {
			status = "SUCCEEDED"
		}
		out := prismTaskResp{
			Status:      status,
			ErrorDetail: m.taskError,
		}
		out.ProgressInfo.PercentageComplete = 100
		writeJSON(w, http.StatusOK, out)
	})
	return mux
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// newTestClient wires a *Client against a test server. No auth,
// no TLS pinning — the mock accepts any request.
func newTestClient(t *testing.T, srv *httptest.Server) *Client {
	t.Helper()
	c, err := NewClient(ClientOptions{
		BaseURL: srv.URL,
		Logger:  nil,
	})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	return c
}

// TestUploadImage_NotPlacedOnCluster: when Prism Central lands the uploaded
// image's bytes on a different cluster (not the VM's), UploadImage must NOT
// hand back a falsely-usable image — it returns ErrImageNotPlaced and deletes
// the dead entry so a retry re-uploads instead of re-attaching the inactive one
// (the observed NO_POLICY_COMPLIANT_CLUSTER reuse loop).
func TestUploadImage_NotPlacedOnCluster(t *testing.T) {
	// TEMP(iso-placement): ensurePlacedOnCluster is temporarily in diagnostic
	// mode — it polls then PROCEEDS to the attach instead of deleting+failing
	// with ErrImageNotPlaced, so we can observe whether the attach works and
	// where PC actually placed the bytes. Re-enable this test when that mode
	// is reverted.
	t.Skip("TEMP(iso-placement): placement pre-check in diagnostic proceed-anyway mode")
	mock := &imagesMock{t: t, imageNotPlaced: true}
	srv := httptest.NewServer(mock.handler())
	defer srv.Close()
	c := newTestClient(t, srv)

	_, _, err := c.UploadImage(context.Background(), "ahv-bridge.iso", []byte("ISO"), "vm-cluster-uuid", "")
	if !errors.Is(err, ErrImageNotPlaced) {
		t.Fatalf("expected ErrImageNotPlaced when bytes land on another cluster, got %v", err)
	}
	if len(mock.deletedUUIDs) == 0 {
		t.Error("expected the not-placed image to be deleted so a retry re-uploads")
	}
}

// TestUploadImage_NotPlacedOnCluster_DiagnosticProceeds PINS the CURRENT
// behavior while ensurePlacedOnCluster is in TEMP(iso-placement) diagnostic
// mode (images.go): when PC lands the bytes on another cluster, UploadImage
// must NOT fail and must NOT delete the image — it logs and PROCEEDS so the
// attach is still attempted. This is the inverse of the skipped
// TestUploadImage_NotPlacedOnCluster above (which asserts the intended
// post-revert delete+ErrImageNotPlaced contract). Without this, neither the old
// nor the new behavior is enforced. When the diagnostic mode is reverted, THIS
// test should fail (prompting its removal + un-skipping the other), making the
// revert deliberate rather than silent.
func TestUploadImage_NotPlacedOnCluster_DiagnosticProceeds(t *testing.T) {
	// Shrink the placement poll so we exercise the proceed-anyway path without
	// the 45s production deadline; restore after.
	savedTimeout, savedInterval := placementPollTimeout, placementPollInterval
	placementPollTimeout, placementPollInterval = 20*time.Millisecond, 5*time.Millisecond
	defer func() { placementPollTimeout, placementPollInterval = savedTimeout, savedInterval }()

	mock := &imagesMock{t: t, imageNotPlaced: true}
	srv := httptest.NewServer(mock.handler())
	defer srv.Close()
	c := newTestClient(t, srv)

	uuid, _, err := c.UploadImage(context.Background(), "ahv-bridge.iso", []byte("ISO"), "vm-cluster-uuid", "")
	if err != nil {
		t.Fatalf("diagnostic mode must proceed (not fail) when bytes land off-cluster, got %v", err)
	}
	if errors.Is(err, ErrImageNotPlaced) {
		t.Fatal("diagnostic mode must NOT return ErrImageNotPlaced")
	}
	if uuid == "" {
		t.Error("expected a usable image UUID even when placement was not confirmed")
	}
	if len(mock.deletedUUIDs) != 0 {
		t.Errorf("diagnostic mode must NOT delete the image; deleted=%v", mock.deletedUUIDs)
	}
}

// THE PRODUCTION CASE. With a STATIC image name, reuse is gated on the content
// SHA carried in spec.description — NOT the catalog checksum_value (which real
// Prism leaves EMPTY for our byte-PUTs). A same-name entry whose description
// SHA matches the bytes must be reused with no re-upload.
func TestUploadImage_ReusesOnContentMatch(t *testing.T) {
	iso := []byte("PRETEND THIS IS AN ISO")
	sum := sha256.Sum256(iso)

	mock := &imagesMock{
		t: t,
		catalog: []ImageEntry{
			// checksum_value stays EMPTY (real-Prism state); the content SHA
			// lives in the description, which is the reuse key.
			{UUID: "existing-uuid", Name: "ahv-bridge.iso",
				Description: imageDescription("", hex.EncodeToString(sum[:]))},
		},
	}
	srv := httptest.NewServer(mock.handler())
	defer srv.Close()
	c := newTestClient(t, srv)

	uuid, reused, err := c.UploadImage(context.Background(), "ahv-bridge.iso", iso, "", "")
	if err != nil {
		t.Fatalf("upload: %v", err)
	}
	if !reused {
		t.Errorf("expected reused=true on name match with empty catalog checksum (the production case)")
	}
	if uuid != "existing-uuid" {
		t.Errorf("expected existing UUID, got %q", uuid)
	}
	if mock.createdCount != 0 {
		t.Errorf("expected zero create calls on dedupe path, got %d", mock.createdCount)
	}
	if mock.uploadedBytes != nil {
		t.Errorf("expected no PUT on dedupe path, got %d bytes", len(mock.uploadedBytes))
	}
}

// OVERRIDE: a same-name entry whose description SHA does NOT match the new
// bytes is stale (a new production build under the static name). It must be
// deleted and the fresh bytes uploaded — NOT reused. This is the core of
// "static prod name, override when content changes".
func TestUploadImage_OverridesStaleSameName(t *testing.T) {
	iso := []byte("THE NEW ISO BYTES")

	mock := &imagesMock{
		t: t,
		catalog: []ImageEntry{
			// Same name, but the description carries an OLD content SHA.
			{UUID: "stale-uuid", Name: "ahv-bridge.iso",
				Description: imageDescription("v1", strings.Repeat("a", 64))},
		},
	}
	srv := httptest.NewServer(mock.handler())
	defer srv.Close()
	c := newTestClient(t, srv)

	uuid, reused, err := c.UploadImage(context.Background(), "ahv-bridge.iso", iso, "", "v2")
	if err != nil {
		t.Fatalf("upload: %v", err)
	}
	if reused {
		t.Errorf("expected reused=false (content changed → override), got reused=true")
	}
	if uuid != "image-uuid-from-create" {
		t.Errorf("expected freshly-created uuid, got %q", uuid)
	}
	if mock.createdCount != 1 {
		t.Errorf("expected one POST /images (fresh upload), got %d", mock.createdCount)
	}
	// The stale entry must have been deleted (override).
	found := false
	for _, d := range mock.deletedUUIDs {
		if d == "stale-uuid" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected stale-uuid to be deleted on override, deleted=%v", mock.deletedUUIDs)
	}
}

func TestUploadImage_NewUploadHappyPath(t *testing.T) {
	iso := []byte("PRETEND THIS IS AN ISO PAYLOAD")
	mock := &imagesMock{t: t}
	srv := httptest.NewServer(mock.handler())
	defer srv.Close()
	c := newTestClient(t, srv)

	uuid, reused, err := c.UploadImage(context.Background(), "ahv-bridge.iso", iso, "", "")
	if err != nil {
		t.Fatalf("upload: %v", err)
	}
	if reused {
		t.Errorf("expected reused=false on new upload")
	}
	if uuid != "image-uuid-from-create" {
		t.Errorf("expected uuid from create response, got %q", uuid)
	}
	if mock.createdCount != 1 {
		t.Errorf("expected exactly one POST /images, got %d", mock.createdCount)
	}
	if len(mock.uploadedBytes) != len(iso) {
		t.Errorf("expected %d uploaded bytes, got %d", len(iso), len(mock.uploadedBytes))
	}
	if string(mock.uploadedBytes) != string(iso) {
		t.Errorf("uploaded payload mismatch")
	}
}

// On Prism Central the create spec MUST pin the image to the VM's cluster via
// initial_placement_ref_list, or the image never gets a policy-compliant
// cluster, stays inactive, and the CD-ROM attach fails with
// NO_POLICY_COMPLIANT_CLUSTER. Regression guard for that bridge-install bug.
func TestUploadImage_SendsClusterPlacement(t *testing.T) {
	iso := []byte("ISO BYTES NEEDING PLACEMENT")
	mock := &imagesMock{t: t}
	srv := httptest.NewServer(mock.handler())
	defer srv.Close()
	c := newTestClient(t, srv)

	const clusterUUID = "cluster-abc-123"
	if _, _, err := c.UploadImage(context.Background(), "ahv-bridge.iso", iso, clusterUUID, ""); err != nil {
		t.Fatalf("upload: %v", err)
	}
	if mock.createdCount != 1 {
		t.Fatalf("expected one create, got %d", mock.createdCount)
	}
	var req prismCreateImageReq
	if err := json.Unmarshal(mock.createdBody, &req); err != nil {
		t.Fatalf("decode create body: %v", err)
	}
	refs := req.Spec.Resources.InitialPlacementRefList
	if len(refs) != 1 {
		t.Fatalf("expected 1 initial placement ref, got %d (body: %s)", len(refs), mock.createdBody)
	}
	if refs[0].Kind != "cluster" || refs[0].UUID != clusterUUID {
		t.Errorf("placement ref = %+v, want {cluster %s}", refs[0], clusterUUID)
	}
}

// An empty clusterUUID (single-cluster Prism Element, which auto-places) must
// NOT emit the placement field — older PE builds reject an unknown cluster ref.
func TestUploadImage_NoPlacementWhenClusterEmpty(t *testing.T) {
	mock := &imagesMock{t: t}
	srv := httptest.NewServer(mock.handler())
	defer srv.Close()
	c := newTestClient(t, srv)

	if _, _, err := c.UploadImage(context.Background(), "ahv-bridge.iso", []byte("x"), "", ""); err != nil {
		t.Fatalf("upload: %v", err)
	}
	var req prismCreateImageReq
	if err := json.Unmarshal(mock.createdBody, &req); err != nil {
		t.Fatalf("decode create body: %v", err)
	}
	if len(req.Spec.Resources.InitialPlacementRefList) != 0 {
		t.Errorf("expected no placement refs for empty cluster, got %+v", req.Spec.Resources.InitialPlacementRefList)
	}
	// omitempty must keep the key out of the wire JSON entirely.
	if strings.Contains(string(mock.createdBody), "initial_placement_ref_list") {
		t.Errorf("initial_placement_ref_list should be omitted, body: %s", mock.createdBody)
	}
}

// A catalog entry that matches the canonical NAME but is INACTIVE (never placed
// on a compliant cluster) must NOT be reused — reusing it would re-trigger the
// NO_POLICY_COMPLIANT_CLUSTER attach failure. We skip it and upload a fresh,
// placed copy. The inactive-skip survives the move to name-based dedupe.
func TestUploadImage_SkipsInactiveDuplicate(t *testing.T) {
	iso := []byte("PRETEND THIS IS AN ISO")

	mock := &imagesMock{
		t: t,
		catalog: []ImageEntry{
			// Same name, inactive — the broken prior upload. Empty checksum,
			// matching real Prism for our byte-PUTs.
			{UUID: "inactive-uuid", Name: "ahv-bridge.iso", State: "INACTIVE"},
		},
	}
	srv := httptest.NewServer(mock.handler())
	defer srv.Close()
	c := newTestClient(t, srv)

	uuid, reused, err := c.UploadImage(context.Background(), "ahv-bridge.iso", iso, "cluster-xyz", "")
	if err != nil {
		t.Fatalf("upload: %v", err)
	}
	if reused {
		t.Errorf("expected reused=false: the matching image is inactive and must not be reused")
	}
	if uuid != "image-uuid-from-create" {
		t.Errorf("expected a freshly created image UUID, got %q", uuid)
	}
	if mock.createdCount != 1 {
		t.Errorf("expected one create (fresh upload), got %d", mock.createdCount)
	}
}

// A DIFFERENT canonical name (a different build hash) must NOT dedupe against an
// existing entry — it is genuinely different content and needs its own upload.
// This is the case that lets superseded builds accumulate (and that GC cleans).
func TestUploadImage_DifferentName_DoesNotMatch(t *testing.T) {
	iso := []byte("ACTUAL ISO BYTES")
	mock := &imagesMock{
		t: t,
		catalog: []ImageEntry{
			// A prior build's image under a DIFFERENT content-addressed name.
			{UUID: "old-uuid", Name: "ahv-remote-console-bridge-sha256-deadbeef.iso"},
		},
	}
	srv := httptest.NewServer(mock.handler())
	defer srv.Close()
	c := newTestClient(t, srv)

	uuid, reused, err := c.UploadImage(context.Background(), "ahv-remote-console-bridge-sha256-cafef00d.iso", iso, "", "")
	if err != nil {
		t.Fatalf("upload: %v", err)
	}
	if reused {
		t.Errorf("expected fresh upload for a new canonical name")
	}
	if uuid == "old-uuid" {
		t.Errorf("should not have reused an entry with a different name")
	}
	if mock.createdCount != 1 {
		t.Errorf("expected fresh POST /images, got %d calls", mock.createdCount)
	}
}

// A same-name image with NO description SHA (a legacy image from the old
// SHA-in-name scheme, or any foreign image that happens to share the name) is
// NOT reused — its content can't be confirmed, so we upload fresh and delete
// the unconfirmed entry. Guards the migration path off the old naming.
func TestUploadImage_NoReuseWhenDescriptionMissing(t *testing.T) {
	iso := []byte("ACTUAL ISO BYTES")
	mock := &imagesMock{
		t: t,
		catalog: []ImageEntry{
			// catalog checksum present but NO description → content unconfirmed.
			{UUID: "legacy-uuid", Name: "ahv-bridge.iso", SHA256: strings.Repeat("0", 64)},
		},
	}
	srv := httptest.NewServer(mock.handler())
	defer srv.Close()
	c := newTestClient(t, srv)

	uuid, reused, err := c.UploadImage(context.Background(), "ahv-bridge.iso", iso, "", "")
	if err != nil {
		t.Fatalf("upload: %v", err)
	}
	if reused {
		t.Errorf("expected no reuse without a confirming description SHA, got reused=true")
	}
	if uuid != "image-uuid-from-create" {
		t.Errorf("expected fresh upload, got %q", uuid)
	}
	if mock.createdCount != 1 {
		t.Errorf("expected one fresh create, got %d", mock.createdCount)
	}
}

func TestUploadImage_TaskFailedReturnsError(t *testing.T) {
	iso := []byte("ISO")
	mock := &imagesMock{
		t:          t,
		taskStatus: "FAILED",
		taskError:  "upstream blew up",
	}
	srv := httptest.NewServer(mock.handler())
	defer srv.Close()
	c := newTestClient(t, srv)

	_, _, err := c.UploadImage(context.Background(), "ahv-bridge.iso", iso, "", "")
	if err == nil {
		t.Fatalf("expected error on task FAILED")
	}
	if !strings.Contains(err.Error(), "FAILED") {
		t.Errorf("expected error to mention FAILED, got %q", err.Error())
	}
	if !strings.Contains(err.Error(), "upstream blew up") {
		t.Errorf("expected error to surface task error_detail, got %q", err.Error())
	}
}

func TestUploadImage_RejectsEmptyInputs(t *testing.T) {
	srv := httptest.NewServer(http.NewServeMux())
	defer srv.Close()
	c := newTestClient(t, srv)

	if _, _, err := c.UploadImage(context.Background(), "", []byte("x"), "", ""); err == nil {
		t.Errorf("expected error on empty name")
	}
	if _, _, err := c.UploadImage(context.Background(), "name.iso", nil, "", ""); err == nil {
		t.Errorf("expected error on nil payload")
	}
	if _, _, err := c.UploadImage(context.Background(), "name.iso", []byte{}, "", ""); err == nil {
		t.Errorf("expected error on empty payload")
	}
}

func TestListImages_HandlesEmptyCatalog(t *testing.T) {
	mock := &imagesMock{t: t}
	srv := httptest.NewServer(mock.handler())
	defer srv.Close()
	c := newTestClient(t, srv)

	out, err := c.ListImages(context.Background())
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(out) != 0 {
		t.Errorf("expected empty list, got %d entries", len(out))
	}
}

func TestListImages_ReturnsAllSeeded(t *testing.T) {
	mock := &imagesMock{
		t: t,
		catalog: []ImageEntry{
			{UUID: "a", Name: "a.iso", SHA256: "aa"},
			{UUID: "b", Name: "b.iso", SHA256: "bb"},
		},
	}
	srv := httptest.NewServer(mock.handler())
	defer srv.Close()
	c := newTestClient(t, srv)

	out, err := c.ListImages(context.Background())
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(out))
	}
	if out[0].UUID != "a" || out[1].UUID != "b" {
		t.Errorf("entries out of order: %+v", out)
	}
}

func TestJSONStringOrSlice_BothShapes(t *testing.T) {
	type wrapper struct {
		T jsonStringOrSlice `json:"t"`
	}
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"string", `{"t":"abc"}`, "abc"},
		{"single-element-array", `{"t":["xyz"]}`, "xyz"},
		{"empty-array", `{"t":[]}`, ""},
		{"null", `{"t":null}`, ""},
		{"missing", `{}`, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var w wrapper
			if err := json.Unmarshal([]byte(tc.in), &w); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if string(w.T) != tc.want {
				t.Errorf("got %q, want %q", string(w.T), tc.want)
			}
		})
	}
}

func TestUploadImage_TaskUUIDFromLocationHeader(t *testing.T) {
	// Some PC builds return the task UUID in the Location header
	// instead of the body. Make sure we handle that fallback.
	mock := &imagesMockLocationHeader{}
	srv := httptest.NewServer(mock.handler())
	defer srv.Close()
	c := newTestClient(t, srv)

	uuid, reused, err := c.UploadImage(context.Background(), "ahv-bridge.iso", []byte("ISO"), "", "")
	if err != nil {
		t.Fatalf("upload: %v", err)
	}
	if reused {
		t.Errorf("expected reused=false")
	}
	if uuid != "image-uuid-from-create" {
		t.Errorf("uuid mismatch: %s", uuid)
	}
	if mock.taskPollHit == 0 {
		t.Errorf("expected at least one task poll, got 0")
	}
}

// imagesMockLocationHeader is a focused mock that returns the
// task UUID via Location header on PUT and verifies we poll the
// right task.
type imagesMockLocationHeader struct {
	mu          sync.Mutex
	taskPollHit int
}

func (m *imagesMockLocationHeader) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/nutanix/v3/images/list", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, prismImageListResp{})
	})
	mux.HandleFunc("/api/nutanix/v3/images", func(w http.ResponseWriter, r *http.Request) {
		out := prismCreateImageResp{}
		out.Metadata.UUID = "image-uuid-from-create"
		writeJSON(w, http.StatusOK, out)
	})
	mux.HandleFunc("/api/nutanix/v3/images/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", "/api/nutanix/v3/tasks/upload-task-id")
		w.WriteHeader(http.StatusAccepted)
	})
	mux.HandleFunc("/api/nutanix/v3/tasks/upload-task-id", func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		m.taskPollHit++
		m.mu.Unlock()
		out := prismTaskResp{Status: "SUCCEEDED"}
		writeJSON(w, http.StatusOK, out)
	})
	mux.HandleFunc("/api/nutanix/v3/tasks/", func(w http.ResponseWriter, r *http.Request) {
		// Any unrecognized task path is wrong.
		http.Error(w, "wrong task uuid", http.StatusInternalServerError)
	})
	return mux
}

// Sanity check that errors.As works against the API error
// returned through the upload helpers — keeps the wrap chain
// usable by upstream callers (the plugin needs IsUnauthorized
// for cred-invalidation logic).
func TestUploadImage_APIErrorPreserved(t *testing.T) {
	mock := http.NewServeMux()
	mock.HandleFunc("/api/nutanix/v3/images/list", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"message":"go away"}`))
	})
	srv := httptest.NewServer(mock)
	defer srv.Close()
	c := newTestClient(t, srv)

	_, _, err := c.UploadImage(context.Background(), "x.iso", []byte("y"), "", "")
	if err == nil {
		t.Fatalf("expected error")
	}
	var ae *APIError
	if !errors.As(err, &ae) {
		t.Fatalf("expected APIError in chain, got %T: %v", err, err)
	}
	if ae.Status != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", ae.Status)
	}
}

// The create spec MUST carry the SHA-256 checksum so Prism records integrity
// and a future secondary-confirm works. (It is still not a dedupe gate — Prism
// won't echo it back — but we must SEND it.)
func TestUploadImage_SendsChecksumOnCreate(t *testing.T) {
	iso := []byte("ISO BYTES NEEDING A CHECKSUM")
	sum := sha256.Sum256(iso)
	want := hex.EncodeToString(sum[:])

	mock := &imagesMock{t: t}
	srv := httptest.NewServer(mock.handler())
	defer srv.Close()
	c := newTestClient(t, srv)

	if _, _, err := c.UploadImage(context.Background(), "ahv-bridge.iso", iso, "", ""); err != nil {
		t.Fatalf("upload: %v", err)
	}
	var req prismCreateImageReq
	if err := json.Unmarshal(mock.createdBody, &req); err != nil {
		t.Fatalf("decode create body: %v", err)
	}
	cs := req.Spec.Resources.Checksum
	if cs == nil {
		t.Fatalf("expected checksum on create spec, body: %s", mock.createdBody)
	}
	if cs.ChecksumAlgorithm != "SHA_256" {
		t.Errorf("checksum_algorithm = %q, want SHA_256", cs.ChecksumAlgorithm)
	}
	if cs.ChecksumValue != want {
		t.Errorf("checksum_value = %q, want %q", cs.ChecksumValue, want)
	}
}

// On a task FAILURE after the POST+PUT, the half-created image is orphan-deleted
// before the error is returned, so a failed mount leaves no stale entry behind.
func TestUploadImage_OrphanDeletedOnTaskFailure(t *testing.T) {
	mock := &imagesMock{
		t:          t,
		taskStatus: "FAILED",
		taskError:  "ingest blew up",
	}
	srv := httptest.NewServer(mock.handler())
	defer srv.Close()
	c := newTestClient(t, srv)

	_, _, err := c.UploadImage(context.Background(), "ahv-bridge.iso", []byte("ISO"), "", "")
	if err == nil {
		t.Fatalf("expected error on task FAILED")
	}
	// The created image's UUID (from the mock create response) must have been
	// DELETEd. (The orphan delete's own task poll also returns FAILED here and
	// is logged non-fatally, but the DELETE request itself is what cleans the
	// catalog — and the mock records it synchronously.)
	if len(mock.deletedUUIDs) != 1 || mock.deletedUUIDs[0] != "image-uuid-from-create" {
		t.Errorf("expected orphan delete of image-uuid-from-create, got %v", mock.deletedUUIDs)
	}
}

func TestDeleteImage(t *testing.T) {
	mock := &imagesMock{
		t: t,
		catalog: []ImageEntry{
			{UUID: "del-me", Name: "ahv-remote-console-bridge-sha256-abc.iso"},
		},
	}
	srv := httptest.NewServer(mock.handler())
	defer srv.Close()
	c := newTestClient(t, srv)

	if err := c.DeleteImage(context.Background(), "del-me"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if len(mock.deletedUUIDs) != 1 || mock.deletedUUIDs[0] != "del-me" {
		t.Errorf("expected delete of del-me, got %v", mock.deletedUUIDs)
	}
}

func TestDeleteImage_RejectsEmptyUUID(t *testing.T) {
	c := &Client{}
	if err := c.DeleteImage(context.Background(), ""); err == nil {
		t.Errorf("expected error on empty uuid")
	}
}

func TestDeleteImage_InUseReturnsAPIError(t *testing.T) {
	mock := &imagesMock{
		t:     t,
		inUse: map[string]bool{"busy": true},
		catalog: []ImageEntry{
			{UUID: "busy", Name: "ahv-remote-console-bridge-sha256-busy.iso"},
		},
	}
	srv := httptest.NewServer(mock.handler())
	defer srv.Close()
	c := newTestClient(t, srv)

	err := c.DeleteImage(context.Background(), "busy")
	if err == nil {
		t.Fatalf("expected error deleting in-use image")
	}
	if !imageInUse(err) {
		t.Errorf("expected imageInUse(err) true for 409 in-use, got err=%v", err)
	}
}

func TestListPluginImages_FiltersToOwnedPrefixes(t *testing.T) {
	mock := &imagesMock{
		t: t,
		catalog: []ImageEntry{
			{UUID: "bridge", Name: "ahv-remote-console-bridge-sha256-aaa.iso"},
			{UUID: "clip", Name: "hc-clip-sha256-bbb.iso"},
			{UUID: "operator", Name: "ubuntu-22.04.iso"},
			{UUID: "customer", Name: "win2022-eval.iso"},
		},
	}
	srv := httptest.NewServer(mock.handler())
	defer srv.Close()
	c := newTestClient(t, srv)

	owned, err := c.ListPluginImages(context.Background())
	if err != nil {
		t.Fatalf("list plugin images: %v", err)
	}
	got := map[string]bool{}
	for _, e := range owned {
		got[e.UUID] = true
	}
	if len(owned) != 2 || !got["bridge"] || !got["clip"] {
		t.Errorf("expected only bridge+clip, got %d: %v", len(owned), owned)
	}
	if got["operator"] || got["customer"] {
		t.Errorf("must never return operator/customer images: %v", owned)
	}
}

// GarbageCollect: owner-scoped (operator/customer untouched), keepNames protects
// the live build, superseded bridge hashes pruned, expired clips pruned,
// fresh clips and kept names retained.
func TestGarbageCollect_OwnerScopedAndKeepNamesAndClipTTL(t *testing.T) {
	now := time.Now()
	keepName := "ahv-remote-console-bridge-sha256-live.iso"
	mock := &imagesMock{
		t: t,
		catalog: []ImageEntry{
			// Live build — in keepNames → KEEP.
			{UUID: "live", Name: keepName},
			// Superseded build hash — not kept → DELETE.
			{UUID: "old-build", Name: "ahv-remote-console-bridge-sha256-old.iso"},
			// Duplicate of the live name (dedupe race) — protected by keepNames.
			{UUID: "live-dup", Name: keepName},
			// Expired clip (2h old, TTL 1h) → DELETE.
			{UUID: "old-clip", Name: "hc-clip-sha256-old.iso", CreatedAt: now.Add(-2 * time.Hour)},
			// Fresh clip (within TTL) → KEEP.
			{UUID: "fresh-clip", Name: "hc-clip-sha256-fresh.iso", CreatedAt: now.Add(-5 * time.Minute)},
			// Clip with no timestamp → cannot age safely → KEEP (skip).
			{UUID: "undated-clip", Name: "hc-clip-sha256-undated.iso"},
			// Operator/customer images — NEVER touched.
			{UUID: "operator", Name: "ubuntu-22.04.iso"},
			{UUID: "customer", Name: "win2022.iso"},
		},
	}
	srv := httptest.NewServer(mock.handler())
	defer srv.Close()
	c := newTestClient(t, srv)

	if err := c.GarbageCollect(context.Background(), []string{keepName}, time.Hour); err != nil {
		t.Fatalf("gc: %v", err)
	}

	deleted := map[string]bool{}
	for _, u := range mock.deletedUUIDs {
		deleted[u] = true
	}
	wantDeleted := []string{"old-build", "old-clip"}
	for _, u := range wantDeleted {
		if !deleted[u] {
			t.Errorf("expected %q to be deleted, deleted=%v", u, mock.deletedUUIDs)
		}
	}
	mustKeep := []string{"live", "live-dup", "fresh-clip", "undated-clip", "operator", "customer"}
	for _, u := range mustKeep {
		if deleted[u] {
			t.Errorf("must NOT have deleted %q, deleted=%v", u, mock.deletedUUIDs)
		}
	}
	if len(mock.deletedUUIDs) != len(wantDeleted) {
		t.Errorf("deleted exactly %v expected, got %v", wantDeleted, mock.deletedUUIDs)
	}
}

// GC reference safety: an in-use image (Prism 409) is SKIPPED, not fatal, and
// the sweep keeps making progress on the rest.
func TestGarbageCollect_SkipsInUseImage(t *testing.T) {
	mock := &imagesMock{
		t:     t,
		inUse: map[string]bool{"mounted": true},
		catalog: []ImageEntry{
			// Currently attached to a VM — Prism rejects the delete.
			{UUID: "mounted", Name: "ahv-remote-console-bridge-sha256-mounted.iso"},
			// Free superseded build — should still be deleted in the same sweep.
			{UUID: "free", Name: "ahv-remote-console-bridge-sha256-free.iso"},
		},
	}
	srv := httptest.NewServer(mock.handler())
	defer srv.Close()
	c := newTestClient(t, srv)

	// keepNames empty → both bridge images are pruning candidates.
	if err := c.GarbageCollect(context.Background(), nil, time.Hour); err != nil {
		t.Fatalf("gc must not be fatal on in-use image: %v", err)
	}
	if len(mock.deletedUUIDs) != 1 || mock.deletedUUIDs[0] != "free" {
		t.Errorf("expected only the free image deleted (in-use skipped), got %v", mock.deletedUUIDs)
	}
}

// clipMaxAge of 0 disables clip pruning entirely; only build-hash pruning runs.
func TestGarbageCollect_ClipTTLZeroDisablesClipPrune(t *testing.T) {
	mock := &imagesMock{
		t: t,
		catalog: []ImageEntry{
			{UUID: "old-clip", Name: "hc-clip-sha256-old.iso", CreatedAt: time.Now().Add(-100 * time.Hour)},
			{UUID: "old-build", Name: "ahv-remote-console-bridge-sha256-old.iso"},
		},
	}
	srv := httptest.NewServer(mock.handler())
	defer srv.Close()
	c := newTestClient(t, srv)

	if err := c.GarbageCollect(context.Background(), nil, 0); err != nil {
		t.Fatalf("gc: %v", err)
	}
	if len(mock.deletedUUIDs) != 1 || mock.deletedUUIDs[0] != "old-build" {
		t.Errorf("expected only old-build deleted with clipMaxAge=0, got %v", mock.deletedUUIDs)
	}
}

func TestImageInUse_MatchesStatusAndMessages(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"409", &APIError{Status: http.StatusConflict}, true},
		{"400 in use", &APIError{Status: 400, Body: `{"message":"image is in use"}`}, true},
		{"422 cannot delete", &APIError{Status: 422, Body: "cannot delete: referenced by vm"}, true},
		{"500 unrelated", &APIError{Status: 500, Body: "boom"}, false},
		{"404 not in use", &APIError{Status: 404, Body: "not found"}, false},
		{"non-api error", errors.New("network"), false},
		{"nil", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := imageInUse(tc.err); got != tc.want {
				t.Errorf("imageInUse = %v, want %v", got, tc.want)
			}
		})
	}
}

// --- content-identity helpers (new naming/override contract) ---

func TestDescContentSHA_RoundTrip(t *testing.T) {
	hexSum := strings.Repeat("ab", 32) // 64 hex chars
	desc := imageDescription("v1.2.3", hexSum)
	if got := descContentSHA(desc); got != hexSum {
		t.Errorf("round-trip: got %q want %q", got, hexSum)
	}
}

func TestDescContentSHA_Edge(t *testing.T) {
	cases := map[string]string{
		"":                                      "",                      // empty
		"no sha here":                           "",                      // no tag
		"sha256:short":                          "",                      // < 64 chars
		"sha256:" + strings.Repeat("g", 64):     "",                      // non-hex
		"x | sha256:" + strings.Repeat("F", 64): strings.Repeat("F", 64), // uppercase ok, mid-string
	}
	for in, want := range cases {
		if got := descContentSHA(in); got != want {
			t.Errorf("descContentSHA(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestClassifySameName(t *testing.T) {
	c := &Client{}
	sha := strings.Repeat("a", 64)
	other := strings.Repeat("b", 64)
	catalog := []ImageEntry{
		{UUID: "match", Name: "agent.iso", State: "COMPLETE", Description: imageDescription("v", sha)},
		{UUID: "stale", Name: "agent.iso", State: "COMPLETE", Description: imageDescription("v", other)},
		{UUID: "dup", Name: "agent.iso", State: "COMPLETE", Description: imageDescription("v", sha)}, // 2nd content match → goes to others
		{UUID: "elsewhere", Name: "other.iso", State: "COMPLETE", Description: imageDescription("v", sha)},
		{UUID: "inactive", Name: "agent.iso", State: "INACTIVE", Description: imageDescription("v", sha)},
	}
	reuse, others := c.classifySameName(catalog, "agent.iso", sha)
	if reuse != "match" {
		t.Fatalf("reuse = %q, want \"match\"", reuse)
	}
	// others must be every other same-name entry (stale, dup, inactive) and NOT
	// the different-name one.
	want := map[string]bool{"stale": true, "dup": true, "inactive": true}
	if len(others) != len(want) {
		t.Fatalf("others = %v, want keys %v", others, want)
	}
	for _, u := range others {
		if !want[u] {
			t.Errorf("unexpected other %q (must never include the different-name image)", u)
		}
	}
}

func TestClassifySameName_NoContentMatch_AllStale(t *testing.T) {
	c := &Client{}
	catalog := []ImageEntry{
		{UUID: "a", Name: "agent.iso", State: "COMPLETE", Description: imageDescription("v", strings.Repeat("c", 64))},
		{UUID: "b", Name: "agent.iso", State: "COMPLETE"}, // no description
	}
	reuse, others := c.classifySameName(catalog, "agent.iso", strings.Repeat("a", 64))
	if reuse != "" {
		t.Errorf("reuse = %q, want \"\" (no content match → override)", reuse)
	}
	if len(others) != 2 {
		t.Errorf("others = %v, want both stale entries", others)
	}
}
