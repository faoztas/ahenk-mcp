// Copyright 2026 Furkan Anıl Öztaş
// SPDX-License-Identifier: Apache-2.0

package orchestration

import (
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
		cfg: model.Config{Repositories: map[string]model.Repo{"demo": {Providers: []string{"agy", "opencode"}}}},
		reg: providers.Registry{Candidates: []model.Capability{
			{Provider: "agy", Model: "gemini-3.8-flash-medium", Status: "available", Speed: "fast",
				Profiles: []model.ExecutionProfile{{Name: "analysis", Status: "verified"}}},
			{Provider: "agy", Model: "gemini-4-argon", Status: "available", Speed: "fast",
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
	j := fpRoutingJob(now, "free")
	if c, _ := e.route(&j, now); c.Provider != "" {
		t.Fatalf("free tier selected a paid route: %+v", c)
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
