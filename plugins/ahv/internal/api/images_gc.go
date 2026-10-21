// Package api — Prism Image Service garbage collection.
//
// Endpoints implemented in this file:
//
//   DELETE /api/nutanix/v3/images/{uuid}   — remove a catalog entry + bytes
//
// Background
//
// Every bridge build and (legacy) clipboard payload is uploaded to
// the cluster's Prism Image Service under a content-addressed name
// (ahv-remote-console-bridge-sha256-<hex>.iso, hc-clip-sha256-<hex>.iso).
// Dedupe (see images.go) stops duplicate UPLOADS of the *current*
// content, but it does nothing about OLD content: a new bridge build
// gets a new hash → a new name → a new catalog entry, and the prior
// build's image lingers forever. Clipboard payloads churn per-copy
// and never repeat. Without a sweep the catalog grows without bound.
//
// GarbageCollect is that sweep. It is deliberately OWNER-SCOPED and
// REFERENCE-SAFE (see the function doc): it only ever touches images
// whose names carry the plugin's own prefixes, and it treats an
// "image in use / cannot delete" failure as a skip — never a fatal —
// so an image currently mounted on a VM is left alone and retried on
// a later sweep.

package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// Plugin-owned image name prefixes. ONLY images whose names start
// with one of these are eligible for garbage collection — operator-
// and customer-uploaded images never carry these prefixes and are
// therefore never touched.
const (
	// AgentImagePrefix is the canonical prefix for the agent installer ISO.
	// It deliberately has NO trailing dash so it matches BOTH the production
	// static name "ahv-remote-console-agent.iso" and the dev-versioned
	// "ahv-remote-console-agent-dev-<version>.iso".
	AgentImagePrefix = "ahv-remote-console-agent"
	// LegacyBridgeImagePrefix is the OLD agent-ISO prefix (content-addressed,
	// "ahv-remote-console-bridge-sha256-<hex>.iso"). Still recognised so the GC
	// prunes images left behind by pre-rename builds.
	LegacyBridgeImagePrefix = "ahv-remote-console-bridge-"
	// ClipImagePrefix is the canonical prefix for (retired) clipboard ISOs.
	ClipImagePrefix = "hc-clip-"
)

// prismDeleteImageResp models DELETE /images/{uuid}. Prism runs the
// delete as a task and returns its UUID in execution_context.
type prismDeleteImageResp struct {
	Status struct {
		ExecutionContext struct {
			TaskUUID jsonStringOrSlice `json:"task_uuid"`
		} `json:"execution_context"`
	} `json:"status"`
}

// DeleteImage removes a single image from the catalog by UUID and
// waits for the delete task to complete.
//
// Prism returns an APIError (preserved in the chain via errors.As)
// when the image cannot be deleted because it is still referenced by
// a VM ("image in use"). Callers that sweep the catalog should treat
// that as a skip rather than a hard failure — see imageInUse and
// GarbageCollect.
func (c *Client) DeleteImage(ctx context.Context, imageUUID string) error {
	if imageUUID == "" {
		return fmt.Errorf("delete image: imageUUID required")
	}
	var resp prismDeleteImageResp
	if err := c.doJSON(ctx, http.MethodDelete, "/api/nutanix/v3/images/"+imageUUID, nil, &resp); err != nil {
		return fmt.Errorf("delete image %s: %w", imageUUID, err)
	}
	taskUUID := string(resp.Status.ExecutionContext.TaskUUID)
	if err := c.waitForTask(ctx, taskUUID, 5*time.Minute); err != nil {
		return fmt.Errorf("wait for delete task (image %s): %w", imageUUID, err)
	}
	return nil
}

// ListPluginImages filters the full catalog down to images this
// plugin owns — i.e. whose name carries the bridge or clip prefix.
// Used by GarbageCollect to scope the sweep; never returns
// operator/customer images.
func (c *Client) ListPluginImages(ctx context.Context) ([]ImageEntry, error) {
	all, err := c.ListImages(ctx)
	if err != nil {
		return nil, err
	}
	var out []ImageEntry
	for _, e := range all {
		if isPluginOwned(e.Name) {
			out = append(out, e)
		}
	}
	return out, nil
}

// isPluginOwned reports whether name carries one of the plugin's
// canonical prefixes. The OWNER-SCOPE gate for the whole GC path.
func isPluginOwned(name string) bool {
	return strings.HasPrefix(name, AgentImagePrefix) ||
		strings.HasPrefix(name, LegacyBridgeImagePrefix) ||
		strings.HasPrefix(name, ClipImagePrefix)
}

// imageInUse reports whether err is Prism's "image is still
// referenced / cannot delete" failure. Prism surfaces this as a 4xx
// (commonly 409 CONFLICT, but the message varies across PC builds),
// so we match defensively on both the status family and well-known
// message substrings. A true result means SKIP-and-retry-next-sweep,
// not fatal.
func imageInUse(err error) bool {
	var ae *APIError
	if !errors.As(err, &ae) {
		return false
	}
	// 409 CONFLICT is the canonical "in use" status on modern PC.
	if ae.Status == http.StatusConflict {
		return true
	}
	// Some builds return a generic 4xx with a descriptive body instead of a
	// dedicated status — match on the message so we don't force-delete a
	// referenced image (which Prism would reject anyway, but we want to log it
	// as a skip rather than a generic error).
	if ae.Status >= 400 && ae.Status < 500 {
		body := strings.ToLower(ae.Body)
		for _, needle := range []string{"in use", "in_use", "still referenced", "cannot delete", "referenced by", "attached"} {
			if strings.Contains(body, needle) {
				return true
			}
		}
	}
	return false
}

// GarbageCollect performs an OWNER-SCOPED, REFERENCE-SAFE sweep of
// the plugin's own images in the Prism catalog. It is best-effort:
// individual delete failures are logged and skipped, never fatal, so
// a partial sweep still makes progress and a referenced image is
// simply retried next time.
//
// Policy:
//
//   - SCOPE: only images whose name carries AgentImagePrefix, LegacyBridgeImagePrefix, or
//     ClipImagePrefix are ever considered. Operator/customer images
//     are invisible to this function.
//   - BRIDGE images: deleted unless their (canonical) name is in
//     keepNames. keepNames carries the current build's canonical
//     name(s), so this prunes superseded build hashes and keeps the
//     live one. A bridge image whose name IS in keepNames is never
//     touched regardless of age.
//   - CLIP images: deleted once older than clipMaxAge. Clipboard
//     payloads are single-use churn; we keep a short grace window so
//     an in-flight attach isn't pulled out from under itself. (Age is
//     derived from the entry's creation time when the catalog exposes
//     it; when it does not — older PC builds omit it from our trimmed
//     /images/list view — clip images are conservatively skipped and
//     left for a future sweep that can see the timestamp. The keep
//     set still protects any clip name passed in keepNames.)
//   - REFERENCE SAFETY: each delete is ATTEMPTED; a Prism "image in
//     use / cannot delete" failure is treated as a SKIP (logged), NOT
//     a force-detach and NOT a fatal error. The image is retried on
//     the next sweep once the VM no longer references it.
//
// keepNames is matched on the full canonical NAME (not UUID), so it
// protects ALL duplicate entries that share a kept name — important
// because Prism allows duplicate names and the dedupe race can create
// a transient duplicate of the live build. clipMaxAge of 0 disables
// clip pruning (only build-hash pruning runs).
func (c *Client) GarbageCollect(ctx context.Context, keepNames []string, clipMaxAge time.Duration) error {
	owned, err := c.ListPluginImages(ctx)
	if err != nil {
		return fmt.Errorf("gc list: %w", err)
	}

	keep := make(map[string]struct{}, len(keepNames))
	for _, n := range keepNames {
		keep[n] = struct{}{}
	}

	now := time.Now()
	var deleted, skipped, kept int
	for _, e := range owned {
		// Never delete a name we were told to keep — protects the live build
		// and every duplicate entry sharing that name.
		if _, ok := keep[e.Name]; ok {
			kept++
			continue
		}

		var reason string
		switch {
		case strings.HasPrefix(e.Name, AgentImagePrefix) || strings.HasPrefix(e.Name, LegacyBridgeImagePrefix):
			// A superseded agent ISO (not in keepNames) → prune. Covers both the
			// new agent naming and the legacy content-addressed bridge names.
			reason = "superseded agent build"
		case strings.HasPrefix(e.Name, ClipImagePrefix):
			if clipMaxAge <= 0 {
				kept++
				continue
			}
			// Without a creation timestamp we cannot age it safely; skip and
			// let a future sweep (or build-hash pruning above) handle it.
			if e.CreatedAt.IsZero() {
				kept++
				continue
			}
			if now.Sub(e.CreatedAt) < clipMaxAge {
				kept++
				continue
			}
			reason = "expired clipboard payload"
		default:
			// Defense in depth — ListPluginImages already filtered, but never
			// touch anything outside our prefixes.
			kept++
			continue
		}

		if err := c.DeleteImage(ctx, e.UUID); err != nil {
			if imageInUse(err) {
				// Referenced by a VM — leave it; retry next sweep. NOT fatal,
				// NOT force-detached.
				skipped++
				if c.log != nil {
					c.log.Info("gc: image in use, skipping (retry next sweep)",
						"image", e.UUID, "name", e.Name)
				}
				continue
			}
			// Any other failure: log and move on. A single bad delete must not
			// abort the whole sweep.
			skipped++
			if c.log != nil {
				c.log.Warn("gc: delete failed, skipping",
					"image", e.UUID, "name", e.Name, "err", err)
			}
			continue
		}
		deleted++
		if c.log != nil {
			c.log.Info("gc: deleted image", "image", e.UUID, "name", e.Name, "reason", reason)
		}
	}

	if c.log != nil {
		c.log.Info("gc: sweep complete",
			"owned", len(owned), "deleted", deleted, "skipped", skipped, "kept", kept)
	}
	return nil
}
