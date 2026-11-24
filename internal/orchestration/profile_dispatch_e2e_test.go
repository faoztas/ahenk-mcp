// Copyright 2026 Furkan Anıl Öztaş
// SPDX-License-Identifier: Apache-2.0

package orchestration

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/faoztas/ahenk-mcp/internal/model"
	"github.com/faoztas/ahenk-mcp/internal/providers"
)

func TestProfileDispatchRefreshAndEffort(t *testing.T) {
	e, dir := setupTestEngine(t)
	if err := os.WriteFile(filepath.Join(dir, "brief.txt"), []byte("synthetic task"), 0600); err != nil {
		t.Fatal(err)
	}
	setupTestGitRepo(t, dir)
	bin := filepath.Join(t.TempDir(), "mock-cli")
	args := bin + ".args"
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > '" + args + "'\nprintf '%s\\n' '{\"session_id\":\"fixture\",\"result\":\"done\",\"subtype\":\"success\",\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}'\n"
	if err := os.WriteFile(bin, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	reg := providers.New(model.Config{Providers: []model.ProviderConfig{{ID: "codex", AttestedAccount: "fixture-account", Path: bin, Model: "gpt-6.1-sol"}, {ID: "claude", Path: bin, Model: "claude-sonnet-5-5"}}})
	reg.Candidates = slices.DeleteFunc(reg.Candidates, func(c model.Capability) bool { return c.Model != "gpt-6.1-sol" && c.Model != "claude-sonnet-5-5" })
	for i := range reg.Candidates {
		if reg.Candidates[i].Provider == "codex" {
			reg.Candidates[i].Quality = []model.QualityEvidence{{Role: "inference", Tier: 2, Source: "LIVE_DETERMINISTIC_ROLE_CALIBRATION"}}
		}
	}
	root, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	e.mu.Lock()
	repo := e.cfg.Repositories["test-repo"]
	e.cfg.CodexBarAccountSHA256 = map[string]string{"codex": model.Hash([]byte("fixture-account"))}
	repo.Root = root
	repo.Providers = []string{"codex", "claude"}
	e.cfg.Repositories["test-repo"] = repo
	e.reg = reg
	// Hold both scheduler slots until the queued job and replacement quota exist.
	e.active["hold-1"], e.active["hold-2"] = func() {}, func() {}
	e.mu.Unlock()
	sub := identitySubmission(t)
	sub.Mode = "read_only"
	sub.RoutingProfile = "quality"
	sub.TargetEffort = "max"
	sub.TaskClass = ""
	sub.ExecutionProfile = ""
	now := time.Now().UTC()
	d, pre := e.EvaluateAutoRoute(sub.RepoID, "coding", nil, sub.Task, now)
	if pre == nil || pre.Provider != "codex" {
		t.Fatalf("initial route %+v", d)
	}
	j, err := e.SubmitRouted(sub, &model.InitialRoute{Provider: pre.Provider, Model: pre.Model, SelectedModel: d.SelectedModel, ResolvedEffort: d.ResolvedEffort, Reason: d.Reason, TaskCategory: "coding", AutoRouted: true})
	if err != nil {
		t.Fatal(err)
	}
	e.SetQuotaForTest("codex", []providers.Quota{quotaFixture("codex", providers.BarCodexBurst, "", 300, 0, now)})
	e.mu.Lock()
	delete(e.active, "hold-1")
	delete(e.active, "hold-2")
	e.mu.Unlock()
	done, err := e.Wait(context.Background(), j.ID, 30*time.Second)
	if err != nil || done.Status != model.Completed || len(done.Attempts) != 1 || done.Attempts[0].Provider != "claude" {
		t.Fatalf("dispatch %s %s %v %+v %+v", done.Status, done.Reason, err, done.Attempts, done.Route)
	}
	data, err := os.ReadFile(args)
	if err != nil || !strings.Contains(string(data), "--effort\nmax\n") || done.InitialRoute.ResolvedEffort != "xhigh" || done.Attempts[0].ResolvedEffort != "max" {
		t.Fatalf("initial effort %q, actual CLI args %q, %v", done.InitialRoute.ResolvedEffort, data, err)
	}
}

func TestProfilePlannerPersistsRestrictedAttempt(t *testing.T) {
	e, dir := setupTestEngine(t)
	if err := os.WriteFile(filepath.Join(dir, "brief.txt"), []byte("synthetic task"), 0600); err != nil {
		t.Fatal(err)
	}
	setupTestGitRepo(t, dir)
	bin := filepath.Join(t.TempDir(), "mock-claude")
	// The mock's summary is also valid as the final inspect step's output.
	script := `#!/bin/sh
printf '%s\n' '{"session_id":"test","result":"{\"steps\":[{\"key\":\"one\",\"kind\":\"inspect\",\"objective\":\"Inspect brief\",\"paths\":[\"brief.txt\"],\"depends_on\":[]}]}","subtype":"success","usage":{"input_tokens":1,"output_tokens":1}}'
`
	if err := os.WriteFile(bin, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	root, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	reg := providers.New(model.Config{Providers: []model.ProviderConfig{{ID: "claude", Path: bin, Model: "claude-sonnet-5-5"}}})
	reg.Candidates = slices.DeleteFunc(reg.Candidates, func(c model.Capability) bool { return c.Provider != "claude" || c.Model != "claude-sonnet-5-5" })
	e.mu.Lock()
	repo := e.cfg.Repositories["test-repo"]
	repo.Root, repo.Providers = root, []string{"claude"}
	e.cfg.Repositories["test-repo"] = repo
	e.reg = reg
	e.mu.Unlock()
	sub := identitySubmission(t)
	sub.Decompose, sub.Mode, sub.ExecutionProfile, sub.RoutingProfile = true, "read_only", "analysis", "quality"
	sub.AllowedPaths = []string{"brief.txt"}
	job, err := e.Submit(sub)
	if err != nil {
		t.Fatal(err)
	}
	done, err := e.Wait(context.Background(), job.ID, 30*time.Second)
	if err != nil || done.Status != model.Completed {
		t.Fatalf("planner status %s %s: %v, routes %+v", done.Status, done.Reason, err, done.Route)
	}
	jobs, err := e.db.All()
	if err != nil {
		t.Fatal(err)
	}
	i := slices.IndexFunc(jobs, func(j model.Job) bool { return j.ID == job.ID })
	if i < 0 {
		t.Fatal("missing planner job")
	}
	saved := jobs[i]
	if saved.Task.ExecutionProfile != "analysis" || len(saved.Attempts) != 1 {
		t.Fatalf("parent profile %q attempts %d", saved.Task.ExecutionProfile, len(saved.Attempts))
	}
	scope := saved.Attempts[0].EffectiveScope
	if scope == nil || scope.Mode != "read_only" || scope.ExecutionProfile != "restricted" {
		t.Fatalf("stored planner scope %+v", scope)
	}
}
