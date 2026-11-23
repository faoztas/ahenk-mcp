package orchestration

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/faoztas/ahenk-mcp/internal/model"
	"github.com/faoztas/ahenk-mcp/internal/providers"
	"github.com/faoztas/ahenk-mcp/internal/store"
)

func TestCircuitBreakerLifecycle(t *testing.T) {
	cb := NewCircuitBreaker()
	if st := cb.State("codex"); st != CircuitClosed {
		t.Fatalf("expected initial state CLOSED, got %s", st)
	}
	if !cb.Allow("codex") {
		t.Fatalf("expected CLOSED breaker to allow requests")
	}

	// Trip with 50ms cooldown
	cb.Trip("codex", 50*time.Millisecond)
	if st := cb.State("codex"); st != CircuitOpen {
		t.Fatalf("expected state OPEN, got %s", st)
	}
	if cb.Allow("codex") {
		t.Fatalf("expected OPEN breaker to block requests")
	}

	// Wait for cooldown expiration -> HALF_OPEN
	time.Sleep(60 * time.Millisecond)
	if st := cb.State("codex"); st != CircuitHalfOpen {
		t.Fatalf("expected state HALF_OPEN after cooldown, got %s", st)
	}
	if !cb.Allow("codex") {
		t.Fatalf("expected HALF_OPEN to admit single probe")
	}

	// Record success -> CLOSED
	cb.Success("codex")
	if st := cb.State("codex"); st != CircuitClosed {
		t.Fatalf("expected state CLOSED after success, got %s", st)
	}
}

func TestCircuitBreakerProbeRelease(t *testing.T) {
	cb := NewCircuitBreaker()
	cb.Trip("codex", 10*time.Millisecond)
	time.Sleep(15 * time.Millisecond)

	// In HALF_OPEN state, first Allow admits probe
	if !cb.Allow("codex") {
		t.Fatalf("expected first probe to be admitted")
	}
	// Second Allow should be blocked while probing
	if cb.Allow("codex") {
		t.Fatalf("expected second probe to be blocked while probing is active")
	}

	// Release probe
	cb.Release("codex")

	// After release, a new probe should be admitted immediately
	if !cb.Allow("codex") {
		t.Fatalf("expected probe to be admitted after Release")
	}
}

func TestCircuitBreakerEligibleReadOnly(t *testing.T) {
	cb := NewCircuitBreaker()
	if !cb.Eligible("codex") {
		t.Fatalf("expected CLOSED breaker to be eligible")
	}

	cb.Trip("codex", 20*time.Millisecond)
	if cb.Eligible("codex") {
		t.Fatalf("expected OPEN breaker to NOT be eligible")
	}

	time.Sleep(25 * time.Millisecond)
	// In HALF_OPEN: Eligible should return true WITHOUT consuming the probe!
	if !cb.Eligible("codex") {
		t.Fatalf("expected HALF_OPEN breaker to be eligible")
	}
	if !cb.Eligible("codex") {
		t.Fatalf("subsequent Eligible call should still be true (read-only, does not consume probe)")
	}
	if st := cb.State("codex"); st != CircuitHalfOpen {
		t.Fatalf("expected state to remain HALF_OPEN, got %s", st)
	}
	if cb.IsProbing("codex") {
		t.Fatalf("expected probing to be false after Eligible checks")
	}

	// Now Allow consumes the probe
	if !cb.Allow("codex") {
		t.Fatalf("expected Allow to succeed after Eligible checks")
	}
	if !cb.IsProbing("codex") {
		t.Fatalf("expected probing to be true after Allow consumes probe")
	}
	// While probing is active, another caller's Eligible should be false
	if cb.Eligible("codex") {
		t.Fatalf("expected in-flight probing to NOT be eligible for other callers")
	}
}

func TestCircuitBreakerProbeExpiryTTL(t *testing.T) {
	cb := NewCircuitBreaker()
	cb.probeTimeout = 20 * time.Millisecond
	cb.Trip("codex", 10*time.Millisecond)
	time.Sleep(15 * time.Millisecond)

	// First probe admitted
	if !cb.Allow("codex") {
		t.Fatalf("expected first probe to be admitted")
	}
	// Immediate second probe blocked
	if cb.Allow("codex") {
		t.Fatalf("expected probe to be blocked")
	}

	// Wait for probe timeout
	time.Sleep(25 * time.Millisecond)

	// Probe expired, should admit a new probe
	if !cb.Allow("codex") {
		t.Fatalf("expected new probe to be admitted after probe timeout expiration")
	}
}

func TestCircuitBreakerActiveTrippedVisibility(t *testing.T) {
	cb := NewCircuitBreaker()
	cb.Trip("codex", 100*time.Millisecond)
	cb.Trip("agy", 10*time.Millisecond)

	tripped := cb.ActiveTripped()
	if _, ok := tripped["codex"]; !ok {
		t.Fatalf("expected codex to be in ActiveTripped during cooldown")
	}
	if _, ok := tripped["agy"]; !ok {
		t.Fatalf("expected agy to be in ActiveTripped during cooldown")
	}

	// Wait for agy cooldown to expire
	time.Sleep(15 * time.Millisecond)

	// Before Allow, agy cooldown passed, not probing yet
	tripped = cb.ActiveTripped()
	if _, ok := tripped["agy"]; ok {
		t.Fatalf("expected agy to NOT be in ActiveTripped before probe admits")
	}

	// Admit probe for agy
	if !cb.Allow("agy") {
		t.Fatalf("expected agy probe to be admitted")
	}

	// While agy is probing, it MUST be visible in ActiveTripped!
	tripped = cb.ActiveTripped()
	exp, ok := tripped["agy"]
	if !ok {
		t.Fatalf("expected probing provider agy to be visible in ActiveTripped")
	}
	if !exp.After(time.Now()) {
		t.Fatalf("expected probing expiry to be in the future, got %v", exp)
	}

	// Success removes agy from ActiveTripped
	cb.Success("agy")
	tripped = cb.ActiveTripped()
	if _, ok := tripped["agy"]; ok {
		t.Fatalf("expected agy to be removed after Success")
	}
}

func TestEngineCircuitBreakerProbeReleasedOnRouteEliminationAndFinish(t *testing.T) {
	cb := NewCircuitBreaker()
	cb.Trip("codex", 10*time.Millisecond)
	time.Sleep(15 * time.Millisecond)

	// In HALF_OPEN state, codex should be eligible and allow one probe
	if cb.State("codex") != CircuitHalfOpen {
		t.Fatalf("expected codex to be HALF_OPEN")
	}

	eng := &Engine{
		cb:       cb,
		cooldown: make(map[string]time.Time),
	}

	// 1. In finish(), if an attempt fails with non-rate-limit error, probe lock is released
	if !cb.Allow("codex") {
		t.Fatalf("expected probe to be admitted")
	}
	if !cb.IsProbing("codex") {
		t.Fatalf("expected codex to be probing")
	}

	j := model.Job{
		ID: "job-fail-test",
		Attempts: []model.Attempt{
			{Number: 1, Provider: "codex"},
		},
	}
	eng.finish(context.Background(), j, errors.New("SYNTAX_ERROR"))

	if cb.IsProbing("codex") {
		t.Fatalf("expected codex probe to be released after finish() with non-rate-limit error")
	}
}

func TestPreviewRouteDoesNotLeakHalfOpenProbeLock(t *testing.T) {
	now := time.Now().UTC()
	cb := NewCircuitBreaker()
	cb.Trip("codex", 10*time.Millisecond)
	time.Sleep(15 * time.Millisecond)
	if st := cb.State("codex"); st != CircuitHalfOpen {
		t.Fatalf("expected codex to be HALF_OPEN, got %s", st)
	}

	repo := model.Repo{
		Providers: []string{"codex", "agy"},
	}
	e := &Engine{
		cfg: model.Config{
			Repositories: map[string]model.Repo{"demo": repo},
		},
		reg: providers.Registry{
			Candidates: []model.Capability{
				{Provider: "codex", Model: "gpt-6.1-sol", Status: "available", Speed: "fast"},
				{Provider: "agy", Model: "gemini-3.8-flash-medium", Status: "available", Speed: "fast",
					Quality:  []model.QualityEvidence{{Role: "inference", Tier: 1, Source: "SYNTHETIC_EVIDENCE"}},
					Profiles: []model.ExecutionProfile{{Name: "restricted", Status: "verified"}}},
			},
		},
		jobs: map[string]model.Job{
			"job-preview": {
				ID:           "job-preview",
				RepoID:       "demo",
				PolicyDigest: model.Digest(repo),
				Status:       model.Queued,
				DeadlineAt:   now.Add(10 * time.Minute),
				Task: model.Task{
					Mode:              "read_only",
					DataClass:         "synthetic",
					TaskClass:         "utility",
					PreferredProvider: "codex",
				},
			},
		},
		active:   map[string]context.CancelFunc{},
		cooldown: map[string]time.Time{},
		quotas:   map[string][]providers.Quota{},
		cb:       cb,
	}

	// 1. Engine.Capacity must NOT acquire a HALF_OPEN probe lock
	_, _, routes := e.Capacity("demo")
	if len(routes) == 0 {
		t.Fatalf("expected Capacity to return preview routes")
	}
	if e.CircuitBreaker().IsProbing("codex") {
		t.Fatalf("Capacity() leaked HALF_OPEN probe lock for codex")
	}
	if st := e.CircuitBreaker().State("codex"); st != CircuitHalfOpen {
		t.Fatalf("expected codex to remain HALF_OPEN after Capacity(), got %s", st)
	}

	// 2. Engine.JobCapacityWarnings must NOT acquire a HALF_OPEN probe lock
	_ = e.JobCapacityWarnings("job-preview")
	if e.CircuitBreaker().IsProbing("codex") {
		t.Fatalf("JobCapacityWarnings() leaked HALF_OPEN probe lock for codex")
	}

	// 3. Engine.previewRoute directly must NOT acquire a HALF_OPEN probe lock
	e.mu.Lock()
	_ = e.previewRoute(model.Job{
		RepoID: "demo",
		Task: model.Task{
			Mode:              "read_only",
			DataClass:         "synthetic",
			TaskClass:         "utility",
			PreferredProvider: "codex",
		},
	}, now)
	e.mu.Unlock()
	if e.CircuitBreaker().IsProbing("codex") {
		t.Fatalf("previewRoute() leaked HALF_OPEN probe lock for codex")
	}

	// 4. Real execution probe MUST still be granted
	if !e.CircuitBreaker().Allow("codex") {
		t.Fatalf("expected Allow(codex) to grant real execution probe after preview calls")
	}
}

func TestRememberRateLimit_UsesRemainingDurationFromAttemptFinishedAt(t *testing.T) {
	tempDir := t.TempDir()
	db, err := store.Open(filepath.Join(tempDir, "state"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	now := time.Now().UTC()
	expiredAt := now.Add(-10 * time.Minute)
	cfg := model.Config{
		Repositories: map[string]model.Repo{
			"demo": {Providers: []string{"codex", "agy"}},
		},
	}
	reg := providers.Registry{
		Candidates: []model.Capability{
			{Provider: "codex", Model: "gpt-6.1-sol", Status: "available"},
		},
	}

	// Insert a historical job whose 1-minute cooldown expired 9 minutes ago.
	if _, err := db.Insert([]model.Job{
		{
			ID:           "expired-rate-limit",
			RepoID:       "demo",
			PolicyDigest: model.Digest(cfg.Repositories["demo"]),
			Status:       model.Failed,
			CreatedAt:    expiredAt,
			DeadlineAt:   expiredAt.Add(10 * time.Minute),
			Attempts: []model.Attempt{
				{
					Number:       1,
					Provider:     "codex",
					Model:        "gpt-6.1-sol",
					Code:         "RATE_LIMIT",
					FinishedAt:   &expiredAt,
					RetryAfterMS: 60_000,
				},
			},
		},
	}, ""); err != nil {
		t.Fatal(err)
	}

	e, err := New(cfg, db, reg)
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()

	if st := e.CircuitBreaker().State("codex"); st != CircuitClosed {
		t.Fatalf("expired historical rate limit re-tripped codex at startup: got state %s", st)
	}
	if tripped := e.CircuitBreaker().ActiveTripped(); len(tripped) != 0 {
		t.Fatalf("expected no active tripped breakers for expired historical limit, got %v", tripped)
	}
	if !e.CircuitBreaker().Allow("codex") {
		t.Fatalf("expected codex to be allowed after expired historical limit")
	}

	// Replay a fresh attempt (FinishedAt 5s ago, 60s delay): cooldown must expire at FinishedAt + 60s.
	freshAt := time.Now().UTC().Add(-5 * time.Second)
	expectedUntil := freshAt.Add(60 * time.Second)
	e.mu.Lock()
	e.rememberRateLimit(model.Attempt{
		Number:       1,
		Provider:     "antigravity",
		Code:         "RATE_LIMIT",
		FinishedAt:   &freshAt,
		RetryAfterMS: 60_000,
	})
	e.mu.Unlock()

	if st := e.CircuitBreaker().State("agy"); st != CircuitOpen {
		t.Fatalf("expected normalized agy to be OPEN for fresh rate limit, got %s", st)
	}
	tripped := e.CircuitBreaker().ActiveTripped()
	gotUntil, ok := tripped["agy"]
	if !ok {
		t.Fatalf("expected agy in ActiveTripped, got %v", tripped)
	}
	if diff := gotUntil.Sub(expectedUntil); diff < -50*time.Millisecond || diff > 50*time.Millisecond {
		t.Fatalf("expected cooldown until %v (FinishedAt+60s), got %v (diff=%v)", expectedUntil, gotUntil, diff)
	}

	// Replaying an older attempt (FinishedAt 30s ago, 60s delay -> until FinishedAt+60s < expectedUntil)
	// must NOT shrink the longer active cooldown.
	olderAt := time.Now().UTC().Add(-30 * time.Second)
	e.mu.Lock()
	e.rememberRateLimit(model.Attempt{
		Number:       1,
		Provider:     "agy",
		Code:         "RATE_LIMIT",
		FinishedAt:   &olderAt,
		RetryAfterMS: 60_000,
	})
	e.mu.Unlock()
	tripped = e.CircuitBreaker().ActiveTripped()
	if got := tripped["agy"]; got.Before(expectedUntil.Add(-50 * time.Millisecond)) {
		t.Fatalf("older rate limit shrank active cooldown: got %v, want >= %v", got, expectedUntil)
	}
}

func TestRoute_MultiModelProviderAndEarlyReturnReleasesProbe(t *testing.T) {
	now := time.Now().UTC()

	// 1. Multi-model / alias normalization in route() allowedProviders
	t.Run("mixed_alias_candidates_share_probe_and_release_properly", func(t *testing.T) {
		cb := NewCircuitBreaker()
		cb.Trip("agy", 10*time.Millisecond)
		time.Sleep(15 * time.Millisecond)

		e := &Engine{
			cfg: model.Config{
				Repositories: map[string]model.Repo{
					"demo": {Providers: []string{"antigravity", "agy"}},
				},
			},
			reg: providers.Registry{
				Candidates: []model.Capability{
					{
						Provider: "antigravity",
						Model:    "gemini-3.8-flash-medium",
						Mode:     "fixture",
						Status:   "available",
						Speed:    "fast",
						Quality:  []model.QualityEvidence{{Role: "inference", Tier: 1, Source: "SYNTHETIC_EVIDENCE"}},
					},
					{
						Provider: "agy",
						Model:    "claude-sonnet",
						Mode:     "fixture",
						Status:   "available",
						Speed:    "fast",
						Profiles: []model.ExecutionProfile{{Name: "analysis", Status: "verified"}},
						Quality:  []model.QualityEvidence{{Role: "inference", Tier: 2, Source: "SYNTHETIC_EVIDENCE"}},
					},
				},
			},
			jobs:     map[string]model.Job{},
			active:   map[string]context.CancelFunc{},
			cooldown: map[string]time.Time{},
			quotas:   map[string][]providers.Quota{},
			cb:       cb,
		}

		j := model.Job{
			ID:         "job-alias",
			RepoID:     "demo",
			DeadlineAt: now.Add(10 * time.Minute),
			Task: model.Task{
				Mode:              "read_only",
				DataClass:         "synthetic",
				PreferredProvider: "agy",
				PreferredModel:    "claude-sonnet",
			},
		}
		e.mu.Lock()
		c, _ := e.route(&j, now)
		e.mu.Unlock()
		if c.Provider != "agy" || c.Model != "claude-sonnet" {
			t.Fatalf("expected alias candidate agy/claude-sonnet to be selected without false CIRCUIT_BREAKER_OPEN, got %+v, route=%+v", c, j.Route)
		}
		if !cb.IsProbing("agy") {
			t.Fatalf("expected agy probe lock to remain held for selected candidate")
		}

		// Now reject the second alias ("agy") via ALREADY_ATTEMPTED while first alias ("antigravity") remains eligible.
		// The probe lock for "agy" MUST stay held because "antigravity" is still in options.
		cb.Release("agy")
		j2 := model.Job{
			ID:         "job-alias-keep",
			RepoID:     "demo",
			DeadlineAt: now.Add(10 * time.Minute),
			Attempts:   []model.Attempt{{Number: 1, Provider: "agy", Model: "claude-sonnet"}},
			Task: model.Task{
				Mode:      "read_only",
				DataClass: "synthetic",
			},
		}
		e.mu.Lock()
		c2, _ := e.route(&j2, now)
		e.mu.Unlock()
		if c2.Provider != "antigravity" {
			t.Fatalf("expected antigravity to be selected, got %+v", c2)
		}
		if !cb.IsProbing("agy") {
			t.Fatalf("expected agy probe lock to NOT be prematurely released while antigravity is selected")
		}
	})

	// 2. Early return in run() on a.Checkpoint != nil releases HALF_OPEN probe lock
	t.Run("checkpoint_early_return_in_run_releases_probe", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		tempDir := canonicalTempDir(t)
		sourceRepo := filepath.Join(tempDir, "source")
		if err := os.MkdirAll(sourceRepo, 0700); err != nil {
			t.Fatal(err)
		}
		_ = os.WriteFile(filepath.Join(sourceRepo, "go.mod"), []byte("module example.test/ckpt\n\ngo 1.24\n"), 0644)
		_ = os.WriteFile(filepath.Join(sourceRepo, "calc.go"), []byte("package ckpt\n\nfunc Add(a, b int) int { return a + b }\n"), 0644)
		_ = os.WriteFile(filepath.Join(sourceRepo, "calc_test.go"), []byte("package ckpt\n\nimport \"testing\"\n\nfunc TestAdd(t *testing.T) { if Add(1,2)!=3 { t.Fail() } }\n"), 0644)
		setupTestGitRepo(t, sourceRepo)

		mockCodexPath := filepath.Join(tempDir, "mock_codex")
		mockScript := `#!/bin/sh
WORKDIR=""
prev=""
for arg in "$@"; do
  if [ "$prev" = "-C" ]; then
    WORKDIR="$arg"
    break
  fi
  prev="$arg"
done
TARGET="${WORKDIR:-.}/calc.go"
cat << 'EOF' > "$TARGET"
package ckpt

func Add(a, b int) int {
	// partial edit
	return a + b
}
EOF
cat << 'EOF'
{"type":"thread.started","thread_id":"sess-ckpt"}
{"type":"item.completed","item":{"type":"agent_message","text":"Partial edit done"}}
{"type":"turn.completed","usage":{"input_tokens":10,"output_tokens":5}}
EOF
exit 0
`
		if err := os.WriteFile(mockCodexPath, []byte(mockScript), 0755); err != nil {
			t.Fatal(err)
		}

		db, err := store.Open(filepath.Join(tempDir, "state"))
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()

		cfg := model.Config{
			Repositories: map[string]model.Repo{
				"ckpt-repo": {
					Root:         sourceRepo,
					AllowedPaths: []string{"calc.go", "calc_test.go", "go.mod"},
					Providers:    []string{"codex", "agy", "opencode"},
					TrustedLocal: &model.TrustedLocalPolicy{
						WritePaths: []string{"calc.go"},
						GoChecks:   map[string]string{"check_add": "TestAdd"},
					},
				},
			},
			Providers: []model.ProviderConfig{
				{ID: "codex", Path: mockCodexPath, Model: "gpt-6.1-sol", Speed: "fast"},
			},
		}
		reg := providers.New(cfg)
		eng, err := New(cfg, db, reg)
		if err != nil {
			t.Fatal(err)
		}
		defer eng.Close()

		// Put codex into HALF_OPEN state
		eng.CircuitBreaker().Trip("codex", 10*time.Millisecond)
		time.Sleep(15 * time.Millisecond)
		if st := eng.CircuitBreaker().State("codex"); st != CircuitHalfOpen {
			t.Fatalf("expected codex to be HALF_OPEN, got %s", st)
		}

		sub := model.Submission{
			RepoID: "ckpt-repo",
			Task: model.Task{
				Objective:           "Update Add partially",
				Mode:                "patch",
				DataClass:           "synthetic",
				ExecutionProfile:    model.TrustedLocalProfile,
				CheckpointObjective: "Make partial edit to calc.go",
				AllowedPaths:        []string{"calc.go", "calc_test.go", "go.mod"},
				Acceptance:          []model.Check{{ID: "check_add", Description: "Test Add", CheckID: "check_add"}},
			},
		}
		job, err := eng.Submit(sub)
		if err != nil {
			t.Fatalf("Submit failed: %v", err)
		}
		pausedJob, err := eng.Wait(ctx, job.ID, 20*time.Second)
		if err != nil {
			t.Fatalf("Wait failed: %v", err)
		}
		if pausedJob.Status != model.Paused || pausedJob.Reason != "CHECKPOINT_AVAILABLE" {
			t.Fatalf("expected PAUSED CHECKPOINT_AVAILABLE, got %s %s", pausedJob.Status, pausedJob.Reason)
		}
		if eng.CircuitBreaker().IsProbing("codex") {
			t.Fatalf("checkpoint early return in run() leaked HALF_OPEN probe lock for codex")
		}
	})

	// 3. Multi-model provider where all models are eliminated in route() must release probe lock
	t.Run("multi_model_provider_eliminated_releases_probe", func(t *testing.T) {
		cb := NewCircuitBreaker()
		cb.Trip("codex", 10*time.Millisecond)
		time.Sleep(15 * time.Millisecond)

		e := &Engine{
			cfg: model.Config{
				Repositories: map[string]model.Repo{
					"demo": {Providers: []string{"codex", "codex-cli"}},
				},
			},
			reg: providers.Registry{
				Candidates: []model.Capability{
					{Provider: "codex", Model: "gpt-6.1-sol", Mode: "fixture", Status: "available", Speed: "fast",
						Quality: []model.QualityEvidence{{Role: "inference", Tier: 2, Source: "SYNTHETIC_EVIDENCE"}}},
					{Provider: "codex-cli", Model: "gpt-6-astra", Mode: "fixture", Status: "available", Speed: "fast",
						Quality: []model.QualityEvidence{{Role: "inference", Tier: 2, Source: "SYNTHETIC_EVIDENCE"}}},
				},
			},
			jobs:     map[string]model.Job{},
			active:   map[string]context.CancelFunc{},
			cooldown: map[string]time.Time{},
			quotas:   map[string][]providers.Quota{},
			cb:       cb,
		}
		j := model.Job{
			ID:         "job-tier-elim",
			RepoID:     "demo",
			DeadlineAt: now.Add(10 * time.Minute),
			Task: model.Task{
				Mode:       "read_only",
				DataClass:  "synthetic",
				TargetTier: "free", // eliminates tier-2 codex candidates
			},
		}
		e.mu.Lock()
		c, _ := e.route(&j, now)
		e.mu.Unlock()
		if c.Provider != "" {
			t.Fatalf("expected no route, got %+v", c)
		}
		if cb.IsProbing("codex") {
			t.Fatalf("expected codex probe lock to be released when all models are eliminated")
		}
	})

	// 4. Early return on reserveReason releases HALF_OPEN probe lock
	t.Run("reserve_reason_early_return_releases_probe", func(t *testing.T) {
		cb := NewCircuitBreaker()
		cb.Trip("agy", 10*time.Millisecond)
		time.Sleep(15 * time.Millisecond)
		if !cb.Allow("agy") {
			t.Fatalf("expected probe to be admitted")
		}
		if !cb.IsProbing("agy") {
			t.Fatalf("expected agy to be probing")
		}

		eng := &Engine{
			cb:       cb,
			cooldown: make(map[string]time.Time),
			jobs:     map[string]model.Job{},
		}
		end := time.Now().UTC()
		j := model.Job{
			ID:         "job-reserve-early",
			DeadlineAt: end.Add(10 * time.Minute),
			Attempts:   []model.Attempt{{Number: 1, Provider: "agy", Code: reserveReason, FinishedAt: &end}},
		}
		eng.mu.Lock()
		handled := eng.settleEarlyAttempt(&j, &j.Attempts[0], end)
		eng.mu.Unlock()
		if !handled {
			t.Fatalf("expected settleEarlyAttempt to handle reserveReason")
		}
		if cb.IsProbing("agy") {
			t.Fatalf("reserveReason early return leaked HALF_OPEN probe lock")
		}
		if j.Status != model.RetryWait || j.Reason != reserveReason {
			t.Fatalf("expected RetryWait with %s, got %s %s", reserveReason, j.Status, j.Reason)
		}
	})
}

func TestCircuitBreakerTripUntilAndNormalization(t *testing.T) {
	cb := NewCircuitBreaker()

	// Past or current deadlines must be a no-op
	cb.TripUntil("antigravity", time.Now().Add(-time.Second))
	if st := cb.State("agy"); st != CircuitClosed {
		t.Fatalf("expected past TripUntil to be a no-op, got %s", st)
	}
	if len(cb.ActiveTripped()) != 0 {
		t.Fatalf("expected empty ActiveTripped after past TripUntil, got %v", cb.ActiveTripped())
	}

	// Future deadline trips normalized key and clears probing
	cb.Trip("codex", 10*time.Millisecond)
	time.Sleep(15 * time.Millisecond)
	if !cb.Allow("codex-cli") {
		t.Fatalf("expected normalized codex-cli probe to be allowed")
	}
	if !cb.IsProbing("codex") {
		t.Fatalf("expected codex to be probing")
	}
	future := time.Now().Add(time.Minute)
	cb.TripUntil("codex-cli", future)
	if cb.IsProbing("codex") {
		t.Fatalf("expected TripUntil to clear probing lock")
	}
	if st := cb.State("codex"); st != CircuitOpen {
		t.Fatalf("expected codex to be OPEN after TripUntil, got %s", st)
	}
	if got := cb.ActiveTripped()["codex"]; !got.Equal(future) {
		t.Fatalf("expected ActiveTripped[codex]=%v, got %v", future, got)
	}
}
