// Package api — v3 spec-driven power transitions for the consent-gated
// mount power cycle.
//
// The v4 $actions path (PowerAction) stays the operator power-button surface.
// The orchestrated mount cycle instead drives the v3 spec round-trip
// (PUT vms/{uuid} with spec.resources.power_state), because:
//
//   - OFF can carry power_state_mechanism {"mechanism":"ACPI"} — the graceful
//     guest shutdown the cycle is REQUIRED to use. This client never issues a
//     hard power-off: if ACPI doesn't get the guest to OFF, the cycle aborts
//     and the serial add is deferred instead.
//   - It is the same GET → mutate → PUT → task-wait shape as the other VM
//     spec edits in this package, with the same fidelity guarantees
//     (unmodeled fields + metadata.spec_version round-trip, status dropped)
//     and the same one-shot 409 retry.

package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// SetPowerStateV3 transitions the VM to state ("ON" or "OFF") via the v3
// spec round-trip and waits for the resulting task. OFF always uses the
// graceful ACPI mechanism — never a hard off. Retries once on HTTP 409.
//
// A SUCCEEDED task means Prism accepted and applied the transition request;
// for OFF the guest may still take time to actually halt (or ignore ACPI
// entirely), so callers confirm with WaitForPowerState.
func (c *Client) SetPowerStateV3(ctx context.Context, vmUUID, state string) error {
	state = strings.ToUpper(state)
	if state != "ON" && state != "OFF" {
		return fmt.Errorf("unsupported v3 power state %q (only ON/OFF)", state)
	}
	err := c.setPowerStateV3Once(ctx, vmUUID, state)
	if isConflict409(err) {
		if c.log != nil {
			c.log.Warn("v3 power transition hit 409 — refetching spec and retrying once",
				"vm", vmUUID, "state", state)
		}
		err = c.setPowerStateV3Once(ctx, vmUUID, state)
	}
	return err
}

func (c *Client) setPowerStateV3Once(ctx context.Context, vmUUID, state string) error {
	var full json.RawMessage
	if err := c.doJSON(ctx, http.MethodGet, "/api/nutanix/v3/vms/"+url.PathEscape(vmUUID), nil, &full); err != nil {
		return fmt.Errorf("get vm: %w", err)
	}
	var doc map[string]interface{}
	if err := json.Unmarshal(full, &doc); err != nil {
		return fmt.Errorf("parse vm doc: %w", err)
	}
	spec, _ := doc["spec"].(map[string]interface{})
	if spec == nil {
		return fmt.Errorf("vm spec is nil")
	}
	resources, _ := spec["resources"].(map[string]interface{})
	if resources == nil {
		return fmt.Errorf("vm resources is nil")
	}
	resources["power_state"] = state
	if state == "OFF" {
		// Graceful guest shutdown via ACPI — the only OFF mechanism this
		// client will ever request (operator-transparency hard requirement:
		// no hard power-off anywhere in the mount cycle).
		resources["power_state_mechanism"] = map[string]interface{}{"mechanism": "ACPI"}
	}
	delete(doc, "status")

	var putResp prismCreateImageResp // /vms PUT shares this shape
	if err := c.doJSON(ctx, http.MethodPut, "/api/nutanix/v3/vms/"+url.PathEscape(vmUUID), doc, &putResp); err != nil {
		return fmt.Errorf("power %s: %w", state, err)
	}
	taskUUID := string(putResp.Status.ExecutionContext.TaskUUID)
	if err := c.waitForTask(ctx, taskUUID, 2*time.Minute); err != nil {
		return fmt.Errorf("power %s task: %w", state, err)
	}
	return nil
}

// WaitForPowerState polls the VM's status power_state until it equals want,
// every `every`, giving up after `total`. Honors ctx. The mount power cycle
// uses it to confirm the guest actually reached OFF after the ACPI request
// (a guest can ignore ACPI; the cycle then aborts and defers — never forces).
func (c *Client) WaitForPowerState(ctx context.Context, vmUUID, want string, total, every time.Duration) error {
	deadline := time.Now().Add(total)
	for {
		vm, err := c.GetVM(ctx, vmUUID)
		if err != nil {
			return fmt.Errorf("poll power state: %w", err)
		}
		if strings.EqualFold(vm.PowerState, want) {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("vm %s did not reach power state %s within %s (last %q)",
				vmUUID, want, total, vm.PowerState)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(every):
		}
	}
}
