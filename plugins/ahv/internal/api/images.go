// Package api — Prism Image Service bindings.
//
// Endpoints implemented in this file:
//
//   POST /api/nutanix/v3/images/list        — paginated image catalog
//   POST /api/nutanix/v3/images             — create image metadata (no bytes)
//   PUT  /api/nutanix/v3/images/{uuid}/file — upload raw ISO bytes
//   GET  /api/nutanix/v3/tasks/{uuid}       — poll long-running task
//
// The upload path is intentionally a two-step (POST metadata, then
// PUT bytes) because Prism v3 separates the catalog entry from the
// blob. The POST kicks off a task that allocates the entry; the PUT
// streams the file payload into that entry; a second task tracks the
// file ingest. We poll the file-ingest task to completion before
// returning the image UUID — otherwise callers that immediately
// attach the image as a disk hit "image not ready" errors against
// AHV.
//
// Dedupe / override logic
//
// The image NAME no longer carries the content SHA: production uses a STATIC
// name (ahv-remote-console-agent.iso) and dev a version-tagged one
// (ahv-remote-console-agent-dev-<version>.iso). So the content identity is
// carried in spec.description as "sha256:<hex>", and reuse is gated on THAT:
//
//   - same name AND matching description SHA → reuse (reused=true);
//   - same name BUT different/absent description SHA → the bytes changed (a new
//     production build) → OVERRIDE: delete the stale entry, upload the new one;
//   - extra same-name entries (dedupe-race duplicates) → deleted alongside.
//
// Why description and not checksum_value: Prism does NOT back-populate
// spec.resources.checksum.checksum_value for an operator byte-PUT (the catalog
// checksum is empty for images we create), so it can never gate reuse. The
// name used to carry the SHA for exactly this reason; description now does — it
// IS round-tripped for byte-PUTs. (NOTE: that round-trip is the one Prism-
// behaviour assumption here; validate against the target Prism build.)
//
// This keeps the "Install agent" button safe to double-click and safe across a
// fleet sharing one cluster: only the first VM uploads, every other reuses; and
// a content change on a static prod name cleanly replaces the old image.

package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// ImageEntry is the trimmed-down representation of one image
// catalog entry returned by /images/list. We only carry what the
// dedupe path needs: UUID for re-use, name to match against the
// caller's filename, and the SHA-256 checksum to match against the
// caller's local copy.
type ImageEntry struct {
	UUID   string
	Name   string
	SHA256 string // hex, from spec.resources.checksum.checksum_value
	// State is status.state. "COMPLETE" means the image is placed on a
	// cluster and is attachable. An image that was created but never placed
	// on a policy-compliant cluster is inactive (non-COMPLETE) and attaching
	// it fails with NO_POLICY_COMPLIANT_CLUSTER — the dedupe path uses this to
	// avoid re-using such a broken entry.
	State string
	// CreatedAt is metadata.creation_time, used by the GC sweep to age out
	// clipboard payloads. Zero when the PC build omits it (older builds) — GC
	// treats a zero time as "cannot age safely" and skips clip pruning for
	// that entry rather than guessing.
	CreatedAt time.Time
	// Description is spec.description. Since the image NAME no longer embeds the
	// content SHA (production uses a STATIC name), the SHA is carried here
	// (as "sha256:<hex>") so reuse can be gated on actual content identity and
	// a content change forces an override. See descContentSHA.
	Description string
	// ImageType is spec.resources.image_type ("ISO_IMAGE" / "DISK_IMAGE"). The
	// catalog picker (ListISOImages) filters to ISO_IMAGE so only mountable ISOs
	// are offered. Empty on older PC builds that omit it — the filter treats
	// empty as "unknown, include it" rather than dropping otherwise-usable rows.
	ImageType string
}

// prismImageListResp models the subset of the /images/list response
// that we read. Prism's full schema is much wider — we keep only
// the fields needed to drive dedupe so a future schema addition
// doesn't break decoding.
type prismImageListResp struct {
	Entities []struct {
		Metadata struct {
			UUID string `json:"uuid"`
			// CreationTime is RFC3339 in v3 (e.g. "2026-05-29T12:34:56Z").
			// Used to age out clipboard payloads in the GC sweep.
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
	} `json:"entities"`
	Metadata struct {
		TotalMatches int `json:"total_matches"`
	} `json:"metadata"`
}

// ListImages walks /api/nutanix/v3/images/list in fixed-size pages
// and returns every catalog entry the authenticated user can see.
// Paginates transparently so callers don't have to.
func (c *Client) ListImages(ctx context.Context) ([]ImageEntry, error) {
	const pageSize = 250
	var out []ImageEntry
	offset := 0
	for {
		body := prismListRequest{
			Kind:   "image",
			Length: pageSize,
			Offset: offset,
		}
		var resp prismImageListResp
		if err := c.doJSON(ctx, http.MethodPost, "/api/nutanix/v3/images/list", body, &resp); err != nil {
			return nil, fmt.Errorf("list images: %w", err)
		}
		for _, e := range resp.Entities {
			var created time.Time
			if e.Metadata.CreationTime != "" {
				// Tolerate parse failures (unexpected format / older builds) by
				// leaving created zero — GC treats zero as "cannot age safely".
				if t, perr := time.Parse(time.RFC3339, e.Metadata.CreationTime); perr == nil {
					created = t
				}
			}
			out = append(out, ImageEntry{
				UUID:        e.Metadata.UUID,
				Name:        e.Spec.Name,
				SHA256:      e.Spec.Resources.Checksum.ChecksumValue,
				State:       e.Status.State,
				CreatedAt:   created,
				Description: e.Spec.Description,
				ImageType:   e.Spec.Resources.ImageType,
			})
		}
		// Stop on the page that fewer-than-pageSize entries — Prism
		// returns a TotalMatches we *could* use, but trusting it
		// across versions is fragile. Short-page is the universal
		// termination signal.
		if len(resp.Entities) < pageSize {
			break
		}
		offset += pageSize
		// Defensive cap so a misbehaving server can't lock us in a
		// loop. 10k images is well beyond any realistic catalog.
		if offset > 10_000 {
			break
		}
	}
	return out, nil
}

// prismCreateImageReq is the body for POST /images. We send only
// the kind + spec; Prism fills in metadata.uuid + task UUID in the
// response.
type prismCreateImageReq struct {
	Metadata prismCreateImageMeta `json:"metadata"`
	Spec     prismImageSpec       `json:"spec"`
}

type prismCreateImageMeta struct {
	Kind string `json:"kind"`
}

type prismImageSpec struct {
	Name string `json:"name"`
	// Description carries the content identity ("…| sha256:<hex>") since the
	// name no longer embeds the SHA. Prism persists and returns spec.description
	// for operator byte-PUTs (unlike checksum_value), so it is the reuse key.
	Description string                  `json:"description,omitempty"`
	Resources   prismImageSpecResources `json:"resources"`
}

type prismImageSpecResources struct {
	ImageType string `json:"image_type"`
	// Checksum records the SHA-256 of the bytes we are about to PUT. Prism
	// stores it for integrity and so a FUTURE secondary confirm (name match +
	// checksum match) can be exact. It is NOT a dedupe gate: Prism does not
	// back-populate this field for an operator byte-PUT, so on the wire it is
	// empty for images we created — the name is the real dedupe key. Omitted
	// (omitempty) when we have no checksum to send.
	Checksum *prismImageChecksum `json:"checksum,omitempty"`
	// InitialPlacementRefList is sent best-effort, but is NOT a reliable v3
	// image SPEC placement control: the authoritative v3 image schema has no
	// per-cluster placement input, and a local byte-PUT lands the bytes on a
	// single Prism-Central-chosen cluster regardless. The real "is this image
	// usable on cluster X" signal is status.resources.current_cluster_reference_list
	// (read via imageActiveOnCluster). Getting an uploaded image onto a specific
	// cluster requires an AHV image placement policy or a catalog checkout —
	// neither is a plain v3 REST verb. We keep sending this (harmless where
	// honored) and VERIFY actual placement after upload (ensurePlacedOnCluster).
	InitialPlacementRefList []prismClusterReference `json:"initial_placement_ref_list,omitempty"`
}

// prismImageChecksum is the spec.resources.checksum sub-object. We always send
// SHA_256 with the lower-hex digest of the bytes.
type prismImageChecksum struct {
	ChecksumAlgorithm string `json:"checksum_algorithm"` // always "SHA_256"
	ChecksumValue     string `json:"checksum_value"`     // lower-hex sha256
}

// prismClusterReference is a v3 reference to a registered cluster.
type prismClusterReference struct {
	Kind string `json:"kind"` // always "cluster"
	UUID string `json:"uuid"`
}

// prismCreateImageResp models POST /images. Prism returns the new
// catalog entry's UUID inside metadata, and a task UUID under
// status.execution_context that tracks the create. We poll that task
// to completion BEFORE PUTting the file bytes — the entry isn't ready
// to receive its /file payload until the create commits, and PUTting
// early is rejected with HTTP 500.
type prismCreateImageResp struct {
	Metadata struct {
		UUID string `json:"uuid"`
	} `json:"metadata"`
	Status struct {
		ExecutionContext struct {
			TaskUUID jsonStringOrSlice `json:"task_uuid"`
		} `json:"execution_context"`
	} `json:"status"`
}

// prismTaskResp models GET /tasks/{uuid}. We watch status until it
// hits a terminal value.
type prismTaskResp struct {
	Status       string `json:"status"`
	ErrorDetail  string `json:"error_detail"`
	ProgressInfo struct {
		PercentageComplete int `json:"percentage_complete"`
	} `json:"progress_info"`
}

// jsonStringOrSlice is a Prism quirk: some endpoints return
// task_uuid as a JSON string, others as a one-element array of
// strings. Older PC builds even mix the two on the same install.
// Decode either shape into the string we actually need.
type jsonStringOrSlice string

func (j *jsonStringOrSlice) UnmarshalJSON(b []byte) error {
	if len(b) == 0 {
		return nil
	}
	if b[0] == '"' {
		var s string
		if err := jsonUnmarshal(b, &s); err != nil {
			return err
		}
		*j = jsonStringOrSlice(s)
		return nil
	}
	if b[0] == '[' {
		var arr []string
		if err := jsonUnmarshal(b, &arr); err != nil {
			return err
		}
		if len(arr) > 0 {
			*j = jsonStringOrSlice(arr[0])
		}
		return nil
	}
	// null / missing => empty
	*j = ""
	return nil
}

// jsonUnmarshal is a tiny indirection so we can switch to a
// streaming decoder later without touching the call sites.
func jsonUnmarshal(b []byte, v any) error {
	return json.Unmarshal(b, v)
}

// UploadImage uploads a single ISO blob to the cluster's Prism
// Image Service.
//
// Returns (uuid, reused=true, nil) if a COMPLETE image with the
// same canonical name (which callers content-address with the full
// sha256 of the bytes) is already in the catalog — the caller can
// attach that UUID directly with no further work. Dedupe is by NAME,
// not by the catalog checksum: Prism leaves checksum_value empty for
// operator byte-PUTs, so a checksum gate would never fire and every
// mount would proliferate a duplicate image.
//
// Returns (uuid, reused=false, nil) after the new image's bytes
// have been ingested AND its associated task has reached SUCCEEDED.
// Callers can attach the returned UUID immediately; AHV will not
// reject it as "not ready".
//
// Returns a wrapped error on any upstream failure. The error
// chain is preserved so callers can use errors.As to recover the
// underlying APIError.
//
// clusterUUID is the UUID of the cluster the image must be usable on (the VM's
// cluster). On Prism Central it is sent as the image's initial placement so the
// bytes are replicated there and the image becomes ACTIVE; pass "" on
// single-cluster Prism Element (auto-placed).
func (c *Client) UploadImage(ctx context.Context, name string, isoBytes []byte, clusterUUID, version string) (string, bool, error) {
	if name == "" {
		return "", false, errors.New("upload image: name required")
	}
	if len(isoBytes) == 0 {
		return "", false, errors.New("upload image: empty payload")
	}

	// 1. SHA-256 of the bytes. The name no longer embeds it (production uses a
	//    STATIC name), so the SHA travels in spec.description as the content
	//    identity. Reuse is gated on it; a content change forces an override.
	sum := sha256.Sum256(isoBytes)
	checksumHex := hex.EncodeToString(sum[:])
	description := imageDescription(version, checksumHex)

	// 2. Look up by NAME, then decide by CONTENT. We walk the catalog because
	//    /images/list does not accept a server-side filter on spec.name across
	//    all PC builds. classifySameName returns the entry to REUSE (same name
	//    AND matching content SHA) plus every OTHER same-name entry — stale
	//    content under a static name, or dedupe-race duplicates — to override.
	existing, err := c.ListImages(ctx)
	if err != nil {
		return "", false, fmt.Errorf("dedupe lookup: %w", err)
	}
	reuse, others := c.classifySameName(existing, name, checksumHex)
	if reuse != "" {
		if err := c.ensurePlacedOnCluster(ctx, reuse, clusterUUID, name); err != nil {
			return "", false, err
		}
		// Tidy any duplicate same-name entries (dedupe-race leftovers). Best-
		// effort + in-use-safe; never blocks a successful reuse.
		c.deleteOthers(ctx, others, name)
		return reuse, true, nil
	}
	// No content match. Any same-name entries are STALE (old content under the
	// static prod name, or a half-baked dup) — override: delete them so the
	// fresh bytes below replace them. GC cannot do this (it keeps the name),
	// so it must happen here. Best-effort + in-use-safe.
	c.deleteOthers(ctx, others, name)

	// 3. POST /images to allocate the catalog entry. We do NOT
	//    send a data_source_reference at this stage — the bytes
	//    follow via PUT /images/{uuid}/file. Some Prism builds
	//    reject the create-with-data_source_reference path
	//    against operator-supplied blobs (it's meant for
	//    inter-cluster image copies).
	resources := prismImageSpecResources{
		ImageType: "ISO_IMAGE",
		// Record integrity so a future secondary-confirm works even though
		// Prism won't echo this back for our byte-PUT.
		Checksum: &prismImageChecksum{
			ChecksumAlgorithm: "SHA_256",
			ChecksumValue:     checksumHex,
		},
	}
	if clusterUUID != "" {
		resources.InitialPlacementRefList = []prismClusterReference{
			{Kind: "cluster", UUID: clusterUUID},
		}
	}
	createReq := prismCreateImageReq{
		Metadata: prismCreateImageMeta{Kind: "image"},
		Spec: prismImageSpec{
			Name:        name,
			Description: description,
			Resources:   resources,
		},
	}

	// 3a. Idempotency vs the duplicate-name race. Two VMs (or a
	//     double-click) can pass the dedupe walk above simultaneously,
	//     both find nothing, and both POST — Prism happily creates two
	//     identically-named images. Re-list immediately before the POST
	//     and reuse if a COMPLETE entry has appeared in the meantime.
	//     This narrows (cannot fully close — Prism has no name-unique
	//     constraint) the window; the GC sweep mops up any residual
	//     duplicate on a later mount.
	recheck, err := c.ListImages(ctx)
	if err == nil {
		if hit, others := c.classifySameName(recheck, name, checksumHex); hit != "" {
			if err := c.ensurePlacedOnCluster(ctx, hit, clusterUUID, name); err != nil {
				return "", false, err
			}
			c.deleteOthers(ctx, others, name)
			return hit, true, nil
		}
	}

	var createResp prismCreateImageResp
	if err := c.doJSON(ctx, http.MethodPost, "/api/nutanix/v3/images", createReq, &createResp); err != nil {
		return "", false, fmt.Errorf("create image: %w", err)
	}
	if createResp.Metadata.UUID == "" {
		return "", false, errors.New("create image: response missing uuid")
	}
	imageUUID := createResp.Metadata.UUID

	// 3b. WAIT for the create task to commit before PUTting bytes. Prism returns
	//     the image UUID immediately, but the catalog entry is not ready to
	//     receive its /file payload until the create task finishes — PUTting too
	//     early is rejected with HTTP 500 ("upload bytes: ... HTTP 500"). This
	//     race was previously unguarded (we relied on the file PUT's own task),
	//     which intermittently 500'd the upload. A missing task UUID means a
	//     synchronous create, so we just proceed. Bounded short.
	if createTask := string(createResp.Status.ExecutionContext.TaskUUID); createTask != "" {
		if err := c.waitForTask(ctx, createTask, 2*time.Minute); err != nil {
			c.deleteOrphan(ctx, imageUUID, name)
			return "", false, fmt.Errorf("create image task: %w", err)
		}
	}

	// 4. PUT /images/{uuid}/file with the raw ISO bytes. The
	//    upload is treated as a long-running task by Prism — the
	//    response carries a task UUID that we poll for completion.
	taskUUID, err := c.uploadImageBytes(ctx, imageUUID, isoBytes)
	if err != nil {
		// The catalog entry exists but has no usable bytes. Orphan-on-failure:
		// delete the half-created image so it can't accumulate or be reused.
		c.deleteOrphan(ctx, imageUUID, name)
		return "", false, fmt.Errorf("upload bytes: %w", err)
	}

	// 5. Poll the task. We allow up to ~10 minutes total for a
	//    multi-GB ISO. The polling cadence is exponential-ish
	//    (capped at 5s) so small ISOs complete quickly without
	//    hammering the API.
	if err := c.waitForTask(ctx, taskUUID, 10*time.Minute); err != nil {
		// Ingest failed or timed out AFTER the POST + byte-PUT. The image is
		// half-created (entry present, bytes possibly incomplete). Delete it
		// before returning so a failed mount doesn't leave a stale INACTIVE
		// duplicate behind that the dedupe path would later skip-and-recreate.
		c.deleteOrphan(ctx, imageUUID, name)
		return "", false, fmt.Errorf("wait for upload task: %w", err)
	}

	if err := c.ensurePlacedOnCluster(ctx, imageUUID, clusterUUID, name); err != nil {
		return "", false, err
	}
	return imageUUID, false, nil
}

// ErrImageNotPlaced means the uploaded image's bytes are not present on the
// target (VM's) cluster, so a CD-ROM attach there fails with
// NO_POLICY_COMPLIANT_CLUSTER. Prism Central places a locally byte-uploaded
// image on a single cluster IT chooses; making it available on another cluster
// requires an AHV image placement policy (tag the image + a policy mapping it to
// the cluster) or a catalog checkout — neither is a plain v3 REST call.
var ErrImageNotPlaced = errors.New("bridge image is not active on the VM's cluster (Prism Central placed it on a different cluster); configure an AHV image placement policy targeting this cluster, or check the image out to it")

// imageActiveOnCluster reports whether the image's file bytes are present
// (ACTIVE) on clusterUUID, read from status.resources.current_cluster_reference_list
// — the authoritative list of clusters where the bytes actually live (distinct
// from the merely-requested initial_placement_ref_list). For a single-cluster
// Prism Element (clusterUUID == "") there is nothing to target, so it trusts the
// top-level status.state == "COMPLETE".
func (c *Client) imageActiveOnCluster(ctx context.Context, imageUUID, clusterUUID string) (bool, error) {
	var resp struct {
		Status struct {
			State     string `json:"state"`
			Resources struct {
				CurrentClusterReferenceList []struct {
					UUID string `json:"uuid"`
				} `json:"current_cluster_reference_list"`
			} `json:"resources"`
		} `json:"status"`
	}
	if err := c.doJSON(ctx, http.MethodGet, "/api/nutanix/v3/images/"+imageUUID, nil, &resp); err != nil {
		return false, err
	}
	// TEMP(iso-placement): dump exactly what PC reports so we can see which
	// cluster a byte-uploaded image actually landed on, versus the VM's cluster.
	if c.log != nil {
		got := make([]string, 0, len(resp.Status.Resources.CurrentClusterReferenceList))
		for _, cr := range resp.Status.Resources.CurrentClusterReferenceList {
			got = append(got, cr.UUID)
		}
		c.log.Warn("TEMP(iso-placement): image placement report",
			"image", imageUUID, "state", resp.Status.State,
			"current_clusters", got, "target_cluster", clusterUUID)
	}
	if clusterUUID == "" {
		return strings.EqualFold(resp.Status.State, "COMPLETE"), nil
	}
	for _, cr := range resp.Status.Resources.CurrentClusterReferenceList {
		if cr.UUID == clusterUUID {
			return true, nil
		}
	}
	return false, nil
}

// ensurePlacedOnCluster verifies the image is attachable on the VM's cluster.
// If it is not, it deletes the dead catalog entry — so the next mount re-uploads
// instead of reusing an image that is INACTIVE here (the observed
// NO_POLICY_COMPLIANT_CLUSTER reuse loop) — and returns ErrImageNotPlaced. A
// verification error (e.g. an older PC build that omits the field) is non-fatal:
// log and let the downstream attach surface any real problem rather than blocking.
// placementPollTimeout / placementPollInterval bound how long ensurePlacedOnCluster
// waits for PC to report the image active on the VM's cluster. They are package
// vars (not consts) only so tests can shrink the poll without a 45s wall-clock
// cost — production behavior is unchanged.
var (
	placementPollTimeout  = 45 * time.Second
	placementPollInterval = 3 * time.Second
)

func (c *Client) ensurePlacedOnCluster(ctx context.Context, imageUUID, clusterUUID, name string) error {
	// Poll briefly: PC can finish replicating the bytes / populating
	// current_cluster_reference_list a few seconds AFTER the ingest task
	// reports SUCCEEDED. Checking exactly once (and deleting on a miss) is
	// what makes every first-time upload fail before placement can settle.
	deadline := time.Now().Add(placementPollTimeout)
	for {
		active, err := c.imageActiveOnCluster(ctx, imageUUID, clusterUUID)
		if err != nil {
			if c.log != nil {
				c.log.Warn("could not verify image cluster placement; proceeding", "image", imageUUID, "err", err)
			}
			return nil
		}
		if active {
			return nil
		}
		if time.Now().After(deadline) {
			break
		}
		select {
		case <-time.After(placementPollInterval):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	// TEMP(iso-placement): placement still not reported on the VM's cluster
	// after the poll. Rather than delete + fail (which never lets a settling
	// or attach-time placement work, and re-uploads the whole ISO every
	// attempt), PROCEED to the attach so we learn whether the CD-ROM attach
	// succeeds anyway. If it fails with NO_POLICY_COMPLIANT_CLUSTER the
	// placement is genuinely broken (→ replication/policy fix); if it succeeds
	// this pre-check was over-eager. Revert to delete+ErrImageNotPlaced once
	// we know. (ErrImageNotPlaced is intentionally not returned in this mode.)
	if c.log != nil {
		c.log.Warn("TEMP(iso-placement): image not reported active on target cluster after poll; proceeding to attach anyway (diagnostic, not deleting)",
			"image", imageUUID, "name", name, "cluster", clusterUUID)
	}
	return nil
}

// imageDescription builds the spec.description that carries the content
// identity. The "sha256:<hex>" token is the reuse key (descContentSHA reads it
// back); the rest is human-facing. version is the build version for ops.
func imageDescription(version, checksumHex string) string {
	if version == "" {
		version = "unknown"
	}
	return fmt.Sprintf("ahv-remote-console agent ISO | ver:%s | sha256:%s", version, checksumHex)
}

// descContentSHA extracts the 64-hex content SHA-256 that imageDescription
// embedded as "sha256:<hex>". Returns "" if absent/malformed (legacy or
// foreign images), which classifySameName treats as a non-match → override.
func descContentSHA(desc string) string {
	const tag = "sha256:"
	i := strings.Index(desc, tag)
	if i < 0 {
		return ""
	}
	rest := desc[i+len(tag):]
	if len(rest) < 64 {
		return ""
	}
	h := rest[:64]
	for _, r := range h {
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F')) {
			return ""
		}
	}
	return h
}

// classifySameName partitions catalog entries that share `name`:
//
//   - reuse: the UUID of the entry to attach directly — same name, a reusable
//     state (COMPLETE, or empty on older PC builds), AND a description whose
//     embedded content SHA equals checksumHex (i.e. the bytes match).
//   - others: every OTHER same-name entry — stale content under the STATIC
//     production name, a non-COMPLETE half-bake, or a dedupe-race duplicate.
//     The caller deletes these (best-effort, in-use-safe) so a content change
//     overrides the old image and duplicates don't accumulate. The GC cannot
//     do this since all these entries share the kept name.
//
// Matching on the description SHA (not the name) is what lets production use a
// STATIC image name yet still override automatically when the bytes change —
// Prism leaves checksum_value empty for byte-PUTs, so the name used to carry
// the SHA; now the description does.
func (c *Client) classifySameName(catalog []ImageEntry, name, checksumHex string) (reuse string, others []string) {
	for _, e := range catalog {
		if e.Name != name {
			continue
		}
		usableState := e.State == "" || strings.EqualFold(e.State, "COMPLETE")
		if reuse == "" && usableState && strings.EqualFold(descContentSHA(e.Description), checksumHex) {
			reuse = e.UUID
			continue
		}
		others = append(others, e.UUID)
	}
	return reuse, others
}

// deleteOthers best-effort deletes superseded/duplicate same-name images. An
// image still attached to a VM ("in use") is skipped and retried on a later
// mount/GC, never force-detached. Errors are logged, never returned — cleanup
// must not fail an otherwise-successful upload/reuse.
func (c *Client) deleteOthers(ctx context.Context, uuids []string, name string) {
	for _, u := range uuids {
		if err := c.DeleteImage(ctx, u); err != nil {
			if imageInUse(err) {
				if c.log != nil {
					c.log.Info("override: superseded image in use, leaving for next sweep",
						"image", u, "name", name)
				}
				continue
			}
			if c.log != nil {
				c.log.Warn("override: failed to delete superseded image (non-fatal)",
					"image", u, "name", name, "err", err)
			}
		}
	}
}

// deleteOrphan best-effort deletes a half-created image after an upload
// failure. Errors are logged, never returned — the caller is already returning
// the original upload error and a failed cleanup must not mask it.
func (c *Client) deleteOrphan(ctx context.Context, imageUUID, name string) {
	// Use a fresh, short-lived context: the caller's ctx may already be
	// cancelled/timed-out (that is often WHY we are cleaning up), which would
	// make the DELETE a no-op.
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
	defer cancel()
	if err := c.DeleteImage(cleanupCtx, imageUUID); err != nil {
		if c.log != nil {
			c.log.Warn("failed to delete orphaned image after upload failure; GC will retry",
				"image", imageUUID, "name", name, "err", err)
		}
		return
	}
	if c.log != nil {
		c.log.Info("deleted orphaned image after upload failure",
			"image", imageUUID, "name", name)
	}
}

// uploadImageBytes does the raw PUT. It returns the task UUID for
// the caller to poll.
func (c *Client) uploadImageBytes(ctx context.Context, imageUUID string, body []byte) (string, error) {
	u, err := c.base.Parse("/api/nutanix/v3/images/" + imageUUID + "/file")
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, u.String(), bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "AHV Remote Console/AHV")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/octet-stream")
	req.ContentLength = int64(len(body))
	if c.auth != nil {
		if err := c.auth.Apply(ctx, req); err != nil {
			return "", fmt.Errorf("authenticator: %w", err)
		}
	}
	// Upload via the dedicated long-timeout client. The shared
	// `c.http` has a 60s hard cap that prematurely kills a multi-MB
	// upload over a high-latency VPN; httpUpload defers to the
	// request context's deadline instead.
	resp, err := c.httpUpload.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if resp.StatusCode/100 != 2 {
		return "", &APIError{Method: http.MethodPut, Path: u.Path, Status: resp.StatusCode, Body: string(raw)}
	}
	// Some PC builds return an empty body on the PUT success and
	// surface the task UUID via the Location header instead. Try
	// the JSON body first, fall through to the header.
	if len(raw) > 0 {
		var parsed prismCreateImageResp
		if err := jsonUnmarshal(raw, &parsed); err == nil {
			if t := string(parsed.Status.ExecutionContext.TaskUUID); t != "" {
				return t, nil
			}
		}
	}
	if loc := resp.Header.Get("Location"); loc != "" {
		// Location is typically /api/nutanix/v3/tasks/<uuid>;
		// pull the last path segment.
		for i := len(loc) - 1; i >= 0; i-- {
			if loc[i] == '/' {
				return loc[i+1:], nil
			}
		}
		return loc, nil
	}
	// No task UUID returned at all — Prism finished synchronously
	// (rare but observed on small ISOs against pc.7.5+). Caller
	// can proceed without polling.
	return "", nil
}

// TaskSnapshot is one read of an upstream task's progress, used by the in-plugin
// task registry to refresh a tracked handle WITHOUT re-implementing the per-API
// parsing. State is the SDK task vocabulary ("running" | "succeeded" |
// "failed"); Error is the operator-facing failure detail (never secrets).
type TaskSnapshot struct {
	State           string
	PercentComplete int
	Error           string
}

// PollTaskOnce does exactly ONE GET of the upstream task (v3 tasks/{uuid} or v4
// config/tasks/{extId}) and maps it into a TaskSnapshot. It is the single-shot
// poll the waiters loop over and the task registry calls to refresh a running
// handle. SUCCEEDED -> "succeeded"; FAILED/ABORTED/CANCELED/CANCELLED ->
// "failed" (with the upstream errorDetail/ErrorDetail); anything else ->
// "running" with percent from progress_info (v3 only; v4 reports no percent).
func (c *Client) PollTaskOnce(ctx context.Context, taskUUID string, isV4 bool) (TaskSnapshot, error) {
	if taskUUID == "" {
		return TaskSnapshot{State: "succeeded", PercentComplete: 100}, nil
	}
	if isV4 {
		var resp v4TaskResp
		path := "/api/prism/v4.0/config/tasks/" + url.PathEscape(taskUUID)
		if err := c.doJSON(ctx, http.MethodGet, path, nil, &resp); err != nil {
			return TaskSnapshot{}, fmt.Errorf("poll task %s: %w", taskDisplayID(taskUUID), err)
		}
		switch strings.ToUpper(resp.Data.Status) {
		case "SUCCEEDED":
			return TaskSnapshot{State: "succeeded", PercentComplete: 100}, nil
		case "FAILED", "ABORTED", "CANCELED", "CANCELLED":
			return TaskSnapshot{State: "failed", PercentComplete: 100, Error: resp.errorDetail()}, nil
		default:
			return TaskSnapshot{State: "running"}, nil
		}
	}
	var resp prismTaskResp
	if err := c.doJSON(ctx, http.MethodGet, "/api/nutanix/v3/tasks/"+taskUUID, nil, &resp); err != nil {
		return TaskSnapshot{}, fmt.Errorf("poll task %s: %w", taskUUID, err)
	}
	switch resp.Status {
	case "SUCCEEDED":
		return TaskSnapshot{State: "succeeded", PercentComplete: 100}, nil
	case "FAILED", "ABORTED", "CANCELED":
		return TaskSnapshot{State: "failed", PercentComplete: 100, Error: resp.ErrorDetail}, nil
	default:
		return TaskSnapshot{State: "running", PercentComplete: resp.ProgressInfo.PercentageComplete}, nil
	}
}

// waitForTask polls /api/nutanix/v3/tasks/{uuid} until the task
// reaches a terminal status or `total` elapses. Empty taskUUID
// short-circuits as success — Prism returned synchronously and
// there's nothing to wait on.
func (c *Client) waitForTask(ctx context.Context, taskUUID string, total time.Duration) error {
	if taskUUID == "" {
		return nil
	}
	deadline := time.Now().Add(total)
	delay := 200 * time.Millisecond
	for {
		snap, err := c.PollTaskOnce(ctx, taskUUID, false)
		if err != nil {
			return err
		}
		switch snap.State {
		case "succeeded":
			return nil
		case "failed":
			if snap.Error != "" {
				return fmt.Errorf("task %s ended FAILED: %s", taskUUID, snap.Error)
			}
			return fmt.Errorf("task %s ended FAILED", taskUUID)
		}
		// Still in flight — back off and retry.
		if time.Now().After(deadline) {
			return fmt.Errorf("task %s did not complete within %s (%d%%)",
				taskUUID, total, snap.PercentComplete)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(delay):
		}
		// Exponential backoff capped at 5s. Small ISOs finish
		// well before we hit the cap; big ones don't need more
		// frequent polling than every 5s.
		delay *= 2
		if delay > 5*time.Second {
			delay = 5 * time.Second
		}
	}
}

// ResolvePublishedImage finds a usable already-published catalog entry under
// name WITHOUT any create/delete machinery — the reuse-only path for
// operators whose role cannot create images. Unlike UploadImage's
// reuse-or-override, a content-SHA mismatch (or a foreign image published
// without the app's description convention) still resolves: this role could
// not replace the image anyway, so a possibly-stale published ISO beats a
// guaranteed 403. stale reports that mismatch so the caller can log it.
// Returns uuid=="" (no error) when no COMPLETE same-name entry exists.
// Placement on the VM's cluster is still verified (read-only) — an
// unplaced image would fail the later attach with a worse error.
func (c *Client) ResolvePublishedImage(ctx context.Context, name string, isoBytes []byte, clusterUUID, version string) (uuid string, stale bool, err error) {
	existing, err := c.ListImages(ctx)
	if err != nil {
		return "", false, fmt.Errorf("published-image lookup: %w", err)
	}
	var entry *ImageEntry
	for i := range existing {
		e := &existing[i]
		if e.Name == name && (e.State == "" || strings.EqualFold(e.State, "COMPLETE")) {
			entry = e
			break
		}
	}
	if entry == nil {
		return "", false, nil
	}
	sum := sha256.Sum256(isoBytes)
	stale = descContentSHA(entry.Description) != hex.EncodeToString(sum[:])
	if err := c.ensurePlacedOnCluster(ctx, entry.UUID, clusterUUID, name); err != nil {
		return "", false, err
	}
	return entry.UUID, stale, nil
}
