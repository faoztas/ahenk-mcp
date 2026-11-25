// Copyright 2026 Furkan Anıl Öztaş
// SPDX-License-Identifier: Apache-2.0

package orchestration

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/faoztas/ahenk-mcp/internal/execution"
	"github.com/faoztas/ahenk-mcp/internal/model"
	"github.com/faoztas/ahenk-mcp/internal/providers"
	"github.com/faoztas/ahenk-mcp/internal/store"
)

func TestDispatchStartsHarnessForOptedInWorker(t *testing.T) {
	repo := canonicalTempDir(t)
	source := filepath.Join(repo, "fixture.txt")
	if err := os.WriteFile(source, []byte("before\n"), 0600); err != nil {
		t.Fatal(err)
	}
	setupTestGitRepo(t, repo)
	cfg := model.Config{StateDir: filepath.Join(canonicalTempDir(t), "state"), EnableFixture: true, Repositories: map[string]model.Repo{
		"synthetic": {Root: repo, AllowedPaths: []string{"fixture.txt"}, Providers: []string{"fixture-a"}},
	}}
	db, err := store.Open(cfg.StateDir)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	e, err := New(cfg, db, providers.New(cfg))
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	submit := func(t *testing.T, objective string) model.Job {
		t.Helper()
		job, err := e.Submit(model.Submission{RepoID: "synthetic", RequestID: strings.TrimPrefix(objective, "fixture:"), Task: model.Task{
			ExecutionProfile: "implementation", Mode: "patch", DataClass: "synthetic", PreferredProvider: "fixture-a",
			Objective: objective, AllowedPaths: []string{"fixture.txt"}, Acceptance: []model.Check{{ID: "edit", Description: "Edit the synthetic fixture"}},
		}})
		if err != nil {
			t.Fatal(err)
		}
		return job
	}
	t.Run("scoped worker edit and finish", func(t *testing.T) {
		job := submit(t, "fixture:harness")
		job, err := e.Wait(t.Context(), job.ID, 40*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		if job.Status != model.Completed || job.Result == nil {
			t.Fatalf("job %s: %s", job.Status, job.Reason)
		}
		if job.Result.Summary != "Fixture completed the harness edit." {
			t.Fatalf("missing harness finish: %q", job.Result.Summary)
		}
		patch, err := e.readVerifiedDiff(job, job.Result.ArtifactID, execution.MaxArtifact)
		if err != nil || !strings.Contains(string(patch), "+after via harness\n") || job.Result.Postimages["fixture.txt"] != model.Hash([]byte("after via harness\n")) {
			t.Fatalf("unexpected verified artifact: %q %v", patch, err)
		}
		original, err := os.ReadFile(source)
		if err != nil || string(original) != "before\n" {
			t.Fatalf("source changed: %q %v", original, err)
		}
	})
	t.Run("ordinary fixture has no socket", func(t *testing.T) {
		job := submit(t, "fixture:slow")
		dir := e.attemptDir(job.ID, 1)
		ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
		defer cancel()
		defer e.Cancel(job.ID)
		for {
			// The worktree exists and the supervisor holds its lease during dispatch.
			_, workErr := os.Stat(filepath.Join(dir, ".workspace", "work", "fixture.txt"))
			_, markerErr := os.Lstat(filepath.Join(dir, ".worker.cleaned"))
			if workErr == nil && errors.Is(markerErr, os.ErrNotExist) {
				break
			}
			select {
			case <-ctx.Done():
				t.Fatal("ordinary worker never acquired its lease")
			case <-time.After(10 * time.Millisecond):
			}
		}
		if _, err := os.Lstat(filepath.Join(dir, ".workspace", ".harness.sock")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("ordinary worker socket: %v", err)
		}
	})
}
