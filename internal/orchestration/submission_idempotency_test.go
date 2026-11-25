// Copyright 2026 Furkan Anıl Öztaş
// SPDX-License-Identifier: Apache-2.0

package orchestration

import (
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/faoztas/ahenk-mcp/internal/model"
	"github.com/faoztas/ahenk-mcp/internal/providers"
)

func identitySubmission(t *testing.T) model.Submission {
	t.Helper()
	in := model.DelegateTaskInput{RequestID: "request", RepoID: "test-repo", Objective: "Check ordering"}
	sub, _, _, err := in.Normalize("", nil)
	if err != nil {
		t.Fatal(err)
	}
	return sub
}

func submitDecision(e *Engine, s model.Submission, provider string) (model.Job, error) {
	return e.SubmitRouted(s, &model.InitialRoute{Provider: provider, Model: "test", SelectedModel: provider + "/test", AutoRouted: true})
}

func TestConcurrentSubmissionKeepsOneJobAndDecision(t *testing.T) {
	e, _ := setupTestEngine(t)
	sub := identitySubmission(t)
	type result struct {
		j   model.Job
		err error
	}
	results := make(chan result, 2)
	start := make(chan struct{})
	ready := make(chan struct{}, 2)
	for _, provider := range []string{"codex", "agy"} {
		go func() {
			ready <- struct{}{}
			<-start
			j, err := submitDecision(e, sub, provider)
			results <- result{j, err}
		}()
	}
	<-ready
	<-ready
	close(start)
	a, b := <-results, <-results
	if a.err != nil || b.err != nil || a.j.ID != b.j.ID {
		t.Fatalf("different admissions: %s %v / %s %v", a.j.ID, a.err, b.j.ID, b.err)
	}
	rows, err := e.db.GetRoutingAudit(a.j.ID)
	if err != nil || len(rows) != 1 {
		t.Fatal(rows, err)
	}
	if a.j.InitialRoute == nil || !reflect.DeepEqual(a.j.InitialRoute, b.j.InitialRoute) || a.j.InitialRoute.SelectedModel != rows[0].AssignedModel {
		t.Fatal("winning decision diverged")
	}
	jobs, err := e.db.All()
	if err != nil || len(jobs) != 1 {
		t.Fatal(jobs, err)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(e.jobs) != 1 {
		t.Fatal("duplicate queue owner")
	}
}

func TestMatchingRetryBypassesOnlyNewJobCapacity(t *testing.T) {
	e, _ := setupTestEngine(t)
	sub := identitySubmission(t)
	first, err := e.Submit(sub)
	if err != nil {
		t.Fatal(err)
	}
	e.mu.Lock()
	for i := 0; i < 999; i++ {
		id := fmt.Sprintf("full-%d", i)
		e.jobs[id] = model.Job{ID: id, CreatedAt: time.Now(), Status: model.Completed}
	}
	e.mu.Unlock()
	retry, err := e.Submit(sub)
	if err != nil || retry.ID != first.ID {
		t.Fatalf("matching retry at capacity: %s %v", retry.ID, err)
	}
	sub.RequestID = "fresh"
	var fault *model.Fault
	if _, err = e.Submit(sub); !errors.As(err, &fault) || fault.Code != "RESOURCE_LIMIT" {
		t.Fatal(err)
	}
}

func TestInitialRoutePrefersEligibleCandidateAndFailsOver(t *testing.T) {
	now := time.Now()
	e := fpRoutingFixture(now)
	e.cfg.Repositories["demo"] = model.Repo{Providers: []string{"codex", "agy"}}
	e.reg.Candidates = append(e.reg.Candidates, model.Capability{Provider: "codex", Model: "gpt-6.1-sol", Status: "available", AttestedAccount: "fixture-account"})
	j := fpRoutingJob(now, "")
	j.Task.PreferredProvider, j.Task.PreferredModel = "", ""
	j.InitialRoute = &model.InitialRoute{AutoRouted: true, Provider: "codex", Model: "gpt-6.1-sol"}
	if c, _ := e.route(&j, now); c.Provider != "codex" {
		t.Fatalf("first decision displaced: %+v", j.Route)
	}
	zero := 0.0
	e.quotas["codex"] = []providers.Quota{{Bucket: providers.BarCodexBurst, ObservedAt: now, Remaining: &zero, State: "KNOWN", Unit: "percent", AccountMatch: "MATCHED"}}
	for _, match := range []string{"MISMATCH", "UNKNOWN", "MATCHED"} {
		e.quotas["codex"][0].AccountMatch = match
		want := "codex"
		if match == "MATCHED" {
			want = "agy"
		}
		if c, _ := e.route(&j, now); c.Provider != want {
			t.Errorf("account %s: selected %s, want %s", match, c.Provider, want)
		}
	}
}

func TestInitialRouteRespectsDispatchStrategy(t *testing.T) {
	for _, strategy := range []string{"latency_first", "conserve"} {
		t.Run(strategy, func(t *testing.T) {
			now := time.Now()
			e := fpRoutingFixture(now)
			e.cfg.Repositories["demo"] = model.Repo{Providers: []string{"agy"}}
			e.reg.Candidates[0].Speed = "slow"
			j := fpRoutingJob(now, "")
			j.Task.PreferredProvider, j.Task.PreferredModel = "", ""
			if strategy == "latency_first" {
				j.Task.Strategy = strategy
			} else {
				j.RoutingMode = "conserve"
			}
			j.InitialRoute = &model.InitialRoute{AutoRouted: true, Provider: "agy", Model: "gemini-3.8-flash-medium"}
			if c, _ := e.route(&j, now); c.Model != "gemini-4-argon" {
				t.Errorf("%s selected %s, want gemini-4-argon", strategy, c.Model)
			}
		})
	}
}

func TestRemovedRepositorySubmission(t *testing.T) {
	e, _ := setupTestEngine(t)
	sub := identitySubmission(t)
	if _, err := e.Submit(sub); err != nil {
		t.Fatal(err)
	}
	e.mu.Lock()
	delete(e.cfg.Repositories, sub.RepoID)
	e.mu.Unlock()
	for _, id := range []string{sub.RequestID, "unused"} {
		sub.RequestID = id
		var fault *model.Fault
		if _, err := e.Submit(sub); !errors.As(err, &fault) || fault.Code != "REPO_NOT_REGISTERED" {
			t.Errorf("request %s: got %v, want REPO_NOT_REGISTERED", id, err)
		}
	}
}

func TestMCPPrivacyProjectionPreservesInternalAccountMatching(t *testing.T) {
	c := model.Capability{Provider: "codex", AttestedAccount: "operator-fixture"}
	hash := model.Hash([]byte(c.AttestedAccount))
	e := Engine{cfg: model.Config{CodexBarAccountSHA256: map[string]string{"codex": hash}}}
	grant := model.PrivateRoute{ExpectedAccountSHA256: hash}
	for _, want := range []bool{true, false} {
		if ok, _ := grant.AccountMatches(c); ok != want || e.quotaAccountMatches(c) != want {
			t.Fatal("authorization and quota must match the exact internal identity")
		}
		c.AttestedAccount = "other-fixture"
	}
}
