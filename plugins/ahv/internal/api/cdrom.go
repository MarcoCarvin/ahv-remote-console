// Package api — CD-ROM disk attach for Prism v3.
//
// Endpoints in this file:
//
//   GET /api/nutanix/v3/vms/{uuid}     — full VM spec (round-trip for PUT)
//   PUT /api/nutanix/v3/vms/{uuid}     — full-spec replace, adds CD-ROM
//   GET /api/nutanix/v3/tasks/{uuid}   — task poll (shared with images.go)
//
// Why GetVMSpec is separate from the existing client.GetVM
//
// client.GetVM returns a *VMDetail with three trimmed fields
// (UUID/Name/ClusterUUID) used by the console-open flow. The
// attach-CD-ROM flow needs the *entire* VM spec because Prism v3's
// PUT is a full-spec replace — any field we omit gets cleared on
// the upstream side. So GetVMSpec round-trips a generic JSON-shaped
// VMState that we modify and PUT back wholesale. Two narrow
// methods are clearer than one wide one with optional return
// shapes.

package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

// VMState is the parsed shape we need to mutate the disk_list on
// a running VM. We carry:
//
//   - Metadata: round-tripped as-is. PUT requires the original
//     metadata.spec_version (Prism uses it for optimistic
//     concurrency); we don't read it, just pass it back.
//   - Spec.Name + Spec.Resources.DiskList: read so we can inspect
//     existing CD-ROMs (for dedupe) and decide the next adapter
//     index for a new one.
//   - rawSpec: the entire spec sub-tree as json.RawMessage. We
//     send this back on PUT instead of re-marshaling our typed
//     copy, so any field Prism added on the wire (resources we
//     don't model) survives the round trip.
//
// The two-pass design (typed read for mutation, raw passthrough
// for fidelity) is what makes the PUT safe against Prism schema
// drift. A future PC adds a `nutanix_guest_tools` field under
// resources; we never look at it, but we don't accidentally erase
// it either.
type VMState struct {
	Metadata json.RawMessage `json:"metadata"`
	Spec     VMSpec          `json:"spec"`

	// rawSpec is set by GetVMSpec from the same JSON body Spec
	// was decoded from. AttachCDROM uses it to splice a single
	// disk entry into the disk_list without touching anything
	// else.
	rawSpec json.RawMessage `json:"-"`
}

// VMSpec is the trimmed view of the VM spec sub-tree that we
// actively read. Fields outside this struct still travel via
// rawSpec — we only need to introspect disk_list.
type VMSpec struct {
	Name      string         `json:"name"`
	Resources VMSpecResource `json:"resources"`
}

type VMSpecResource struct {
	DiskList []VMDisk `json:"disk_list"`
}

// VMDisk is one entry in the spec.resources.disk_list. Prism
// distinguishes disks by device_properties.device_type ("DISK" vs
// "CDROM") and by device_properties.disk_address.{adapter_type,
// device_index}. We carry data_source_reference.uuid so the
// dedupe check ("is this image already mounted?") works on a
// stable identifier.
type VMDisk struct {
	UUID                string             `json:"uuid,omitempty"`
	DeviceProperties    VMDeviceProperties `json:"device_properties"`
	DataSourceReference *VMDataSourceRef   `json:"data_source_reference,omitempty"`
	DiskSizeBytes       int64              `json:"disk_size_bytes,omitempty"`
	DiskSizeMib         int64              `json:"disk_size_mib,omitempty"`
	StorageConfig       json.RawMessage    `json:"storage_config,omitempty"`
}

type VMDeviceProperties struct {
	DeviceType  string        `json:"device_type"`
	DiskAddress VMDiskAddress `json:"disk_address"`
}

type VMDiskAddress struct {
	AdapterType string `json:"adapter_type"`
	DeviceIndex int    `json:"device_index"`
}

type VMDataSourceRef struct {
	Kind string `json:"kind"`
	UUID string `json:"uuid"`
}

// GetVMSpec fetches the full /api/nutanix/v3/vms/{uuid} payload
// and decodes both a typed view (for mutation) and a raw passthrough
// (for fidelity on the PUT). Returns *VMState; the caller can mutate
// .Spec.Resources.DiskList in place before calling UpdateVMSpec.
func (c *Client) GetVMSpec(ctx context.Context, vmUUID string) (*VMState, error) {
	if vmUUID == "" {
		return nil, errors.New("get vm spec: vmUUID required")
	}
	// We need the raw body to preserve any spec fields we don't
	// model. doJSON would decode straight to our typed struct
	// and discard the rest, so we go through newRequest and
	// keep the body.
	req, err := c.newRequest(ctx, http.MethodGet, "/api/nutanix/v3/vms/"+url.PathEscape(vmUUID), nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 32*1024*1024))
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode/100 != 2 {
		return nil, &APIError{Method: http.MethodGet, Path: req.URL.Path, Status: resp.StatusCode, Body: string(raw)}
	}
	var state VMState
	if err := json.Unmarshal(raw, &state); err != nil {
		return nil, fmt.Errorf("decode vm spec: %w", err)
	}
	// Re-extract the spec sub-tree from the raw body so we can
	// PUT it back untouched. A second pass into a string-keyed
	// map is the cheapest way to do this without writing a
	// json-tree walker.
	var top map[string]json.RawMessage
	if err := json.Unmarshal(raw, &top); err != nil {
		return nil, fmt.Errorf("decode top-level: %w", err)
	}
	state.rawSpec = top["spec"]
	return &state, nil
}

// AttachCDROM attaches the given imageUUID as a SATA CD-ROM on
// the VM and waits for the operation to complete.
//
// Idempotent: if the VM already has a CD-ROM whose
// data_source_reference.uuid matches imageUUID, this returns
// taskUUID="" + reused=true + driveLetter="D:" without making a
// PUT request. Re-running this against an already-installed bridge
// is a safe no-op.
//
// Drive letter hint: Windows assigns CD-ROMs starting at D: (A:
// reserved for floppy, C: for the system disk). With two SATA
// CD-ROMs the second one ends up at E:, and so on. This is a
// best-effort guess that the renderer surfaces in the
// installer-mounted UX — exact mapping depends on the guest, but
// D: is correct for the vast majority of single-CD-ROM Windows
// setups.
func (c *Client) AttachCDROM(ctx context.Context, vmUUID, imageUUID string) (string, string, bool, error) {
	if vmUUID == "" {
		return "", "", false, errors.New("attach cdrom: vmUUID required")
	}
	if imageUUID == "" {
		return "", "", false, errors.New("attach cdrom: imageUUID required")
	}
	taskUUID, drive, reused, err := c.attachCDROMOnce(ctx, vmUUID, imageUUID)
	if isConflict409(err) {
		// One-shot 409 retry (Prism CONCURRENT_REQUESTS_NOT_ALLOWED /
		// spec_version conflict, observed in the field): a fresh GET picks up
		// the new spec_version, re-apply the mutation, PUT once more.
		if c.log != nil {
			c.log.Warn("attach cdrom hit 409 — refetching spec and retrying once", "vm", vmUUID)
		}
		taskUUID, drive, reused, err = c.attachCDROMOnce(ctx, vmUUID, imageUUID)
	}
	return taskUUID, drive, reused, err
}

// attachCDROMOnce is a single GET → mutate → PUT → task-wait attempt;
// AttachCDROM wraps it with the one-shot 409 retry.
func (c *Client) attachCDROMOnce(ctx context.Context, vmUUID, imageUUID string) (string, string, bool, error) {
	state, err := c.GetVMSpec(ctx, vmUUID)
	if err != nil {
		return "", "", false, fmt.Errorf("fetch vm spec: %w", err)
	}

	// AHV refuses to hot-add IDE/SATA disks to a powered-on VM
	// ("Cannot complete request in state On. IDE/SATA disk hot
	// add is not supported"). So we don't ADD a CD-ROM — we
	// MODIFY the existing one's media. Same operation a physical
	// "eject this disc, insert that disc" maps to, which IS
	// supported at runtime by every hypervisor including AHV.
	//
	// Strategy: find the first existing CD-ROM device. If the
	// VM has one, mutate its data_source_reference to our image
	// UUID. If the VM has none, surface a clear error asking the
	// operator to add a CD-ROM device in Prism (with the VM
	// powered off) and retry. Most AHV VMs ship with an empty
	// CD-ROM by default so this is rarely a real friction.
	cdromIdx := -1
	cdromCount := 0
	for i, d := range state.Spec.Resources.DiskList {
		if d.DeviceProperties.DeviceType != "CDROM" {
			continue
		}
		cdromCount++
		if d.DataSourceReference != nil && d.DataSourceReference.UUID == imageUUID {
			// Same image already in the drive — no-op.
			return "", driveLetterForCDROM(0), true, nil
		}
		if cdromIdx == -1 {
			// Take the first CD-ROM as our target. If the VM
			// has multiple CD-ROM slots, we always use the
			// first one — operators who need to keep an
			// existing ISO mounted somewhere should add a
			// second CD-ROM in Prism beforehand.
			cdromIdx = i
		}
	}
	if cdromIdx == -1 {
		return "", "", false, fmt.Errorf(
			"VM has no CD-ROM device — power off the VM in Prism, add an empty CD-ROM device, restart, then retry")
	}

	// Mutate the existing CD-ROM's data_source_reference. Leave
	// device_type / disk_address untouched so AHV understands this
	// as a media change rather than a device add.
	state.Spec.Resources.DiskList[cdromIdx].DataSourceReference = &VMDataSourceRef{
		Kind: "image",
		UUID: imageUUID,
	}

	// Splice the updated disk_list back into the raw spec
	// payload so we PUT *everything* Prism gave us, with only
	// disk_list replaced. This is the schema-drift-safe path.
	updatedSpec, err := spliceDiskList(state.rawSpec, state.Spec.Resources.DiskList)
	if err != nil {
		return "", "", false, fmt.Errorf("splice disk_list: %w", err)
	}

	// Build the PUT body. We send metadata + spec; api_version
	// is implied by the endpoint.
	put := map[string]json.RawMessage{
		"metadata": state.Metadata,
		"spec":     updatedSpec,
	}
	var putResp prismCreateImageResp // shares shape with /vms PUT: metadata.uuid + execution_context.task_uuid
	if err := c.doJSON(ctx, http.MethodPut, "/api/nutanix/v3/vms/"+url.PathEscape(vmUUID), put, &putResp); err != nil {
		return "", "", false, fmt.Errorf("update vm: %w", err)
	}
	taskUUID := string(putResp.Status.ExecutionContext.TaskUUID)

	// Wait for the attach to settle. Without this, callers
	// sometimes see "VM busy, retry" errors on subsequent
	// operations even though the CD-ROM lands fine.
	if err := c.waitForTask(ctx, taskUUID, 5*time.Minute); err != nil {
		return taskUUID, "", false, fmt.Errorf("wait for attach task: %w", err)
	}

	// We always mount into the FIRST CD-ROM device — that's the
	// one Windows assigns the lowest letter to. `cdromCount` is
	// retained as a sanity check / future hook but doesn't drive
	// the drive-letter hint anymore.
	_ = cdromCount
	return taskUUID, driveLetterForCDROM(0), false, nil
}

// DetachCDROM ejects the media from the VM's first CD-ROM drive by clearing
// its data_source_reference, and waits for the operation to complete.
//
// This is a media EJECT, not a device removal: device_type / disk_address are
// left untouched so AHV treats it like ejecting a disc (supported on a running
// VM, exactly like AttachCDROM's media change) rather than a SATA hot-remove
// (unsupported while powered on).
//
// HARD INVARIANT: only entries whose device_properties.device_type == "CDROM"
// are ever considered. A "DISK" entry is never touched — nulling a disk's
// data_source_reference would be data-loss.
//
// Idempotent: if the VM has no CD-ROM, or the first CD-ROM is already empty,
// this returns taskUUID="" + ejected=false without making a PUT request.
func (c *Client) DetachCDROM(ctx context.Context, vmUUID string) (string, bool, error) {
	if vmUUID == "" {
		return "", false, errors.New("detach cdrom: vmUUID required")
	}
	taskUUID, ejected, err := c.detachCDROMOnce(ctx, vmUUID)
	if isConflict409(err) {
		// One-shot 409 retry, mirroring AttachCDROM: a fresh GET picks up the
		// new spec_version, re-apply the mutation, PUT once more.
		if c.log != nil {
			c.log.Warn("detach cdrom hit 409 — refetching spec and retrying once", "vm", vmUUID)
		}
		taskUUID, ejected, err = c.detachCDROMOnce(ctx, vmUUID)
	}
	return taskUUID, ejected, err
}

// detachCDROMOnce is a single GET → mutate → PUT → task-wait attempt;
// DetachCDROM wraps it with the one-shot 409 retry.
func (c *Client) detachCDROMOnce(ctx context.Context, vmUUID string) (string, bool, error) {
	state, err := c.GetVMSpec(ctx, vmUUID)
	if err != nil {
		return "", false, fmt.Errorf("fetch vm spec: %w", err)
	}

	// Find the FIRST CD-ROM device. Never a DISK — that would be data-loss.
	cdromIdx := -1
	for i, d := range state.Spec.Resources.DiskList {
		if d.DeviceProperties.DeviceType != "CDROM" {
			continue
		}
		cdromIdx = i
		break
	}
	if cdromIdx == -1 {
		// No CD-ROM at all — nothing to eject. Treat as a no-op rather than an
		// error so a double-click on Eject doesn't surface a scary message.
		return "", false, nil
	}
	if state.Spec.Resources.DiskList[cdromIdx].DataSourceReference == nil {
		// Already empty — idempotent no-op, no PUT.
		return "", false, nil
	}

	// Clear the media. spliceDiskList re-marshals VMDisk with
	// DataSourceReference omitempty, so a nil ref serializes the disk WITHOUT
	// the field — exactly what Prism reads as an empty drive. device_type /
	// disk_address stay as-is so AHV sees an eject, not a hot-remove.
	state.Spec.Resources.DiskList[cdromIdx].DataSourceReference = nil

	updatedSpec, err := spliceDiskList(state.rawSpec, state.Spec.Resources.DiskList)
	if err != nil {
		return "", false, fmt.Errorf("splice disk_list: %w", err)
	}
	put := map[string]json.RawMessage{
		"metadata": state.Metadata,
		"spec":     updatedSpec,
	}
	var putResp prismCreateImageResp // /vms PUT shares this shape
	if err := c.doJSON(ctx, http.MethodPut, "/api/nutanix/v3/vms/"+url.PathEscape(vmUUID), put, &putResp); err != nil {
		return "", false, fmt.Errorf("update vm: %w", err)
	}
	taskUUID := string(putResp.Status.ExecutionContext.TaskUUID)
	if err := c.waitForTask(ctx, taskUUID, 5*time.Minute); err != nil {
		return taskUUID, false, fmt.Errorf("wait for detach task: %w", err)
	}
	return taskUUID, true, nil
}

// driveLetterForCDROM returns a best-guess Windows drive letter
// for the Nth CD-ROM on the VM (0-indexed). Convention used by
// the renderer:
//
//   - A: floppy index 0 (reserved, not surfaced here)
//   - B: floppy index 1 (reserved)
//   - C: system disk
//   - D: first CD-ROM
//   - E: second CD-ROM
//   - ...
//
// Past Z: we run out of letters; the renderer ignores empty
// hints, so we just return "" there.
func driveLetterForCDROM(existingCDROMs int) string {
	// existingCDROMs is the count of CD-ROMs already present
	// BEFORE we add ours. So our new CD-ROM is the
	// (existingCDROMs)-th index in zero-based terms (0 → D, 1 →
	// E, ...).
	const firstCDLetter = 'D'
	letter := byte(firstCDLetter) + byte(existingCDROMs) // #nosec G115 -- existingCDROMs is a small VM device count, always < 23
	if letter > 'Z' {
		return ""
	}
	return string([]byte{letter, ':'})
}

// spliceDiskList rebuilds the spec JSON with disk_list replaced.
// We decode the spec into a generic map, decode resources into a
// generic map, replace resources["disk_list"], then re-encode.
// Everything outside disk_list survives untouched.
func spliceDiskList(rawSpec json.RawMessage, disks []VMDisk) (json.RawMessage, error) {
	if len(rawSpec) == 0 {
		// No raw spec available (shouldn't happen — GetVMSpec
		// always sets it). Build a minimal spec with just the
		// disk list as a fallback.
		fallback := map[string]any{
			"resources": map[string]any{
				"disk_list": disks,
			},
		}
		return json.Marshal(fallback)
	}
	var spec map[string]json.RawMessage
	if err := json.Unmarshal(rawSpec, &spec); err != nil {
		return nil, fmt.Errorf("decode spec: %w", err)
	}
	var resources map[string]json.RawMessage
	if raw, ok := spec["resources"]; ok {
		if err := json.Unmarshal(raw, &resources); err != nil {
			return nil, fmt.Errorf("decode resources: %w", err)
		}
	} else {
		resources = map[string]json.RawMessage{}
	}
	disksRaw, err := json.Marshal(disks)
	if err != nil {
		return nil, fmt.Errorf("encode disk_list: %w", err)
	}
	resources["disk_list"] = disksRaw
	resourcesRaw, err := json.Marshal(resources)
	if err != nil {
		return nil, fmt.Errorf("encode resources: %w", err)
	}
	spec["resources"] = resourcesRaw
	return json.Marshal(spec)
}
