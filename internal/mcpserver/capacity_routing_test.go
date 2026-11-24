// Copyright 2026 Furkan Anıl Öztaş
// SPDX-License-Identifier: Apache-2.0

package mcpserver

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/faoztas/ahenk-mcp/internal/model"
	"github.com/faoztas/ahenk-mcp/internal/orchestration"
	"github.com/faoztas/ahenk-mcp/internal/providers"
	"github.com/faoztas/ahenk-mcp/internal/store"
)

func TestRoutePreviewAndDispatchShareEligibility(t *testing.T) {
	session, _, e := routingTestServer(t)
	now := time.Now().UTC()
	task := model.Task{Mode: "read_only", DataClass: "synthetic", DeadlineSeconds: 600}
	e.CircuitBreaker().Trip("codex", time.Hour)
	if d, healthy := e.EvaluateAutoRoute("test-repo", "coding", nil, task, now); healthy == nil || healthy.Provider != "agy" {
		t.Fatalf("healthy AGY was not selectable: %+v", d)
	}
	left := 10.0
	e.SetQuotaForTest("agy", []providers.Quota{{Provider: "agy", Family: "gemini_argon", Bucket: providers.BarAgyArgon, WindowMinutes: 10080, Unit: "percent", AccountMatch: "MATCHED", Remaining: &left, State: "KNOWN", ObservedAt: now}})
	out, err := callMCPTool(session, "list_capabilities", map[string]any{"repo_id": "test-repo", "include_quota": true})
	if err != nil {
		t.Fatal(err)
	}
	var reason string
	for _, raw := range out["routing_preview"].([]any) {
		d := raw.(map[string]any)
		if d["provider"] == "agy" {
			reason, _ = d["reason"].(string)
		}
	}
	if reason != "AGY_WEEKLY_RESERVE_PROTECTED" {
		t.Fatalf("setup did not reach reserve: %v", out)
	}
	d, c := e.EvaluateAutoRoute("test-repo", "coding", nil, task, now)
	if c != nil && c.Provider == "agy" || out["routing_summary"].(map[string]any)["recommended_frontier"] == "agy:gemini-4-argon" {
		t.Fatal(d, out["routing_summary"])
	}
}

func TestQuotaDiagnosticsDoesNotConsumeHalfOpenProbe(t *testing.T) {
	session, _, e := routingTestServer(t)
	cb := e.CircuitBreaker()
	cb.Trip("codex", -time.Second)
	for range 2 {
		out, err := callMCPTool(session, "get_provider_quotas", map[string]any{})
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, raw := range out["bars"].([]any) {
			b := raw.(map[string]any)
			if b["bucket"] == providers.BarCodexBurst {
				found = true
				if b["remaining_percent"] != nil || b["health_pct"] != nil || b["state"] != "UNKNOWN" || b["circuit_state"] != "HALF_OPEN" {
					t.Fatal(b)
				}
			}
		}
		if !found {
			t.Fatal(out)
		}
		if _, err := callMCPTool(session, "list_capabilities", map[string]any{"repo_id": "test-repo", "include_quota": true}); err != nil {
			t.Fatal(err)
		}
		if cb.IsProbing("codex") {
			t.Fatal("diagnostics consumed probe")
		}
	}
	if !cb.Allow("codex") || cb.Allow("codex") {
		t.Fatal("expected one probe")
	}
}

func TestQuotaDiagnosticsKeepModelScopedRateLimit(t *testing.T) {
	now := time.Now().UTC()
	db, err := store.Open(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	const limited = "crouton-alpha-free"
	const alternate = "beam-free"
	job := model.Job{ID: "prior", RepoID: "demo", Status: model.Failed, CreatedAt: now, Attempts: []model.Attempt{{Number: 1, Provider: "opencode", Model: limited, Code: "RATE_LIMIT", RetryAfterMS: 300000, FinishedAt: &now}}}
	if _, err := db.Insert([]model.Job{job}, ""); err != nil {
		t.Fatal(err)
	}
	cfg := model.Config{Repositories: map[string]model.Repo{"demo": {Providers: []string{"opencode"}}}}
	reg := providers.Registry{Candidates: []model.Capability{
		{Provider: "opencode", Model: limited, Status: "available", Speed: "fast"},
		{Provider: "opencode", Model: alternate, Status: "available", Speed: "fast"},
	}}
	e, err := orchestration.New(cfg, db, reg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { e.Close() })
	session := connectRoutingServer(t, e, cfg, reg)
	out, err := callMCPTool(session, "list_capabilities", map[string]any{"repo_id": "demo", "include_quota": true})
	if err != nil {
		t.Fatal(err)
	}
	if rec := out["routing_summary"].(map[string]any)["recommended_free"]; rec != "opencode:"+alternate {
		t.Fatal(out)
	}
	d, cap := e.EvaluateAutoRoute("demo", "coding", nil, model.Task{Mode: "read_only", DataClass: "synthetic"}, now)
	if cap == nil || cap.Model != alternate {
		t.Fatal(d)
	}
}

func TestAdmissionMCPRecommendations(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	cfg := model.Config{Repositories: map[string]model.Repo{"demo": {Providers: []string{"codex"}}}}
	reg := providers.Registry{Candidates: []model.Capability{{Provider: "codex", Model: "gpt-6-luna", Status: "available", Speed: "fast"}}}
	e, err := orchestration.New(cfg, db, reg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { e.Close() })
	session := connectRoutingServer(t, e, cfg, reg)
	out, err := callMCPTool(session, "list_capabilities", map[string]any{"repo_id": "demo", "include_quota": true})
	if err != nil {
		t.Fatal(err)
	}
	routes := out["routing_preview"].([]any)
	if len(routes) != 1 || routes[0].(map[string]any)["reason"] != "LUNA_CODING_DENIED" || out["routing_summary"].(map[string]any)["recommended_frontier"] != nil {
		t.Fatal(routes, out["routing_summary"])
	}
}
