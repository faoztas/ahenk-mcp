// Copyright 2026 Furkan Anıl Öztaş
// SPDX-License-Identifier: Apache-2.0

package readiness

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/faoztas/ahenk-mcp/internal/execution"
	"github.com/faoztas/ahenk-mcp/internal/model"
	"github.com/faoztas/ahenk-mcp/internal/providers"
	"github.com/faoztas/ahenk-mcp/internal/store"
)

func TestMissingStateDoesNotInitializeOrVerify(t *testing.T) {
	cfg := model.Config{StateDir: filepath.Join(t.TempDir(), "missing")}
	r, err := Read(t.Context(), cfg, time.Now())
	if err != nil || r.State != "MISSING" || r.Offline.Ready || r.AGY.Ready || r.OpenCode.Ready || r.MultiProvider.Ready {
		t.Fatalf("unexpected readiness: %+v %v", r, err)
	}
	if _, err := os.Stat(cfg.StateDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("readiness initialized state: %v", err)
	}
}

func TestAGYGateRejectsIncompleteEvidence(t *testing.T) {
	// These invented records exercise predicates. They are not live evidence.
	start := time.Now().Add(-time.Minute)
	end := start.Add(time.Second)
	capability := model.Capability{Provider: "agy", Status: "available", Model: "gemini-3.8-flash-medium",
		Fingerprint: "dfaf54852553d89ee8f38b54502deb379f9fe3f194c7f7ea5db05466e4c4bd59"}
	for _, tc := range []struct {
		name   string
		mutate func(*store.JobEvidence, *model.Attempt)
	}{
		{"verified", nil},
		{"fixture flag", func(j *store.JobEvidence, a *model.Attempt) {
			j.Result.Verification = append(j.Result.Verification, "FIXTURE_OUTPUT")
		}},
		{"wrong marker", func(j *store.JobEvidence, a *model.Attempt) {
			j.Task.Acceptance[0].Description = "Return exactly ahenk-ready-00000001"
		}},
		{"no completed audit", func(j *store.JobEvidence, a *model.Attempt) { j.CompletedAudited = false }},
		{"wrong model", func(j *store.JobEvidence, a *model.Attempt) { a.AGY.Model = "wrong" }},
		{"no startup", func(j *store.JobEvidence, a *model.Attempt) { a.AGY.StartupVerified = false }},
		{"no terminal", func(j *store.JobEvidence, a *model.Attempt) { a.AGY.TerminalVerified = false }},
		{"zero usage", func(j *store.JobEvidence, a *model.Attempt) { a.AGY.TotalTokens = 0 }},
		{"no quota evidence", func(j *store.JobEvidence, a *model.Attempt) { a.AGY.SubscriptionQuotaVerified = false }},
		{"unknown cost policy", func(j *store.JobEvidence, a *model.Attempt) { a.Cost = nil }},
		{"soft denial", func(j *store.JobEvidence, a *model.Attempt) { a.Code = "PERMISSION_DENIED" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			marker := "ahenk-ready-12345678"
			cost := &model.CostEvidence{Policy: "SUBSCRIPTION_ONLY", Status: "DOCUMENTED_COST_PROTECTION", Documented: "VERIFIED",
				RuntimeVerified: "UNKNOWN", Account: "SAME_NATIVE_CLI_CONTEXT"}
			a := model.Attempt{Number: 1, Provider: "agy", Model: capability.Model, Fingerprint: capability.Fingerprint,
				StartedAt: start, FinishedAt: &end, Cost: cost, AGY: &model.AGYRunEvidence{Model: capability.Model,
					PermissionMode: "request-review", PromptSent: true, StartupVerified: true, TerminalVerified: true,
					SubscriptionQuotaVerified: true, TotalTokens: 1, Cost: cost}}
			j := store.JobEvidence{RunningAudited: true, CompletedAudited: true, Job: model.Job{
				ID: "job", Status: model.Completed, SnapshotID: model.Hash([]byte("snapshot")), DeadlineAt: end.Add(time.Second),
				Task: model.Task{DataClass: "synthetic", Mode: "read_only",
					Acceptance: []model.Check{{ID: "readiness", Description: "Return exactly " + marker}}},
				Result: &model.Result{Summary: marker, SummarySHA256: model.Hash([]byte(marker)), Cost: cost,
					Verification: []string{"SCOPED_CONTEXT", "PROCESS_EXIT", "AGY_PROFILE_VERIFIED", "DOCUMENTED_COST_PROTECTION"}},
			}}
			if tc.mutate != nil {
				tc.mutate(&j, &a)
			}
			j.Attempts = []model.Attempt{a}
			got := liveGate(capability, map[string]store.JobEvidence{"job": j})
			if got.Ready != (tc.mutate == nil) {
				t.Fatalf("unexpected gate: %+v", got)
			}
		})
	}
}

func TestOpenCodeLegacyGateExpiresWithoutProvingLiveReadiness(t *testing.T) {
	now := time.Now()
	finished := now.Add(-time.Minute)
	j := store.JobEvidence{Job: model.Job{
		ID: "previous-policy", Attempts: []model.Attempt{
			{Provider: "opencode", Code: "RATE_LIMIT", FinishedAt: &finished, RetryAfterMS: 120_000},
		},
	}}
	cfg := model.Config{}
	reg := providers.New(cfg)
	report := evaluate(cfg, reg, "", []store.JobEvidence{j}, nil, now)
	if report.OpenCode.Ready || report.OpenCodeCapacity != "UNKNOWN" || report.OpenCodeRecovery != "RECORDED_GATE_ACTIVE" ||
		report.OpenCodeFreeGate == nil ||
		report.OpenCodeFreeGate.Scope != "model" || report.OpenCodeFreeGate.Reason != "RATE_LIMIT" ||
		!report.OpenCodeFreeGate.RetryAt.Equal(now.Add(time.Minute)) {
		t.Fatalf("missing legacy Free gate: %+v", report)
	}
	report = evaluate(cfg, reg, "", []store.JobEvidence{j}, nil, now.Add(time.Minute))
	if report.OpenCode.Ready || report.OpenCodeFreeGate != nil || report.OpenCodeCapacity != "UNKNOWN" ||
		report.OpenCodeRecovery != "RECORDED_GATE_ELAPSED" {
		t.Fatalf("expired gate became availability: %+v", report)
	}
}

func TestImportedOpenCodeRecoveryKeepsOriginAndDoesNotClaimSuccess(t *testing.T) {
	now := time.Now()
	finished := now.Add(-time.Minute)
	const cooledModel = "opencode/deepseek-v4.1-flash-free"
	records := []store.OpenCodeCooldownImport{{JobID: "historical-job", Attempt: 1, Model: cooledModel, FinishedAt: finished, RetryAfterMS: 120_000}}
	reg := providers.Registry{Candidates: []model.Capability{{Provider: "opencode", Model: cooledModel}}}
	r := evaluate(model.Config{}, reg, "", nil, records, now)
	if r.OpenCode.Ready || r.OpenCodeRecovery != "RECORDED_GATE_ACTIVE" || r.OpenCodeRecoveryGate == nil ||
		r.OpenCodeRecoveryGate.Origin != "verified_import" || r.OpenCodeRecoveryGate.JobID != "historical-job" ||
		!r.OpenCodeRecoveryGate.RetryAt.Equal(now.Add(time.Minute)) {
		t.Fatalf("historical import lost its provenance or claimed success: %+v", r)
	}
	reg.Candidates[0].Model = "opencode/longcat-2.5-free"
	r = evaluate(model.Config{}, reg, "", nil, records, now)
	if r.OpenCodeFreeGate != nil || r.OpenCodeRecovery != "RECORDED_GATE_OTHER_MODEL" || r.OpenCodeRecoveryGate.Model != cooledModel {
		t.Fatalf("imported model-only gate blocked another route: %+v", r)
	}
	records[0].ProviderRateLimitScope = "pool"
	r = evaluate(model.Config{}, reg, "", nil, records, now)
	if r.OpenCodeFreeGate == nil || r.OpenCodeFreeGate.ProviderScope != "pool" || r.OpenCodeRecovery != "RECORDED_GATE_ACTIVE" {
		t.Fatalf("imported provider-asserted pool gate did not block alternate: %+v", r)
	}
}

func TestOpenCodeGateRequiresExactFreeProof(t *testing.T) {
	const marker = "ahenk-ready-12345678"
	start := time.Now().Add(-time.Minute)
	end := start.Add(time.Second)
	c := model.Capability{Provider: "opencode", Status: "available", Model: "opencode/deepseek-v4.1-flash-free",
		Fingerprint: "bfd1f79c2fb005246c56d0e9fb472206c22782d065872d71c26f01f3eb3a0a82"}
	cost := &model.CostEvidence{Policy: "FREE_ONLY", Status: "ZERO_PRICE_CATALOG", Documented: "VERIFIED",
		RuntimeVerified: "VERIFIED", Account: "ANONYMOUS"}
	a := model.Attempt{Number: 1, Provider: c.Provider, Model: c.Model, Fingerprint: c.Fingerprint,
		StartedAt: start, FinishedAt: &end, Cost: cost}
	j := store.JobEvidence{RunningAudited: true, CompletedAudited: true, Job: model.Job{
		ID: "job", Status: model.Completed, SnapshotID: model.Hash([]byte("snapshot")), DeadlineAt: end.Add(time.Second),
		Task: model.Task{DataClass: "synthetic", Mode: "read_only",
			Acceptance: []model.Check{{ID: "readiness", Description: "Return exactly " + marker}}},
		Result: &model.Result{Summary: marker, SummarySHA256: model.Hash([]byte(marker)), Cost: cost,
			Verification: []string{"SCOPED_CONTEXT", "PROCESS_EXIT", "TOOL_FREE_TEXT", "ZERO_PRICE_CATALOG"}},
	}}
	j.Attempts = []model.Attempt{a}
	if !liveGate(c, map[string]store.JobEvidence{j.ID: j}).Ready {
		t.Fatal("valid simulated Free proof was rejected")
	}
	j.Attempts[0].Cost = nil
	if liveGate(c, map[string]store.JobEvidence{j.ID: j}).Ready {
		t.Fatal("missing catalog evidence was accepted")
	}
	j.Attempts[0].Cost = cost
	c.Model = "different-model"
	if liveGate(c, map[string]store.JobEvidence{j.ID: j}).Ready {
		t.Fatal("different model was accepted")
	}

	c.Model, c.OpenCodeTransport = a.Model, model.OpenCodeNativeReadAsk
	if liveGate(c, map[string]store.JobEvidence{j.ID: j}).Ready {
		t.Fatal("legacy stateless evidence was accepted for the native transport")
	}
	j.Attempts[0].OpenCodeTransport = model.OpenCodeNativeReadAsk
	if liveGate(c, map[string]store.JobEvidence{j.ID: j}).Ready {
		t.Fatal("native transport without terminal evidence was accepted")
	}
	zero := 0.0
	j.Attempts[0].OpenCode = &model.OpenCodeRunEvidence{Model: c.Model, PromptSent: true, GuardVerified: true,
		TerminalVerified: true, NativeFinish: "stop", InputTokens: 40, OutputTokens: 7, ReportedCost: &zero}
	staleDispatch := store.JobEvidence{Job: model.Job{ID: "legacy-dispatch", Status: model.Blocked, CreatedAt: end,
		Task: model.Task{DataClass: "synthetic", Mode: "read_only", PreferredProvider: c.Provider, PreferredModel: c.Model}}}
	jobs := map[string]store.JobEvidence{j.ID: j, staleDispatch.ID: staleDispatch}
	gate := liveGate(c, jobs)
	if !gate.Ready || gate.Observation.OpenCode == nil || gate.Observation.OpenCodeTransport != model.OpenCodeNativeReadAsk {
		t.Fatalf("native terminal evidence or transport was lost: %+v", gate)
	}
	zero = 0.01
	if liveGate(c, jobs).Ready {
		t.Fatal("native run with nonzero reported cost was accepted")
	}
}

func TestOpenCodeModelCooldownDoesNotMaskAlternateSmoke(t *testing.T) {
	const marker = "ahenk-ready-12345678"
	const deepSeek = "opencode/deepseek-v4.1-flash-free"
	const longCat = "opencode/longcat-2.5-free"
	const fingerprint = "bfd1f79c2fb005246c56d0e9fb472206c22782d065872d71c26f01f3eb3a0a82"
	now := time.Now()
	rejected, retryAt := now.Add(-100*time.Millisecond), now.Add(time.Hour)
	start, finish := now.Add(-10*time.Second), now.Add(-time.Second)
	repo := model.Repo{AllowedPaths: []string{m1Path}, Providers: []string{"opencode"}}
	cfg := model.Config{Repositories: map[string]model.Repo{"opencode-smoke": repo}}
	cost := &model.CostEvidence{Policy: "FREE_ONLY", Status: "ZERO_PRICE_CATALOG", Documented: "VERIFIED", RuntimeVerified: "VERIFIED", Account: "ANONYMOUS"}
	for _, tc := range []struct {
		name, selected, upstreamScope       string
		includeSmoke, blocked, ready, multi bool
	}{
		{name: "legacy inferred pool still blocks its own model", selected: deepSeek, includeSmoke: true, blocked: true},
		{name: "alternate is not blocked but needs smoke", selected: longCat},
		{name: "exact alternate smoke is independently live", selected: longCat, includeSmoke: true, ready: true},
		{name: "configured blocked sibling cannot mask healthy smoke", selected: longCat, includeSmoke: true, ready: true, multi: true},
		{name: "upstream pool blocks alternate", selected: longCat, upstreamScope: "pool", includeSmoke: true, blocked: true},
		{name: "upstream account blocks alternate", selected: longCat, upstreamScope: "account", includeSmoke: true, blocked: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := model.Capability{Provider: "opencode", Status: "available", Model: tc.selected, Fingerprint: fingerprint}
			failed := store.JobEvidence{RunningAudited: true, Job: model.Job{
				ID: "rejected-model", RepoID: "opencode-smoke", PolicyDigest: model.Digest(repo), Status: model.Blocked,
				Task: model.Task{DataClass: "synthetic", Mode: "read_only"},
				Attempts: []model.Attempt{{Number: 1, Provider: "opencode", Model: deepSeek, Fingerprint: fingerprint,
					Code: "RATE_LIMIT", StartedAt: rejected.Add(-time.Second), FinishedAt: &rejected,
					RetryAt: &retryAt, HealthScope: "pool", ProviderRateLimitScope: tc.upstreamScope}},
			}}
			jobs := []store.JobEvidence{failed}
			if tc.includeSmoke {
				jobs = append(jobs, store.JobEvidence{RunningAudited: true, CompletedAudited: true, Job: model.Job{
					ID: "exact-alternate", RepoID: "opencode-smoke", PolicyDigest: model.Digest(repo), Status: model.Completed,
					SnapshotID: model.Hash([]byte("snapshot")), DeadlineAt: now,
					Task: model.Task{DataClass: "synthetic", Mode: "read_only",
						Acceptance: []model.Check{{ID: "readiness", Description: "Return exactly " + marker}}},
					Result: &model.Result{Summary: marker, SummarySHA256: model.Hash([]byte(marker)), Cost: cost,
						Verification: []string{"SCOPED_CONTEXT", "PROCESS_EXIT", "TOOL_FREE_TEXT", "ZERO_PRICE_CATALOG"}},
					Attempts: []model.Attempt{{Number: 1, Provider: "opencode", Model: longCat, Fingerprint: fingerprint,
						StartedAt: start, FinishedAt: &finish, Cost: cost}},
				}})
			}
			reg := providers.Registry{Candidates: []model.Capability{c}}
			if tc.multi {
				sibling := c
				sibling.Model = deepSeek
				reg.Candidates = append(reg.Candidates, sibling)
			}
			r := evaluate(cfg, reg, "", jobs, nil, now)
			if tc.multi && (len(r.OpenCodeModels) != 2 || r.OpenCodeModels[0].Model != deepSeek ||
				r.OpenCodeModels[0].ActiveGate == nil || r.OpenCodeModels[1].Model != longCat || r.OpenCodeModels[1].ActiveGate != nil) {
				t.Fatalf("readiness collapsed independently configured models: %+v", r.OpenCodeModels)
			}
			if tc.upstreamScope != "" && (len(r.OpenCodeSharedGates) != 1 || r.OpenCodeSharedGates[0].Scope != tc.upstreamScope) {
				t.Fatalf("readiness did not separately report the explicit shared gate: %+v", r.OpenCodeSharedGates)
			}
			if (r.OpenCodeFreeGate != nil) != tc.blocked || r.OpenCode.Ready != tc.ready || r.MultiProvider.Ready {
				t.Fatalf("unexpected readiness: %+v", r)
			}
			gate := r.OpenCodeRecoveryGate
			if gate == nil || gate.RecordedScope != "pool" || gate.Model != deepSeek || !gate.RetryAt.Equal(retryAt) {
				t.Fatalf("lost old rate-limit provenance: %+v", gate)
			}
			if !tc.blocked && (r.OpenCodeRecovery != "RECORDED_GATE_OTHER_MODEL" || gate.ProviderScope != "UNKNOWN" || gate.Scope != "model") {
				t.Fatalf("inferred scope blocked another model: %+v", r)
			}
			if tc.ready && r.OpenCode.Observation.Model != longCat {
				t.Fatalf("readiness proved the wrong exact route: %+v", r.OpenCode)
			}
		})
	}
}

func TestBatchReceiverMarkerMustComeFromDiff(t *testing.T) {
	const marker = "ahenk-handoff-abc12345"
	j := model.Job{Task: model.Task{Objective: "Return the value added by the diff",
		Acceptance: []model.Check{{ID: "received", Description: "Return the value in the supplied diff"}}},
		Result: &model.Result{Summary: marker}}
	s := execution.Snapshot{}
	if !markerOnlyInDiff(s, j, model.HandoffManifest{}) {
		t.Fatal("receiver has no alternative source for the marker")
	}
	j.Task.Acceptance[0].Description = "Return " + marker
	if markerOnlyInDiff(s, j, model.HandoffManifest{}) {
		t.Fatal("expected answer was disclosed in the receiver acceptance text")
	}
	j.Task.Acceptance[0].Description = "Return the value in the supplied diff"
	if markerOnlyInDiff(s, j, model.HandoffManifest{AcceptanceIDs: []string{marker}}) {
		t.Fatal("expected answer was disclosed in the handoff metadata")
	}
}

func TestReadinessDoesNotResurrectSupersededFreeGate(t *testing.T) {
	now := time.Date(2026, 11, 10, 18, 0, 0, 0, time.UTC)
	old := now.Add(-2 * time.Minute)
	start := now.Add(-time.Minute)
	const id = "opencode/glm-5.3-flash-free"
	const pin = "bfd1f79c2fb005246c56d0e9fb472206c22782d065872d71c26f01f3eb3a0a82"
	c := model.Capability{Provider: "opencode", Model: id, Fingerprint: pin, Status: "available", OpenCodeTransport: model.OpenCodeNativeReadAsk}
	zero := 0.0
	success := model.Attempt{Provider: "opencode", Model: id, Fingerprint: pin, OpenCodeTransport: model.OpenCodeNativeReadAsk,
		StartedAt: start, FinishedAt: &now, OpenCode: &model.OpenCodeRunEvidence{
			GuardVerified: true, TerminalVerified: true, PromptSent: true, Model: id, ReportedCost: &zero,
		}}
	imported := store.OpenCodeCooldownImport{Model: id, Fingerprint: pin, OpenCodeTransport: model.OpenCodeNativeReadAsk,
		FinishedAt: old, RetryAfterMS: 3_600_000}
	jobs := []store.JobEvidence{{Job: model.Job{Attempts: []model.Attempt{success}}}}
	gates := openCodeGateHistory(jobs, []store.OpenCodeCooldownImport{imported})
	previous := openCodeModelReport(c, Gate{}, gates, start)
	if previous.ActiveGate == nil {
		t.Fatal("future success changed historical readiness")
	}
	current := openCodeModelReport(c, Gate{Ready: true}, gates, now)
	if current.ActiveGate != nil || current.Recovery != "RECORDED_GATE_SUPERSEDED" || !current.Readiness.Ready {
		t.Fatalf("stale gate returned: %+v", current)
	}
}
