// Copyright 2026 Furkan Anıl Öztaş
// SPDX-License-Identifier: Apache-2.0

package mcpserver

import (
	"context"
	"fmt"
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

func setupTestMCPServer(t *testing.T) (*mcp.ClientSession, *store.Store) {
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

	return session, db
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

	// Scenario A: Codex 100% full (0% remaining), Antigravity Gemini 97.85% free (97.85% remaining)
	zero := 0.0
	agyRemain := 97.85
	e.SetQuotaForTest("codex", []providers.Quota{
		{Bucket: providers.BarCodexBurst, Remaining: &zero},
	})
	e.SetQuotaForTest("agy", []providers.Quota{
		{Bucket: providers.BarAgyGemini, Remaining: &agyRemain},
	})

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
	if !e.CircuitBreaker().Allow("codex") {
		t.Fatalf("expected cb.Allow(codex) to succeed immediately after router's Eligible() call")
	}
	// Release the probe so delegate_task below exercises end-to-end auto-routing and dispatch
	e.CircuitBreaker().Release("codex")
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
