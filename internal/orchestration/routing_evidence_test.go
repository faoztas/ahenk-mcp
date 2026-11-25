// Copyright 2026 Furkan Anıl Öztaş
// SPDX-License-Identifier: Apache-2.0

package orchestration_test

import (
	"context"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/faoztas/ahenk-mcp/internal/model"
	"github.com/faoztas/ahenk-mcp/internal/orchestration"
	"github.com/faoztas/ahenk-mcp/internal/providers"
	"github.com/faoztas/ahenk-mcp/internal/store"
)

func TestRoutingAuditSeparatesFixtureFallbackFromFirstDecision(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	initGitRepoForAuditTest(t, root)
	cfg := model.Config{
		StateDir:      filepath.Join(t.TempDir(), "state"),
		EnableFixture: true,
		Repositories: map[string]model.Repo{
			"repo": {Root: root, AllowedPaths: []string{"."}, Providers: []string{"fixture-a", "fixture-b"}},
		},
	}
	db, err := store.Open(cfg.StateDir)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	e, err := orchestration.New(cfg, db, providers.New(cfg))
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()

	in := model.DelegateTaskInput{Objective: "fixture:429", Mode: "read_only", RequestID: "request", RepoID: "repo"}
	sub, _, _, err := in.Normalize("", nil)
	if err != nil {
		t.Fatal(err)
	}
	first := &model.InitialRoute{
		Provider: "fixture-a", Model: "synthetic", SelectedModel: "fixture-a/synthetic",
		AutoRouted: true, ResolvedEffort: "high", Reason: "NOMINAL_HIGH_EFFORT",
	}
	job, err := e.SubmitRouted(sub, first)
	if err != nil {
		t.Fatal(err)
	}
	done, err := e.Wait(context.Background(), job.ID, 10*time.Second)
	if err != nil || done.Status != model.Completed {
		t.Fatalf("fixture fallback did not complete: %s %s, %v", done.Status, done.Reason, err)
	}
	evidence := store.ProjectRoutingEvidence(done, nil)
	if evidence.FirstDecision == nil || evidence.FirstDecision.ResolvedEffort != "high" {
		t.Fatalf("missing first decision effort: %+v", evidence.FirstDecision)
	}
	want := []store.RoutingExecutionAttempt{
		{Number: 1, Provider: "fixture-a", Model: "synthetic", Status: "FAILED", ReasonCode: "RATE_LIMIT"},
		{Number: 2, Provider: "fixture-b", Model: "synthetic", Status: "SUCCESS"},
	}
	if !reflect.DeepEqual(evidence.ExecutionAttempts, want) {
		t.Fatalf("fixture fallback evidence = %+v, want %+v", evidence.ExecutionAttempts, want)
	}
}
