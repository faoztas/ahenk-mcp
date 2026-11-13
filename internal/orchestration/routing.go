// Copyright 2026 Furkan Anıl Öztaş
// SPDX-License-Identifier: Apache-2.0

package orchestration

import (
	"strings"
	"time"

	"github.com/faoztas/ahenk-mcp/internal/providers"
	"github.com/faoztas/ahenk-mcp/internal/store"
)

// RouteDecision contains the end-to-end multi-provider dynamic routing result.
type RouteDecision struct {
	SelectedModel  string           `json:"selected_model"`
	ResolvedEffort string           `json:"resolved_effort"`
	Reason         string           `json:"reason"`
	TaskCategory   string           `json:"task_category"`
	QuotaPressure  float64          `json:"quota_pressure"`
	Stage          OperationalStage `json:"stage"`
	Latency        time.Duration    `json:"latency"`
}

// EvaluateRouting executes the end-to-end routing pipeline evaluating operational stage,
// priority matrix fallback order, circuit breaker allowances, model admission, effort resolution,
// and durable quota state persistence.
func EvaluateRouting(db *store.Store, category string, complexity float64, barHealths map[string]float64) RouteDecision {
	start := time.Now()

	codexHealth, hasCodex := barHealths[providers.BarCodexBurst]
	if !hasCodex {
		codexHealth = 1.0
	}
	agyHealth, hasAgy := barHealths[providers.BarAgyGemini]
	if !hasAgy {
		agyHealth = 1.0
	}
	freeHealth, hasFree := barHealths[providers.BarOpenCodeFree]
	if !hasFree {
		freeHealth = 1.0
	}
	opusHealth, hasOpus := barHealths[providers.BarAgyOpus]
	if !hasOpus {
		opusHealth = 1.0
	}

	stage := DetermineOperationalStage(codexHealth, agyHealth, freeHealth)

	cb := NewCircuitBreaker()
	if codexHealth <= 0.0 {
		cb.Trip("codex", 1*time.Hour)
	}
	if agyHealth <= 0.0 {
		cb.Trip("antigravity", 1*time.Hour)
		cb.Trip("agy", 1*time.Hour)
	}
	if freeHealth <= 0.0 {
		cb.Trip("opencode", 1*time.Hour)
	}

	matrix := FallbackMatrix(category)
	var selectedModel string
	var candHealth float64 = 1.0

	for _, cand := range matrix {
		parts := strings.Split(cand, "/")
		prov := parts[0]

		h := 1.0
		switch NormalizeProvider(prov) {
		case "codex":
			h = codexHealth
		case "agy":
			h = agyHealth
		case "opencode":
			h = freeHealth
		}

		if h <= 0.0 {
			continue
		}

		if !cb.Allow(prov) {
			continue
		}

		if err := ValidateModelAdmission(category, cand, opusHealth); err != nil {
			cb.Release(prov)
			continue
		}

		selectedModel = cand
		candHealth = h
		break
	}

	if selectedModel == "" && len(matrix) > 0 {
		for _, cand := range matrix {
			parts := strings.Split(cand, "/")
			prov := parts[0]
			h := 1.0
			switch NormalizeProvider(prov) {
			case "codex":
				h = codexHealth
			case "agy":
				h = agyHealth
			case "opencode":
				h = freeHealth
			}
			if h > 0.0 {
				selectedModel = cand
				candHealth = h
				break
			}
		}
		if selectedModel == "" {
			selectedModel = matrix[0]
		}
	}

	effort, reason := ResolveEffort(category, complexity, candHealth, selectedModel)

	// If the system is in Stage 3 (Codex Exhausted failover), Stage 2 (Pressure), or Stage 4,
	// throttle effort from high to medium to preserve failover quota.
	if (stage == Stage3CodexExhausted || stage == Stage2MediumPressure || stage == Stage4AGYExhausted) && effort == EffortHigh {
		effort = EffortMedium
		reason = "QUOTA_PRESSURE_THROTTLED"
	}

	if db != nil {
		now := time.Now().UTC()
		_ = db.SaveQuotaState(store.ProviderQuotaState{
			ProviderID:           "codex",
			TierName:             "codex_pro",
			WindowDurationHours:  5.0,
			RequestsUsedInWindow: int((1.0 - codexHealth) * 180),
			MaxWindowRequests:    180,
			HealthPct:            codexHealth,
			CircuitState:         string(cb.State("codex")),
			UpdatedAt:            now,
		})
		_ = db.SaveQuotaState(store.ProviderQuotaState{
			ProviderID:           "antigravity",
			TierName:             "google_ai_pro",
			WindowDurationHours:  5.0,
			RequestsUsedInWindow: int((1.0 - agyHealth) * 100),
			MaxWindowRequests:    100,
			HealthPct:            agyHealth,
			CircuitState:         string(cb.State("antigravity")),
			UpdatedAt:            now,
		})
		_ = db.SaveQuotaState(store.ProviderQuotaState{
			ProviderID:           "opencode",
			TierName:             "opencode_free",
			WindowDurationHours:  16.6,
			RequestsUsedInWindow: int((1.0 - freeHealth) * 50),
			MaxWindowRequests:    50,
			HealthPct:            freeHealth,
			CircuitState:         string(cb.State("opencode")),
			UpdatedAt:            now,
		})
	}

	elapsed := time.Since(start)

	return RouteDecision{
		SelectedModel:  selectedModel,
		ResolvedEffort: string(effort),
		Reason:         reason,
		TaskCategory:   category,
		QuotaPressure:  1.0 - candHealth,
		Stage:          stage,
		Latency:        elapsed,
	}
}
