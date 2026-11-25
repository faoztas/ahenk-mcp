// Copyright 2026 Furkan Anıl Öztaş
// SPDX-License-Identifier: Apache-2.0

package mcpserver

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/faoztas/ahenk-mcp/internal/model"
	"github.com/faoztas/ahenk-mcp/internal/orchestration"
	"github.com/faoztas/ahenk-mcp/internal/providers"
	"github.com/faoztas/ahenk-mcp/internal/store"
)

func TestMCPAuditPreservesInitialChoiceAfterQueuedQuotaChange(t *testing.T) {
	dir := t.TempDir()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	brief := "brief.txt"
	if err := os.WriteFile(filepath.Join(root, brief), []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"init", "-b", "main"}, {"add", brief}, {"commit", "-m", "fixture"}} {
		cmd := exec.CommandContext(t.Context(), "/usr/bin/git", append([]string{"-C", root, "-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "-c", "commit.gpgsign=false", "-c", "core.hooksPath=/dev/null"}, args...)...)
		cmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=/var/empty", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null"}
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("fixture repository: %v: %s", err, out)
		}
	}
	codex, claude := filepath.Join(dir, "codex"), filepath.Join(dir, "claude")
	entered, release, trace := filepath.Join(dir, "started"), filepath.Join(dir, "release"), filepath.Join(dir, "arguments")
	if err := os.WriteFile(codex, []byte("#!/bin/sh\nexit 1\n"), 0700); err != nil {
		t.Fatal(err)
	}
	script := fmt.Sprintf(`#!/bin/sh
printf '%%s\n' "$@" > %q
if [ ! -e %q ]; then
  : > %q
  until [ -e %q ]; do sleep 0.02; done
fi
printf '%%s\n' '{"session_id":"fixture","result":"done","subtype":"success","usage":{"input_tokens":1,"output_tokens":1}}'
`, trace, entered, entered, release)
	if err := os.WriteFile(claude, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	cfg := model.Config{
		StateDir:              filepath.Join(dir, "state"),
		CodexBarAccountSHA256: map[string]string{"codex": model.Hash([]byte("fixture-account"))},
		Repositories: map[string]model.Repo{"demo": {Root: root, AllowedPaths: []string{brief},
			Providers: []string{"codex", "claude"}, Limits: model.ProjectLimits{MaxActive: 1}}},
		Providers: []model.ProviderConfig{
			{ID: "codex", Path: codex, Model: "gpt-6.1-sol", AttestedAccount: "fixture-account"},
			{ID: "claude", Path: claude, Model: "claude-sonnet-5-5"},
		},
	}
	reg := providers.New(cfg)
	reg.Candidates = slices.DeleteFunc(reg.Candidates, func(c model.Capability) bool {
		return c.Model != "gpt-6.1-sol" && c.Model != "claude-sonnet-5-5"
	})
	for i := range reg.Candidates {
		if reg.Candidates[i].Provider == "codex" {
			reg.Candidates[i].Quality = []model.QualityEvidence{{Role: "inference", Tier: 2, Source: "LIVE_DETERMINISTIC_ROLE_CALIBRATION"}}
		}
	}
	db, err := store.Open(cfg.StateDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	engine, err := orchestration.New(cfg, db, reg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = engine.Close() })
	session := connectRoutingServer(t, engine, cfg, reg)
	call := func(tool string, args map[string]any) map[string]any {
		t.Helper()
		out, err := callMCPTool(session, tool, args)
		if err != nil || out["code"] != nil {
			t.Fatalf("%s failed: %v, %v", tool, err, out)
		}
		return out
	}
	task := func(id string) map[string]any {
		return map[string]any{"repo_id": "demo", "request_id": id, "task": "Inspect brief.txt",
			"routing_profile": "quality", "category": "coding", "deadline_seconds": 900,
			"allowed_paths": []string{brief}, "mode": "read_only", "data_class": "synthetic", "execution_mode": "async"}
	}
	holder := task("holder")
	holder["target"] = "claude"
	call("delegate_task", holder)
	ready, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		if _, err := os.Stat(entered); err == nil {
			break
		}
		select {
		case <-ticker.C:
		case <-ready.Done():
			t.Fatal("mock CLI did not acquire the repository slot")
		}
	}

	req := task("queued")
	req["target"] = map[string]any{"provider": "auto", "effort": "max"}
	submitted := call("delegate_task", req)
	id, ok := submitted["job_id"].(string)
	before, routed := submitted["route_decision"].(map[string]any)
	if !ok || !routed || before["selected_model"] != "codex/gpt-6.1-sol" || before["resolved_effort"] != "xhigh" {
		t.Fatalf("initial selection did not choose Codex xhigh: %v", submitted)
	}
	now := time.Now().UTC()
	reset, remaining := now.Add(5*time.Hour), 0.0
	engine.SetQuotaForTest("codex", []providers.Quota{{
		Provider: "codex", Bucket: providers.BarCodexBurst, State: "EXHAUSTED", AccountMatch: "MATCHED",
		Unit: "percent", Remaining: &remaining, WindowMinutes: 300, ObservedAt: now, ResetAt: &reset, Confidence: "exact",
	}})
	if err := os.WriteFile(release, nil, 0600); err != nil {
		t.Fatal(err)
	}
	done, err := engine.Wait(t.Context(), id, 15*time.Second)
	if err != nil || done.Status != model.Completed || len(done.Attempts) != 1 || done.Attempts[0].Provider != "claude" {
		t.Fatalf("queued selection not rerouted to Claude: status %s, attempts %+v, err %v", done.Status, done.Attempts, err)
	}
	args, err := os.ReadFile(trace)
	if err != nil || !strings.Contains(string(args), "--effort\nmax\n") {
		t.Fatalf("mock Claude argv does not include max effort: %q, %v", args, err)
	}
	audit := call("get_routing_audit", map[string]any{"job_id": id})
	first, _ := audit["first_decision"].(map[string]any)
	if first["selected_model"] != before["selected_model"] || first["resolved_effort"] != "xhigh" ||
		first["reason"] != "PROFILE_QUALITY" || first["capacity_state"] != "UNKNOWN" || first["quota_pressure"] != nil {
		t.Errorf("first decision lost its selected model, effort, reason, or unknown capacity: %v", first)
	}
	actual, _ := audit["execution_attempts"].([]any)
	if len(actual) != 1 {
		t.Fatalf("expected one actual attempt: %v", audit)
	}
	attempt, _ := actual[0].(map[string]any)
	if attempt["provider"] != "claude" || attempt["model"] != "claude-sonnet-5-5" ||
		attempt["target_effort"] != "max" || attempt["status"] != "SUCCESS" {
		t.Fatalf("unexpected execution audit: %v", attempt)
	}
}
