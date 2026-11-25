// Copyright 2026 Furkan Anıl Öztaş
// SPDX-License-Identifier: Apache-2.0

package mcpserver

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/faoztas/ahenk-mcp/internal/model"
	"github.com/faoztas/ahenk-mcp/internal/orchestration"
	"github.com/faoztas/ahenk-mcp/internal/providers"
	"github.com/faoztas/ahenk-mcp/internal/store"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func setupTestServer(t *testing.T) (*mcp.ClientSession, *orchestration.Engine, *model.Config, string) {
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
		StateDir: dbDir,
		Repositories: map[string]model.Repo{
			"test-repo": {
				Root:         repoDir,
				AllowedPaths: []string{"."},
				Providers:    []string{"opencode"},
			},
		},
	}
	e, err := orchestration.New(*cfg, db, providers.Registry{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { e.Close() })

	s := New(e, *cfg, providers.Registry{})
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

	return session, e, cfg, repoDir
}

func callServerTool(ctx context.Context, t *testing.T, session *mcp.ClientSession, tool string, args any) map[string]any {
	t.Helper()
	res, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name:      tool,
		Arguments: args,
	})
	if err != nil {
		t.Fatalf("tool call error: %v", err)
	}
	out, ok := res.StructuredContent.(map[string]any)
	if !ok {
		t.Fatalf("expected StructuredContent map, got %+v", res)
	}
	return out
}

func TestServer_ToolsList_SubagentDescriptions(t *testing.T) {
	session, _, _, _ := setupTestServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	list, err := session.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("unexpected error listing tools: %v", err)
	}

	toolMap := make(map[string]*mcp.Tool)
	for _, tool := range list.Tools {
		toolMap[tool.Name] = tool
	}

	delTool, ok := toolMap["delegate_task"]
	if !ok {
		t.Fatal("expected delegate_task tool to be registered")
	}
	if delTool.Description == "" {
		t.Fatal("expected non-empty description for delegate_task")
	}

	if _, ok := toolMap["wait_jobs"]; !ok {
		t.Fatal("expected wait_jobs tool to be registered")
	}
	if _, ok := toolMap["spawn_subagent"]; !ok {
		t.Fatal("expected spawn_subagent tool to be registered")
	}
}

func TestServer_AllowUnknownFields(t *testing.T) {
	session, _, _, _ := setupTestServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	// Calling delegate_task with unknown fields like "thought", "metadata"
	// and execution_mode: "async" should not return INVALID_INPUT.
	args := map[string]any{
		"task":           "Investigate bug",
		"repo_id":        "test-repo",
		"execution_mode": "async",
		"thought":        "I should inspect the repository",
		"client_notes":   "Extra notes from client harness",
	}
	out := callServerTool(ctx, t, session, "delegate_task", args)
	if out["code"] == "INVALID_INPUT" {
		t.Fatalf("unknown fields must be tolerated, got INVALID_INPUT: %+v", out)
	}
	if out["job_id"] == nil || out["job_id"] == "" {
		t.Fatalf("expected job_id returned, got %+v", out)
	}
}

func TestServer_DelegateTask_AsyncMode(t *testing.T) {
	session, _, _, _ := setupTestServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	args := model.DelegateTaskInput{
		Task:          "Async background task",
		RepoID:        "test-repo",
		ExecutionMode: "async",
	}
	out := callServerTool(ctx, t, session, "delegate_task", args)
	if out["job_id"] == nil || out["job_id"] == "" {
		t.Fatalf("expected job_id returned for async call, got %+v", out)
	}
	if out["status"] != model.Created && out["status"] != model.Queued && out["status"] != model.Running {
		t.Fatalf("expected initial status (CREATED/QUEUED/RUNNING), got %v", out["status"])
	}
}

func TestServer_SpawnSubagent_Alias(t *testing.T) {
	session, _, _, _ := setupTestServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	args := map[string]any{
		"task":           "Subagent task via alias",
		"repo_id":        "test-repo",
		"execution_mode": "async",
	}
	out := callServerTool(ctx, t, session, "spawn_subagent", args)
	if out["job_id"] == nil || out["job_id"] == "" {
		t.Fatalf("expected job_id from spawn_subagent, got %+v", out)
	}
}

func TestServer_DelegateTask_WaitMode(t *testing.T) {
	session, e, _, _ := setupTestServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	// Asynchronously mark the queued job as completed so Wait returns
	go func() {
		for i := 0; i < 50; i++ {
			time.Sleep(20 * time.Millisecond)
			e.CompleteRunningJobsForTest("Sync execution result")
		}
	}()

	args := model.DelegateTaskInput{
		Task:               "Synchronous subagent task",
		RepoID:             "test-repo",
		WaitTimeoutSeconds: 2,
	}
	out := callServerTool(ctx, t, session, "delegate_task", args)
	if out["job_id"] == nil || out["job_id"] == "" {
		t.Fatalf("expected job_id in wait mode response, got %+v", out)
	}
	if out["status"] != model.Completed {
		t.Fatalf("expected COMPLETED status in wait mode, got %v", out["status"])
	}
	if out["summary"] != "Sync execution result" {
		t.Fatalf("expected summary in wait mode response, got %v", out["summary"])
	}
}

func TestServer_WaitJobs(t *testing.T) {
	session, e, _, _ := setupTestServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	// Launch 2 async tasks
	out1 := callServerTool(ctx, t, session, "delegate_task", model.DelegateTaskInput{
		Task:          "Parallel task 1",
		RepoID:        "test-repo",
		ExecutionMode: "async",
	})
	out2 := callServerTool(ctx, t, session, "delegate_task", model.DelegateTaskInput{
		Task:          "Parallel task 2",
		RepoID:        "test-repo",
		ExecutionMode: "async",
	})

	id1 := out1["job_id"].(string)
	id2 := out2["job_id"].(string)

	go func() {
		time.Sleep(50 * time.Millisecond)
		e.CompleteJobForTest(id1, "Completed "+id1)
		e.CompleteJobForTest(id2, "Completed "+id2)
	}()

	waitOut := callServerTool(ctx, t, session, "wait_jobs", model.WaitJobsInput{
		JobIDs:         []string{id1, id2},
		Mode:           "all",
		TimeoutSeconds: 2,
	})

	if waitOut["count"] != float64(2) && waitOut["count"] != 2 {
		t.Fatalf("expected count 2, got %v", waitOut["count"])
	}
	jobsList, ok := waitOut["jobs"].([]any)
	if !ok || len(jobsList) != 2 {
		t.Fatalf("expected 2 jobs in array, got %+v", waitOut["jobs"])
	}
}

func TestHandleDelegateTask_DynamicRepoAndMaxDepth(t *testing.T) {
	dir := t.TempDir()
	dbDir := filepath.Join(dir, "state")
	if err := os.Mkdir(dbDir, 0700); err != nil {
		t.Fatal(err)
	}
	dynRepoDir := filepath.Join(dir, "dyn-repo")
	if err := os.Mkdir(dynRepoDir, 0700); err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(dbDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	// Start with 0 repositories in startup cfg.
	cfg := &model.Config{
		StateDir:     dbDir,
		Repositories: map[string]model.Repo{},
	}
	reg := providers.Registry{
		Candidates: []model.Capability{
			{Provider: "codex", Model: "gpt-6.1-sol", Status: "available"},
		},
	}
	e, err := orchestration.New(*cfg, db, reg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { e.Close() })

	s := New(e, *cfg, reg)
	ct, st := mcp.NewInMemoryTransports()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	go func() { _ = s.Run(ctx, st) }()

	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "1.0"}, nil)
	session, err := client.Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { session.Close() })

	// 1. Dynamically register repo via ResolveRepo(cwd) during delegate_task.
	out := callServerTool(ctx, t, session, "delegate_task", map[string]any{
		"task":           "Inspect dynamic repository",
		"cwd":            dynRepoDir,
		"execution_mode": "async",
	})
	if out["code"] != nil {
		t.Fatalf("expected dynamic delegate_task to succeed, got %+v", out)
	}
	repoID, _ := out["repo_id"].(string)
	if repoID == "" {
		t.Fatalf("expected dynamically resolved repo_id, got %+v", out)
	}

	// 2. list_capabilities must immediately see the dynamically registered repo.
	caps := callServerTool(ctx, t, session, "list_capabilities", map[string]any{
		"repo_id": repoID,
	})
	if caps["code"] == "REPO_NOT_REGISTERED" {
		t.Fatalf("list_capabilities failed to see dynamically registered repo %q: %+v", repoID, caps)
	}
	if caps["scheduler_status"] != "active" {
		t.Fatalf("expected active scheduler_status, got %+v", caps)
	}

	// 3. AHENK_MAX_DEPTH=1 in env clamps client's max_depth: 10 and blocks recursive delegation.
	t.Setenv("AHENK_MAX_DEPTH", "1")
	t.Setenv("AHENK_CHILD_SESSION", "0")
	clampedOut := callServerTool(ctx, t, session, "delegate_task", map[string]any{
		"task":           "Check max_depth clamping",
		"repo_id":        repoID,
		"max_depth":      10,
		"execution_mode": "async",
	})
	clampedID, _ := clampedOut["job_id"].(string)
	if clampedID == "" {
		t.Fatalf("expected job_id for clamped task, got %+v", clampedOut)
	}
	clampedJob, err := e.Get(clampedID)
	if err != nil {
		t.Fatal(err)
	}
	if clampedJob.Task.MaxDepth != 1 {
		t.Fatalf("expected stored MaxDepth clamped to 1 by AHENK_MAX_DEPTH=1, got %d", clampedJob.Task.MaxDepth)
	}

	t.Setenv("AHENK_CHILD_SESSION", "1")
	t.Setenv("AHENK_CHILD_DEPTH", "1")
	blockedOut := callServerTool(ctx, t, session, "delegate_task", map[string]any{
		"task":           "Recursive attempt",
		"repo_id":        repoID,
		"max_depth":      10,
		"execution_mode": "async",
	})
	if blockedOut["code"] != "POLICY_DENIED" {
		t.Fatalf("expected POLICY_DENIED for recursive call with AHENK_MAX_DEPTH=1, got %+v", blockedOut)
	}
	t.Setenv("AHENK_CHILD_SESSION", "0")
	t.Setenv("AHENK_CHILD_DEPTH", "")
	t.Setenv("AHENK_MAX_DEPTH", "")

	// 4. Whitespace-prefixed JSON args succeed in add().
	wsOut := callServerTool(ctx, t, session, "delegate_task", json.RawMessage(" \n\t {\"task\":\"Whitespace JSON\",\"repo_id\":\""+repoID+"\",\"execution_mode\":\"async\"} "))
	if wsOut["job_id"] == nil || wsOut["job_id"] == "" {
		t.Fatalf("expected job_id for whitespace-prefixed JSON args, got %+v", wsOut)
	}
}

func TestGetJobResult_DiffDefaultArtifactAndNilResult(t *testing.T) {
	session, e, cfg, _ := setupTestServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	// 1. Incomplete job (Result == nil) with view:"diff" must return a structured error, not panic.
	asyncOut := callServerTool(ctx, t, session, "delegate_task", map[string]any{
		"task":           "Incomplete task",
		"repo_id":        "test-repo",
		"execution_mode": "async",
	})
	incompleteID := asyncOut["job_id"].(string)
	nilRes := callServerTool(ctx, t, session, "get_job_result", map[string]any{
		"job_id": incompleteID,
		"view":   "diff",
	})
	if nilRes["code"] == nil || nilRes["code"] == "" {
		t.Fatalf("expected structured error for view:diff on incomplete job, got %+v", nilRes)
	}

	// 2. Job with verified diff artifact: view:"diff" without artifact_id uses j.Result.ArtifactID.
	withDiffOut := callServerTool(ctx, t, session, "delegate_task", map[string]any{
		"task":           "Patch task with diff",
		"repo_id":        "test-repo",
		"execution_mode": "async",
	})
	diffJobID := withDiffOut["job_id"].(string)
	const patchText = "diff --git a/a.txt b/a.txt\n--- a/a.txt\n+++ b/a.txt\n@@ -1 +1 @@\n-old\n+new\n"
	artifactsDir := filepath.Join(e.Store().Dir, "artifacts")
	if err := os.WriteFile(filepath.Join(artifactsDir, diffJobID+".diff"), []byte(patchText), 0600); err != nil {
		t.Fatal(err)
	}
	// Persist Result with ArtifactID == diffJobID via engine test helper or store
	j, err := e.Get(diffJobID)
	if err != nil {
		t.Fatal(err)
	}
	j.Status = model.Completed
	j.Result = &model.Result{
		Summary:        "Patched",
		ArtifactID:     diffJobID,
		ArtifactSHA256: model.Hash([]byte(patchText)),
		Preimages:      map[string]string{"a.txt": model.Hash([]byte("old\n"))},
	}
	if err := e.Store().Save(j); err != nil {
		t.Fatal(err)
	}
	// Reload via a new engine/server sharing the same state dir and repository policy
	// so the reloaded engine can resolve j under the current PolicyDigest.
	cfg2 := *cfg
	cfg2.StateDir = e.Store().Dir
	e2, err := orchestration.New(cfg2, e.Store(), providers.Registry{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { e2.Close() })
	s2 := New(e2, cfg2, providers.Registry{})
	ct2, st2 := mcp.NewInMemoryTransports()
	go func() { _ = s2.Run(ctx, st2) }()
	c2 := mcp.NewClient(&mcp.Implementation{Name: "c2", Version: "1.0"}, nil)
	sess2, err := c2.Connect(ctx, ct2, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sess2.Close() })

	diffOut := callServerTool(ctx, t, sess2, "get_job_result", map[string]any{
		"job_id": diffJobID,
		"view":   "diff",
	})
	if diffOut["code"] != nil {
		t.Fatalf("expected view:diff without artifact_id to default to j.Result.ArtifactID, got %+v", diffOut)
	}
	if diffOut["diff"] != patchText {
		t.Fatalf("expected diff %q, got %+v", patchText, diffOut["diff"])
	}
}

func TestDiagnosticTools_QuotasBreakersAuditWaitJobs(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// 1. get_provider_quotas with e == nil reports UNKNOWN (not HEALTHY) and includes gemini-argon-wtu-weekly.
	nilServer := New(nil, model.Config{}, providers.Registry{})
	ctNil, stNil := mcp.NewInMemoryTransports()
	go func() { _ = nilServer.Run(ctx, stNil) }()
	cNil := mcp.NewClient(&mcp.Implementation{Name: "nil-client", Version: "1.0"}, nil)
	sessNil, err := cNil.Connect(ctx, ctNil, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sessNil.Close() })

	nilQuotas := callServerTool(ctx, t, sessNil, "get_provider_quotas", map[string]any{})
	barsNil, ok := nilQuotas["bars"].([]any)
	if !ok || len(barsNil) == 0 {
		t.Fatalf("expected bars array when e == nil, got %+v", nilQuotas)
	}
	foundArgon := false
	for _, raw := range barsNil {
		b := raw.(map[string]any)
		if b["state"] != "UNKNOWN" {
			t.Fatalf("expected state UNKNOWN when e == nil, got %+v", b)
		}
		if b["bucket"] == providers.BarAgyArgon {
			foundArgon = true
		}
	}
	if !foundArgon {
		t.Fatalf("expected %s in get_provider_quotas, got %+v", providers.BarAgyArgon, barsNil)
	}

	// 2. Active server: get_active_circuit_breakers deduplicated consistent schema + routingSummary checks.
	session, e, _, _ := setupTestServer(t)
	// routingSummary with no breakers returns [] (not nil) and skips 0% health codex/claude.
	zero := 0.0
	e.SetQuotaForTest("codex", []providers.Quota{{AccountMatch: "MATCHED", Unit: "percent", State: "KNOWN", Bucket: providers.BarCodexBurst, Remaining: &zero}})
	e.SetQuotaForTest("claude", []providers.Quota{{AccountMatch: "MATCHED", Unit: "percent", Bucket: "primary", Remaining: &zero, State: "EXHAUSTED"}})

	items := []model.Capability{
		{Provider: "codex", Model: "gpt-6.1-sol", Status: "available", FPTier: "hard"},
		{Provider: "claude", Model: "claude-opus-4-6", Status: "available", FPTier: "hard"},
		{Provider: "agy", Model: "gemini-4-argon", Status: "available", FPTier: "hard", HeadroomPercent: 80, HeadroomState: "HEALTHY"},
	}
	sum := routingSummary(items, e.ActiveCircuitBreakers(), []model.Decision{{Provider: "agy", Model: "gemini-4-argon", Reason: "AVAILABLE_CANDIDATE"}})
	breakersVal, ok := sum["active_circuit_breakers"].([]map[string]any)
	if !ok || breakersVal == nil {
		t.Fatalf("expected non-nil []map[string]any for active_circuit_breakers, got %#v", sum["active_circuit_breakers"])
	}
	if sum["recommended_frontier"] != "agy:gemini-4-argon" {
		t.Fatalf("expected routingSummary to skip 0%% codex/claude and recommend agy:gemini-4-argon, got %+v", sum)
	}

	// Trip codex circuit breaker and check get_active_circuit_breakers schema & deduplication.
	e.CircuitBreaker().Trip("codex", time.Minute)
	e.CircuitBreaker().Trip("agy", time.Minute)
	breakersOut := callServerTool(ctx, t, session, "get_active_circuit_breakers", map[string]any{})
	tb, ok := breakersOut["tripped_breakers"].([]any)
	if !ok || len(tb) < 2 {
		t.Fatalf("expected tripped_breakers entries, got %+v", breakersOut)
	}
	seenProviders := map[string]int{}
	for _, raw := range tb {
		entry := raw.(map[string]any)
		for _, field := range []string{"provider", "model", "scope", "code", "state", "until"} {
			if _, has := entry[field]; !has {
				t.Fatalf("breaker entry missing required field %q: %+v", field, entry)
			}
		}
		seenProviders[entry["provider"].(string)]++
	}
	for p, count := range seenProviders {
		if count > 1 {
			t.Fatalf("duplicate breaker entry for provider %q: %+v", p, tb)
		}
	}

	// Also verify list_capabilities skips exhausted/tripped providers in routing_summary
	// and serializes active_circuit_breakers as [] when empty.
	dirRS := t.TempDir()
	dbRS, err := store.Open(filepath.Join(dirRS, "state"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { dbRS.Close() })
	cfgRS := model.Config{
		CodexBarAccountSHA256: map[string]string{"codex": model.Hash([]byte("fixture-account")), "claude": model.Hash([]byte("fixture-account"))},
		StateDir:              filepath.Join(dirRS, "state"),
		Repositories: map[string]model.Repo{
			"r1": {Root: dirRS, AllowedPaths: []string{"."}, Providers: []string{"codex", "claude", "agy"}},
		},
	}
	regRS := providers.Registry{
		Candidates: []model.Capability{
			{AttestedAccount: "fixture-account", Provider: "codex", Model: "gpt-6.1-sol", Status: "available"},
			{AttestedAccount: "fixture-account", Provider: "claude", Model: "claude-opus-4-6", Status: "available"},
			{Provider: "agy", Model: "gemini-4-argon", Status: "available"},
		},
	}
	eRS, err := orchestration.New(cfgRS, dbRS, regRS)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { eRS.Close() })
	eRS.SetQuotaForTest("codex", []providers.Quota{{AccountMatch: "MATCHED", Unit: "percent", State: "KNOWN", Bucket: providers.BarCodexBurst, Remaining: &zero}})
	eRS.SetQuotaForTest("claude", []providers.Quota{{AccountMatch: "MATCHED", Unit: "percent", Bucket: "primary", Remaining: &zero, State: "EXHAUSTED"}})
	sRS := New(eRS, cfgRS, regRS)
	ctRS, stRS := mcp.NewInMemoryTransports()
	go func() { _ = sRS.Run(ctx, stRS) }()
	cRS := mcp.NewClient(&mcp.Implementation{Name: "rs-client", Version: "1.0"}, nil)
	sessRS, err := cRS.Connect(ctx, ctRS, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sessRS.Close() })

	capsOut := callServerTool(ctx, t, sessRS, "list_capabilities", map[string]any{"repo_id": "r1"})
	rs, ok := capsOut["routing_summary"].(map[string]any)
	if !ok {
		t.Fatalf("expected routing_summary map, got %+v", capsOut)
	}
	if arr, isArr := rs["active_circuit_breakers"].([]any); !isArr || len(arr) != 0 {
		t.Fatalf("expected empty JSON array [] for active_circuit_breakers, got %#v", rs["active_circuit_breakers"])
	}
	if rec, has := rs["recommended_frontier"]; has && rec != "agy:gemini-4-argon" {
		t.Fatalf("expected exhausted codex/claude to be skipped in favor of agy (or omitted), got %v", rec)
	}
	// Now trip agy too: recommended_frontier must not fall back to exhausted codex/claude.
	eRS.CircuitBreaker().Trip("agy", time.Minute)
	capsOut2 := callServerTool(ctx, t, sessRS, "list_capabilities", map[string]any{"repo_id": "r1"})
	rs2 := capsOut2["routing_summary"].(map[string]any)
	if rec, has := rs2["recommended_frontier"]; has {
		t.Fatalf("expected no recommended_frontier when codex/claude are 0%% and agy is tripped, got %v", rec)
	}

	// 3. get_routing_audit validates ValidID and enforces repoAllowed (NOT_FOUND for unknown/cross-repo).
	invalidAudit := callServerTool(ctx, t, session, "get_routing_audit", map[string]any{"job_id": "../bad"})
	if invalidAudit["code"] != "INVALID_INPUT" {
		t.Fatalf("expected INVALID_INPUT for invalid job_id in get_routing_audit, got %+v", invalidAudit)
	}
	unknownAudit := callServerTool(ctx, t, session, "get_routing_audit", map[string]any{"job_id": "unknown-job-1"})
	if unknownAudit["code"] != "NOT_FOUND" {
		t.Fatalf("expected NOT_FOUND for unknown job_id in get_routing_audit, got %+v", unknownAudit)
	}

	// 4. ID validation on get_job_status, list_jobs, cancel_job, resume_job.
	if out := callServerTool(ctx, t, session, "get_job_status", map[string]any{"job_id": "../bad"}); out["code"] != "INVALID_INPUT" {
		t.Fatalf("get_job_status invalid job_id: got %+v", out)
	}
	if out := callServerTool(ctx, t, session, "list_jobs", map[string]any{"repo_id": "../bad"}); out["code"] != "INVALID_INPUT" {
		t.Fatalf("list_jobs invalid repo_id: got %+v", out)
	}
	if out := callServerTool(ctx, t, session, "list_jobs", map[string]any{"status": "bad status"}); out["code"] != "INVALID_INPUT" {
		t.Fatalf("list_jobs invalid status: got %+v", out)
	}
	if out := callServerTool(ctx, t, session, "cancel_job", map[string]any{"job_id": "../bad"}); out["code"] != "INVALID_INPUT" {
		t.Fatalf("cancel_job invalid job_id: got %+v", out)
	}
	if out := callServerTool(ctx, t, session, "resume_job", map[string]any{"job_id": "../bad", "request_id": "ok"}); out["code"] != "INVALID_INPUT" {
		t.Fatalf("resume_job invalid job_id: got %+v", out)
	}
	if out := callServerTool(ctx, t, session, "resume_job", map[string]any{"job_id": "job-1", "request_id": "../bad"}); out["code"] != "INVALID_INPUT" {
		t.Fatalf("resume_job invalid request_id: got %+v", out)
	}
}
