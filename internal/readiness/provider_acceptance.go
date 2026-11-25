// Copyright 2026 Furkan Anıl Öztaş
// SPDX-License-Identifier: Apache-2.0

package readiness

import (
	"fmt"
	"math"
	"regexp"
	"slices"
	"strings"

	"github.com/faoztas/ahenk-mcp/internal/model"
)

const (
	EvidenceKindSynthetic = "synthetic_fixture"
	EvidenceKindLive      = "live_acceptance"

	QuotaStateUnknown   = "UNKNOWN"
	QuotaStateStale     = "STALE"
	QuotaStateKnown     = "KNOWN"
	QuotaStateExhausted = "EXHAUSTED"
)

var sha256HexLower = regexp.MustCompile(`^[a-f0-9]{64}$`)

// UsageEvidence is an optional, validated usage record.
type UsageEvidence struct {
	InputTokens  int64 `json:"input_tokens,omitempty"`
	OutputTokens int64 `json:"output_tokens,omitempty"`
	TotalTokens  int64 `json:"total_tokens,omitempty"`
}

// ProviderAcceptanceRow binds one provider adapter's exact route identity,
// role/profile scope, execution provenance, artifact/check binding, usage/quota
// truthfulness and evidence kind. Synthetic fixture rows can never claim live
// acceptance.
type ProviderAcceptanceRow struct {
	Provider      string `json:"provider"`
	Model         string `json:"model"`
	Transport     string `json:"transport"`
	Executable    string `json:"executable"`
	HostClient    string `json:"host_client"`
	WorkerAdapter string `json:"worker_adapter"`

	Role    string `json:"role"`
	Profile string `json:"profile"`

	StartupVerified  bool   `json:"startup_verified"`
	JobID            string `json:"job_id"`
	AttemptID        string `json:"attempt_id"`
	ExitCode         int    `json:"exit_code"`
	TerminalVerified bool   `json:"terminal_verified"`
	ResultVerified   bool   `json:"result_verified"`

	Check model.CheckEvidence `json:"check"`

	ArtifactSHA256  string `json:"artifact_sha256"`
	ExpectedSHA256  string `json:"expected_sha256"`
	CleanupVerified bool   `json:"cleanup_verified"`

	Usage *UsageEvidence `json:"usage,omitempty"`

	Cost *model.CostEvidence `json:"cost,omitempty"`

	QuotaState     string   `json:"quota_state"`
	QuotaRemaining *float64 `json:"quota_remaining,omitempty"`

	EvidenceKind   string `json:"evidence_kind"`
	LiveAcceptance bool   `json:"live_acceptance"`

	Account string `json:"account,omitempty"`
	Notes   string `json:"notes,omitempty"`
}

// ProviderAcceptanceMatrix aggregates acceptance rows for all four adapters.
type ProviderAcceptanceMatrix struct {
	Rows []ProviderAcceptanceRow `json:"rows"`
}

var providerAllowedRoles = map[string]map[string]bool{
	"codex":    {"code": true, "debug": true, "review": true},
	"claude":   {"code": true, "review": true, "plan": true},
	"agy":      {"code": true, "debug": true},
	"opencode": {"review": true, "research": true, "utility": true},
}

var providerAllowedTransports = map[string]map[string]bool{
	"codex":    {"cli": true},
	"claude":   {"cli": true},
	"agy":      {"headless": true},
	"opencode": {"stateless": true, "native_read_ask": true},
}

var providerAllowedExecutables = map[string]map[string]bool{
	"codex":    {"codex": true},
	"claude":   {"claude": true},
	"agy":      {"agy": true},
	"opencode": {"opencode": true},
}

var allowedProfiles = map[string]bool{
	"restricted": true, "analysis": true, "review": true, "implementation": true,
	model.TrustedLocalProfile: true,
	"price_performance":       true,
	"quality":                 true,
	"price":                   true,
}

var allProviders = []string{"codex", "claude", "agy", "opencode"}

func VerifyProviderAcceptanceRow(row ProviderAcceptanceRow) error {
	if !slices.Contains(allProviders, row.Provider) {
		return fmt.Errorf("provider %q is not one of the four exact adapters", row.Provider)
	}
	if strings.TrimSpace(row.Model) == "" {
		return fmt.Errorf("model is required")
	}
	if slices.Contains([]string{"latest", "default", "auto"}, strings.ToLower(row.Model)) || row.Model == row.Provider {
		return fmt.Errorf("model %q is an alias, not an exact model identity", row.Model)
	}
	if !providerAllowedTransports[row.Provider][row.Transport] {
		return fmt.Errorf("transport %q is not approved for provider %q", row.Transport, row.Provider)
	}
	if !providerAllowedExecutables[row.Provider][row.Executable] {
		return fmt.Errorf("executable %q is not approved for provider %q", row.Executable, row.Provider)
	}

	if strings.TrimSpace(row.HostClient) == "" || strings.TrimSpace(row.WorkerAdapter) == "" {
		return fmt.Errorf("host client and worker adapter are required")
	}
	if row.HostClient == row.WorkerAdapter {
		return fmt.Errorf("host client and worker adapter must be distinct")
	}
	if row.WorkerAdapter != row.Provider {
		return fmt.Errorf("worker adapter %q does not match provider %q", row.WorkerAdapter, row.Provider)
	}

	if !providerAllowedRoles[row.Provider][row.Role] {
		return fmt.Errorf("role %q is out of scope for provider %q", row.Role, row.Provider)
	}
	if !allowedProfiles[row.Profile] {
		return fmt.Errorf("profile %q is not approved", row.Profile)
	}

	if !row.StartupVerified {
		return fmt.Errorf("startup is not verified")
	}
	if strings.TrimSpace(row.JobID) == "" {
		return fmt.Errorf("job id is required")
	}
	if strings.TrimSpace(row.AttemptID) == "" {
		return fmt.Errorf("attempt id is required")
	}
	if row.ExitCode != 0 {
		return fmt.Errorf("worker exit code must be 0")
	}
	if !row.TerminalVerified || !row.ResultVerified {
		return fmt.Errorf("terminal/result verification is required, not just a zero exit code")
	}

	if row.Check.ID == "" {
		return fmt.Errorf("registered check is required")
	}
	status := strings.ToLower(strings.TrimSpace(row.Check.Status))
	if status != "passed" && status != "pass" && status != "ok" {
		return fmt.Errorf("check %q did not pass (status=%q)", row.Check.ID, row.Check.Status)
	}
	if row.Check.ExitStatus != 0 {
		return fmt.Errorf("check %q exit status must be 0", row.Check.ID)
	}
	if !sha256HexLower.MatchString(row.ArtifactSHA256) {
		return fmt.Errorf("artifact sha256 must be 64 lowercase hex")
	}
	if !sha256HexLower.MatchString(row.ExpectedSHA256) {
		return fmt.Errorf("expected sha256 must be 64 lowercase hex")
	}
	if row.ArtifactSHA256 != row.ExpectedSHA256 {
		return fmt.Errorf("artifact sha256 does not match expected hash")
	}
	if !row.CleanupVerified {
		return fmt.Errorf("cleanup is not verified")
	}

	if row.Usage != nil {
		if row.Usage.InputTokens < 0 || row.Usage.OutputTokens < 0 || row.Usage.TotalTokens < 0 {
			return fmt.Errorf("usage tokens must be non-negative")
		}
		if row.Usage.TotalTokens == 0 && row.Usage.InputTokens+row.Usage.OutputTokens > 0 {
			return fmt.Errorf("usage total tokens must not be zero when input/output tokens are present")
		}
		if row.Usage.InputTokens+row.Usage.OutputTokens > row.Usage.TotalTokens {
			return fmt.Errorf("usage total tokens is inconsistent")
		}
	}

	switch row.QuotaState {
	case QuotaStateUnknown, QuotaStateStale:
		if row.QuotaRemaining != nil {
			return fmt.Errorf("unknown/stale quota must not carry a numeric value")
		}
	case QuotaStateKnown, QuotaStateExhausted:
		if row.QuotaRemaining == nil {
			return fmt.Errorf("known/exhausted quota state requires a numeric remaining value")
		}
		if math.IsNaN(*row.QuotaRemaining) || math.IsInf(*row.QuotaRemaining, 0) {
			return fmt.Errorf("quota remaining must be finite")
		}
		if *row.QuotaRemaining < 0 || *row.QuotaRemaining > 100 {
			return fmt.Errorf("quota remaining must be within [0, 100]")
		}
		if row.QuotaState == QuotaStateExhausted && *row.QuotaRemaining != 0 {
			return fmt.Errorf("exhausted quota remaining must be 0")
		}
	default:
		return fmt.Errorf("quota state %q is not allowed", row.QuotaState)
	}

	if row.Cost != nil {
		if strings.TrimSpace(row.Cost.Policy) == "" {
			return fmt.Errorf("cost evidence policy is required")
		}
		if strings.TrimSpace(row.Cost.Status) == "" {
			return fmt.Errorf("cost evidence status is required")
		}
	}

	if row.EvidenceKind != EvidenceKindSynthetic && row.EvidenceKind != EvidenceKindLive {
		return fmt.Errorf("evidence kind %q is not allowed", row.EvidenceKind)
	}
	if row.EvidenceKind == EvidenceKindSynthetic && row.LiveAcceptance {
		return fmt.Errorf("synthetic fixture cannot claim live acceptance")
	}

	costPolicy, costStatus, costAccount := "", "", ""
	if row.Cost != nil {
		costPolicy = row.Cost.Policy
		costStatus = row.Cost.Status
		costAccount = row.Cost.Account
	}
	scanned := strings.Join([]string{
		row.Provider, row.Model, row.Transport, row.Executable, row.HostClient,
		row.WorkerAdapter, row.Role, row.Profile, row.JobID, row.AttemptID,
		row.Check.ID, row.Check.Test,
		row.ArtifactSHA256, row.ExpectedSHA256, row.QuotaState, row.EvidenceKind,
		row.Account, row.Notes,
		costPolicy, costStatus, costAccount,
	}, "\n")
	if strings.Contains(row.Account, "@") {
		return fmt.Errorf("row account must not contain an email address")
	}
	if model.ContainsSecret([]byte(scanned)) {
		return fmt.Errorf("row contains a raw credential or secret")
	}
	if model.ContainsPersonalData([]byte(scanned)) {
		return fmt.Errorf("row contains personal data")
	}

	return nil
}

func VerifyProviderAcceptanceMatrix(m ProviderAcceptanceMatrix) error {
	seen := map[string]bool{}
	for i, row := range m.Rows {
		if err := VerifyProviderAcceptanceRow(row); err != nil {
			return fmt.Errorf("row %d (%s): %w", i, row.Provider, err)
		}
		if seen[row.Provider] {
			return fmt.Errorf("duplicate provider row %q", row.Provider)
		}
		seen[row.Provider] = true
	}
	for _, p := range allProviders {
		if !seen[p] {
			return fmt.Errorf("missing acceptance row for provider %q", p)
		}
	}
	return nil
}
