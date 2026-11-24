// Copyright 2026 Furkan Anıl Öztaş
// SPDX-License-Identifier: Apache-2.0

package orchestration

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/faoztas/ahenk-mcp/internal/model"
	"github.com/faoztas/ahenk-mcp/internal/store"
)

func decodeProfileFixture(t *testing.T, wire string, dest any) {
	t.Helper()
	if err := json.Unmarshal([]byte(wire), dest); err != nil {
		t.Fatal(err)
	}
}

func wantJobProfile(t *testing.T, j model.Job, profile model.RoutingProfile, source string) {
	t.Helper()
	if j.RoutingProfile != profile || j.RoutingProfileSource != source {
		t.Fatalf("job %s profile = %q/%q, want %q/%q", j.Key, j.RoutingProfile, j.RoutingProfileSource, profile, source)
	}
}

func TestRoutingProfilePrecedenceAndRetry(t *testing.T) {
	e, _ := setupTestEngine(t)
	s := identitySubmission(t)
	submit := func() model.Job {
		t.Helper()
		j, err := e.Submit(s)
		if err != nil {
			t.Fatal(err)
		}
		return j
	}
	wantJobProfile(t, submit(), "", "legacy")
	setRepo := func(raw string) {
		e.mu.Lock()
		defer e.mu.Unlock()
		r := e.cfg.Repositories[s.RepoID]
		decodeProfileFixture(t, raw, &r)
		e.cfg.Repositories[s.RepoID] = r
	}
	setRepo(`{"default_routing_profile":"price"}`)
	s.RequestID = "inherited"
	first := submit()
	wantJobProfile(t, first, "price", "repo_config")
	setRepo(`{"default_routing_profile":"quality"}`)
	retry := submit()
	if retry.ID != first.ID {
		t.Fatal("retry created a new job")
	}
	wantJobProfile(t, retry, "price", "repo_config")
	if retry.Task.RoutingProfile != "" {
		t.Fatal("default leaked into explicit intent")
	}
	s.RoutingProfile = "price"
	var changed *model.Fault
	if _, err := e.Submit(s); !errors.As(err, &changed) || changed.Code != "IDEMPOTENCY_CONFLICT" {
		t.Fatalf("explicitly selecting the first default: %v", err)
	}
	s.RequestID = "explicit"
	decodeProfileFixture(t, `{"routing_profile":"quality"}`, &s.Task)
	wantJobProfile(t, submit(), "quality", "request")
	decodeProfileFixture(t, `{"routing_profile":"price"}`, &s.Task)
	var fault *model.Fault
	if _, err := e.Submit(s); !errors.As(err, &fault) || fault.Code != "IDEMPOTENCY_CONFLICT" {
		t.Fatalf("explicit change: %v", err)
	}
	setRepo(`{"allowed_paths":["limited"]}`)
	if _, err := e.Submit(s); !errors.As(err, &fault) || fault.Code != "NOT_FOUND" {
		t.Fatalf("policy change: %v", err)
	}
}

func TestRoutingProfilePlannerInheritance(t *testing.T) {
	e, _ := setupTestEngine(t)
	s := identitySubmission(t)
	b := model.Batch{RepoID: s.RepoID, RequestID: s.RequestID, Items: []model.Item{{Key: "one", Task: s.Task}, {Key: "two", Task: s.Task}}}
	decodeProfileFixture(t, `{"routing_profile":"price"}`, &b)
	decodeProfileFixture(t, `{"routing_profile":"quality"}`, &b.Items[1].Task)
	parent, err := e.SubmitBatch(b)
	if err != nil {
		t.Fatal(err)
	}
	wantJobProfile(t, parent, "price", "batch")
	for i, id := range parent.Children {
		child, err := e.Get(id)
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			wantJobProfile(t, child, "price", "batch")
		} else {
			wantJobProfile(t, child, "quality", "request")
		}
	}
	retry, err := e.SubmitBatch(b)
	if err != nil || retry.ID != parent.ID {
		t.Fatalf("batch retry = %s: %v", retry.ID, err)
	}
	b.RoutingProfile = "quality"
	var conflict *model.Fault
	if _, err := e.SubmitBatch(b); !errors.As(err, &conflict) || conflict.Code != "IDEMPOTENCY_CONFLICT" {
		t.Fatalf("changed batch profile: %v", err)
	}
	p, repo := testPlanningParent()
	decodeProfileFixture(t, `{"routing_profile":"price","routing_profile_source":"repo_config"}`, &p)
	children, err := buildPlan(p, repo, testPlan, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	for _, child := range children {
		wantJobProfile(t, child, "price", "repo_config")
	}
	for _, bad := range []string{strings.Replace(testPlan, `"kind":"inspect"`, `"kind":"inspect","routing_profile":"quality"`, 1), strings.Replace(testPlan, "calc.go", "other.go", 1)} {
		if _, err := buildPlan(p, repo, bad, time.Now()); err == nil {
			t.Fatal("accepted expanded planner authority")
		}
	}
}

func TestRoutingProfileReloadsLegacyAndAdmittedJobs(t *testing.T) {
	e, _ := setupTestEngine(t)
	legacyIntent := identitySubmission(t)
	s := legacyIntent
	s.RequestID = "admitted"
	s.RoutingProfile = model.RoutingProfileQuality
	admitted, err := e.Submit(s)
	if err != nil {
		t.Fatal(err)
	}
	if err = e.Close(); err != nil {
		t.Fatal(err)
	}
	r := e.cfg.Repositories[s.RepoID]
	if fault := legacyIntent.Task.Validate(r); fault != nil {
		t.Fatal(fault)
	}
	// Captured from the staged A1 code using the validated pre-profile submission.
	const legacyDigest = "v2:0c2947c00cf05180462a54fdbd2bd93da349fdd1df19498be55cd8b4b7f7bf14"
	legacy := model.Job{
		ID: "pre-profile", RepoID: legacyIntent.RepoID, RequestID: legacyIntent.RequestID, Task: legacyIntent.Task,
		Status: model.Completed, PolicyDigest: model.Digest(r), PlanSHA256: model.Hash([]byte(testPlan)),
	}
	if _, err = e.db.Insert([]model.Job{legacy}, legacyDigest); err != nil {
		t.Fatal(err)
	}
	e.db.Close()
	r.DefaultRoutingProfile = model.RoutingProfilePrice
	e.cfg.Repositories[s.RepoID] = r
	db, err := store.Open(e.cfg.StateDir)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	reopened, err := New(e.cfg, db, e.reg)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	old, err := reopened.Get(legacy.ID)
	if err != nil {
		t.Fatal(err)
	}
	wantJobProfile(t, old, "", "legacy")
	if old.PlanSHA256 != legacy.PlanSHA256 {
		t.Fatal("stored plan hash changed on restore")
	}
	legacyRetry, err := reopened.Submit(legacyIntent)
	if err != nil || legacyRetry.ID != legacy.ID {
		t.Fatalf("pre-profile retry = %s, want %s: %v", legacyRetry.ID, legacy.ID, err)
	}
	wantJobProfile(t, legacyRetry, "", "legacy")
	retry, err := reopened.Submit(s)
	if err != nil || retry.ID != admitted.ID {
		t.Fatalf("reopened retry %s: %v", retry.ID, err)
	}
	wantJobProfile(t, retry, "quality", "request")

	reopened.mu.Lock()
	r.AllowedPaths = []string{"limited"}
	reopened.cfg.Repositories[s.RepoID] = r
	reopened.mu.Unlock()
	var fault *model.Fault
	if _, err := reopened.Get(legacy.ID); !errors.As(err, &fault) || fault.Code != "NOT_FOUND" {
		t.Fatalf("lookup under revoked policy: %v", err)
	}
	if _, err := reopened.Submit(legacyIntent); !errors.As(err, &fault) || fault.Code != "NOT_FOUND" {
		t.Fatalf("retry under revoked policy: %v", err)
	}
}

func TestRoutingPreferenceAdmissionChecksEveryRepository(t *testing.T) {
	e, _ := setupTestEngine(t)
	sub := identitySubmission(t)
	selected, ok := e.Repo(sub.RepoID)
	if !ok {
		t.Fatal("missing selected repository")
	}
	otherRoot := t.TempDir()
	alias := filepath.Join(t.TempDir(), "state-alias")
	if err := os.Symlink(otherRoot, alias); err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(alias, "state")
	profile := "price"
	if err := store.SaveRoutingPreference(state, sub.RepoID, selected.Root, &profile); err != nil {
		t.Fatal(err)
	}

	e.mu.Lock()
	e.cfg.StateDir = state
	e.cfg.Repositories["other"] = model.Repo{Root: otherRoot}
	e.mu.Unlock()

	job, err := e.Submit(sub)
	if err != nil {
		t.Fatal(err)
	}
	wantJobProfile(t, job, "", "legacy")
	if job.RoutingPreferenceWarning != "INVALID_ROUTING_PREFERENCE" {
		t.Fatalf("job warning = %q", job.RoutingPreferenceWarning)
	}
}

func TestAutoSubmissionResolvesRepoAndReturnsPersistedRoute(t *testing.T) {
	e, root := setupTestEngine(t)
	t.Chdir(root)
	e.mu.Lock()
	repo := e.cfg.Repositories["test-repo"]
	repo.Providers = []string{"codex"}
	repo.DefaultRoutingProfile = model.RoutingProfileQuality
	e.cfg.Repositories["test-repo"] = repo
	e.reg.Candidates = []model.Capability{{Provider: "codex", Model: "gpt-6.1-sol", Status: "available"}}
	stateDir := e.cfg.StateDir
	e.mu.Unlock()
	sub := identitySubmission(t)
	sub.Mode = "read_only"
	sub.DeadlineSeconds = 900

	withoutRepo := sub
	withoutRepo.RepoID = ""
	resolved, first, err := e.SubmitAutoRouted(withoutRepo, "coding", nil)
	if err != nil {
		t.Fatal(err)
	}
	if resolved.RepoID != sub.RepoID || first == nil || !reflect.DeepEqual(first, resolved.InitialRoute) {
		t.Errorf("auto submission from working directory: job %s route %+v, returned %+v", resolved.RepoID, resolved.InitialRoute, first)
	}

	sub.RequestID = "retry-profile-change"
	before, _, err := e.SubmitAutoRouted(sub, "coding", nil)
	if err != nil || before.InitialRoute == nil {
		t.Fatalf("initial submission: route %+v, err %v", before.InitialRoute, err)
	}
	price := "price"
	if err := store.SaveRoutingPreference(stateDir, sub.RepoID, root, &price); err != nil {
		t.Fatal(err)
	}
	retry, decision, err := e.SubmitAutoRouted(sub, "coding", nil)
	if err != nil || retry.ID != before.ID || !reflect.DeepEqual(decision, before.InitialRoute) {
		t.Fatalf("retry changed job or initial route, route %+v, err %v", decision, err)
	}
}
