// Copyright 2026 Furkan Anıl Öztaş
// SPDX-License-Identifier: Apache-2.0

package orchestration

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/faoztas/ahenk-mcp/internal/model"
	"github.com/faoztas/ahenk-mcp/internal/providers"
	"github.com/faoztas/ahenk-mcp/internal/store"
)

func setupTestEngine(t *testing.T) (*Engine, string) {
	t.Helper()
	dir := t.TempDir()
	dbDir := filepath.Join(dir, "state")
	if err := os.Mkdir(dbDir, 0700); err != nil {
		t.Fatal(err)
	}
	repoDir := filepath.Join(dir, "repo")
	if err := os.Mkdir(repoDir, 0700); err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(dbDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	cfg := model.Config{
		StateDir: dbDir,
		Repositories: map[string]model.Repo{
			"test-repo": {
				Root:         repoDir,
				AllowedPaths: []string{"."},
				Providers:    []string{"opencode"},
			},
		},
	}
	e, err := New(cfg, db, providers.Registry{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { e.Close() })
	return e, repoDir
}

func insertTestJob(t *testing.T, e *Engine, job model.Job) {
	t.Helper()
	e.mu.Lock()
	defer e.mu.Unlock()
	if _, err := e.db.Insert([]model.Job{job}, ""); err != nil {
		t.Fatal(err)
	}
	e.jobs[job.ID] = job
}

func TestEngine_Wait_ImmediateReturn(t *testing.T) {
	e, _ := setupTestEngine(t)
	jobID := "job-completed-1"
	now := time.Now().UTC()
	insertTestJob(t, e, model.Job{
		ID:        jobID,
		RepoID:    "test-repo",
		RequestID: "req-imm",
		CreatedAt: now,
		Status:    model.Completed,
		Result:    &model.Result{Summary: "Done instantly"},
	})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	j, err := e.Wait(ctx, jobID, 2*time.Second)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if j.Status != model.Completed {
		t.Fatalf("expected completed status, got %s", j.Status)
	}
	if j.Result == nil || j.Result.Summary != "Done instantly" {
		t.Fatalf("expected result summary, got %+v", j.Result)
	}
}

func TestEngine_Wait_WakeupOnComplete(t *testing.T) {
	e, _ := setupTestEngine(t)
	jobID := "job-running-1"
	now := time.Now().UTC()
	insertTestJob(t, e, model.Job{
		ID:         jobID,
		RepoID:     "test-repo",
		RequestID:  "req-wake",
		CreatedAt:  now,
		DeadlineAt: time.Now().Add(time.Hour),
		Status:     model.Running,
	})

	go func() {
		time.Sleep(100 * time.Millisecond)
		e.mu.Lock()
		j := e.jobs[jobID]
		j.Status = model.Completed
		j.Result = &model.Result{Summary: "Finished async"}
		e.persist(j)
		e.mu.Unlock()
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	start := time.Now()
	j, err := e.Wait(ctx, jobID, 3*time.Second)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if j.Status != model.Completed {
		t.Fatalf("expected COMPLETED, got %s", j.Status)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatalf("wait took too long, expected prompt wakeup: %v", time.Since(start))
	}
}

func TestEngine_Wait_Timeout(t *testing.T) {
	e, _ := setupTestEngine(t)
	jobID := "job-slow-1"
	now := time.Now().UTC()
	insertTestJob(t, e, model.Job{
		ID:         jobID,
		RepoID:     "test-repo",
		RequestID:  "req-slow",
		CreatedAt:  now,
		DeadlineAt: now.Add(time.Hour),
		Status:     model.Running,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	start := time.Now()
	j, err := e.Wait(ctx, jobID, 150*time.Millisecond)
	if err != nil {
		t.Fatalf("expected no error on timeout, got %v", err)
	}
	if j.Status != model.Running {
		t.Fatalf("expected status to remain Running on timeout, got %s", j.Status)
	}
	if time.Since(start) > 500*time.Millisecond {
		t.Fatalf("expected timeout after ~150ms, took %v", time.Since(start))
	}
}

func TestEngine_WaitJobs_All(t *testing.T) {
	e, _ := setupTestEngine(t)
	j1, j2 := "job-multi-1", "job-multi-2"
	now := time.Now().UTC()
	insertTestJob(t, e, model.Job{ID: j1, RepoID: "test-repo", RequestID: "req-m1", CreatedAt: now, DeadlineAt: now.Add(time.Hour), Status: model.Running})
	insertTestJob(t, e, model.Job{ID: j2, RepoID: "test-repo", RequestID: "req-m2", CreatedAt: now, DeadlineAt: now.Add(time.Hour), Status: model.Running})

	go func() {
		time.Sleep(80 * time.Millisecond)
		e.mu.Lock()
		job1 := e.jobs[j1]
		job1.Status = model.Completed
		e.persist(job1)
		e.mu.Unlock()

		time.Sleep(80 * time.Millisecond)
		e.mu.Lock()
		job2 := e.jobs[j2]
		job2.Status = model.Completed
		e.persist(job2)
		e.mu.Unlock()
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	results, err := e.WaitJobs(ctx, []string{j1, j2}, "all", 3*time.Second)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(results))
	}
	for _, res := range results {
		if res.Status != model.Completed {
			t.Fatalf("expected all jobs completed, got %+v", res)
		}
	}
}

func TestEngine_WaitJobs_Any(t *testing.T) {
	e, _ := setupTestEngine(t)
	j1, j2 := "job-any-1", "job-any-2"
	now := time.Now().UTC()
	insertTestJob(t, e, model.Job{ID: j1, RepoID: "test-repo", RequestID: "req-any1", CreatedAt: now, DeadlineAt: now.Add(time.Hour), Status: model.Running})
	insertTestJob(t, e, model.Job{ID: j2, RepoID: "test-repo", RequestID: "req-any2", CreatedAt: now, DeadlineAt: now.Add(time.Hour), Status: model.Running})

	go func() {
		time.Sleep(80 * time.Millisecond)
		e.mu.Lock()
		job1 := e.jobs[j1]
		job1.Status = model.Completed
		e.persist(job1)
		e.mu.Unlock()
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	results, err := e.WaitJobs(ctx, []string{j1, j2}, "any", 3*time.Second)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(results))
	}
	// At least one must be completed
	hasCompleted := results[0].Status == model.Completed || results[1].Status == model.Completed
	if !hasCompleted {
		t.Fatalf("expected at least one completed job in mode:any")
	}
}
