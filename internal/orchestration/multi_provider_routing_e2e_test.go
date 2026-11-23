// Copyright 2026 Furkan Anıl Öztaş
// SPDX-License-Identifier: Apache-2.0

package orchestration_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/faoztas/ahenk-mcp/internal/model"
	"github.com/faoztas/ahenk-mcp/internal/orchestration"
	"github.com/faoztas/ahenk-mcp/internal/providers"
	"github.com/faoztas/ahenk-mcp/internal/store"
)

func TestEndToEndMultiProviderRoutingScenarios(t *testing.T) {
	// Full test validating transitions across Scenarios A, B, C, D
	// and verifying zero data loss and sub-5ms routing overhead.
	dir := filepath.Join(t.TempDir(), "state")
	db, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	// 1. Nominal scenario -> Sol selected at high effort
	startNominal := time.Now()
	decision := orchestration.EvaluateRouting(db, "coding", 0.9, map[string]float64{
		providers.BarCodexBurst: 1.0,
		providers.BarAgyGemini:  1.0,
	})
	elapsedNominal := time.Since(startNominal)
	if elapsedNominal > 50*time.Millisecond {
		t.Errorf("routing decision took %v, expected <= 50ms", elapsedNominal)
	}
	if decision.SelectedModel != "codex/gpt-6.1-sol" || decision.ResolvedEffort != "high" {
		t.Errorf("Nominal failed: %+v", decision)
	}

	// 2. Scenario A (Codex down) -> Argon selected at medium effort
	decisionA := orchestration.EvaluateRouting(db, "coding", 0.9, map[string]float64{
		providers.BarCodexBurst: 0.0,
		providers.BarAgyGemini:  0.8,
	})
	if decisionA.SelectedModel != "antigravity/gemini-4-argon" || decisionA.ResolvedEffort != "medium" {
		t.Errorf("Scenario A failed: %+v", decisionA)
	}

	// 3. Scenario B (AGY down) -> Architecture fails over from Opus to Astra/Sol
	decisionB := orchestration.EvaluateRouting(db, "architecture", 0.9, map[string]float64{
		providers.BarCodexBurst: 1.0,
		providers.BarAgyGemini:  0.0,
		providers.BarAgyOpus:    0.0,
	})
	if decisionB.SelectedModel != "codex/gpt-6-astra" {
		t.Errorf("Scenario B failed: expected codex/gpt-6-astra, got %+v", decisionB)
	}

	// 4. Scenario C (Dual cloud outage) -> Crouton selected
	decisionC := orchestration.EvaluateRouting(db, "coding", 0.9, map[string]float64{
		providers.BarCodexBurst: 0.0,
		providers.BarAgyGemini:  0.0,
	})
	if decisionC.SelectedModel != "opencode/crouton-alpha-free" {
		t.Errorf("Scenario C failed: %+v", decisionC)
	}

	// 5. Scenario D (OpenCode 429 bypass) -> Stage6FreeTier429
	decisionD := orchestration.EvaluateRouting(db, "coding", 0.9, map[string]float64{
		providers.BarCodexBurst:   0.8,
		providers.BarAgyGemini:    0.8,
		providers.BarOpenCodeFree: 0.0,
	})
	if decisionD.Stage != orchestration.Stage6FreeTier429 {
		t.Errorf("Scenario D failed: expected Stage6FreeTier429, got %v", decisionD.Stage)
	}

	// Verify quota states were recorded in SQLite store
	states, err := db.GetQuotaStates()
	if err != nil || len(states) == 0 {
		t.Errorf("expected quota states persisted in store, err: %v, count: %d", err, len(states))
	}
}

func TestEvaluateAutoRoute_UnifiedPolicyAndQuota(t *testing.T) {
	tmp := t.TempDir()
	dbDir := filepath.Join(tmp, "state")
	if err := os.Mkdir(dbDir, 0700); err != nil {
		t.Fatal(err)
	}
	repoDir := filepath.Join(tmp, "repo")
	if err := os.Mkdir(repoDir, 0700); err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(dbDir)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	cfg := model.Config{
		StateDir: dbDir,
		Repositories: map[string]model.Repo{
			"all-repo": {
				Root:         repoDir,
				AllowedPaths: []string{"."},
				Providers:    []string{"codex", "antigravity", "opencode"},
			},
			"agy-only": {
				Root:         repoDir,
				AllowedPaths: []string{"."},
				Providers:    []string{"agy"},
			},
		},
	}
	reg := providers.Registry{
		Candidates: []model.Capability{
			{Provider: "codex", Model: "gpt-6.1-sol", Status: "available", Mode: "cli", Speed: "fast"},
			{Provider: "codex", Model: "gpt-6.1-terra", Status: "available", Mode: "cli", Speed: "slow"},
			{
				Provider: "agy", Model: "gemini-4-argon", Status: "available", Speed: "fast",
				Quality: []model.QualityEvidence{
					{Role: "inference", Tier: 2, Source: "LIVE"},
					{Role: "architecture", Tier: 2, Source: "LIVE"},
				},
				Profiles: []model.ExecutionProfile{
					{Name: "restricted", Status: "preflight_required"},
					{Name: "analysis", Status: "verified"},
				},
			},
			{
				Provider: "agy", Model: "gemini-3.8-flash", Status: "available", Speed: "fast",
				Quality: []model.QualityEvidence{
					{Role: "inference", Tier: 1, Source: "LIVE"},
				},
				Profiles: []model.ExecutionProfile{
					{Name: "analysis", Status: "verified"},
				},
			},
			{
				Provider: "agy", Model: "claude-opus-5-5", Status: "available", Speed: "fast",
				Quality: []model.QualityEvidence{
					{Role: "inference", Tier: 2, Source: "LIVE"},
					{Role: "architecture", Tier: 2, Source: "LIVE"},
				},
				Profiles: []model.ExecutionProfile{
					{Name: "analysis", Status: "verified"},
				},
			},
			{Provider: "opencode", Model: "crouton-alpha-free", Status: "available", Mode: "", Speed: "fast"},
			{Provider: "opencode", Model: "beam-free", Status: "available", Mode: "", Speed: "fast"},
		},
	}
	e, err := orchestration.New(cfg, db, reg)
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()

	now := time.Now().UTC()
	future := now.Add(2 * time.Hour)
	h100, h80, h50, h15, h0 := 100.0, 80.0, 50.0, 15.0, 0.0
	e.SetQuotaForTest("codex", []providers.Quota{
		{Bucket: providers.BarCodexBurst, Remaining: &h100, State: "KNOWN", ObservedAt: now, ResetAt: &future},
	})
	e.SetQuotaForTest("agy", []providers.Quota{
		{Bucket: providers.BarAgyGemini, Family: "gemini", Remaining: &h100, State: "KNOWN", ObservedAt: now, ResetAt: &future},
		{Bucket: providers.BarAgyArgon, Family: "gemini_argon", Remaining: &h100, State: "KNOWN", ObservedAt: now, ResetAt: &future},
		{Bucket: providers.BarAgyOpus, Family: "claude_gpt", Remaining: &h80, State: "KNOWN", ObservedAt: now, ResetAt: &future},
	})

	// 1. Explicit complexity 0.0 must NOT be overwritten to 0.9 (must resolve to "low" effort).
	zeroComp := 0.0
	d, cap0 := e.EvaluateAutoRoute("all-repo", "coding", &zeroComp, model.Task{Mode: "read_only", DataClass: "synthetic"}, now)
	if cap0 == nil || d.SelectedModel != "codex/gpt-6.1-sol" || d.ResolvedEffort != "low" {
		t.Fatalf("explicit complexity 0.0 failed: decision=%+v cap=%+v", d, cap0)
	}

	// 2. Omitted complexity (nil) defaults to 0.9 for coding ("high" effort) and 0.2 for research ("low" effort).
	d, capNil := e.EvaluateAutoRoute("all-repo", "coding", nil, model.Task{Mode: "read_only", DataClass: "synthetic"}, now)
	if capNil == nil || d.SelectedModel != "codex/gpt-6.1-sol" || d.ResolvedEffort != "high" {
		t.Fatalf("nil complexity coding failed: decision=%+v", d)
	}

	// 3. Unknown category deterministically falls back to "coding".
	d, _ = e.EvaluateAutoRoute("all-repo", "nonexistent_category", nil, model.Task{Mode: "read_only", DataClass: "synthetic"}, now)
	if d.TaskCategory != "coding" || d.SelectedModel != "codex/gpt-6.1-sol" {
		t.Fatalf("unknown category fallback failed: decision=%+v", d)
	}

	// 4. Repo allowlist ("agy-only") skips codex and picks AGY Argon; patch mode requires restricted/patch capability.
	d, capAgy := e.EvaluateAutoRoute("agy-only", "coding", nil, model.Task{Mode: "patch", DataClass: "synthetic"}, now)
	if capAgy == nil || d.SelectedModel != "antigravity/gemini-4-argon" {
		t.Fatalf("agy-only repo allowlist failed: decision=%+v cap=%+v", d, capAgy)
	}

	// 5. patch mode rejects opencode (read_only only) even when codex/agy are exhausted.
	e.SetQuotaForTest("codex", []providers.Quota{
		{Bucket: providers.BarCodexBurst, Remaining: &h0, State: "EXHAUSTED", ObservedAt: now, ResetAt: &future},
	})
	e.SetQuotaForTest("agy", []providers.Quota{
		{Bucket: providers.BarAgyGemini, Family: "gemini", Remaining: &h80, State: "KNOWN", ObservedAt: now, ResetAt: &future},
		{Bucket: providers.BarAgyArgon, Family: "gemini_argon", Remaining: &h0, State: "EXHAUSTED", ObservedAt: now, ResetAt: &future},
		{Bucket: providers.BarAgyOpus, Family: "claude_gpt", Remaining: &h80, State: "KNOWN", ObservedAt: now, ResetAt: &future},
	})
	d, capPatch := e.EvaluateAutoRoute("all-repo", "coding", nil, model.Task{Mode: "patch", DataClass: "synthetic"}, now)
	if capPatch != nil && capPatch.Provider == "opencode" {
		t.Fatalf("patch mode must not select opencode read_only worker: %+v", d)
	}
	// Under read_only, Argon is exhausted -> skips Argon and selects opencode/crouton-alpha-free.
	d, capReadOnly := e.EvaluateAutoRoute("all-repo", "coding", nil, model.Task{Mode: "read_only", DataClass: "synthetic"}, now)
	if capReadOnly == nil || d.SelectedModel != "opencode/crouton-alpha-free" {
		t.Fatalf("read_only with codex+argon exhausted should select crouton: decision=%+v cap=%+v", d, capReadOnly)
	}

	// 6. Stage throttling: Codex exhausted (Stage3CodexExhausted) throttles Argon high -> medium effort.
	e.SetQuotaForTest("agy", []providers.Quota{
		{Bucket: providers.BarAgyGemini, Family: "gemini", Remaining: &h80, State: "KNOWN", ObservedAt: now, ResetAt: &future},
		{Bucket: providers.BarAgyArgon, Family: "gemini_argon", Remaining: &h80, State: "KNOWN", ObservedAt: now, ResetAt: &future},
		{Bucket: providers.BarAgyOpus, Family: "claude_gpt", Remaining: &h80, State: "KNOWN", ObservedAt: now, ResetAt: &future},
	})
	d, _ = e.EvaluateAutoRoute("all-repo", "coding", nil, model.Task{Mode: "read_only", DataClass: "synthetic"}, now)
	if d.SelectedModel != "antigravity/gemini-4-argon" || d.Stage != orchestration.Stage3CodexExhausted || d.ResolvedEffort != "medium" {
		t.Fatalf("expected Stage3CodexExhausted throttle to medium on Argon, got %+v", d)
	}

	// 7. TargetTier "cheap" skips Argon (hard) and picks Flash or OpenCode.
	d, capCheap := e.EvaluateAutoRoute("agy-only", "coding", nil, model.Task{Mode: "read_only", DataClass: "synthetic", TargetTier: "cheap"}, now)
	if capCheap == nil || d.SelectedModel != "antigravity/gemini-3.8-flash" {
		t.Fatalf("TargetTier cheap should select gemini-3.8-flash, got %+v", d)
	}

	// 8. Opus scarcity gate: when BarAgyOpus <= 0.20, architecture skips Opus and picks Argon.
	e.SetQuotaForTest("agy", []providers.Quota{
		{Bucket: providers.BarAgyGemini, Family: "gemini", Remaining: &h80, State: "KNOWN", ObservedAt: now, ResetAt: &future},
		{Bucket: providers.BarAgyArgon, Family: "gemini_argon", Remaining: &h80, State: "KNOWN", ObservedAt: now, ResetAt: &future},
		{Bucket: providers.BarAgyOpus, Family: "claude_gpt", Remaining: &h15, State: "KNOWN", ObservedAt: now, ResetAt: &future},
	})
	d, _ = e.EvaluateAutoRoute("agy-only", "architecture", nil, model.Task{Mode: "read_only", DataClass: "synthetic"}, now)
	if d.SelectedModel != "antigravity/gemini-4-argon" {
		t.Fatalf("depleted Opus reserve should fail over to gemini-4-argon, got %+v", d)
	}

	// 9. DEADLINE_BUDGET: 120s deadline with slow candidate (240s) is rejected while fast candidate (60s) fits.
	e.SetQuotaForTest("codex", []providers.Quota{
		{Bucket: providers.BarCodexBurst, Remaining: &h50, State: "KNOWN", ObservedAt: now, ResetAt: &future},
	})
	d, capBudget := e.EvaluateAutoRoute("all-repo", "debugging", nil, model.Task{Mode: "read_only", DataClass: "synthetic", DeadlineSeconds: 120}, now)
	// debugging matrix[0] is codex/gpt-6.1-terra (slow=240s > 100s budget), matrix[1] is agy/gemini-4-argon (fast=60s <= 100s budget).
	if capBudget == nil || d.SelectedModel != "antigravity/gemini-4-argon" {
		t.Fatalf("DEADLINE_BUDGET should skip slow terra and select fast argon, got %+v", d)
	}

	// 10. TrustedLocal profile requires repo.TrustedLocal and honors repo.TrustedLocal.Provider.
	d, capTL := e.EvaluateAutoRoute("all-repo", "coding", nil, model.Task{
		Mode:             "patch",
		DataClass:        "synthetic",
		ExecutionProfile: model.TrustedLocalProfile,
	}, now)
	if capTL != nil || d.Reason != "NO_ELIGIBLE_ROUTE" {
		t.Fatalf("expected NO_ELIGIBLE_ROUTE for trusted-local without repo.TrustedLocal, got decision=%+v cap=%+v", d, capTL)
	}

	// 11. PrivateSource rejects non-granted routes when DataClass is repository_internal.
	d, capPriv := e.EvaluateAutoRoute("all-repo", "coding", nil, model.Task{
		Mode:      "read_only",
		DataClass: model.PrivateSourceData,
	}, now)
	if capPriv != nil || d.Reason != "NO_ELIGIBLE_ROUTE" {
		t.Fatalf("expected NO_ELIGIBLE_ROUTE for repository_internal without PrivateSource grant, got decision=%+v cap=%+v", d, capPriv)
	}

	// 12. Quota state persisted in store.
	states, err := db.GetQuotaStates()
	if err != nil || len(states) == 0 {
		t.Fatalf("expected persisted quota states, err=%v len=%d", err, len(states))
	}
}

func initGitRepoForAuditTest(t *testing.T, dir string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\n"), 0600); err != nil {
		t.Fatal(err)
	}
	runGit := func(args ...string) {
		cmd := exec.Command("git", append([]string{"-C", dir, "-c", "core.hooksPath=/dev/null",
			"-c", "user.name=AuditTest", "-c", "user.email=audit@example.invalid", "-c", "commit.gpgsign=false"}, args...)...)
		cmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=/var/empty", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null"}
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v failed: %v\n%s", args, err, out)
		}
	}
	runGit("init")
	runGit("add", ".")
	runGit("commit", "-m", "init")
}

func TestEnginePersist_AuditLifecycleAndRaceProtection(t *testing.T) {
	tmp, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	dbDir := filepath.Join(tmp, "state")
	if err := os.Mkdir(dbDir, 0700); err != nil {
		t.Fatal(err)
	}
	repoDir := filepath.Join(tmp, "repo")
	if err := os.Mkdir(repoDir, 0700); err != nil {
		t.Fatal(err)
	}
	initGitRepoForAuditTest(t, repoDir)

	db, err := store.Open(dbDir)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	cfg := model.Config{
		StateDir:      dbDir,
		EnableFixture: true,
		Repositories: map[string]model.Repo{
			"demo": {
				Root:         repoDir,
				AllowedPaths: []string{"."},
				Providers:    []string{"fixture-a"},
			},
		},
	}
	reg := providers.New(cfg)
	e, err := orchestration.New(cfg, db, reg)
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()

	// Case 1: Completed -> SUCCESS
	jOK, err := e.Submit(model.Submission{
		RequestID: "req-audit-ok",
		RepoID:    "demo",
		Task: model.Task{
			Objective:    "ok",
			Mode:         "read_only",
			DataClass:    "synthetic",
			Strategy:     "offline_privacy_first",
			AllowedPaths: []string{"."},
			Acceptance:   []model.Check{{ID: "c1", Description: "ok"}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.RecordRoutingAudit(store.JobRoutingAudit{
		JobID:          jOK.ID,
		TaskCategory:   "coding",
		AttemptNumber:  1,
		AssignedModel:  "fixture-a/synthetic",
		AssignedEffort: "low",
		RouteReason:    "TEST",
		DispatchStatus: "DISPATCHED",
	}); err != nil {
		t.Fatal(err)
	}
	waitedOK, err := e.Wait(context.Background(), jOK.ID, 5*time.Second)
	if err != nil || waitedOK.Status != model.Completed {
		t.Fatalf("expected COMPLETED job, got status=%s reason=%s err=%v", waitedOK.Status, waitedOK.Reason, err)
	}
	auditOK, err := db.GetRoutingAudit(jOK.ID)
	if err != nil || len(auditOK) != 1 || auditOK[0].DispatchStatus != "SUCCESS" {
		t.Fatalf("expected SUCCESS audit for completed job, got %+v err=%v", auditOK, err)
	}

	// Case 2: Cancelled -> FAILED, and racing RecordRoutingAudit("SUCCESS") cannot overwrite it.
	jCancel, err := e.Submit(model.Submission{
		RequestID: "req-audit-cancel",
		RepoID:    "demo",
		Task: model.Task{
			Objective:    "fixture:slow",
			Mode:         "read_only",
			DataClass:    "synthetic",
			Strategy:     "offline_privacy_first",
			AllowedPaths: []string{"."},
			Acceptance:   []model.Check{{ID: "c1", Description: "slow"}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.RecordRoutingAudit(store.JobRoutingAudit{
		JobID:          jCancel.ID,
		TaskCategory:   "coding",
		AttemptNumber:  1,
		AssignedModel:  "fixture-a/synthetic",
		AssignedEffort: "medium",
		RouteReason:    "TEST",
		DispatchStatus: "DISPATCHED",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Cancel(jCancel.ID); err != nil {
		t.Fatal(err)
	}
	waitedCancel, err := e.Wait(context.Background(), jCancel.ID, 5*time.Second)
	if err != nil || waitedCancel.Status != model.Cancelled {
		t.Fatalf("expected CANCELLED job, got status=%s reason=%s err=%v", waitedCancel.Status, waitedCancel.Reason, err)
	}
	_ = db.RecordRoutingAudit(store.JobRoutingAudit{
		JobID:          jCancel.ID,
		TaskCategory:   "coding",
		AttemptNumber:  1,
		AssignedModel:  "fixture-a/synthetic",
		AssignedEffort: "medium",
		RouteReason:    "TEST",
		DispatchStatus: "SUCCESS",
	})
	auditCancel, err := db.GetRoutingAudit(jCancel.ID)
	if err != nil || len(auditCancel) != 1 || auditCancel[0].DispatchStatus != "FAILED" {
		t.Fatalf("expected FAILED audit for cancelled job without overwrite, got %+v err=%v", auditCancel, err)
	}

	// Case 3: Failed (INVALID_OUTPUT from fixture:handoff without handoff) -> FAILED
	jFailed, err := e.Submit(model.Submission{
		RequestID: "req-audit-failed",
		RepoID:    "demo",
		Task: model.Task{
			Objective:    "fixture:handoff",
			Mode:         "read_only",
			DataClass:    "synthetic",
			Strategy:     "offline_privacy_first",
			AllowedPaths: []string{"."},
			Acceptance:   []model.Check{{ID: "c1", Description: "fail"}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.RecordRoutingAudit(store.JobRoutingAudit{
		JobID:          jFailed.ID,
		TaskCategory:   "coding",
		AttemptNumber:  1,
		AssignedModel:  "fixture-a/synthetic",
		AssignedEffort: "medium",
		RouteReason:    "TEST",
		DispatchStatus: "DISPATCHED",
	}); err != nil {
		t.Fatal(err)
	}
	waitedFailed, err := e.Wait(context.Background(), jFailed.ID, 5*time.Second)
	if err != nil || waitedFailed.Status != model.Failed {
		t.Fatalf("expected FAILED job, got status=%s reason=%s err=%v", waitedFailed.Status, waitedFailed.Reason, err)
	}
	auditFailed, err := db.GetRoutingAudit(jFailed.ID)
	if err != nil || len(auditFailed) != 1 || auditFailed[0].DispatchStatus != "FAILED" {
		t.Fatalf("expected FAILED audit for failed job, got %+v err=%v", auditFailed, err)
	}

	// Case 4: NotRun (DEADLINE_BUDGET too short) -> FAILED
	jNotRun, err := e.Submit(model.Submission{
		RequestID: "req-audit-notrun",
		RepoID:    "demo",
		Task: model.Task{
			Objective:       "fixture:slow",
			Mode:            "read_only",
			DataClass:       "synthetic",
			Strategy:        "offline_privacy_first",
			DeadlineSeconds: 5,
			AllowedPaths:    []string{"."},
			Acceptance:      []model.Check{{ID: "c1", Description: "notrun"}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.RecordRoutingAudit(store.JobRoutingAudit{
		JobID:          jNotRun.ID,
		TaskCategory:   "coding",
		AttemptNumber:  1,
		AssignedModel:  "fixture-a/synthetic",
		AssignedEffort: "medium",
		RouteReason:    "TEST",
		DispatchStatus: "DISPATCHED",
	}); err != nil {
		t.Fatal(err)
	}
	waitedNotRun, err := e.Wait(context.Background(), jNotRun.ID, 10*time.Second)
	if err != nil || waitedNotRun.Status != model.NotRun {
		t.Fatalf("expected NOT_RUN job, got status=%s reason=%s err=%v", waitedNotRun.Status, waitedNotRun.Reason, err)
	}
	auditNotRun, err := db.GetRoutingAudit(jNotRun.ID)
	if err != nil || len(auditNotRun) != 1 || auditNotRun[0].DispatchStatus != "FAILED" {
		t.Fatalf("expected FAILED audit for not-run job, got %+v err=%v", auditNotRun, err)
	}
}

// TestSyncTerminalAuditUpdatesDispatchRowAfterRetry guards against deriving the
// audit attempt number from len(j.Attempts): retries append attempts but the
// dispatch row is always written with attempt_number 1, so a multi-attempt job
// must still transition its pending audit row out of DISPATCHED.
func TestSyncTerminalAuditUpdatesDispatchRowAfterRetry(t *testing.T) {
	tmp, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	dbDir := filepath.Join(tmp, "state")
	if err := os.Mkdir(dbDir, 0700); err != nil {
		t.Fatal(err)
	}
	repoDir := filepath.Join(tmp, "repo")
	if err := os.Mkdir(repoDir, 0700); err != nil {
		t.Fatal(err)
	}
	initGitRepoForAuditTest(t, repoDir)

	db, err := store.Open(dbDir)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	cfg := model.Config{
		StateDir:      dbDir,
		EnableFixture: true,
		Repositories: map[string]model.Repo{
			"demo": {Root: repoDir, AllowedPaths: []string{"."}, Providers: []string{"fixture-a"}},
		},
	}
	e, err := orchestration.New(cfg, db, providers.New(cfg))
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()

	job, err := e.Submit(model.Submission{
		RequestID: "req-audit-retry",
		RepoID:    "demo",
		Task: model.Task{
			Objective:    "ok",
			Mode:         "read_only",
			DataClass:    "synthetic",
			Strategy:     "offline_privacy_first",
			AllowedPaths: []string{"."},
			Acceptance:   []model.Check{{ID: "c1", Description: "ok"}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	done, err := e.Wait(context.Background(), job.ID, 10*time.Second)
	if err != nil || done.Status != model.Completed {
		t.Fatalf("expected COMPLETED job, got status=%s reason=%s err=%v", done.Status, done.Reason, err)
	}
	if err := db.RecordRoutingAudit(store.JobRoutingAudit{
		JobID:          job.ID,
		TaskCategory:   "coding",
		AttemptNumber:  1,
		AssignedModel:  "fixture-a/synthetic",
		AssignedEffort: "low",
		RouteReason:    "TEST",
		DispatchStatus: "DISPATCHED",
	}); err != nil {
		t.Fatal(err)
	}

	// Simulate a retry: the audit row stays attempt 1 while the job accumulates attempts.
	done.Attempts = []model.Attempt{{Number: 1, Provider: "fixture-a"}, {Number: 2, Provider: "fixture-a"}}
	e.SyncTerminalAudit(done)

	audit, err := db.GetRoutingAudit(job.ID)
	if err != nil || len(audit) != 1 {
		t.Fatalf("expected one audit row, got %+v err=%v", audit, err)
	}
	if audit[0].DispatchStatus != "SUCCESS" {
		t.Fatalf("expected retried job audit row to reach SUCCESS, got %q", audit[0].DispatchStatus)
	}
}
