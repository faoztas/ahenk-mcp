// Copyright 2026 Furkan Anıl Öztaş
// SPDX-License-Identifier: Apache-2.0

package execution

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/faoztas/ahenk-mcp/internal/model"
)

func TestPrivateWorktreePreservesSnapshotAndSource(t *testing.T) {
	source, attempt, snapshot, task := worktreeFixture(t)
	w := newTestWorktree(t, attempt, snapshot, task)
	for name, want := range map[string]string{"code/a.go": "working\n", "code/b.go": "untracked\n"} {
		got, err := os.ReadFile(filepath.Join(w.Dir, name))
		if err != nil || string(got) != want {
			t.Fatalf("worktree %s = %q, %v", name, got, err)
		}
	}
	for _, name := range []string{"outside.txt", "code/ignored.go"} {
		if _, err := os.Lstat(filepath.Join(w.Dir, name)); !os.IsNotExist(err) {
			t.Fatalf("unapproved file %s appeared in workspace: %v", name, err)
		}
	}
	if _, err := git(t.Context(), w.Dir, nil, "cat-file", "-e", snapshot.BaseSHA); err == nil {
		t.Fatal("worker can resolve the original commit from its Git metadata")
	}

	testWrite(t, w.Dir, "code/a.go", "fixed\n")
	if err := os.Remove(filepath.Join(w.Dir, "code/b.go")); err != nil {
		t.Fatal(err)
	}
	testWrite(t, w.Dir, "code/c.go", "new file\n")
	got, err := w.Collect(t.Context(), attempt)
	if err != nil {
		t.Fatal(err)
	}
	if !got.SourceUnchanged || got.ArtifactSHA256 != model.Hash(got.Patch) || !bytes.Contains(got.Patch, []byte("+fixed")) {
		t.Fatalf("unexpected implementation artifact: %+v", got)
	}
	for name, hashes := range map[string][2]string{
		"code/a.go": {model.Hash([]byte("working\n")), model.Hash([]byte("fixed\n"))},
		"code/b.go": {model.Hash([]byte("untracked\n")), "absent"},
		"code/c.go": {"absent", model.Hash([]byte("new file\n"))},
	} {
		if got.Preimages[name] != hashes[0] || got.Postimages[name] != hashes[1] {
			t.Fatalf("unexpected preimage or postimage for %s", name)
		}
	}
	data, err := os.ReadFile(filepath.Join(source, "code/a.go"))
	if err != nil || string(data) != "working\n" {
		t.Fatalf("source checkout changed: %q, %v", data, err)
	}
	index, err := git(t.Context(), source, nil, "show", ":code/a.go")
	if err != nil || string(index) != "staged\n" {
		t.Fatalf("source index changed: %q, %v", index, err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(w.Dir); !os.IsNotExist(err) {
		t.Fatalf("owned worktree was not removed: %v", err)
	}
}

func TestPrivateWorktreeFixtureAppliesOnlyVerifiedPatch(t *testing.T) {
	source, attempt, snapshot, task := worktreeFixture(t)
	if w, err := PrepareWorktree(t.Context(), source, snapshot, task); err == nil || w != nil {
		t.Fatal("source checkout accepted as an attempt directory")
	}
	w := newTestWorktree(t, attempt, snapshot, task)
	patch := []byte("diff --git a/code/a.go b/code/a.go\n--- a/code/a.go\n+++ b/code/a.go\n@@ -1 +1 @@\n-working\n+fixed\n")
	escape := bytes.ReplaceAll(patch, []byte("code/a.go"), []byte("../outside.txt"))
	if err := w.ApplyPatch(t.Context(), escape); err == nil {
		t.Fatal("fixture patch escaped the path boundary")
	}
	if err := w.ApplyPatch(t.Context(), patch); err != nil {
		t.Fatal(err)
	}
	out, err := w.Collect(t.Context(), attempt)
	if err != nil || out.Postimages["code/a.go"] != model.Hash([]byte("fixed\n")) {
		t.Fatalf("fixture edit was not verified: %v", err)
	}
}

func TestPrivateWorktreeRejectsInvalidOutput(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*testing.T, string, string)
	}{
		{"out of scope", func(t *testing.T, work, _ string) { testWrite(t, work, "outside.txt", "injected\n") }},
		{"symlink", func(t *testing.T, work, source string) {
			if err := os.Symlink(filepath.Join(source, "outside.txt"), filepath.Join(work, "code", "link")); err != nil {
				t.Fatal(err)
			}
		}},
		{"Git metadata", func(t *testing.T, work, source string) {
			testWrite(t, work, ".git", "gitdir: "+filepath.Join(source, ".git")+"\n")
		}},
		{"source index drift", func(t *testing.T, _, source string) { fixtureGit(t, source, "add", "--", "code/a.go") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			source, attempt, snapshot, task := worktreeFixture(t)
			w := newTestWorktree(t, attempt, snapshot, task)
			testWrite(t, w.Dir, "code/a.go", "fixed\n")
			tc.change(t, w.Dir, source)
			out, err := w.Collect(t.Context(), attempt)
			if err == nil || len(out.Patch) != 0 || out.ArtifactSHA256 != "" {
				t.Fatalf("invalid worktree produced an artifact: %v", err)
			}
		})
	}
}

func TestPrivateWorktreeQuarantinesUnconfirmedCleanup(t *testing.T) {
	_, attempt, snapshot, task := worktreeFixture(t)
	w := newTestWorktree(t, attempt, snapshot, task)
	lease, err := os.OpenFile(filepath.Join(attempt, ".worker.lock"), os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Flock(int(lease.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		lease.Close()
		t.Fatal(err)
	}
	if err := w.Close(); !errors.Is(err, ErrCleanup) {
		t.Fatalf("live lease did not quarantine the workspace: %v", err)
	}
	if err := lease.Close(); err != nil {
		t.Fatal(err)
	}
	if err := RecoverWorktree(attempt); !errors.Is(err, ErrCleanup) {
		t.Fatalf("cleanup without a supervisor marker was accepted: %v", err)
	}
	marker := filepath.Join(attempt, ".worker.cleaned")
	if err := os.Symlink(".worker.lock", marker); err != nil {
		t.Fatal(err)
	}
	if err := RecoverWorktree(attempt); !errors.Is(err, ErrCleanup) {
		t.Fatalf("cleanup accepted a symlink marker: %v", err)
	}
	if err := os.Remove(marker); err != nil {
		t.Fatal(err)
	}
	testWrite(t, attempt, ".worker.cleaned", "")
	if err := RecoverWorktree(attempt); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(w.Dir); !os.IsNotExist(err) {
		t.Fatalf("recovered worktree was not removed: %v", err)
	}
}

func worktreeFixture(t *testing.T) (string, string, Snapshot, model.Task) {
	t.Helper()
	temp, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	source, attempt := filepath.Join(temp, "source"), filepath.Join(temp, "attempt")
	for _, path := range []string{filepath.Join(source, "code"), attempt} {
		if err := os.MkdirAll(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	fixtureGit(t, source, "init", "--template=")
	testWrite(t, source, "code/a.go", "base\n")
	testWrite(t, source, "outside.txt", "outside\n")
	fixtureGit(t, source, "add", "--", ".")
	fixtureGit(t, source, "-c", "user.name=Fixture", "-c", "user.email=fixture@example.test", "commit", "-m", "Fixture")
	testWrite(t, source, "code/a.go", "staged\n")
	fixtureGit(t, source, "add", "--", "code/a.go")
	testWrite(t, source, "code/a.go", "working\n")
	testWrite(t, source, "code/b.go", "untracked\n")
	testWrite(t, source, "code/ignored.go", "unselected\n")
	task := model.Task{
		AllowedPaths: []string{"code"}, UntrackedPaths: []string{"code/b.go"},
		WorkspaceMode: "include_working_changes",
	}
	snapshot, err := Capture(t.Context(), model.Repo{Root: source, AllowedPaths: task.AllowedPaths}, task)
	if err != nil {
		t.Fatal(err)
	}
	return source, attempt, snapshot, task
}

func newTestWorktree(t *testing.T, attempt string, snapshot Snapshot, task model.Task) *Worktree {
	t.Helper()
	w, err := PrepareWorktree(t.Context(), attempt, snapshot, task)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := w.Close(); err != nil {
			t.Error(err)
		}
	})
	return w
}

func fixtureGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	if _, err := git(t.Context(), dir, nil, args...); err != nil {
		t.Fatalf("git %s: %v", strings.Join(args, " "), err)
	}
}

func testWrite(t *testing.T, dir, name, contents string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(contents), 0600); err != nil {
		t.Fatal(err)
	}
}
