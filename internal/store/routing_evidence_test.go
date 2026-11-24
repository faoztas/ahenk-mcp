// Copyright 2026 Furkan Anıl Öztaş
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"database/sql"
	"math"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/faoztas/ahenk-mcp/internal/model"
	"github.com/faoztas/ahenk-mcp/internal/store"
)

func TestRoutingAuditFirstDecisionAndAttemptsStayDistinct(t *testing.T) {
	cfg := identityConfig(t)
	e, db := identityEngine(t, cfg)
	sub := intent(t)
	job, err := routedSubmit(e, sub)
	if err != nil {
		t.Fatal(err)
	}
	e.Close()

	now := time.Now().UTC()
	job.Attempts = []model.Attempt{
		{Number: 1, Provider: "agy", Model: "gemini", StartedAt: now, FinishedAt: &now, Code: "RATE_LIMIT", HTTPStatus: 429},
		{Number: 2, Provider: "codex", Model: "gpt", StartedAt: now, FinishedAt: &now},
	}
	job.Status = model.Completed
	// A terminal snapshot must settle the legacy row before that snapshot becomes durable.
	if err := db.Save(job); err != nil {
		t.Fatal(err)
	}
	rows, err := db.GetRoutingAudit(job.ID)
	if err != nil || len(rows) != 1 || rows[0].DispatchStatus != "SUCCESS" || rows[0].AssignedModel != "antigravity/gemini" {
		t.Fatalf("terminal save must settle the one original routing row: %+v, %v", rows, err)
	}
	db.Close()

	e, db = identityEngine(t, cfg)
	defer db.Close()
	defer e.Close()
	retry, err := e.SubmitRouted(sub, &model.InitialRoute{Provider: "codex", Model: "gpt", SelectedModel: "codex/gpt"})
	if err != nil {
		t.Fatal(err)
	}
	if retry.ID != job.ID || retry.InitialRoute == nil || !reflect.DeepEqual(job.InitialRoute, retry.InitialRoute) ||
		!reflect.DeepEqual(job.Attempts, retry.Attempts) || retry.Status != model.Completed {
		t.Fatalf("restart/retry changed original routing or execution evidence: %+v", retry)
	}
	if err := db.UpdateRoutingAuditStatus(job.ID, 1, "FAILED", 999); err != nil {
		t.Fatal(err)
	}
	after, err := db.GetRoutingAudit(job.ID)
	if err != nil || !reflect.DeepEqual(rows, after) {
		t.Fatalf("terminal row changed after restart/retry/replay: %+v, %v", after, err)
	}
}

func TestRestartReconcilesLegacyDispatch(t *testing.T) {
	cfg := identityConfig(t)
	db, err := store.Open(cfg.StateDir)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	job := model.Job{ID: "legacy", RequestID: "legacy", RepoID: "repo", PolicyDigest: cfg.Repositories["repo"].PolicyDigest(), Status: model.Completed, CreatedAt: now}
	if _, err := db.Insert([]model.Job{job}, job.ID); err != nil {
		t.Fatal(err)
	}
	for i, status := range []string{"DISPATCHED", "FAILED", "DISPATCHED"} {
		if err := db.RecordRoutingAudit(store.JobRoutingAudit{JobID: job.ID, AttemptNumber: i + 1, AssignedModel: "codex/gpt", DispatchStatus: status, LatencyMs: 17, CreatedAt: now}); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()
	// Reject one row after SQLite has begun the repair. The failed open must
	// release its ownership lock and leave the entire pre-restart state intact.
	raw, err := sql.Open("sqlite", filepath.Join(cfg.StateDir, "state.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	if _, err := raw.Exec(`CREATE TRIGGER reject_repair BEFORE UPDATE ON job_routing_audit
 WHEN old.attempt_number=3 BEGIN SELECT RAISE(FAIL,'fixture repair failure'); END`); err != nil {
		t.Fatal(err)
	}
	if reopened, err := store.Open(cfg.StateDir); err == nil {
		reopened.Close()
		t.Fatal("startup ignored a failed repair")
	}
	var pending int
	if err := raw.QueryRow("SELECT count(*) FROM job_routing_audit WHERE dispatch_status='DISPATCHED'").Scan(&pending); err != nil || pending != 2 {
		t.Fatalf("partial repair, pending=%d err=%v", pending, err)
	}
	if _, err := raw.Exec("DROP TRIGGER reject_repair"); err != nil {
		t.Fatal(err)
	}

	var previous []store.JobRoutingAudit
	for reopen := 0; reopen < 2; reopen++ {
		e, db := identityEngine(t, cfg)
		got, rows, err := e.GetWithRoutingAudit(job.ID)
		e.Close()
		db.Close()
		if err != nil || got.Status != model.Completed || len(got.Attempts) != 0 || len(rows) != 3 || rows[2].DispatchStatus != "SUCCESS" || rows[0].DispatchStatus != "SUCCESS" || rows[1].DispatchStatus != "FAILED" || rows[1].LatencyMs != 17 {
			t.Fatalf("restart %d returned %+v, %v", reopen, rows, err)
		}
		if reopen > 0 && !reflect.DeepEqual(previous, rows) {
			t.Fatal("restart changed settled rows")
		}
		previous = rows
	}
}

func TestRoutingEvidenceScoreHistoricalOverflow(t *testing.T) {
	ulp := math.Nextafter(1, 2)
	for _, tc := range []struct{ in, want float64 }{{ulp, 1}, {1, 1}, {.75, .75}, {math.Nextafter(ulp, 2), 0}, {1.2, 0}, {-1, 0}, {math.NaN(), 0}, {math.Inf(1), 0}} {
		got := store.ProjectRoutingEvidence(model.Job{InitialRoute: &model.InitialRoute{ProfileScore: tc.in}}, nil)
		if got.FirstDecision.ProfileScore != tc.want {
			t.Errorf("score %.17g got %g", tc.in, got.FirstDecision.ProfileScore)
		}
	}
}

func TestPrivateRoutingReasonProjection(t *testing.T) {
	for _, reason := range []string{"DATA_CLASS_MISMATCH", "DETERMINISTIC_CHECK_REQUIRED", "BRIEF_NOT_APPROVED", "PRIVATE_GRANT_REQUIRED", "WRITER_ISOLATION_UNVERIFIED", "not-a-gate"} {
		want := reason
		if reason == "not-a-gate" {
			want = ""
		}
		if r := store.ProjectRoutingEvidence(model.Job{Reason: reason}, nil); r.ReasonCode != want {
			t.Error("missing safe reason", reason)
		}
	}
}
