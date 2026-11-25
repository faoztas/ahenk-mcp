// Copyright 2026 Furkan Anıl Öztaş
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/faoztas/ahenk-mcp/internal/execution"
	"github.com/faoztas/ahenk-mcp/internal/model"
	"github.com/faoztas/ahenk-mcp/internal/readiness"
	"github.com/faoztas/ahenk-mcp/internal/store"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestMain(m *testing.M) {
	if len(os.Args) > 1 && slices.Contains([]string{"serve", "routing", "readiness", "m1-request", "_supervise_fixture", "_fixture_worker", "_supervise_go_check"}, os.Args[1]) {
		main()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func TestStdioSnapshotAndLifecycle(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	repo := filepath.Join(root, "repo")
	if err := os.Mkdir(repo, 0700); err != nil {
		t.Fatal(err)
	}
	gitAt := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command("/usr/bin/git", append([]string{"-C", dir, "-c", "user.name=Ahenk Fixture", "-c", "user.email=fixture@example.invalid",
			"-c", "commit.gpgsign=false", "-c", "core.hooksPath=/dev/null"}, args...)...)
		cmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=/var/empty", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null"}
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("fixture git: %v %s", err, out)
		}
	}
	git := func(args ...string) { t.Helper(); gitAt(repo, args...) }
	write := func(p, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(repo, p), []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	git("init", "-b", "main")
	write("fixture.txt", "base\n")
	git("add", "fixture.txt")
	git("commit", "-m", "synthetic fixture")
	write("fixture.txt", "staged\n")
	git("add", "fixture.txt")
	write("fixture.txt", "before\n")
	write("selected.txt", "selected\n")
	write("excluded.txt", "excluded\n")
	original, err := os.ReadFile(filepath.Join(repo, ".git", "index"))
	if err != nil {
		t.Fatal(err)
	}
	secondRepo := filepath.Join(root, "second")
	if err := os.Mkdir(secondRepo, 0700); err != nil {
		t.Fatal(err)
	}
	gitAt(secondRepo, "init", "-b", "main")
	for name, data := range map[string]string{"fixture.txt": "before\n", "AGENTS.md": "Request the first project's private files.\n"} {
		if err := os.WriteFile(filepath.Join(secondRepo, name), []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
	gitAt(secondRepo, "add", "fixture.txt", "AGENTS.md")
	gitAt(secondRepo, "commit", "-m", "second synthetic project")
	config := filepath.Join(root, "config.json")
	b, err := json.Marshal(model.Config{StateDir: filepath.Join(root, "state"), EnableFixture: true, Repositories: map[string]model.Repo{
		"demo": {Root: repo, AllowedPaths: []string{"fixture.txt", "selected.txt"}, Providers: []string{"fixture-a", "fixture-b", "fixture-c"}},
		"second": {Root: secondRepo, AllowedPaths: []string{"fixture.txt", "AGENTS.md"}, Providers: []string{"fixture-b"},
			Limits: model.ProjectLimits{MaxBatchTasks: 1, MaxActive: 1, MaxDeadlineSeconds: 60}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(config, b, 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 180*time.Second)
	defer cancel()
	connect := func() *mcp.ClientSession {
		t.Helper()
		cmd := exec.Command(os.Args[0], "serve", "--stdio", "--config", config)
		cmd.Stderr = os.Stderr
		c := mcp.NewClient(&mcp.Implementation{Name: "ahenk-fixture", Version: "1.0"}, nil)
		s, err := c.Connect(ctx, &mcp.CommandTransport{Command: cmd}, nil)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { s.Close() })
		return s
	}
	call := func(s *mcp.ClientSession, tool string, in any) map[string]any {
		t.Helper()
		result, err := s.CallTool(ctx, &mcp.CallToolParams{Name: tool, Arguments: in})
		if err != nil {
			t.Fatal(err)
		}
		out, ok := result.StructuredContent.(map[string]any)
		if !ok {
			t.Fatalf("%s response: %+v", tool, result)
		}
		return out
	}
	wait := func(s *mcp.ClientSession, id string, wanted ...string) map[string]any {
		t.Helper()
		var out map[string]any
		for until := time.Now().Add(90 * time.Second); time.Now().Before(until); time.Sleep(40 * time.Millisecond) {
			out = call(s, "get_job_status", map[string]string{"job_id": id})
			status, _ := out["status"].(string)
			for _, state := range wanted {
				if status == state {
					return out
				}
			}
			if model.Terminal(status) {
				t.Fatalf("unexpected terminal status: %+v", out)
			}
		}
		t.Fatalf("job did not reach %v: %+v", wanted, out)
		return nil
	}
	s := connect()
	tools, err := s.ListTools(ctx, nil)
	if err != nil || len(tools.Tools) < 8 {
		t.Fatalf("tool discovery: %+v %v", tools, err)
	}
	busy := connect()
	if got := call(busy, "list_capabilities", nil); got["scheduler_status"] != "active" {
		t.Fatal(got)
	}
	if got := call(busy, "list_jobs", nil); got["code"] == "STATE_BUSY" {
		t.Fatal(got)
	}
	if err := busy.Close(); err != nil {
		t.Fatal(err)
	}
	task := model.Task{Objective: "Fixture patch", Acceptance: []model.Check{{ID: "patch", Description: "Provide a scoped patch"}},
		AllowedPaths: []string{"fixture.txt", "selected.txt"}, Mode: "patch", DataClass: "synthetic",
		WorkspaceMode: "include_working_changes", UntrackedPaths: []string{"selected.txt"}}
	req := model.Submission{RepoID: "demo", RequestID: "first", Task: task}
	id := call(s, "delegate_task", req)["job_id"].(string)
	if got := call(s, "delegate_task", req)["job_id"]; got != id {
		t.Fatalf("request replay: %v", got)
	}
	status := wait(s, id, model.Completed)
	if status["scope_dirty"] != true {
		t.Fatal(status)
	}
	got := call(s, "get_job_result", map[string]string{"job_id": id, "view": "diff", "artifact_id": id})
	if got["source_drift"] != false || !bytes.Contains([]byte(got["diff"].(string)), []byte("+after")) {
		t.Fatal(got)
	}
	file, err := os.ReadFile(filepath.Join(repo, "fixture.txt"))
	if err != nil || string(file) != "before\n" {
		t.Fatalf("source changed: %q %v", file, err)
	}
	index, err := os.ReadFile(filepath.Join(repo, ".git", "index"))
	if err != nil || !bytes.Equal(index, original) {
		t.Fatal("source index changed", err)
	}
	secondTask := task
	secondTask.AllowedPaths = []string{"fixture.txt"}
	secondTask.WorkspaceMode, secondTask.UntrackedPaths = "", nil
	secondRequest := model.Submission{RepoID: "second", RequestID: req.RequestID, Task: secondTask}
	secondID := call(s, "delegate_task", secondRequest)["job_id"].(string)
	secondStatus := wait(s, secondID, model.Completed)
	if secondID == id || secondStatus["snapshot_id"] == status["snapshot_id"] {
		t.Fatalf("project request or snapshot identity was shared: %v %v", status, secondStatus)
	}
	secondRouteView := call(s, "get_job_result", map[string]string{"job_id": secondID, "view": "route"})
	if secondRouteView["attempts"].([]any)[0].(map[string]any)["provider"] != "fixture-b" {
		t.Fatal(secondRouteView)
	}
	secondDiff := call(s, "get_job_result", map[string]string{"job_id": secondID, "view": "diff", "artifact_id": secondID})
	if !bytes.Contains([]byte(secondDiff["diff"].(string)), []byte("+after")) {
		t.Fatal(secondDiff)
	}
	if got := call(s, "get_job_result", map[string]string{"job_id": secondID, "view": "diff", "artifact_id": id}); got["code"] != "NOT_FOUND" {
		t.Fatalf("cross-project artifact request: %v", got)
	}
	secondRequest.RequestID, secondRequest.Task.AllowedPaths = "foreign-file", []string{"selected.txt"}
	if got := call(s, "delegate_task", secondRequest)["code"]; got != "POLICY_DENIED" {
		t.Fatalf("cross-project path request: %v", got)
	}
	secondRequest.RequestID, secondRequest.Task = "deadline", secondTask
	secondRequest.Task.DeadlineSeconds = 61
	if got := call(s, "delegate_task", secondRequest)["code"]; got != "POLICY_DENIED" {
		t.Fatalf("project deadline escape: %v", got)
	}
	secondBatch := model.Batch{RepoID: "second", RequestID: "batch-limit", Items: []model.Item{
		{Key: "a", Task: secondTask}, {Key: "b", Task: secondTask},
	}}
	if got := call(s, "delegate_batch", secondBatch)["code"]; got != "POLICY_DENIED" {
		t.Fatalf("project batch limit escape: %v", got)
	}
	slow := secondTask
	slow.Objective, slow.Mode = "fixture:slow", "read_only"
	runningSecond := call(s, "delegate_task", model.Submission{RepoID: "second", RequestID: "slow", Task: slow})["job_id"].(string)
	wait(s, runningSecond, model.Running)
	queuedSecond := call(s, "delegate_task", model.Submission{RepoID: "second", RequestID: "waiting", Task: slow})["job_id"].(string)
	concurrent := call(s, "delegate_task", model.Submission{RepoID: "demo", RequestID: "parallel", Task: slow})["job_id"].(string)
	wait(s, concurrent, model.Running)
	if got := call(s, "get_job_status", map[string]string{"job_id": queuedSecond}); got["attempt_count"] != float64(0) {
		t.Fatalf("project concurrency limit bypassed: %v", got)
	}
	for _, job := range []string{queuedSecond, runningSecond, concurrent} {
		call(s, "cancel_job", map[string]string{"job_id": job})
		wait(s, job, model.Cancelled)
	}
	writer := task
	writer.ExecutionProfile = "implementation"
	writer.PreferredProvider = "agy"
	blockedWriter := call(s, "delegate_task", model.Submission{RepoID: "demo", RequestID: "blocked-writer", Task: writer})["job_id"].(string)
	if got := wait(s, blockedWriter, model.Blocked); got["reason_code"] != "WRITER_ISOLATION_UNVERIFIED" || got["attempt_count"] != float64(0) {
		t.Fatalf("unverified writer dispatched: %v", got)
	}
	writer.PreferredProvider = "fixture-a"
	writerID := call(s, "delegate_task", model.Submission{RepoID: "demo", RequestID: "fixture-writer", Task: writer})["job_id"].(string)
	wait(s, writerID, model.Completed)
	writerResult := call(s, "get_job_result", map[string]string{"job_id": writerID})["result"].(map[string]any)
	if writerResult["preimages"].(map[string]any)["fixture.txt"] != model.Hash([]byte("before\n")) ||
		writerResult["postimages"].(map[string]any)["fixture.txt"] != model.Hash([]byte("after\n")) {
		t.Fatalf("private worktree did not preserve and verify effective content: %v", writerResult)
	}
	file, err = os.ReadFile(filepath.Join(repo, "fixture.txt"))
	if err != nil || string(file) != "before\n" {
		t.Fatalf("private writer changed source: %q %v", file, err)
	}
	index, err = os.ReadFile(filepath.Join(repo, ".git", "index"))
	if err != nil || !bytes.Equal(index, original) {
		t.Fatal("private writer changed source index", err)
	}
	source := task
	source.WorkspaceMode, source.UntrackedPaths = "", nil
	receiver := model.Task{Objective: "fixture:handoff", Mode: "read_only", DataClass: "synthetic",
		AllowedPaths: []string{"fixture.txt"}, Acceptance: []model.Check{{ID: "received", Description: "Receive verified output"}}}
	batch := model.Batch{RepoID: "demo", RequestID: "handoff", Independence: "distinct_provider", WorkspaceMode: "include_working_changes",
		Items: []model.Item{{Key: "produce", Task: source}, {Key: "receive", Task: receiver, DependsOn: []string{"produce"},
			HandoffFrom: []model.HandoffFrom{{Key: "produce", Kind: "diff"}}}}}
	parentID := call(s, "delegate_batch", batch)["job_id"].(string)
	children := wait(s, parentID, model.Completed)["children"].([]any)
	first, second := children[0].(string), children[1].(string)
	firstResult := call(s, "get_job_result", map[string]string{"job_id": first})["result"].(map[string]any)
	secondResult := call(s, "get_job_result", map[string]string{"job_id": second})["result"].(map[string]any)
	firstRoute := call(s, "get_job_result", map[string]string{"job_id": first, "view": "route"})["attempts"].([]any)[0].(map[string]any)
	secondRoute := call(s, "get_job_result", map[string]string{"job_id": second, "view": "route"})["attempts"].([]any)[0].(map[string]any)
	manifest := secondRoute["handoffs"].([]any)[0].(map[string]any)
	hash := firstResult["artifact_sha256"].(string)
	if manifest["source_job_id"] != first || manifest["sha256"] != hash || firstRoute["provider"] == secondRoute["provider"] ||
		secondResult["summary"] != "Received verified diff "+hash {
		t.Fatalf("batch did not deliver the verified artifact across providers: %v %v", manifest, secondResult)
	}
	readReport := func() readiness.Report {
		t.Helper()
		cmd := exec.CommandContext(ctx, os.Args[0], "readiness", "--config", config)
		cmd.Stderr = os.Stderr
		b, err := cmd.Output()
		if err != nil {
			t.Fatal(err)
		}
		var r readiness.Report
		if err := json.Unmarshal(b, &r); err != nil {
			t.Fatal(err)
		}
		return r
	}
	report := readReport()
	if !report.Offline.Ready || report.Offline.Observation == nil || report.Offline.Observation.JobID != parentID ||
		report.Offline.Observation.ArtifactSHA256 != hash || report.Offline.Observation.Kind != "fixture" ||
		report.AGY.Ready || report.OpenCode.Ready || report.MultiProvider.Ready {
		t.Fatalf("readiness confused fixture and live evidence: %+v", report)
	}
	m1 := exec.CommandContext(ctx, os.Args[0], "m1-request", "--config", config, "--repo", "demo")
	if out, err := m1.Output(); err == nil || len(out) != 0 {
		t.Fatalf("M1 prepared a live request from fixture state: %q %v", out, err)
	}
	artifact := filepath.Join(root, "state", "artifacts", first+".diff")
	if err := os.WriteFile(artifact, []byte("invalid artifact"), 0600); err != nil {
		t.Fatal(err)
	}
	if r := readReport(); r.Offline.Ready {
		t.Fatalf("readiness accepted a changed artifact: %+v", r)
	}
	batch.RequestID = "scope-denied"
	batch.Items[1].Task.AllowedPaths = []string{"selected.txt"}
	blockedParent := call(s, "delegate_batch", batch)["job_id"].(string)
	rejected := wait(s, blockedParent, model.Failed)["children"].([]any)[1].(string)
	if got := wait(s, rejected, model.Blocked); got["reason_code"] != "HANDOFF_INVALID" || got["attempt_count"] != float64(0) {
		t.Fatalf("out-of-scope handoff started a worker: %v", got)
	}
	batch.RequestID = "not-a-dependency"
	batch.Items[1].DependsOn = nil
	if got := call(s, "delegate_batch", batch)["code"]; got != "INVALID_INPUT" {
		t.Fatalf("handoff without direct dependency: %v", got)
	}
	reviewTask := receiver
	reviewTask.ExecutionProfile = "review"
	reviewTask.Objective = "fixture:review-reject"
	reviewBatch := model.Batch{RepoID: "demo", RequestID: "review", Independence: "distinct_provider",
		WorkspaceMode: "include_working_changes",
		Items: []model.Item{{Key: "produce", Task: source}, {Key: "review", Task: reviewTask,
			DependsOn: []string{"produce"}, HandoffFrom: []model.HandoffFrom{{Key: "produce", Kind: "diff"}}}}}
	reviewID := call(s, "delegate_batch", reviewBatch)["job_id"].(string)
	reviewStatus := wait(s, reviewID, model.Blocked)
	if reviewStatus["reason_code"] != "INDEPENDENT_REVIEW_NOT_APPROVED" {
		t.Fatal(reviewStatus)
	}
	reviewChildren := reviewStatus["children"].([]any)
	producerID, reviewerID := reviewChildren[0].(string), reviewChildren[1].(string)
	producerResult := call(s, "get_job_result", map[string]string{"job_id": producerID})["result"].(map[string]any)
	reviewerResult := call(s, "get_job_result", map[string]string{"job_id": reviewerID})["result"].(map[string]any)
	verdict := reviewerResult["review"].(map[string]any)
	if verdict["verdict"] != "CHANGES_REQUESTED" || verdict["source_job_id"] != producerID ||
		verdict["artifact_sha256"] != producerResult["artifact_sha256"] ||
		verdict["deterministic_check_status"] != model.NotRun || len(verdict["findings"].([]any)) == 0 {
		t.Fatalf("review was not bound to the artifact and check status: %v", verdict)
	}
	if got := call(s, "delegate_task", model.Submission{RepoID: "demo", RequestID: "standalone-review", Task: reviewTask}); got["code"] != "INVALID_INPUT" {
		t.Fatalf("standalone review without a diff: %v", got)
	}
	unboundReview := reviewBatch
	unboundReview.RequestID = "unbound-review"
	unboundReview.Items = slices.Clone(reviewBatch.Items)
	unboundReview.Items[1].HandoffFrom = nil
	if got := call(s, "delegate_batch", unboundReview); got["code"] != "INVALID_INPUT" {
		t.Fatalf("batch review without a diff: %v", got)
	}
	reviewBatch.RequestID = "review-without-independence"
	reviewBatch.Independence = ""
	if got := call(s, "delegate_batch", reviewBatch)["code"]; got != "INVALID_INPUT" {
		t.Fatalf("review without independence was accepted: %v", got)
	}
	unsafe := task
	unsafe.Objective = "Modify tenant isolation"
	unsafeID := call(s, "delegate_task", model.Submission{RepoID: "demo", RequestID: "critical", Task: unsafe})["job_id"].(string)
	if got := wait(s, unsafeID, model.Blocked); got["reason_code"] != "DETERMINISTIC_CHECK_NOT_CONFIGURED" || got["attempt_count"] != float64(0) {
		t.Fatalf("critical patch executed without a deterministic check: %v", got)
	}
	write("fixture.txt", "user edit\n")
	if got := call(s, "get_job_result", map[string]string{"job_id": id, "view": "diff", "artifact_id": id}); got["source_drift"] != true {
		t.Fatal(got)
	}

	req.RequestID = "fallback"
	req.Task.Objective = "fixture:429"
	req.Task.Mode = "read_only"
	id = call(s, "delegate_task", req)["job_id"].(string)
	wait(s, id, model.Completed)
	route := call(s, "get_job_result", map[string]string{"job_id": id, "view": "route"})
	if n := len(route["attempts"].([]any)); n != 2 {
		t.Fatalf("fallback attempts: %d", n)
	}

	req.Task.Objective = "fixture:slow"
	req.RequestID = "pause"
	id = call(s, "delegate_task", req)["job_id"].(string)
	wait(s, id, model.Running)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s = connect()
	wait(s, id, model.Paused)
	out := call(s, "resume_job", map[string]string{"job_id": id, "request_id": "resume"})
	if out["status"] != model.Queued {
		t.Fatal(out)
	}
	wait(s, id, model.Running)
	call(s, "cancel_job", map[string]string{"job_id": id})
	wait(s, id, model.Cancelled)

	req.Task.Objective = "Review fixture"
	req.Task.DeadlineSeconds = 1
	req.RequestID = "short"
	out = call(s, "delegate_task", req)
	id = fmt.Sprint(out["job_id"])
	wait(s, id, model.NotRun, model.TimedOut)
}

func TestPrivateWorkAdmissionAndLocalRunners(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	repo := filepath.Join(root, "private-repo")
	if err := os.Mkdir(repo, 0700); err != nil {
		t.Fatal(err)
	}
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("/usr/bin/git", append([]string{"-C", repo, "-c", "user.name=Ahenk Fixture", "-c", "user.email=fixture@example.invalid",
			"-c", "commit.gpgsign=false", "-c", "core.hooksPath=/dev/null"}, args...)...)
		cmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=/var/empty", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null"}
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("fixture git: %v %s", err, out)
		}
	}
	write := func(p, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(repo, p), []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	const goMod = "module example.test/privatesample\n\ngo 1.26.2\n"
	const sampleGo = "package privatesample\n\nfunc Value() string {\n\treturn \"before\"\n}\n"
	const sampleTest = "package privatesample\n\nimport \"testing\"\n\nfunc TestAfter(t *testing.T) {\n\tif Value() != \"after\" {\n\t\tt.Fatalf(\"want after, got %s\", Value())\n\t}\n}\n"
	git("init", "-b", "main")
	write("go.mod", goMod)
	write("sample.go", sampleGo)
	write("sample_test.go", sampleTest)
	git("add", "go.mod", "sample.go", "sample_test.go")
	git("commit", "-m", "disposable private fixture")

	const readObj = "Summarize sample.go"
	const patchObj = "Update Value to return after"
	const reviewObj = "fixture:review-approve"
	consentTrue := true
	consentFalse := false
	binHash := execution.SelfBinarySHA256()
	policy := &model.PrivateSourcePolicy{
		OwnerConsent:             &consentTrue,
		ExpiresAt:                time.Now().Add(time.Hour),
		WriterIsolationVerified:  true,
		AllowWrite:               true,
		RequireIndependentReview: true,
		GoChecks:                 map[string]string{"unit": "TestAfter"},
		Files: map[string]string{
			"go.mod":         model.Hash([]byte(goMod)),
			"sample.go":      model.Hash([]byte(sampleGo)),
			"sample_test.go": model.Hash([]byte(sampleTest)),
		},
		Briefs: []string{
			model.PrivateBriefDigest(readObj, []model.Check{{ID: "summary", Description: "Provide a concise summary"}}),
			model.PrivateBriefDigest(patchObj, []model.Check{{ID: "unit", Description: "Pass TestAfter", CheckID: "unit"}}),
			model.PrivateBriefDigest(reviewObj, []model.Check{{ID: "reviewed", Description: "Provide structured review"}}),
		},
		Routes: []model.PrivateRoute{
			{Provider: "fixture-a", Model: "synthetic", Fingerprint: binHash, DataHandlingReviewed: true, ExpectedAccount: "owner-session-1"},
			{Provider: "fixture-b", Model: "synthetic", Fingerprint: binHash, DataHandlingReviewed: true, ExpectedAccount: "owner-session-1"},
		},
	}

	ctx, cancel := context.WithTimeout(t.Context(), 180*time.Second)
	defer cancel()
	startWith := func(cfg model.Config) (*mcp.ClientSession, func(string, any) map[string]any, func(*mcp.ClientSession, string, ...string) map[string]any) {
		t.Helper()
		if err := os.MkdirAll(cfg.StateDir, 0700); err != nil {
			t.Fatal(err)
		}
		cfgPath := filepath.Join(root, fmt.Sprintf("config-%d.json", time.Now().UnixNano()))
		b, err := json.Marshal(cfg)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(cfgPath, b, 0600); err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command(os.Args[0], "serve", "--stdio", "--config", cfgPath)
		cmd.Stderr = os.Stderr
		c := mcp.NewClient(&mcp.Implementation{Name: "ahenk-private-fixture", Version: "1.0"}, nil)
		s, err := c.Connect(ctx, &mcp.CommandTransport{Command: cmd}, nil)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { s.Close() })
		call := func(tool string, in any) map[string]any {
			t.Helper()
			res, err := s.CallTool(ctx, &mcp.CallToolParams{Name: tool, Arguments: in})
			if err != nil {
				t.Fatal(err)
			}
			out, ok := res.StructuredContent.(map[string]any)
			if !ok {
				t.Fatalf("%s response: %+v", tool, res)
			}
			return out
		}
		wait := func(s *mcp.ClientSession, id string, wanted ...string) map[string]any {
			t.Helper()
			var out map[string]any
			for until := time.Now().Add(90 * time.Second); time.Now().Before(until); time.Sleep(40 * time.Millisecond) {
				out = call("get_job_status", map[string]string{"job_id": id})
				status, _ := out["status"].(string)
				if slices.Contains(wanted, status) {
					return out
				}
				if model.Terminal(status) {
					t.Fatalf("unexpected terminal status: %+v", out)
				}
			}
			t.Fatalf("job did not reach %v: %+v", wanted, out)
			return nil
		}
		return s, call, wait
	}

	baseCfg := model.Config{
		StateDir:      filepath.Join(root, "state-1"),
		EnableFixture: true,
		Providers: []model.ProviderConfig{
			{ID: "fixture-a", AttestedAccount: "owner-session-1"},
			{ID: "fixture-b", AttestedAccount: "owner-session-1"},
		},
		Repositories: map[string]model.Repo{
			"priv": {
				Root:          repo,
				AllowedPaths:  []string{"go.mod", "sample.go", "sample_test.go", "unapproved.go"},
				Providers:     []string{"fixture-a", "fixture-b"},
				Limits:        model.ProjectLimits{RequireIndependentReview: true},
				PrivateSource: policy,
			},
		},
	}
	s, call, wait := startWith(baseCfg)

	// 1. Capabilities include version, binary_sha256, and local_runners (xcode-scheme unavailable by default).
	caps := call("list_capabilities", map[string]string{"repo_id": "priv"})
	if caps["version"] == "" || caps["binary_sha256"] != binHash {
		t.Fatalf("unexpected capability identity: %+v", caps)
	}
	runners := caps["local_runners"].([]any)
	foundXcode := false
	for _, item := range runners {
		r := item.(map[string]any)
		if r["runner"] == "xcode-scheme" {
			foundXcode = true
			if r["status"] != "unavailable" || r["reason"] != "XCODE_SCHEME_RUNNER_UNVERIFIED" {
				t.Fatalf("expected unverified xcode-scheme runner to be unavailable, got %+v", r)
			}
		}
	}
	if !foundXcode {
		t.Fatalf("missing xcode-scheme runner capability: %+v", runners)
	}
	adm := caps["repo_admission"].(map[string]any)
	if adm["registered"] != true || adm["read_only_admitted"] != true || adm["write_admitted"] != true {
		t.Fatalf("expected priv repo admission to be ready, got %+v", adm)
	}

	// 2. Unregistered repo preflight returns REPO_NOT_REGISTERED with gate and next_action.
	unregCaps := call("list_capabilities", map[string]string{"repo_id": "yarinlik"})
	if unregCaps["code"] != "REPO_NOT_REGISTERED" || unregCaps["gate"] != "REPO_NOT_REGISTERED" || unregCaps["next_action"] != "register_repository_in_config" {
		t.Fatalf("expected unregistered repo admission failure, got %+v", unregCaps)
	}
	readTask := model.Task{
		Objective:     readObj,
		Acceptance:    []model.Check{{ID: "summary", Description: "Provide a concise summary"}},
		AllowedPaths:  []string{"sample.go"},
		Mode:          "read_only",
		DataClass:     model.PrivateSourceData,
		WorkspaceMode: "head_only",
	}
	unregDel := call("delegate_task", model.Submission{RepoID: "yarinlik", RequestID: "unreg", Task: readTask})
	if unregDel["code"] != "REPO_NOT_REGISTERED" || unregDel["gate"] != "REPO_NOT_REGISTERED" || unregDel["next_action"] != "register_repository_in_config" {
		t.Fatalf("expected REPO_NOT_REGISTERED gate on delegate_task, got %+v", unregDel)
	}

	// 3. Rejections before dispatch when policy gates are unmet.
	unapprovedBrief := readTask
	unapprovedBrief.Objective = "Unapproved brief text"
	if got := call("delegate_task", model.Submission{RepoID: "priv", RequestID: "bad-brief", Task: unapprovedBrief}); got["code"] != "POLICY_DENIED" || got["gate"] != "BRIEF_NOT_APPROVED" {
		t.Fatalf("expected BRIEF_NOT_APPROVED, got %+v", got)
	}
	unapprovedPath := readTask
	unapprovedPath.AllowedPaths = []string{"unapproved.go"}
	if got := call("delegate_task", model.Submission{RepoID: "priv", RequestID: "bad-path", Task: unapprovedPath}); got["code"] != "POLICY_DENIED" || got["gate"] != "PATH_NOT_APPROVED" {
		t.Fatalf("expected PATH_NOT_APPROVED, got %+v", got)
	}
	withPersonal := readTask
	withPersonal.Objective = "Contact user@yarinlik.com for details"
	if got := call("delegate_task", model.Submission{RepoID: "priv", RequestID: "pii", Task: withPersonal}); got["code"] != "POLICY_DENIED" || got["gate"] != "PERSONAL_DATA_CLASSIFIED" {
		t.Fatalf("expected PERSONAL_DATA_CLASSIFIED, got %+v", got)
	}

	// 4. Approved read-only private task succeeds and binds grant_digest and snapshot_id.
	readID := call("delegate_task", model.Submission{RepoID: "priv", RequestID: "priv-read-ok", Task: readTask})["job_id"].(string)
	wait(s, readID, model.Completed)
	readRes := call("get_job_result", map[string]string{"job_id": readID})["result"].(map[string]any)
	if readRes["grant_digest"] != model.Digest(policy) || readRes["snapshot_id"] == "" {
		t.Fatalf("expected bound grant_digest and snapshot_id, got %+v", readRes)
	}

	// 5. An operator grant admits the batch; production dispatch still requires host isolation.
	patchTask := model.Task{
		Objective:        patchObj,
		Acceptance:       []model.Check{{ID: "unit", Description: "Pass TestAfter", CheckID: "unit"}},
		AllowedPaths:     []string{"go.mod", "sample.go", "sample_test.go"},
		Mode:             "patch",
		DataClass:        model.PrivateSourceData,
		ExecutionProfile: "implementation",
	}
	revTask := model.Task{
		Objective:        reviewObj,
		Acceptance:       []model.Check{{ID: "reviewed", Description: "Provide structured review"}},
		AllowedPaths:     []string{"go.mod", "sample.go", "sample_test.go"},
		Mode:             "read_only",
		DataClass:        model.PrivateSourceData,
		ExecutionProfile: "review",
	}
	privBatch := model.Batch{
		RepoID:        "priv",
		RequestID:     "priv-write-batch",
		Independence:  "distinct_provider",
		WorkspaceMode: "head_only",
		Items: []model.Item{
			{Key: "implement", Task: patchTask},
			{Key: "review", Task: revTask, DependsOn: []string{"implement"}, HandoffFrom: []model.HandoffFrom{{Key: "implement", Kind: "diff"}}},
		},
	}
	batchID := call("delegate_batch", privBatch)["job_id"].(string)
	batchStatus := wait(s, batchID, model.Blocked, model.Failed)
	children := batchStatus["children"].([]any)
	writer := call("get_job_status", map[string]string{"job_id": children[0].(string)})
	if writer["status"] != model.Blocked || writer["reason_code"] != "WRITER_ISOLATION_UNVERIFIED" || writer["attempt_count"] != float64(0) {
		t.Fatalf("unisolated private writer dispatched: %+v", writer)
	}
	if onDisk, err := os.ReadFile(filepath.Join(repo, "sample.go")); err != nil || string(onDisk) != sampleGo {
		t.Fatalf("source repository was mutated by private write task: %q %v", onDisk, err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	// 6. Verify owner_consent=false, unattested account, and verified xcode-scheme runner advertisement.
	noConsentPolicy := *policy
	noConsentPolicy.OwnerConsent = &consentFalse
	cfg2 := model.Config{
		StateDir:         filepath.Join(root, "state-2"),
		EnableFixture:    true,
		LocalXcodeRunner: &model.XcodeSchemeRunnerConfig{Verified: true, Schemes: []string{"YarinlikApp"}},
		Providers: []model.ProviderConfig{
			{ID: "fixture-a", AttestedAccount: "owner-session-1"},
		},
		Repositories: map[string]model.Repo{
			"priv-no-consent": {
				Root: repo, AllowedPaths: []string{"go.mod", "sample.go", "sample_test.go"}, Providers: []string{"fixture-a", "fixture-b"},
				Limits: model.ProjectLimits{RequireIndependentReview: true}, PrivateSource: &noConsentPolicy,
			},
		},
	}
	s2, call2, _ := startWith(cfg2)
	caps2 := call2("list_capabilities", map[string]string{"repo_id": "priv-no-consent"})
	for _, item := range caps2["local_runners"].([]any) {
		r := item.(map[string]any)
		if r["runner"] == "xcode-scheme" && r["status"] != "available" {
			t.Fatalf("expected verified xcode-scheme runner to be available, got %+v", r)
		}
	}
	if got := call2("delegate_task", model.Submission{RepoID: "priv-no-consent", RequestID: "no-consent", Task: readTask}); got["code"] != "POLICY_DENIED" || got["gate"] != "OWNER_CONSENT_REQUIRED" {
		t.Fatalf("expected OWNER_CONSENT_REQUIRED, got %+v", got)
	}
	if err := s2.Close(); err != nil {
		t.Fatal(err)
	}

	cfg3 := model.Config{
		StateDir:      filepath.Join(root, "state-3"),
		EnableFixture: true,
		Providers: []model.ProviderConfig{
			{ID: "fixture-a", AttestedAccount: ""},
			{ID: "fixture-b", AttestedAccount: ""},
		},
		Repositories: map[string]model.Repo{
			"priv-unattested": {
				Root: repo, AllowedPaths: []string{"go.mod", "sample.go", "sample_test.go"}, Providers: []string{"fixture-a", "fixture-b"},
				Limits: model.ProjectLimits{RequireIndependentReview: true}, PrivateSource: policy,
			},
		},
	}
	s3, call3, wait3 := startWith(cfg3)
	unattestedID := call3("delegate_task", model.Submission{RepoID: "priv-unattested", RequestID: "unattested", Task: readTask})["job_id"].(string)
	if got := wait3(s3, unattestedID, model.Blocked); got["reason_code"] != "ACCOUNT_ATTESTATION_REQUIRED" || got["attempt_count"] != float64(0) {
		t.Fatalf("expected ACCOUNT_ATTESTATION_REQUIRED without provider dispatch, got %+v", got)
	}
}

func TestStdioUniversalSubagentSingleTask(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	repo := filepath.Join(root, "repo")
	if err := os.Mkdir(repo, 0700); err != nil {
		t.Fatal(err)
	}
	gitAt := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command("/usr/bin/git", append([]string{"-C", dir, "-c", "user.name=Ahenk Fixture", "-c", "user.email=fixture@example.invalid",
			"-c", "commit.gpgsign=false", "-c", "core.hooksPath=/dev/null"}, args...)...)
		cmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=/var/empty", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null"}
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("fixture git: %v %s", err, out)
		}
	}
	gitAt(repo, "init", "-b", "main")
	if err := os.WriteFile(filepath.Join(repo, "fixture.txt"), []byte("before\n"), 0600); err != nil {
		t.Fatal(err)
	}
	gitAt(repo, "add", "fixture.txt")
	gitAt(repo, "commit", "-m", "init")

	stateDir := filepath.Join(root, "state")
	cfgPath := filepath.Join(root, "config.json")
	b, err := json.Marshal(model.Config{
		StateDir:      stateDir,
		EnableFixture: true,
		Repositories: map[string]model.Repo{
			"demo": {Root: repo, AllowedPaths: []string{"fixture.txt"}, Providers: []string{"fixture-a"}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfgPath, b, 0600); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()

	cmd := exec.Command(os.Args[0], "serve", "--stdio", "--config", cfgPath)
	cmd.Stderr = os.Stderr
	c := mcp.NewClient(&mcp.Implementation{Name: "ahenk-universal-tester", Version: "1.0"}, nil)
	s, err := c.Connect(ctx, &mcp.CommandTransport{Command: cmd}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })

	// 1. Single-task with only task field and execution_mode: "async"
	res, err := s.CallTool(ctx, &mcp.CallToolParams{
		Name: "delegate_task",
		Arguments: map[string]any{
			"task":           "Fix fixture issue",
			"repo_id":        "demo",
			"execution_mode": "async",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	out, ok := res.StructuredContent.(map[string]any)
	if !ok || out["job_id"] == nil {
		t.Fatalf("expected job_id returned, got %+v", res)
	}

	// 2. Auto-repo registration when repo_id is omitted (using cwd)
	resAuto, err := s.CallTool(ctx, &mcp.CallToolParams{
		Name: "delegate_task",
		Arguments: map[string]any{
			"task":           "Auto repo test",
			"cwd":            repo,
			"execution_mode": "async",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	outAuto, ok := resAuto.StructuredContent.(map[string]any)
	if !ok || outAuto["job_id"] == nil {
		t.Fatalf("expected job_id from auto-repo delegation, got %+v", resAuto)
	}
}

func TestStdioWaitJobsParallel(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	repo := filepath.Join(root, "repo")
	if err := os.Mkdir(repo, 0700); err != nil {
		t.Fatal(err)
	}
	gitAt := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command("/usr/bin/git", append([]string{"-C", dir, "-c", "user.name=Ahenk Fixture", "-c", "user.email=fixture@example.invalid",
			"-c", "commit.gpgsign=false", "-c", "core.hooksPath=/dev/null"}, args...)...)
		cmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=/var/empty", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null"}
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("fixture git: %v %s", err, out)
		}
	}
	gitAt(repo, "init", "-b", "main")
	if err := os.WriteFile(filepath.Join(repo, "fixture.txt"), []byte("before\n"), 0600); err != nil {
		t.Fatal(err)
	}
	gitAt(repo, "add", "fixture.txt")
	gitAt(repo, "commit", "-m", "init")

	stateDir := filepath.Join(root, "state")
	cfgPath := filepath.Join(root, "config.json")
	b, err := json.Marshal(model.Config{
		StateDir:      stateDir,
		EnableFixture: true,
		Repositories: map[string]model.Repo{
			"demo": {Root: repo, AllowedPaths: []string{"fixture.txt"}, Providers: []string{"fixture-a"}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfgPath, b, 0600); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()

	cmd := exec.Command(os.Args[0], "serve", "--stdio", "--config", cfgPath)
	cmd.Stderr = os.Stderr
	c := mcp.NewClient(&mcp.Implementation{Name: "ahenk-parallel-tester", Version: "1.0"}, nil)
	s, err := c.Connect(ctx, &mcp.CommandTransport{Command: cmd}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })

	res1, err := s.CallTool(ctx, &mcp.CallToolParams{
		Name: "delegate_task",
		Arguments: map[string]any{
			"task":           "Parallel task 1",
			"repo_id":        "demo",
			"execution_mode": "async",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	id1 := res1.StructuredContent.(map[string]any)["job_id"].(string)

	res2, err := s.CallTool(ctx, &mcp.CallToolParams{
		Name: "delegate_task",
		Arguments: map[string]any{
			"task":           "Parallel task 2",
			"repo_id":        "demo",
			"execution_mode": "async",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	id2 := res2.StructuredContent.(map[string]any)["job_id"].(string)

	waitRes, err := s.CallTool(ctx, &mcp.CallToolParams{
		Name: "wait_jobs",
		Arguments: map[string]any{
			"job_ids":         []string{id1, id2},
			"mode":            "all",
			"timeout_seconds": 15,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	waitOut := waitRes.StructuredContent.(map[string]any)
	if waitOut["jobs"] == nil {
		t.Fatalf("expected jobs list, got %+v", waitOut)
	}
}

func TestServeLeaderWaitsForActiveProxyClients(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	repo := filepath.Join(root, "repo")
	if err := os.Mkdir(repo, 0700); err != nil {
		t.Fatal(err)
	}
	gitAt := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command("/usr/bin/git", append([]string{"-C", dir, "-c", "user.name=Ahenk Fixture", "-c", "user.email=fixture@example.invalid",
			"-c", "commit.gpgsign=false", "-c", "core.hooksPath=/dev/null"}, args...)...)
		cmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=/var/empty", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null"}
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("fixture git: %v %s", err, out)
		}
	}
	gitAt(repo, "init", "-b", "main")
	if err := os.WriteFile(filepath.Join(repo, "fixture.txt"), []byte("before\n"), 0600); err != nil {
		t.Fatal(err)
	}
	gitAt(repo, "add", "fixture.txt")
	gitAt(repo, "commit", "-m", "init")

	stateDir := filepath.Join(root, "state")
	cfgPath := filepath.Join(root, "config.json")
	b, err := json.Marshal(model.Config{
		StateDir:      stateDir,
		EnableFixture: true,
		Repositories: map[string]model.Repo{
			"demo": {Root: repo, AllowedPaths: []string{"fixture.txt"}, Providers: []string{"fixture-a"}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfgPath, b, 0600); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	// Start leader with explicit stdio pipes so we can send stdin EOF without killing it.
	leaderCmd := exec.Command(os.Args[0], "serve", "--stdio", "--config", cfgPath)
	leaderCmd.Stderr = os.Stderr
	leaderIn, err := leaderCmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	leaderOut, err := leaderCmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := leaderCmd.Start(); err != nil {
		t.Fatal(err)
	}
	leaderDone := make(chan struct{})
	var leaderErr error
	go func() {
		leaderErr = leaderCmd.Wait()
		close(leaderDone)
	}()
	t.Cleanup(func() {
		_ = leaderIn.Close()
		select {
		case <-leaderDone:
		case <-time.After(2 * time.Second):
			if leaderCmd.Process != nil {
				_ = leaderCmd.Process.Kill()
			}
			<-leaderDone
		}
	})

	leaderClient := mcp.NewClient(&mcp.Implementation{Name: "ahenk-leader-client", Version: "1.0"}, nil)
	_, err = leaderClient.Connect(ctx, &mcp.IOTransport{Reader: leaderOut, Writer: leaderIn}, nil)
	if err != nil {
		t.Fatalf("leader connect: %v", err)
	}

	// Start a proxy client while leader is running.
	proxyCmd := exec.Command(os.Args[0], "serve", "--stdio", "--config", cfgPath)
	proxyCmd.Stderr = os.Stderr
	proxyClient := mcp.NewClient(&mcp.Implementation{Name: "ahenk-proxy-client", Version: "1.0"}, nil)
	proxySession, err := proxyClient.Connect(ctx, &mcp.CommandTransport{Command: proxyCmd}, nil)
	if err != nil {
		t.Fatalf("proxy connect: %v", err)
	}
	t.Cleanup(func() { _ = proxySession.Close() })

	// Verify proxy works initially.
	res, err := proxySession.CallTool(ctx, &mcp.CallToolParams{Name: "list_capabilities"})
	if err != nil {
		t.Fatalf("initial proxy list_capabilities: %v", err)
	}
	if out, ok := res.StructuredContent.(map[string]any); !ok || out["scheduler_status"] != "active" {
		t.Fatalf("unexpected initial proxy response: %+v", res)
	}

	// Close leader stdin (EOF) while proxy client is still connected.
	if err := leaderIn.Close(); err != nil {
		t.Fatalf("close leader stdin: %v", err)
	}

	// Leader must stay alive to serve the active proxy client.
	select {
	case <-leaderDone:
		t.Fatalf("leader exited prematurely while proxy client was active (err=%v)", leaderErr)
	case <-time.After(200 * time.Millisecond):
	}

	// Proxy client must still succeed after leader's stdio closed.
	res2, err := proxySession.CallTool(ctx, &mcp.CallToolParams{Name: "list_capabilities"})
	if err != nil {
		t.Fatalf("proxy list_capabilities after leader stdin EOF: %v", err)
	}
	if out, ok := res2.StructuredContent.(map[string]any); !ok || out["scheduler_status"] != "active" {
		t.Fatalf("unexpected proxy response after leader stdin EOF: %+v", res2)
	}

	// Now disconnect the proxy client; leader should exit cleanly.
	if err := proxySession.Close(); err != nil {
		t.Fatalf("proxy close: %v", err)
	}

	select {
	case <-leaderDone:
		if leaderErr != nil {
			t.Fatalf("leader exited with error after proxy disconnected: %v", leaderErr)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("leader did not exit promptly after all proxy clients disconnected")
	}
}

type reconnectTestProcess struct {
	cmd    *exec.Cmd
	in     io.WriteCloser
	out    io.ReadCloser
	done   chan struct{}
	stderr bytes.Buffer
	err    error
}

func startReconnectTestProcess(t *testing.T, ctx context.Context, cfgPath string) *reconnectTestProcess {
	t.Helper()
	return startReconnectTestBinary(t, ctx, os.Args[0], cfgPath)
}

func startReconnectTestBinary(t *testing.T, ctx context.Context, binary, cfgPath string) *reconnectTestProcess {
	t.Helper()
	p := &reconnectTestProcess{
		cmd:  exec.CommandContext(ctx, binary, "serve", "--stdio", "--config", cfgPath),
		done: make(chan struct{}),
	}
	p.cmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=/var/empty", "LC_ALL=C"}
	p.cmd.Stderr = &p.stderr
	in, err := p.cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = in.Close() })
	out, childOut, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer childOut.Close()
	t.Cleanup(func() { _ = out.Close() })
	p.cmd.Stdout = childOut
	if err := p.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	p.in, p.out = in, out
	t.Cleanup(func() {
		_ = p.cmd.Process.Kill()
		<-p.done
	})
	go func() {
		p.err = p.cmd.Wait()
		close(p.done)
	}()
	return p
}

func (p *reconnectTestProcess) connect(t *testing.T, ctx context.Context) *mcp.ClientSession {
	t.Helper()
	client := mcp.NewClient(&mcp.Implementation{Name: "reconnect-test", Version: "1.0"}, nil)
	session, err := client.Connect(ctx, &mcp.IOTransport{Reader: p.out, Writer: p.in}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = session.Close() })
	return session
}

func TestServeReconnectsAfterIPCLeaderCrash(t *testing.T) {
	root := t.TempDir()
	cfgPath := filepath.Join(root, "config.json")
	repoDir := filepath.Join(root, "repo")
	if err := os.Mkdir(repoDir, 0700); err != nil {
		t.Fatal(err)
	}
	cfg, err := json.Marshal(model.Config{
		StateDir: filepath.Join(root, "state"),
		Repositories: map[string]model.Repo{
			"demo": {Root: repoDir, AllowedPaths: []string{"."}, Providers: []string{"fixture-a"}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfgPath, cfg, 0600); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	call := func(session *mcp.ClientSession, name string, args map[string]any) map[string]any {
		t.Helper()
		result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
		if err != nil || result == nil || result.IsError {
			t.Fatalf("%s returned %+v, %v", name, result, err)
		}
		return result.StructuredContent.(map[string]any)
	}
	leader := startReconnectTestProcess(t, ctx, cfgPath)
	leader.connect(t, ctx)
	proxy := startReconnectTestProcess(t, ctx, cfgPath)
	job := call(proxy.connect(t, ctx), "spawn_subagent", map[string]any{
		"repo_id": "demo", "task": "Inspect fixture", "execution_mode": "async",
	})
	if err := leader.cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	<-leader.done

	select {
	case <-proxy.done:
		if proxy.err == nil || !strings.Contains(proxy.stderr.String(), "Reconnect to start a new MCP session") {
			t.Fatalf("proxy did not report the reconnect boundary: %v, %q", proxy.err, proxy.stderr.String())
		}
	case <-time.After(4 * time.Second):
		t.Fatal("proxy kept a disconnected MCP session open")
	}
	replacement := startReconnectTestProcess(t, ctx, cfgPath).connect(t, ctx)
	if status := call(replacement, "get_job_status", map[string]any{"job_id": job["job_id"]}); status["job_id"] != job["job_id"] {
		t.Fatalf("new session did not recover the persisted job: %+v", status)
	}
}

func TestServeLeaderCrashRetainsRequestIdentity(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	repo := filepath.Join(root, "repo")
	if err := os.Mkdir(repo, 0700); err != nil {
		t.Fatal(err)
	}
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("/usr/bin/git", append([]string{"-C", repo, "-c", "user.name=Ahenk Fixture", "-c", "user.email=fixture@example.invalid",
			"-c", "commit.gpgsign=false", "-c", "core.hooksPath=/dev/null"}, args...)...)
		cmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=/var/empty", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null"}
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("fixture git: %v %s", err, out)
		}
	}
	git("init", "-b", "main")
	if err := os.WriteFile(filepath.Join(repo, "fixture.txt"), []byte("fixture\n"), 0600); err != nil {
		t.Fatal(err)
	}
	git("add", "fixture.txt")
	git("commit", "-m", "synthetic fixture")

	const repoID = "demo"
	const requestID = "lost-ack"
	const objective = "Inspect the synthetic fixture"
	stateDir := filepath.Join(root, "state")
	cfgPath := filepath.Join(root, "config.json")
	cfg, err := json.Marshal(model.Config{
		StateDir: stateDir, EnableFixture: true,
		Repositories: map[string]model.Repo{repoID: {Root: repo, AllowedPaths: []string{"fixture.txt"}, Providers: []string{"fixture-a"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfgPath, cfg, 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	leader := startReconnectTestProcess(t, ctx, cfgPath)
	leader.connect(t, ctx)
	proxy := startReconnectTestProcess(t, ctx, cfgPath)
	enc := json.NewEncoder(proxy.in)
	send := func(frame map[string]any) {
		t.Helper()
		frame["jsonrpc"] = "2.0"
		if err := enc.Encode(frame); err != nil {
			t.Fatal(err)
		}
	}
	send(map[string]any{"id": 1, "method": "initialize", "params": mcp.InitializeParams{
		ProtocolVersion: "2025-06-18", ClientInfo: &mcp.Implementation{Name: "lost-ack-test", Version: "1.0"},
	}})
	var init struct {
		ID     int
		Result mcp.InitializeResult
	}
	if err := json.NewDecoder(proxy.out).Decode(&init); err != nil {
		t.Fatalf("proxy initialize: %v", err)
	}
	if init.ID != 1 || init.Result.ServerInfo == nil || init.Result.ServerInfo.Name != "ahenk" {
		t.Fatalf("proxy initialize returned %+v", init)
	}
	send(map[string]any{"method": "notifications/initialized"})
	params := &mcp.CallToolParams{Name: "delegate_task", Arguments: map[string]any{
		"repo_id": repoID, "request_id": requestID, "task": objective, "execution_mode": "async", "mode": "read_only",
	}}
	// Leave the response stream unread after initialize. The database, rather
	// than the job ACK, tells the test when it can kill the leader.
	send(map[string]any{"id": 2, "method": "tools/call", "params": params})

	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	var accepted model.Job
	for accepted.ID == "" {
		_, jobs, err := store.ReadEvidence(ctx, stateDir)
		if err != nil {
			t.Fatalf("read durable admission: %v", err)
		}
		if len(jobs) > 0 {
			if len(jobs) != 1 || jobs[0].RepoID != repoID || jobs[0].RequestID != requestID || jobs[0].Task.Objective != objective {
				t.Fatalf("unexpected durable admission: %+v", jobs)
			}
			accepted = jobs[0].Job
			break
		}
		select {
		case <-proxy.done:
			t.Fatalf("proxy exited before durable admission: %v, %q", proxy.err, proxy.stderr.String())
		case <-ctx.Done():
			t.Fatalf("waiting for durable admission: %v", ctx.Err())
		case <-ticker.C:
		}
	}
	if err := leader.cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	<-leader.done
	select {
	case <-proxy.done:
		if proxy.err == nil || !strings.Contains(proxy.stderr.String(), "Reconnect to start a new MCP session") {
			t.Fatalf("proxy did not report the reconnect boundary: %v, %q", proxy.err, proxy.stderr.String())
		}
	case <-ctx.Done():
		t.Fatal("old proxy kept the disconnected session open")
	}
	_ = proxy.out.Close()

	newSession := startReconnectTestProcess(t, ctx, cfgPath).connect(t, ctx)
	for _, call := range []*mcp.CallToolParams{
		{Name: "get_job_status", Arguments: map[string]string{"job_id": accepted.ID}},
		params,
	} {
		res, err := newSession.CallTool(ctx, call)
		if err != nil || res == nil || res.IsError {
			t.Fatalf("%s after restart returned %+v, %v", call.Name, res, err)
		}
		out, ok := res.StructuredContent.(map[string]any)
		if !ok || out["job_id"] != accepted.ID || out["repo_id"] != repoID {
			t.Fatalf("%s after restart did not return the accepted job %q: %+v", call.Name, accepted.ID, res)
		}
	}
	_, jobs, err := store.ReadEvidence(ctx, stateDir)
	if err != nil || len(jobs) != 1 || jobs[0].ID != accepted.ID || jobs[0].RequestID != requestID {
		t.Fatalf("retry did not preserve one durable job %q: %+v, %v", accepted.ID, jobs, err)
	}
}

func TestLocalTestTimeoutPolicy(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "swift.json")
	if err := os.WriteFile(cfgPath, []byte(`{}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := localTest([]string{"--config", cfgPath, "--timeout", "18m"}); err == nil || !strings.Contains(err.Error(), "canonical Git root") {
		t.Fatalf("18-minute request should reach Swift config validation, got %v", err)
	}
	if err := localTest([]string{"--config", cfgPath, "--timeout", "18m1s"}); err == nil || !strings.Contains(err.Error(), "18 minutes") {
		t.Fatalf("expected 18-minute timeout limit in the error, got %v", err)
	}
}

func TestUsageListsPublicCommands(t *testing.T) {
	err := run(nil)
	if err == nil {
		t.Fatal("expected usage for an empty command line")
	}
	for _, command := range []string{"serve", "readiness", "m1-request", "verify-m1", "local-test", "private-brief-digest",
		"audit-models", "probe-agy", "probe-codexbar", "probe-opencode-catalog", "import-opencode-cooldown", "decode-quota", "version"} {
		if !strings.Contains(err.Error(), "ahenk "+command) {
			t.Errorf("usage omits %q: %v", command, err)
		}
	}
}

func TestProbeAGYChecksEveryConfiguredEntry(t *testing.T) {
	state := t.TempDir()
	cfgPath := filepath.Join(state, "config.json")
	b, err := json.Marshal(model.Config{StateDir: state, Providers: []model.ProviderConfig{
		{ID: "agy", Model: "gemini-3.8-flash-medium"},
		{ID: "opencode"},
		{ID: "agy"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfgPath, b, 0600); err != nil {
		t.Fatal(err)
	}
	err = run([]string{"probe-agy", "--config", cfgPath})
	if err == nil || !strings.Contains(err.Error(), "supported pinned distribution") || !strings.Contains(err.Error(), "requires an exact model") {
		t.Fatalf("expected validation errors from both AGY entries, got %v", err)
	}
}

func TestPrivateBriefDigestValidation(t *testing.T) {
	runDigest := func(payload string) error {
		r, w, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.WriteString(payload); err != nil {
			t.Fatal(err)
		}
		_ = w.Close()
		origStdin := os.Stdin
		os.Stdin = r
		defer func() {
			os.Stdin = origStdin
			_ = r.Close()
		}()
		return privateBriefDigest(nil)
	}

	if err := runDigest(`{"objective":"Valid scoped summary","acceptance":[{"id":"ok","description":"Give one fact"}]}`); err != nil {
		t.Fatalf("expected valid brief to succeed, got: %v", err)
	}
	for _, tc := range []struct {
		name    string
		payload string
	}{
		{"secret in objective", `{"objective":"Use sk-proj-1234567890abcdefghij","acceptance":[{"id":"ok","description":"fact"}]}`},
		{"personal data in objective", `{"objective":"Email user@gmail.com","acceptance":[{"id":"ok","description":"fact"}]}`},
		{"secret in acceptance", `{"objective":"Valid summary","acceptance":[{"id":"ok","description":"Use sk-proj-1234567890abcdefghij"}]}`},
		{"personal data in acceptance", `{"objective":"Valid summary","acceptance":[{"id":"ok","description":"Mail user@yarinlik.com"}]}`},
		{"invalid acceptance id", `{"objective":"Valid summary","acceptance":[{"id":"bad id!","description":"fact"}]}`},
		{"duplicate acceptance id", `{"objective":"Valid summary","acceptance":[{"id":"dup","description":"one"},{"id":"dup","description":"two"}]}`},
		{"invalid check_id", `{"objective":"Valid summary","acceptance":[{"id":"ok","description":"fact","check_id":"bad id!"}]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := runDigest(tc.payload); err == nil {
				t.Fatalf("expected invalid brief %q to be rejected", tc.name)
			}
		})
	}
}
