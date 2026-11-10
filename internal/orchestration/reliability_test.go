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

func TestRestartFencesUnconfirmedWorker(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	repo := model.Repo{Root: t.TempDir()}
	now := time.Now().UTC()
	job := model.Job{ID: "crashed", RepoID: "demo", RequestID: "first", PolicyDigest: model.Digest(repo),
		Status: model.Running, CreatedAt: now, DeadlineAt: now.Add(time.Hour),
		Attempts: []model.Attempt{{Number: 1, StartedAt: now}}}
	if _, err := db.Insert([]model.Job{job}, ""); err != nil {
		t.Fatal(err)
	}
	dir := (&Engine{scratch: filepath.Join(db.Dir, "scratch")}).attemptDir(job.ID, 1)
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".worker.lock"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	e, err := New(model.Config{Repositories: map[string]model.Repo{"demo": repo}}, db, providers.Registry{})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	if got, err := e.Resume(job.ID, "resume"); err == nil || got.Status != model.Interrupted || len(got.Attempts) != 1 {
		t.Fatalf("resume bypassed worker cleanup: %+v %v", got, err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".worker.cleaned"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	got, err := e.Resume(job.ID, "resume")
	if err != nil || got.ResumeRequest != "resume" || len(got.Attempts) != 1 {
		t.Fatalf("confirmed cleanup did not permit resume: %+v %v", got, err)
	}
	if replay, err := e.Resume(job.ID, "resume"); err != nil || replay.ID != got.ID {
		t.Fatalf("resume did not replay idempotently: %+v %v", replay, err)
	}
}

func TestBatchCancellationPersistsBeforeSignallingWorkers(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	repo := model.Repo{}
	parent := model.Job{ID: "parent", RepoID: "demo", Status: model.Queued, PolicyDigest: model.Digest(repo), Children: []string{"running", "queued"}}
	running := model.Job{ID: "running", Parent: parent.ID, Status: model.Running}
	queued := model.Job{ID: "queued", Parent: parent.ID, Status: model.Queued}
	if _, err := db.Insert([]model.Job{parent, running, queued}, ""); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	e := &Engine{db: db, cfg: model.Config{Repositories: map[string]model.Repo{"demo": repo}},
		jobs:   map[string]model.Job{parent.ID: parent, running.ID: running, queued.ID: queued},
		active: map[string]context.CancelFunc{running.ID: cancel}}
	// A missing durable child forces the transaction to roll back before any signal.
	if err := db.Delete(queued.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Cancel(parent.ID); err == nil || ctx.Err() != nil || e.jobs[parent.ID].Status != model.Queued {
		t.Fatalf("cancellation escaped a failed transaction: %v, %v", err, ctx.Err())
	}
	if _, err := db.Insert([]model.Job{queued}, ""); err != nil {
		t.Fatal(err)
	}
	if got, err := e.Cancel(parent.ID); err != nil || got.Status != model.Cancelling || ctx.Err() == nil {
		t.Fatalf("batch cancellation: %+v %v", got, err)
	}
	rows, err := db.All()
	if err != nil || len(rows) != 3 {
		t.Fatalf("load cancellation: %v", err)
	}
	for _, row := range rows {
		want := model.Cancelling
		if row.ID == queued.ID {
			want = model.Cancelled
		}
		if row.Status != want {
			t.Fatalf("wrong durable state for %s: %s", row.ID, row.Status)
		}
	}
}

func TestCancelledIntentSurvivesRestartUntilCleanup(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	repo := model.Repo{}
	now := time.Now()
	job := model.Job{ID: "cancelling", RepoID: "demo", Status: model.Cancelling, PolicyDigest: model.Digest(repo),
		CreatedAt: now, DeadlineAt: now.Add(-time.Second), Attempts: []model.Attempt{{Number: 1, StartedAt: now}}}
	if _, err := db.Insert([]model.Job{job}, ""); err != nil {
		t.Fatal(err)
	}
	dir := (&Engine{scratch: filepath.Join(db.Dir, "scratch")}).attemptDir(job.ID, 1)
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".worker.lock"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	e, err := New(model.Config{Repositories: map[string]model.Repo{"demo": repo}}, db, providers.Registry{})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	e.mu.Lock()
	e.schedule(now)
	e.mu.Unlock()
	if got, err := e.Resume(job.ID, "resume"); err == nil || got.Status != model.Cancelling {
		t.Fatalf("restart or deadline overwrote cancellation: %+v %v", got, err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".worker.cleaned"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	e.mu.Lock()
	e.schedule(now)
	e.mu.Unlock()
	if got, err := e.Get(job.ID); err != nil || got.Status != model.Cancelled || len(got.Attempts) != 1 {
		t.Fatalf("confirmed cleanup did not finish cancellation: %+v %v", got, err)
	}
}
