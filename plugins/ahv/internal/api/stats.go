// Package api — live VM utilization read via the Prism v3 groups/arithmos
// query.
//
// Live CPU / memory / disk / network utilization is NOT carried on
// GET /api/nutanix/v3/vms/{uuid} (that document holds spec + config, not the
// time-series sample). The live sample comes from the v3 `groups` query — a
// READ-ONLY POST against the arithmos store — which is exactly the call Prism
// Central's own UI makes to populate a VM's mini-charts.
//
// GetVMStats is strictly read-only: one POST to /api/nutanix/v3/groups, no spec
// mutation, no task wait. A non-2xx surfaces as the structured *APIError from
// doJSON verbatim so the router can map a 403 to rbac_denied.
//
// TIME WINDOW IS MANDATORY: arithmos is a time-series store. A groups query
// with no interval returns metric NAMES but no VALUES (every field n/a — one of
// the bugs this file fixes). The request therefore asks for samples over an
// explicit epoch-microsecond window [now-lookback, now] with a downsampling
// interval, so a recent point is always found.
//
// SCOPING: a single VM is selected by entity_ids:[<uuid>] (top-level), NOT by a
// filter_criteria "vm==<uuid>" string — "vm" is not a valid groups filter
// attribute for mh_vm, so that form matched nothing and returned all-n/a.
//
// ATTRIBUTE NAMES ARE NOT IN THE SWAGGER: `groups` is a dynamic arithmos query,
// so the v3 spec does not enumerate the per-entity stat attribute names. The
// names below are the canonical arithmos `vm` stat names that Prism Central's
// own per-VM charts request. AOS versions vary, so GetVMStats also emits a
// guarded debug log of the entity_type and the metric names the cluster
// actually returned (no PII) — that is how an operator confirms/iterates the
// names against a live cluster without a code change to inspect the response.

package api

import (
	"context"
	"net/http"
	"strconv"
	"time"
)

// statUnavailable is the sentinel a metric carries when Prism returned no value
// for it (the attribute was absent/empty in the group result). The renderer
// renders it as a muted "n/a" rather than a misleading 0.
const statUnavailable int64 = -1

// statsEntityType is the arithmos entity that carries live VM metrics. "mh_vm"
// (multi-hypervisor VM) is the arithmos VM entity Prism Central's per-VM charts
// query; plain "vm" is the v3 management entity and does NOT carry the
// time-series stats. If a live cluster returns an empty result, the guarded
// debug log surfaces the entity_type so this can be confirmed/flipped to "vm".
const statsEntityType = "mh_vm"

// Stats look-back window. arithmos samples VM metrics at ~30s resolution, so the
// query asks for an explicit epoch-microsecond window [now-lookback, now]: this
// (NOT number_of_intervals_for_latest_data, which does not drive stat VALUES in
// the groups stat path) is what makes arithmos return values instead of just
// metric names. A ~5-minute look-back guarantees a recent sample is in-window
// even if the most recent one hasn't been written yet.
const (
	statsDownsamplingIntervalSec = 30                 // arithmos VM-stat sampling resolution (seconds)
	statsLookbackUsec            = int64(300_000_000) // ~5 min look-back, in microseconds
)

// VMStatsSample is one point-in-time utilization read for a VM. CPU and memory
// are in parts-per-million (ppm; /10000 == percent). Disk/network rates are
// bytes per second. Any field equal to statUnavailable (-1) means Prism did not
// report that metric for this sample.
type VMStatsSample struct {
	CPUUsagePpm          int64
	MemoryUsagePpm       int64
	DiskBytesReadPerSec  int64
	DiskBytesWritePerSec int64
	NetBytesRxPerSec     int64
	NetBytesTxPerSec     int64
}

// Canonical arithmos VM-stat attribute names requested in the groups query.
// These are the live per-VM metrics Prism Central's own charts read. Kept in
// one place so the request and the response parse agree on the exact set.
//
// Disk: the controller I/O bandwidth metrics are reported in KBps (NOT
// bytes/sec) and are explicitly named read/write, so they need no direction
// juggling — but they MUST be scaled ×1024 at parse time (see kBpsToBps) since
// the renderer treats disk/net as raw bytes/sec. (The older
// controller_user_bytes_*_per_second names do not exist in arithmos and always
// came back empty → every field n/a.)
//
// Net: the only renderer-compatible (bytes/sec-style) per-VM net RATE fields are
// received_rate_kBps / transmitted_rate_kBps (KBps → ×1024), but those are
// confirmed only on the virtual_nic entity and may or may not roll up under
// mh_vm depending on AOS. Since the groups API SILENTLY returns unknown
// attributes empty (proven: the old hypervisor_*_bytes_per_second names came
// back empty, not a non-2xx, while the other metrics still resolved), we request
// every candidate spelling in ONE query and map whichever the cluster actually
// populates (firstAvailable). The cumulative byte COUNTERS are requested too but
// only as a diagnostic — they resolve under mh_vm but are monotonic counters,
// NOT rates, so they are deliberately NOT mapped into a bytes/sec field. The
// returned_metric_names log makes the right spelling self-evident on a live
// cluster (generate guest traffic, else net legitimately reads ~0).
const (
	attrCPUUsagePpm    = "hypervisor_cpu_usage_ppm"
	attrMemoryUsagePpm = "memory_usage_ppm"
	attrDiskReadBps    = "controller_read_io_bandwidth_kBps"
	attrDiskWriteBps   = "controller_write_io_bandwidth_kBps"
	// Net RX/TX rate candidates (KBps; ×1024). Bare + dotted-namespace spellings.
	attrNetRxRateKBps    = "received_rate_kBps"
	attrNetTxRateKBps    = "transmitted_rate_kBps"
	attrNetRxRateKBpsDot = "network.received_rate_kBps"
	attrNetTxRateKBpsDot = "network.transmitted_rate_kBps"
	// Cumulative byte counters — diagnostic only (resolve under mh_vm but are
	// counters, not rates); never mapped into a bytes/sec field.
	attrNetRxBytes = "hypervisor_num_received_bytes"
	attrNetTxBytes = "hypervisor_num_transmitted_bytes"
)

// statAttrs is the ordered set of attributes the query requests and the parse
// reads back. Single source of truth for both directions. The net candidates are
// all requested at once (the API ignores unknown names) so one query probes
// every spelling and the log reveals which populates.
var statAttrs = []string{
	attrCPUUsagePpm,
	attrMemoryUsagePpm,
	attrDiskReadBps,
	attrDiskWriteBps,
	attrNetRxRateKBps,
	attrNetTxRateKBps,
	attrNetRxRateKBpsDot,
	attrNetTxRateKBpsDot,
	attrNetRxBytes,
	attrNetTxBytes,
}

// groupsRequest is the v3 groups/arithmos query body (groups_get_entities_request).
// entity_type is the arithmos VM entity that carries the live metrics; entity_ids
// scopes it to one VM by UUID (the canonical scoping — the older
// filter_criteria "vm==<uuid>" used an attribute that does not exist for mh_vm,
// so the VM never matched and every field came back n/a); group_member_attributes
// names the metrics to return; and the start/end epoch-microsecond window +
// downsampling_interval supply the MANDATORY time window without which arithmos
// returns names-but-no-values.
type groupsRequest struct {
	EntityType            string            `json:"entity_type"`
	EntityIDs             []string          `json:"entity_ids,omitempty"`
	GroupMemberCount      int               `json:"group_member_count"`
	GroupMemberAttributes []groupsAttribute `json:"group_member_attributes"`
	FilterCriteria        string            `json:"filter_criteria,omitempty"`
	DownsamplingInterval  int               `json:"downsampling_interval"`
	StartTimeUsecs        int64             `json:"start_time_usecs"`
	EndTimeUsecs          int64             `json:"end_time_usecs"`
}

// groupsAttribute is one groups_requested_attribute. `operation` is an optional
// downsampling/aggregation function; it is omitted here (bare attribute) because
// the time window already collapses each metric to a value and a non-schema
// operation can cause the attribute to be dropped. Kept as a field so it can be
// set if a live cluster turns out to require it.
type groupsAttribute struct {
	Attribute string `json:"attribute"`
	Operation string `json:"operation,omitempty"`
}

// groupsResponse parses the nested arithmos result shape, matching the swagger
// chain groups_get_entities_response -> group_results[] (groups_group_result)
// -> entity_results[] (groups_entity) -> data[] (groups_field_data) ->
// values[] (groups_timevalue_pair){ values[] }.
//
// Only the first non-empty inner value of each named attribute is read — a
// single sample is all the panel needs.
type groupsResponse struct {
	EntityType   string `json:"entity_type"`
	GroupResults []struct {
		EntityResults []struct {
			Data []struct {
				Name   string `json:"name"`
				Values []struct {
					Values []string `json:"values"`
				} `json:"values"`
			} `json:"data"`
		} `json:"entity_results"`
	} `json:"group_results"`
}

// GetVMStats reads one live utilization sample for the VM via the v3 groups
// query. Read-only: a single POST, no mutation, no task wait. Missing/empty
// metrics map to -1 (unavailable) so an unparseable or absent attribute
// degrades gracefully instead of showing a wrong 0.
//
// vmUUID is already validated upstream by splitResourceID (canonical UUID), and
// it travels in the request BODY (not a URL path), so no path-escaping is
// needed.
func (c *Client) GetVMStats(ctx context.Context, vmUUID string) (*VMStatsSample, error) {
	attrs := make([]groupsAttribute, 0, len(statAttrs))
	for _, a := range statAttrs {
		// Bare attribute (no operation): the time window + downsampling resolve
		// each metric to a value. "LAST" is arithmos_cli vocabulary, not part of
		// the v3 groups REST schema, and sending it can make the attribute be
		// ignored — another path to all-n/a.
		attrs = append(attrs, groupsAttribute{Attribute: a})
	}
	now := time.Now().UnixMicro()
	req := groupsRequest{
		EntityType:            statsEntityType,
		EntityIDs:             []string{vmUUID},
		GroupMemberCount:      1,
		GroupMemberAttributes: attrs,
		DownsamplingInterval:  statsDownsamplingIntervalSec,
		StartTimeUsecs:        now - statsLookbackUsec,
		EndTimeUsecs:          now,
	}
	var resp groupsResponse
	if err := c.doJSON(ctx, http.MethodPost, "/api/nutanix/v3/groups", req, &resp); err != nil {
		return nil, err
	}

	byName := flattenGroupData(&resp)
	c.logStatsResponse(&resp, byName)

	return &VMStatsSample{
		CPUUsagePpm:          metricValue(byName, attrCPUUsagePpm),
		MemoryUsagePpm:       metricValue(byName, attrMemoryUsagePpm),
		DiskBytesReadPerSec:  kBpsToBps(metricValue(byName, attrDiskReadBps)),
		DiskBytesWritePerSec: kBpsToBps(metricValue(byName, attrDiskWriteBps)),
		// Map the first net RATE field the cluster populated (bare or dotted),
		// ×1024 (KBps→bytes/sec). The cumulative counters are intentionally NOT
		// used here — a monotonic counter in a bytes/sec field renders as an
		// ever-growing wrong number.
		NetBytesRxPerSec: kBpsToBps(firstAvailable(byName, attrNetRxRateKBps, attrNetRxRateKBpsDot)),
		NetBytesTxPerSec: kBpsToBps(firstAvailable(byName, attrNetTxRateKBps, attrNetTxRateKBpsDot)),
	}, nil
}

// firstAvailable returns the first of the named metrics that is present (not the
// unavailable sentinel), or unavailable if none resolved. Lets the query probe
// several candidate attribute spellings and use whichever the cluster returns.
func firstAvailable(byName map[string]string, names ...string) int64 {
	for _, n := range names {
		if v := metricValue(byName, n); v != statUnavailable {
			return v
		}
	}
	return statUnavailable
}

// kBpsToBps converts a KBps metric (the controller I/O bandwidth attributes are
// reported in KBps) to bytes/sec, preserving the -1 unavailable sentinel so an
// absent metric still degrades to n/a rather than -1024.
func kBpsToBps(v int64) int64 {
	if v == statUnavailable {
		return v
	}
	return v * 1024
}

// logStatsResponse emits a guarded, PII-free debug line so an operator can
// confirm the live arithmos attribute names against a real cluster and iterate
// on statAttrs without code spelunking. It logs ONLY the request entity_type,
// the response entity_type, the metric NAMES the cluster returned (not values —
// values are utilization, not PII, but logging only names keeps the line small
// and the contract "names to confirm"), and group/entity counts.
func (c *Client) logStatsResponse(resp *groupsResponse, byName map[string]string) {
	if c.log == nil {
		return
	}
	names := make([]string, 0, len(byName))
	for n := range byName {
		names = append(names, n)
	}
	entityCount := 0
	for _, g := range resp.GroupResults {
		entityCount += len(g.EntityResults)
	}
	// When the query matched no entity or parsed no metric, the panel shows
	// all-n/a — log that at WARN so it is visible at the runtime's default level
	// (which is above Debug). entity_count==0 ⇒ scoping/entity_type wrong;
	// names empty with entity_count>0 ⇒ attribute names wrong for this AOS. The
	// healthy case stays at Debug. PII-free either way: counts + metric NAMES
	// only, never values.
	logf := c.log.Debug
	if entityCount == 0 || len(names) == 0 {
		logf = c.log.Warn
	}
	logf("vm stats groups query result",
		"req_entity_type", statsEntityType,
		"resp_entity_type", resp.EntityType,
		"group_count", len(resp.GroupResults),
		"entity_count", entityCount,
		"returned_metric_names", names,
	)
}

// flattenGroupData walks the nested group/entity/data shape and returns a
// name->first-non-empty-value map. Later occurrences do not overwrite an
// already-captured value, so the first entity's sample wins.
func flattenGroupData(resp *groupsResponse) map[string]string {
	out := map[string]string{}
	for _, g := range resp.GroupResults {
		for _, e := range g.EntityResults {
			for _, d := range e.Data {
				if _, seen := out[d.Name]; seen {
					continue
				}
				for _, v := range d.Values {
					if len(v.Values) > 0 && v.Values[0] != "" {
						out[d.Name] = v.Values[0]
						break
					}
				}
			}
		}
	}
	return out
}

// metricValue parses the named metric as an int64, returning -1 (unavailable)
// when the metric is absent or not an integer.
func metricValue(byName map[string]string, name string) int64 {
	s, ok := byName[name]
	if !ok || s == "" {
		return statUnavailable
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return statUnavailable
	}
	return n
}
