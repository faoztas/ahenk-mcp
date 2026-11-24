// Copyright 2026 Furkan Anıl Öztaş
// SPDX-License-Identifier: Apache-2.0

package readiness

import (
	"strings"
	"testing"

	"github.com/faoztas/ahenk-mcp/internal/model"
)

func validRow(provider string) ProviderAcceptanceRow {
	hash := strings.Repeat("a", 64)
	role := map[string]string{
		"codex":    "code",
		"claude":   "review",
		"agy":      "code",
		"opencode": "review",
	}[provider]
	host := map[string]string{
		"codex":    "codex-cli",
		"claude":   "claude-cli",
		"agy":      "agy-host",
		"opencode": "opencode-host",
	}[provider]
	transport := map[string]string{
		"codex":    "cli",
		"claude":   "cli",
		"agy":      "headless",
		"opencode": "stateless",
	}[provider]
	var quotaRemaining *float64
	quotaState := QuotaStateKnown
	if provider == "opencode" {
		quotaState = QuotaStateUnknown
	} else {
		v := 42.5
		quotaRemaining = &v
	}
	_ = quotaState
	return ProviderAcceptanceRow{
		Provider:         provider,
		Model:            provider + "-model-1",
		Transport:        transport,
		Executable:       provider,
		HostClient:       host,
		WorkerAdapter:    provider,
		Role:             role,
		Profile:          "restricted",
		StartupVerified:  true,
		JobID:            "job-" + provider,
		AttemptID:        "attempt-1",
		ExitCode:         0,
		TerminalVerified: true,
		ResultVerified:   true,
		Check: model.CheckEvidence{
			ID:         "TestProvider",
			Test:       "TestProvider",
			Status:     "passed",
			ExitStatus: 0,
		},
		ArtifactSHA256:  hash,
		ExpectedSHA256:  hash,
		CleanupVerified: true,
		Usage: &UsageEvidence{
			InputTokens:  10,
			OutputTokens: 5,
			TotalTokens:  15,
		},
		QuotaState:     quotaState,
		QuotaRemaining: quotaRemaining,
		EvidenceKind:   EvidenceKindSynthetic,
		LiveAcceptance: false,
	}
}

func TestProviderAcceptanceRequiresBoundWorkerAndCheck(t *testing.T) {
	// Valid synthetic rows for all four adapters.
	for _, p := range []string{"codex", "claude", "agy", "opencode"} {
		row := validRow(p)
		if err := VerifyProviderAcceptanceRow(row); err != nil {
			t.Fatalf("valid row %s rejected: %v", p, err)
		}
	}

	// Valid matrix.
	rows := make([]ProviderAcceptanceRow, 0, 4)
	for _, p := range []string{"codex", "claude", "agy", "opencode"} {
		rows = append(rows, validRow(p))
	}
	if err := VerifyProviderAcceptanceMatrix(ProviderAcceptanceMatrix{Rows: rows}); err != nil {
		t.Fatalf("valid matrix rejected: %v", err)
	}

	// Negative cases.
	cases := []struct {
		name   string
		mutate func(*ProviderAcceptanceRow)
	}{
		{"wrong binary", func(r *ProviderAcceptanceRow) { r.Executable = "wrong" }},
		{"wrong transport", func(r *ProviderAcceptanceRow) { r.Transport = "http" }},
		{"alias model", func(r *ProviderAcceptanceRow) { r.Model = "latest" }},
		{"host confused with worker", func(r *ProviderAcceptanceRow) { r.HostClient = r.WorkerAdapter }},
		{"worker not provider", func(r *ProviderAcceptanceRow) { r.WorkerAdapter = "other" }},
		{"out of scope role", func(r *ProviderAcceptanceRow) { r.Role = "admin" }},
		{"invalid profile", func(r *ProviderAcceptanceRow) { r.Profile = "bogus" }},
		{"exit 0 missing terminal", func(r *ProviderAcceptanceRow) {
			r.ExitCode = 0
			r.TerminalVerified = false
			r.ResultVerified = false
		}},
		{"corrupt hash", func(r *ProviderAcceptanceRow) {
			r.ArtifactSHA256 = strings.Repeat("b", 64)
		}},
		{"mismatched hash", func(r *ProviderAcceptanceRow) {
			r.ExpectedSHA256 = strings.Repeat("b", 64)
		}},
		{"invalid hex hash", func(r *ProviderAcceptanceRow) {
			r.ArtifactSHA256 = strings.ToUpper(strings.Repeat("a", 64))
		}},
		{"failed check", func(r *ProviderAcceptanceRow) {
			r.Check.Status = "failed"
		}},
		{"missing check", func(r *ProviderAcceptanceRow) {
			r.Check.ID = ""
		}},
		{"check nonzero exit", func(r *ProviderAcceptanceRow) {
			r.Check.ExitStatus = 1
		}},
		{"unverified cleanup", func(r *ProviderAcceptanceRow) {
			r.CleanupVerified = false
		}},
		{"fabricated quota zero instead of unknown", func(r *ProviderAcceptanceRow) {
			r.QuotaState = QuotaStateUnknown
			v := 0.0
			r.QuotaRemaining = &v
		}},
		{"synthetic claims live", func(r *ProviderAcceptanceRow) {
			r.EvidenceKind = EvidenceKindSynthetic
			r.LiveAcceptance = true
		}},
		{"secret leakage", func(r *ProviderAcceptanceRow) {
			r.Notes = "api_key=\"sk-abcdefghijklmnopqrstuvwxyz0123456789\""
		}},
		{"nonzero worker exit code", func(r *ProviderAcceptanceRow) { r.ExitCode = 1 }},
		{"exact account email leakage", func(r *ProviderAcceptanceRow) { r.Account = "user@example.com" }},
		{"personal data in notes", func(r *ProviderAcceptanceRow) { r.Notes = "personal_data" }},
		{"invalid quota state", func(r *ProviderAcceptanceRow) { r.QuotaState = "BOGUS" }},
		{"out-of-range quota remaining", func(r *ProviderAcceptanceRow) {
			v := 150.0
			r.QuotaRemaining = &v
		}},
		{"exhausted nonzero quota", func(r *ProviderAcceptanceRow) {
			r.QuotaState = QuotaStateExhausted
			v := 5.0
			r.QuotaRemaining = &v
		}},
		{"invalid cost evidence", func(r *ProviderAcceptanceRow) {
			r.Cost = &model.CostEvidence{Policy: "", Status: ""}
		}},
		{"usage total zero with positive parts", func(r *ProviderAcceptanceRow) {
			r.Usage.TotalTokens = 0
			r.Usage.InputTokens = 10
			r.Usage.OutputTokens = 5
		}},
		{"usage negative tokens", func(r *ProviderAcceptanceRow) {
			r.Usage.InputTokens = -1
		}},
		{"usage input+output exceeds total", func(r *ProviderAcceptanceRow) {
			r.Usage.InputTokens = 10
			r.Usage.OutputTokens = 5
			r.Usage.TotalTokens = 12
		}},
		{"opencode disallowed role", func(r *ProviderAcceptanceRow) {
			*r = validRow("opencode")
			r.Role = "code"
		}},
		{"agy disallowed role", func(r *ProviderAcceptanceRow) {
			*r = validRow("agy")
			r.Role = "review"
		}},
		{"missing startup", func(r *ProviderAcceptanceRow) { r.StartupVerified = false }},
		{"missing job id", func(r *ProviderAcceptanceRow) { r.JobID = "" }},
		{"missing attempt id", func(r *ProviderAcceptanceRow) { r.AttemptID = "" }},
		{"unknown evidence kind", func(r *ProviderAcceptanceRow) { r.EvidenceKind = "other" }},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			row := validRow("codex")
			tc.mutate(&row)
			if err := VerifyProviderAcceptanceRow(row); err == nil {
				t.Fatalf("expected error for %s, got nil", tc.name)
			}
		})
	}

	// Matrix missing provider.
	incomplete := make([]ProviderAcceptanceRow, 0, 3)
	for _, p := range []string{"codex", "claude", "agy"} {
		incomplete = append(incomplete, validRow(p))
	}
	if err := VerifyProviderAcceptanceMatrix(ProviderAcceptanceMatrix{Rows: incomplete}); err == nil {
		t.Fatal("expected error for incomplete matrix")
	}
	// duplicate provider
	dup := append([]ProviderAcceptanceRow{}, validRow("codex"), validRow("codex"))
	if err := VerifyProviderAcceptanceMatrix(ProviderAcceptanceMatrix{Rows: dup}); err == nil {
		t.Fatal("expected error for duplicate provider")
	}
}
