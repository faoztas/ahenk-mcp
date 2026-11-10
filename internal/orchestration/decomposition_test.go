// Copyright 2026 Furkan Anıl Öztaş
// SPDX-License-Identifier: Apache-2.0

package orchestration

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/faoztas/ahenk-mcp/internal/model"
	"github.com/faoztas/ahenk-mcp/internal/store"
)

const testPlan = `{"steps":[{"key":"look","kind":"inspect","objective":"Inspect Go","paths":["calc.go"],"depends_on":[]},{"key":"edit","kind":"implement","objective":"Correct Go","paths":["calc.go"],"depends_on":["look"]},{"key":"review","kind":"review","objective":"Review Go","paths":["calc.go"],"depends_on":["edit"]}]}`

func testPlanningParent() (model.Job, model.Repo) {
	repo := model.Repo{AllowedPaths: []string{"calc.go"}, Providers: []string{"agy", "opencode"}, Limits: model.ProjectLimits{RequireIndependentReview: true},
		TrustedLocal: &model.TrustedLocalPolicy{WritePaths: []string{"calc.go"}, GoChecks: map[string]string{"test": "TestCalc"}}}
	parent := model.Job{ID: "parent", RepoID: "demo", Status: model.Running, DeadlineAt: time.Now().Add(time.Hour), PolicyDigest: model.Digest(repo),
		Task: model.Task{Decompose: true, Mode: "patch", DataClass: "synthetic", ExecutionProfile: model.TrustedLocalProfile, Objective: "Implement the scoped Go change",
			AllowedPaths: []string{"calc.go"}, Acceptance: []model.Check{{ID: "check", Description: "Pass the check", CheckID: "test"}}}}
	return parent, repo
}

func TestAutomaticPlanKeepsScopesChecksAndReview(t *testing.T) {
	parent, repo := testPlanningParent()
	jobs, err := buildPlan(parent, repo, testPlan, time.Now())
	if err != nil || len(jobs) != 3 {
		t.Fatalf("%+v %v", jobs, err)
	}
	if jobs[1].Task.Acceptance[0].CheckID != "test" || jobs[1].Task.Decompose ||
		!slices.Contains(jobs[2].DependsOn, jobs[1].ID) || len(jobs[2].HandoffFrom) != 1 ||
		jobs[2].HandoffFrom[0] != (model.HandoffFrom{Key: "edit", Kind: "diff"}) {
		t.Fatalf("wrong derived plan %+v", jobs)
	}
	for _, bad := range []string{
		strings.Replace(testPlan, `"depends_on":[]`, `"depends_on":["review"]`, 1),
		strings.Replace(testPlan, `"depends_on":["look"]`, `"depends_on":["missing"]`, 1),
		strings.Replace(testPlan, `"paths":["calc.go"]`, `"paths":["other.go"]`, 1),
		strings.Replace(testPlan, `"kind":"inspect"`, `"kind":"implement"`, 1),
		strings.Replace(testPlan, `"kind":"review"`, `"kind":"inspect"`, 1),
		strings.Replace(testPlan, `"kind":"inspect"`, `"kind":"inspect","provider":"paid"`, 1),
	} {
		if _, err := buildPlan(parent, repo, bad, time.Now()); err == nil {
			t.Fatalf("invalid plan accepted: %s", bad)
		}
	}
	repo.Limits.MaxBatchTasks = 2
	if _, err := buildPlan(parent, repo, testPlan, time.Now()); err == nil {
		t.Fatal("project batch limit bypassed")
	}
}

func TestPlanExpansionIsAtomicAndFenced(t *testing.T) {
	parent, repo := testPlanningParent()
	db, err := store.Open(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Insert([]model.Job{parent}, ""); err != nil {
		t.Fatal(err)
	}
	children, err := buildPlan(parent, repo, testPlan, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	for _, child := range children {
		parent.Children = append(parent.Children, child.ID)
	}
	parent.Status = model.Queued
	// Simulate a duplicate child insert after the transaction updates parent.
	bad := slices.Clone(children)
	bad[1].ID = bad[0].ID
	if err := db.Expand(parent, bad); err == nil {
		t.Fatal("duplicate transaction accepted")
	}
	rows, err := db.All()
	if err != nil || len(rows) != 1 || len(rows[0].Children) != 0 || rows[0].Status != model.Running {
		t.Fatalf("partial graph visible: %+v %v", rows, err)
	}
	if err := db.Expand(parent, children); err != nil {
		t.Fatal(err)
	}
	if err := db.Expand(parent, children); err == nil {
		t.Fatal("second expansion bypassed fence")
	}
	rows, err = db.All()
	if err != nil || len(rows) != 4 {
		t.Fatalf("wrong durable graph %+v %v", rows, err)
	}
}
