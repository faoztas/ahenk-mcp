// Copyright 2026 Furkan Anıl Öztaş
// SPDX-License-Identifier: Apache-2.0

package orchestration

import (
	"context"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/faoztas/ahenk-mcp/internal/model"
	"github.com/faoztas/ahenk-mcp/internal/providers"
	"github.com/faoztas/ahenk-mcp/internal/store"
)

var capacityClock = time.Date(2026, 11, 10, 16, 0, 0, 0, time.UTC)

func quotaFixture(provider, bucket, family string, minutes int, left float64, now time.Time) providers.Quota {
	reset := now.Add(time.Duration(minutes) * time.Minute)
	state := "KNOWN"
	if left == 0 {
		state = "EXHAUSTED"
	}
	return providers.Quota{Provider: provider, Bucket: bucket, Family: family, WindowMinutes: minutes,
		Unit: "percent", State: state, Remaining: &left, AccountMatch: "MATCHED", ObservedAt: now, ResetAt: &reset, Confidence: "exact"}
}

func capacityFixture(now time.Time) (*Engine, model.Job) {
	q := []model.QualityEvidence{{Role: "inference", Tier: 1, Source: "SYNTHETIC_EVIDENCE"}}
	e := &Engine{
		cfg: model.Config{Repositories: map[string]model.Repo{"demo": {Providers: []string{"agy", "opencode"}}}},
		reg: providers.Registry{Candidates: []model.Capability{
			{Provider: "agy", Model: "gemini-3.8-flash-medium", Status: "available", Speed: "fast", Quality: slices.Clone(q),
				Profiles: []model.ExecutionProfile{{Name: "restricted", Status: "preflight_required"}}},
			{Provider: "opencode", Model: "opencode/glm-5.3-flash-free", Status: "available", Speed: "fast", Quality: slices.Clone(q)},
		}},
		jobs: map[string]model.Job{}, active: map[string]context.CancelFunc{}, quotas: map[string][]providers.Quota{
			"codex": {quotaFixture("codex", "primary", "", 300, 60, now), quotaFixture("codex", "secondary", "", weeklyMinutes, 70, now)},
			"agy":   {quotaFixture("agy", "gemini-5h", "gemini", 300, 80, now), quotaFixture("agy", "gemini-weekly", "gemini", weeklyMinutes, 80, now)},
		},
	}
	return e, model.Job{ID: "test-job", RepoID: "demo", Status: model.Queued, DeadlineAt: now.Add(10 * time.Minute),
		Task: model.Task{Mode: "read_only", DataClass: "synthetic", TaskClass: "utility"}}
}

func requireWarning(t *testing.T, warnings []model.CapacityWarning, code string) model.CapacityWarning {
	t.Helper()
	for _, w := range warnings {
		if w.Code == code && w.ID != "" && w.Severity != "" && w.NextAction != "" && w.Consequence != "" {
			return w
		}
	}
	t.Fatalf("missing %s in %+v", code, warnings)
	return model.CapacityWarning{}
}

func TestCapacityQualityAndReserveT01T02T03(t *testing.T) {
	now := capacityClock
	e, j := capacityFixture(now)
	e.reg.Candidates[0].Quality[0].Tier = 2
	if c, _ := e.route(&j, now); c.Provider != "agy" {
		t.Fatal("T01 selected lower evidence tier")
	}
	e.reg.Candidates[0].Quality[0].Tier = 1
	if c, _ := e.route(&j, now); c.Provider != "opencode" {
		t.Fatal("T02 did not conserve quota for equivalent candidates")
	}
	e.reg.Candidates[0].Quality[0].Tier = 2
	e.quotas["agy"][1] = quotaFixture("agy", "gemini-weekly", "gemini", weeklyMinutes, 16, now)
	if c, _ := e.route(&j, now); c.Provider != "" {
		t.Fatal("T03 silently downgraded quality")
	}
	finishWithoutRoute(&j)
	w := requireWarning(t, e.capacityWarnings(j.ID, j.Route, j.Status, now), reserveReason)
	if w.Threshold != 15 || w.Remaining == nil || *w.Remaining != 16 || w.Alternative != "NO_EQUIVALENT_OPENCODE_ROUTE" {
		t.Fatalf("reserve warning lacks evidence: %+v", w)
	}
	repo := e.cfg.Repositories[j.RepoID]
	repo.AllowAGYReserveOverride = true
	e.cfg.Repositories[j.RepoID] = repo
	j.Task.AGYReserveOverride = "Approved synthetic priority override"
	if c, _ := e.route(&j, now); c.Provider != "agy" ||
		!slices.ContainsFunc(j.Route, func(d model.Decision) bool { return d.Reason == "SELECTED" && d.ReserveOverride && d.WeeklyFloor == 0 }) {
		t.Fatal("authorized override did not preserve routing audit")
	}
}

func TestCapacityControllerWarningsT04T05T06T07T13T17(t *testing.T) {
	now := capacityClock
	e, j := capacityFixture(now)
	e.quotas["codex"][0] = quotaFixture("codex", "primary", "", 300, 15, now)
	e.route(&j, now)
	if w := requireWarning(t, e.capacityWarnings(j.ID, j.Route, j.Status, now), "CODEX_CONTROLLER_LOW"); w.Remaining == nil || *w.Remaining != 15 {
		t.Fatal("T04 low window lost its remaining balance")
	}
	e.quotas["agy"][1] = quotaFixture("agy", "gemini-weekly", "gemini", weeklyMinutes, 14, now)
	reset := now.Add(time.Hour)
	e.rememberOpenCodeHealth(&model.Attempt{Provider: "opencode", Model: e.reg.Candidates[1].Model, Code: "RATE_LIMIT", FinishedAt: &now, RetryAt: &reset})
	e.route(&j, now)
	finishWithoutRoute(&j)
	w := requireWarning(t, e.capacityWarnings(j.ID, j.Route, j.Status, now), "CODEX_ONLY_WARNING")
	if w.ResetAt == nil || !w.ResetAt.Equal(reset) {
		t.Fatal("T05 reset was lost")
	}
	again := requireWarning(t, e.capacityWarnings(j.ID, j.Route, j.Status, now.Add(time.Second)), "CODEX_ONLY_WARNING")
	if again.ID != w.ID {
		t.Fatal("stable warning duplicated on poll")
	}

	e.quotas["codex"][0] = quotaFixture("codex", "primary", "", 300, 0, now)
	warnings := e.capacityWarnings(j.ID, j.Route, j.Status, now)
	requireWarning(t, warnings, "CODEX_5H_EXHAUSTED_OPENCODE_UNAVAILABLE")
	requireWarning(t, warnings, "ALL_ELIGIBLE_CAPACITY_BLOCKED")
	if slices.ContainsFunc(warnings, func(w model.CapacityWarning) bool { return w.Code == "CODEX_ONLY_WARNING" }) {
		t.Fatal("T06/T17 claimed exhausted controller usable")
	}

	// A separate usable AGY family must remain visible when one is protected.
	other := e.reg.Candidates[0]
	other.Model = "gemini-4-argon-medium"
	e.reg.Candidates = append(e.reg.Candidates, other)
	e.quotas["agy"] = append(e.quotas["agy"], quotaFixture("agy", "gemini-argon-wtu-weekly", "gemini_argon", weeklyMinutes, 70, now))
	e.route(&j, now)
	partial := requireWarning(t, e.capacityWarnings(j.ID, j.Route, j.Status, now), "CODEX_5H_EXHAUSTED_OPENCODE_UNAVAILABLE")
	if partial.AGYState != "PREFLIGHT_REQUIRED" {
		t.Fatal("healthy AGY family was hidden by another family's reserve")
	}
	e.reg.Candidates = e.reg.Candidates[:2]
	e.route(&j, now)

	e.quotas["codex"] = e.quotas["codex"][1:]
	for _, at := range []time.Time{now, now.Add(16 * time.Minute)} {
		warnings = e.capacityWarnings(j.ID, j.Route, j.Status, at)
		requireWarning(t, warnings, "CAPACITY_UNVERIFIED")
		if slices.ContainsFunc(warnings, func(w model.CapacityWarning) bool {
			return w.Code == "CODEX_ONLY_WARNING" || w.Code == "ALL_ELIGIBLE_CAPACITY_BLOCKED"
		}) {
			t.Fatal("T07/T13 inferred unavailable or available capacity")
		}
		c := e.capacitySnapshot(at, "demo")
		if c.Codex[0].State != "UNKNOWN" || c.Codex[0].Remaining != nil {
			t.Fatal("missing five-hour window fabricated a balance")
		}
	}
}

func TestCapacityAGYFamilyResetAndPacingT08T09T15(t *testing.T) {
	now := capacityClock
	e, j := capacityFixture(now)
	base := e.reg.Candidates[0]
	for _, id := range []string{"claude-sonnet-5-5-medium", "gemini-4-argon-medium"} {
		c := base
		c.Model = id
		e.reg.Candidates = append(e.reg.Candidates, c)
	}
	j.Task.PreferredProvider = "agy"
	e.quotas["agy"][1] = quotaFixture("agy", "gemini-weekly", "gemini", weeklyMinutes, 0, now)
	e.quotas["agy"] = append(e.quotas["agy"], quotaFixture("agy", "3p-weekly", "claude_gpt", weeklyMinutes, 60, now),
		quotaFixture("agy", "gemini-argon-wtu-weekly", "gemini_argon", weeklyMinutes, 0, now))
	j.Task.PreferredModel = "gemini-4-argon-medium"
	if c, _ := e.route(&j, now); c.Model != "claude-sonnet-5-5-medium" {
		t.Fatal("T08/T09 mixed family quota")
	}
	for _, q := range e.capacitySnapshot(now, "demo").AGY {
		if q.Family == "gemini_argon" && (q.WindowMinutes != weeklyMinutes || q.State != "EXHAUSTED") {
			t.Fatalf("T09 fabricated Argon capacity: %+v", q)
		}
	}
	at := now.Add(time.Minute)
	e.quotas["agy"][1].ResetAt = &at
	if c, _ := e.route(&j, at); c.Model == "gemini-4-argon-medium" {
		t.Fatal("T15 reset unrelated bucket")
	}
	e.quotas["agy"][1] = quotaFixture("agy", "gemini-weekly", "gemini", weeklyMinutes, 80, at)
	j.Task.PreferredModel = "gemini-3.8-flash-medium"
	if c, _ := e.route(&j, at); c.Model != j.Task.PreferredModel {
		t.Fatal("T15 fresh reset did not reopen family")
	}

	q := e.quotas["agy"][1]
	older := q
	older.ObservedAt = at.Add(-10 * time.Minute)
	left := 82.0
	older.Remaining = &left
	e.agyHistory = []providers.Quota{older, q}
	if r := e.agyReserve(j, "gemini", at, time.Minute, e.quotas["codex"][1]); r.reason != "AGY_WEEKLY_PACING" {
		t.Fatalf("weekly burn projection did not pace discretionary work: %+v", r)
	}
	j.Task.Priority = "high"
	if r := e.agyReserve(j, "gemini", at, time.Minute, e.quotas["codex"][1]); r.reason != "" {
		t.Fatal("pacing blocked high-priority work above floor")
	}
}

func TestCapacityOpenCodeScopeT10T11(t *testing.T) {
	now := capacityClock
	e, j := capacityFixture(now)
	second := e.reg.Candidates[1]
	second.Model = "opencode/another-free"
	e.reg.Candidates = append(e.reg.Candidates, second)
	j.Task.PreferredProvider = "opencode"
	a := model.Attempt{Provider: "opencode", Model: e.reg.Candidates[1].Model, Code: "RATE_LIMIT", FinishedAt: &now, RetryAfterMS: 60000}
	e.rememberOpenCodeHealth(&a)
	if c, _ := e.route(&j, now); c.Model != second.Model {
		t.Fatal("T10 model gate widened")
	}
	w := requireWarning(t, e.capacityWarnings(j.ID, j.Route, j.Status, now), "OPENCODE_FREE_UNAVAILABLE")
	if w.Scope != "model" || w.Model != a.Model || w.Remaining != nil {
		t.Fatal("T10 fabricated pool or balance")
	}
	a.ProviderRateLimitScope = "pool"
	e.rememberOpenCodeHealth(&a)
	if c, _ := e.route(&j, now); c.Provider != "agy" {
		t.Fatal("T11 bypassed asserted pool gate")
	}
	w = requireWarning(t, e.capacityWarnings(j.ID, j.Route, j.Status, now), "OPENCODE_FREE_UNAVAILABLE")
	if w.Scope != "pool" || w.Model != "" {
		t.Fatal("T11 did not report asserted pool")
	}
	if c, _ := e.route(&j, now.Add(time.Minute)); c.Provider != "opencode" {
		t.Fatal("pool remained blocked at reset")
	}
}

func TestCapacityReviewAndCostT12T16(t *testing.T) {
	now := capacityClock
	e, j := capacityFixture(now)
	j.Parent = "batch"
	e.jobs[j.Parent] = model.Job{Distinct: true, Children: []string{"producer", j.ID}}
	e.jobs["producer"] = model.Job{Attempts: []model.Attempt{{Provider: "agy-headless"}}}
	j.QualityRole = "review"
	for i := range e.reg.Candidates {
		e.reg.Candidates[i].Quality = []model.QualityEvidence{{Role: "review", Tier: 2}}
	}
	if c, _ := e.route(&j, now); c.Provider != "opencode" {
		t.Fatal("T12 replaced independent reviewer")
	}
	e.reg.Candidates[1].Cost = &model.CostEvidence{Policy: "PAID"}
	if c, _ := e.route(&j, now); c.Provider != "" {
		t.Fatal("T16 accepted paid route or self-review")
	}
	e.reg.Candidates[1].Cost = nil
	e.reg.Candidates[1].Mode = "tool_free"
	if c, _ := e.route(&j, now); c.Provider != "" {
		t.Fatal("T16 accepted live route without price admission")
	}
}

func TestCapacityReservationAtomicT14(t *testing.T) {
	now := capacityClock
	e, j := capacityFixture(now)
	e.reg.Candidates = e.reg.Candidates[:1]
	var wg sync.WaitGroup
	decisions := make(chan string, 2)
	for _, id := range []string{"one", "two"} {
		wg.Go(func() {
			e.mu.Lock()
			defer e.mu.Unlock()
			job := j
			job.ID = id
			c, _ := e.route(&job, now)
			if c.Provider == "" {
				decisions <- job.Route[0].Reason
				return
			}
			job.Attempts = []model.Attempt{{Provider: c.Provider, Model: c.Model, StartedAt: now, AGYWeeklyBuffer: 2}}
			e.jobs[id], e.active[id] = job, func() {}
			decisions <- "SELECTED"
		})
	}
	wg.Wait()
	close(decisions)
	var got []string
	for d := range decisions {
		got = append(got, d)
	}
	slices.Sort(got)
	if !slices.Equal(got, []string{"AGY_FAMILY_IN_FLIGHT", "SELECTED"}) {
		t.Fatal(got)
	}
}

func TestCapacityLegacySQLiteT18(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	now := capacityClock
	e, j := capacityFixture(now)
	j.PolicyDigest = model.Digest(e.cfg.Repositories[j.RepoID])
	j.Status = model.Completed
	j.Task.TaskClass = "review"
	if _, err := db.Insert([]model.Job{j}, ""); err != nil {
		t.Fatal(err)
	}
	restored, err := New(e.cfg, db, e.reg)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	old, err := restored.Get(j.ID)
	if err != nil || old.Status != model.Completed || jobQualityRole(old) != "inference" {
		t.Fatalf("legacy row changed: %+v %v", old, err)
	}
}

func TestCapacityRequiresScopedEvidenceForExplicitRoles(t *testing.T) {
	e, j := capacityFixture(capacityClock)
	e.cfg.EnableFixture = true
	r := e.cfg.Repositories[j.RepoID]
	r.Providers = append(r.Providers, "fixture-a")
	e.cfg.Repositories[j.RepoID] = r
	e.reg.Candidates = append(e.reg.Candidates, model.Capability{Provider: "fixture-a", Model: "synthetic", Mode: "fixture", Status: "available"})
	j.QualityRole = "architecture"
	if c, _ := e.route(&j, capacityClock); c.Provider != "" {
		t.Fatal("inference or a fixture was promoted to architecture")
	}
	finishWithoutRoute(&j)
	if j.Reason != "QUALITY_EVIDENCE_REQUIRED" {
		t.Fatalf("quality rejection was obscured: %+v", j.Route)
	}

	j.QualityRole = "inference"
	e.reg.Candidates[0].Status, e.reg.Candidates[1].Status = "unverified", "unverified"
	if c, _ := e.route(&j, capacityClock); c.Provider != "" {
		t.Fatal("fixture silently replaced real work")
	}
	j.Task.Strategy = "offline_privacy_first"
	if c, _ := e.route(&j, capacityClock); c.Provider != "fixture-a" {
		t.Fatal("explicit offline fixture was rejected")
	}
}

func TestCapacityReservationCoversAttemptsOverlappingObservation(t *testing.T) {
	now := capacityClock
	e, j := capacityFixture(now)
	e.quotas["agy"][1] = quotaFixture("agy", "gemini-weekly", "gemini", weeklyMinutes, 19, now.Add(-time.Minute))
	finish := now.Add(-time.Second)
	e.jobs["previous"] = model.Job{Attempts: []model.Attempt{{Provider: "agy", Model: "gemini-3.8-flash-medium",
		StartedAt: now.Add(-2 * time.Minute), FinishedAt: &finish, AGYWeeklyBuffer: 3}}}
	r := e.agyReserve(j, "gemini", now, time.Minute, e.quotas["codex"][1])
	if r.reason != reserveReason || r.floor != 20 {
		t.Fatalf("unaccounted overlapping spend: %+v", r)
	}
}
