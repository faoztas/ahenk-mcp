package orchestration

import (
	"strings"
	"testing"

	"github.com/faoztas/ahenk-mcp/internal/providers"
)

func providerFamily(modelID string) string {
	parts := strings.Split(modelID, "/")
	if len(parts) < 2 {
		return modelID
	}
	prefix := parts[0]
	model := parts[1]
	if prefix == "antigravity" {
		if strings.HasPrefix(model, "gemini") {
			return "gemini"
		}
		if strings.HasPrefix(model, "claude") {
			return "claude"
		}
	}
	return prefix
}

func TestPriorityMatrix10UniqueEngines(t *testing.T) {
	categories := []string{
		"coding", "debugging", "code_review",
		"architecture", "planning", "research", "agent_orchestration",
	}
	for _, cat := range categories {
		matrix := FallbackMatrix(cat)
		if len(matrix) != 10 {
			t.Fatalf("category %s: expected 10 engines, got %d", cat, len(matrix))
		}
		seen := map[string]bool{}
		for i, engine := range matrix {
			if seen[engine] {
				t.Fatalf("category %s: duplicate engine %s at position %d", cat, engine, i)
			}
			seen[engine] = true
		}
		// First fallback must be from a different provider family than primary
		primary := matrix[0]
		fallback0 := matrix[1]
		primaryFamily := providerFamily(primary)
		fallback0Family := providerFamily(fallback0)
		if primaryFamily == fallback0Family {
			t.Fatalf("category %s: primary %s and fallback0 %s share same provider family %s", cat, primary, fallback0, primaryFamily)
		}
		if cat != "code_review" && strings.Split(primary, "/")[0] == strings.Split(fallback0, "/")[0] {
			t.Fatalf("category %s: primary %s and fallback0 %s share same provider prefix", cat, primary, fallback0)
		}
	}
	if err := ValidatePriorityMatrix(); err != nil {
		t.Fatalf("priority matrix validation failed: %v", err)
	}
}

func TestCodexExhaustedFallbackLeap(t *testing.T) {
	// P0-2: When Codex quota is exhausted (health == 0.0), router must skip all codex models
	// and leap to next healthy provider (e.g. antigravity/gemini-4-argon for coding and debugging).
	for _, cat := range []string{"coding", "debugging"} {
		decision := EvaluateRouting(nil, cat, 0.9, map[string]float64{
			providers.BarCodexBurst: 0.0,
			providers.BarAgyGemini:  0.9785,
		})
		if strings.HasPrefix(decision.SelectedModel, "codex/") {
			t.Fatalf("category %s: selected exhausted codex model: %s", cat, decision.SelectedModel)
		}
		if decision.SelectedModel != "antigravity/gemini-4-argon" {
			t.Fatalf("category %s: expected antigravity/gemini-4-argon, got %s", cat, decision.SelectedModel)
		}
	}
}
