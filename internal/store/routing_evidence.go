// Copyright 2026 Furkan Anıl Öztaş
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"math"
	"regexp"
	"strconv"
	"strings"

	"github.com/faoztas/ahenk-mcp/internal/model"
)

// RoutingEvidence projects the persisted admission decision and execution records
// from a job that the caller has already authorized.
type RoutingEvidence struct {
	RoutingProfile             model.RoutingProfile
	RoutingProfileSource       string
	FirstDecision              *FirstRoutingDecision
	ExecutionAttempts          []RoutingExecutionAttempt
	ExecutionAttemptsTruncated bool
	ReasonCode                 string
}

type FirstRoutingDecision struct {
	Provider       string   `json:"provider,omitempty"`
	Model          string   `json:"model,omitempty"`
	SelectedModel  string   `json:"selected_model,omitempty"`
	ResolvedEffort string   `json:"resolved_effort,omitempty"`
	Reason         string   `json:"reason,omitempty"`
	CapacityState  string   `json:"capacity_state,omitempty"`
	CostClass      string   `json:"cost_class,omitempty"`
	ProfileScore   float64  `json:"profile_score,omitempty"`
	QuotaPressure  *float64 `json:"quota_pressure,omitempty"`
}

type RoutingExecutionAttempt struct {
	Number       int    `json:"number,omitempty"`
	Provider     string `json:"provider,omitempty"`
	Model        string `json:"model,omitempty"`
	TargetEffort string `json:"target_effort,omitempty"`
	Status       string `json:"status"`
	ReasonCode   string `json:"reason_code,omitempty"`
	HTTPStatus   int    `json:"http_status,omitempty"`
}

const routingExecutionAttemptLimit = 32

// ModelIdentity verifies an exact provider/model pair against trusted registry
// data. An empty provider asks for an exact catalog model name when the
// legacy selection has no separate provider field.
type ModelIdentity func(provider, model string) bool

// ProjectRoutingEvidence uses the most recent attempts in the job snapshot.
// Legacy dispatch rows can exist before execution and have job-level terminal status.
func ProjectRoutingEvidence(job model.Job, known ModelIdentity) RoutingEvidence {
	start := max(0, len(job.Attempts)-routingExecutionAttemptLimit)
	attempts := job.Attempts[start:]
	out := RoutingEvidence{
		RoutingProfileSource:       "legacy",
		ExecutionAttempts:          make([]RoutingExecutionAttempt, 0, len(attempts)),
		ExecutionAttemptsTruncated: start > 0,
		ReasonCode:                 routingReason(job.Reason),
	}
	if job.RoutingProfile.Valid() {
		out.RoutingProfile = job.RoutingProfile
	}
	switch job.RoutingProfileSource {
	case "request", "batch", "operator", "repo_config":
		out.RoutingProfileSource = job.RoutingProfileSource
	}

	if r := job.InitialRoute; r != nil {
		out.FirstDecision = &FirstRoutingDecision{
			Provider:       routeIdentifier(r.Provider, false),
			Model:          known.modelIdentifier(r.Provider, r.Model),
			SelectedModel:  selectedIdentifier(r.SelectedModel, known),
			ResolvedEffort: routingEffort(r.ResolvedEffort), Reason: routingReason(r.Reason),
		}
		switch r.CapacityState {
		case "HEALTHY", "LOW", "CRITICAL", "EXHAUSTED", "UNKNOWN", "STALE", "RATE_LIMITED", "PROTECTED_RESERVE", "UNAVAILABLE":
			out.FirstDecision.CapacityState = r.CapacityState
		}
		switch r.CostClass {
		case "free", "included", "unknown":
			out.FirstDecision.CostClass = r.CostClass
		}
		if r.ProfileScore > 0 && r.ProfileScore <= math.Nextafter(1, 2) {
			out.FirstDecision.ProfileScore = min(1, r.ProfileScore)
		}
		switch r.CapacityState {
		case "HEALTHY", "LOW", "CRITICAL", "EXHAUSTED", "":
			// Old decisions omit the state; a zero then provides no measurement.
			if r.QuotaPressure >= 0 && r.QuotaPressure <= 1 && (r.CapacityState != "" || r.QuotaPressure > 0) {
				pressure := r.QuotaPressure
				out.FirstDecision.QuotaPressure = &pressure
			}
		}
	}

	for _, a := range attempts {
		attempt := RoutingExecutionAttempt{
			Number: max(0, a.Number), Provider: routeIdentifier(a.Provider, false),
			Model:        known.modelIdentifier(a.Provider, a.Model),
			TargetEffort: routingEffort(a.ResolvedEffort), Status: "UNKNOWN", ReasonCode: routingReason(a.Code),
		}
		if a.HTTPStatus >= 100 && a.HTTPStatus <= 599 {
			attempt.HTTPStatus = a.HTTPStatus
		}
		if attempt.ReasonCode == "" && (a.HTTPStatus == 429 || a.UpstreamStatus == 429) {
			attempt.ReasonCode = "RATE_LIMIT"
		}
		switch {
		case a.FinishedAt != nil:
			attempt.Status = "SUCCESS"
			if a.Code != "" || attempt.ReasonCode == "RATE_LIMIT" {
				attempt.Status = "FAILED"
			}
		case !a.StartedAt.IsZero():
			attempt.Status = "UNFINISHED"
		}
		out.ExecutionAttempts = append(out.ExecutionAttempts, attempt)
	}
	return out
}

// ProjectLegacyRoutingAudit keeps the legacy row fields while filtering stored
// strings for display. Callers must authorize access to the row first.
func ProjectLegacyRoutingAudit(row JobRoutingAudit, known ModelIdentity) JobRoutingAudit {
	if !model.ValidID(row.JobID) || model.ContainsSecret([]byte(row.JobID)) {
		row.JobID = ""
	}
	switch row.TaskCategory {
	case "coding", "debugging", "code_review", "architecture", "planning", "research", "agent_orchestration":
	default:
		row.TaskCategory = ""
	}
	row.AssignedModel = selectedIdentifier(row.AssignedModel, known)
	row.AssignedEffort = routingEffort(row.AssignedEffort)
	row.RouteReason = routingReason(row.RouteReason)
	switch row.DispatchStatus {
	case "DISPATCHED", "SUCCESS", "FAILED":
	default:
		row.DispatchStatus = ""
	}
	return row
}

func (known ModelIdentity) modelIdentifier(provider, name string) string {
	return routeIdentifier(name, provider != "" && known != nil && routeHostPattern.MatchString(name) && known(provider, name))
}

func selectedIdentifier(value string, known ModelIdentity) string {
	if known != nil && routeHostPattern.MatchString(value) && known("", value) {
		return routeIdentifier(value, true)
	}
	provider, name, qualified := strings.Cut(value, "/")
	if qualified && routeHostPattern.MatchString(name) {
		// Validate the model on its own so a provider prefix cannot disguise a
		// host:port as a tag in a nested model namespace.
		if len(value) > 256 || routeIdentifier(provider, false) == "" || known.modelIdentifier(provider, name) == "" {
			return ""
		}
		return value
	}
	return routeIdentifier(value, false)
}

var routeHostPattern = regexp.MustCompile(`(?i)\.[a-z]`)
var numericHostPattern = regexp.MustCompile(`^[0-9]+(?:\.[0-9]+)+$`)

// Route identifiers allow model namespaces and tags. Free text, URL syntax,
// and credentials belong outside the evidence.
func routeIdentifier(value string, verified bool) string {
	if len(value) > 256 || model.ContainsSecret([]byte(value)) {
		return ""
	}
	segments := strings.Split(value, "/")
	for i, segment := range segments {
		name, tag, tagged := strings.Cut(segment, ":")
		if !model.ValidID(name) || numericHostPattern.MatchString(name) || (!verified && routeHostPattern.MatchString(name)) || strings.EqualFold(name, "localhost") {
			return ""
		}
		if tagged {
			if i != len(segments)-1 || !model.ValidID(tag) || numericHostPattern.MatchString(tag) || (!verified && routeHostPattern.MatchString(tag)) {
				return ""
			}
			if _, err := strconv.ParseUint(tag, 10, 16); err == nil && i == 0 {
				return ""
			}
			switch strings.ToLower(name) {
			case "http", "https", "ws", "wss", "ftp", "file", "unix", "tcp", "udp":
				return ""
			}
		}
	}
	return value
}

func routingEffort(value string) string {
	switch value {
	case "low", "medium", "high", "xhigh", "max":
		return value
	default:
		return ""
	}
}

func routingReason(value string) string {
	switch value {
	case "ROUTINE_TASK_LOW_EFFORT", "QUOTA_PRESSURE_THROTTLED", "QUOTA_CRITICAL_LOW_EFFORT", "NOMINAL_HIGH_EFFORT",
		"STANDARD_EFFORT", "EXPLICIT_TARGET_EFFORT", "EXPLICIT_TARGET_PREFERENCE",
		"PROFILE_PRICE_PERFORMANCE", "PROFILE_QUALITY", "PROFILE_PRICE", "PRICE_EVIDENCE_REQUIRED", "QUOTA_EXHAUSTED", "QUOTA_CRITICAL_HEADROOM", "RATE_LIMIT",
		"AGY_WEEKLY_PACING", "AGY_WEEKLY_RESERVE_PROTECTED", "AGY_FAMILY_IN_FLIGHT", "COOLDOWN", "CIRCUIT_OPEN",
		"NO_ELIGIBLE_ROUTE", "CLIENT_CANCELLED", "CANCELLED", "ATTEMPT_TIMEOUT", "DEADLINE_EXPIRED",
		"OWNER_RESTARTED", "OWNER_DISCONNECTED", "TEMPORARY_PROVIDER_ERROR", "EXECUTION_ERROR", "CLEANUP_UNCONFIRMED",
		"POLICY_DENIED", "PERMISSION_DENIED", "AUTH_REQUIRED", "ZERO_COST_UNVERIFIED", "QUALITY_EVIDENCE_REQUIRED",
		"PRIVATE_SOURCE_ROUTE_DENIED", "HANDOFF_INVALID", "REPO_MISMATCH", "SNAPSHOT_CHANGED",
		"WORKSPACE_ENTRY_UNSUPPORTED", "BINARY_UNVERIFIED", "BINARY_CHANGED", "MODEL_UNVERIFIED", "INVALID_OUTPUT",
		"DETERMINISTIC_CHECK_NOT_CONFIGURED", "DETERMINISTIC_CHECK_FAILED", "DEPENDENCY_FAILED", "CHECKPOINT_AVAILABLE",
		"AGY_INIT_MISMATCH", "AGY_INIT_INVALID", "AGY_TOOL_SCOPE_UNVERIFIED", "AGY_PROFILE_UNVERIFIED",
		"HANDOFF_PROVIDER_POLICY", "WRITER_ISOLATION_UNVERIFIED", "TRUSTED_LOCAL_PROVIDER_UNAVAILABLE",
		"OPENCODE_MODEL_IN_FLIGHT", "OPENCODE_RECHECK_DEFERRED", "OPENCODE_RECHECK_ROUTE_REQUIRED",
		"ROLE_CALIBRATION_FAILED", "ROLE_CALIBRATION_ROUTE_REQUIRED", "M1_ROUTE_NOT_VERIFIED", "ACCOUNT_ATTESTATION_REQUIRED", "ACCOUNT_MISMATCH",
		"OWNER_CONSENT_REQUIRED", "GRANT_EXPIRED", "DATA_HANDLING_UNREVIEWED", "ENGINE_UNAVAILABLE", "REPO_NOT_REGISTERED",
		"REPO_POLICY_CHANGED", "BATCH_CHILD_FAILED", "INDEPENDENT_REVIEW_REQUIRED", "INDEPENDENT_REVIEW_NOT_APPROVED",
		"TRUSTED_LOCAL_POLICY_REQUIRED", "ACCESS_DENIED", "AGY_WORKER_ERROR", "ARTIFACT_SAVE_FAILED", "CHECKPOINT_READY",
		"CHECK_FAILED", "CLAUDE_LAUNCH_FAILED", "CODEX_LAUNCH_FAILED", "CONTEXT_LIMIT", "DIGEST_MISMATCH", "EXECUTION_FAILED",
		"INVALID_PLAN", "MODEL_UNAVAILABLE", "OPENCODE_CATALOG_UNAVAILABLE", "OPENCODE_UPSTREAM_UNAVAILABLE", "PATH_NOT_APPROVED",
		"DATA_CLASS_MISMATCH", "BRIEF_NOT_APPROVED", "DETERMINISTIC_CHECK_REQUIRED", "PERSONAL_DATA_CLASSIFIED", "PRIVATE_GRANT_REQUIRED", "PRIVATE_MODE_UNSUPPORTED", "PROVIDER_STARTUP_FAILED",
		"PROVIDER_STARTUP_TIMEOUT", "PROVIDER_UNAVAILABLE", "QUOTA_RESERVE", "RECURSIVE_DELEGATION_DENIED", "RESOURCE_LIMIT",
		"SECRET_DETECTED", "ZERO_COST_UNAVAILABLE":
		return value
	}
	for _, prefix := range []string{"OPENCODE_HTTP_", "OPENCODE_UPSTREAM_HTTP_"} {
		status, ok := strings.CutPrefix(value, prefix)
		if ok && len(status) == 3 {
			if code, err := strconv.Atoi(status); err == nil && code >= 100 && code <= 599 {
				return value
			}
		}
	}
	return ""
}
