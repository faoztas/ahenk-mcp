// Copyright 2026 Furkan Anıl Öztaş
// SPDX-License-Identifier: Apache-2.0

package execution

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/faoztas/ahenk-mcp/internal/model"
)

func TestLocalSwiftStagesOnlyPinnedCommit(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) string {
		t.Helper()
		out, err := git(t.Context(), root, nil, args...)
		if err != nil {
			t.Fatal(err)
		}
		return strings.TrimSpace(string(out))
	}
	run("init")
	committed := "import XCTest\n"
	testWrite(t, root, "Test.swift", committed)
	testWrite(t, root, "Other.swift", "// excluded\n")
	run("add", "Test.swift", "Other.swift")
	run("-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "-c", "commit.gpgsign=false", "commit", "-qm", "test")
	cfg := localSwiftConfig{Root: root, Commit: run("rev-parse", "HEAD"), Files: map[string]string{"Test.swift": model.Hash([]byte(committed))}}
	testWrite(t, root, "Test.swift", "// local edit\n")
	dir := t.TempDir()
	if err := stageSwiftFiles(t.Context(), cfg, dir); err != nil {
		t.Fatal(err)
	}
	actual, err := os.ReadFile(filepath.Join(dir, "Test.swift"))
	if err != nil || string(actual) != committed {
		t.Fatalf("staged unexpected data: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "Other.swift")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("staging included a file without approval")
	}
	cfg.Files["Test.swift"] = model.Hash([]byte("different"))
	if err := stageSwiftFiles(t.Context(), cfg, t.TempDir()); err == nil {
		t.Fatal("staged a file with a mismatching approval")
	}
	cfg.Commit = strings.Repeat("a", 40)
	if err := stageSwiftFiles(t.Context(), cfg, t.TempDir()); err == nil {
		t.Fatal("accepted a different commit")
	}
}

func TestLocalSwiftRequiresCompletedXCTestSuite(t *testing.T) {
	const passed = "Test Suite 'All tests' passed at 2026-01-01.\n\t Executed 2 tests, with 0 failures (0 unexpected) in 0.1 seconds\n"
	if n := swiftTestCount(passed, 0); n != 2 {
		t.Fatalf("test count = %d", n)
	}
	if n := swiftTestCount(passed, 1); n != 0 {
		t.Fatal("failed invocation reported a pass")
	}
	for _, output := range []string{
		"Executed 2 tests, with 0 failures ",
		strings.Replace(passed, "Executed 2", "Executed 0", 1),
		strings.Replace(passed, "0 failures", "1 failure", 1),
	} {
		if n := swiftTestCount(output, 0); n != 0 {
			t.Fatal("incomplete or unsuccessful XCTest summary was accepted")
		}
	}
}
