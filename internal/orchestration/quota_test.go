// Copyright 2026 Furkan Anıl Öztaş
// SPDX-License-Identifier: Apache-2.0

package orchestration

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/faoztas/ahenk-mcp/internal/model"
	"github.com/faoztas/ahenk-mcp/internal/providers"
	"github.com/faoztas/ahenk-mcp/internal/store"
)

func TestQuotaRoutingKeepsFamiliesAndFallbackSeparate(t *testing.T) {
	now := time.Now()
	zero, spare, session, controller := 0.0, 75.0, 28.0, 98.0
	e := Engine{
		cfg: model.Config{Repositories: map[string]model.Repo{"demo": {Providers: []string{"agy", "opencode"}}}},
		reg: providers.Registry{Candidates: []model.Capability{
			{Provider: "agy", Model: "gemini-3.8-flash-medium", Status: "available", Speed: "fast",
				Profiles: []model.ExecutionProfile{{Name: "analysis", Status: "verified"}}},
			{Provider: "agy", Model: "gemini-4-argon", Status: "available", Speed: "fast",
				Profiles: []model.ExecutionProfile{{Name: "analysis", Status: "verified"}}},
			{Provider: "agy", Model: "claude-sonnet", Status: "available", Speed: "fast",
				Profiles: []model.ExecutionProfile{{Name: "analysis", Status: "verified"}}},
			{Provider: "opencode", Model: "free", Status: "available", Speed: "fast"},
		}},
		quotas: map[string][]providers.Quota{
			"agy": {
				{Family: "gemini", Bucket: "gemini-5h", Unit: "percent", AccountMatch: "MATCHED", State: "EXHAUSTED", Remaining: &zero, ObservedAt: now},
				{Family: "gemini_argon", Bucket: "gemini-argon-wtu-weekly", Unit: "percent", AccountMatch: "MATCHED", State: "KNOWN", Remaining: &spare, ObservedAt: now},
				{Family: "claude_gpt", Bucket: "3p-weekly", Unit: "percent", AccountMatch: "MATCHED", State: "EXHAUSTED", Remaining: &zero, ObservedAt: now},
			},
			"codex": {
				{Bucket: "primary", WindowMinutes: 300, Unit: "percent", AccountMatch: "MATCHED", State: "KNOWN", Remaining: &session, ObservedAt: now},
				{Bucket: "secondary", WindowMinutes: 10080, Unit: "percent", AccountMatch: "MATCHED", State: "KNOWN", Remaining: &controller, ObservedAt: now},
			},
		},
	}
	j := model.Job{RepoID: "demo", DeadlineAt: now.Add(time.Hour), Task: model.Task{
		Mode: "read_only", DataClass: "synthetic", PreferredProvider: "agy", PreferredModel: "gemini-3.8-flash-medium",
	}}
	c, _ := e.route(&j, now)
	if c.Model != "gemini-4-argon" || j.Route[0].Reason != "QUOTA_EXHAUSTED" ||
		j.Route[0].ControllerRemaining == nil || *j.Route[0].ControllerRemaining != controller ||
		j.Route[1].QuotaBucket != "3p-weekly" || j.Route[1].Reason != "QUOTA_EXHAUSTED" {
		t.Fatalf("wrong selection or audit: %+v, %+v", c, j.Route)
	}
	e.quotas["agy"][0].AccountMatch = "MISMATCH"
	e.quotas["codex"][1].AccountMatch = "MISMATCH"
	c, _ = e.route(&j, now)
	if c.Model != "gemini-3.8-flash-medium" || j.Route[0].ControllerRemaining != nil {
		t.Fatalf("mismatched account affected routing: %+v, %+v", c, j.Route)
	}
	e.quotas["agy"][0].AccountMatch = "MATCHED"
	c, _ = e.route(&j, now.Add(16*time.Minute))
	if c.Model != "gemini-3.8-flash-medium" {
		t.Fatalf("stale quota blocked a route: %+v", c)
	}
}

func TestOpenCodeRetryAfterSurvivesSchedulerRestart(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	now := time.Now()
	j := model.Job{ID: "limited", RepoID: "demo", Status: model.Failed, CreatedAt: now, Attempts: []model.Attempt{
		{Number: 1, Provider: "opencode", Model: "free", Code: "RATE_LIMIT", FinishedAt: &now, RetryAfterMS: 3_600_000, HealthScope: "pool"},
	}}
	if _, err := db.Insert([]model.Job{j}, ""); err != nil {
		t.Fatal(err)
	}
	cfg := model.Config{Repositories: map[string]model.Repo{"demo": {Providers: []string{"opencode", "fixture-b"}}}}
	reg := providers.Registry{Candidates: []model.Capability{
		{Provider: "opencode", Model: "free", Status: "available", Speed: "fast"},
		{Provider: "opencode", Model: "alternate", Status: "available", Speed: "fast"},
	}}
	e, err := New(cfg, db, reg)
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	quota := e.Quota("opencode")
	if q := quotaForModel(t, quota, "free"); q.State != "UNKNOWN" || q.Remaining != nil || q.Reason != "RATE_LIMIT" || q.HealthScope != "model" ||
		q.ResetAt == nil || !q.ResetAt.Equal(now.Add(time.Hour)) {
		t.Fatalf("rate limit was not reported as a gate on unknown quota: %+v", q)
	}
	if q := quotaForModel(t, quota, "alternate"); q.ResetAt != nil {
		t.Fatalf("legacy inferred pool gate affected alternate: %+v", q)
	}
	jobs, err := db.All()
	if err != nil || jobs[0].Attempts[0].HealthScope != "pool" {
		t.Fatalf("historical attempt changed on load: %+v %v", jobs, err)
	}
	job := model.Job{RepoID: "demo", DeadlineAt: now.Add(2 * time.Hour), Task: model.Task{
		Mode: "read_only", DataClass: "synthetic", PreferredProvider: "opencode", PreferredModel: "free",
	}}
	e.mu.Lock()
	defer e.mu.Unlock()
	if c, _ := e.route(&job, now.Add(30*time.Minute)); c.Model != "alternate" || job.Route[0].Reason != "COOLDOWN" {
		t.Fatalf("provider cooldown was ignored: %+v, %+v", c, job.Route)
	}
	if c, _ := e.route(&job, now.Add(time.Hour)); c.Model != "free" {
		t.Fatalf("provider remained blocked after Retry-After: %+v", c)
	}
}

func TestOpenCodeModelLimitsRemainIndependentAfterRestart(t *testing.T) {
	now := time.Now().UTC()
	const first, second, third = "opencode/free-a", "opencode/free-a-alt", "opencode/free-c"
	engine, job := openCodeFixture(now)
	engine.cfg.Repositories["demo"] = model.Repo{Providers: []string{"opencode"}}
	engine.reg.Candidates = []model.Capability{
		{Provider: "opencode", Model: first, Status: "available", Speed: "fast"},
		{Provider: "opencode", Model: second, Status: "available", Speed: "fast"},
		{Provider: "opencode", Model: third, Status: "available", Speed: "fast"},
	}
	job.DeadlineAt = now.Add(4 * time.Hour)
	job.Task.PreferredModel = first

	firstReset, secondReset := now.Add(2*time.Hour), now.Add(15*time.Minute)
	firstLimit := model.Attempt{Number: 1, Provider: "opencode", Model: first, Code: "RATE_LIMIT", FinishedAt: &now, RetryAfterMS: 7_200_000}
	engine.rememberOpenCodeHealth(&firstLimit)
	assertRoute := func(e *Engine, at time.Time, selected string, resets map[string]time.Time) {
		t.Helper()
		e.mu.Lock()
		defer e.mu.Unlock()
		if c, _ := e.route(&job, at); c.Model != selected {
			t.Fatalf("selected %q, want %q at %v: %+v", c.Model, selected, at, job.Route)
		}
		if len(job.Route) != len(engine.reg.Candidates) {
			t.Fatalf("route omitted a model: %+v", job.Route)
		}
		for _, decision := range job.Route {
			reset, blocked := resets[decision.Model]
			if blocked && (decision.Reason != "COOLDOWN" || decision.HealthScope != "model" || decision.RetryAt == nil || !decision.RetryAt.Equal(reset)) ||
				!blocked && (decision.Reason == "COOLDOWN" || decision.RetryAt != nil) {
				t.Fatalf("wrong model gate for %q, want reset %v: %+v", decision.Model, reset, decision)
			}
		}
	}
	assertRoute(engine, now, second, map[string]time.Time{first: firstReset})
	secondLimit := model.Attempt{Number: 1, Provider: "opencode", Model: second, Code: "ACCESS_DENIED", FinishedAt: &now}
	engine.rememberOpenCodeHealth(&secondLimit)
	resets := map[string]time.Time{first: firstReset, second: secondReset}
	assertRoute(engine, now, third, resets)
	assertQuota := func(e *Engine) {
		t.Helper()
		rows := e.Quota("opencode")
		if len(rows) != 3 {
			t.Fatalf("expected one row per exact model: %+v", rows)
		}
		for id, reason := range map[string]string{first: "RATE_LIMIT", second: "ACCESS_DENIED", third: ""} {
			q := quotaForModel(t, rows, id)
			if q.Provider != "opencode" || q.OpenCodeAccount != model.OpenCodeAnonymous || q.State != "UNKNOWN" || q.Remaining != nil {
				t.Fatalf("wrong quota identity or inferred capacity: %+v", q)
			}
			if reset, blocked := resets[q.Model]; blocked {
				if q.Reason != reason || q.HealthScope != "model" || q.ResetAt == nil || !q.ResetAt.Equal(reset) {
					t.Fatalf("wrong quota gate, want %s and %v: %+v", reason, reset, q)
				}
			} else if q.ResetAt != nil || q.HealthScope != "" || q.Reason != "" {
				t.Fatalf("unaffected model reported a gate: %+v", q)
			}
		}
	}
	assertQuota(engine)

	db, err := store.Open(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	firstLimit.HealthScope = "pool" // Simulate the historical local inference.
	if _, err := db.Insert([]model.Job{
		{ID: "first-model-limit", RepoID: "demo", Status: model.Failed, CreatedAt: now, Attempts: []model.Attempt{firstLimit}},
		{ID: "second-model-limit", RepoID: "demo", Status: model.Failed, CreatedAt: now, Attempts: []model.Attempt{secondLimit}},
	}, ""); err != nil {
		t.Fatal(err)
	}
	restarted, err := New(engine.cfg, db, engine.reg)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	assertQuota(restarted)
	assertRoute(restarted, now, third, resets)
	assertRoute(restarted, secondReset, second, map[string]time.Time{first: firstReset})
	assertRoute(restarted, firstReset, first, nil)
}

func TestOpenCodeLegacyTransientReplayUsesOnlyExactModelHistory(t *testing.T) {
	now := time.Now().UTC()
	first, second, third := now.Add(-4*time.Minute), now.Add(-3*time.Minute), now.Add(-2*time.Minute)
	poolReset := third.Add(5 * time.Minute)
	db, err := store.Open(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Insert([]model.Job{
		{ID: "first-failure", RepoID: "demo", Status: model.Failed, CreatedAt: first, Attempts: []model.Attempt{
			{Number: 1, Provider: "opencode", Model: "free-a", Code: "ATTEMPT_TIMEOUT", FinishedAt: &first, HealthScope: "pool"},
		}},
		{ID: "second-failure", RepoID: "demo", Status: model.Failed, CreatedAt: second, Attempts: []model.Attempt{
			{Number: 1, Provider: "opencode", Model: "free-b", Code: "TEMPORARY_PROVIDER_ERROR", FinishedAt: &second, HealthScope: "pool"},
		}},
		{ID: "third-failure", RepoID: "demo", Status: model.Failed, CreatedAt: third, Attempts: []model.Attempt{
			{Number: 1, Provider: "opencode", Model: "free-a", Code: "TEMPORARY_PROVIDER_ERROR", FinishedAt: &third, HealthScope: "pool", RetryAt: &poolReset},
		}},
	}, ""); err != nil {
		t.Fatal(err)
	}
	fixture, job := openCodeFixture(now)
	e, err := New(fixture.cfg, db, fixture.reg)
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	for _, q := range e.Quota("opencode") {
		if q.ResetAt != nil || q.HealthScope != "" {
			t.Fatalf("legacy aggregate transient circuit survived as a model gate: %+v", q)
		}
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if c, _ := e.route(&job, now); c.Model != "free-a" {
		t.Fatalf("replayed pooled circuit blocked the preferred model: %+v %+v", c, job.Route)
	}
}

func TestOpenCodeTransportCooldownSurvivesRestart(t *testing.T) {
	const id, alternate, upstream = "opencode/free-a", "opencode/free-b", "elsewhere/free-a"
	const account = "test-account"
	for _, scope := range []string{"", "model", "account", "pool"} {
		t.Run("provider_scope_"+scope, func(t *testing.T) {
			db, err := store.Open(filepath.Join(t.TempDir(), "state"))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			now := time.Now().UTC()
			limit := model.Attempt{Number: 1, Provider: "opencode", Model: id, OpenCodeAccount: account,
				Code: "RATE_LIMIT", FinishedAt: &now, RetryAfterMS: 3_600_000, HealthScope: "pool", ProviderRateLimitScope: scope}
			if _, err := db.Insert([]model.Job{{ID: "legacy-limit", RepoID: "demo", Status: model.Failed, CreatedAt: now,
				Attempts: []model.Attempt{limit}}}, ""); err != nil {
				t.Fatal(err)
			}
			native := model.Capability{Provider: "opencode", Model: id, OpenCodeAccount: account,
				OpenCodeTransport: model.OpenCodeNativeReadAsk, Status: "available", Speed: "fast"}
			legacy, otherModel, otherAccount, otherUpstream := native, native, native, native
			legacy.OpenCodeTransport = ""
			otherModel.Model, otherAccount.OpenCodeAccount, otherUpstream.Model = alternate, "other-account", upstream
			reg := providers.Registry{Candidates: []model.Capability{legacy, native, otherModel, otherAccount, otherUpstream}}
			e, err := New(model.Config{}, db, reg)
			if err != nil {
				t.Fatal(err)
			}
			defer e.Close()
			e.mu.Lock()
			for _, tc := range []struct {
				candidate model.Capability
				blocked   bool
			}{{legacy, true}, {native, scope != ""}, {otherModel, scope == "account" || scope == "pool"},
				{otherAccount, false}, {otherUpstream, false}} {
				actual, gate := e.openCodeGate(tc.candidate, now)
				if (actual != "") != tc.blocked || (tc.blocked && !gate.until.Equal(now.Add(time.Hour))) {
					t.Errorf("wrong replayed gate for %+v: %q %+v", tc.candidate, actual, gate)
				}
			}
			e.mu.Unlock()
			var seen int
			for _, q := range e.Quota("opencode") {
				if q.Model != id || q.OpenCodeAccount != account {
					continue
				}
				seen++
				blocked := q.OpenCodeTransport == model.OpenCodeStateless || scope == "model"
				if (q.ResetAt != nil) != (blocked && (scope == "" || scope == "model")) {
					t.Fatalf("wrong transport quota: %+v", q)
				}
			}
			if seen != 2 {
				t.Fatalf("expected separate quota rows for the two transports, got %d", seen)
			}
			jobs, err := db.All()
			if err != nil || len(jobs) != 1 || jobs[0].Attempts[0].OpenCodeTransport != "" ||
				jobs[0].Attempts[0].HealthScope != "pool" {
				t.Fatalf("restart rewrote raw legacy evidence: %+v %v", jobs, err)
			}
		})
	}
}

func TestHistoricalOpenCodeGateKeepsProvenanceWithoutCopyingJobs(t *testing.T) {
	now := time.Now().UTC()
	source, err := store.Open(filepath.Join(t.TempDir(), "source"))
	if err != nil {
		t.Fatal(err)
	}
	job := model.Job{ID: "historical-limit", RepoID: "demo", Status: model.Failed, CreatedAt: now, Attempts: []model.Attempt{
		{Number: 1, Provider: "opencode", Model: "opencode/deepseek-v4.1-flash-free", Code: "RATE_LIMIT", FinishedAt: &now,
			OpenCodeTransport: model.OpenCodeNativeReadAsk,
			Fingerprint:       "bfd1f79c2fb005246c56d0e9fb472206c22782d065872d71c26f01f3eb3a0a82", RetryAfterMS: 3_600_000},
	}}
	_, err = source.Insert([]model.Job{job}, "")
	source.Close()
	if err != nil {
		t.Fatal(err)
	}
	records, err := store.ReadOpenCodeRateLimits(context.Background(), source.Dir)
	if err != nil || len(records) != 1 || records[0].JobID != job.ID || records[0].OpenCodeTransport != model.OpenCodeNativeReadAsk {
		t.Fatalf("missing historical limit: %+v %v", records, err)
	}
	db, err := store.Open(filepath.Join(t.TempDir(), "destination"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for i := range 2 {
		stored, err := db.SaveOpenCodeCooldownImports(records, now.Add(time.Duration(i)*time.Minute))
		if err != nil || len(stored) != 1 || !stored[0].ImportedAt.Equal(now) || stored[0].OpenCodeTransport != model.OpenCodeNativeReadAsk {
			t.Fatalf("import was not idempotent: %+v %v", stored, err)
		}
	}
	jobs, err := db.All()
	if err != nil || len(jobs) != 0 {
		t.Fatalf("historical attempt was copied into live job history: %d %v", len(jobs), err)
	}
	reg := providers.Registry{Candidates: []model.Capability{
		{Provider: "opencode", Model: "opencode/deepseek-v4.1-flash-free", OpenCodeTransport: model.OpenCodeNativeReadAsk},
		{Provider: "opencode", Model: "opencode/another-free-model"},
	}}
	e, err := New(model.Config{}, db, reg)
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	quota := e.Quota("opencode")
	if q := quotaForModel(t, quota, "opencode/deepseek-v4.1-flash-free"); q.Reason != "RATE_LIMIT" || q.HealthScope != "model" ||
		q.OpenCodeTransport != model.OpenCodeNativeReadAsk || q.ResetAt == nil || !q.ResetAt.Equal(now.Add(time.Hour)) {
		t.Fatalf("historical model gate missing: %+v", q)
	}
	if q := quotaForModel(t, quota, "opencode/another-free-model"); q.ResetAt != nil {
		t.Fatalf("historical gate blocked another model: %+v", q)
	}
}

func TestOpenCodeCooldownImportRetainsExactAccountAndDeadline(t *testing.T) {
	now := time.Now().UTC()
	reset := now.Add(90 * time.Minute)
	const modelID, alternate = "opencode/deepseek-v4.1-flash-free", "opencode/longcat-2.5-free"
	const account, otherAccount = "account-a", "account-b"
	source, err := store.Open(filepath.Join(t.TempDir(), "source"))
	if err != nil {
		t.Fatal(err)
	}
	job := model.Job{ID: "account-limit", RepoID: "demo", Status: model.Failed, CreatedAt: now, Attempts: []model.Attempt{
		{Number: 1, Provider: "opencode", Model: modelID, OpenCodeAccount: account, Code: "RATE_LIMIT", FinishedAt: &now,
			Fingerprint: "bfd1f79c2fb005246c56d0e9fb472206c22782d065872d71c26f01f3eb3a0a82", RetryAfterMS: 3_600_000,
			RetryAt: &reset, HealthScope: "pool"},
	}}
	_, err = source.Insert([]model.Job{job}, "")
	source.Close()
	if err != nil {
		t.Fatal(err)
	}
	records, err := store.ReadOpenCodeRateLimits(context.Background(), source.Dir)
	if err != nil || len(records) != 1 {
		t.Fatalf("could not read source gate: %+v %v", records, err)
	}
	db, err := store.Open(filepath.Join(t.TempDir(), "destination"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	r := records[0]
	legacy := store.OpenCodeCooldownImport{SourceDir: r.SourceDir, JobID: r.JobID, Attempt: r.Attempt, Model: r.Model,
		Fingerprint: r.Fingerprint, FinishedAt: r.FinishedAt, RetryAfterMS: r.RetryAfterMS, ProviderRateLimitScope: r.ProviderRateLimitScope}
	if _, err := db.SaveOpenCodeCooldownImports([]store.OpenCodeCooldownImport{legacy}, now); err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 2; i++ {
		stored, err := db.SaveOpenCodeCooldownImports(records, now.Add(time.Duration(i)*time.Minute))
		if err != nil || len(stored) != 1 || !stored[0].ImportedAt.Equal(now) {
			t.Fatalf("re-import changed the record's identity or original provenance: %+v %v", stored, err)
		}
	}
	reg := providers.Registry{Candidates: []model.Capability{
		{Provider: "opencode", Model: modelID, OpenCodeAccount: account},
		{Provider: "opencode", Model: modelID, OpenCodeAccount: otherAccount},
		{Provider: "opencode", Model: alternate, OpenCodeAccount: account},
	}}
	e, err := New(model.Config{}, db, reg)
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	rows := e.Quota("opencode")
	if len(rows) != 3 {
		t.Fatalf("legacy import left a spurious gate: %+v", rows)
	}
	var found bool
	for _, q := range rows {
		if q.Model == modelID && q.OpenCodeAccount == account {
			found = true
			if q.Reason != "RATE_LIMIT" || q.HealthScope != "model" || q.ResetAt == nil || !q.ResetAt.Equal(reset) {
				t.Fatalf("import lost the exact saved model reset: %+v", q)
			}
		} else if q.ResetAt != nil || q.HealthScope != "" {
			t.Fatalf("import widened a legacy gate to another model or account: %+v", q)
		}
	}
	if !found {
		t.Fatal("import did not retain the model's exact account")
	}
}

func TestOpenCodeHealthScopesAndFallback(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct {
		code, providerScope, scope, selected string
	}{
		{"MODEL_UNAVAILABLE", "", "model", "free-b"},
		{"ZERO_COST_UNVERIFIED", "", "model", "free-b"},
		{"ACCESS_DENIED", "", "model", "free-b"},
		{"AUTH_REQUIRED", "", "model", "free-b"},
		{"ZERO_COST_UNAVAILABLE", "", "model", "free-b"},
		{"RATE_LIMIT", "", "model", "free-b"},
		{"RATE_LIMIT", "pool", "pool", "synthetic"},
		{"RATE_LIMIT", "account", "account", "synthetic"},
		{"TEMPORARY_PROVIDER_ERROR", "", "model", "free-b"},
		{"ATTEMPT_TIMEOUT", "", "model", "free-b"},
		{"OPENCODE_UPSTREAM_UNAVAILABLE", "", "model", "free-b"},
		{"PROVIDER_STARTUP_TIMEOUT", "", "model", "free-b"},
		{"PROVIDER_STARTUP_FAILED", "", "model", "free-b"},
	} {
		t.Run(tc.code+"/"+tc.providerScope, func(t *testing.T) {
			e, job := openCodeFixture(now)
			attempt := model.Attempt{Provider: "opencode", Model: "free-a", Code: tc.code, FinishedAt: &now,
				ProviderRateLimitScope: tc.providerScope}
			e.rememberOpenCodeHealth(&attempt)
			candidate, _ := e.route(&job, now)
			if candidate.Model != tc.selected || attempt.HealthScope != tc.scope || attempt.RetryAt == nil ||
				job.Route[0].Reason != "COOLDOWN" || job.Route[0].HealthScope != tc.scope {
				t.Fatalf("wrong cooldown or fallback: %+v %+v %+v", attempt, candidate, job.Route)
			}
			// An existing job may use a different configured model once.
			job.Attempts = []model.Attempt{attempt}
			if c, _ := e.route(&job, *attempt.RetryAt); c.Model != "free-b" {
				t.Fatalf("retried the failed model or skipped the alternate: %+v", c)
			}
			job.Attempts = append(job.Attempts, model.Attempt{Provider: "opencode", Model: "free-b"})
			if c, _ := e.route(&job, *attempt.RetryAt); c.Provider != "fixture-b" {
				t.Fatalf("retried an attempted model: %+v", c)
			}
		})
	}
}

func TestOpenCodeCircuitAndRecoveryProbeAreModelScoped(t *testing.T) {
	now := time.Now()
	e, job := openCodeFixture(now)
	e.cfg.Repositories["demo"] = model.Repo{Providers: []string{"opencode"}}
	for _, event := range []struct {
		model, code string
		offset      time.Duration
		delay       time.Duration
	}{
		{"free-a", "TEMPORARY_PROVIDER_ERROR", -2 * time.Minute, 30 * time.Second},
		{"free-b", "ATTEMPT_TIMEOUT", -90 * time.Second, 30 * time.Second},
		{"free-b", "TEMPORARY_PROVIDER_ERROR", -75 * time.Second, 30 * time.Second},
		{"free-a", "ATTEMPT_TIMEOUT", -time.Minute, 30 * time.Second},
		{"free-b", "", -50 * time.Second, 0},
		{"free-b", "ATTEMPT_TIMEOUT", -40 * time.Second, 30 * time.Second},
		{"free-a", "TEMPORARY_PROVIDER_ERROR", 0, 5 * time.Minute},
	} {
		at := now.Add(event.offset)
		a := model.Attempt{Provider: "opencode", Model: event.model, Code: event.code, FinishedAt: &at}
		e.rememberOpenCodeHealth(&a)
		if event.delay > 0 && (a.RetryAt == nil || !a.RetryAt.Equal(at.Add(event.delay))) {
			t.Fatalf("another model changed the circuit delay for %s: %+v", event.model, a)
		}
	}
	until := now.Add(5 * time.Minute)
	if c, _ := e.route(&job, now); c.Model != "free-b" {
		t.Fatalf("one open circuit blocked another model: %+v %+v", c, job.Route)
	}
	job.Attempts = []model.Attempt{{Provider: "opencode", Model: "free-b"}}
	if c, _ := e.route(&job, now); c.Provider != "" {
		t.Fatalf("open circuit allowed a route: %+v", c)
	}
	finishWithoutRoute(&job)
	if job.Status != model.RetryWait || !job.ReadyAt.Equal(until) {
		t.Fatalf("wrong circuit wait: %s %v", job.Status, job.ReadyAt)
	}
	short := job
	short.Attempts = nil
	short.DeadlineAt = until.Add(time.Second)
	candidates := e.reg.Candidates
	e.reg.Candidates = candidates[:1]
	e.route(&short, now)
	e.reg.Candidates = candidates
	finishWithoutRoute(&short)
	if short.Status != model.Blocked || short.Reason != "COOLDOWN" {
		t.Fatalf("job waited past its useful deadline: %s %s", short.Status, short.Reason)
	}
	if c, _ := e.route(&job, until); c.Model != "free-a" {
		t.Fatalf("recovery probe remained blocked: %+v", c)
	}
	e.active = map[string]context.CancelFunc{"probe": func() {}}
	e.jobs = map[string]model.Job{"probe": {Attempts: []model.Attempt{{Provider: "opencode", Model: "free-a"}}}}
	if c, _ := e.route(&job, until); c.Provider != "" || job.Route[0].Reason != "OPENCODE_MODEL_IN_FLIGHT" || job.Route[0].HealthScope != "model" {
		t.Fatalf("concurrent probe bypassed the gate: %+v %+v", c, job.Route)
	}
	job.Attempts = nil
	if c, _ := e.route(&job, until); c.Model != "free-b" {
		t.Fatalf("an in-flight probe blocked another model: %+v %+v", c, job.Route)
	}
}

func TestOpenCodeAccountsAndProviderAssertedSharedLimits(t *testing.T) {
	now := time.Now().UTC()
	const modelID, alternateID, otherUpstreamID = "opencode/free-a", "opencode/free-b", "elsewhere/free-a"
	const firstAccount, secondAccount = "account-a", "account-b"
	for _, scope := range []string{"model", "pool", "account"} {
		t.Run(scope, func(t *testing.T) {
			e, job := openCodeFixture(now)
			e.reg.Candidates = []model.Capability{
				{Provider: "opencode", Model: modelID, OpenCodeAccount: firstAccount, Status: "available", Speed: "fast"},
				{Provider: "opencode", Model: alternateID, OpenCodeAccount: firstAccount, Status: "available", Speed: "fast"},
				{Provider: "opencode", Model: modelID, OpenCodeAccount: secondAccount, Status: "available", Speed: "fast"},
				{Provider: "opencode", Model: otherUpstreamID, OpenCodeAccount: firstAccount, Status: "available", Speed: "fast"},
				{Provider: "fixture-b", Model: modelID, Status: "available", Speed: "fixture", Mode: "fixture"},
			}
			job.Task.PreferredModel = modelID
			limited := model.Attempt{Provider: "opencode", Model: modelID, OpenCodeAccount: firstAccount, Code: "RATE_LIMIT",
				ProviderRateLimitScope: scope, FinishedAt: &now, RetryAfterMS: 120_000}
			e.rememberOpenCodeHealth(&limited)
			e.rememberRateLimit(model.Attempt{Provider: "fixture-b", Model: modelID, Code: "RATE_LIMIT", FinishedAt: &now})
			if c, _ := e.route(&job, now); c.Provider != "opencode" || c.Model != modelID || c.OpenCodeAccount != secondAccount {
				t.Fatalf("a different account or provider blocked the exact route: %+v %+v", c, job.Route)
			}
			for _, decision := range job.Route {
				if decision.Model == alternateID && (decision.Reason == "COOLDOWN") != (scope != "model") {
					t.Fatalf("wrong shared gate on same-account alternate: %+v", decision)
				}
				if decision.Model == otherUpstreamID && decision.Reason != "FALLBACK_CANDIDATE" {
					t.Fatalf("same account label widened the gate to another upstream: %+v", decision)
				}
			}
			rows := e.Quota("opencode")
			wantRows := map[[2]string]bool{
				{firstAccount, modelID}:         scope == "model",
				{firstAccount, alternateID}:     false,
				{secondAccount, modelID}:        false,
				{firstAccount, otherUpstreamID}: false,
			}
			if scope != "model" {
				wantRows[[2]string{firstAccount, ""}] = true
			}
			if len(rows) != len(wantRows) {
				t.Fatalf("expected exact model rows and any separate shared row: %+v", rows)
			}
			for _, q := range rows {
				key := [2]string{q.OpenCodeAccount, q.Model}
				blocked, expected := wantRows[key]
				delete(wantRows, key)
				if !expected || (q.Model == otherUpstreamID) != (q.OpenCodeProvider == "elsewhere") ||
					blocked && (q.Reason != "RATE_LIMIT" || q.HealthScope != scope || q.ResetAt == nil || !q.ResetAt.Equal(now.Add(2*time.Minute))) ||
					!blocked && (q.ResetAt != nil || q.HealthScope != "") {
					t.Fatalf("wrong account/model/shared quota gate: %+v", q)
				}
			}
			if scope != "model" {
				return
			}
			// Earlier attempts and active workers reserve only their exact identity.
			job.Attempts = []model.Attempt{limited}
			e.active = map[string]context.CancelFunc{"probe": func() {}}
			e.jobs = map[string]model.Job{"probe": {Attempts: []model.Attempt{{Provider: "opencode", Model: modelID, OpenCodeAccount: firstAccount}}}}
			reset := now.Add(2 * time.Minute)
			if c, _ := e.route(&job, reset); c.Model != modelID || c.OpenCodeAccount != secondAccount {
				t.Fatalf("another account's previous attempt or worker blocked the exact route: %+v %+v", c, job.Route)
			}
			for _, q := range e.openCodeQuota(reset) {
				reserved := q.Model == modelID && q.OpenCodeAccount == firstAccount
				if reserved && (q.Reason != "OPENCODE_MODEL_IN_FLIGHT" || q.HealthScope != "model") || !reserved && q.Reason != "" || q.ResetAt != nil {
					t.Fatalf("incorrect quota reservation at reset: %+v", q)
				}
			}
		})
	}
}

func TestM1RoutesAutomaticallyAmongOnlyVerifiedOpenCodeIdentities(t *testing.T) {
	now := time.Now().UTC()
	e, job := openCodeFixture(now)
	e.cfg.Repositories["demo"] = model.Repo{Providers: []string{"opencode"}}
	job.Task.PreferredProvider, job.Task.PreferredModel = "", ""
	const first, second, third = "opencode/free-a", "opencode/free-b", "opencode/free-c"
	const verifiedFP = "verified-fingerprint"
	e.reg.Candidates = []model.Capability{
		{Provider: "opencode", Model: first, Status: "available", Speed: "fast"},
		{Provider: "opencode", Model: second, OpenCodeAccount: "other-account", Fingerprint: verifiedFP, Status: "available", Speed: "fast"},
		{Provider: "opencode", Model: second, Fingerprint: "other-fingerprint", Status: "available", Speed: "fast"},
		{Provider: "opencode", Model: second, OpenCodeTransport: model.OpenCodeNativeReadAsk, Fingerprint: verifiedFP, Status: "available", Speed: "fast"},
		{Provider: "opencode", Model: second, Fingerprint: verifiedFP, Status: "available", Speed: "fast"},
		{Provider: "opencode", Model: third, Fingerprint: verifiedFP, Status: "available", Speed: "fast"},
	}
	ordinary := job
	job.M1OpenCodeRoutes = []model.OpenCodeRoute{
		{Model: second, Account: model.OpenCodeAnonymous, Fingerprint: verifiedFP},
		{Model: third, Account: model.OpenCodeAnonymous, Fingerprint: verifiedFP},
	}
	if c, _ := e.route(&job, now); c.Model != second || model.OpenCodeAccountID(c.OpenCodeAccount) != model.OpenCodeAnonymous || c.Fingerprint != verifiedFP {
		t.Fatalf("M1 selected a route without exact prior evidence: %+v %+v", c, job.Route)
	}
	if len(job.Route) != len(e.reg.Candidates) {
		t.Fatalf("M1 route audit omitted a candidate: %+v", job.Route)
	}
	for _, decision := range job.Route[:4] {
		if decision.Reason != "M1_ROUTE_NOT_VERIFIED" {
			t.Fatalf("wrong reason for unverified M1 candidate: %+v", decision)
		}
	}
	limited := model.Attempt{Provider: "opencode", Model: second, OpenCodeAccount: model.OpenCodeAnonymous, Code: "RATE_LIMIT", FinishedAt: &now}
	e.rememberOpenCodeHealth(&limited)
	if c, _ := e.route(&job, now); c.Model != third {
		t.Fatalf("M1 skipped an unaffected verified alternate: %+v %+v", c, job.Route)
	}
	job.M1OpenCodeRoutes[0].OpenCodeTransport = model.OpenCodeNativeReadAsk
	if c, _ := e.route(&job, now); c.Model != second || c.OpenCodeTransport != model.OpenCodeNativeReadAsk {
		t.Fatalf("M1 could not select its verified transport during a stateless cooldown: %+v %+v", c, job.Route)
	}
	job.M1OpenCodeRoutes[0].OpenCodeTransport = ""
	limited = model.Attempt{Provider: "opencode", Model: third, Code: "RATE_LIMIT", FinishedAt: &now, RetryAfterMS: 120_000}
	e.rememberOpenCodeHealth(&limited)
	if c, _ := e.route(&job, now); c.Provider != "" {
		t.Fatalf("M1 fell back to an unverified route: %+v %+v", c, job.Route)
	}
	finishWithoutRoute(&job)
	if job.Status != model.RetryWait || !job.ReadyAt.Equal(now.Add(time.Minute)) {
		t.Fatalf("M1 did not wait for the first verified route's reset: %s %s %v", job.Status, job.Reason, job.ReadyAt)
	}
	if c, _ := e.route(&ordinary, now); c.Model != first {
		t.Fatalf("ordinary job inherited the M1 admission restriction: %+v %+v", c, ordinary.Route)
	}
}

func TestM1AdmissionPersistsRoutesAndKeepsAcceptedRequestIdempotent(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	e, _ := openCodeFixture(time.Now())
	e.db, e.jobs = db, make(map[string]model.Job)
	e.cfg.Repositories["demo"] = model.Repo{Providers: []string{"opencode"}, AllowedPaths: []string{"marker.txt"}}
	request := func(id string) model.Batch {
		return model.Batch{RepoID: "demo", RequestID: id, Items: []model.Item{
			{Key: "receive", Task: model.Task{Objective: "Read the synthetic marker", DataClass: "synthetic", Mode: "read_only",
				AllowedPaths: []string{"marker.txt"}, Acceptance: []model.Check{{ID: "marker", Description: "Return the marker"}}}},
		}}
	}
	allowed := model.OpenCodeRoute{Model: "opencode/free-a", Account: model.OpenCodeAnonymous, Fingerprint: "verified-fingerprint",
		OpenCodeTransport: model.OpenCodeNativeReadAsk}
	parent, err := e.SubmitM1Batch(request("fixture-m1"), []model.OpenCodeRoute{allowed}, nil)
	if err != nil {
		t.Fatal(err)
	}
	denied := model.Error("M1_ROUTE_NOT_VERIFIED", "the model gate is active")
	if duplicate, err := e.SubmitM1Batch(request("fixture-m1"), nil, denied); err != nil || duplicate.ID != parent.ID {
		t.Fatalf("retry did not return the accepted request: %q %v", duplicate.ID, err)
	}
	if _, err := e.SubmitM1Batch(request("different-request"), nil, denied); err != denied {
		t.Fatalf("new batch bypassed missing admission: %v", err)
	}
	jobs, err := db.All()
	if err != nil || len(jobs) != 2 || len(jobs[1].M1OpenCodeRoutes) != 1 || jobs[1].M1OpenCodeRoutes[0] != allowed {
		t.Fatalf("exact M1 route was not preserved in durable receiver state: %+v %v", jobs, err)
	}
}

func quotaForModel(t *testing.T, rows []providers.Quota, modelID string) providers.Quota {
	t.Helper()
	for _, q := range rows {
		if q.Model == modelID {
			return q
		}
	}
	t.Fatalf("missing quota for %q: %+v", modelID, rows)
	return providers.Quota{}
}

func openCodeFixture(now time.Time) (*Engine, model.Job) {
	e := &Engine{
		cfg:      model.Config{Repositories: map[string]model.Repo{"demo": {Providers: []string{"opencode", "fixture-b"}}}},
		cooldown: make(map[string]time.Time),
		reg: providers.Registry{Candidates: []model.Capability{
			{Provider: "opencode", Model: "free-a", Status: "available", Speed: "fast"},
			{Provider: "opencode", Model: "free-b", Status: "available", Speed: "fast"},
			{Provider: "fixture-b", Model: "synthetic", Status: "available", Speed: "fixture", Mode: "fixture"},
		}},
	}
	j := model.Job{RepoID: "demo", DeadlineAt: now.Add(time.Hour), Task: model.Task{
		Mode: "read_only", DataClass: "synthetic", PreferredProvider: "opencode", PreferredModel: "free-a",
	}}
	return e, j
}

func TestQuotaHealthNormalizationAndNonBlocking(t *testing.T) {
	e := &Engine{
		quotas:           map[string][]providers.Quota{},
		quotaRefreshedAt: time.Now().UTC(),
	}
	remaining75 := 75.0
	remaining20 := 20.0
	e.quotas["codex"] = []providers.Quota{
		{Bucket: providers.BarCodexBurst, Remaining: &remaining75, State: "KNOWN"},
		{Bucket: providers.BarCodexWeekly, Remaining: &remaining20, State: "KNOWN"},
	}
	if h := e.HealthPercent(providers.BarCodexBurst); h != 0.75 {
		t.Fatalf("expected health 0.75, got %v", h)
	}
	if st := e.BarState(providers.BarCodexBurst); st != "HEALTHY" {
		t.Errorf("expected HEALTHY, got %s", st)
	}
	if st := e.BarState(providers.BarCodexWeekly); st != "PRESSURE_HIGH" {
		t.Errorf("expected PRESSURE_HIGH, got %s", st)
	}

	// Verify non-blocking concurrency under 50 routines
	var wg sync.WaitGroup
	start := time.Now()
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			q := e.Quota("codex")
			if len(q) == 0 {
				t.Errorf("expected quotas returned")
			}
		}()
	}
	wg.Wait()
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Errorf("quota read took too long: %v", elapsed)
	}
}
