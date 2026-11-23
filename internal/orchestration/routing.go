// Copyright 2026 Furkan Anıl Öztaş
// SPDX-License-Identifier: Apache-2.0

package orchestration

import (
	"slices"
	"strings"
	"time"

	"github.com/faoztas/ahenk-mcp/internal/model"
	"github.com/faoztas/ahenk-mcp/internal/providers"
	"github.com/faoztas/ahenk-mcp/internal/store"
)

func repoAllowsProvider(repo model.Repo, provider string) bool {
	if repo.PrivateSource != nil {
		return slices.Contains(repo.Providers, provider)
	}
	provider = normalizeProvider(provider)
	return slices.ContainsFunc(repo.Providers, func(allowed string) bool {
		return normalizeProvider(allowed) == provider
	})
}

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

// EvaluateAutoRoute selects the best eligible candidate from the priority matrix for the
// given repository, task category, optional complexity, and task constraints using live
// quota freshness, circuit breakers, cooldown gates, policy admissibility, and deadline budgets.
func (e *Engine) EvaluateAutoRoute(repoID, category string, complexity *float64, task model.Task, now time.Time) (RouteDecision, *model.Capability) {
	start := time.Now()
	if now.IsZero() {
		now = time.Now().UTC()
	}

	normalizedCat := strings.ToLower(strings.TrimSpace(category))
	matrix := FallbackMatrix(normalizedCat)
	if len(matrix) == 0 {
		normalizedCat = "coding"
		matrix = FallbackMatrix(normalizedCat)
	}

	effComplexity := 0.9
	if normalizedCat == "research" {
		effComplexity = 0.2
	}
	if complexity != nil {
		effComplexity = *complexity
	}

	if e == nil {
		return RouteDecision{
			TaskCategory:  normalizedCat,
			Reason:        "ENGINE_UNAVAILABLE",
			QuotaPressure: 1.0,
			Latency:       time.Since(start),
		}, nil
	}

	deadline := now.Add(task.Deadline())
	role := routingRole(task)
	targetTier := effectiveTargetTier(task)

	e.mu.Lock()
	var repo model.Repo
	var hasRepo bool
	if repoID != "" {
		repo, hasRepo = e.cfg.Repositories[repoID]
		if !hasRepo {
			e.mu.Unlock()
			return RouteDecision{
				TaskCategory:  normalizedCat,
				Reason:        "REPO_NOT_REGISTERED",
				QuotaPressure: 1.0,
				Latency:       time.Since(start),
			}, nil
		}
	}

	codexBurst := e.healthPercentLocked(providers.BarCodexBurst, now)
	codexWeekly := e.healthPercentLocked(providers.BarCodexWeekly, now)
	codexHealth := min(codexBurst, codexWeekly)

	agyGemini5h := min(e.healthPercentLocked(providers.BarAgyGemini, now), e.healthPercentLocked("gemini-5h", now))
	agyGeminiWeekly := e.healthPercentLocked("gemini-weekly", now)
	agyGeminiHealth := min(agyGemini5h, agyGeminiWeekly)
	hasAgyGemini := e.hasFreshQuotaLocked(providers.BarAgyGemini, now) ||
		e.hasFreshQuotaLocked("gemini-5h", now) ||
		e.hasFreshQuotaLocked("gemini-weekly", now)

	opusHealth := e.healthPercentLocked(providers.BarAgyOpus, now)
	argonHealth := e.healthPercentLocked(providers.BarAgyArgon, now)
	hasArgon := e.hasFreshQuotaLocked(providers.BarAgyArgon, now)
	freeHealth := e.healthPercentLocked(providers.BarOpenCodeFree, now)

	agyStageHealth := min(agyGeminiHealth, argonHealth)
	if (agyGeminiHealth <= 0.0 && hasArgon && argonHealth > 0.0) ||
		(argonHealth <= 0.0 && hasAgyGemini && agyGeminiHealth > 0.0) {
		agyStageHealth = max(0.01, agyStageHealth)
	}

	stage := DetermineOperationalStage(codexHealth, agyStageHealth, freeHealth)

	liveRepo := e.cfg.EnableFixture && hasRepo && (repoAllowsProvider(repo, "agy") || repoAllowsProvider(repo, "opencode"))

	candHealthFor := func(normProv, modelName string) float64 {
		switch normProv {
		case "codex":
			return codexHealth
		case "opencode":
			return freeHealth
		case "agy":
			switch providers.AGYModelFamily(modelName) {
			case "gemini_argon":
				return argonHealth
			case "claude_gpt":
				return opusHealth
			case "gemini":
				return agyGeminiHealth
			default:
				return min(agyGeminiHealth, argonHealth, opusHealth)
			}
		default:
			return 1.0
		}
	}

	isCandidateEligibleLocked := func(c model.Capability) bool {
		normCProv := NormalizeProvider(c.Provider)
		if hasRepo && !repoAllowsProvider(repo, c.Provider) {
			return false
		}
		if c.Status != "available" {
			return false
		}
		if task.DataClass == model.PrivateSourceData && (!hasRepo || !repo.PrivateSource.Allows(c, task, now)) {
			return false
		}
		if !workerCostAllowed(c) {
			return false
		}
		quality := e.routeQuality(c, role, now)
		if quality.Tier == 0 || (hasRepo && quality.Tier < repo.Limits.MinimumQualityTier) {
			return false
		}
		if !providers.AllowsNativeChild(c, task) || !providers.AllowsContinuation(c, task) || !providers.AuditRouteAllows(c, task, now) {
			return false
		}
		if task.ExecutionProfile == model.TrustedLocalProfile {
			if !hasRepo || repo.TrustedLocal == nil || !providers.AllowsTrustedLocal(c) ||
				(repo.TrustedLocal.Provider != "" && NormalizeProvider(repo.TrustedLocal.Provider) != normCProv) {
				return false
			}
		}
		if task.ExecutionProfile == "implementation" && task.DataClass != model.PrivateSourceData {
			if c.Mode != "fixture" && !((normCProv == "codex" || normCProv == "claude") && c.Status == "available") {
				return false
			}
		}
		if normCProv == "agy" && !providers.AllowsAGYProfile(c, task) {
			return false
		}
		if normCProv == "opencode" && task.ExecutionProfile != model.TrustedLocalProfile &&
			(task.DataClass != "synthetic" && task.DataClass != model.PrivateSourceData || task.Mode != "read_only") {
			return false
		}
		if liveRepo && c.Mode == "fixture" && task.Strategy != "offline_privacy_first" {
			return false
		}
		if c.Mode == "fixture" && task.DataClass != "synthetic" && task.DataClass != model.PrivateSourceData {
			return false
		}
		if task.Strategy == "offline_privacy_first" && c.Mode != "fixture" {
			return false
		}
		if e.cb != nil && !e.cb.Eligible(c.Provider) {
			return false
		}
		if normCProv == "opencode" && c.Mode != "fixture" {
			if scope, _ := e.openCodeGate(c, now); scope != "" {
				return false
			}
		} else if e.cooldown[providers.Family(c.Provider)].After(now) {
			return false
		}

		if targetTierViolation(targetTier, providers.FPTier(normCProv, c.Model), quality.Tier) != "" {
			return false
		}

		estimate := providers.Estimate(c, task)
		if _, fits := providers.Budget(now, deadline, task, estimate); !fits {
			return false
		}
		return true
	}

	findMatchingCandidateLocked := func(normProv, candModel string) (model.Capability, bool) {
		for _, c := range e.reg.Candidates {
			if NormalizeProvider(c.Provider) == normProv && c.Model == candModel && isCandidateEligibleLocked(c) {
				return c, true
			}
		}
		for _, c := range e.reg.Candidates {
			if NormalizeProvider(c.Provider) == normProv && candModel != "" && strings.HasPrefix(c.Model, candModel+"-") && isCandidateEligibleLocked(c) {
				return c, true
			}
		}
		for _, c := range e.reg.Candidates {
			if NormalizeProvider(c.Provider) == normProv && c.Model == "" && isCandidateEligibleLocked(c) {
				return c, true
			}
		}
		return model.Capability{}, false
	}

	var selectedModel string
	var selectedCap model.Capability
	var hasSelected bool
	var candHealth = 1.0

	for _, cand := range matrix {
		parts := strings.SplitN(cand, "/", 2)
		candProv := parts[0]
		candModel := ""
		if len(parts) == 2 {
			candModel = parts[1]
		}
		normCandProv := NormalizeProvider(candProv)

		h := candHealthFor(normCandProv, candModel)
		if h <= 0.0 {
			continue
		}
		if err := ValidateModelAdmission(normalizedCat, cand, opusHealth); err != nil {
			continue
		}
		matched, ok := findMatchingCandidateLocked(normCandProv, candModel)
		if !ok {
			continue
		}

		selectedCap = matched
		hasSelected = true
		selectedModel = cand
		if matched.Model != "" && matched.Model != candModel {
			selectedModel = candProv + "/" + matched.Model
		}
		candHealth = h
		break
	}

	db := e.db
	cb := e.cb
	e.mu.Unlock()

	if db != nil {
		codexCircuit := string(CircuitClosed)
		agyCircuit := string(CircuitClosed)
		openCodeCircuit := string(CircuitClosed)
		if cb != nil {
			codexCircuit = string(cb.State("codex"))
			agyCircuit = string(cb.State("antigravity"))
			openCodeCircuit = string(cb.State("opencode"))
		}
		_ = db.SaveQuotaState(store.ProviderQuotaState{
			ProviderID:           "codex",
			TierName:             "codex_pro",
			WindowDurationHours:  5.0,
			RequestsUsedInWindow: int((1.0 - codexHealth) * 180),
			MaxWindowRequests:    180,
			HealthPct:            codexHealth,
			CircuitState:         codexCircuit,
			UpdatedAt:            now,
		})
		_ = db.SaveQuotaState(store.ProviderQuotaState{
			ProviderID:           "antigravity",
			TierName:             "google_ai_pro",
			WindowDurationHours:  5.0,
			RequestsUsedInWindow: int((1.0 - agyStageHealth) * 100),
			MaxWindowRequests:    100,
			HealthPct:            agyStageHealth,
			CircuitState:         agyCircuit,
			UpdatedAt:            now,
		})
		_ = db.SaveQuotaState(store.ProviderQuotaState{
			ProviderID:           "opencode",
			TierName:             "opencode_free",
			WindowDurationHours:  16.6,
			RequestsUsedInWindow: int((1.0 - freeHealth) * 50),
			MaxWindowRequests:    50,
			HealthPct:            freeHealth,
			CircuitState:         openCodeCircuit,
			UpdatedAt:            now,
		})
	}

	if !hasSelected {
		return RouteDecision{
			TaskCategory:  normalizedCat,
			Reason:        "NO_ELIGIBLE_ROUTE",
			QuotaPressure: 1.0,
			Stage:         stage,
			Latency:       time.Since(start),
		}, nil
	}

	var effort EffortLevel
	var reason string
	if task.TargetEffort != "" {
		effort = EffortLevel(task.TargetEffort)
		reason = "EXPLICIT_TARGET_EFFORT"
	} else {
		effort, reason = ResolveEffort(normalizedCat, effComplexity, candHealth, selectedModel)
		if (stage == Stage3CodexExhausted || stage == Stage2MediumPressure || stage == Stage4AGYExhausted) && effort == EffortHigh {
			effort = EffortMedium
			reason = "QUOTA_PRESSURE_THROTTLED"
		}
	}

	return RouteDecision{
		SelectedModel:  selectedModel,
		ResolvedEffort: string(effort),
		Reason:         reason,
		TaskCategory:   normalizedCat,
		QuotaPressure:  1.0 - candHealth,
		Stage:          stage,
		Latency:        time.Since(start),
	}, &selectedCap
}
