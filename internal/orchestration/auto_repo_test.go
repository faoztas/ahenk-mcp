// Copyright 2026 Furkan Anıl Öztaş
// SPDX-License-Identifier: Apache-2.0

package orchestration

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/faoztas/ahenk-mcp/internal/model"
	"github.com/faoztas/ahenk-mcp/internal/providers"
	"github.com/faoztas/ahenk-mcp/internal/store"
)

func TestEngine_AutoRegisterRepo_Existing(t *testing.T) {
	dir := t.TempDir()
	dbDir := filepath.Join(dir, "state")
	if err := os.Mkdir(dbDir, 0700); err != nil {
		t.Fatal(err)
	}
	repoDir := filepath.Join(dir, "myrepo")
	if err := os.Mkdir(repoDir, 0700); err != nil {
		t.Fatal(err)
	}
	canonicalRepo, _ := filepath.EvalSymlinks(repoDir)

	db, err := store.Open(dbDir)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	cfg := model.Config{
		StateDir: dbDir,
		Repositories: map[string]model.Repo{
			"existing-repo": {
				Root:         canonicalRepo,
				AllowedPaths: []string{"."},
				Providers:    []string{"opencode"},
			},
		},
	}
	e, err := New(cfg, db, providers.Registry{})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()

	resolvedID, err := e.ResolveRepo(repoDir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resolvedID != "existing-repo" {
		t.Fatalf("expected existing-repo, got %q", resolvedID)
	}
}

func TestEngine_AutoRegisterRepo_Dynamic(t *testing.T) {
	dir := t.TempDir()
	dbDir := filepath.Join(dir, "state")
	if err := os.Mkdir(dbDir, 0700); err != nil {
		t.Fatal(err)
	}
	repoDir := filepath.Join(dir, "dynamic-git-repo")
	if err := os.Mkdir(repoDir, 0700); err != nil {
		t.Fatal(err)
	}
	// Init git in repoDir
	cmd := exec.Command("git", "-C", repoDir, "init")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init failed: %v %s", err, out)
	}

	db, err := store.Open(dbDir)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	cfg := model.Config{
		StateDir:     dbDir,
		Repositories: map[string]model.Repo{},
	}
	e, err := New(cfg, db, providers.Registry{})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()

	resolvedID, err := e.ResolveRepo(repoDir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resolvedID == "" {
		t.Fatalf("expected dynamic repo ID, got empty string")
	}

	// Verify repo is added to e.cfg.Repositories
	r, ok := e.cfg.Repositories[resolvedID]
	if !ok {
		t.Fatalf("expected repo %q in e.cfg.Repositories", resolvedID)
	}
	if len(r.AllowedPaths) != 1 || r.AllowedPaths[0] != "." {
		t.Fatalf("expected allowed_paths ['.'], got %v", r.AllowedPaths)
	}
	if len(r.Providers) == 0 {
		t.Fatalf("expected non-empty providers list")
	}
}
