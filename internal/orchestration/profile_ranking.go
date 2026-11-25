// Copyright 2026 Furkan Anıl Öztaş
// SPDX-License-Identifier: Apache-2.0

package orchestration

import (
	"cmp"
	"slices"
	"strings"
	"time"

	"github.com/faoztas/ahenk-mcp/internal/model"
	"github.com/faoztas/ahenk-mcp/internal/providers"
)

func qualifiedModel(c model.Capability) string {
	if strings.HasPrefix(c.Model, c.Provider+"/") {
		return c.Model
	}
	return c.Provider + "/" + c.Model
}

// Profile scoring uses relative policy weights. Cost classes describe
// incremental spend only; unknown prices have no monetary estimate.
const (
	profileQualityWeight     = .40
	profileEvidenceWeight    = .06
	profileReliabilityWeight = .14
	profileHeadroomWeight    = .14
	profileLatencyWeight     = .16
	profileCostWeight        = .10
)

type profileRank struct {
	option                                   candidate
	preference, evidence, reliability, price int
	headroom                                 float64
	latency                                  time.Duration
}

func explicitPreference(task model.Task, c model.Capability) int {
	score := 0
	if task.PreferredProvider != "" && normalizeProvider(task.PreferredProvider) == normalizeProvider(c.Provider) {
		score += 2
	}
	if task.PreferredModel != "" {
		provider, name, qualified := strings.Cut(task.PreferredModel, "/")
		if task.PreferredModel == c.Model || qualified && normalizeProvider(provider) == normalizeProvider(c.Provider) && name == strings.TrimPrefix(c.Model, c.Provider+"/") {
			score++
		}
	}
	return score
}

func verifiedRoleSource(source string) bool {
	return source == "LIVE" || strings.HasPrefix(source, "LIVE_")
}

func (e *Engine) rankProfile(j *model.Job, options []candidate, now time.Time) []candidate {
	category := normalizedRoutingCategory(j.Task.Category)
	ranks := make([]profileRank, 0, len(options))
	for _, o := range options {
		c := o.capability
		d := &o.decision
		class := e.profileCost(c, j.Task, now)
		d.CostClass = class
		modelID := qualifiedModel(c)
		if normalizeProvider(c.Provider) == "agy" {
			modelID = "antigravity/" + c.Model
		}
		if j.RoutingProfile == model.RoutingProfilePrice && class == "unknown" {
			d.PolicyEligible = false
			d.Reason = "PRICE_EVIDENCE_REQUIRED"
			j.Route = append(j.Route, *d)
			continue
		}

		r := profileRank{preference: explicitPreference(j.Task, c), reliability: e.workerReliability(c, now), headroom: .5, latency: o.estimate}
		if c.AuditLatencyMS > 0 && c.AuditValidUntil != nil && now.Before(*c.AuditValidUntil) {
			r.latency = time.Duration(c.AuditLatencyMS) * time.Millisecond
		}
		// A capability can carry preflight and verified results at the same
		// tier. Catalog order must not hide the verified result for this role.
		for _, q := range c.Quality {
			if q.Role == d.QualityRole && q.Tier == d.QualityTier && verifiedRoleSource(q.Source) {
				d.QualitySource = q.Source
				break
			}
		}
		if verifiedRoleSource(d.QualitySource) {
			r.evidence = 1
		}
		if d.QuotaRemaining != nil {
			r.headroom = *d.QuotaRemaining / 100
		}
		if d.HeadroomState != "" && c.Provider != "opencode" {
			r.headroom = d.HeadroomPercent / 100
		}
		switch class {
		case "free":
			r.price = 2
		case "included":
			r.price = 1
		}
		d.RankingReason = "PROFILE_" + strings.ToUpper(string(j.RoutingProfile))
		if r.preference > 0 {
			d.RankingReason = "EXPLICIT_TARGET_PREFERENCE"
		}
		if j.RoutingProfile == model.RoutingProfilePricePerformance {
			speed := min(1.0, float64(time.Minute)/float64(max(time.Millisecond, r.latency)))
			d.ProfileScore = profileQualityWeight*float64(d.QualityTier)/2 +
				profileEvidenceWeight*float64(r.evidence) +
				profileReliabilityWeight*float64(r.reliability+1)/2 +
				profileHeadroomWeight*r.headroom +
				profileLatencyWeight*speed +
				profileCostWeight*float64(r.price)/2
			d.ProfileScore = max(0, min(1, d.ProfileScore))
		}
		effort := j.Task.TargetEffort
		if effort == "" {
			complexity := j.Task.Complexity
			if complexity == 0 && !j.Task.HasComplexity {
				complexity = .9
			}
			level, _ := ResolveEffort(category, complexity, r.headroom, modelID)
			effort = string(level)
		}
		effortTask := j.Task
		effortTask.RoutingProfile, effortTask.TargetEffort = j.RoutingProfile, effort
		d.ResolvedEffort = providers.AdapterEffort(c, effortTask)
		r.option = o
		ranks = append(ranks, r)
	}
	slices.SortFunc(ranks, func(a, b profileRank) int {
		if diff := cmp.Compare(b.preference, a.preference); diff != 0 {
			return diff
		}
		ad, bd := a.option.decision, b.option.decision
		var policy int
		switch j.RoutingProfile {
		case model.RoutingProfilePrice:
			policy = cmp.Compare(b.price, a.price)
		case model.RoutingProfileQuality:
			policy = cmp.Or(cmp.Compare(b.evidence, a.evidence), cmp.Compare(bd.QualityTier, ad.QualityTier))
		case model.RoutingProfilePricePerformance:
			policy = cmp.Compare(bd.ProfileScore, ad.ProfileScore)
		}
		ac, bc := a.option.capability, b.option.capability
		return cmp.Or(policy, cmp.Compare(b.reliability, a.reliability), cmp.Compare(b.headroom, a.headroom), cmp.Compare(a.latency, b.latency),
			strings.Compare(ac.Provider, bc.Provider), strings.Compare(ac.Model, bc.Model),
			strings.Compare(ac.OpenCodeTransport, bc.OpenCodeTransport), strings.Compare(ac.OpenCodeEndpoint, bc.OpenCodeEndpoint),
			strings.Compare(ac.Fingerprint, bc.Fingerprint), strings.Compare(ac.OpenCodeAccount, bc.OpenCodeAccount),
			strings.Compare(ac.AttestedAccountSHA256, bc.AttestedAccountSHA256), strings.Compare(ac.AttestedAccount, bc.AttestedAccount))
	})
	ranked := options[:0]
	for _, r := range ranks {
		ranked = append(ranked, r.option)
	}
	return ranked
}

// Reuse a recent terminal zero-cost receipt only for the exact same route and
// task scope. Dispatch still performs its current catalog and endpoint checks.
func (e *Engine) profileCost(c model.Capability, t model.Task, now time.Time) string {
	cost := c.Cost
	if cost == nil {
		return "unknown"
	}
	if providers.VerifiedIncludedRoute(c, t) {
		return "included"
	}
	if !providers.FreeRouteIdentity(c) || cost.Policy != "FREE_ONLY" || cost.Account != model.OpenCodeAnonymous {
		return "unknown"
	}
	if providers.MatchesCostReceipt(c, t) && verifiedFreeReceipt(cost) && t.Mode == "read_only" && t.DataClass == "synthetic" && !t.NativeChild && t.FollowUp == "" &&
		slices.Contains([]string{"", "restricted", "analysis"}, t.ExecutionProfile) {
		return "free"
	}
	if cost.Status != "CATALOG_PREFLIGHT_REQUIRED" && !verifiedFreeReceipt(cost) {
		return "unknown"
	}
	scope := t.AttemptScope()
	for _, job := range e.jobs {
		if job.Status != model.Completed {
			continue
		}
		for _, a := range job.Attempts {
			if a.EffectiveScope == nil || *a.EffectiveScope != scope || a.Code != "" ||
				a.FinishedAt == nil || a.FinishedAt.After(now) || now.Sub(*a.FinishedAt) >= 24*time.Hour {
				continue
			}
			if a.Provider != c.Provider || a.Model != c.Model || a.Fingerprint != c.Fingerprint ||
				a.OpenCodeAccount != c.OpenCodeAccount || model.OpenCodeTransportID(a.OpenCodeTransport) != model.OpenCodeTransportID(c.OpenCodeTransport) || a.OpenCodeEndpoint != c.OpenCodeEndpoint || !verifiedFreeReceipt(a.Cost) {
				continue
			}
			if ev := a.OpenCode; ev != nil && ev.TerminalVerified && ev.GuardVerified && ev.Model == c.Model && ev.ReportedCost != nil && *ev.ReportedCost == 0 {
				return "free"
			}
		}
	}
	return "unknown"
}

func verifiedFreeReceipt(cost *model.CostEvidence) bool {
	return cost != nil && cost.Account == model.OpenCodeAnonymous && cost.Policy == "FREE_ONLY" && cost.Status == "ZERO_PRICE_CATALOG" && cost.Documented == "VERIFIED" && cost.RuntimeVerified == "VERIFIED"
}
