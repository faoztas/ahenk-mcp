// Copyright 2026 Furkan Anıl Öztaş
// SPDX-License-Identifier: Apache-2.0

package orchestration

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/faoztas/ahenk-mcp/internal/execution"
	"github.com/faoztas/ahenk-mcp/internal/model"
	"github.com/faoztas/ahenk-mcp/internal/providers"
	"github.com/faoztas/ahenk-mcp/internal/store"
)

func TestMain(m *testing.M) {
	if len(os.Args) > 1 && slices.Contains([]string{"_supervise_go_check", "_supervise_fixture", "_fixture_worker"}, os.Args[1]) {
		switch os.Args[1] {
		case "_supervise_go_check":
			if err := execution.SuperviseGoChecks(); err != nil {
				os.Exit(1)
			}
			os.Exit(0)
		case "_supervise_fixture":
			if err := execution.SuperviseFixture(); err != nil {
				os.Exit(1)
			}
			os.Exit(0)
		case "_fixture_worker":
			if err := execution.FixtureWorker(); err != nil {
				os.Exit(1)
			}
			os.Exit(0)
		}
	}
	os.Exit(m.Run())
}

func setupTestGitRepo(t *testing.T, dir string) {
	t.Helper()
	runGit := func(args ...string) {
		cmd := exec.Command("git", append([]string{"-C", dir, "-c", "core.hooksPath=/dev/null",
			"-c", "user.name=SubagentTest", "-c", "user.email=subagent@example.invalid", "-c", "commit.gpgsign=false"}, args...)...)
		cmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=/var/empty", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null"}
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v failed: %v\nOutput: %s", args, err, out)
		}
	}
	runGit("init")
	runGit("add", ".")
	runGit("commit", "-m", "Initial commit")
}

func canonicalTempDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("failed to evaluate symlinks for temp dir: %v", err)
	}
	return dir
}

// 1. Pre/Post Image SHA-256 Immutability Test
func TestWriterIsolationPrePostSha256Immutability(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	tempDir := canonicalTempDir(t)
	sourceRepo := filepath.Join(tempDir, "source")
	if err := os.MkdirAll(sourceRepo, 0700); err != nil {
		t.Fatal(err)
	}

	goMod := filepath.Join(sourceRepo, "go.mod")
	_ = os.WriteFile(goMod, []byte("module example.test/immut\n\ngo 1.24\n"), 0644)
	calcGo := filepath.Join(sourceRepo, "calc.go")
	originalCalc := "package immut\n\nfunc Add(a, b int) int { return a + b }\n"
	_ = os.WriteFile(calcGo, []byte(originalCalc), 0644)

	setupTestGitRepo(t, sourceRepo)

	// Worktree hazırlanırken TakeSnapshot ve VerifySnapshot ile doğrulanması
	s, err := execution.TakeSnapshot(ctx, sourceRepo, "snap-immut", "head_only", []string{"calc.go"})
	if err != nil {
		t.Fatalf("failed to take snapshot: %v", err)
	}

	ok, err := execution.VerifySnapshot(ctx, s)
	if err != nil || !ok {
		t.Fatalf("expected VerifySnapshot to be true before execution, got %v, err: %v", ok, err)
	}

	attemptDir := filepath.Join(tempDir, "attempt")
	if err := execution.NewAttemptDir(attemptDir); err != nil {
		t.Fatal(err)
	}

	task := model.Task{
		Objective:    "Optimize Add function",
		Mode:         "patch",
		AllowedPaths: []string{"calc.go"},
	}

	w, err := execution.PrepareWorktree(ctx, attemptDir, s, task)
	if err != nil {
		t.Fatalf("failed to prepare worktree: %v", err)
	}
	defer w.Close()

	// Subagent edits calc.go inside the isolated worktree
	workCalc := filepath.Join(w.Dir, "calc.go")
	modifiedCalc := "package immut\n\nfunc Add(a, b int) int {\n\t// optimized\n\treturn a + b\n}\n"
	if err := os.WriteFile(workCalc, []byte(modifiedCalc), 0644); err != nil {
		t.Fatal(err)
	}

	// Verify source repo working directory was completely untouched on disk
	sourceContent, err := os.ReadFile(calcGo)
	if err != nil || string(sourceContent) != originalCalc {
		t.Fatalf("source repo file was modified: got %s, expected %s", string(sourceContent), originalCalc)
	}

	scratch := filepath.Join(tempDir, "scratch")
	_ = os.MkdirAll(scratch, 0700)

	result, err := w.Collect(ctx, scratch)
	if err != nil {
		t.Fatalf("failed to collect worktree: %v", err)
	}

	// Kaynak deponun çalışma dizininde hiçbir dosyanın değişmediğinin (SourceUnchanged: true) kanıtlanması
	if !result.SourceUnchanged {
		t.Error("expected SourceUnchanged to be true")
	}

	// Worktree tamamlanırken VerifySnapshot ile doğrulanması
	ok, err = execution.VerifySnapshot(ctx, s)
	if err != nil || !ok {
		t.Fatalf("expected VerifySnapshot to remain true after execution, got %v, err: %v", ok, err)
	}

	if len(result.Patch) == 0 {
		t.Error("expected non-empty patch in result")
	}
	pre, hasPre := result.Preimages["calc.go"]
	post, hasPost := result.Postimages["calc.go"]
	if !hasPre || !hasPost {
		t.Fatalf("missing pre or post images: pre=%v, post=%v", hasPre, hasPost)
	}
	if pre == post {
		t.Errorf("expected different pre and post SHA-256 hashes, got pre=%s post=%s", pre, post)
	}
}

// 2. AllowedPaths Writer Isolation İhlali Testi
func TestWriterIsolationAllowedPathsViolation(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	tempDir := canonicalTempDir(t)
	sourceRepo := filepath.Join(tempDir, "source_viol")
	if err := os.MkdirAll(sourceRepo, 0700); err != nil {
		t.Fatal(err)
	}

	goMod := filepath.Join(sourceRepo, "go.mod")
	_ = os.WriteFile(goMod, []byte("module example.test/viol\n\ngo 1.24\n"), 0644)
	safeGo := filepath.Join(sourceRepo, "safe.go")
	_ = os.WriteFile(safeGo, []byte("package viol\nfunc Safe() {}\n"), 0644)

	setupTestGitRepo(t, sourceRepo)

	s, err := execution.TakeSnapshot(ctx, sourceRepo, "snap-viol", "head_only", []string{"safe.go"})
	if err != nil {
		t.Fatalf("failed to take snapshot: %v", err)
	}

	attemptDir := filepath.Join(tempDir, "attempt_viol")
	if err := execution.NewAttemptDir(attemptDir); err != nil {
		t.Fatal(err)
	}

	task := model.Task{
		Objective:    "Modify safe.go",
		Mode:         "patch",
		AllowedPaths: []string{"safe.go"},
	}

	w, err := execution.PrepareWorktree(ctx, attemptDir, s, task)
	if err != nil {
		t.Fatalf("failed to prepare worktree: %v", err)
	}
	defer w.Close()

	// Subagent creates or modifies a file outside AllowedPaths (protected file)
	unauthorized := filepath.Join(w.Dir, "tokens.go")
	if err := os.WriteFile(unauthorized, []byte("package viol\nvar Secret = 1\n"), 0644); err != nil {
		t.Fatal(err)
	}

	scratch := filepath.Join(tempDir, "scratch_viol")
	_ = os.MkdirAll(scratch, 0700)

	_, err = w.Collect(ctx, scratch)
	if err == nil {
		t.Fatal("expected policy denial for unauthorized file edit, got nil")
	}

	var fault *model.Fault
	if !errors.As(err, &fault) || fault.Code != "POLICY_DENIED" || !strings.Contains(fault.Message, "native edit changed a protected file") {
		t.Fatalf("expected POLICY_DENIED fault with 'native edit changed a protected file', got: %v", err)
	}
}

// 3. Go Checks & Diff Snapshot Entegrasyonu
func TestSubagentGoChecksAndDiffIntegration(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	tempDir := canonicalTempDir(t)
	sourceRepo := filepath.Join(tempDir, "source_checks")
	if err := os.MkdirAll(sourceRepo, 0700); err != nil {
		t.Fatal(err)
	}

	goMod := filepath.Join(sourceRepo, "go.mod")
	_ = os.WriteFile(goMod, []byte("module example.test/checks\n\ngo 1.24\n"), 0644)
	calcGo := filepath.Join(sourceRepo, "calc.go")
	_ = os.WriteFile(calcGo, []byte("package checks\n\nfunc Add(a, b int) int { return a + b }\n"), 0644)
	calcTest := filepath.Join(sourceRepo, "calc_test.go")
	_ = os.WriteFile(calcTest, []byte("package checks\n\nimport \"testing\"\n\nfunc TestAdd(t *testing.T) {\n\tif Add(2, 3) != 5 {\n\t\tt.Fail()\n\t}\n}\n"), 0644)

	setupTestGitRepo(t, sourceRepo)

	s, err := execution.TakeSnapshot(ctx, sourceRepo, "snap-checks", "head_only", []string{"calc.go", "calc_test.go", "go.mod"})
	if err != nil {
		t.Fatalf("failed to take snapshot: %v", err)
	}

	attemptDir := filepath.Join(tempDir, "attempt_checks")
	if err := execution.NewAttemptDir(attemptDir); err != nil {
		t.Fatal(err)
	}

	task := model.Task{
		Objective:    "Refactor Add function",
		Mode:         "patch",
		AllowedPaths: []string{"calc.go", "calc_test.go", "go.mod"},
		Acceptance: []model.Check{
			{ID: "check_add", Description: "Run TestAdd", CheckID: "TestAdd"},
		},
	}

	w, err := execution.PrepareWorktree(ctx, attemptDir, s, task)
	if err != nil {
		t.Fatalf("failed to prepare worktree: %v", err)
	}
	defer w.Close()

	// Subagent edits calc.go in worktree
	workCalc := filepath.Join(w.Dir, "calc.go")
	_ = os.WriteFile(workCalc, []byte("package checks\n\nfunc Add(a, b int) int {\n\t// verified\n\treturn a + b\n}\n"), 0644)

	tool, err := execution.GoCheckTool()
	if err != nil {
		t.Fatalf("go check tool unavailable: %v", err)
	}

	checks := []model.CheckEvidence{
		{ID: "check_add", Test: "TestAdd", Status: model.NotRun, ExitStatus: -1},
	}

	verifiedChecks, err := execution.RunGoChecks(ctx, attemptDir, w.Dir, tool, checks)
	if err != nil {
		t.Fatalf("RunGoChecks failed: %v", err)
	}
	if len(verifiedChecks) != 1 || verifiedChecks[0].Status != "PASSED" || verifiedChecks[0].ExitStatus != 0 {
		t.Fatalf("unexpected check evidence: %+v", verifiedChecks)
	}

	scratch := filepath.Join(tempDir, "scratch_checks")
	_ = os.MkdirAll(scratch, 0700)

	result, err := w.Collect(ctx, scratch)
	if err != nil {
		t.Fatalf("failed to collect worktree: %v", err)
	}
	if !result.SourceUnchanged {
		t.Error("expected SourceUnchanged to be true")
	}
	if len(result.Patch) == 0 {
		t.Error("expected non-empty unified patch")
	}
}

// 4. Uçtan Uca Live Subagent Verification Senaryosu
func TestSubagentE2ELiveVerification(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	tempDir := canonicalTempDir(t)
	sourceRepo := filepath.Join(tempDir, "source_e2e")
	if err := os.MkdirAll(sourceRepo, 0700); err != nil {
		t.Fatal(err)
	}

	goMod := filepath.Join(sourceRepo, "go.mod")
	_ = os.WriteFile(goMod, []byte("module example.test/e2e\n\ngo 1.24\n"), 0644)
	calcGo := filepath.Join(sourceRepo, "calc.go")
	originalCalc := "package e2e\n\nfunc Add(a, b int) int { return a + b }\n"
	_ = os.WriteFile(calcGo, []byte(originalCalc), 0644)
	calcTest := filepath.Join(sourceRepo, "calc_test.go")
	_ = os.WriteFile(calcTest, []byte("package e2e\n\nimport \"testing\"\n\nfunc TestAdd(t *testing.T) {\n\tif Add(2, 3) != 5 {\n\t\tt.Fail()\n\t}\n}\n"), 0644)

	setupTestGitRepo(t, sourceRepo)

	// Create mock Codex CLI binary that simulates autonomous subagent editing inside the worktree
	mockCodexPath := filepath.Join(tempDir, "mock_codex")
	mockScript := fmt.Sprintf(`#!/bin/sh
# Find workdir from -C argument if present
WORKDIR=""
prev=""
for arg in "$@"; do
  if [ "$prev" = "-C" ]; then
    WORKDIR="$arg"
    break
  fi
  prev="$arg"
done

if [ -n "$WORKDIR" ]; then
  TARGET="$WORKDIR/calc.go"
else
  TARGET="calc.go"
fi

if [ -f "$TARGET" ]; then
  cat << 'EOF' > "$TARGET"
package e2e

func Add(a, b int) int {
	// refactored by autonomous codex subagent
	return a + b
}
EOF
fi

cat << 'EOF'
{"type":"thread.started","thread_id":"sess-codex-e2e-subagent"}
{"type":"item.completed","item":{"type":"agent_message","text":"Refactored Add function with zero regressions"}}
{"type":"turn.completed","usage":{"input_tokens":120,"output_tokens":45}}
EOF
exit 0
`)
	if err := os.WriteFile(mockCodexPath, []byte(mockScript), 0755); err != nil {
		t.Fatalf("failed to write mock codex: %v", err)
	}

	dbPath := filepath.Join(tempDir, "state.db")
	db, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	cfg := model.Config{
		Repositories: map[string]model.Repo{
			"e2e-repo": {
				Root:         sourceRepo,
				AllowedPaths: []string{"calc.go", "calc_test.go", "go.mod"},
				Providers:    []string{"codex"},
			},
		},
		Providers: []model.ProviderConfig{
			{
				ID:    "codex",
				Path:  mockCodexPath,
				Model: "gpt-6.1-sol",
				Speed: "fast",
			},
		},
	}

	reg := providers.New(cfg)
	engine, err := New(cfg, db, reg)
	if err != nil {
		t.Fatalf("failed to create engine: %v", err)
	}
	defer engine.Close()

	// 1. Submit task via delegate_task with target: "auto" / "codex" and registered Go check
	sub := model.Submission{
		RepoID: "e2e-repo",
		Task: model.Task{
			Objective:         "Refactor Add function to optimize performance",
			Category:          "coding",
			Mode:              "patch",
			AllowedPaths:      []string{"calc.go", "calc_test.go", "go.mod"},
			PreferredProvider: "codex",
			PreferredModel:    "gpt-6.1-sol",
			ExecutionProfile:  "implementation",
			Acceptance: []model.Check{
				{ID: "check_add", Description: "Test Add Function", CheckID: "TestAdd"},
			},
		},
	}

	job, err := engine.Submit(sub)
	if err != nil {
		t.Fatalf("failed to submit task: %v", err)
	}

	// 2. Wait for subagent to complete
	completedJob, err := engine.Wait(ctx, job.ID, 75*time.Second)
	if err != nil {
		t.Fatalf("engine.Wait failed: %v", err)
	}

	// 3. Verify terminal COMPLETED status
	if completedJob.Status != model.Completed {
		t.Fatalf("expected job status COMPLETED, got %s (reason: %s)", completedJob.Status, completedJob.Reason)
	}

	// 4. Verify pre/post SHA-256 and source repo immutability
	sourceContent, _ := os.ReadFile(calcGo)
	if string(sourceContent) != originalCalc {
		t.Errorf("source repository was corrupted! got %s, expected original %s", string(sourceContent), originalCalc)
	}

	// 5. Verify Unified diff patch is populated in Job.Diff / Job.Artifacts
	if completedJob.Diff == "" {
		t.Error("expected non-empty Job.Diff")
	}
	if !strings.Contains(completedJob.Diff, "refactored by autonomous codex subagent") {
		t.Errorf("expected diff to contain subagent comment, got: %s", completedJob.Diff)
	}

	// 6. Verify Check summary is recorded as PASSED
	if completedJob.Result == nil || len(completedJob.Result.Checks) == 0 {
		t.Fatal("expected test check evidence in Job.Result")
	}
	if completedJob.Result.Checks[0].Status != "PASSED" || completedJob.Result.Checks[0].ExitStatus != 0 {
		t.Errorf("expected check to pass, got %+v", completedJob.Result.Checks[0])
	}
}

func TestImplementationPreservesPrimaryErrorOnCleanupFailure(t *testing.T) {
	tempDir := canonicalTempDir(t)
	sourceRepo := filepath.Join(tempDir, "source")
	if err := os.MkdirAll(sourceRepo, 0700); err != nil {
		t.Fatal(err)
	}
	const sampleGo = "package immut\n\nfunc Value() string { return \"before\" }\n"
	const sampleTest = "package immut\n\nimport \"testing\"\n\nfunc TestAfter(t *testing.T) {}\n"
	_ = os.WriteFile(filepath.Join(sourceRepo, "go.mod"), []byte("module example.test/immut\n\ngo 1.24\n"), 0644)
	_ = os.WriteFile(filepath.Join(sourceRepo, "sample.go"), []byte(sampleGo), 0644)
	_ = os.WriteFile(filepath.Join(sourceRepo, "sample_test.go"), []byte(sampleTest), 0644)
	setupTestGitRepo(t, sourceRepo)

	snap, err := execution.TakeSnapshot(context.Background(), sourceRepo, "snap-errjoin", "head_only", []string{"go.mod", "sample.go", "sample_test.go"})
	if err != nil {
		t.Fatalf("TakeSnapshot: %v", err)
	}

	cfg := model.Config{
		StateDir:      filepath.Join(tempDir, "state"),
		EnableFixture: true,
		Providers:     []model.ProviderConfig{{ID: "fixture-a", AttestedAccount: "owner-1"}},
	}
	db, err := store.Open(cfg.StateDir)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	e, err := New(cfg, db, providers.New(cfg))
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()

	runCase := func(t *testing.T, private bool) {
		t.Helper()
		attemptDir := filepath.Join(tempDir, fmt.Sprintf("attempt-%t", private))
		if err := execution.NewAttemptDir(attemptDir); err != nil {
			t.Fatal(err)
		}
		workspaceDir := filepath.Join(attemptDir, ".workspace")
		t.Cleanup(func() {
			_ = os.Chmod(workspaceDir, 0700)
		})

		var capFixture model.Capability
		for _, c := range e.reg.Candidates {
			if c.Provider == "fixture-a" {
				capFixture = c
				break
			}
		}

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		// Once RunFixture starts (OpenWorkerLease removes .worker.cleaned after PrepareWorktree returns),
		// lock .workspace perms so w.Close() returns ErrCleanup, then cancel ctx so RunFixture fails.
		go func() {
			cleanedMarker := filepath.Join(attemptDir, ".worker.cleaned")
			for range 500 {
				if _, err := os.Stat(cleanedMarker); os.IsNotExist(err) {
					time.Sleep(50 * time.Millisecond)
					_ = os.Chmod(workspaceDir, 0500)
					cancel()
					return
				}
				time.Sleep(10 * time.Millisecond)
			}
		}()

		task := model.Task{
			Objective:    "fixture:slow",
			Mode:         "patch",
			DataClass:    "synthetic",
			AllowedPaths: []string{"sample.go", "sample_test.go", "go.mod"},
		}
		files := map[string]string{
			"go.mod":         "module example.test/immut\n\ngo 1.24\n",
			"sample.go":      sampleGo,
			"sample_test.go": sampleTest,
		}

		var gotErr error
		if !private {
			_, _, gotErr = e.runFixtureImplementation(ctx, attemptDir, capFixture, task, snap, files, nil)
		} else {
			consent := true
			policy := &model.PrivateSourcePolicy{
				OwnerConsent:            &consent,
				ExpiresAt:               time.Now().Add(time.Hour),
				WriterIsolationVerified: true,
				AllowWrite:              true,
				GoChecks:                map[string]string{"unit": "TestAfter"},
				Files: map[string]string{
					"go.mod":         model.Hash([]byte("module example.test/immut\n\ngo 1.24\n")),
					"sample.go":      model.Hash([]byte(sampleGo)),
					"sample_test.go": model.Hash([]byte(sampleTest)),
				},
				Briefs: []string{model.PrivateBriefDigest("fixture:slow", []model.Check{{ID: "unit", Description: "Pass TestAfter", CheckID: "unit"}})},
				Routes: []model.PrivateRoute{{Provider: "fixture-a", Model: "synthetic", Fingerprint: execution.SelfBinarySHA256(), DataHandlingReviewed: true, ExpectedAccount: "owner-1"}},
			}
			task.DataClass = model.PrivateSourceData
			task.Acceptance = []model.Check{{ID: "unit", Description: "Pass TestAfter", CheckID: "unit"}}
			_, _, _, gotErr = e.runPrivateWriteImplementation(ctx, attemptDir, capFixture, task, snap, files, nil, policy)
		}
		if !errors.Is(gotErr, execution.ErrCleanup) {
			t.Fatalf("expected ErrCleanup in joined error, got: %v", gotErr)
		}
		if !errors.Is(gotErr, context.Canceled) {
			t.Fatalf("primary context.Canceled error was clobbered by cleanup error: %v", gotErr)
		}
	}

	t.Run("fixture implementation", func(t *testing.T) { runCase(t, false) })
	t.Run("private write implementation", func(t *testing.T) { runCase(t, true) })
}

func TestPatchWithCheckIDCannotSkipGoChecks(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	tempDir := canonicalTempDir(t)
	sourceRepo := filepath.Join(tempDir, "source_check_required")
	if err := os.MkdirAll(sourceRepo, 0700); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(sourceRepo, "go.mod"), []byte("module example.test/checkreq\n\ngo 1.24\n"), 0644)
	_ = os.WriteFile(filepath.Join(sourceRepo, "fixture.txt"), []byte("before\n"), 0644)
	_ = os.WriteFile(filepath.Join(sourceRepo, "calc.go"), []byte("package checkreq\n\nfunc Ok() bool { return true }\n"), 0644)
	_ = os.WriteFile(filepath.Join(sourceRepo, "calc_test.go"), []byte("package checkreq\n\nimport \"testing\"\n\nfunc TestFail(t *testing.T) {\n\tt.Fatal(\"expected deterministic failure\")\n}\n\nfunc TestPass(t *testing.T) {}\n"), 0644)
	setupTestGitRepo(t, sourceRepo)

	dbPath := filepath.Join(tempDir, "state")
	db, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	cfg := model.Config{
		StateDir:      dbPath,
		EnableFixture: true,
		Repositories: map[string]model.Repo{
			"check-repo": {
				Root:         sourceRepo,
				AllowedPaths: []string{"fixture.txt", "calc.go", "calc_test.go", "go.mod"},
				Providers:    []string{"codex", "fixture-a"},
			},
		},
	}
	reg := providers.New(cfg)
	// Ensure a deterministic codex candidate exists in registry, then put codex on cooldown
	// so routing falls back to fixture-a while subagent=true.
	reg.Candidates = append(reg.Candidates, model.Capability{
		Provider: "codex",
		Model:    "gpt-6.1-sol",
		Status:   "available",
		Mode:     "cli",
		Speed:    "fast",
	})
	e, err := New(cfg, db, reg)
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()

	// 1. Explicit implementation profile with fixture-a must run Go checks and fail TestFail.
	subImpl, err := e.Submit(model.Submission{
		RepoID:    "check-repo",
		RequestID: "impl-fixture-fail",
		Task: model.Task{
			Objective:         "Modify fixture.txt",
			Mode:              "patch",
			DataClass:         "synthetic",
			ExecutionProfile:  "implementation",
			PreferredProvider: "fixture-a",
			AllowedPaths:      []string{"fixture.txt", "calc.go", "calc_test.go", "go.mod"},
			Acceptance: []model.Check{
				{ID: "check_fail", Description: "Run TestFail", CheckID: "TestFail"},
			},
		},
	})
	if err != nil {
		t.Fatalf("submit impl: %v", err)
	}
	gotImpl, err := e.Wait(ctx, subImpl.ID, 60*time.Second)
	if err != nil {
		t.Fatalf("wait impl: %v", err)
	}
	if gotImpl.Status == model.Completed || gotImpl.Reason != "CHECK_FAILED" {
		t.Fatalf("implementation fixture skipped Go check: status=%s reason=%s result=%+v", gotImpl.Status, gotImpl.Reason, gotImpl.Result)
	}

	// 2. Implicit patch task with CheckID when codex is on cooldown and routes to fixture-a must still run Go checks.
	e.mu.Lock()
	e.cooldown["codex"] = time.Now().Add(10 * time.Minute)
	e.mu.Unlock()
	subFallback, err := e.Submit(model.Submission{
		RepoID:    "check-repo",
		RequestID: "fallback-fixture-fail",
		Task: model.Task{
			Objective:    "Modify fixture.txt with fallback",
			Mode:         "patch",
			DataClass:    "synthetic",
			AllowedPaths: []string{"fixture.txt", "calc.go", "calc_test.go", "go.mod"},
			Acceptance: []model.Check{
				{ID: "check_fail", Description: "Run TestFail", CheckID: "TestFail"},
			},
		},
	})
	if err != nil {
		t.Fatalf("submit fallback: %v", err)
	}
	gotFallback, err := e.Wait(ctx, subFallback.ID, 60*time.Second)
	if err != nil {
		t.Fatalf("wait fallback: %v", err)
	}
	if gotFallback.Status == model.Completed || gotFallback.Reason != "CHECK_FAILED" {
		t.Fatalf("fallback patch skipped Go check: status=%s reason=%s result=%+v", gotFallback.Status, gotFallback.Reason, gotFallback.Result)
	}

	// 3. Batch handoff into implementation patch task with CheckID must forward handoffs to worker.
	batchJob, err := e.SubmitBatch(model.Batch{
		RepoID:    "check-repo",
		RequestID: "batch-handoff-impl",
		Items: []model.Item{
			{
				Key: "produce",
				Task: model.Task{
					Objective:         "Produce fixture diff",
					Mode:              "patch",
					DataClass:         "synthetic",
					PreferredProvider: "fixture-a",
					AllowedPaths:      []string{"fixture.txt"},
					Acceptance:        []model.Check{{ID: "p", Description: "diff"}},
				},
			},
			{
				Key:       "consume",
				DependsOn: []string{"produce"},
				HandoffFrom: []model.HandoffFrom{
					{Key: "produce", Kind: "diff"},
				},
				Task: model.Task{
					Objective:         "fixture:handoff",
					Mode:              "patch",
					DataClass:         "synthetic",
					ExecutionProfile:  "implementation",
					PreferredProvider: "fixture-a",
					AllowedPaths:      []string{"fixture.txt", "calc.go", "calc_test.go", "go.mod"},
					Acceptance:        []model.Check{{ID: "check_pass", Description: "Run TestPass", CheckID: "TestPass"}},
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("submit batch: %v", err)
	}
	gotBatch, err := e.Wait(ctx, batchJob.ID, 60*time.Second)
	if err != nil {
		t.Fatalf("wait batch: %v", err)
	}
	if gotBatch.Status != model.Completed {
		consumer, _ := e.Get(gotBatch.Children[1])
		t.Fatalf("expected batch with handoff into subagent implementation to complete, got batch=%s(%s) consumer=%s(%s)", gotBatch.Status, gotBatch.Reason, consumer.Status, consumer.Reason)
	}
}
