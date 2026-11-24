// Copyright 2026 Furkan Anıl Öztaş
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/faoztas/ahenk-mcp/internal/model"
	"github.com/faoztas/ahenk-mcp/internal/store"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestDelegationExamplesRespectPresetAndOneShotOverride(t *testing.T) {
	f := newClientPreferenceFixture(t)
	client := f.connect(t, "codex-fixture")
	f.routing(t, "get", "price_performance", "repo_config")
	f.routing(t, "set", "quality", "operator", "quality")

	override := readPreferenceExample(t)
	overrideResult := f.call(t, client, "delegate_task", override)
	if overrideResult["status"] != model.Blocked || overrideResult["reason_code"] != "PRICE_EVIDENCE_REQUIRED" ||
		overrideResult["attempt_count"] != float64(0) {
		t.Fatalf("Price override must reject fixture routes without verified cost: %v", overrideResult)
	}
	overrideID := preferenceField[string](t, overrideResult, "job_id")

	auto := override
	delete(auto, "routing_profile")
	auto["request_id"] = "demo-auto"
	auto["execution_mode"] = "async"
	autoID := preferenceField[string](t, f.call(t, client, "delegate_task", auto), "job_id")
	f.wait(t, client, autoID)

	_, jobs, err := store.ReadEvidence(f.ctx, f.state)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []struct {
		id, profile, source string
	}{
		{overrideID, "price", "request"},
		{autoID, "quality", "operator"},
	} {
		index := slices.IndexFunc(jobs, func(job store.JobEvidence) bool { return job.ID == want.id })
		if index < 0 {
			t.Fatalf("job %s was missing from durable state", want.id)
		}
		job := jobs[index]
		if string(job.RoutingProfile) != want.profile || job.RoutingProfileSource != want.source {
			t.Errorf("job %s has profile %q from %q, want %q from %q",
				job.ID, job.RoutingProfile, job.RoutingProfileSource, want.profile, want.source)
		}
	}
	f.routing(t, "get", "quality", "operator")
	f.routing(t, "reset", "price_performance", "repo_config")
}

func TestTwoClientsShareProfileAndVerifiedHandoff(t *testing.T) {
	f := newClientPreferenceFixture(t)
	codex := f.connect(t, "codex-fixture")
	claude := f.connect(t, "claude-code-fixture")
	f.routing(t, "set", "quality", "operator", "quality")

	firstRequest := preferenceSubmission("codex-inspect", "Inspect fixture.txt")
	first := f.call(t, codex, "delegate_task", firstRequest)
	firstID := preferenceField[string](t, first, "job_id")
	second := f.call(t, claude, "delegate_task", preferenceSubmission("claude-summarize", "Summarize fixture.txt"))
	secondID := preferenceField[string](t, second, "job_id")
	if firstID == secondID {
		t.Fatalf("independent requests share job ID %s", firstID)
	}
	for _, job := range f.wait(t, claude, firstID, secondID) {
		if job["summary"] != "Synthetic adapter completed the offline fixture." {
			t.Errorf("unexpected fixture summary for %v: %v", job["job_id"], job["summary"])
		}
	}
	firstAudit := f.audit(t, claude, first, "quality", "operator", "SUCCESS")
	f.audit(t, codex, second, "quality", "operator", "SUCCESS")

	override := preferenceSubmission("claude-override", "Inspect fixture.txt")
	override["routing_profile"] = "price_performance"
	result := f.call(t, claude, "delegate_task", override)
	f.wait(t, codex, preferenceField[string](t, result, "job_id"))
	f.audit(t, codex, result, "price_performance", "request", "SUCCESS")
	f.routing(t, "get", "quality", "operator")

	producer := model.Task{Objective: "Update fixture.txt", Mode: "patch", DataClass: "synthetic",
		AllowedPaths: []string{"fixture.txt"}, Acceptance: []model.Check{{ID: "patch", Description: "Provide the scoped patch"}}}
	receiver := model.Task{Objective: "fixture:handoff", Mode: "read_only", DataClass: "synthetic",
		AllowedPaths: []string{"fixture.txt"}, Acceptance: []model.Check{{ID: "receive", Description: "Receive the verified diff"}}}
	batch := model.Batch{RepoID: "demo", RequestID: "verified-handoff", Independence: "distinct_provider",
		Items: []model.Item{
			{Key: "produce", Task: producer},
			{Key: "receive", Task: receiver, DependsOn: []string{"produce"}, HandoffFrom: []model.HandoffFrom{{Key: "produce", Kind: "diff"}}},
		}}
	parentID := preferenceField[string](t, f.call(t, codex, "delegate_batch", batch), "job_id")
	parent := f.wait(t, claude, parentID)[0]
	children := preferenceField[[]any](t, parent, "children")
	if len(children) != 2 {
		t.Fatalf("batch returned %d child IDs, want 2", len(children))
	}
	producerID, receiverID := children[0], children[1]
	produced := preferenceField[map[string]any](t, f.call(t, codex, "get_job_result",
		map[string]any{"job_id": producerID}), "result")
	artifactID := preferenceField[string](t, produced, "artifact_id")
	diff := f.call(t, claude, "get_job_result", map[string]any{"job_id": producerID, "view": "diff", "artifact_id": artifactID})
	patch := preferenceField[string](t, diff, "diff")
	hash := model.Hash([]byte(patch))
	if diff["source_drift"] != false || diff["artifact_sha256"] != hash || produced["artifact_sha256"] != hash ||
		!strings.Contains(patch, "--- a/fixture.txt\n+++ b/fixture.txt\n") || !strings.Contains(patch, "-before\n+after\n") {
		t.Fatalf("producer diff failed path, content, or hash verification: %v", diff)
	}
	received := preferenceField[map[string]any](t, f.call(t, claude, "get_job_result",
		map[string]any{"job_id": receiverID}), "result")
	produceAttempts := f.attempts(t, codex, producerID)
	receiveAttempts := f.attempts(t, claude, receiverID)
	handoffs := preferenceField[[]any](t, receiveAttempts[0], "handoffs")
	if len(handoffs) != 1 {
		t.Fatalf("receiver got %d handoffs, want 1", len(handoffs))
	}
	manifest, ok := handoffs[0].(map[string]any)
	if !ok || manifest["source_job_id"] != producerID || manifest["kind"] != "diff" || manifest["sha256"] != hash ||
		manifest["source_attempt"] != produceAttempts[0]["number"] || received["summary"] != "Received verified diff "+hash {
		t.Fatalf("receiver did not verify the producer's diff: manifest %v, result %v", manifest, received)
	}
	if a, b := produceAttempts[0]["provider"], receiveAttempts[0]["provider"]; a == b {
		t.Fatalf("producer and receiver both executed on %v", a)
	}
	source, err := os.ReadFile(f.source)
	if err != nil || string(source) != "before\n" {
		t.Fatalf("source file after batch = %q, err %v", source, err)
	}

	fallback := f.call(t, claude, "delegate_task", preferenceSubmission("quota-fallback", "fixture:429"))
	fallbackID := preferenceField[string](t, fallback, "job_id")
	f.wait(t, codex, fallbackID)
	attempts := f.attempts(t, codex, fallbackID)
	if len(attempts) != 2 || attempts[0]["provider"] != "fixture-a" || attempts[0]["code"] != "RATE_LIMIT" ||
		attempts[1]["provider"] != "fixture-b" || attempts[1]["code"] != nil {
		t.Fatalf("expected rate limit on fixture-a followed by success on fixture-b: %v", attempts)
	}
	f.audit(t, claude, fallback, "quality", "operator", "FAILED", "SUCCESS")

	f.routing(t, "set", "price", "operator", "price")
	f.close(t, claude, codex)
	restarted := f.connect(t, "codex-reconnected-fixture")
	replay := f.call(t, restarted, "delegate_task", firstRequest)
	if replay["job_id"] != firstID || replay["status"] != model.Completed || replay["attempt_count"] != float64(1) {
		t.Fatalf("replay after restart did not return the original completed job: %v", replay)
	}
	replayedAudit := f.audit(t, restarted, replay, "quality", "operator", "SUCCESS")
	for _, key := range []string{"first_decision", "routing_profile", "routing_profile_source", "execution_attempts"} {
		if !reflect.DeepEqual(replayedAudit[key], firstAudit[key]) {
			t.Errorf("replay changed %s from %v to %v", key, firstAudit[key], replayedAudit[key])
		}
	}
	f.close(t, restarted)

	child := f.connect(t, "bounded-child-fixture", "AHENK_CHILD_SESSION=1", "AHENK_CHILD_DEPTH=1", "AHENK_MAX_DEPTH=1")
	blocked, err := child.CallTool(f.ctx, &mcp.CallToolParams{
		Name: "spawn_subagent", Arguments: preferenceSubmission("blocked-child", "Inspect fixture.txt"),
	})
	if err != nil {
		t.Fatal(err)
	}
	out, ok := blocked.StructuredContent.(map[string]any)
	if !ok || !blocked.IsError || out["code"] != "POLICY_DENIED" || out["job_id"] != nil {
		t.Fatalf("recursion guard did not reject the nested submission: %v", blocked)
	}
}

type clientPreferenceFixture struct {
	ctx                           context.Context
	binary, config, state, source string
}

func newClientPreferenceFixture(t *testing.T) clientPreferenceFixture {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	f := clientPreferenceFixture{
		binary: filepath.Join(root, "ahenk"),
		config: filepath.Join(root, "fixture config.json"),
		state:  filepath.Join(root, "state"),
		source: filepath.Join(root, "repo", "fixture.txt"),
	}
	ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
	t.Cleanup(cancel)
	f.ctx = ctx

	// Build the command so routing and serve both enter the real CLI without a TestMain hook.
	build := exec.CommandContext(ctx, "go", "build", "-mod=readonly", "-o", f.binary, ".")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build fixture CLI: %v\n%s", err, out)
	}
	repo := filepath.Dir(f.source)
	if err := os.Mkdir(repo, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.source, []byte("before\n"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"init", "-b", "main"}, {"add", "fixture.txt"}, {"commit", "-m", "synthetic fixture"}} {
		base := []string{"-C", repo, "-c", "user.name=Ahenk Fixture", "-c", "user.email=fixture@example.invalid",
			"-c", "commit.gpgsign=false", "-c", "core.hooksPath=/dev/null"}
		git := exec.CommandContext(ctx, "/usr/bin/git", append(base, args...)...)
		git.Env = []string{"PATH=/usr/bin:/bin", "HOME=/var/empty", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null"}
		if out, err := git.CombinedOutput(); err != nil {
			t.Fatalf("fixture git %s: %v\n%s", args[0], err, out)
		}
	}
	config, err := json.Marshal(model.Config{StateDir: f.state, EnableFixture: true, Repositories: map[string]model.Repo{
		"demo": {Root: repo, AllowedPaths: []string{"fixture.txt"}, Providers: []string{"fixture-a", "fixture-b"},
			DefaultRoutingProfile: model.RoutingProfilePricePerformance},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.config, config, 0600); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f clientPreferenceFixture) command(args ...string) *exec.Cmd {
	cmd := exec.CommandContext(f.ctx, f.binary, args...)
	cmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=/var/empty", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null"}
	return cmd
}

func (f clientPreferenceFixture) routing(t *testing.T, command, profile, source string, value ...string) {
	t.Helper()
	args := append([]string{"routing", command, "--config", f.config, "--repo", "demo"}, value...)
	out, err := f.command(args...).CombinedOutput()
	if err != nil {
		t.Errorf("routing %s: %v: %s", command, err, out)
		return
	}
	var wire map[string]any
	if err := json.Unmarshal(out, &wire); err != nil || wire["repo_id"] != "demo" ||
		wire["routing_profile"] != profile || wire["routing_profile_source"] != source {
		t.Errorf("routing %s returned %s (%v), want repo demo and profile %q from %q", command, out, err, profile, source)
	}
}

func (f clientPreferenceFixture) connect(t *testing.T, name string, env ...string) *mcp.ClientSession {
	t.Helper()
	cmd := f.command("serve", "--stdio", "--config", f.config)
	cmd.Env = append(cmd.Env, env...)
	cmd.Stderr = os.Stderr
	client := mcp.NewClient(&mcp.Implementation{Name: name, Version: "1.0"}, nil)
	session, err := client.Connect(f.ctx, &mcp.CommandTransport{Command: cmd}, nil)
	if err != nil {
		t.Fatalf("connect %s: %v", name, err)
	}
	t.Cleanup(func() { _ = session.Close() })
	tools, err := session.ListTools(f.ctx, nil)
	if err != nil {
		t.Fatalf("list tools for %s: %v", name, err)
	}
	for _, name := range []string{"delegate_task", "spawn_subagent", "wait_jobs", "delegate_batch", "get_job_result", "get_routing_audit"} {
		if !slices.ContainsFunc(tools.Tools, func(tool *mcp.Tool) bool { return tool.Name == name }) {
			t.Fatalf("tool discovery omitted %s", name)
		}
	}
	return session
}

func (f clientPreferenceFixture) close(t *testing.T, clients ...*mcp.ClientSession) {
	t.Helper()
	for _, client := range clients {
		if err := client.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

func (f clientPreferenceFixture) call(t *testing.T, client *mcp.ClientSession, tool string, args any) map[string]any {
	t.Helper()
	result, err := client.CallTool(f.ctx, &mcp.CallToolParams{Name: tool, Arguments: args})
	if err != nil {
		t.Fatalf("%s: %v", tool, err)
	}
	out, ok := result.StructuredContent.(map[string]any)
	if result.IsError || !ok {
		t.Fatalf("%s: %v", tool, result)
	}
	return out
}

func (f clientPreferenceFixture) wait(t *testing.T, client *mcp.ClientSession, ids ...string) []map[string]any {
	t.Helper()
	out := f.call(t, client, "wait_jobs", model.WaitJobsInput{JobIDs: ids, Mode: "all", TimeoutSeconds: 12})
	wire := preferenceField[[]any](t, out, "jobs")
	if len(wire) != len(ids) {
		t.Fatalf("wait returned %d jobs, want %d", len(wire), len(ids))
	}
	jobs := make([]map[string]any, 0, len(ids))
	for _, id := range ids {
		var completed map[string]any
		for _, item := range wire {
			job, ok := item.(map[string]any)
			if ok && job["job_id"] == id && job["status"] == model.Completed {
				completed = job
				break
			}
		}
		if completed == nil {
			t.Fatalf("job %s did not complete: %v", id, out)
		}
		jobs = append(jobs, completed)
	}
	return jobs
}

func (f clientPreferenceFixture) attempts(t *testing.T, client *mcp.ClientSession, id any) []map[string]any {
	t.Helper()
	out := f.call(t, client, "get_job_result", map[string]any{"job_id": id, "view": "route"})
	wire := preferenceField[[]any](t, out, "attempts")
	if len(wire) == 0 {
		t.Fatalf("job %v has no executed attempts", id)
	}
	attempts := make([]map[string]any, 0, len(wire))
	for _, item := range wire {
		attempt, ok := item.(map[string]any)
		if !ok {
			t.Fatalf("job %v has malformed attempt: %v", id, item)
		}
		attempts = append(attempts, attempt)
	}
	return attempts
}

func (f clientPreferenceFixture) audit(t *testing.T, client *mcp.ClientSession, submission map[string]any, profile, source string, statuses ...string) map[string]any {
	t.Helper()
	id := preferenceField[string](t, submission, "job_id")
	out := f.call(t, client, "get_routing_audit", map[string]string{"job_id": id})
	_ = preferenceField[[]any](t, out, "attempts")
	if out["job_id"] != id || out["status"] != model.Completed || out["routing_profile"] != profile || out["routing_profile_source"] != source {
		t.Errorf("audit profile for %s = %v from %v, want %s from %s: %v",
			id, out["routing_profile"], out["routing_profile_source"], profile, source, out)
	}
	// C2 omits first_decision when admission recorded no selected route.
	decision, hasDecision := submission["route_decision"].(map[string]any)
	rawFirst, hasFirst := out["first_decision"]
	first, isObject := rawFirst.(map[string]any)
	if hasDecision != hasFirst || (hasDecision && (!isObject || first["selected_model"] != decision["selected_model"])) {
		t.Errorf("audit first decision for %s = %v, admission decision %v", id, rawFirst, decision)
	}
	executed, ok := out["execution_attempts"].([]any)
	if !ok || len(executed) != len(statuses) {
		t.Errorf("audit for %s has execution_attempts %v, want %d entries", id, out["execution_attempts"], len(statuses))
		return out
	}
	attempts := f.attempts(t, client, id)
	if len(attempts) != len(executed) {
		t.Fatalf("route result and audit disagree on attempts for %s: %v / %v", id, attempts, executed)
	}
	for i, status := range statuses {
		actual, ok := executed[i].(map[string]any)
		attempt := attempts[i]
		if !ok || actual["number"] != float64(i+1) || actual["provider"] != attempt["provider"] ||
			actual["model"] != attempt["model"] || actual["status"] != status || actual["reason_code"] != attempt["code"] {
			t.Errorf("execution attempt %d for %s = %v, want status %s for route attempt %v",
				i+1, id, actual, status, attempt)
		}
	}
	return out
}

func readPreferenceExample(t *testing.T) map[string]any {
	t.Helper()
	readme, err := os.ReadFile("../../README.md")
	if err != nil {
		t.Fatal(err)
	}
	_, section, found := strings.Cut(string(readme), "### Repository routing presets and one-job overrides")
	if !found {
		t.Fatal("README routing preset section is missing")
	}
	_, body, found := strings.Cut(section, "```json\n")
	if !found {
		t.Fatal("README override example is missing")
	}
	sample, _, found := strings.Cut(body, "\n```")
	var request map[string]any
	if err := json.Unmarshal([]byte(sample), &request); !found || err != nil {
		t.Fatalf("invalid README override example: %v", err)
	}
	return request
}

func preferenceSubmission(requestID, objective string) map[string]any {
	return map[string]any{
		"repo_id": "demo", "request_id": requestID, "target": map[string]string{"provider": "auto"},
		"objective": objective, "execution_mode": "async", "mode": "read_only", "data_class": "synthetic",
		"allowed_paths": []string{"fixture.txt"},
		"acceptance":    []model.Check{{ID: "inspect", Description: "Summarize the fixture"}},
	}
}

func preferenceField[T any](t *testing.T, out map[string]any, field string) T {
	t.Helper()
	value, ok := out[field].(T)
	if !ok {
		t.Fatalf("missing or malformed %s in %v", field, out)
	}
	return value
}
