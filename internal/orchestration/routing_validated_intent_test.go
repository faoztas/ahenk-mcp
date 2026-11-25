// Copyright 2026 Furkan Anıl Öztaş
// SPDX-License-Identifier: Apache-2.0

package orchestration

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/faoztas/ahenk-mcp/internal/model"
)

func validatedIntentEngine(t *testing.T, deadline int) *Engine {
	t.Helper()
	e, _ := setupTestEngine(t)
	// Drive routing explicitly without launching a worker.
	e.stop()
	e.wg.Wait()
	repo := e.cfg.Repositories["test-repo"]
	repo.Providers = []string{"codex"}
	repo.DefaultRoutingProfile = model.RoutingProfileQuality
	repo.Limits.MaxDeadlineSeconds = deadline
	e.cfg.Repositories["test-repo"] = repo
	e.reg.Candidates = []model.Capability{{Provider: "codex", Model: "gpt-6.1-sol", Status: "available", Speed: "fast"}}
	return e
}

func TestAutoSubmissionUsesProjectDeadlineInPreview(t *testing.T) {
	e := validatedIntentEngine(t, 30)
	sub := identitySubmission(t)
	sub.Mode = "read_only"
	j, preview, err := e.SubmitAutoRouted(sub, "coding", nil)
	if err != nil || j.DeadlineAt.Sub(j.CreatedAt) != 30*time.Second {
		t.Fatalf("admission deadline = %v, err %v", j.DeadlineAt.Sub(j.CreatedAt), err)
	}
	if preview != nil || j.InitialRoute != nil {
		t.Fatalf("selected worker outside 30s deadline: %+v", preview)
	}
	rows, err := e.db.GetRoutingAudit(j.ID)
	if err != nil || len(rows) != 0 {
		t.Fatalf("unexpected first routing audit: %+v, %v", rows, err)
	}
}

func assertResolvedEffort(t *testing.T, e *Engine, j model.Job, want string) {
	t.Helper()
	e.mu.Lock()
	defer e.mu.Unlock()
	c, _ := e.route(&j, j.CreatedAt)
	if c.Provider != "codex" || len(j.Route) == 0 || j.Route[0].ResolvedEffort != want {
		t.Errorf("job %s expected codex effort %s, got %+v", j.Key, want, j.Route)
	}
}

func TestExplicitZeroEffortAfterEngineRestart(t *testing.T) {
	e := validatedIntentEngine(t, 0)
	sub := identitySubmission(t)
	sub.Mode = "read_only"
	sub.HasComplexity = true
	j, err := e.Submit(sub)
	if err != nil {
		t.Fatal(err)
	}
	assertResolvedEffort(t, e, j, "low")
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	restarted, err := New(e.cfg, e.db, e.reg)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	reloaded, err := restarted.Get(j.ID)
	if err != nil {
		t.Fatal(err)
	}
	assertResolvedEffort(t, restarted, reloaded, "low")
}

func TestBatchExplicitZeroEffortAndReplay(t *testing.T) {
	e := validatedIntentEngine(t, 0)
	wire := `{"repo_id":"test-repo","request_id":"batch","items":[
		{"key":"zero","complexity":0,"mode":"read_only","objective":"Check ordering","data_class":"synthetic","allowed_paths":["."],"acceptance":[{"id":"test","description":"Done"}]},
		{"key":"omitted","mode":"read_only","objective":"Check ordering","data_class":"synthetic","allowed_paths":["."],"acceptance":[{"id":"test","description":"Done"}]}]}`
	var in model.BatchInput
	if err := json.Unmarshal([]byte(wire), &in); err != nil {
		t.Fatal(err)
	}
	b := model.Batch(in)
	parent, err := e.SubmitBatch(b)
	if err != nil {
		t.Fatal(err)
	}
	for i, want := range []string{"low", "medium"} {
		j, err := e.Get(parent.Children[i])
		if err != nil {
			t.Fatal(err)
		}
		assertResolvedEffort(t, e, j, want)
	}
	retry, err := e.SubmitBatch(b)
	if err != nil || retry.ID != parent.ID {
		t.Fatalf("matching replay: %v", err)
	}
	b.Items[0].HasComplexity = false
	var fault *model.Fault
	if _, err := e.SubmitBatch(b); !errors.As(err, &fault) || fault.Code != "IDEMPOTENCY_CONFLICT" {
		t.Errorf("zero retried as omitted: %v", err)
	}

	b.RequestID = "new-omitted"
	if _, err := e.SubmitBatch(b); err != nil {
		t.Fatal(err)
	}
	b.Items[0].HasComplexity = true
	if _, err := e.SubmitBatch(b); !errors.As(err, &fault) || fault.Code != "IDEMPOTENCY_CONFLICT" {
		t.Errorf("omitted retried as zero: %v", err)
	}
}

func TestLegacyBatchWithExplicitZeroReplaysAfterRestart(t *testing.T) {
	e := validatedIntentEngine(t, 0)
	wire := `{"repo_id":"test-repo","request_id":"legacy-batch","items":[
		{"key":"one","complexity":0,"mode":"read_only","objective":"Check ordering","data_class":"synthetic","allowed_paths":["."],"acceptance":[{"id":"test","description":"Done"}]}]}`
	var original model.Batch
	if err := json.Unmarshal([]byte(wire), &original); err != nil {
		t.Fatal(err)
	}
	repo := e.cfg.Repositories[original.RepoID]
	// The prior scheduler hashed the normalized item, with zero still omitted.
	if fault := original.Items[0].Task.Validate(repo); fault != nil {
		t.Fatal(fault)
	}
	now := time.Now().UTC()
	before := model.Job{ID: "legacy-batch-job", RepoID: original.RepoID, RequestID: original.RequestID,
		Children: []string{"legacy-batch-child"}, Status: model.Completed, PolicyDigest: repo.PolicyDigest(), CreatedAt: now}
	child := model.Job{ID: before.Children[0], Parent: before.ID, Key: "one", RepoID: before.RepoID,
		Status: model.Completed, PolicyDigest: before.PolicyDigest, Task: original.Items[0].Task, CreatedAt: now}
	if _, err := e.db.Insert([]model.Job{before, child}, model.Digest(original)); err != nil {
		t.Fatal(err)
	}
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	restarted, err := New(e.cfg, e.db, e.reg)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()

	var current model.BatchInput
	if err := json.Unmarshal([]byte(wire), &current); err != nil {
		t.Fatal(err)
	}
	retry, err := restarted.SubmitBatch(model.Batch(current))
	if err != nil || retry.ID != before.ID || retry.Status != model.Completed {
		t.Fatalf("legacy replay: job %s, status %s, err %v", retry.ID, retry.Status, err)
	}
	current.Items[0].Objective = "A different request"
	var conflict *model.Fault
	if _, err := restarted.SubmitBatch(model.Batch(current)); !errors.As(err, &conflict) || conflict.Code != "IDEMPOTENCY_CONFLICT" {
		t.Fatalf("changed legacy input must conflict: %v", err)
	}
}

func TestAutoSubmissionKeepsImplicitReadRole(t *testing.T) {
	e := validatedIntentEngine(t, 0)
	sub := identitySubmission(t)
	sub.Mode = "read_only"
	e.reg.Candidates[0].Quality = []model.QualityEvidence{{Role: "inference", Tier: 2, Source: "LIVE_DETERMINISTIC_ROLE_CALIBRATION"}}
	repo := e.cfg.Repositories[sub.RepoID]
	repo.Limits.MinimumQualityTier = 2
	e.cfg.Repositories[sub.RepoID] = repo
	j, preview, err := e.SubmitAutoRouted(sub, "coding", nil)
	if err != nil || preview == nil || preview.Provider != "codex" {
		t.Fatalf("implicit read preview = %+v, err %v", preview, err)
	}
	c, _ := e.route(&j, j.CreatedAt)
	if c.Provider != preview.Provider || j.Route[0].QualityRole != "inference" {
		t.Fatalf("implicit read dispatch = %+v", j.Route)
	}
}

func TestBatchIntentUsesVersionedDigestForAllBatches(t *testing.T) {
	b := model.Batch{Items: []model.Item{{Key: "one", Task: identitySubmission(t).Task}}}
	for _, complexity := range []float64{0, 0.5} {
		b.Items[0].Complexity = complexity
		b.Items[0].HasComplexity = complexity != 0
		if !strings.HasPrefix(model.BatchIntentDigest(b), "v2:batch:") {
			t.Fatalf("unversioned digest for new batch with complexity %v", complexity)
		}
	}
}

func TestComplexityPresenceAliases(t *testing.T) {
	for _, obj := range []string{`{}`, `{"Complexity":0}`, `{"complexity":0,"COMPLEXITY":1}`, `{"complexity":1,"complexity":0}`} {
		for _, form := range []string{"single", "nested", "batch"} {
			var flag bool
			var err error
			if form == "batch" {
				var b model.BatchInput
				err = json.Unmarshal([]byte(`{"items":[`+obj+`]}`), &b)
				if err == nil {
					flag = b.Items[0].HasComplexity
				}
			} else {
				wire := obj
				if form == "nested" {
					wire = `{"task":` + obj + `}`
				}
				var s model.DelegateTaskInput
				err = json.Unmarshal([]byte(wire), &s)
				flag = s.HasComplexity
			}
			duplicate := strings.Contains(obj, ",")
			if (err != nil) != duplicate || !duplicate && flag != (obj != "{}") {
				t.Errorf("%s %s: presence=%v error=%v", form, obj, flag, err)
			}
		}
	}
}
