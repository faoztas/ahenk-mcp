package orchestration

import (
	"errors"
	"fmt"
)

// priorityMatrices defines the deterministic 10-unique model engine priority matrix
// for all 7 standard task categories in Ahenk MCP.
var priorityMatrices = map[string][]string{
	"coding": {
		"codex/gpt-6.1-sol",
		"antigravity/gemini-4-argon",
		"opencode/crouton-alpha-free",
		"opencode/beam-free",
		"codex/gpt-6.1-terra",
		"antigravity/gemini-3.8-flash",
		"opencode/deepseek-v4.1-flash-free",
		"codex/gpt-6-astra",
		"opencode/iceman-free",
		"opencode/minimax-m3.1-flash-free",
	},
	"debugging": {
		"codex/gpt-6.1-terra",
		"antigravity/gemini-4-argon",
		"codex/gpt-6.1-sol",
		"opencode/crouton-alpha-free",
		"opencode/beam-free",
		"opencode/deepseek-v4.1-flash-free",
		"opencode/big-pickle",
		"antigravity/gemini-3.8-flash",
		"codex/gpt-6-astra",
		"opencode/iceman-free",
	},
	"code_review": {
		"antigravity/gemini-3.8-flash",
		"antigravity/claude-opus-5-5",
		"opencode/deepseek-v4.1-flash-free",
		"codex/gpt-6.1-terra",
		"antigravity/gemini-4-argon",
		"opencode/crouton-alpha-free",
		"codex/gpt-6.1-sol",
		"codex/gpt-6-astra",
		"opencode/beam-free",
		"opencode/big-pickle",
	},
	"architecture": {
		"antigravity/claude-opus-5-5",
		"codex/gpt-6-astra",
		"antigravity/gemini-4-argon",
		"codex/gpt-6.1-sol",
		"antigravity/gemini-3.8-flash",
		"opencode/crouton-alpha-free",
		"opencode/beam-free",
		"codex/gpt-6.1-terra",
		"opencode/deepseek-v4.1-flash-free",
		"opencode/big-pickle",
	},
	"planning": {
		"antigravity/gemini-4-argon",
		"codex/gpt-6.1-terra",
		"antigravity/gemini-3.8-flash",
		"opencode/big-pickle",
		"opencode/crouton-alpha-free",
		"opencode/beam-free",
		"opencode/deepseek-v4.1-flash-free",
		"codex/gpt-6.1-sol",
		"codex/gpt-6-astra",
		"opencode/iceman-free",
	},
	"research": {
		"codex/gpt-6-luna",
		"opencode/beam-free",
		"antigravity/gemini-3.8-flash",
		"opencode/crouton-alpha-free",
		"codex/gpt-6.1-terra",
		"opencode/deepseek-v4.1-flash-free",
		"antigravity/gemini-4-argon",
		"opencode/iceman-free",
		"opencode/minimax-m3.1-flash-free",
		"opencode/longcat-2.5-free",
	},
	"agent_orchestration": {
		"antigravity/gemini-4-argon",
		"codex/gpt-6-astra",
		"antigravity/claude-opus-5-5",
		"antigravity/gemini-3.8-flash",
		"opencode/crouton-alpha-free",
		"opencode/beam-free",
		"codex/gpt-6.1-terra",
		"opencode/deepseek-v4.1-flash-free",
		"codex/gpt-6.1-sol",
		"opencode/iceman-free",
	},
}

// FallbackMatrix returns a copy of the ordered fallback list of 10 model engines for category.
func FallbackMatrix(category string) []string {
	engines, ok := priorityMatrices[category]
	if !ok {
		return nil
	}
	out := make([]string, len(engines))
	copy(out, engines)
	return out
}

// ValidatePriorityMatrix validates that all 7 required categories exist, each contains
// exactly 10 distinct engines, and no duplicate engines exist within a category.
func ValidatePriorityMatrix() error {
	requiredCategories := []string{
		"coding", "debugging", "code_review",
		"architecture", "planning", "research", "agent_orchestration",
	}
	for _, cat := range requiredCategories {
		engines, ok := priorityMatrices[cat]
		if !ok {
			return fmt.Errorf("missing required category: %s", cat)
		}
		if len(engines) != 10 {
			return fmt.Errorf("category %s has %d engines, expected 10", cat, len(engines))
		}
		seen := make(map[string]bool, len(engines))
		for i, engine := range engines {
			if engine == "" {
				return fmt.Errorf("category %s has empty engine at index %d", cat, i)
			}
			if seen[engine] {
				return fmt.Errorf("category %s has duplicate engine: %s", cat, engine)
			}
			seen[engine] = true
		}
	}
	return nil
}

// ValidateModelAdmission verifies whether a candidate model can be admitted for a given task category,
// enforcing Claude Opus scarcity preservation and Luna strategic repositioning.
func ValidateModelAdmission(category string, modelID string, opusHealth float64) error {
	if modelID == "antigravity/claude-opus-5-5" {
		allowed := category == "architecture" || category == "code_review" || category == "agent_orchestration"
		if !allowed {
			return errors.New("OPUS_SCARCITY_GATE_TASK_DENIED")
		}
		if opusHealth <= 0.20 {
			return errors.New("OPUS_RESERVE_DEPLETED")
		}
	}
	if modelID == "codex/gpt-6-luna" && (category == "coding" || category == "debugging") {
		return errors.New("LUNA_CODING_DENIED")
	}
	return nil
}
