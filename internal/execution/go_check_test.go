// Copyright 2026 Furkan Anıl Öztaş
// SPDX-License-Identifier: Apache-2.0

package execution

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestGoCheckRequiresNamedTestAndPackagePass(t *testing.T) {
	run := `{"Action":"run","Test":"TestFixture","Package":"example.invalid/fixture"}`
	pass := `{"Action":"pass","Test":"TestFixture","Package":"example.invalid/fixture"}`
	pkg := `{"Action":"pass","Package":"example.invalid/fixture"}`
	for _, tc := range []struct {
		name string
		log  string
		want bool
	}{
		{"verified", run + "\n" + pass + "\n" + pkg, true},
		{"no matching test", pkg, false},
		{"test only", run + "\n" + pass, false},
		{"failed package", run + "\n" + pass + "\n" + `{"Action":"fail","Package":"example.invalid/fixture"}`, false},
		{"skipped", run + "\n" + `{"Action":"skip","Test":"TestFixture"}` + "\n" + pkg, false},
		{"malformed stream", run + "\n" + pass + "\n" + pkg + "\nbroken", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := goCheckPassed([]byte(tc.log), "TestFixture"); got != tc.want {
				t.Fatalf("goCheckPassed returned %t, want %t", got, tc.want)
			}
		})
	}
}

func TestUnconfirmedGoCheckKeepsWorktreeQuarantined(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "attempt")
	if err := NewAttemptDir(dir); err != nil {
		t.Fatal(err)
	}
	checkDir := filepath.Join(dir, goCheckDir)
	workspace := filepath.Join(dir, privateWorkspace)
	for _, path := range []string{checkDir, workspace} {
		if err := os.Mkdir(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := Save(dir, goCheckIntent, nil); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(checkDir, workerLock), nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := RecoverWorktree(dir); !errors.Is(err, ErrCleanup) {
		t.Fatalf("unconfirmed test process did not quarantine the worktree: %v", err)
	}
	if _, err := os.Stat(workspace); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(checkDir, workerLock)); err != nil {
		t.Fatal(err)
	}
	if err := RecoverWorktree(dir); !errors.Is(err, ErrCleanup) {
		t.Fatalf("missing Go check lock did not quarantine the worktree: %v", err)
	}
	if err := os.Rename(checkDir, checkDir+".hidden"); err != nil {
		t.Fatal(err)
	}
	if err := RecoverWorktree(dir); !errors.Is(err, ErrCleanup) {
		t.Fatalf("missing Go check directory did not quarantine the worktree: %v", err)
	}
	if err := os.Rename(checkDir+".hidden", checkDir); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(checkDir, workerLock), nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(checkDir, workerCleaned), nil, 0600); err != nil {
		t.Fatal(err)
	}
	real, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := RecoverWorktree(real); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(workspace); !os.IsNotExist(err) {
		t.Fatalf("released worktree not removed: %v", err)
	}
}
