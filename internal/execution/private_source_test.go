// Copyright 2026 Furkan Anıl Öztaş
// SPDX-License-Identifier: Apache-2.0

package execution

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/faoztas/ahenk-mcp/internal/model"
)

func TestPrivateSourceCaptureHEADOnly(t *testing.T) {
	ctx := context.Background()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) {
		t.Helper()
		if _, err := git(ctx, dir, nil, args...); err != nil {
			t.Fatal(err)
		}
	}
	write := func(path, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, path), []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	commit := func() {
		run("-c", "user.name=Fixture", "-c", "user.email=fixture@example.test", "-c", "commit.gpgsign=false", "commit", "-qm", "Synthetic fixture")
	}
	run("init", "-q")
	const approved = "package example\n"
	write("approved.go", approved)
	write("other.go", "package example // excluded\n")
	run("add", "--", "approved.go", "other.go")
	commit()
	write("approved.go", "package example // staged but never read\n")
	run("add", "--", "approved.go")
	write("approved.go", "package example // unstaged but never read\n")

	policy := &model.PrivateSourcePolicy{ExpiresAt: time.Now().Add(time.Hour),
		Files: map[string]string{"approved.go": model.Hash([]byte(approved))}}
	repo := model.Repo{Root: dir, PrivateSource: policy}
	task := model.Task{DataClass: model.PrivateSourceData, WorkspaceMode: "head_only", AllowedPaths: []string{"approved.go"}}
	snapshot, err := Capture(ctx, repo, task)
	if err != nil {
		t.Fatal(err)
	}
	files, err := snapshot.Context(task.AllowedPaths)
	if err != nil || len(files) != 1 || files["approved.go"] != approved {
		t.Fatalf("captured something other than approved HEAD bytes: %v", err)
	}
	commit()
	if _, err := Capture(ctx, repo, task); err == nil {
		t.Fatal("accepted changed HEAD content")
	} else if f, ok := err.(*model.Fault); !ok || f.Gate != "DIGEST_MISMATCH" || f.NextAction == "" {
		t.Fatalf("expected DIGEST_MISMATCH gate fault, got %v", err)
	}

	const withPersonal = "package example // contact owner@yarinlik.com\n"
	write("approved.go", withPersonal)
	run("add", "--", "approved.go")
	commit()
	policy.Files["approved.go"] = model.Hash([]byte(withPersonal))
	if _, err := Capture(ctx, repo, task); err == nil {
		t.Fatal("accepted personal data in HEAD")
	} else if f, ok := err.(*model.Fault); !ok || f.Code != "POLICY_DENIED" || f.Gate != "PERSONAL_DATA_CLASSIFIED" {
		t.Fatalf("expected PERSONAL_DATA_CLASSIFIED gate fault, got %v", err)
	}
}

func TestPrivateWorkerIsolationEvidenceRejectsExternalEffects(t *testing.T) {
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	seed, work := filepath.Join(base, "seed.git"), filepath.Join(base, "work")
	if err := initPrivateGit(t.Context(), seed); err != nil {
		t.Fatal(err)
	}
	const allowed = "value.go"
	files := make(map[string]*Version)
	for path, content := range map[string]string{
		"go.mod":        "module example.invalid/isolation\n\ngo 1.20\n",
		allowed:         "package fixture\n\nconst Value = 1\n",
		"value_test.go": "package fixture\n\nimport \"testing\"\n\nfunc TestValue(t *testing.T) { if Value != 2 { t.Fatal(Value) } }\n",
	} {
		files[path], err = version("100644", []byte(content))
		if err != nil {
			t.Fatal(err)
		}
	}
	tree, err := writePrivateTree(t.Context(), seed, files)
	if err != nil {
		t.Fatal(err)
	}
	commit, err := git(t.Context(), seed, []byte("Synthetic isolation fixture\n"),
		"-c", "user.name=Fixture", "-c", "user.email=fixture@example.test", "-c", "commit.gpgSign=false", "commit-tree", tree)
	if err != nil {
		t.Fatal(err)
	}
	fixtureGit(t, seed, "worktree", "add", "--detach", work, strings.TrimSpace(string(commit)))

	const sentinelName = "outside.txt"
	sentinel := filepath.Join(base, sentinelName)
	sentinelDigest := func() string {
		t.Helper()
		b, err := os.ReadFile(sentinel)
		if err != nil {
			t.Fatal(err)
		}
		return model.Hash(b)
	}
	testWrite(t, base, sentinelName, "original sentinel\n")
	before := sentinelDigest()
	testWrite(t, work, allowed, "package fixture\n\nconst Value = 2\n")
	diff, err := git(t.Context(), work, nil, "diff", "--name-only", "--")
	if err != nil || strings.TrimSpace(string(diff)) != allowed {
		t.Fatalf("expected one observed worktree write, got %q: %v", diff, err)
	}

	tool, err := GoCheckTool()
	if err != nil {
		t.Fatal(err)
	}
	check := model.CheckEvidence{ID: "verify-value", Test: "TestValue"}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	if err := runGoCheck(ctx, tool, work, t.TempDir(), &check); err != nil || check.Status != "PASSED" {
		t.Fatalf("registered Go check = %+v, %v", check, err)
	}
	fixtureGit(t, seed, "worktree", "remove", "--force", work)
	_, statErr := os.Lstat(work)
	if !os.IsNotExist(statErr) {
		t.Fatalf("expected fixture worktree cleanup, got %v", statErr)
	}
	// The fixture supplies synthetic backend, process, and network observations.
	// Its plain worktree run and registered check cannot establish host isolation.
	evidence := PrivateWorkerIsolationEvidence{
		Backend: PrivateWorkerHostIsolated, WorktreeRoot: work, WritableRoot: work,
		AllowedWritePaths: []string{allowed}, WrittenPaths: strings.Fields(string(diff)),
		ExternalFilePath: sentinel, ExternalFileBeforeSHA256: before, ExternalFileAfterSHA256: sentinelDigest(),
		HostProcessAccessDenied: true, NetworkEgressDenied: true, CleanupConfirmed: true,
		CheckKind: "go", RegisteredGoChecks: map[string]string{check.ID: check.Test}, Check: check,
	}
	t.Run("verified backend evidence with passing Go check", func(t *testing.T) {
		if err := VerifyPrivateWorkerIsolation(evidence); err != nil {
			t.Fatalf("valid host isolation evidence rejected: %v", err)
		}
	})
	t.Run("registered Swift check evidence", func(t *testing.T) {
		swift := evidence
		swift.CheckKind = "swift"
		swift.Check.Test = "FixtureTests/testValue"
		swift.RegisteredSwiftChecks = map[string]string{swift.Check.ID: swift.Check.Test}
		if err := VerifyPrivateWorkerIsolation(swift); err != nil {
			t.Fatalf("registered Swift check rejected: %v", err)
		}
	})

	for _, tc := range []struct {
		name   string
		change func(*PrivateWorkerIsolationEvidence)
	}{
		{"worktree only", func(ev *PrivateWorkerIsolationEvidence) { ev.Backend = "worktree_only" }},
		{"unsupported backend", func(ev *PrivateWorkerIsolationEvidence) { ev.Backend = "unknown" }},
		{"writable parent directory", func(ev *PrivateWorkerIsolationEvidence) { ev.WritableRoot = base }},
		{"out of scope write", func(ev *PrivateWorkerIsolationEvidence) { ev.WrittenPaths = []string{"go.mod"} }},
		{"invalid write scope", func(ev *PrivateWorkerIsolationEvidence) { ev.AllowedWritePaths = []string{".", allowed} }},
		{"no observed write", func(ev *PrivateWorkerIsolationEvidence) { ev.WrittenPaths = nil }},
		{"sentinel is worktree parent", func(ev *PrivateWorkerIsolationEvidence) { ev.ExternalFilePath = base }},
		{"uppercase digest", func(ev *PrivateWorkerIsolationEvidence) {
			ev.ExternalFileBeforeSHA256 = strings.ToUpper(before)
			ev.ExternalFileAfterSHA256 = ev.ExternalFileBeforeSHA256
		}},
		{"invalid registered Swift selector", func(ev *PrivateWorkerIsolationEvidence) {
			ev.CheckKind = "swift"
			ev.Check.Test = "../Test:invalid"
			ev.RegisteredSwiftChecks = map[string]string{ev.Check.ID: ev.Check.Test}
		}},
		{"sentinel inside workspace", func(ev *PrivateWorkerIsolationEvidence) { ev.ExternalFilePath = filepath.Join(work, sentinelName) }},
		{"missing sentinel digests", func(ev *PrivateWorkerIsolationEvidence) {
			ev.ExternalFileBeforeSHA256, ev.ExternalFileAfterSHA256 = "", ""
		}},
		{"no host process boundary", func(ev *PrivateWorkerIsolationEvidence) { ev.HostProcessAccessDenied = false }},
		{"no egress boundary", func(ev *PrivateWorkerIsolationEvidence) { ev.NetworkEgressDenied = false }},
		{"cleanup unconfirmed", func(ev *PrivateWorkerIsolationEvidence) { ev.CleanupConfirmed = false }},
		{"failed registered check", func(ev *PrivateWorkerIsolationEvidence) { ev.Check.Status, ev.Check.ExitStatus = model.Failed, 1 }},
		{"nonzero check exit", func(ev *PrivateWorkerIsolationEvidence) { ev.Check.ExitStatus = 1 }},
		{"unregistered check", func(ev *PrivateWorkerIsolationEvidence) { ev.Check.Test = "TestOther" }},
		{"Go check claimed as Swift", func(ev *PrivateWorkerIsolationEvidence) { ev.CheckKind = "swift" }},
		{"unsupported check kind", func(ev *PrivateWorkerIsolationEvidence) { ev.CheckKind = "shell" }},
		{"tampered external sentinel", func(ev *PrivateWorkerIsolationEvidence) {
			testWrite(t, base, sentinelName, "modified sentinel\n")
			ev.ExternalFileAfterSHA256 = sentinelDigest()
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			invalid := evidence
			tc.change(&invalid)
			if err := VerifyPrivateWorkerIsolation(invalid); err == nil {
				t.Fatal("invalid host isolation evidence was accepted")
			}
		})
	}
}
