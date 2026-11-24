// Copyright 2026 Furkan Anıl Öztaş
// SPDX-License-Identifier: Apache-2.0

package orchestration

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"math"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/faoztas/ahenk-mcp/internal/model"
	"github.com/faoztas/ahenk-mcp/internal/providers"
	"github.com/faoztas/ahenk-mcp/internal/store"
)

func TestPerfectProfileScoreSurvivesRoutingEvidence(t *testing.T) {
	now := capacityClock
	e, j := profileFixture(now)
	c := e.reg.Candidates[2]
	e.jobs = map[string]model.Job{"verified": {Status: model.Completed, Attempts: []model.Attempt{{Provider: c.Provider, Model: c.Model, Fingerprint: c.Fingerprint, OpenCodeAccount: c.OpenCodeAccount, OpenCodeTransport: c.OpenCodeTransport, FinishedAt: &now, OpenCode: &model.OpenCodeRunEvidence{TerminalVerified: true}}}}}
	j.RoutingProfile = model.RoutingProfilePricePerformance
	full := 100.0
	r := e.rankProfile(&j, []candidate{{capability: c, estimate: time.Second, decision: model.Decision{QualityTier: 2, QualitySource: "LIVE", QuotaRemaining: &full}}}, now)
	score := r[0].decision.ProfileScore
	j.InitialRoute = &model.InitialRoute{ProfileScore: score}
	projected := store.ProjectRoutingEvidence(j, nil).FirstDecision.ProfileScore
	if score != 1 || projected != 1 {
		t.Fatalf("perfect score %0.17g projected %g", score, projected)
	}
}

func TestAGYFiveDayHeadroomAndReserve(t *testing.T) {
	now := capacityClock
	e, j := profileFixture(now)
	const family = "claude_gpt"
	for _, tc := range []struct {
		bucket  string
		minutes int
	}{{providers.BarAgyOpus, 7200}, {"3p-weekly", 10080}} {
		short := quotaFixture("agy", providers.BarAgyOpus5h, family, 300, 80, now)
		long := quotaFixture("agy", tc.bucket, family, tc.minutes, 3, now)
		e.quotas["agy"] = []providers.Quota{short, long}
		h, ok := e.familyHeadroom(family, now)
		r := e.agyReserve(j, family, now, time.Minute, providers.Quota{}, true)
		if !ok || math.Abs(h.ProviderMargin-.338) > 1e-9 || r.reason != reserveReason {
			t.Errorf("%s blend %g reserve %s", tc.bucket, h.ProviderMargin, r.reason)
		}
		e.quotas["agy"] = []providers.Quota{long}
		if h, ok := e.familyHeadroom(family, now); !ok || h.State != headroomCritical {
			t.Errorf("%s long-only %+v", tc.bucket, h)
		}
		if _, ok := e.familyHeadroom(family, now.Add(time.Hour)); ok {
			t.Fatal("stale headroom accepted")
		}
		e.quotas["agy"][0].AccountMatch = "MISMATCH"
		if _, ok := e.familyHeadroom(family, now); ok {
			t.Fatal("unmatched headroom accepted")
		}
	}
}

func TestAGYFiveDayReserveReconstruction(t *testing.T) {
	now := capacityClock
	e, j := profileFixture(now)
	reset := now.Add(time.Hour)
	for _, bucket := range []string{"3p-weekly", providers.BarAgyOpus, "gemini-weekly"} {
		ev := &model.AGYReserveEvidence{Bucket: bucket, ObservedAt: now, ResetAt: &reset, Remaining: 12}
		e.jobs = map[string]model.Job{"native": {Attempts: []model.Attempt{{Provider: "agy", Model: "claude-fixture", AGY: &model.AGYRunEvidence{WeeklyReserve: ev, ReserveAccountSHA256: e.cfg.CodexBarAccountSHA256["agy"]}}}}}
		for _, matched := range []bool{true, false} {
			r := e.agyReserve(j, "claude_gpt", now, time.Minute, providers.Quota{}, matched)
			if r.weekly.WindowMinutes != 7200 || (r.reason == reserveReason) != (matched && bucket != "gemini-weekly") {
				t.Errorf("reconstruction %s matched=%t: %+v", bucket, matched, r)
			}
		}
		if r := e.agyReserve(j, "claude_gpt", now.Add(6*time.Minute), time.Minute, providers.Quota{}, true); r.reason != "" {
			t.Fatal("stale reconstruction", r)
		}
	}
}

func TestAGYReserveBindingAcrossRestart(t *testing.T) {
	for _, previous := range []string{"previous", "current", "", "legacy-configured"} {
		t.Run("binding-"+previous, func(t *testing.T) {
			e, _ := setupTestEngine(t)
			now := time.Now().UTC()
			reset := now.Add(time.Hour)
			ev := model.AGYRunEvidence{WeeklyReserve: &model.AGYReserveEvidence{Bucket: "3p-weekly", Remaining: 12, ObservedAt: now, ResetAt: &reset}}
			if previous == "legacy-configured" {
				// The previous producer populated this field from configuration alone.
				if err := json.Unmarshal([]byte(`{"reserve_account_sha256":"`+model.Hash([]byte("current"))+`"}`), &ev); err != nil {
					t.Fatal(err)
				}
			} else if previous != "" {
				ev.ReserveAccountSHA256 = model.Hash([]byte(previous))
			}
			insertTestJob(t, e, model.Job{ID: "native", CreatedAt: now, Status: model.Failed, Attempts: []model.Attempt{{Provider: "agy", Model: "claude-fixture", AGY: &ev}}})
			if err := e.Close(); err != nil {
				t.Fatal(err)
			}
			cfg := e.cfg
			cfg.CodexBarAccountSHA256 = map[string]string{"agy": model.Hash([]byte("current"))}
			re, err := New(cfg, e.db, e.reg)
			if err != nil {
				t.Fatal(err)
			}
			defer re.Close()
			re.mu.Lock()
			defer re.mu.Unlock()
			re.quotas["agy"] = []providers.Quota{quotaFixture("agy", providers.BarAgyOpus, "claude_gpt", 7200, 80, now.Add(-time.Second))}
			c := model.Capability{Provider: "agy", AttestedAccount: "current"}
			r := re.agyReserve(model.Job{}, "claude_gpt", now, time.Minute, providers.Quota{}, re.quotaAccountMatches(c))
			want := 80.0
			if previous == "current" {
				want = 12
			}
			if r.weekly.Remaining == nil || *r.weekly.Remaining != want {
				t.Fatalf("selected %s quota, want %.0f%%", r.weekly.Source, want)
			}
		})
	}
}

func TestNoRoutePreservesPriceAndCriticalHeadroomReasons(t *testing.T) {
	empty := model.Job{}
	finishWithoutRoute(&empty)
	if empty.Status != model.NotRun || empty.Reason != "NO_ELIGIBLE_ROUTE" || len(empty.Attempts) != 0 {
		t.Fatalf("empty candidate pool: %s %s", empty.Status, empty.Reason)
	}
	now := time.Now().UTC()
	for _, reason := range []string{"PRICE_EVIDENCE_REQUIRED", "QUOTA_CRITICAL_HEADROOM"} {
		j := model.Job{DeadlineAt: now.Add(time.Hour), Route: []model.Decision{{Reason: reason}}}
		finishWithoutRoute(&j)
		if j.Status != model.Blocked || j.Reason != reason || store.ProjectRoutingEvidence(j, nil).ReasonCode != reason || len(j.Attempts) != 0 {
			t.Errorf("%s: %s %s", reason, j.Status, j.Reason)
		}
		retry := now.Add(time.Minute)
		j.Route[0].RetryAt = &retry
		finishWithoutRoute(&j)
		if j.Status != model.RetryWait || !j.ReadyAt.Equal(retry) {
			t.Fatal("lost retry", j)
		}
		j.Route = append([]model.Decision{{Reason: "QUALITY_EVIDENCE_REQUIRED"}}, j.Route...)
		j.Route[1].RetryAt = nil
		finishWithoutRoute(&j)
		if j.Reason != "QUALITY_EVIDENCE_REQUIRED" {
			t.Fatal("quality gate precedence", j)
		}
		j.Attempts = []model.Attempt{{Code: "INVALID_OUTPUT"}}
		finishWithoutRoute(&j)
		if j.Status != model.Failed || j.Reason != "INVALID_OUTPUT" {
			t.Fatal("lost worker failure", j)
		}
	}
}

func breakerFixture(t *testing.T) (*Engine, model.Attempt, model.Attempt) {
	t.Helper()
	e, _ := setupTestEngine(t)
	now := time.Now().UTC()
	earlier := now.Add(-time.Minute)
	after := now.Add(time.Second)
	zero := 0.
	c := model.Capability{Provider: "opencode", Model: "opencode/beam-free", Fingerprint: "bfd1f79c2fb005246c56d0e9fb472206c22782d065872d71c26f01f3eb3a0a82"}
	a := model.Attempt{Number: 1, Provider: c.Provider, Model: c.Model, Fingerprint: c.Fingerprint, StartedAt: earlier, FinishedAt: &now, Code: "RATE_LIMIT", ProviderRateLimitScope: "pool", RetryAfterMS: 120000}
	success := a
	success.Code = ""
	success.FinishedAt = &after
	success.OpenCode = &model.OpenCodeRunEvidence{GuardVerified: true, TerminalVerified: true, PromptSent: true, Model: c.Model, ReportedCost: &zero}
	if !OpenCodeVerifiedSuccess(success) {
		t.Fatal("invalid success fixture")
	}
	return e, a, success
}

// Commit a finished observation using the same settlement and SQLite save as dispatch.
func commitBreakerFixture(e *Engine, j model.Job) error {
	a := &j.Attempts[0]
	e.openCodeHistory.Apply(a)
	e.persistFinalAttempt(j, nil)
	return e.fatal
}

func breakerSQL(t *testing.T, e *Engine) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(e.db.Dir, "state.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	var v int
	if err := db.QueryRow("PRAGMA user_version").Scan(&v); err != nil || v != 2 {
		t.Fatal("schema version", v, err)
	}
	return db
}

func TestOpenCodeBreakerReplayPreservesAcceptedTransitions(t *testing.T) {
	for _, scenario := range []string{"ordinary overlap", "accepted recovery", "late rejected 429", "legacy 429"} {
		t.Run(scenario, func(t *testing.T) {
			e, trip, success := breakerFixture(t)
			add := func(id string, a model.Attempt, legacy bool) {
				j := model.Job{ID: id, RepoID: "test-repo", Status: model.Failed, CreatedAt: time.Now(), Attempts: []model.Attempt{a}}
				if legacy {
					insertTestJob(t, e, j)
					return
				}
				intent := j
				intent.Attempts = nil
				insertTestJob(t, e, intent)
				e.mu.Lock()
				err := commitBreakerFixture(e, j)
				e.mu.Unlock()
				if err != nil {
					t.Fatal(err)
				}
			}
			want := CircuitClosed
			switch scenario {
			case "ordinary overlap":
				add("trip", trip, false)
				add("unowned-success", success, false)
				add("legacy-success", success, true)
				want = CircuitOpen
			case "accepted recovery", "late rejected 429":
				add("trip", trip, false)
				e.cb.Trip("opencode", -time.Second)
				probe, ok := e.cb.Acquire("opencode")
				if !ok || probe == 0 {
					t.Fatal("missing owned recovery")
				}
				success.ProbeID = probe
				// A wall-clock change cannot reverse the order of committed decisions.
				tm := trip.FinishedAt.Add(-time.Second)
				success.FinishedAt = &tm
				add("recovery", success, false)
				if scenario == "late rejected 429" {
					trip.ProbeID = probe
					add("late", trip, false)
				}
			case "legacy 429":
				add("legacy", trip, true)
			}
			if got := e.cb.State("opencode"); got != want {
				t.Fatal("live", got, want)
			}
			if err := e.Close(); err != nil {
				t.Fatal(err)
			}
			re, err := New(e.cfg, e.db, e.reg)
			if err != nil {
				t.Fatal(err)
			}
			defer re.Close()
			breakerSQL(t, re)
			if got := re.cb.State("opencode"); got != want {
				t.Errorf("replay %s, live %s", got, want)
			}
			re.mu.Lock()
			scope, _ := re.openCodeGate(model.Capability{Provider: "opencode", Model: trip.Model}, time.Now())
			re.mu.Unlock()
			if scope != "pool" {
				t.Fatalf("active scoped evidence lost: %q", scope)
			}
		})
	}
}

func TestOpenCodeBreakerSurvivesCollection(t *testing.T) {
	for _, recovered := range []bool{false, true} {
		name := "expired trip"
		if recovered {
			name = "pruned recovery"
		}
		t.Run(name, func(t *testing.T) {
			e, trip, success := breakerFixture(t)
			now := time.Now().UTC()
			old := now.Add(-31 * 24 * time.Hour)
			expired := now.Add(-time.Minute)
			trip.OpenCodeCircuit = &model.OpenCodeCircuitDecision{Sequence: 1, Until: &expired}
			created := old
			pruned := "trip"
			want := CircuitHalfOpen
			if recovered {
				created = now
				pruned = "recovery"
				want = CircuitClosed
			}
			insertTestJob(t, e, model.Job{ID: "trip", CreatedAt: created, Status: model.Failed, Attempts: []model.Attempt{trip}})
			if recovered {
				success.OpenCodeCircuit = &model.OpenCodeCircuitDecision{Sequence: 2}
				insertTestJob(t, e, model.Job{ID: "recovery", CreatedAt: old, Status: model.Completed, Attempts: []model.Attempt{success}})
			}
			e.mu.Lock()
			e.collect(now)
			_, exists := e.jobs[pruned]
			e.mu.Unlock()
			if exists {
				t.Fatal("terminal job survived collection")
			}
			if err := e.Close(); err != nil {
				t.Fatal(err)
			}
			re, err := New(e.cfg, e.db, e.reg)
			if err != nil {
				t.Fatal(err)
			}
			defer re.Close()
			breakerSQL(t, re)
			if got := re.cb.State("opencode"); got != want {
				t.Fatalf("after collection and restart: got %s want %s", got, want)
			}
		})
	}
}

func TestOpenCodeBreakerReplayAtomicPersistence(t *testing.T) {
	e, trip, _ := breakerFixture(t)
	db := breakerSQL(t, e)
	j := model.Job{ID: "rollback", RepoID: "test-repo", Status: model.Failed, CreatedAt: time.Now()}
	insertTestJob(t, e, j)
	if _, err := db.Exec("CREATE TRIGGER fail_attempt BEFORE INSERT ON audit BEGIN SELECT RAISE(ABORT,'fixture rollback'); END"); err != nil {
		t.Fatal(err)
	}
	j.Attempts = []model.Attempt{trip}
	e.mu.Lock()
	err := commitBreakerFixture(e, j)
	e.mu.Unlock()
	if err == nil {
		t.Fatal("expected rollback")
	}
	js, err := e.db.All()
	if err != nil || len(js) != 1 || len(js[0].Attempts) != 0 {
		t.Fatal("partial commit", err)
	}
	if e.cb.State("opencode") != CircuitClosed {
		t.Error("rolled-back trip became observable")
	}
	re, err := New(e.cfg, e.db, e.reg)
	if err != nil {
		t.Fatal(err)
	}
	defer re.Close()
	if re.cb.State("opencode") != CircuitClosed {
		t.Fatal("rolled-back trip survived restart")
	}
	breakerSQL(t, re)
}

// Pause the test driver at the first acceptance check after the worker has
// returned and the scheduler has committed VERIFYING.
type acceptanceContext struct {
	context.Context
	check func()
}

func (c acceptanceContext) Err() error {
	c.check()
	return c.Context.Err()
}

// deadlineBeforeNotification models the interval between reaching a deadline
// and delivery of the timer's cancellation. A worker can return in this interval.
type deadlineBeforeNotification struct {
	context.Context
	at time.Time
}

func (c deadlineBeforeNotification) Deadline() (time.Time, bool) { return c.at, true }

func TestAttemptFinalizationCancellationAndDeadlineAudit(t *testing.T) {
	for _, scenario := range []string{"cancel", "deadline", "success", "worker error and cancel", "attempt deadline", "attempt expires during verification"} {
		t.Run(scenario, func(t *testing.T) {
			e, repoDir := setupTestEngine(t)
			e.stop()
			e.wg.Wait()
			e.ctx, e.stop = context.WithCancel(context.Background())
			dir := canonicalTempDir(t)
			bin := filepath.Join(dir, "claude")
			started := filepath.Join(dir, "started")
			release := filepath.Join(dir, "release")
			script := "#!/bin/sh\ntouch '" + started + "'\nwhile [ ! -e '" + release + "' ]; do sleep .01; done\n"
			if scenario == "worker error and cancel" {
				script += "printf '%s\\n' invalid\n"
			} else {
				script += "printf '%s\\n' '{\"session_id\":\"fixture\",\"result\":\"done\",\"subtype\":\"success\",\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}'\n"
			}
			if err := os.WriteFile(bin, []byte(script), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(repoDir, "brief.txt"), []byte("fixture"), 0600); err != nil {
				t.Fatal(err)
			}
			setupTestGitRepo(t, repoDir)
			repo := e.cfg.Repositories["test-repo"]
			repo.Root, _ = filepath.EvalSymlinks(repoDir)
			repo.Providers = []string{"claude"}
			e.cfg.Repositories["test-repo"] = repo
			e.reg = providers.New(model.Config{Providers: []model.ProviderConfig{{ID: "claude", Path: bin, Model: "claude-sonnet-5-5"}}})
			sub := identitySubmission(t)
			sub.Mode = "read_only"
			sub.AllowedPaths = []string{"brief.txt"}
			j, err := e.SubmitRouted(sub, &model.InitialRoute{Provider: "claude", Model: "claude-sonnet-5-5", SelectedModel: "claude/claude-sonnet-5-5", AutoRouted: true})
			if err != nil {
				t.Fatal(err)
			}
			done := make(chan struct{})
			var workerContext context.Context = context.Background()
			if scenario == "attempt deadline" {
				workerContext = deadlineBeforeNotification{workerContext, time.Now().Add(time.Second)}
			}
			atAcceptance, resume := make(chan struct{}), make(chan struct{})
			continueAcceptance := sync.OnceFunc(func() { close(resume) })
			defer continueAcceptance()
			if scenario == "cancel" || scenario == "deadline" || scenario == "attempt expires during verification" {
				pause := sync.OnceFunc(func() { close(atAcceptance); <-resume })
				workerContext = acceptanceContext{workerContext, func() {
					jobs, err := e.db.All()
					if err == nil && len(jobs) == 1 && jobs[0].Status == model.Verifying {
						pause()
					}
				}}
			}
			go func() { defer close(done); e.run(workerContext, j) }()
			defer os.WriteFile(release, nil, 0600)
			for limit := time.Now().Add(5 * time.Second); ; {
				if _, err := os.Stat(started); err == nil {
					break
				}
				select {
				case <-done:
					t.Fatalf("worker never launched: %+v", e.jobs[j.ID])
				default:
				}
				if time.Now().After(limit) {
					t.Fatal("worker did not start")
				}
				time.Sleep(5 * time.Millisecond)
			}
			if scenario == "cancel" || scenario == "deadline" || scenario == "attempt expires during verification" {
				if err := os.WriteFile(release, nil, 0600); err != nil {
					t.Fatal(err)
				}
				select {
				case <-atAcceptance:
				case <-done:
					t.Fatal("worker completed without reaching acceptance")
				case <-time.After(10 * time.Second):
					t.Fatal("worker did not reach acceptance")
				}
			}
			if scenario == "attempt deadline" {
				deadline, _ := workerContext.Deadline()
				time.Sleep(time.Until(deadline))
			}
			e.mu.Lock()
			current := e.jobs[j.ID]
			want := model.Completed
			switch scenario {
			case "cancel", "worker error and cancel":
				current.Status = model.Cancelling
				want = model.Cancelled
			case "attempt expires during verification":
				// Shorten the persisted attempt while acceptance is paused. The job
				// deadline and worker context both still allow completion.
				current.Attempts = slices.Clone(current.Attempts)
				current.Attempts[0].DeadlineAt = time.Now().UTC()
				e.persist(current)
				want = model.RetryWait
			case "attempt deadline":
				want = model.RetryWait
			case "deadline":
				current.DeadlineAt = time.Now().Add(-time.Second)
				want = model.TimedOut
			}
			e.jobs[j.ID] = current
			err = os.WriteFile(release, nil, 0600)
			e.mu.Unlock()
			if err != nil {
				t.Fatal(err)
			}
			continueAcceptance()
			<-done
			assert := func(owner *Engine) {
				got, rows, err := owner.GetWithRoutingAudit(j.ID)
				expectedStatus := want
				if owner != e && want == model.RetryWait {
					expectedStatus = model.Paused
				}
				if err != nil || got.Status != expectedStatus || len(rows) != 1 || len(got.Attempts) != 1 {
					t.Fatalf("status %s rows %d err %v", got.Status, len(rows), err)
				}
				at := store.ProjectRoutingEvidence(got, nil).ExecutionAttempts[0]
				if (scenario == "attempt deadline" || scenario == "attempt expires during verification") && (got.Attempts[0].Code != "ATTEMPT_TIMEOUT" || !got.DeadlineAt.After(*got.Attempts[0].FinishedAt) || got.Result != nil) {
					t.Fatal("attempt timeout lost", got.Attempts[0].Code)
				}
				if want == model.Completed {
					if at.Status != "SUCCESS" || rows[0].DispatchStatus != "SUCCESS" {
						t.Fatal(at, rows)
					}
					return
				}
				if at.Status == "SUCCESS" || rows[0].DispatchStatus == "SUCCESS" || model.Terminal(want) && rows[0].DispatchStatus != "FAILED" || got.Attempts[0].Code == "" {
					t.Errorf("terminal %s but attempt %s code %q", want, at.Status, got.Attempts[0].Code)
				}
				if scenario == "worker error and cancel" && got.Attempts[0].Code == "CLIENT_CANCELLED" {
					t.Error("lost worker fault")
				}
			}
			assert(e)
			if err := e.Close(); err != nil {
				t.Fatal(err)
			}
			re, err := New(e.cfg, e.db, e.reg)
			if err != nil {
				t.Fatal(err)
			}
			defer re.Close()
			assert(re)
		})
	}
}

func TestBatchRetrySurvivesReducedProviderRegistry(t *testing.T) {
	e := validatedIntentEngine(t, 0)
	repo := e.cfg.Repositories["test-repo"]
	repo.Providers = []string{"codex", "claude"}
	e.cfg.Repositories["test-repo"] = repo
	e.reg.Candidates = append(e.reg.Candidates, model.Capability{Provider: "claude", Status: "available"})
	task := identitySubmission(t).Task
	task.Mode = "read_only"
	b := model.Batch{RepoID: "test-repo", RequestID: "batch-retry", Independence: "distinct_provider", Items: []model.Item{{Key: "one", Task: task}, {Key: "two", Task: task}}}
	first, err := e.SubmitBatch(b)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	re, err := New(e.cfg, e.db, providers.Registry{Candidates: e.reg.Candidates[:1]})
	if err != nil {
		t.Fatal(err)
	}
	defer re.Close()
	got, err := re.SubmitBatch(b)
	if err != nil || got.ID != first.ID || got.RoutingProfile != first.RoutingProfile {
		t.Errorf("batch replay %q %v", got.ID, err)
	}
	wantFault := func(b model.Batch, want string) {
		var fault *model.Fault
		if _, err := re.SubmitBatch(b); !errors.As(err, &fault) || fault.Code != want {
			t.Errorf("want %s got %v", want, err)
		}
	}
	b.Items[0].Objective = "changed"
	wantFault(b, "IDEMPOTENCY_CONFLICT")
	b.Items[0].Objective = task.Objective
	b.RequestID = "new"
	wantFault(b, "NO_ELIGIBLE_ROUTE")
	re.mu.Lock()
	delete(re.cfg.Repositories, b.RepoID)
	re.mu.Unlock()
	b.RequestID = first.RequestID
	wantFault(b, "REPO_NOT_REGISTERED")
	if jobs, err := re.db.All(); err != nil || len(jobs) != 3 {
		t.Fatal("extra job", err)
	}
	if rows, err := re.db.GetRoutingAudit(first.ID); err != nil || len(rows) != 0 {
		t.Fatal("extra audit", err)
	}
}

func TestLegacyOpenCodeModelPrefixMatching(t *testing.T) {
	for _, name := range []string{"beam-free", "opencode/beam-free", "other/beam-free"} {
		e, j := profileFixture(capacityClock)
		c := e.reg.Candidates[2]
		c.Mode = "fixture"
		c.Model = name
		e.reg.Candidates = []model.Capability{c}
		d, matched := e.evaluateAutoRoute("demo", "coding", nil, j.Task, capacityClock, "", "inference")
		if name == "other/beam-free" {
			if matched != nil {
				t.Fatal("foreign namespace accepted")
			}
			continue
		}
		if matched == nil || matched.Model != name || d.SelectedModel != "opencode/beam-free" {
			t.Errorf("matrix %s selected %s", name, d.SelectedModel)
		}
		s := identitySubmission(t)
		old := s
		old.PreferredProvider = "opencode"
		old.PreferredModel = name
		old.TargetEffort = "low"
		legacy := model.Job{Task: old.Task}
		ok, r := matchLegacySubmission(s, model.Digest(old), legacy, &store.JobRoutingAudit{AssignedModel: "opencode/beam-free", AssignedEffort: "low", RouteReason: "STANDARD_EFFORT"})
		if !ok || r == nil || r.Model != name {
			t.Errorf("legacy retry %s", name)
		}
	}
}

func TestCapacityAllRepositoriesUseStableOrder(t *testing.T) {
	e, _ := profileFixture(capacityClock)
	e.cfg.Repositories = map[string]model.Repo{"a": {Providers: []string{"codex"}}, "b": {Providers: []string{"agy"}}}
	_, _, a := e.Capacity("a")
	_, _, b := e.Capacity("b")
	key := func(rows []model.Decision) string {
		v := ""
		for _, row := range rows {
			v += row.Provider + "/" + row.Model + ":" + row.Reason + ","
		}
		return v
	}
	want := key(a) + key(b)
	if key(a) == key(b) {
		t.Fatal("indistinguishable fixtures")
	}
	for range 100 {
		_, _, both := e.Capacity("")
		if key(both) != want {
			t.Fatal("capacity previews changed repository order")
		}
	}
}
