// Copyright 2026 Furkan Anıl Öztaş
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"debug/macho"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/faoztas/ahenk-mcp/internal/model"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const intelPackage = "ahenk-darwin-amd64"

func TestIntelPackageMCPContract(t *testing.T) {
	source, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	t.Run("invalid_layout_or_binary", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), intelPackage)
		if err := os.MkdirAll(filepath.Join(dir, "licenses", "go"), 0700); err != nil {
			t.Fatal(err)
		}
		binary := filepath.Join(dir, "ahenk")
		writePackageTestFile(t, binary, []byte("#!/bin/sh\necho wrong-binary\n"), 0700)
		assertRejected := func(want string) {
			t.Helper()
			err := verifyIntelPackage(t.Context(), binary, source)
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("expected %q error, got %v", want, err)
			}
		}
		assertRejected("LICENSE")
		for name, src := range map[string]string{"LICENSE": "LICENSE", "THIRD_PARTY.md": "docs/third-party-licenses.md"} {
			data, err := os.ReadFile(filepath.Join(source, src))
			if err != nil {
				t.Fatal(err)
			}
			writePackageTestFile(t, filepath.Join(dir, name), data, 0600)
		}
		writePackageTestFile(t, filepath.Join(dir, "licenses", "go", "LICENSE"), []byte("synthetic Go license"), 0600)
		assertRejected("--version")
	})
	// CI supplies AHENK_PACKAGE_BINARY from the verified Intel archive.
	t.Run("extracted_archive", func(t *testing.T) {
		binary := os.Getenv("AHENK_PACKAGE_BINARY")
		e := packagedIPCEvidence{
			SourceSHA:  os.Getenv("AHENK_PACKAGE_SOURCE_SHA"),
			ArchiveSHA: [2]string{os.Getenv("AHENK_PACKAGE_SHA_FIRST"), os.Getenv("AHENK_PACKAGE_SHA_SECOND")},
		}
		if binary == "" && e.SourceSHA == "" && e.ArchiveSHA == [2]string{} {
			t.Skip("Intel archives are supplied by the package build")
		}
		ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
		defer cancel()
		checkout := packageGit(t, ctx, source, "rev-parse", "HEAD")
		if err := e.verifyProvenance(checkout); err != nil {
			t.Fatal(err)
		}
		if err := verifyIntelPackage(ctx, binary, source); err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(binary)
		if err != nil {
			t.Fatal(err)
		}
		e.BinarySHA = model.Hash(data)
		runPackagedIPC(t, ctx, binary, &e)
		if err := verifyPackagedIPCEvidence(e, checkout); err != nil {
			t.Fatal(err)
		}
		t.Logf("G-031 Intel package with synthetic jobs: source=%s archive_sha256=%s,%s binary_sha256=%s clients=%v replacement=%d job=%s artifact_sha256=%s dynamic_repo=%s reaped=%t/%t/%t",
			e.SourceSHA, e.ArchiveSHA[0], e.ArchiveSHA[1], e.BinarySHA, e.Clients, e.Replacement, e.After.ID, e.After.Digest, e.DynamicRepo,
			e.LeaderReaped, e.ProxyReaped, e.ReplacementReaped)
	})
}

func writePackageTestFile(t *testing.T, path string, data []byte, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, data, mode); err != nil {
		t.Fatal(err)
	}
}

type packageJobTrace struct {
	ID, Digest string
}

type packagedIPCEvidence struct {
	SourceSHA, BinarySHA                         string
	ArchiveSHA                                   [2]string
	Clients                                      [2]int
	Replacement                                  int
	Before, After                                packageJobTrace
	LeaderReaped, ProxyReaped, ReplacementReaped bool
	DynamicRepo                                  string
}

func TestPackagedIPCEvidenceRequiresRestartAndTwoClients(t *testing.T) {
	const checkout = "0123456789abcdef0123456789abcdef01234567"
	hash := strings.Repeat("a", 64)
	good := packagedIPCEvidence{
		SourceSHA: checkout, BinarySHA: hash, ArchiveSHA: [2]string{hash, hash},
		Clients: [2]int{101, 102}, Replacement: 103,
		Before: packageJobTrace{"job-1", hash}, After: packageJobTrace{"job-1", hash},
		LeaderReaped: true, ProxyReaped: true, ReplacementReaped: true,
		DynamicRepo: "local-dynamic-01234567",
	}
	if err := verifyPackagedIPCEvidence(good, checkout); err != nil {
		t.Fatalf("complete synthetic evidence: %v", err)
	}
	for _, tc := range []struct {
		want string
		edit func(*packagedIPCEvidence)
	}{
		{"source SHA", func(e *packagedIPCEvidence) { e.SourceSHA = strings.Repeat("b", 40) }},
		{"binary SHA", func(e *packagedIPCEvidence) { e.BinarySHA = "" }},
		{"archive SHA", func(e *packagedIPCEvidence) { e.ArchiveSHA[1] = "" }},
		{"archive hashes differ", func(e *packagedIPCEvidence) { e.ArchiveSHA[1] = strings.Repeat("b", 64) }},
		{"two clients", func(e *packagedIPCEvidence) { e.Clients[1] = 0 }},
		{"two clients", func(e *packagedIPCEvidence) { e.Clients[1] = e.Clients[0] }},
		{"fresh initialize", func(e *packagedIPCEvidence) { e.Replacement = 0 }},
		{"reaped", func(e *packagedIPCEvidence) { e.ProxyReaped = false }},
		{"job after restart", func(e *packagedIPCEvidence) { e.After.ID = "" }},
		{"job after restart", func(e *packagedIPCEvidence) { e.After.Digest = strings.Repeat("b", 64) }},
		{"repository policy", func(e *packagedIPCEvidence) { e.DynamicRepo = "" }},
	} {
		e := good
		tc.edit(&e)
		if err := verifyPackagedIPCEvidence(e, checkout); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("expected %q rejection, got %v", tc.want, err)
		}
	}
}

func verifyIntelPackage(ctx context.Context, binary, source string) error {
	dir := filepath.Dir(binary)
	if !filepath.IsAbs(binary) || filepath.Base(dir) != intelPackage || filepath.Base(binary) != "ahenk" {
		return fmt.Errorf("expected an absolute extracted %s/ahenk path", intelPackage)
	}
	info, err := os.Lstat(binary)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0111 == 0 {
		return fmt.Errorf("ahenk must be a regular executable: %v", err)
	}
	for _, file := range []struct{ name, source string }{
		{"LICENSE", "LICENSE"},
		{"THIRD_PARTY.md", "docs/third-party-licenses.md"},
	} {
		want, err := os.ReadFile(filepath.Join(source, file.source))
		if err != nil {
			return err
		}
		got, err := os.ReadFile(filepath.Join(dir, file.name))
		if err != nil || len(got) == 0 || !bytes.Equal(got, want) {
			return fmt.Errorf("%s must match the checkout: %v", file.name, err)
		}
	}
	if goLicense, err := os.ReadFile(filepath.Join(dir, "licenses", "go", "LICENSE")); err != nil || len(goLicense) == 0 {
		return fmt.Errorf("missing Go LICENSE: %v", err)
	}
	cmd := exec.CommandContext(ctx, binary, "--version")
	cmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=/var/empty", "LC_ALL=C"}
	out, err := cmd.CombinedOutput()
	wantVersion := "ahenk " + model.Version + " darwin/amd64"
	if err != nil || strings.TrimSpace(string(out)) != wantVersion {
		return fmt.Errorf("ahenk --version: got %q, %v, want %q", out, err, wantVersion)
	}
	executable, err := macho.Open(binary)
	if err != nil {
		return fmt.Errorf("open Intel Mach-O: %w", err)
	}
	defer executable.Close()
	if executable.Cpu != macho.CpuAmd64 || executable.Type != macho.TypeExec {
		return fmt.Errorf("expected an Intel Mach-O executable, got %v %v", executable.Cpu, executable.Type)
	}
	return nil
}

func packageGit(t *testing.T, ctx context.Context, dir string, args ...string) string {
	t.Helper()
	cmd := exec.CommandContext(ctx, "/usr/bin/git", append([]string{"-C", dir, "-c", "user.name=Ahenk Fixture",
		"-c", "user.email=fixture@example.invalid", "-c", "commit.gpgsign=false", "-c", "core.hooksPath=/dev/null"}, args...)...)
	cmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=/var/empty", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null"}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func runPackagedIPC(t *testing.T, ctx context.Context, binary string, e *packagedIPCEvidence) {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	repo, dynamic := filepath.Join(root, "configured"), filepath.Join(root, "dynamic")
	for _, dir := range []string{repo, dynamic} {
		if err := os.Mkdir(dir, 0700); err != nil {
			t.Fatal(err)
		}
		packageGit(t, ctx, dir, "init", "-b", "main")
		writePackageTestFile(t, filepath.Join(dir, "fixture.txt"), []byte("before\n"), 0600)
		packageGit(t, ctx, dir, "add", "fixture.txt")
		packageGit(t, ctx, dir, "commit", "-m", "synthetic package fixture")
	}
	const repoID = "demo"
	cfgPath := filepath.Join(root, "config.json")
	cfg, err := json.Marshal(model.Config{
		StateDir: filepath.Join(root, "state"), EnableFixture: true,
		Repositories: map[string]model.Repo{
			repoID: {Root: repo, AllowedPaths: []string{"fixture.txt"}, Providers: []string{"fixture-a"}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	writePackageTestFile(t, cfgPath, cfg, 0600)

	call := func(s *mcp.ClientSession, name string, args any, wantCode string) map[string]any {
		t.Helper()
		result, err := s.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
		if err != nil || result == nil {
			t.Fatalf("%s: %v", name, err)
		}
		out, ok := result.StructuredContent.(map[string]any)
		code, _ := out["code"].(string)
		if !ok || result.IsError != (wantCode != "") || code != wantCode {
			t.Fatalf("%s: wanted code %q, got %+v", name, wantCode, result)
		}
		return out
	}
	start := func() (*reconnectTestProcess, *mcp.ClientSession) {
		t.Helper()
		p := startReconnectTestBinary(t, ctx, binary, cfgPath)
		s := p.connect(t, ctx)
		init := s.InitializeResult()
		if init == nil || init.ServerInfo == nil || init.ServerInfo.Name != "ahenk" || init.ServerInfo.Version != model.Version {
			t.Fatalf("unexpected initialize result: %+v", init)
		}
		tools, err := s.ListTools(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		for _, name := range []string{"list_capabilities", "delegate_task", "wait_jobs", "get_job_status", "get_job_result"} {
			if !slices.ContainsFunc(tools.Tools, func(tool *mcp.Tool) bool { return tool.Name == name }) {
				t.Fatalf("packaged CLI discovery missing %s", name)
			}
		}
		caps := call(s, "list_capabilities", map[string]string{"repo_id": repoID}, "")
		admission, _ := caps["repo_admission"].(map[string]any)
		if caps["scheduler_status"] != "active" || caps["binary_sha256"] != e.BinarySHA || admission["registered"] != true {
			t.Fatalf("packaged CLI identity or configured repo admission: %+v", caps)
		}
		return p, s
	}
	result := func(s *mcp.ClientSession, jobID string) packageJobTrace {
		t.Helper()
		out := call(s, "get_job_result", map[string]string{"job_id": jobID, "view": "diff"}, "")
		diff, _ := out["diff"].(string)
		digest, _ := out["artifact_sha256"].(string)
		if out["job_id"] != jobID || out["repo_id"] != repoID || out["status"] != model.Completed ||
			out["source_drift"] != false || out["next_cursor"] != nil || !strings.Contains(diff, "-before\n+after\n") ||
			digest != model.Hash([]byte(diff)) {
			t.Fatalf("unexpected packaged job result: %+v", out)
		}
		return packageJobTrace{jobID, digest}
	}
	registerDynamic := func(s *mcp.ClientSession, request string) (string, string) {
		t.Helper()
		out := call(s, "delegate_task", map[string]any{
			"cwd": dynamic, "task": "Read the synthetic fixture", "mode": "read_only",
			"execution_mode": "wait", "request_id": request, "preferred_provider": "fixture-a",
		}, "")
		id, _ := out["job_id"].(string)
		repo, _ := out["repo_id"].(string)
		if out["status"] != model.Completed || !model.ValidID(id) || !strings.HasPrefix(repo, "local-dynamic-") {
			t.Fatalf("dynamic repo job failed: %+v", out)
		}
		return repo, id
	}
	waitExit := func(p *reconnectTestProcess, role string) {
		t.Helper()
		select {
		case <-p.done:
		case <-time.After(5 * time.Second):
			t.Fatalf("%s did not exit", role)
		}
	}

	leader, first := start()
	proxy, second := start()
	e.Clients = [2]int{leader.cmd.Process.Pid, proxy.cmd.Process.Pid}
	submitted := call(second, "delegate_task", model.Submission{
		RepoID: repoID, RequestID: "packaged-patch",
		Task: model.Task{Objective: "Patch the synthetic fixture", Mode: "patch", DataClass: "synthetic",
			PreferredProvider: "fixture-a", AllowedPaths: []string{"fixture.txt"},
			Acceptance: []model.Check{{ID: "patch", Description: "Produce a scoped patch"}}},
	}, "")
	id, _ := submitted["job_id"].(string)
	if !model.ValidID(id) {
		t.Fatalf("invalid submitted job: %+v", submitted)
	}
	call(first, "wait_jobs", map[string]any{"job_ids": []string{id}, "mode": "all", "timeout_seconds": 15}, "")
	e.Before = result(first, id)
	if got := result(second, id); got != e.Before {
		t.Fatalf("proxy job result: %+v, want %+v", got, e.Before)
	}
	dynRepo, dynJob := registerDynamic(second, "dynamic-before")
	dynArgs := map[string]string{"repo_id": dynRepo}
	call(first, "list_capabilities", dynArgs, "")

	if err := leader.cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	waitExit(leader, "crashed leader")
	if leader.cmd.ProcessState.ExitCode() != -1 {
		t.Fatalf("leader did not exit by signal: %v", leader.err)
	}
	e.LeaderReaped = true
	waitExit(proxy, "disconnected proxy")
	if proxy.cmd.ProcessState.ExitCode() != 1 || !strings.Contains(proxy.stderr.String(), "Reconnect to start a new MCP session") {
		t.Fatalf("proxy missed the reconnect boundary: %v, %q", proxy.err, proxy.stderr.String())
	}
	e.ProxyReaped = true

	replacement, fresh := start()
	e.Replacement = replacement.cmd.Process.Pid
	e.After = result(fresh, id)
	call(fresh, "list_capabilities", dynArgs, "REPO_NOT_REGISTERED")
	dynStatusArgs := map[string]string{"job_id": dynJob}
	call(fresh, "get_job_status", dynStatusArgs, "NOT_FOUND")
	if got, _ := registerDynamic(fresh, "dynamic-after"); got != dynRepo {
		t.Fatalf("dynamic repo changed identity: got %q want %q", got, dynRepo)
	}
	if got := call(fresh, "get_job_status", dynStatusArgs, ""); got["job_id"] != dynJob || got["repo_id"] != dynRepo {
		t.Fatalf("dynamic job missing after re-registration: %+v", got)
	}
	e.DynamicRepo = dynRepo
	if err := fresh.Close(); err != nil {
		t.Fatal(err)
	}
	waitExit(replacement, "replacement leader")
	if replacement.err != nil {
		t.Fatalf("replacement exit: %v, %q", replacement.err, replacement.stderr.String())
	}
	e.ReplacementReaped = true
}

func (e packagedIPCEvidence) verifyProvenance(checkout string) error {
	if !packageDigest(e.SourceSHA, 20) || e.SourceSHA != checkout {
		return fmt.Errorf("source SHA %q does not match checkout %q", e.SourceSHA, checkout)
	}
	if !packageDigest(e.ArchiveSHA[0], 32) || !packageDigest(e.ArchiveSHA[1], 32) {
		return fmt.Errorf("both archive SHA-256 values are required")
	}
	if e.ArchiveSHA[0] != e.ArchiveSHA[1] {
		return fmt.Errorf("archive hashes differ")
	}
	return nil
}

func verifyPackagedIPCEvidence(e packagedIPCEvidence, checkout string) error {
	switch {
	case !packageDigest(e.BinarySHA, 32):
		return fmt.Errorf("binary SHA-256 is required")
	case e.Clients[0] <= 0 || e.Clients[1] <= 0 || e.Clients[0] == e.Clients[1]:
		return fmt.Errorf("two clients must have distinct process traces")
	case e.Replacement <= 0:
		return fmt.Errorf("fresh initialize is required")
	case !e.LeaderReaped || !e.ProxyReaped || !e.ReplacementReaped:
		return fmt.Errorf("all three serve processes must be reaped")
	case !model.ValidID(e.Before.ID) || !packageDigest(e.Before.Digest, 32) || e.Before != e.After:
		return fmt.Errorf("job after restart must match the original job ID and artifact SHA-256")
	case !strings.HasPrefix(e.DynamicRepo, "local-dynamic-"):
		return fmt.Errorf("dynamic repository policy evidence is required")
	}
	return e.verifyProvenance(checkout)
}

func packageDigest(value string, size int) bool {
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == size && strings.ToLower(value) == value
}
