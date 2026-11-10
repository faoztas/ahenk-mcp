// Copyright 2026 Furkan Anıl Öztaş
// SPDX-License-Identifier: Apache-2.0

package orchestration

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/faoztas/ahenk-mcp/internal/execution"
	"github.com/faoztas/ahenk-mcp/internal/model"
)

func TestCheckpointRestoresOnlyVerifiedWorktree(t *testing.T) {
	ctx := t.Context()
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	dir := func(name string) string {
		p := filepath.Join(base, name)
		if err := os.Mkdir(p, 0700); err != nil {
			t.Fatal(err)
		}
		return p
	}
	repo := dir("source")
	git := func(args ...string) {
		t.Helper()
		cmd := exec.CommandContext(ctx, "/usr/bin/git", append([]string{"-C", repo, "-c", "core.hooksPath=/dev/null",
			"-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "-c", "commit.gpgsign=false"}, args...)...)
		cmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=/var/empty", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null"}
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("fixture git: %v %s", err, output)
		}
	}
	write := func(root, contents string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(root, "calc.go"), []byte(contents), 0600); err != nil {
			t.Fatal(err)
		}
	}
	git("init")
	write(repo, "package fixture\n\nconst value = 0\n")
	git("add", "calc.go")
	git("commit", "-m", "Synthetic baseline")
	task := model.Task{AllowedPaths: []string{"calc.go"}, WorkspaceMode: "head_only"}
	s, err := execution.Capture(ctx, model.Repo{Root: repo}, task)
	if err != nil {
		t.Fatal(err)
	}
	prepare := func(name string) *execution.Worktree {
		t.Helper()
		attempt := filepath.Join(base, name)
		if err := execution.NewAttemptDir(attempt); err != nil {
			t.Fatal(err)
		}
		w, err := execution.PrepareWorktree(ctx, attempt, s, task)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = w.Close() })
		return w
	}
	e := Engine{artifacts: dir("artifacts"), scratch: dir("scratch")}
	first := prepare("interrupted")
	write(first.Dir, "package fixture\n\nconst value = 1\n")
	partial, err := first.Collect(ctx, e.scratch)
	if err != nil || !partial.SourceUnchanged {
		t.Fatalf("partial edit verification failed: %v", err)
	}
	checkpoint := &model.Checkpoint{ArtifactID: "partial", SHA256: partial.ArtifactSHA256, SnapshotID: s.ID,
		SourceFingerprint: partial.SourceFingerprint, Preimages: partial.Preimages}
	if err := e.saveArtifact(checkpoint.ArtifactID, partial.Patch); err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	second := prepare("resume")
	if err := e.restoreCheckpoint(ctx, second, s.ID, checkpoint, task.AllowedPaths); err != nil {
		t.Fatal(err)
	}
	recovered, err := second.Collect(ctx, e.scratch)
	if err != nil || recovered.ArtifactSHA256 != partial.ArtifactSHA256 || !recovered.SourceUnchanged {
		t.Fatalf("resumed worktree evidence mismatch: %v", err)
	}
	wrongPreimage := *checkpoint
	wrongPreimage.Preimages = map[string]string{"calc.go": model.Hash([]byte("other content"))}
	if err := e.restoreCheckpoint(ctx, prepare("wrong-preimage"), s.ID, &wrongPreimage, task.AllowedPaths); err == nil {
		t.Fatal("altered checkpoint preimage accepted")
	}
	third := prepare("tampered")
	if err := os.WriteFile(filepath.Join(e.artifacts, "partial.diff"), []byte("changed"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := e.restoreCheckpoint(ctx, third, s.ID, checkpoint, task.AllowedPaths); err == nil {
		t.Fatal("altered checkpoint accepted")
	}
	write(repo, "package fixture\n\nconst value = 2\n")
	fourth := prepare("source-drift")
	if err := e.restoreCheckpoint(ctx, fourth, s.ID, checkpoint, task.AllowedPaths); err == nil {
		t.Fatal("checkpoint accepted after source drift")
	}
}

func TestPlannedCheckpointUsesDurableSourceAttempt(t *testing.T) {
	checkpoint := &model.Checkpoint{ArtifactID: "synthetic-partial"}
	job := model.Job{Task: model.Task{ExecutionProfile: model.TrustedLocalProfile, CheckpointObjective: "Make first edit"},
		Attempts: []model.Attempt{{Provider: "agy", Checkpoint: checkpoint}, {Provider: "opencode"}}}
	if latestCheckpoint(job) != nil || checkpointProvider(job) != "" {
		t.Fatal("checkpoint restored before explicit resume")
	}
	job.ResumeRequest = "synthetic-resume"
	if latestCheckpoint(job) != checkpoint || checkpointProvider(job) != "agy" {
		t.Fatal("retry lost checkpoint or its provider")
	}
	job.Task.CheckpointObjective = ""
	if latestCheckpoint(job) != checkpoint || checkpointProvider(job) != "" {
		t.Fatal("unplanned checkpoint changed its provider policy")
	}
}

func TestCheckpointAcceptanceRejectsSignedOverflow(t *testing.T) {
	dir := t.TempDir()
	test, err := os.ReadFile("testdata/checkpoint/calc_test.go")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "calc_test.go"), test, 0600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		body string
		pass bool
	}{
		{"if cents < 0 { cents = -cents }; if cents > 10 { return 10 }; return cents", false},
		{"if cents < -10 || cents > 10 { return 10 }; if cents < 0 { cents = -cents }; return cents", true},
	} {
		src := "package fixture\nfunc RoundDollar(cents int) int { " + tc.body + " }\n"
		if err := os.WriteFile(filepath.Join(dir, "calc.go"), []byte(src), 0600); err != nil {
			t.Fatal(err)
		}
		cmd := exec.CommandContext(t.Context(), "go", "test", "-run", "^TestWholeDollar$", "calc.go", "calc_test.go")
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GOTOOLCHAIN=local", "GOPROXY=off", "GOFLAGS=")
		out, err := cmd.CombinedOutput()
		if (err == nil) != tc.pass || !tc.pass && !strings.Contains(string(out), "want 10") {
			t.Fatalf("boundary suite result for pass=%v: %v\n%s", tc.pass, err, out)
		}
	}
}
