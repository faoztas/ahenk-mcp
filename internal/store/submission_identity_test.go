// Copyright 2026 Furkan Anıl Öztaş
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"database/sql"
	"errors"
	"path/filepath"
	"testing"

	"github.com/faoztas/ahenk-mcp/internal/model"
	"github.com/faoztas/ahenk-mcp/internal/orchestration"
	"github.com/faoztas/ahenk-mcp/internal/providers"
	"github.com/faoztas/ahenk-mcp/internal/store"
)

func identityConfig(t *testing.T) model.Config {
	return model.Config{StateDir: filepath.Join(t.TempDir(), "state"), Repositories: map[string]model.Repo{"repo": {Root: t.TempDir(), AllowedPaths: []string{"."}, Providers: []string{"codex", "agy"}}}}
}

func identityEngine(t *testing.T, cfg model.Config) (*orchestration.Engine, *store.Store) {
	t.Helper()
	db, err := store.Open(cfg.StateDir)
	if err != nil {
		t.Fatal(err)
	}
	e, err := orchestration.New(cfg, db, providers.Registry{})
	if err != nil {
		db.Close()
		t.Fatal(err)
	}
	return e, db
}

func intent(t *testing.T) model.Submission {
	t.Helper()
	in := model.DelegateTaskInput{Objective: "Inspect ordering", RequestID: "request", RepoID: "repo"}
	sub, _, _, err := in.Normalize("", nil)
	if err != nil {
		t.Fatal(err)
	}
	return sub
}

func routedSubmit(e *orchestration.Engine, sub model.Submission) (model.Job, error) {
	return e.SubmitRouted(sub, &model.InitialRoute{Provider: "agy", Model: "gemini", SelectedModel: "antigravity/gemini", ResolvedEffort: "medium", AutoRouted: true})
}

func TestSubmissionIdentityReadsLegacyAndRestarts(t *testing.T) {
	for _, kind := range []string{"legacy auto", "missing evidence", "wrong evidence", "exact legacy", "v2"} {
		t.Run(kind, func(t *testing.T) {
			cfg := identityConfig(t)
			cfg.Repositories["other"] = model.Repo{Root: t.TempDir(), AllowedPaths: []string{"."}}
			sub := intent(t)
			e, db := identityEngine(t, cfg)
			old := sub
			old.PreferredProvider = "codex"
			old.PreferredModel = "gpt"
			old.TargetEffort = "high"
			if fault := old.Task.Validate(cfg.Repositories["repo"]); fault != nil {
				t.Fatal(fault)
			}
			id := "original"
			if kind == "v2" {
				j, err := routedSubmit(e, sub)
				if err != nil {
					t.Fatal(err)
				}
				id = j.ID
			} else {
				j := model.Job{ID: id, RepoID: sub.RepoID, RequestID: sub.RequestID, Task: old.Task, PolicyDigest: model.Digest(cfg.Repositories["repo"]), Status: model.Completed}
				if _, err := db.Insert([]model.Job{j}, model.Digest(old)); err != nil {
					t.Fatal(err)
				}
				if kind == "legacy auto" || kind == "wrong evidence" {
					m := "codex/gpt"
					if kind == "wrong evidence" {
						m = "codex/other"
					}
					if err := db.RecordRoutingAudit(store.JobRoutingAudit{JobID: id, AttemptNumber: 1, AssignedModel: m, AssignedEffort: "high", DispatchStatus: "SUCCESS", TaskCategory: "coding", RouteReason: "NOMINAL_HIGH_EFFORT"}); err != nil {
						t.Fatal(err)
					}
				}
			}
			e.Close()
			db.Close()
			e, db = identityEngine(t, cfg)
			defer db.Close()
			defer e.Close()
			if kind == "exact legacy" {
				sub = old
			}
			j, err := e.Submit(sub)
			if kind == "missing evidence" || kind == "wrong evidence" {
				var fault *model.Fault
				if !errors.As(err, &fault) || fault.Code != "IDEMPOTENCY_CONFLICT" {
					t.Fatalf("unproven match: %v", err)
				}
				return
			}
			if err != nil || j.ID != id {
				t.Fatalf("retry after restart: %s %v", j.ID, err)
			}
			switch kind {
			case "v2":
				if j.InitialRoute == nil || j.InitialRoute.SelectedModel != "antigravity/gemini" || j.Task.PreferredProvider != "" || j.Task.PreferredModel != "" || j.Task.TargetEffort != "" {
					t.Fatalf("restart lost intent or first decision: %+v", j)
				}
			case "legacy auto":
				if j.InitialRoute == nil || j.InitialRoute.SelectedModel != "codex/gpt" {
					t.Fatal("legacy decision lost")
				}
				var conflict *model.Fault
				if _, err := e.Submit(old); !errors.As(err, &conflict) || conflict.Code != "IDEMPOTENCY_CONFLICT" {
					t.Fatalf("explicit target matching a legacy automatic route: got %v, want IDEMPOTENCY_CONFLICT", err)
				}
			}
			other := sub
			other.RepoID = "other"
			separate, err := e.Submit(other)
			if err != nil || separate.ID == id {
				t.Fatalf("request crossed repo boundary: %s %v", separate.ID, err)
			}
			sub.Objective += " changed"
			if _, err := e.Submit(sub); err == nil {
				t.Fatal("changed intent admitted")
			}
		})
	}
}

func TestSubmissionIdentityRollsBackOnAuditWriteFailure(t *testing.T) {
	cfg := identityConfig(t)
	e, db := identityEngine(t, cfg)
	defer db.Close()
	defer e.Close()
	raw, err := sql.Open("sqlite", filepath.Join(cfg.StateDir, "state.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	_, err = raw.Exec("CREATE TRIGGER reject_audit BEFORE INSERT ON job_routing_audit BEGIN SELECT RAISE(ABORT, 'injected audit failure'); END")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = routedSubmit(e, intent(t)); err == nil {
		t.Fatal("missing audit error")
	}
	for _, table := range []string{"jobs", "audit", "job_routing_audit"} {
		var count int
		if err := raw.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 0 {
			t.Fatalf("after audit failure %s has %d rows", table, count)
		}
	}
}
