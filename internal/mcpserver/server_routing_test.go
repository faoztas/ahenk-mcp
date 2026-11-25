// Copyright 2026 Furkan Anıl Öztaş
// SPDX-License-Identifier: Apache-2.0

package mcpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/faoztas/ahenk-mcp/internal/model"
	"github.com/faoztas/ahenk-mcp/internal/orchestration"
	"github.com/faoztas/ahenk-mcp/internal/providers"
	"github.com/faoztas/ahenk-mcp/internal/store"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func setupTestMCPServer(t *testing.T) (*mcp.ClientSession, *store.Store) {
	t.Helper()
	session, db, _ := routingTestServer(t)
	return session, db
}

func routingTestServer(t *testing.T) (*mcp.ClientSession, *store.Store, *orchestration.Engine) {
	session, db, engine, _ := routingTestServerWithProfile(t, "")
	return session, db, engine
}

func routingTestServerWithProfile(t *testing.T, profile model.RoutingProfile) (*mcp.ClientSession, *store.Store, *orchestration.Engine, string) {
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

	cfg := &model.Config{
		CodexBarAccountSHA256: map[string]string{"agy": model.Hash([]byte("fixture-account")), "codex": model.Hash([]byte("fixture-account"))},
		StateDir:              dbDir,
		Repositories: map[string]model.Repo{
			"test-repo": {
				DefaultRoutingProfile: profile, Root: repoDir,
				AllowedPaths: []string{"."},
				Providers:    []string{"codex", "antigravity", "opencode"},
			},
		},
	}
	testReg := providers.Registry{
		Candidates: []model.Capability{
			{AttestedAccount: "fixture-account", Provider: "codex", Model: "gpt-6.1-sol", Status: "available"},
			{
				AttestedAccount: "fixture-account", Provider: "agy", Model: "gemini-4-argon", Status: "available",
				Profiles: []model.ExecutionProfile{
					{Name: "restricted", Status: "preflight_required"},
					{Name: "analysis", Status: "verified"},
				},
			},
			{Provider: "opencode", Model: "kimi-k2.5", Status: "available"},
		},
	}
	e, err := orchestration.New(*cfg, db, testReg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { e.Close() })

	return connectRoutingServer(t, e, *cfg, testReg), db, e, dbDir
}

func connectRoutingServer(t *testing.T, e *orchestration.Engine, cfg model.Config, reg providers.Registry) *mcp.ClientSession {
	t.Helper()
	s := New(e, cfg, reg)
	ct, st := mcp.NewInMemoryTransports()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	go func() {
		_ = s.Run(ctx, st)
	}()

	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "1.0"}, nil)
	session, err := client.Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { session.Close() })

	return session
}

func callMCPTool(session *mcp.ClientSession, tool string, args any) (map[string]any, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	res, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name:      tool,
		Arguments: args,
	})
	if err != nil {
		return nil, err
	}
	out, ok := res.StructuredContent.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("expected StructuredContent map, got %+v", res)
	}
	return out, nil
}

func TestDelegateTaskAutoResolution(t *testing.T) {
	server, s := setupTestMCPServer(t)
	res, err := callMCPTool(server, "delegate_task", map[string]any{
		"task": map[string]any{
			"category":  "coding",
			"objective": "Implement O(N log N) interval binary search",
		},
		"target": "auto",
	})
	if err != nil {
		t.Fatalf("delegate_task call failed: %v", err)
	}
	rd, ok := res["route_decision"].(map[string]any)
	if !ok {
		t.Fatalf("expected route_decision in response, got %v", res)
	}
	if rd["selected_model"] != "codex/gpt-6.1-sol" {
		t.Errorf("expected gpt-6.1-sol, got %v", rd["selected_model"])
	}
	if rd["resolved_effort"] != "high" {
		t.Errorf("expected high effort, got %v", rd["resolved_effort"])
	}

	// Verify routing audit logged
	jobID, ok := res["job_id"].(string)
	if !ok || jobID == "" {
		t.Fatalf("expected job_id in response, got %v", res)
	}
	audit, err := s.GetRoutingAudit(jobID)
	if err != nil || len(audit) == 0 {
		t.Fatalf("expected audit logged, got %v", err)
	}
}

func TestDelegateTaskCodexQuotaExhaustedFailover(t *testing.T) {
	session, db, e := routingTestServer(t)

	// Scenario A: Codex 100% full (0% remaining), Antigravity Gemini 97.85% free (97.85% remaining)
	zero := 0.0
	agyRemain := 97.85
	e.SetQuotaForTest("codex", []providers.Quota{
		{AccountMatch: "MATCHED", Unit: "percent", State: "KNOWN", Bucket: providers.BarCodexBurst, Remaining: &zero},
	})
	e.SetQuotaForTest("agy", []providers.Quota{
		{AccountMatch: "MATCHED", Unit: "percent", State: "KNOWN", Bucket: providers.BarAgyGemini, Remaining: &agyRemain},
	})

	res, err := callMCPTool(session, "delegate_task", map[string]any{
		"task": map[string]any{
			"category":  "coding",
			"objective": "Implement search algorithm",
		},
		"target": "auto",
	})
	if err != nil {
		t.Fatalf("delegate_task call failed: %v", err)
	}
	rd, ok := res["route_decision"].(map[string]any)
	if !ok {
		t.Fatalf("expected route_decision in response, got %v", res)
	}
	// With Codex exhausted, router must select antigravity/gemini-4-argon!
	if rd["selected_model"] != "antigravity/gemini-4-argon" {
		t.Fatalf("expected failover to antigravity/gemini-4-argon, got %v", rd["selected_model"])
	}

	jobID, ok := res["job_id"].(string)
	if !ok || jobID == "" {
		t.Fatalf("expected job_id, got %v", res)
	}
	audit, err := db.GetRoutingAudit(jobID)
	if err != nil || len(audit) == 0 {
		t.Fatalf("expected audit logged, got err=%v audit=%v", err, audit)
	}
	if audit[0].AssignedModel != "antigravity/gemini-4-argon" {
		t.Fatalf("expected audit assigned model antigravity/gemini-4-argon, got %s", audit[0].AssignedModel)
	}
	if audit[0].DispatchStatus != "DISPATCHED" {
		t.Fatalf("expected audit dispatch status DISPATCHED, got %s", audit[0].DispatchStatus)
	}
}

func TestCircuitBreakerHalfOpenAutoRoutingRegression(t *testing.T) {
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

	cfg := &model.Config{
		StateDir: dbDir,
		Repositories: map[string]model.Repo{
			"test-repo": {
				Root:         repoDir,
				AllowedPaths: []string{"."},
				Providers:    []string{"codex", "antigravity", "opencode"},
			},
		},
	}
	testReg := providers.Registry{
		Candidates: []model.Capability{
			{Provider: "codex", Model: "gpt-6.1-sol", Status: "available"},
			{Provider: "agy", Model: "gemini-4-argon", Status: "available"},
			{Provider: "opencode", Model: "kimi-k2.5", Status: "available"},
		},
	}
	e, err := orchestration.New(*cfg, db, testReg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { e.Close() })

	// 1. Trip codex provider for 30ms cooldown
	e.CircuitBreaker().Trip("codex", 30*time.Millisecond)
	if st := e.CircuitBreaker().State("codex"); st != orchestration.CircuitOpen {
		t.Fatalf("expected codex circuit state OPEN after Trip, got %s", st)
	}

	// Wait for cooldown to expire -> HALF_OPEN
	time.Sleep(40 * time.Millisecond)
	if st := e.CircuitBreaker().State("codex"); st != orchestration.CircuitHalfOpen {
		t.Fatalf("expected codex circuit state HALF_OPEN after cooldown, got %s", st)
	}

	// Explicitly assert that router's Eligible() check is strictly read-only:
	// it does NOT consume the probe lock (cb.probing["codex"] == false), and
	// cb.Allow("codex") succeeds immediately.
	if !e.CircuitBreaker().Eligible("codex") {
		t.Fatalf("expected codex to be eligible in HALF_OPEN")
	}
	if e.CircuitBreaker().IsProbing("codex") {
		t.Fatalf("expected cb.IsProbing(codex) == false after router's Eligible() call")
	}
	probeID, allowed := e.CircuitBreaker().Acquire("codex")
	if !allowed {
		t.Fatalf("expected cb.Allow(codex) to succeed immediately after router's Eligible() call")
	}
	// Release the probe so delegate_task below exercises end-to-end auto-routing and dispatch
	e.CircuitBreaker().Release("codex", probeID)
	if e.CircuitBreaker().IsProbing("codex") {
		t.Fatalf("expected cb.IsProbing(codex) == false after Release()")
	}

	s := New(e, *cfg, testReg)
	ct, st := mcp.NewInMemoryTransports()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	go func() {
		_ = s.Run(ctx, st)
	}()

	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "1.0"}, nil)
	session, err := client.Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { session.Close() })

	// 2. Dispatch task with auto-routing
	res, err := callMCPTool(session, "delegate_task", map[string]any{
		"task": map[string]any{
			"category":  "coding",
			"objective": "Verify circuit breaker half-open probe handling",
		},
		"target": "auto",
	})
	if err != nil {
		t.Fatalf("delegate_task call failed: %v", err)
	}

	// 3. Verify router did NOT consume probe, and engine selected codex without CIRCUIT_BREAKER_OPEN
	rd, ok := res["route_decision"].(map[string]any)
	if !ok {
		t.Fatalf("expected route_decision in response, got %v", res)
	}
	if rd["selected_model"] != "codex/gpt-6.1-sol" {
		t.Fatalf("expected selected_model codex/gpt-6.1-sol, got %v", rd["selected_model"])
	}

	jobID, ok := res["job_id"].(string)
	if !ok || jobID == "" {
		t.Fatalf("expected job_id in response, got %v", res)
	}

	if status, _ := res["status"].(string); status == "BLOCKED" {
		t.Fatalf("job was blocked unexpectedly: %v", res["reason"])
	}

	audit, err := db.GetRoutingAudit(jobID)
	if err != nil || len(audit) == 0 {
		t.Fatalf("expected audit logged, got err=%v audit=%v", err, audit)
	}
	if audit[0].AssignedModel != "codex/gpt-6.1-sol" {
		t.Fatalf("expected assigned model codex/gpt-6.1-sol, got %s", audit[0].AssignedModel)
	}
}

func TestDelegateTaskAsyncAuditStatusTransition(t *testing.T) {
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

	cfg := &model.Config{
		StateDir: dbDir,
		Repositories: map[string]model.Repo{
			"test-repo": {
				Root:         repoDir,
				AllowedPaths: []string{"."},
				Providers:    []string{"codex", "antigravity", "opencode"},
			},
		},
	}
	testReg := providers.Registry{
		Candidates: []model.Capability{
			{Provider: "codex", Model: "gpt-6.1-sol", Status: "available"},
		},
	}
	e, err := orchestration.New(*cfg, db, testReg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { e.Close() })

	s := New(e, *cfg, testReg)
	ct, st := mcp.NewInMemoryTransports()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	go func() {
		_ = s.Run(ctx, st)
	}()

	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "1.0"}, nil)
	session, err := client.Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { session.Close() })

	res, err := callMCPTool(session, "delegate_task", map[string]any{
		"task": map[string]any{
			"category":  "coding",
			"objective": "Async task audit status test",
		},
		"target":         "auto",
		"execution_mode": "async",
	})
	if err != nil {
		t.Fatalf("delegate_task async call failed: %v", err)
	}

	jobID, ok := res["job_id"].(string)
	if !ok || jobID == "" {
		t.Fatalf("expected job_id, got %v", res)
	}

	audit, err := db.GetRoutingAudit(jobID)
	if err != nil || len(audit) == 0 {
		t.Fatalf("expected audit record, got err=%v", err)
	}
	initialStatus := audit[0].DispatchStatus
	if initialStatus != "DISPATCHED" && initialStatus != "SUCCESS" && initialStatus != "FAILED" {
		t.Fatalf("unexpected dispatch status: %s", initialStatus)
	}
}

func TestDelegateTaskAutoRetryAfterQuotaChange(t *testing.T) {
	session, db, e := routingTestServer(t)
	task := map[string]any{"objective": "Implement interval search", "category": "coding"}
	args := map[string]any{"task": task, "repo_id": "test-repo", "request_id": "same-request", "execution_mode": "async"}
	call := func(t *testing.T, session *mcp.ClientSession, tool string) map[string]any {
		t.Helper()
		out, err := callMCPTool(session, tool, args)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	first := call(t, session, "delegate_task")
	id, ok := first["job_id"].(string)
	if !ok {
		t.Fatal(first)
	}
	decision := first["route_decision"].(map[string]any)
	if decision["selected_model"] != "codex/gpt-6.1-sol" {
		t.Fatal(first)
	}
	e.CompleteJobForTest(id, "finished before retry")
	audit, err := db.GetRoutingAudit(id)
	if err != nil || len(audit) != 1 {
		t.Fatal(audit, err)
	}
	zero, healthy := 0.0, 97.85
	e.SetQuotaForTest("codex", []providers.Quota{{AccountMatch: "MATCHED", Unit: "percent", State: "KNOWN", Bucket: providers.BarCodexBurst, Remaining: &zero}})
	e.SetQuotaForTest("agy", []providers.Quota{{AccountMatch: "MATCHED", Unit: "percent", State: "KNOWN", Bucket: providers.BarAgyGemini, Remaining: &healthy}})
	args["request_id"] = "fresh-request"
	fresh := call(t, session, "delegate_task")
	if fresh["route_decision"].(map[string]any)["selected_model"] != "antigravity/gemini-4-argon" {
		t.Fatal(fresh)
	}
	args["request_id"] = "same-request"
	args["wait_timeout_seconds"] = 1
	args["execution_mode"] = "wait"
	for _, tool := range []string{"delegate_task", "spawn_subagent"} {
		t.Run(tool, func(t *testing.T) {
			retry := call(t, session, tool)
			if retry["job_id"] != id || !reflect.DeepEqual(retry["route_decision"], decision) {
				t.Fatalf("first decision lost on retry: %v", retry)
			}
			rows, err := db.GetRoutingAudit(id)
			if err != nil || !reflect.DeepEqual(rows, audit) {
				t.Fatal(rows, err)
			}
		})
	}
	for _, change := range []struct {
		field string
		value any
	}{{"objective", "Different objective"}, {"category", "research"}, {"complexity", 0.0}, {"preferred_provider", "codex"}, {"preferred_model", "gpt-6.1-sol"}, {"target_tier", "cheap"}, {"target_effort", "high"}} {
		t.Run(change.field, func(t *testing.T) {
			old, exists := task[change.field]
			task[change.field] = change.value
			defer func() {
				if exists {
					task[change.field] = old
				} else {
					delete(task, change.field)
				}
			}()
			if out := call(t, session, "delegate_task"); out["code"] != "IDEMPOTENCY_CONFLICT" {
				t.Fatal(out)
			}
		})
	}
	t.Run("changed repo policy", func(t *testing.T) {
		repo, _ := e.Repo("test-repo")
		if err := e.Close(); err != nil {
			t.Fatal(err)
		}
		repo.Providers = []string{"codex"}
		repo.AllowedPaths = []string{"internal"}
		task["allowed_paths"] = []string{"."}
		cfg := model.Config{StateDir: db.Dir, Repositories: map[string]model.Repo{"test-repo": repo}}
		next, err := orchestration.New(cfg, db, providers.Registry{})
		if err != nil {
			t.Fatal(err)
		}
		defer next.Close()
		session := connectRoutingServer(t, next, cfg, providers.Registry{})
		out := call(t, session, "delegate_task")
		if out["code"] != "NOT_FOUND" || out["job_id"] != nil {
			t.Fatal(out)
		}
	})
}

func TestSubmissionComplexityPresence(t *testing.T) {
	session, _ := setupTestMCPServer(t)
	list, err := session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"delegate_task", "spawn_subagent", "delegate_batch"} {
		found := false
		for _, tool := range list.Tools {
			if tool.Name == name {
				found = true
				schema, err := json.Marshal(tool.InputSchema)
				if err != nil {
					t.Fatal(err)
				}
				if bytes.Contains(schema, []byte(`"has_complexity"`)) {
					t.Errorf("%s exposes has_complexity", name)
				}
			}
		}
		if !found {
			t.Errorf("missing tool %s", name)
		}
	}
	var sub model.Submission
	omitted := model.SubmissionIntentDigest(sub)
	sub.HasComplexity = true
	if omitted == model.SubmissionIntentDigest(sub) {
		t.Error("omitted and explicit zero complexity have the same intent digest")
	}
	sub.Complexity = 0.5
	withPresence := model.SubmissionIntentDigest(sub)
	sub.HasComplexity = false
	if model.SubmissionIntentDigest(sub) != withPresence {
		t.Error("nonzero complexity must have a single identity across Go and MCP callers")
	}
}

func TestAutoMCPRanksEffectiveRoutingProfile(t *testing.T) {
	session, _, e, state := routingTestServerWithProfile(t, "price")
	for _, tc := range []struct {
		request     string
		profile     model.RoutingProfile
		wantInitial bool
	}{{"repo-profile", "", false}, {"request-profile", "quality", true}, {"operator-profile", "", true}} {
		if tc.request == "operator-profile" {
			repo, _ := e.Repo("test-repo")
			profile := "quality"
			if err := store.SaveRoutingPreference(state, "test-repo", repo.Root, &profile); err != nil {
				t.Fatal(err)
			}
		}
		args := map[string]any{"request_id": tc.request, "repo_id": "test-repo", "task": "Check interval ordering", "execution_mode": "async", "mode": "read_only", "deadline_seconds": 900}
		if tc.profile != "" {
			args["routing_profile"] = tc.profile
		}
		result, err := callMCPTool(session, "delegate_task", args)
		if err != nil {
			t.Fatal(err)
		}
		id, ok := result["job_id"].(string)
		if !ok {
			t.Fatal(result)
		}
		got, err := e.Get(id)
		if err != nil {
			t.Fatal(err)
		}
		if (got.InitialRoute != nil) != tc.wantInitial || (result["route_decision"] != nil) != tc.wantInitial {
			t.Fatalf("%s profile %s initial %+v response %+v", tc.request, got.RoutingProfile, got.InitialRoute, result)
		}
	}
}
