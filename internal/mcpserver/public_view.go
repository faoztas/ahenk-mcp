// Copyright 2026 Furkan Anıl Öztaş
// SPDX-License-Identifier: Apache-2.0

package mcpserver

import (
	"bytes"
	"encoding/json"
	"strings"
)

// A publicShape lists the fields of a wire record. Nested identity-bearing
// records have their own shape. Internal and persisted types keep their full
// identities for receipt checks, private grants, and quota matching.
type publicShape map[string]publicShape

func fields(names string, nested publicShape) publicShape {
	if nested == nil {
		nested = publicShape{}
	}
	for _, key := range strings.Fields(names) {
		nested[key] = nil
	}
	return nested
}

var publicCost = fields("documented_cost_protection runtime_cost_protection_verified policy status", publicShape{
	"verified_route": fields("provider model fingerprint adapter transport endpoint scope", nil),
})
var publicCapability = fields(`opencode_endpoint quality_evidence audit_valid_until audit_latency_ms opencode_transport
	opencode_catalog_sha256 execution_profiles provider model fp_tier headroom_percent
	headroom_state status reason execution_mode fingerprint speed`, publicShape{"cost": publicCost})
var publicQuota = fields(`retry_at recovery_state cooldown_scope endpoint quality_evidence last_success_at
	last_failure_at last_failure_code retry_source opencode_transport opencode_provider
	provider model health_scope bucket family window_minutes reason unit remaining state
	source requested_source confidence account_match observed_at reset_at`, publicShape{"cost": publicCost})
var publicCapacity = fields("headroom", publicShape{"codex": publicQuota, "agy": publicQuota, "opencode_free": publicQuota})
var publicDecision = fields(`resolved_effort ranking_reason cost_class profile_score cooldown_scope recovery_state
	retry_source policy_eligible agy_weekly_buffer_percent task_class quality_role
	quality_tier quality_source fp_tier headroom_percent headroom_state capacity_state
	quota_observed_at agy_weekly_floor_percent agy_weekly_threshold_percent
	agy_reserve_override warning_codes opencode_transport quota_bucket quota_remaining_percent
	quota_reset_at controller_remaining_percent fingerprint provider model reason health_scope
	retry_at estimate_seconds`, publicShape{"cost": publicCost})
var publicAttempt = fields(`resolved_effort effective_scope opencode_recheck opencode_endpoint retry_hints
	cooldown_scope agy_weekly_buffer_percent checkpoint resumed_checkpoint checks
	opencode_transport opencode codex claude handoffs number provider model fingerprint
	started_at deadline_at heartbeat_at finished_at code retry_after_ms http_status
	upstream_status retry_source provider_rate_limit_scope health_scope retry_at
	rate_limit_suppressed`, publicShape{
	"cost": publicCost,
	"agy": fields(`weekly_reserve native_child session profile observed_tools native_finish_observed
	last_event model advertised_tools permission_mode subscription_quota_verified
	rejected_step startup_verified terminal_verified prompt_sent input_tokens output_tokens
	total_tokens`, publicShape{"cost": publicCost}),
})
var publicResult = fields(`role_calibration checks review summary_sha256 grant_digest snapshot_id summary artifact_id
	artifact_sha256 verification preimages postimages`, publicShape{"cost": publicCost})

type publicView struct {
	value any
	shape publicShape
}

func (v publicView) MarshalJSON() ([]byte, error) {
	data, err := json.Marshal(v.value)
	if err != nil {
		return nil, err
	}
	return v.shape.project(data)
}

func (shape publicShape) project(data json.RawMessage) (json.RawMessage, error) {
	if shape == nil || bytes.Equal(data, []byte("null")) {
		return data, nil
	}
	if len(data) > 0 && data[0] == '[' {
		var items []json.RawMessage
		if err := json.Unmarshal(data, &items); err != nil {
			return nil, err
		}
		for i, item := range items {
			view, err := shape.project(item)
			if err != nil {
				return nil, err
			}
			items[i] = view
		}
		return json.Marshal(items)
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(data, &obj); err != nil {
		return nil, err
	}
	for key, value := range obj {
		child, ok := shape[key]
		if !ok {
			delete(obj, key)
			continue
		}
		projected, err := child.project(value)
		if err != nil {
			return nil, err
		}
		obj[key] = projected
	}
	return json.Marshal(obj)
}
