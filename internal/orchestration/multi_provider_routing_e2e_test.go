// Copyright 2026 Furkan Anıl Öztaş
// SPDX-License-Identifier: Apache-2.0

package orchestration_test

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/faoztas/ahenk-mcp/internal/orchestration"
	"github.com/faoztas/ahenk-mcp/internal/providers"
	"github.com/faoztas/ahenk-mcp/internal/store"
)

func TestEndToEndMultiProviderRoutingScenarios(t *testing.T) {
	// Full test validating transitions across Scenarios A, B, C, D
	// and verifying zero data loss and sub-5ms routing overhead.
	dir := filepath.Join(t.TempDir(), "state")
	db, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	// 1. Nominal scenario -> Sol selected at high effort
	startNominal := time.Now()
	decision := orchestration.EvaluateRouting(db, "coding", 0.9, map[string]float64{
		providers.BarCodexBurst: 1.0,
		providers.BarAgyGemini:  1.0,
	})
	elapsedNominal := time.Since(startNominal)
	if elapsedNominal > 5*time.Millisecond {
		t.Errorf("routing decision took %v, expected <= 5ms", elapsedNominal)
	}
	if decision.SelectedModel != "codex/gpt-6.1-sol" || decision.ResolvedEffort != "high" {
		t.Errorf("Nominal failed: %+v", decision)
	}

	// 2. Scenario A (Codex down) -> Argon selected at medium effort
	decisionA := orchestration.EvaluateRouting(db, "coding", 0.9, map[string]float64{
		providers.BarCodexBurst: 0.0,
		providers.BarAgyGemini:  0.8,
	})
	if decisionA.SelectedModel != "antigravity/gemini-4-argon" || decisionA.ResolvedEffort != "medium" {
		t.Errorf("Scenario A failed: %+v", decisionA)
	}

	// 3. Scenario B (AGY down) -> Architecture fails over from Opus to Astra/Sol
	decisionB := orchestration.EvaluateRouting(db, "architecture", 0.9, map[string]float64{
		providers.BarCodexBurst: 1.0,
		providers.BarAgyGemini:  0.0,
		providers.BarAgyOpus:    0.0,
	})
	if decisionB.SelectedModel != "codex/gpt-6-astra" {
		t.Errorf("Scenario B failed: expected codex/gpt-6-astra, got %+v", decisionB)
	}

	// 4. Scenario C (Dual cloud outage) -> Crouton selected
	decisionC := orchestration.EvaluateRouting(db, "coding", 0.9, map[string]float64{
		providers.BarCodexBurst: 0.0,
		providers.BarAgyGemini:  0.0,
	})
	if decisionC.SelectedModel != "opencode/crouton-alpha-free" {
		t.Errorf("Scenario C failed: %+v", decisionC)
	}

	// 5. Scenario D (OpenCode 429 bypass) -> Stage6FreeTier429
	decisionD := orchestration.EvaluateRouting(db, "coding", 0.9, map[string]float64{
		providers.BarCodexBurst:   0.8,
		providers.BarAgyGemini:    0.8,
		providers.BarOpenCodeFree: 0.0,
	})
	if decisionD.Stage != orchestration.Stage6FreeTier429 {
		t.Errorf("Scenario D failed: expected Stage6FreeTier429, got %v", decisionD.Stage)
	}

	// Verify quota states were recorded in SQLite store
	states, err := db.GetQuotaStates()
	if err != nil || len(states) == 0 {
		t.Errorf("expected quota states persisted in store, err: %v, count: %d", err, len(states))
	}
}
