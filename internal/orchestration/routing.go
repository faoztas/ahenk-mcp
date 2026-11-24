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
	CapacityState  string           `json:"capacity_state,omitempty"`
	CostClass      string           `json:"cost_class,omitempty"`
	ProfileScore   float64          `json:"profile_score,omitempty"`
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

	if short, ok := barHealths[providers.BarAgyOpus5h]; ok {
		opusHealth = min(opusHealth, short)
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

		if !cb.Eligible(prov) {
			continue
		}

		if err := ValidateModelAdmission(category, cand, opusHealth); err != nil {
			continue
		}

		selectedModel = cand
		candHealth = h
		break
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

	if selectedModel == "" {
		return RouteDecision{TaskCategory: category, Stage: stage, Reason: "NO_ELIGIBLE_ROUTE", QuotaPressure: 1, Latency: time.Since(start)}
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

func normalizedRoutingCategory(category string) string {
	category = strings.ToLower(strings.TrimSpace(category))
	if _, ok := priorityMatrices[category]; !ok {
		return "coding"
	}
	return category
}

func legacyModelName(provider, name string) string {
	if provider == "opencode" {
		return strings.TrimPrefix(name, "opencode/")
	}
	return name
}

// EvaluateAutoRoute selects the best eligible candidate from the priority matrix for the
// given repository, task category, optional complexity, and task constraints using live
// quota freshness, circuit breakers, cooldown gates, policy admissibility, and deadline budgets.
func (e *Engine) EvaluateAutoRoute(repoID, category string, complexity *float64, task model.Task, now time.Time) (RouteDecision, *model.Capability) {
	profile := task.RoutingProfile
	if e != nil && profile == "" {
		if repo, ok := e.Repo(repoID); ok {
			profile, _, _ = e.defaultRoutingProfile(repoID, repo, "")
		}
	}
	return e.evaluateAutoRoute(repoID, category, complexity, task, now, profile, routingRole(task))
}

func (e *Engine) evaluateAutoRoute(repoID, category string, complexity *float64, task model.Task, now time.Time, profile model.RoutingProfile, role string) (RouteDecision, *model.Capability) {
	start := time.Now()
	if now.IsZero() {
		now = time.Now().UTC()
	}

	normalizedCat := normalizedRoutingCategory(category)
	task.Category = normalizedCat
	matrix := FallbackMatrix(normalizedCat)

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

	opusHealth := e.opusAdmissionHealth(now)
	argonHealth := e.healthPercentLocked(providers.BarAgyArgon, now)
	hasArgon := e.hasFreshQuotaLocked(providers.BarAgyArgon, now)
	freeHealth := e.healthPercentLocked(providers.BarOpenCodeFree, now)

	agyStageHealth := min(agyGeminiHealth, argonHealth)
	if (agyGeminiHealth <= 0.0 && hasArgon && argonHealth > 0.0) ||
		(argonHealth <= 0.0 && hasAgyGemini && agyGeminiHealth > 0.0) {
		agyStageHealth = max(0.01, agyStageHealth)
	}

	stage := DetermineOperationalStage(codexHealth, agyStageHealth, freeHealth)

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

	var policy *model.Repo
	if hasRepo {
		policy = &repo
	}
	task.Category = normalizedCat
	if complexity != nil {
		task.HasComplexity, task.Complexity = true, *complexity
	}
	job := model.Job{RepoID: repoID, Task: task, DeadlineAt: deadline, QualityRole: role, RoutingProfile: profile}
	if profile != "" && task.Decompose {
		job.Task = planningTask(task, task.Mode == "patch" && repo.Limits.RequireIndependentReview || task.Risk == "critical")
		job.QualityRole = "planning"
	}
	options, _ := e.eligibleCandidates(&job, now, policy)
	if profile != "" {
		ranked := e.rankProfile(&job, options, now)
		e.mu.Unlock()
		decision := RouteDecision{TaskCategory: normalizedCat, Stage: stage, Reason: "NO_ELIGIBLE_ROUTE", Latency: time.Since(start)}
		if len(ranked) == 0 {
			return decision, nil
		}
		best := ranked[0]
		decision.SelectedModel = qualifiedModel(best.capability)
		decision.ResolvedEffort = best.decision.ResolvedEffort
		decision.Reason = best.decision.RankingReason
		decision.CapacityState, decision.CostClass, decision.ProfileScore = best.decision.CapacityState, best.decision.CostClass, best.decision.ProfileScore
		if best.decision.QuotaRemaining != nil {
			decision.QuotaPressure = 1 - *best.decision.QuotaRemaining/100
		}
		return decision, &best.capability
	}
	findMatchingCandidateLocked := func(normProv, candModel string) (model.Capability, bool) {
		for _, option := range options {
			c := option.capability
			if NormalizeProvider(c.Provider) == normProv && legacyModelName(normProv, c.Model) == candModel {
				return c, true
			}
		}
		for _, option := range options {
			c := option.capability
			if NormalizeProvider(c.Provider) == normProv && candModel != "" && strings.HasPrefix(legacyModelName(normProv, c.Model), candModel+"-") {
				return c, true
			}
		}
		for _, option := range options {
			c := option.capability
			if NormalizeProvider(c.Provider) == normProv && c.Model == "" {
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

		matched, ok := findMatchingCandidateLocked(normCandProv, candModel)
		if !ok {
			continue
		}

		h := 1.0
		sameAccount := e.quotaAccountMatches(matched)
		if normCandProv == "opencode" || sameAccount {
			h = candHealthFor(normCandProv, matched.Model)
		}
		// A provider-only capability takes its model from the matrix.
		if matched.Model == "" {
			health := 1.0
			if sameAccount {
				health = opusHealth
			}
			if ValidateModelAdmission(normalizedCat, cand, health) != nil {
				continue
			}
		}

		selectedCap = matched
		hasSelected = true
		selectedModel = cand
		if name := legacyModelName(normCandProv, matched.Model); name != "" && name != candModel {
			selectedModel = candProv + "/" + name
		}
		candHealth = h
		break
	}

	e.mu.Unlock()

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
