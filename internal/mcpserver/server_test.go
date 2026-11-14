// Copyright 2026 Furkan Anıl Öztaş
// SPDX-License-Identifier: Apache-2.0

package mcpserver

import (
	"context"
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
