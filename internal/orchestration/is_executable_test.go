// Copyright 2026 Furkan Anıl Öztaş
// SPDX-License-Identifier: Apache-2.0

package orchestration

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/faoztas/ahenk-mcp/internal/model"
	"github.com/faoztas/ahenk-mcp/internal/providers"
)

func createDummyBin(t *testing.T, dir, name string) string {
	realDir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		realDir = dir
	}
	p := filepath.Join(realDir, name)
	script := "#!/bin/sh\necho ok\n"
	if err := os.WriteFile(p, []byte(script), 0755); err != nil {
		t.Fatalf("failed to create dummy binary: %v", err)
	}
	return p
}

func TestIsExecutable(t *testing.T) {
	tempDir := t.TempDir()
	codexBin := createDummyBin(t, tempDir, "mock_codex")
	claudeBin := createDummyBin(t, tempDir, "mock_claude")

	// 1. When candidates are available via configured valid paths
	cfg := model.Config{
		Providers: []model.ProviderConfig{
			{ID: "codex", Path: codexBin, Model: "gpt-6.1-sol"},
			{ID: "claude", Path: claudeBin, Model: "claude-opus-5-5"},
		},
	}
	reg := providers.New(cfg)
	e := &Engine{cfg: cfg, reg: reg}

	if !e.IsExecutable("codex") {
		t.Error("expected IsExecutable('codex') to be true with valid binary")
	}
	if !e.IsExecutable("codex-cli") {
		t.Error("expected IsExecutable('codex-cli') to normalize and be true")
	}
	if !e.IsExecutable("claude") {
		t.Error("expected IsExecutable('claude') to be true with valid binary")
	}
	if !e.IsExecutable("claude-cli") {
		t.Error("expected IsExecutable('claude-cli') to normalize and be true")
	}

	// 2. When candidates are missing/disabled
	missingCfg := model.Config{
		Providers: []model.ProviderConfig{
			{ID: "codex", Path: filepath.Join(tempDir, "non_existent_codex")},
			{ID: "claude", Path: filepath.Join(tempDir, "non_existent_claude")},
		},
	}
	missingReg := providers.New(missingCfg)
	missingEngine := &Engine{cfg: missingCfg, reg: missingReg}

	// In missingEngine, neither candidate is available nor valid on disk
	for _, c := range missingReg.Candidates {
		if c.Provider == "claude" && c.Status == "available" {
			t.Fatalf("claude should not be available with missing binary")
		}
	}
	if missingEngine.IsExecutable("claude") {
		t.Error("expected IsExecutable('claude') to be false with non-existent binary")
	}
}
