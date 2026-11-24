// Copyright 2026 Furkan Anıl Öztaş
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"slices"
	"strings"
	"testing"

	"github.com/faoztas/ahenk-mcp/internal/model"
)

type clientAcceptanceTraceRow struct {
	Host       string
	JobID      string
	AttemptID  string
	Provider   string
	Tool       string
	ToolCallID string
	Sanitized  bool
	Trace      string
	Synthetic  bool
}

type clientAcceptanceResultControl struct {
	JobID        string
	AttemptID    string
	ResultSHA256 string
	Verified     bool
	ArtifactHash string
}

type clientAcceptanceEvidence struct {
	TraceRows        []clientAcceptanceTraceRow
	EffectiveProfile string
	ProfileSource    string
	Providers        []string
	JobIDs           []string
	AttemptIDs       []string
	ResultControl    clientAcceptanceResultControl
	Status           string
	LiveVerified     bool
	BlockedReason    string
	TraceValid       bool
}

func clientAcceptanceFault(code, reason, message string) error {
	return &model.Fault{Code: code, ProviderReason: reason, Message: message}
}

func isAhenkClientTool(tool string) bool {
	switch strings.TrimSpace(tool) {
	case "delegate_task", "ahenk.delegate_task", "mcp__ahenk__delegate_task":
		return true
	default:
		return false
	}
}

func normalizeClientHost(host string) string {
	host = strings.ToLower(strings.TrimSpace(host))
	switch host {
	case "codex", "codex-cli", "codex_cli":
		return "codex"
	case "claude", "claude-code", "claude_code", "claudecode":
		return "claude"
	default:
		return host
	}
}

// verifyClientAcceptanceEvidence checks consistency of an offline evidence record.
// Live provenance must be established by capturing actual host tool calls.
func verifyClientAcceptanceEvidence(ev clientAcceptanceEvidence, expected model.RoutingProfile) error {
	if !ev.TraceValid {
		return clientAcceptanceFault("INVALID_OUTPUT", "CLIENT_ACCEPTANCE_TRACE_INVALID", "client acceptance evidence requires a valid sanitized trace")
	}
	if len(ev.TraceRows) < 2 {
		return clientAcceptanceFault("INVALID_OUTPUT", "CLIENT_ACCEPTANCE_TRACE_MISSING", "client acceptance evidence requires explicit sanitized trace rows")
	}

	ahenkHosts := map[string]bool{}
	callIDs := map[string]bool{}
	actualProviders := map[string]bool{}
	resultBound := false
	for _, row := range ev.TraceRows {
		if strings.TrimSpace(row.Host) == "" {
			return clientAcceptanceFault("INVALID_OUTPUT", "CLIENT_ACCEPTANCE_HOST_MISSING", "client acceptance trace row is missing a host identity")
		}
		if !row.Sanitized {
			return clientAcceptanceFault("INVALID_OUTPUT", "CLIENT_ACCEPTANCE_TRACE_UNSANITIZED", "client acceptance trace row is not sanitized")
		}
		if model.ContainsSecret([]byte(row.Trace)) || model.ContainsPersonalData([]byte(row.Trace)) {
			return clientAcceptanceFault("INVALID_OUTPUT", "CLIENT_ACCEPTANCE_TRACE_UNSANITIZED", "client acceptance trace row contains sensitive data")
		}
		callID := strings.TrimSpace(row.ToolCallID)
		if callID == "" || callIDs[callID] {
			return clientAcceptanceFault("INVALID_OUTPUT", "CLIENT_ACCEPTANCE_TOOL_CALL_ID_INVALID", "client acceptance trace rows require distinct tool-call IDs")
		}
		callIDs[callID] = true
		if ev.Status == "live" && row.Synthetic {
			return clientAcceptanceFault("INVALID_OUTPUT", "CLIENT_ACCEPTANCE_LIVE_CLAIM_INVALID", "synthetic trace rows cannot support live verification")
		}
		if isAhenkClientTool(row.Tool) {
			if !model.ValidID(row.JobID) || !model.ValidID(row.AttemptID) || !model.ValidID(row.Provider) ||
				!slices.Contains(ev.JobIDs, row.JobID) || !slices.Contains(ev.AttemptIDs, row.AttemptID) || !slices.Contains(ev.Providers, row.Provider) {
				return clientAcceptanceFault("INVALID_OUTPUT", "CLIENT_ACCEPTANCE_TRACE_UNBOUND", "each Ahenk MCP call must match a recorded job, attempt and provider")
			}
			ahenkHosts[normalizeClientHost(row.Host)] = true
			actualProviders[row.Provider] = true
			resultBound = resultBound || row.JobID == ev.ResultControl.JobID && row.AttemptID == ev.ResultControl.AttemptID
		}
	}
	if !ahenkHosts["codex"] || !ahenkHosts["claude"] {
		return clientAcceptanceFault("INVALID_OUTPUT", "HOST_NATIVE_ONLY", "each host requires an auditable Ahenk MCP delegation call")
	}
	if !expected.Valid() || model.RoutingProfile(ev.EffectiveProfile) != expected {
		return clientAcceptanceFault("INVALID_OUTPUT", "CLIENT_ACCEPTANCE_PROFILE_INVALID", "client acceptance evidence must match the selected profile")
	}
	if ev.ProfileSource != "operator" {
		return clientAcceptanceFault("INVALID_OUTPUT", "CLIENT_ACCEPTANCE_PROFILE_SOURCE_INVALID", "client acceptance evidence must match the selected profile source")
	}

	if len(actualProviders) < 2 {
		return clientAcceptanceFault("INVALID_OUTPUT", "CLIENT_ACCEPTANCE_PROVIDER_COUNT_INVALID", "client acceptance requires multiple observed execution providers")
	}
	rc := ev.ResultControl
	if !rc.Verified {
		return clientAcceptanceFault("INVALID_OUTPUT", "CLIENT_ACCEPTANCE_RESULT_UNVERIFIED", "client acceptance evidence requires verified result control")
	}
	if !resultBound {
		return clientAcceptanceFault("INVALID_OUTPUT", "CLIENT_ACCEPTANCE_RESULT_CONTROL_MISSING", "client acceptance result control must match the job and attempt from one recorded call")
	}
	if !isSHA256Hex(rc.ResultSHA256) {
		return clientAcceptanceFault("INVALID_OUTPUT", "CLIENT_ACCEPTANCE_RESULT_DIGEST_INVALID", "client acceptance result digest is invalid")
	}
	if strings.TrimSpace(rc.ArtifactHash) != "" {
		artifact := strings.TrimPrefix(strings.ToLower(strings.TrimSpace(rc.ArtifactHash)), "sha256:")
		if !isSHA256Hex(artifact) {
			return clientAcceptanceFault("INVALID_OUTPUT", "CLIENT_ACCEPTANCE_ARTIFACT_DIGEST_INVALID", "client acceptance artifact digest is invalid")
		}
	}

	switch ev.Status {
	case "synthetic":
		if ev.LiveVerified {
			return clientAcceptanceFault("INVALID_OUTPUT", "CLIENT_ACCEPTANCE_LIVE_CLAIM_INVALID", "synthetic evidence cannot claim live verification")
		}
		if strings.TrimSpace(ev.BlockedReason) != "" {
			return clientAcceptanceFault("INVALID_OUTPUT", "CLIENT_ACCEPTANCE_LIVE_CLAIM_INVALID", "synthetic evidence cannot also be blocked")
		}
	case "live":
		if !ev.LiveVerified {
			return clientAcceptanceFault("INVALID_OUTPUT", "CLIENT_ACCEPTANCE_LIVE_CLAIM_INVALID", "live evidence requires live verification")
		}
		if strings.TrimSpace(ev.BlockedReason) != "" {
			return clientAcceptanceFault("INVALID_OUTPUT", "CLIENT_ACCEPTANCE_LIVE_CLAIM_INVALID", "live evidence cannot also be blocked")
		}
	case "blocked":
		if ev.LiveVerified {
			return clientAcceptanceFault("INVALID_OUTPUT", "CLIENT_ACCEPTANCE_LIVE_CLAIM_INVALID", "blocked evidence cannot claim live verification")
		}
		if strings.TrimSpace(ev.BlockedReason) == "" {
			return clientAcceptanceFault("INVALID_OUTPUT", "CLIENT_ACCEPTANCE_BLOCKED_REASON_MISSING", "blocked evidence requires a truthful blocked reason")
		}
	default:
		return clientAcceptanceFault("INVALID_OUTPUT", "CLIENT_ACCEPTANCE_STATUS_INVALID", "client acceptance evidence status must be synthetic, live, or blocked")
	}

	return nil
}

func isSHA256Hex(s string) bool {
	if len(s) != 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return false
		}
	}
	return true
}

func validClientAcceptanceEvidence() clientAcceptanceEvidence {
	hash := model.Hash([]byte("result"))
	return clientAcceptanceEvidence{
		TraceRows: []clientAcceptanceTraceRow{
			{Host: "codex", JobID: "job-1", AttemptID: "attempt-1", Provider: "fixture-a", Tool: "delegate_task", ToolCallID: "call-codex-1", Sanitized: true, Trace: "sanitized codex trace", Synthetic: true},
			{Host: "claude", JobID: "job-2", AttemptID: "attempt-2", Provider: "fixture-b", Tool: "delegate_task", ToolCallID: "call-claude-1", Sanitized: true, Trace: "sanitized claude trace", Synthetic: true},
		},
		EffectiveProfile: "quality",
		ProfileSource:    "operator",
		Providers:        []string{"fixture-a", "fixture-b"},
		JobIDs:           []string{"job-1", "job-2"},
		AttemptIDs:       []string{"attempt-1", "attempt-2"},
		ResultControl: clientAcceptanceResultControl{
			JobID:        "job-1",
			AttemptID:    "attempt-1",
			ResultSHA256: hash,
			Verified:     true,
			ArtifactHash: hash,
		},
		Status:        "synthetic",
		LiveVerified:  false,
		BlockedReason: "",
		TraceValid:    true,
	}
}

func TestClientAcceptanceEvidenceRejectsHostNativeOnly(t *testing.T) {
	for _, profile := range model.RoutingProfiles() {
		evidence := validClientAcceptanceEvidence()
		evidence.EffectiveProfile = string(profile)
		if err := verifyClientAcceptanceEvidence(evidence, profile); err != nil {
			t.Fatalf("valid client acceptance evidence for %s rejected: %v", profile, err)
		}
	}

	for _, tc := range []struct {
		name   string
		mutate func(*clientAcceptanceEvidence)
	}{
		{"host-native only", func(ev *clientAcceptanceEvidence) {
			for i := range ev.TraceRows {
				ev.TraceRows[i].Tool = "host:native_read"
			}
		}},
		{"Claude used host-native only", func(ev *clientAcceptanceEvidence) {
			ev.TraceRows[1].Tool = "host:native_read"
		}},
		{"look-alike tool identifier", func(ev *clientAcceptanceEvidence) {
			ev.TraceRows[1].Tool = "unrelated_ahenk_mcp_tool"
		}},
		{"mismatched execution identity", func(ev *clientAcceptanceEvidence) {
			ev.ResultControl.AttemptID = "attempt-unknown"
		}},
		{"invalid profile source", func(ev *clientAcceptanceEvidence) {
			ev.ProfileSource = "environment"
		}},
		{"synthetic rows marked live", func(ev *clientAcceptanceEvidence) {
			ev.Status = "live"
			ev.LiveVerified = true
		}},
		{"profile from per-call override", func(ev *clientAcceptanceEvidence) {
			ev.ProfileSource = "request"
		}},
		{"wrong selected preset", func(ev *clientAcceptanceEvidence) {
			ev.EffectiveProfile = "price"
		}},
		{"host call lacks correlated job", func(ev *clientAcceptanceEvidence) {
			ev.TraceRows[1].JobID = "foreign-job"
		}},
		{"host calls use one provider", func(ev *clientAcceptanceEvidence) {
			ev.TraceRows[1].Provider = ev.TraceRows[0].Provider
		}},
		{"result identities belong to different calls", func(ev *clientAcceptanceEvidence) {
			ev.ResultControl.AttemptID = "attempt-2"
		}},
		{"missing codex host", func(ev *clientAcceptanceEvidence) {
			ev.TraceRows = ev.TraceRows[1:]
		}},
		{"missing claude host", func(ev *clientAcceptanceEvidence) {
			ev.TraceRows = ev.TraceRows[:1]
		}},
		{"unsanitized trace row", func(ev *clientAcceptanceEvidence) {
			ev.TraceRows[0].Sanitized = false
		}},
		{"trace contains secret", func(ev *clientAcceptanceEvidence) {
			ev.TraceRows[0].Trace = "token = \"sk-abcdefghijklmnopqrstuvwxyz123456\""
		}},
		{"missing tool-call id", func(ev *clientAcceptanceEvidence) {
			ev.TraceRows[0].ToolCallID = ""
		}},
		{"invalid trace", func(ev *clientAcceptanceEvidence) {
			ev.TraceValid = false
		}},
		{"missing effective profile", func(ev *clientAcceptanceEvidence) {
			ev.EffectiveProfile = ""
		}},
		{"missing profile source", func(ev *clientAcceptanceEvidence) {
			ev.ProfileSource = ""
		}},
		{"single provider", func(ev *clientAcceptanceEvidence) {
			ev.Providers = []string{"fixture-a"}
		}},
		{"missing job id", func(ev *clientAcceptanceEvidence) {
			ev.JobIDs = nil
		}},
		{"missing attempt id", func(ev *clientAcceptanceEvidence) {
			ev.AttemptIDs = nil
		}},
		{"unverified result", func(ev *clientAcceptanceEvidence) {
			ev.ResultControl.Verified = false
		}},
		{"invalid result digest", func(ev *clientAcceptanceEvidence) {
			ev.ResultControl.ResultSHA256 = "not-a-digest"
		}},
		{"synthetic claims live", func(ev *clientAcceptanceEvidence) {
			ev.Status = "synthetic"
			ev.LiveVerified = true
		}},
		{"synthetic claims blocked", func(ev *clientAcceptanceEvidence) {
			ev.Status = "synthetic"
			ev.BlockedReason = "upstream 429"
		}},
		{"live without verification", func(ev *clientAcceptanceEvidence) {
			ev.Status = "live"
			ev.LiveVerified = false
		}},
		{"blocked without reason", func(ev *clientAcceptanceEvidence) {
			ev.Status = "blocked"
			ev.BlockedReason = ""
		}},
		{"blocked claims live", func(ev *clientAcceptanceEvidence) {
			ev.Status = "blocked"
			ev.LiveVerified = true
			ev.BlockedReason = "upstream 429"
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			invalid := validClientAcceptanceEvidence()
			tc.mutate(&invalid)
			if err := verifyClientAcceptanceEvidence(invalid, model.RoutingProfileQuality); err == nil {
				t.Fatal("invalid client acceptance evidence was accepted")
			}
		})
	}
}
