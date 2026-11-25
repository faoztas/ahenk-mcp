// Copyright 2026 Furkan Anıl Öztaş
// SPDX-License-Identifier: Apache-2.0

package orchestration

import (
	"slices"
	"testing"
	"time"

	"github.com/faoztas/ahenk-mcp/internal/model"
	"github.com/faoztas/ahenk-mcp/internal/providers"
)

// fpRoutingFixture builds an engine with an AGY tier-2 (Argon) and tier-1
// (Flash) candidate plus healthy Gemini quota windows.
func fpRoutingFixture(now time.Time) *Engine {
	session, weekly := 60.0, 80.0
	argonWeekly := 55.0
	return &Engine{
		cfg: model.Config{
			CodexBarAccountSHA256: map[string]string{"agy": model.Hash([]byte("fixture-account")), "codex": model.Hash([]byte("fixture-account"))},
			Repositories:          map[string]model.Repo{"demo": {Providers: []string{"agy", "opencode"}}},
		},
		reg: providers.Registry{Candidates: []model.Capability{
			{AttestedAccount: "fixture-account", Provider: "agy", Model: "gemini-3.8-flash-medium", Status: "available", Speed: "fast",
				Profiles: []model.ExecutionProfile{{Name: "analysis", Status: "verified"}}},
			{AttestedAccount: "fixture-account", Provider: "agy", Model: "gemini-4-argon", Status: "available", Speed: "fast",
				Profiles: []model.ExecutionProfile{{Name: "analysis", Status: "verified"}},
				Quality:  []model.QualityEvidence{{Role: "inference", Tier: 2, Source: "LIVE"}}},
			{Provider: "opencode", Model: "free-a", Status: "available", Speed: "fast"},
		}},
		quotas: map[string][]providers.Quota{
			"agy": {
				{Family: "gemini", Bucket: "gemini-5h", Unit: "percent", AccountMatch: "MATCHED", State: "KNOWN", Remaining: &session, ObservedAt: now},
				{Family: "gemini", Bucket: "gemini-weekly", Unit: "percent", AccountMatch: "MATCHED", State: "KNOWN", Remaining: &weekly, ObservedAt: now},
				{Family: "gemini_argon", Bucket: "gemini-argon-wtu-weekly", Unit: "percent", AccountMatch: "MATCHED", State: "KNOWN", Remaining: &argonWeekly, ObservedAt: now},
			},
		},
	}
}

func fpRoutingJob(now time.Time, targetTier string) model.Job {
	return model.Job{RepoID: "demo", DeadlineAt: now.Add(time.Hour), Task: model.Task{
		Mode: "read_only", DataClass: "synthetic", TargetTier: targetTier,
		PreferredProvider: "agy", PreferredModel: "gemini-3.8-flash-medium",
	}}
}

func TestExecutionRouteUsesSameRepositoryProviderAliasAsAutoRoute(t *testing.T) {
	now := time.Now()
	e := fpRoutingFixture(now)
	e.cfg.Repositories["demo"] = model.Repo{Providers: []string{"antigravity"}}
	j := fpRoutingJob(now, "cheap")
	j.Task.PreferredProvider, j.Task.PreferredModel = "", ""

	decision, selected := e.EvaluateAutoRoute(j.RepoID, "coding", nil, j.Task, now)
	if selected == nil || selected.Provider != "agy" {
		t.Fatalf("alias did not admit the AGY capability during auto-routing: %+v", decision)
	}
	j.Task.PreferredProvider, j.Task.PreferredModel = selected.Provider, selected.Model
	if got, _ := e.route(&j, now); got.Provider != selected.Provider || got.Model != selected.Model {
		t.Fatalf("execution route chose %s/%s, want %s/%s: %+v",
			got.Provider, got.Model, selected.Provider, selected.Model, j.Route)
	}
}

func TestQualityTierBelowBestRemoved(t *testing.T) {
	now := time.Now()
	e := fpRoutingFixture(now)
	e.cfg.Repositories["demo"] = model.Repo{Providers: []string{"agy"}}
	j := fpRoutingJob(now, "cheap")
	if c, _ := e.route(&j, now); c.Model != "gemini-3.8-flash-medium" {
		t.Fatalf("cheap tier did not select the Flash model: %+v", c)
	}
	for _, d := range j.Route {
		if d.Reason == "QUALITY_TIER_BELOW_BEST" {
			t.Fatalf("rigid quality tier elimination survived: %+v", d)
		}
		if d.Model == "gemini-4-argon" && (d.PolicyEligible || d.Reason != "TARGET_TIER_CONSERVE_PREMIUM") {
			t.Fatalf("tier-2 candidate was not conserved: %+v", d)
		}
	}
	// Auto routing keeps the same explicit preference without the old lock.
	j = fpRoutingJob(now, "")
	if c, _ := e.route(&j, now); c.Model != "gemini-3.8-flash-medium" {
		t.Fatalf("auto tier overrode the explicit model preference: %+v", c)
	}
	for _, d := range j.Route {
		if d.Reason == "QUALITY_TIER_BELOW_BEST" {
			t.Fatalf("rigid quality tier elimination survived in auto mode: %+v", d)
		}
	}
}

func TestTargetTierRoutingMatrix(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct {
		tier, selected string
	}{
		{"cheap", "gemini-3.8-flash-medium"},
		{"balanced", "gemini-4-argon"},
		{"hard", "gemini-4-argon"},
		{"frontier", "gemini-4-argon"},
	} {
		e := fpRoutingFixture(now)
		e.cfg.Repositories["demo"] = model.Repo{Providers: []string{"agy"}}
		j := fpRoutingJob(now, tc.tier)
		j.Task.PreferredProvider, j.Task.PreferredModel = "", ""
		c, _ := e.route(&j, now)
		if c.Model != tc.selected {
			t.Fatalf("tier %q selected %q, want %q: %+v", tc.tier, c.Model, tc.selected, j.Route)
		}
	}
	// A "free" target rejects every paid tier candidate explicitly.
	e := fpRoutingFixture(now)
	e.reg.Candidates[2].Model = "beam-free"
	j := fpRoutingJob(now, "free")
	decision, selected := e.EvaluateAutoRoute(j.RepoID, "coding", nil, j.Task, now)
	if selected == nil || selected.Provider != "opencode" {
		t.Fatalf("free tier auto-routing did not select OpenCode: %+v", decision)
	}
	if c, _ := e.route(&j, now); c.Provider != selected.Provider || c.Model != selected.Model {
		t.Fatalf("free tier execution did not select %s/%s: %+v", selected.Provider, selected.Model, j.Route)
	}
	for _, d := range j.Route {
		if d.Provider != "opencode" && (d.PolicyEligible || d.Reason != "TARGET_TIER_MISMATCH_FREE_ONLY") {
			t.Fatalf("paid candidate was not gated by the free tier: %+v", d)
		}
	}
}

func TestHeadroomCriticalBarrierAndDegradedWarning(t *testing.T) {
	now := time.Now()
	critical, argonWeekly := 4.0, 55.0
	reset := now.Add(3 * time.Hour)
	e := fpRoutingFixture(now)
	e.cfg.Repositories["demo"] = model.Repo{Providers: []string{"agy"}}
	e.quotas["agy"] = []providers.Quota{
		{Family: "gemini", Bucket: "gemini-5h", Unit: "percent", AccountMatch: "MATCHED", State: "KNOWN", Remaining: &critical, ResetAt: &reset, ObservedAt: now},
		{Family: "gemini_argon", Bucket: "gemini-argon-wtu-weekly", Unit: "percent", AccountMatch: "MATCHED", State: "KNOWN", Remaining: &argonWeekly, ObservedAt: now},
	}
	j := fpRoutingJob(now, "")
	if c, _ := e.route(&j, now); c.Model != "gemini-4-argon" {
		t.Fatalf("critical headroom did not fall back to the next family: %+v", c)
	}
	var flash *model.Decision
	for i := range j.Route {
		if j.Route[i].Model == "gemini-3.8-flash-medium" {
			flash = &j.Route[i]
		}
	}
	if flash == nil || flash.Reason != "QUOTA_CRITICAL_HEADROOM" || flash.RetryAt == nil || !flash.RetryAt.Equal(reset) {
		t.Fatalf("critical headroom barrier missing: %+v", flash)
	}
	// Degraded headroom (10%) stays eligible but records the reserve warning.
	degraded := 10.0
	e.quotas["agy"][0].Remaining = &degraded
	e.quotas["agy"][0].State = "KNOWN"
	j = fpRoutingJob(now, "")
	if c, _ := e.route(&j, now); c.Model != "gemini-3.8-flash-medium" {
		t.Fatalf("degraded headroom blocked a healthy route: %+v", c)
	}
	for _, d := range j.Route {
		if d.Model == "gemini-3.8-flash-medium" && (!d.PolicyEligible ||
			slicesNotContains(d.WarningCodes, "HEADROOM_DEGRADED")) {
			t.Fatalf("degraded headroom warning missing: %+v", d)
		}
	}
}

func slicesNotContains(rows []string, code string) bool {
	for _, r := range rows {
		if r == code {
			return false
		}
	}
	return true
}

func TestOpenCode429FastFallbackToCheapTier(t *testing.T) {
	now := time.Now()
	e := fpRoutingFixture(now)
	j := fpRoutingJob(now, "")
	j.Task.PreferredProvider = "opencode"
	j.Task.PreferredModel = "free-a"
	limited := model.Attempt{Provider: "opencode", Model: "free-a", Code: "RATE_LIMIT", FinishedAt: &now, RetryAfterMS: 600_000}
	e.rememberOpenCodeHealth(&limited)
	j.Attempts = []model.Attempt{limited}
	c, _ := e.route(&j, now)
	if c.Provider != "agy" || c.Model != "gemini-3.8-flash-medium" {
		t.Fatalf("429 did not fall back to the cheap tier: %+v", c)
	}
	var openGate *model.Decision
	for i := range j.Route {
		if j.Route[i].Model == "free-a" {
			openGate = &j.Route[i]
		}
	}
	if openGate == nil || openGate.Reason != "COOLDOWN" {
		t.Fatalf("429 cooldown gate missing: %+v", openGate)
	}
	var selected *model.Decision
	for i := range j.Route {
		if j.Route[i].Reason == "SELECTED" {
			selected = &j.Route[i]
		}
	}
	if selected == nil || !slicesContains(selected.WarningCodes, "FALLBACK_TO_CHEAP_TIER") {
		t.Fatalf("cheap tier fallback marker missing: %+v", j.Route)
	}
}

func slicesContains(rows []string, code string) bool {
	for _, r := range rows {
		if r == code {
			return true
		}
	}
	return false
}

func TestPreferredModelPrefixNormalization(t *testing.T) {
	now := time.Now()
	e := fpRoutingFixture(now)

	// Test case 1: naked model name
	j1 := model.Job{RepoID: "demo", DeadlineAt: now.Add(time.Hour), Task: model.Task{
		Mode: "read_only", DataClass: "synthetic",
		PreferredProvider: "antigravity", PreferredModel: "gemini-4-argon",
	}}
	c1, _ := e.route(&j1, now)
	if c1.Model != "gemini-4-argon" {
		t.Fatalf("expected gemini-4-argon with naked model name, got %+v", c1)
	}

	// Test case 2: prefixed model name ("antigravity/gemini-4-argon")
	j2 := model.Job{RepoID: "demo", DeadlineAt: now.Add(time.Hour), Task: model.Task{
		Mode: "read_only", DataClass: "synthetic",
		PreferredProvider: "antigravity", PreferredModel: "antigravity/gemini-4-argon",
	}}
	c2, _ := e.route(&j2, now)
	if c2.Model != "gemini-4-argon" {
		t.Fatalf("expected gemini-4-argon with prefixed model name, got %+v", c2)
	}

	// Test case 3: prefixed with canonical provider "agy/gemini-4-argon"
	j3 := model.Job{RepoID: "demo", DeadlineAt: now.Add(time.Hour), Task: model.Task{
		Mode: "read_only", DataClass: "synthetic",
		PreferredProvider: "agy", PreferredModel: "agy/gemini-4-argon",
	}}
	c3, _ := e.route(&j3, now)
	if c3.Model != "gemini-4-argon" {
		t.Fatalf("expected gemini-4-argon with agy/ prefix, got %+v", c3)
	}

	// Test case 4 (Decisive): Under default auto-routing with no preference, gemini-4-argon wins
	// because it has Tier 2 quality evidence. Verify this baseline first:
	jBaseline := model.Job{RepoID: "demo", DeadlineAt: now.Add(time.Hour), Task: model.Task{
		Mode: "read_only", DataClass: "synthetic",
	}}
	cBaseline, _ := e.route(&jBaseline, now)
	if cBaseline.Model != "gemini-4-argon" {
		t.Fatalf("expected baseline to route to higher-scoring gemini-4-argon, got %+v", cBaseline)
	}

	// Test case 5 (Decisive): PreferredModel: "antigravity/gemini-3.8-flash-medium".
	// Despite argon having higher quality score, prefix normalization MUST match "gemini-3.8-flash-medium",
	// awarding preference bonus and selecting flash-medium over argon.
	j4 := model.Job{RepoID: "demo", DeadlineAt: now.Add(time.Hour), Task: model.Task{
		Mode: "read_only", DataClass: "synthetic",
		PreferredProvider: "antigravity", PreferredModel: "antigravity/gemini-3.8-flash-medium",
	}}
	c4, _ := e.route(&j4, now)
	if c4.Model != "gemini-3.8-flash-medium" {
		t.Fatalf("expected flash-medium to override argon via prefixed preference 'antigravity/gemini-3.8-flash-medium', got %+v", c4)
	}

	// Test case 6 (Decisive with canonical provider prefix "agy/"):
	j5 := model.Job{RepoID: "demo", DeadlineAt: now.Add(time.Hour), Task: model.Task{
		Mode: "read_only", DataClass: "synthetic",
		PreferredProvider: "agy", PreferredModel: "agy/gemini-3.8-flash-medium",
	}}
	c5, _ := e.route(&j5, now)
	if c5.Model != "gemini-3.8-flash-medium" {
		t.Fatalf("expected flash-medium to override argon via prefixed preference 'agy/gemini-3.8-flash-medium', got %+v", c5)
	}
}

func TestExecutionRouteRejectsExhaustedCodex(t *testing.T) {
	now := capacityClock
	for _, tc := range []struct {
		name, bucket      string
		auto, alternative bool
	}{
		{"A1_auto_burst", providers.BarCodexBurst, true, false},
		{"explicit_burst", providers.BarCodexBurst, false, false},
		{"explicit_weekly", providers.BarCodexWeekly, false, false},
		{"healthy_alternative", providers.BarCodexBurst, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := fpRoutingFixture(now)
			c := model.Capability{AttestedAccount: "fixture-account", Provider: "codex", Model: "gpt-6.1-sol", Status: "available", Speed: "fast"}
			if tc.alternative {
				e.reg.Candidates = append(e.reg.Candidates[:1], c)
			} else {
				e.reg.Candidates = []model.Capability{c}
			}
			e.cfg.Repositories["demo"] = model.Repo{Providers: []string{"codex", "agy"}}
			j := fpRoutingJob(now, "")
			j.Task.PreferredProvider, j.Task.PreferredModel = c.Provider, c.Model
			if tc.auto {
				j.Task.PreferredProvider, j.Task.PreferredModel = "", ""
				j.InitialRoute = &model.InitialRoute{Provider: c.Provider, Model: c.Model, AutoRouted: true}
			}
			if _, selected := e.EvaluateAutoRoute(j.RepoID, "coding", nil, j.Task, now); selected == nil || selected.Provider != "codex" {
				t.Fatal("fixture did not admit healthy Codex")
			}
			minutes := 300
			if tc.bucket == providers.BarCodexWeekly {
				minutes = weeklyMinutes
			}
			q := quotaFixture("codex", tc.bucket, "", minutes, 0, now)
			e.quotas["codex"] = []providers.Quota{q}
			d, pre := e.EvaluateAutoRoute(j.RepoID, "coding", nil, j.Task, now)
			got, _ := e.route(&j, now)
			if tc.alternative {
				if pre == nil || pre.Provider != "agy" || got.Provider != "agy" {
					t.Fatal(d, got, j.Route)
				}
			} else if pre != nil || d.Reason != "NO_ELIGIBLE_ROUTE" || got.Provider != "" {
				t.Fatal(d, got, j.Route)
			}
			if !tc.auto {
				rejected := j.Route[0]
				if rejected.Provider != "codex" || rejected.Reason != "QUOTA_EXHAUSTED" || rejected.RetryAt == nil || !rejected.RetryAt.Equal(*q.ResetAt) {
					t.Fatal(j.Route)
				}
			}
		})
	}
}

func TestCapacitySnapshotAtCircuitReset(t *testing.T) {
	now := capacityClock
	until := now.Add(time.Minute)
	e, j := capacityFixture(now)
	e.cb = NewCircuitBreaker()
	e.reg.Candidates = []model.Capability{{Provider: "codex", Model: "gpt-6.1-sol", Speed: "fast", Status: "available"}}
	e.cfg.Repositories["demo"] = model.Repo{Providers: []string{"codex"}}
	preview := func(at time.Time, want bool) {
		t.Helper()
		_, pre := e.EvaluateAutoRoute("demo", "coding", nil, j.Task, at)
		got, _, _ := e.routeWithMode(&j, at, true)
		if (pre != nil) != want || (got.Provider != "") != want {
			t.Fatal(at, pre, got)
		}
	}
	preview(now, true)
	e.cb.cooldown["codex"] = until
	preview(until.Add(-time.Nanosecond), false)
	preview(until, true)
	preview(until, true)
	got, _, probeID := e.routeWithMode(&j, until, false)
	if got.Provider != "codex" {
		t.Fatal("preview consumed probe", j.Route)
	}
	preview(until, false)
	e.cb.Release("codex", probeID)
	preview(until, true)
}

func profileFixture(now time.Time) (*Engine, model.Job) {
	e := fpRoutingFixture(now)
	// Runtime evidence is specific to the adapter, account and route.
	e.reg.Candidates = []model.Capability{
		{AttestedAccount: "fixture-account", Provider: "codex", Model: "gpt-6-astra", Mode: "cli", Status: "available", Speed: "slow", Quality: []model.QualityEvidence{{Role: "inference", Tier: 2, Source: "LIVE_DETERMINISTIC_ROLE_CALIBRATION"}}},
		{Provider: "agy", Model: "gemini-3.8-flash-medium", Mode: "headless", Status: "available", Speed: "fast", Profiles: []model.ExecutionProfile{{Name: "analysis", Status: "verified"}}, Quality: []model.QualityEvidence{{Role: "inference", Tier: 2, Source: "SCOPED_ROUTE_PREFLIGHT"}}, Fingerprint: "dfaf54852553d89ee8f38b54502deb379f9fe3f194c7f7ea5db05466e4c4bd59", AttestedAccount: "fixture-account", Cost: &model.CostEvidence{Policy: "SUBSCRIPTION_ONLY", Status: "ZERO_COST_VERIFIED", Documented: "VERIFIED", RuntimeVerified: "VERIFIED", Account: "fixture-account"}},
		{Provider: "opencode", Model: "opencode/beam-free", Speed: "slow", Mode: "tool_free", Status: "available", Quality: []model.QualityEvidence{{Role: "inference", Tier: 1, Source: "SCOPED_ROUTE_PREFLIGHT"}}, Fingerprint: "bfd1f79c2fb005246c56d0e9fb472206c22782d065872d71c26f01f3eb3a0a82", OpenCodeAccount: model.OpenCodeAnonymous, OpenCodeTransport: model.OpenCodeStateless, OpenCodeEndpoint: model.OpenCodeFreeEndpoint, Cost: &model.CostEvidence{Policy: "FREE_ONLY", Status: "ZERO_PRICE_CATALOG", Documented: "VERIFIED", RuntimeVerified: "VERIFIED", Account: model.OpenCodeAnonymous}},
	}
	e.cfg.Repositories["demo"] = model.Repo{Providers: []string{"codex", "opencode", "agy"}}
	j := fpRoutingJob(now, "")
	j.Task.PreferredProvider, j.Task.PreferredModel = "", ""
	j.Task.DeadlineSeconds = 900
	for i := 1; i < len(e.reg.Candidates); i++ {
		c := e.reg.Candidates[i]
		c.Cost.VerifiedRoute = fixtureCostRoute(c, j.Task)
	}
	return e, j
}

func fixtureCostRoute(c model.Capability, t model.Task) *model.CostRoute {
	return &model.CostRoute{Provider: c.Provider, Model: c.Model, Fingerprint: c.Fingerprint, Adapter: c.Mode, Transport: c.OpenCodeTransport, Endpoint: c.OpenCodeEndpoint, Account: c.Cost.Account, Scope: t.AttemptScope()}
}

func TestRoutingProfilesSelectDifferentEligibleModels(t *testing.T) {
	now := capacityClock
	for _, tc := range []struct {
		profile  model.RoutingProfile
		provider string
	}{{"quality", "codex"}, {"price_performance", "agy"}, {"price", "opencode"}} {
		t.Run(string(tc.profile), func(t *testing.T) {
			e, j := profileFixture(now)
			repo := e.cfg.Repositories["demo"]
			repo.DefaultRoutingProfile = tc.profile
			e.cfg.Repositories["demo"] = repo
			j.RoutingProfile = tc.profile
			for range 2 {
				d, pre := e.EvaluateAutoRoute("demo", "coding", nil, j.Task, now)
				got, _ := e.route(&j, now)
				if pre == nil || pre.Provider != tc.provider || got.Provider != tc.provider {
					t.Fatalf("profile %s preview %s dispatch %s want %s", tc.profile, d.SelectedModel, got.Provider, tc.provider)
				}
				slices.Reverse(e.reg.Candidates)
			}
		})
	}
}

func TestRoutingProfilesRespectHardConstraints(t *testing.T) {
	now := capacityClock
	for _, tc := range []struct {
		name   string
		change func(*model.Capability)
	}{
		{"catalog_only", func(c *model.Capability) { c.Cost.Status = "CATALOG_PREFLIGHT_REQUIRED" }},
		{"no_runtime", func(c *model.Capability) { c.Cost.RuntimeVerified = "UNKNOWN" }},
		{"conflicting_attestation", func(c *model.Capability) { c.AttestedAccount = "other-account" }},
		{"account_missing", func(c *model.Capability) { c.OpenCodeAccount = "" }},
		{"wrong_transport", func(c *model.Capability) { c.OpenCodeTransport = "different" }},
		{"wrong_endpoint", func(c *model.Capability) { c.OpenCodeEndpoint = "other" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, j := profileFixture(now)
			j.RoutingProfile = "price"
			j.Task.RoutingProfile = "price"
			e.reg.Candidates = e.reg.Candidates[2:]
			tc.change(&e.reg.Candidates[0])
			_, pre := e.EvaluateAutoRoute("demo", "coding", nil, j.Task, now)
			got, _ := e.route(&j, now)
			if pre != nil || got.Provider != "" || len(j.Route) != 1 || j.Route[0].Reason != "PRICE_EVIDENCE_REQUIRED" {
				t.Fatalf("unverified cost admitted or omitted rejection: %+v", j.Route)
			}
		})
	}
	e, j := profileFixture(now)
	j.RoutingProfile = "price"
	c := e.reg.Candidates[2]
	a := model.Attempt{Provider: c.Provider, Model: c.Model, OpenCodeAccount: c.OpenCodeAccount, OpenCodeTransport: c.OpenCodeTransport, OpenCodeEndpoint: c.OpenCodeEndpoint, Code: "RATE_LIMIT", FinishedAt: &now, RetryAfterMS: 60000}
	e.rememberOpenCodeHealth(&a)
	j.Attempts = []model.Attempt{a}
	if got, _ := e.route(&j, now); got.Provider != "agy" {
		t.Fatal("missing included fallback", j.Route)
	}
	e.reg.Candidates[1].Cost.RuntimeVerified = "UNKNOWN"
	if got, _ := e.route(&j, now); got.Provider != "" {
		t.Fatal("unverified fallback", j.Route)
	}
}

func TestProfileSharedEligibility(t *testing.T) {
	now := capacityClock
	for _, profile := range model.RoutingProfiles() {
		for _, gate := range []string{"quota", "reserve", "quality", "repo", "private", "deadline", "role", "category"} {
			t.Run(string(profile)+"/"+gate, func(t *testing.T) {
				e, j := profileFixture(now)
				j.RoutingProfile = profile
				j.Task.RoutingProfile = profile
				c := e.reg.Candidates[1]
				repo := e.cfg.Repositories["demo"]
				switch gate {
				case "quota":
					e.quotas["agy"] = []providers.Quota{quotaFixture("agy", "gemini-5h", "gemini", 300, 0, now)}
				case "reserve":
					e.quotas["agy"] = []providers.Quota{quotaFixture("agy", "gemini-weekly", "gemini", weeklyMinutes, 12, now)}
				case "quality":
					c.Quality[0].Tier = 1
					repo.Limits.MinimumQualityTier = 2
				case "repo":
					repo.Providers = []string{"opencode"}
				case "private":
					j.Task.DataClass = model.PrivateSourceData
				case "deadline":
					j.Task.DeadlineSeconds = 21
					j.DeadlineAt = now.Add(21 * time.Second)
				case "role":
					j.Task.TaskClass = "architecture"
				case "category":
					c.Model = "claude-opus-5-5"
				}
				e.cfg.Repositories["demo"] = repo
				e.reg.Candidates = []model.Capability{c}
				_, pre := e.EvaluateAutoRoute("demo", "coding", nil, j.Task, now)
				got, _ := e.route(&j, now)
				if pre != nil || got.Provider != "" || len(j.Route) != 1 {
					t.Fatalf("constraint %s admitted route: %+v", gate, j.Route)
				}
			})
		}
	}
}

func TestProfileTieAndExplicitPreference(t *testing.T) {
	for _, profile := range model.RoutingProfiles() {
		t.Run(string(profile), func(t *testing.T) {
			now := capacityClock
			e, j := profileFixture(now)
			a := e.reg.Candidates[2]
			b := a
			b.Model = "opencode/aaa-free"
			independent := *a.Cost
			b.Cost = &independent
			b.Cost.VerifiedRoute = fixtureCostRoute(b, j.Task)
			e.reg.Candidates = []model.Capability{a, b}
			j.Task.RoutingProfile = profile
			j.RoutingProfile = profile
			for range 2 {
				_, pre := e.EvaluateAutoRoute("demo", "coding", nil, j.Task, now)
				got, _ := e.route(&j, now)
				if pre == nil || pre.Model != b.Model || got.Model != b.Model {
					t.Fatal("unstable tie", pre, j.Route)
				}
				slices.Reverse(e.reg.Candidates)
			}
			j.Task.PreferredModel = a.Model
			if got, _ := e.route(&j, now); got.Model != a.Model {
				t.Fatal("explicit preference lost", j.Route)
			}
		})
	}
}

func TestProfilePriceUsesRecentMatchingRuntimeReceipt(t *testing.T) {
	now := capacityClock
	e, j := profileFixture(now)
	c := e.reg.Candidates[2]
	c.Model = "opencode/glm-5.3-flash-free"
	c.OpenCodeTransport = model.OpenCodeNativeReadAsk
	receipt := *c.Cost
	receipt.VerifiedRoute = nil
	c.Cost = &model.CostEvidence{Policy: "FREE_ONLY", Status: "CATALOG_PREFLIGHT_REQUIRED", Account: model.OpenCodeAnonymous}
	e.reg.Candidates = []model.Capability{c}
	j.RoutingProfile = "price"
	j.Task.ExecutionProfile = "analysis"
	zero := 0.0
	a := model.Attempt{EffectiveScope: &model.AttemptScope{Mode: j.Task.Mode, DataClass: j.Task.DataClass, ExecutionProfile: j.Task.ExecutionProfile}, Provider: c.Provider, Model: c.Model, Fingerprint: c.Fingerprint, FinishedAt: &now, OpenCodeAccount: c.OpenCodeAccount,
		OpenCodeEndpoint: c.OpenCodeEndpoint, OpenCodeTransport: c.OpenCodeTransport, Cost: &receipt,
		OpenCode: &model.OpenCodeRunEvidence{Model: c.Model, TerminalVerified: true, GuardVerified: true, ReportedCost: &zero}}
	for _, tc := range []struct {
		name   string
		change func(*model.Job)
		want   bool
	}{
		{"exact", func(*model.Job) {}, true},
		{"missing_cost", func(j *model.Job) { j.Attempts[0].OpenCode.ReportedCost = nil }, false},
		{"other_transport", func(j *model.Job) { j.Attempts[0].OpenCodeTransport = model.OpenCodeStateless }, false},
		{"other_profile", func(j *model.Job) { j.Attempts[0].EffectiveScope.ExecutionProfile = "review" }, false},
		{"analysis_parent_restricted_planner", func(j *model.Job) {
			j.Task.Decompose = true
			j.Attempts[0].EffectiveScope.ExecutionProfile = "restricted"
		}, false},
		{"old_planner", func(j *model.Job) { j.Task.Decompose = true; j.Attempts[0].EffectiveScope = nil }, false},
		{"stale", func(j *model.Job) { at := now.Add(-24 * time.Hour); j.Attempts[0].FinishedAt = &at }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ev := *a.OpenCode
			previous := model.Job{Status: model.Completed, Task: j.Task, Attempts: []model.Attempt{a}}
			scope := *a.EffectiveScope
			previous.Attempts[0].EffectiveScope = &scope
			previous.Attempts[0].OpenCode = &ev
			tc.change(&previous)
			e.jobs = map[string]model.Job{"receipt": previous}
			got, _ := e.route(&j, now)
			if (got.Provider == "opencode") != tc.want {
				t.Fatal(j.Route)
			}
		})
	}
	t.Run("exact_planner_scope", func(t *testing.T) {
		parent := j.Task
		parent.Decompose = true
		planner := planningTask(parent, false)
		scope := planner.AttemptScope()
		attempt := a
		attempt.EffectiveScope = &scope
		e.jobs = map[string]model.Job{"planner": {Status: model.Completed, Task: parent, Attempts: []model.Attempt{attempt}}}
		if got := e.profileCost(c, planner, now); got != "free" {
			t.Fatalf("restricted receipt for restricted task: %s", got)
		}
	})
}

func TestProfileEffortUsesSelectedTransport(t *testing.T) {
	e, j := profileFixture(capacityClock)
	j.Task.RoutingProfile = "quality"
	j.Task.TargetEffort = "max"
	for _, tc := range []struct{ provider, want string }{{"codex", "xhigh"}, {"agy", "medium"}, {"opencode", ""}} {
		j.Task.PreferredProvider = tc.provider
		d, c := e.EvaluateAutoRoute("demo", "coding", nil, j.Task, capacityClock)
		if c == nil || c.Provider != tc.provider || d.ResolvedEffort != tc.want {
			t.Fatalf("provider %s effort %q decision %+v", tc.provider, tc.want, d)
		}
	}
}

func TestProfilePriceCannotTransferDirectReceipt(t *testing.T) {
	e, j := profileFixture(capacityClock)
	candidates := slices.Clone(e.reg.Candidates[1:])
	for _, provider := range []string{"codex", "claude"} {
		c := model.Capability{Provider: provider, Model: "one", Mode: "cli", AttestedAccount: "fixture-account", Fingerprint: model.Hash([]byte(provider))}
		receipt := *candidates[0].Cost
		c.Cost = &receipt
		receipt.VerifiedRoute = fixtureCostRoute(c, j.Task)
		candidates = append(candidates, c)
	}
	for _, c := range candidates {
		t.Run(c.Provider, func(t *testing.T) {
			if got := e.profileCost(c, j.Task, capacityClock); got == "unknown" {
				t.Fatal("fixture lacks its positive receipt")
			}
			unbound := c
			cost := *c.Cost
			cost.VerifiedRoute = nil
			unbound.Cost = &cost
			if got := e.profileCost(unbound, j.Task, capacityClock); got != "unknown" {
				t.Fatalf("unbound receipt qualified as %s", got)
			}
			other := c
			if c.Provider == "agy" {
				other.Model = "gemini-4-argon"
			} else if c.Provider == "opencode" {
				other.Model = "opencode/another-free"
			} else {
				other.Model = "two"
			}
			if got := e.profileCost(other, j.Task, capacityClock); got != "unknown" {
				t.Errorf("another model inherited %s receipt", got)
			}
			task := j.Task
			task.ExecutionProfile = "restricted"
			c.Profiles = append(c.Profiles, model.ExecutionProfile{Name: "restricted", Status: "verified"})
			if got := e.profileCost(c, task, capacityClock); got != "unknown" {
				t.Errorf("another profile inherited %s receipt", got)
			}
		})
	}
}

func TestProfileCLIReliabilityRecovers(t *testing.T) {
	for _, provider := range []string{"codex", "claude"} {
		t.Run(provider, func(t *testing.T) {
			now := capacityClock
			failed := now.Add(-time.Minute)
			c := model.Capability{Provider: provider, Mode: "cli", Model: "test", Fingerprint: "binary"}
			a := model.Attempt{Provider: provider, Model: c.Model, Fingerprint: c.Fingerprint, Code: "EXECUTION_FAILED", FinishedAt: &failed}
			e := Engine{jobs: map[string]model.Job{"old": {Attempts: []model.Attempt{a}}}}
			if got := e.workerReliability(c, now); got != -1 {
				t.Fatal(got)
			}
			a.Code, a.FinishedAt = "", &now
			ev := &model.CLIRunEvidence{}
			if provider == "codex" {
				a.Codex = ev
			} else {
				a.Claude = ev
			}
			e.jobs["new"] = model.Job{Attempts: []model.Attempt{a}}
			if got := e.workerReliability(c, now); got != -1 {
				t.Fatalf("unverified success replaced failure: %d", got)
			}
			ev.TerminalVerified = true
			if got := e.workerReliability(c, now); got != 1 {
				t.Fatalf("verified success left reliability %d", got)
			}
		})
	}
}
