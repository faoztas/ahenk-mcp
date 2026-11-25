// Copyright 2026 Furkan Anıl Öztaş
// SPDX-License-Identifier: Apache-2.0

package mcpserver

import (
	"context"
	"encoding/json"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/faoztas/ahenk-mcp/internal/model"
	"github.com/faoztas/ahenk-mcp/internal/orchestration"
	"github.com/faoztas/ahenk-mcp/internal/providers"
	"github.com/faoztas/ahenk-mcp/internal/store"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestMCPRoutingAuditShowsProfileSourceAndActualAttempt(t *testing.T) {
	cfg := model.Config{StateDir: filepath.Join(t.TempDir(), "state"), Repositories: map[string]model.Repo{
		"allowed": {Root: t.TempDir(), AllowedPaths: []string{"."}},
	}}
	db, err := store.Open(cfg.StateDir)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	now := time.Now().UTC()
	initial := &model.InitialRoute{
		Provider: "codex", Model: "gpt-6.1-sol", SelectedModel: "codex/gpt-6.1-sol",
		ResolvedEffort: "high", Reason: "QUOTA_PRESSURE_THROTTLED", QuotaPressure: 0.75, ProfileScore: 1,
	}
	job := model.Job{
		ID: "fallback", RepoID: "allowed", PolicyDigest: cfg.Repositories["allowed"].PolicyDigest(),
		Status: model.Completed, InitialRoute: initial, RoutingProfile: model.RoutingProfileQuality, RoutingProfileSource: "operator",
		Task: model.Task{Objective: "fixture-objective-hidden"}, Diff: "fixture-diff-hidden",
		Attempts: []model.Attempt{
			{Number: 1, Provider: "codex", Model: "gpt-6.1-sol", StartedAt: now, FinishedAt: &now, Code: "RATE_LIMIT",
				HTTPStatus: 429, ProviderReason: "fixture-provider-message-hidden"},
			{Number: 2, Provider: "opencode", Model: "namespace/fallback-model", StartedAt: now, FinishedAt: &now,
				OpenCodeAccount: "fixture-account-hidden", OpenCodeEndpoint: "https://example.invalid/private", Fingerprint: "fixture-fingerprint-hidden",
				Handoffs: []model.HandoffManifest{{}}},
		},
	}
	jobs := []model.Job{
		job,
		{ID: "no-run", RepoID: job.RepoID, PolicyDigest: job.PolicyDigest, Status: model.NotRun, Reason: "NO_ELIGIBLE_ROUTE", InitialRoute: initial},
		{ID: "agy-family-wait", RepoID: job.RepoID, PolicyDigest: job.PolicyDigest, Status: model.Interrupted,
			Reason: "AGY_FAMILY_IN_FLIGHT", DeadlineAt: now.Add(time.Hour)},
		{ID: "no-price", RepoID: job.RepoID, PolicyDigest: job.PolicyDigest, Status: model.Blocked, Reason: "PRICE_EVIDENCE_REQUIRED"},
		{ID: "critical", RepoID: job.RepoID, PolicyDigest: job.PolicyDigest, Status: model.Blocked, Reason: "QUOTA_CRITICAL_HEADROOM"},
		{ID: "quota-blocked", RepoID: job.RepoID, PolicyDigest: job.PolicyDigest, Status: model.Blocked, Reason: "QUOTA_EXHAUSTED", InitialRoute: initial},
		{ID: "cancelled", RepoID: job.RepoID, PolicyDigest: job.PolicyDigest, Status: model.Cancelled, Reason: "CLIENT_CANCELLED", InitialRoute: initial},
		{ID: "historical", RepoID: job.RepoID, PolicyDigest: job.PolicyDigest, Status: model.Completed, Attempts: []model.Attempt{{Number: 1}}},
		{ID: "legacy-selection", RepoID: job.RepoID, PolicyDigest: job.PolicyDigest, Status: model.Completed},
		{ID: "different-repo", RepoID: "other", Status: model.Completed, InitialRoute: initial},
	}
	for _, j := range jobs {
		j.RequestID = j.ID
		j.CreatedAt = now
		if _, err := db.Insert([]model.Job{j}, j.ID); err != nil {
			t.Fatal(err)
		}
	}
	for _, id := range []string{job.ID, "legacy-selection"} {
		if err := db.RecordRoutingAudit(store.JobRoutingAudit{JobID: id, AttemptNumber: 1, AssignedModel: initial.SelectedModel, DispatchStatus: "SUCCESS"}); err != nil {
			t.Fatal(err)
		}
	}

	e, err := orchestration.New(cfg, db, providers.Registry{})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	session := connectRoutingServer(t, e, cfg, providers.Registry{})
	call := func(tool, id string) map[string]any {
		t.Helper()
		out, err := callMCPTool(session, tool, map[string]any{"job_id": id})
		if err != nil {
			t.Fatal(err)
		}
		return out
	}

	audit := call("get_routing_audit", job.ID)
	if audit["job_id"] != job.ID || audit["routing_profile"] != "quality" || audit["routing_profile_source"] != "operator" {
		t.Fatalf("missing persisted routing profile and source: %+v", audit)
	}
	first, ok := audit["first_decision"].(map[string]any)
	if !ok || first["selected_model"] != "codex/gpt-6.1-sol" || first["resolved_effort"] != "high" ||
		first["reason"] != "QUOTA_PRESSURE_THROTTLED" || first["quota_pressure"] != 0.75 || first["profile_score"] != 1.0 {
		t.Fatalf("invalid first routing decision: %+v", audit["first_decision"])
	}
	legacy, ok := audit["attempts"].([]any)
	if !ok || len(legacy) != 1 || legacy[0].(map[string]any)["dispatch_status"] != "SUCCESS" {
		t.Fatalf("legacy routing rows were changed: %+v", audit["attempts"])
	}
	actual, ok := audit["execution_attempts"].([]any)
	if !ok || len(actual) != 2 {
		t.Fatalf("expected two actual attempts: %+v", audit["execution_attempts"])
	}
	rateLimited := actual[0].(map[string]any)
	if rateLimited["number"] != float64(1) || rateLimited["model"] != "gpt-6.1-sol" || rateLimited["status"] != "FAILED" ||
		rateLimited["reason_code"] != "RATE_LIMIT" || rateLimited["http_status"] != float64(429) {
		t.Fatalf("invalid rate-limited attempt: %+v", rateLimited)
	}
	fallback := actual[1].(map[string]any)
	if fallback["number"] != float64(2) || fallback["status"] != "SUCCESS" || fallback["model"] != "namespace/fallback-model" ||
		fallback["provider"] != "opencode" || audit["status"] != "COMPLETED" {
		t.Fatalf("invalid fallback evidence or terminal status: %+v", audit)
	}
	status := call("get_job_status", job.ID)
	for _, key := range []string{"routing_profile", "routing_profile_source", "first_decision", "execution_attempts", "status", "reason_code"} {
		if !reflect.DeepEqual(audit[key], status[key]) {
			t.Errorf("%s differs across handlers: %v, %v", key, audit[key], status[key])
		}
	}
	for _, out := range []map[string]any{audit, status} {
		wire, err := json.Marshal(out)
		if err != nil {
			t.Fatal(err)
		}
		for _, forbidden := range []string{"fixture-", "example.invalid", "opencode_account", "opencode_endpoint", "handoffs", "fingerprint", "provider_reason"} {
			if strings.Contains(string(wire), forbidden) {
				t.Errorf("routing evidence includes %q", forbidden)
			}
		}
	}

	for _, tc := range []struct{ id, status, reason string }{
		{"no-run", "NOT_RUN", "NO_ELIGIBLE_ROUTE"},
		{"quota-blocked", "BLOCKED", "QUOTA_EXHAUSTED"},
		{"cancelled", "CANCELLED", "CLIENT_CANCELLED"},
	} {
		out := call("get_routing_audit", tc.id)
		if out["status"] != tc.status || out["reason_code"] != tc.reason ||
			!reflect.DeepEqual(out["execution_attempts"], []any{}) || out["first_decision"] == nil {
			t.Errorf("incorrect evidence for %s: %+v", tc.id, out)
		}
	}
	for _, tool := range []string{"get_job_status", "get_routing_audit"} {
		out := call(tool, "agy-family-wait")
		if out["reason_code"] != "AGY_FAMILY_IN_FLIGHT" {
			t.Errorf("%s dropped the persisted AGY family reason: %+v", tool, out)
		}
	}

	for id, reason := range map[string]string{"no-price": "PRICE_EVIDENCE_REQUIRED", "critical": "QUOTA_CRITICAL_HEADROOM"} {
		for _, tool := range []string{"get_routing_audit", "get_job_status"} {
			out := call(tool, id)
			if out["status"] != model.Blocked || out["reason_code"] != reason || len(out["execution_attempts"].([]any)) != 0 {
				t.Error(id, tool, out)
			}
		}
	}
	historical := call("get_routing_audit", "historical")
	if _, exists := historical["first_decision"]; exists || historical["routing_profile_source"] != "legacy" ||
		!reflect.DeepEqual(historical["execution_attempts"], []any{map[string]any{"number": float64(1), "status": "UNKNOWN"}}) {
		t.Errorf("missing historical fields should stay unknown: %+v", historical)
	}
	legacyOnly := call("get_routing_audit", "legacy-selection")
	if _, exists := legacyOnly["first_decision"]; exists || !reflect.DeepEqual(legacyOnly["execution_attempts"], []any{}) ||
		len(legacyOnly["attempts"].([]any)) != 1 {
		t.Errorf("legacy selection was reported as a first decision or actual attempt: %+v", legacyOnly)
	}

	for id, code := range map[string]string{"different-repo": "NOT_FOUND", "../bad": "INVALID_INPUT"} {
		for _, tool := range []string{"get_routing_audit", "get_job_status"} {
			out := call(tool, id)
			if out["code"] != code || out["job_id"] != nil || out["first_decision"] != nil || out["execution_attempts"] != nil {
				t.Errorf("%s disclosed evidence for %s: %+v", tool, id, out)
			}
		}
	}
}

func TestMCPRoutingEvidenceHistoricalRowsAndStatus(t *testing.T) {
	cfg := model.Config{StateDir: filepath.Join(t.TempDir(), "state"), Repositories: map[string]model.Repo{
		"allowed": {Root: t.TempDir(), AllowedPaths: []string{"."}},
	}}
	db, err := store.Open(cfg.StateDir)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	// Keep fixture jobs inside retention while the scheduler serves MCP reads.
	now := time.Now().UTC().Truncate(time.Second)
	digest := cfg.Repositories["allowed"].PolicyDigest()
	const rawError = "historical failure at https://operator.invalid/private"
	const unknownCode = "HISTORICAL_ACCOUNT_LABEL"
	longHistory := make([]model.Attempt, 65)
	for i := range longHistory {
		longHistory[i] = model.Attempt{Number: i + 1, Provider: "codex", StartedAt: now, FinishedAt: &now, Code: "RATE_LIMIT"}
	}
	longHistory[len(longHistory)-1].FinishedAt = nil
	longHistory[len(longHistory)-1].Code = ""
	const dotted = "model.alpha"
	const hostPort = "operator.invalid:443"
	const fakeSecret = "sk-fixture-only-abcdefghijklmnopqrstuvwxyz.alpha"
	reg := providers.Registry{Candidates: []model.Capability{
		{Provider: "fixture", Model: dotted, Status: "available"},
		{Provider: "fixture", Model: hostPort, Status: "available"},
		{Provider: "fixture", Model: fakeSecret, Status: "available"},
		{Provider: "fixture", Model: "model.unverified", Status: "unverified"},
	}}
	for _, j := range []model.Job{
		{ID: "dotted", Status: model.Failed, Reason: "ROLE_CALIBRATION_FAILED", InitialRoute: &model.InitialRoute{Provider: "fixture", Model: dotted, SelectedModel: "fixture/" + dotted, Reason: "ROLE_CALIBRATION_FAILED"}, Attempts: []model.Attempt{
			{Provider: "fixture", Model: dotted, Code: "ROLE_CALIBRATION_FAILED"},
			{Provider: "another", Model: dotted},
			{Provider: "fixture", Model: "model.unverified"},
			{Provider: "fixture", Model: hostPort},
			{Provider: "fixture", Model: fakeSecret},
			{Provider: "fixture", Model: "operator.invalid/private"},
		}},
		{ID: "historical-errors", Status: model.Failed, Reason: rawError},
		{ID: "historical-code", Status: model.Failed, Reason: unknownCode},
		{ID: "stable-code", Status: model.Blocked, Reason: "REPO_POLICY_CHANGED"},
		{ID: "http-code", Status: model.Failed, Reason: "OPENCODE_UPSTREAM_HTTP_400"},
		{ID: "long-history", Status: model.Paused, Attempts: longHistory},
		{ID: "at-history-limit", Status: model.Failed, Attempts: longHistory[:32]},
		{ID: "empty-history", Status: model.NotRun, Reason: "NO_ELIGIBLE_ROUTE"},
	} {
		j.RepoID, j.PolicyDigest, j.RequestID, j.CreatedAt = "allowed", digest, j.ID, now
		j.DeadlineAt = now.Add(time.Hour)
		if _, err := db.Insert([]model.Job{j}, j.ID); err != nil {
			t.Fatal(err)
		}
	}
	const testKey = "sk-fixture-only-abcdefghijklmnopqrstuvwxyz"
	for _, row := range []store.JobRoutingAudit{
		{AttemptNumber: 1, TaskCategory: "code_review", AssignedModel: "opencode/namespace/model-2.5:free",
			AssignedEffort: "max", RouteReason: "EXPLICIT_TARGET_EFFORT", DispatchStatus: "SUCCESS"},
		{AttemptNumber: 2, TaskCategory: "fixture_account", AssignedModel: "https://operator.invalid/private",
			AssignedEffort: testKey, RouteReason: rawError, DispatchStatus: "fixture-raw-status"},
		{AttemptNumber: 3, AssignedModel: "operator.invalid/private", RouteReason: unknownCode},
		{AttemptNumber: 4, AssignedModel: testKey},
		{AttemptNumber: 5, AssignedModel: "localhost:4096/private"},
		{AttemptNumber: 6, AssignedModel: strings.Repeat("x/", 129) + "model"},
	} {
		row.JobID, row.CreatedAt, row.QuotaPressureAtDispatch, row.LatencyMs = "historical-errors", now, 0.75, 27
		if err := db.RecordRoutingAudit(row); err != nil {
			t.Fatal(err)
		}
	}

	if err := db.RecordRoutingAudit(store.JobRoutingAudit{JobID: "dotted", AttemptNumber: 1, AssignedModel: "fixture/" + dotted, RouteReason: "ROLE_CALIBRATION_FAILED", DispatchStatus: "FAILED"}); err != nil {
		t.Fatal(err)
	}
	if err := db.RecordRoutingAudit(store.JobRoutingAudit{JobID: "dotted", AttemptNumber: 2, AssignedModel: "fixture/" + hostPort, DispatchStatus: "FAILED"}); err != nil {
		t.Fatal(err)
	}
	if err := db.RecordRoutingAudit(store.JobRoutingAudit{JobID: "dotted", AttemptNumber: 3, AssignedModel: dotted, DispatchStatus: "FAILED"}); err != nil {
		t.Fatal(err)
	}
	e, err := orchestration.New(cfg, db, reg)
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	session := connectRoutingServer(t, e, cfg, reg)
	call := func(t *testing.T, tool, id string) map[string]any {
		t.Helper()
		out, err := callMCPTool(session, tool, map[string]any{"job_id": id})
		if err != nil {
			t.Fatal(err)
		}
		return out
	}

	for _, tool := range []string{"get_routing_audit", "get_job_status"} {
		out := call(t, tool, "dotted")
		first := out["first_decision"].(map[string]any)
		attempts := out["execution_attempts"].([]any)
		if first["model"] != dotted || first["selected_model"] != "fixture/"+dotted || attempts[0].(map[string]any)["model"] != dotted {
			t.Errorf("%s dropped registered dotted identity", tool)
		}
		if first["reason"] != "ROLE_CALIBRATION_FAILED" || out["reason_code"] != "ROLE_CALIBRATION_FAILED" || attempts[0].(map[string]any)["reason_code"] != "ROLE_CALIBRATION_FAILED" {
			t.Errorf("%s dropped calibration reason", tool)
		}
		for _, a := range attempts[1:] {
			if a.(map[string]any)["model"] != nil {
				t.Errorf("%s disclosed unverified identity", tool)
			}
		}
		if tool == "get_routing_audit" {
			rows := out["attempts"].([]any)
			if rows[2].(map[string]any)["assigned_model"] != dotted {
				t.Error("unqualified registered selection lost")
			}
			if rows[1].(map[string]any)["assigned_model"] != "" {
				t.Error("legacy row disclosed an explicit host and port")
			}
			row := rows[0].(map[string]any)
			if row["assigned_model"] != "fixture/"+dotted || row["route_reason"] != "ROLE_CALIBRATION_FAILED" {
				t.Error("legacy dotted model or calibration reason lost")
			}
		}
	}
	t.Run("legacy rows and values", func(t *testing.T) {
		out := call(t, "get_routing_audit", "historical-errors")
		rows, ok := out["attempts"].([]any)
		if !ok || len(rows) != 6 {
			t.Fatal("expected six legacy rows")
		}
		for i, raw := range rows {
			want := map[string]any{
				"job_id": "historical-errors", "attempt_number": float64(i + 1), "created_at": now.Format(time.RFC3339),
				"quota_pressure_at_dispatch": 0.75, "latency_ms": float64(27),
				"task_category": "", "assigned_model": "", "assigned_effort": "", "route_reason": "", "dispatch_status": "",
			}
			if i == 0 {
				want["task_category"], want["assigned_model"] = "code_review", "opencode/namespace/model-2.5:free"
				want["assigned_effort"], want["route_reason"], want["dispatch_status"] = "max", "EXPLICIT_TARGET_EFFORT", "SUCCESS"
			}
			if !reflect.DeepEqual(raw, want) {
				t.Errorf("legacy row %d changed shape or exposed a non-routing value", i+1)
			}
		}
	})
	for _, tool := range []string{"get_job_status", "get_routing_audit"} {
		t.Run(tool+"/reason codes", func(t *testing.T) {
			for _, tc := range []struct{ id, code string }{
				{"historical-errors", ""}, {"historical-code", ""}, {"stable-code", "REPO_POLICY_CHANGED"},
				{"http-code", "OPENCODE_UPSTREAM_HTTP_400"}, {"empty-history", "NO_ELIGIBLE_ROUTE"},
			} {
				if out := call(t, tool, tc.id); out["reason_code"] != tc.code {
					t.Errorf("%s did not return the expected safe reason code", tc.id)
				}
			}
		})
		t.Run(tool+"/recent history", func(t *testing.T) {
			out := call(t, tool, "long-history")
			attempts, ok := out["execution_attempts"].([]any)
			if !ok || len(attempts) != 32 || out["execution_attempts_truncated"] != true {
				t.Fatal("expected a marked, bounded window of execution attempts")
			}
			for i, raw := range attempts {
				if raw.(map[string]any)["number"] != float64(34+i) {
					t.Errorf("execution attempts are out of order at index %d", i)
				}
			}
			last := attempts[len(attempts)-1].(map[string]any)
			if last["status"] != "UNFINISHED" || last["reason_code"] != nil {
				t.Error("latest unfinished execution attempt was lost")
			}
			if tool == "get_job_status" && out["attempt_count"] != float64(65) {
				t.Error("status lost the total attempt count")
			}
			atLimit := call(t, tool, "at-history-limit")
			if attempts, ok := atLimit["execution_attempts"].([]any); !ok || len(attempts) != 32 || atLimit["execution_attempts_truncated"] != nil {
				t.Error("execution history at the limit should be complete")
			}
		})
		t.Run(tool+"/empty history", func(t *testing.T) {
			out := call(t, tool, "empty-history")
			if !reflect.DeepEqual(out["execution_attempts"], []any{}) || out["execution_attempts_truncated"] != nil {
				t.Error("empty execution evidence should be an untruncated array")
			}
			if tool == "get_routing_audit" && !reflect.DeepEqual(out["attempts"], []any{}) {
				t.Error("empty legacy history should remain an array")
			}
		})
	}
}

// Inject the stored status at the MCP boundary because startup recovery changes
// unrecognized nonterminal job states before ordinary tool calls can read them.
func TestHistoricalStatusMCPBoundary(t *testing.T) {
	server := mcp.NewServer(&mcp.Implementation{Name: "fixture", Version: "1"}, nil)
	add(server, nil, "fixture_status", "fixture", true, func(_ context.Context, in listInput) (response, error) {
		return response{"job_id": "fixture", "status": in.Status}, nil
	})
	ct, st := mcp.NewInMemoryTransports()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go server.Run(ctx, st)
	session, err := mcp.NewClient(&mcp.Implementation{Name: "fixture", Version: "1"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	for _, st := range []string{model.Created, model.Queued, model.Running, model.Verifying, model.RetryWait, model.Cancelling, model.Paused, model.Interrupted, model.Completed, model.Failed, model.Cancelled, model.TimedOut, model.Blocked, model.NotRun, "historical https://fixture.invalid/status"} {
		res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "fixture_status", Arguments: listInput{Status: st}})
		if err != nil {
			t.Fatal(err)
		}
		want := st
		if strings.HasPrefix(st, "historical") {
			want = "UNKNOWN"
		}
		if res.StructuredContent.(map[string]any)["status"] != want || res.Content[0].(*mcp.TextContent).Text != "Ahenk fixture_status "+want {
			t.Errorf("incorrect structured or summary status for %q", st)
		}
	}
}

func TestMCPResponsesNeverExposeAccountIdentity(t *testing.T) {
	now := time.Now().UTC()
	cfg := model.Config{StateDir: filepath.Join(t.TempDir(), "state"), Repositories: map[string]model.Repo{"repo": {Root: t.TempDir(), AllowedPaths: []string{"."}, Providers: []string{"opencode"}}}}
	db, err := store.Open(cfg.StateDir)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	raw, digest := "private-fixture-attestation", model.Hash([]byte("private-fixture-attestation"))
	cost := &model.CostEvidence{Policy: "FREE_ONLY", Account: "private-fixture-cost", VerifiedRoute: &model.CostRoute{Provider: "opencode", Account: "private-fixture-receipt"}}
	c := model.Capability{Provider: "opencode", Model: "fixture-model", Status: "available", Mode: "tool_free", Cost: cost, AttestedAccount: raw, AttestedAccountSHA256: digest, OpenCodeAccount: "private-fixture-route"}
	reg := providers.Registry{Candidates: []model.Capability{c}}
	job := model.Job{ID: "historical-private", RequestID: "legacy", RepoID: "repo", PolicyDigest: cfg.Repositories["repo"].PolicyDigest(), CreatedAt: now, Status: model.Completed,
		Result: &model.Result{Summary: "public summary", Cost: cost}, Route: []model.Decision{{Provider: c.Provider, Model: c.Model, Cost: cost, OpenCodeAccount: c.OpenCodeAccount}},
		Attempts: []model.Attempt{{Provider: c.Provider, Model: c.Model, Number: 1, FinishedAt: &now, OpenCodeAccount: c.OpenCodeAccount, Cost: cost, AGY: &model.AGYRunEvidence{Cost: cost, ReserveAccountSHA256: digest}}}}
	if _, err := db.Insert([]model.Job{job}, job.ID); err != nil {
		t.Fatal(err)
	}
	if err := db.RecordRoutingAudit(store.JobRoutingAudit{JobID: job.ID, AssignedModel: c.Model, AttemptNumber: 1}); err != nil {
		t.Fatal(err)
	}
	e, err := orchestration.New(cfg, db, reg)
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	cs := connectRoutingServer(t, e, cfg, reg)
	for _, tc := range []struct {
		name   string
		args   map[string]any
		public string
	}{
		{"list_capabilities", map[string]any{"repo_id": "repo", "include_quota": true}, "fixture-model"},
		{"get_provider_quotas", map[string]any{}, "circuit_state"},
		{"get_job_result", map[string]any{"job_id": job.ID}, "public summary"},
		{"get_job_result", map[string]any{"job_id": job.ID, "view": "route"}, "fixture-model"},
		{"get_job_status", map[string]any{"job_id": job.ID}, "execution_attempts"},
		{"get_routing_audit", map[string]any{"job_id": job.ID}, "execution_attempts"},
		{"wait_jobs", map[string]any{"job_ids": []string{job.ID}}, "public summary"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: tc.name, Arguments: tc.args})
			if err != nil || r.IsError {
				t.Fatalf("call: %v %+v", err, r)
			}
			b, err := json.Marshal(r)
			if err != nil || !strings.Contains(string(b), tc.public) {
				t.Fatalf("missing public field %q: %v", tc.public, err)
			}
			for _, marker := range []string{raw, digest, c.OpenCodeAccount, cost.Account, cost.VerifiedRoute.Account, `"attested_account"`, `"attested_account_sha256"`, `"opencode_account"`, `"account"`} {
				if strings.Contains(string(b), marker) {
					t.Error("identity exposed", marker)
				}
			}
		})
	}
	stored, err := db.All()
	if err != nil || stored[0].Attempts[0].Cost.VerifiedRoute.Account != cost.VerifiedRoute.Account {
		t.Fatal("private receipt changed", err)
	}
}
