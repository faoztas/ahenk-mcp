// Copyright 2026 Furkan Anıl Öztaş
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/faoztas/ahenk-mcp/internal/model"
	"github.com/faoztas/ahenk-mcp/internal/store"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func routingTestConfig(t *testing.T) (model.Config, string) {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cfg := model.Config{
		StateDir: filepath.Join(root, "state"),
		Repositories: map[string]model.Repo{
			"demo":  {Root: filepath.Join(root, "repo"), AllowedPaths: []string{"."}, DefaultRoutingProfile: "price_performance"},
			"other": {Root: filepath.Join(root, "other"), AllowedPaths: []string{"."}},
		},
	}
	for _, repo := range cfg.Repositories {
		if err := os.Mkdir(repo.Root, 0700); err != nil {
			t.Fatal(err)
		}
	}
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(root, "config.json")
	if err := os.WriteFile(configPath, data, 0600); err != nil {
		t.Fatal(err)
	}
	return cfg, configPath
}

func runRoutingTestCLI(t *testing.T, args ...string) ([]byte, error) {
	t.Helper()
	out, err := os.CreateTemp(t.TempDir(), "stdout")
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	stdout := os.Stdout
	defer func() { os.Stdout = stdout }()
	os.Stdout = out
	runErr := run(append([]string{"routing"}, args...))
	if _, err := out.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	result, err := io.ReadAll(out)
	if err != nil {
		t.Fatal(err)
	}
	return result, runErr
}

func checkRoutingTestCLI(t *testing.T, configPath, repoID, command, argument, profile, source string) {
	t.Helper()
	args := []string{command, "--config", configPath, "--repo", repoID}
	if argument != "" {
		args = append(args, argument)
	}
	output, err := runRoutingTestCLI(t, args...)
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		RepoID  string `json:"repo_id"`
		Profile string `json:"routing_profile"`
		Source  string `json:"routing_profile_source"`
	}
	if err := json.Unmarshal(output, &got); err != nil {
		t.Fatalf("invalid routing output %q: %v", output, err)
	}
	if got.RepoID != repoID || got.Profile != profile || got.Source != source {
		t.Fatalf("routing %s %s returned %+v, want %q from %q", command, repoID, got, profile, source)
	}
}

func TestRoutingCLISetGetReset(t *testing.T) {
	cfg, configPath := routingTestConfig(t)
	db, err := store.Open(cfg.StateDir)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	// Hold scheduler ownership while exercising the independent operator command.
	files, err := filepath.Glob(filepath.Join(cfg.StateDir, "state.sqlite*"))
	if err != nil || len(files) == 0 {
		t.Fatal("missing SQLite fixture")
	}
	files = append(files, configPath, filepath.Join(cfg.StateDir, "owner.lock"))
	before := make(map[string][]byte, len(files))
	for _, path := range files {
		before[path], err = os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
	}
	checkRoutingTestCLI(t, configPath, "demo", "get", "", "price_performance", "repo_config")
	for _, profile := range []string{"price_performance", "quality", "price"} {
		checkRoutingTestCLI(t, configPath, "demo", "set", profile, profile, "operator")
		checkRoutingTestCLI(t, configPath, "demo", "get", "", profile, "operator")
	}
	checkRoutingTestCLI(t, configPath, "other", "get", "", "", "legacy")

	for _, args := range [][]string{
		nil,
		{"list", "--config", configPath, "--repo", "demo"},
		{"get", "--config", configPath},
		{"get", "--repo", "demo"},
		{"get", "--config", configPath, "--repo", "unknown"},
		{"set", "--config", configPath, "--repo", "unknown", "quality"},
		{"set", "--config", configPath, "--repo", "demo"},
		{"set", "--config", configPath, "--repo", "demo", "fast"},
		{"set", "--config", configPath, "--repo", "demo", "quality", "price"},
		{"reset", "--config", configPath, "--repo", "demo", "quality"},
	} {
		if out, err := runRoutingTestCLI(t, args...); err == nil || len(out) != 0 {
			t.Errorf("routing %q accepted invalid arguments: %q, %v", args, out, err)
		}
	}
	checkRoutingTestCLI(t, configPath, "demo", "get", "", "price", "operator")
	checkRoutingTestCLI(t, configPath, "demo", "reset", "", "price_performance", "repo_config")
	checkRoutingTestCLI(t, configPath, "demo", "get", "", "price_performance", "repo_config")
	checkRoutingTestCLI(t, configPath, "other", "reset", "", "", "legacy")

	for _, path := range files {
		after, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(before[path], after) {
			t.Errorf("routing command changed %s", filepath.Base(path))
		}
	}
}

func TestRoutingCLIRejectsAliasedState(t *testing.T) {
	for _, target := range []string{"demo", "other"} {
		for _, cmd := range []string{"get", "set", "reset"} {
			t.Run(target+"/"+cmd, func(t *testing.T) {
				cfg, configPath := routingTestConfig(t)
				link := filepath.Join(filepath.Dir(configPath), "alias")
				if err := os.Symlink(cfg.Repositories[target].Root, link); err != nil {
					t.Fatal(err)
				}
				cfg.StateDir = filepath.Join(link, "missing", "state")
				data, err := json.Marshal(cfg)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(configPath, data, 0600); err != nil {
					t.Fatal(err)
				}
				args := []string{cmd, "--config", configPath, "--repo", "demo"}
				if cmd == "set" {
					args = append(args, "quality")
				}
				out, err := runRoutingTestCLI(t, args...)
				if err == nil || len(out) != 0 {
					t.Error("accepted repository-local state")
				}
				if _, err := os.Lstat(filepath.Join(link, "missing")); !errors.Is(err, os.ErrNotExist) {
					t.Error("created repository-local state")
				}
			})
		}
	}
}

func TestRoutingCLIWithoutScheduler(t *testing.T) {
	cfg, configPath := routingTestConfig(t)
	checkRoutingTestCLI(t, configPath, "demo", "get", "", "price_performance", "repo_config")
	checkRoutingTestCLI(t, configPath, "demo", "reset", "", "price_performance", "repo_config")
	if _, err := os.Lstat(cfg.StateDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("read or reset created state")
	}
	checkRoutingTestCLI(t, configPath, "demo", "set", "price", "price", "operator")
	files, err := os.ReadDir(cfg.StateDir)
	if err != nil || len(files) != 1 || !strings.HasPrefix(files[0].Name(), "routing-preference-") {
		t.Fatal("expected only the preference record in fresh state")
	}
}

func TestRoutingCLIBoundedDiagnostics(t *testing.T) {
	_, configPath := routingTestConfig(t)
	sentinel := "fixture-secret-" + strings.Repeat("x", 512) + "\x1b[31m"
	for _, args := range [][]string{
		{"get", "--config", configPath, "--repo", sentinel},
		{"get", "--" + sentinel},
		{"get", "--config", filepath.Join(t.TempDir(), sentinel), "--repo", "demo"},
		{"set", "--config", configPath, "--repo", "demo", sentinel},
	} {
		cmd := exec.Command(os.Args[0], append([]string{"routing"}, args...)...)
		cmd.Env = []string{"GORACE=atexit_sleep_ms=0"}
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		out, err := cmd.Output()
		if err == nil || len(out) != 0 || stderr.Len() == 0 || stderr.Len() > 160 ||
			strings.ContainsAny(stderr.String(), "\x1b") || strings.Contains(stderr.String(), "fixture-secret") {
			t.Errorf("unsafe diagnostic (exit %v, stdout %d bytes, stderr %d bytes)", err, len(out), stderr.Len())
		}
	}
}

func TestRoutingCLIInvalidPreferenceFallsBack(t *testing.T) {
	cfg, configPath := routingTestConfig(t)
	checkRoutingTestCLI(t, configPath, "demo", "set", "quality", "quality", "operator")
	if err := os.Chmod(cfg.StateDir, 0755); err != nil {
		t.Fatal(err)
	}
	checkRoutingTestCLI(t, configPath, "demo", "get", "", "price_performance", "repo_config")
	out, err := runRoutingTestCLI(t, "reset", "--config", configPath, "--repo", "demo")
	if !errors.Is(err, store.ErrInvalidRoutingPreference) || len(out) != 0 {
		t.Fatalf("reset with unsafe state returned %q, %v", out, err)
	}
	if err := os.Chmod(cfg.StateDir, 0700); err != nil {
		t.Fatal(err)
	}
	checkRoutingTestCLI(t, configPath, "demo", "get", "", "quality", "operator")
}

func TestOperatorRoutingPreferenceUpdatesRunningMCPClients(t *testing.T) {
	cfg, configPath := routingTestConfig(t)
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	logFile, err := os.CreateTemp(t.TempDir(), "scheduler-log")
	if err != nil {
		t.Fatal(err)
	}
	defer logFile.Close()
	connect := func(name string) *mcp.ClientSession {
		t.Helper()
		cmd := exec.CommandContext(ctx, os.Args[0], "serve", "--stdio", "--config", configPath)
		cmd.Env = []string{"PATH=/usr/bin:/bin", "GORACE=atexit_sleep_ms=0"}
		cmd.Stderr = logFile
		client := mcp.NewClient(&mcp.Implementation{Name: name, Version: "1.0"}, nil)
		session, err := client.Connect(ctx, &mcp.CommandTransport{Command: cmd}, nil)
		if err != nil {
			t.Fatalf("connect %s: %v", name, err)
		}
		t.Cleanup(func() { _ = session.Close() })
		return session
	}
	call := func(client *mcp.ClientSession, tool string, args any) map[string]any {
		t.Helper()
		result, err := client.CallTool(ctx, &mcp.CallToolParams{Name: tool, Arguments: args})
		if err != nil {
			t.Fatalf("%s: %v", tool, err)
		}
		out, ok := result.StructuredContent.(map[string]any)
		if result.IsError || !ok {
			t.Fatalf("%s returned %+v", tool, result)
		}
		return out
	}
	submit := func(client *mcp.ClientSession, request, profile string) string {
		t.Helper()
		args := map[string]any{
			"cwd": cfg.Repositories["demo"].Root, "task": "Inspect fixture",
			"request_id": request, "execution_mode": "async", "mode": "read_only",
		}
		if profile != "" {
			args["routing_profile"] = profile
		}
		out := call(client, "delegate_task", args)
		id, _ := out["job_id"].(string)
		if id == "" || out["repo_id"] != "demo" {
			t.Fatalf("unexpected submission: %+v", out)
		}
		return id
	}
	batch := func(client *mcp.ClientSession, request, profile string) (string, []string) {
		t.Helper()
		task := model.Task{
			Objective: "Inspect fixture", AllowedPaths: []string{"."}, Mode: "read_only", DataClass: "synthetic",
			Acceptance: []model.Check{{ID: "inspect", Description: "Report findings"}},
		}
		explicitItem := task
		explicitItem.RoutingProfile = "quality"
		args := model.Batch{
			RepoID: "demo", RequestID: request, RoutingProfile: model.RoutingProfile(profile),
			Items: []model.Item{
				{Key: "first", Task: task},
				{Key: "second", Task: explicitItem, DependsOn: []string{"first"}},
			},
		}
		out := call(client, "delegate_batch", args)
		id, _ := out["job_id"].(string)
		children, _ := out["children"].([]any)
		if id == "" || len(children) != 2 {
			t.Fatalf("unexpected batch: %+v", out)
		}
		return id, []string{children[0].(string), children[1].(string)}
	}
	stored := func(id string) model.Job {
		t.Helper()
		_, jobs, err := store.ReadEvidence(ctx, cfg.StateDir)
		if err != nil {
			t.Fatal(err)
		}
		for _, job := range jobs {
			if job.ID == id {
				return job.Job
			}
		}
		t.Fatalf("job %s was not persisted", id)
		return model.Job{}
	}
	checkStored := func(id, profile, source string) {
		t.Helper()
		job := stored(id)
		if string(job.RoutingProfile) != profile || job.RoutingProfileSource != source {
			t.Fatalf("job %s profile = %q from %q, want %q from %q", id, job.RoutingProfile, job.RoutingProfileSource, profile, source)
		}
	}
	checkWarning := func(id, want string) {
		t.Helper()
		if got := stored(id).RoutingPreferenceWarning; got != want {
			t.Errorf("job %s warning = %q, want %q", id, got, want)
		}
	}

	leader := connect("routing-leader")
	proxy := connect("routing-proxy")
	old := submit(leader, "before", "")
	oldBatch, oldChildren := batch(proxy, "before-batch", "")
	checkStored(old, "price_performance", "repo_config")

	// Bootstrap the record directly to exercise engine adoption independently of CLI registration.
	preference := "quality"
	if err := store.SaveRoutingPreference(cfg.StateDir, "demo", cfg.Repositories["demo"].Root, &preference); err != nil {
		t.Fatal(err)
	}
	checkStored(submit(leader, "after-leader", ""), "quality", "operator")
	checkStored(submit(proxy, "after-proxy", ""), "quality", "operator")

	checkRoutingTestCLI(t, configPath, "demo", "set", "price", "price", "operator")
	checkStored(submit(leader, "cli-leader", ""), "price", "operator")
	checkStored(submit(proxy, "cli-proxy", ""), "price", "operator")
	checkStored(submit(proxy, "explicit", "quality"), "quality", "request")
	checkRoutingTestCLI(t, configPath, "demo", "get", "", "price", "operator")

	if retry := submit(proxy, "before", ""); retry != old {
		t.Fatalf("retry returned %s, want %s", retry, old)
	}
	checkStored(old, "price_performance", "repo_config")
	if retry, _ := batch(leader, "before-batch", ""); retry != oldBatch {
		t.Fatalf("batch retry returned %s, want %s", retry, oldBatch)
	}
	checkStored(oldBatch, "price_performance", "repo_config")
	checkStored(oldChildren[0], "price_performance", "repo_config")
	checkStored(oldChildren[1], "quality", "request")

	operatorBatch, operatorChildren := batch(proxy, "operator-batch", "")
	checkStored(operatorBatch, "price", "operator")
	checkStored(operatorChildren[0], "price", "operator")
	checkStored(operatorChildren[1], "quality", "request")
	explicitBatch, explicitChildren := batch(proxy, "explicit-batch", "price_performance")
	checkStored(explicitBatch, "price_performance", "batch")
	checkStored(explicitChildren[0], "price_performance", "batch")
	checkStored(explicitChildren[1], "quality", "request")

	records, err := filepath.Glob(filepath.Join(cfg.StateDir, "routing-preference-*"))
	if err != nil || len(records) != 1 {
		t.Fatal("expected one preference record")
	}
	if err := os.WriteFile(records[0], []byte("malformed"), 0600); err != nil {
		t.Fatal(err)
	}

	warned := submit(proxy, "invalid-record", "")
	warnedBatch, invalidChildren := batch(leader, "invalid-record-batch", "")
	const invalid = "INVALID_ROUTING_PREFERENCE"
	checkWarning(warned, invalid)
	checkWarning(warnedBatch, invalid)
	checkStored(warned, "price_performance", "repo_config")
	checkStored(invalidChildren[0], "price_performance", "repo_config")
	checkStored(invalidChildren[1], "quality", "request")
	checkWarning(submit(leader, "cli-leader", ""), "")
	replayedBatch, _ := batch(proxy, "operator-batch", "")
	checkWarning(replayedBatch, "")
	checkStored(replayedBatch, "price", "operator")
	checkWarning(submit(proxy, "explicit-invalid-record", "price"), "")
	override, _ := batch(leader, "explicit-invalid-record-batch", "price")
	checkWarning(override, "")
	checkRoutingTestCLI(t, configPath, "demo", "set", "price", "price", "operator")

	for _, session := range []*mcp.ClientSession{proxy, leader} {
		if err := session.Close(); err != nil {
			t.Fatalf("close session: %v", err)
		}
	}
	logs, err := os.ReadFile(logFile.Name())
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(logs, []byte("routing preference")) {
		t.Error("admission or replay logged a mutable preference")
	}
	restarted := connect("routing-restarted")
	if submit(restarted, "invalid-record", "") != warned {
		t.Fatal("replaced warned submission")
	}
	retryBatch, _ := batch(restarted, "invalid-record-batch", "")
	if retryBatch != warnedBatch {
		t.Fatal("replaced warned batch")
	}
	checkWarning(warned, invalid)
	checkWarning(warnedBatch, invalid)
	checkStored(warned, "price_performance", "repo_config")
	checkStored(warnedBatch, "price_performance", "repo_config")
	checkStored(submit(restarted, "after-restart", ""), "price", "operator")
	if retry := submit(restarted, "before", ""); retry != old {
		t.Fatalf("retry after restart returned %s, want %s", retry, old)
	}
	checkStored(old, "price_performance", "repo_config")

	checkRoutingTestCLI(t, configPath, "demo", "reset", "", "price_performance", "repo_config")
	checkStored(submit(restarted, "after-reset", ""), "price_performance", "repo_config")
}

func TestRoutingCLIUsage(t *testing.T) {
	if err := run(nil); err == nil || !strings.Contains(err.Error(), "ahenk routing {get,set,reset}") {
		t.Fatalf("routing usage missing: %v", err)
	}
}
