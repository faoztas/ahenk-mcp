// Copyright 2026 Furkan Anıl Öztaş
// SPDX-License-Identifier: Apache-2.0

package providers

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/faoztas/ahenk-mcp/internal/model"
)

func createDummyExecutable(t *testing.T, dir, name string) string {
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

func TestRegistryCLIModels(t *testing.T) {
	// A bare Registry without explicit provider configs must contain the required capability models.
	reg := New(model.Config{})

	requiredCodexModels := []string{
		"gpt-6.1-sol",
		"gpt-6.1-terra",
		"gpt-6-astra",
		"gpt-6-luna",
	}
	requiredClaudeModels := []string{
		"claude-opus-5-5",
		"claude-sonnet-5-5",
	}

	for _, reqModel := range requiredCodexModels {
		idx := slices.IndexFunc(reg.Candidates, func(c model.Capability) bool {
			return c.Provider == "codex" && c.Model == reqModel
		})
		if idx < 0 {
			t.Errorf("missing required codex candidate model: %s", reqModel)
		} else {
			c := reg.Candidates[idx]
			if c.Mode != "cli" {
				t.Errorf("expected codex candidate mode 'cli', got %s", c.Mode)
			}
		}
	}

	for _, reqModel := range requiredClaudeModels {
		idx := slices.IndexFunc(reg.Candidates, func(c model.Capability) bool {
			return c.Provider == "claude" && c.Model == reqModel
		})
		if idx < 0 {
			t.Errorf("missing required claude candidate model: %s", reqModel)
		} else {
			c := reg.Candidates[idx]
			if c.Mode != "cli" {
				t.Errorf("expected claude candidate mode 'cli', got %s", c.Mode)
			}
		}
	}
}

func TestRegistryCLIAdmission(t *testing.T) {
	tempDir := t.TempDir()
	codexPath := createDummyExecutable(t, tempDir, "codex")
	claudePath := createDummyExecutable(t, tempDir, "claude")

	cfg := model.Config{
		Providers: []model.ProviderConfig{
			{ID: "codex", Path: codexPath, Model: "gpt-5-sol", Speed: "fast"},
			{ID: "claude", Path: claudePath, Model: "claude-3-7-sonnet", Speed: "fast"},
		},
	}

	reg := New(cfg)

	var codexCaps, claudeCaps []model.Capability
	for _, c := range reg.Candidates {
		if c.Provider == "codex" {
			codexCaps = append(codexCaps, c)
		}
		if c.Provider == "claude" {
			claudeCaps = append(claudeCaps, c)
		}
	}

	if len(codexCaps) == 0 {
		t.Fatal("expected codex candidates in Registry.Candidates")
	}
	for _, c := range codexCaps {
		if c.Status != "available" {
			t.Errorf("expected codex candidate %s to be available, got %s (reason: %s)", c.Model, c.Status, c.Reason)
		}
		if c.Fingerprint == "" {
			t.Errorf("expected non-empty fingerprint for codex model %s", c.Model)
		}
		if !AllowsTrustedLocal(c) {
			t.Errorf("expected AllowsTrustedLocal(codex %s) to be true", c.Model)
		}
	}

	if len(claudeCaps) == 0 {
		t.Fatal("expected claude candidates in Registry.Candidates")
	}
	for _, c := range claudeCaps {
		if c.Status != "available" {
			t.Errorf("expected claude candidate %s to be available, got %s (reason: %s)", c.Model, c.Status, c.Reason)
		}
		if c.Fingerprint == "" {
			t.Errorf("expected non-empty fingerprint for claude model %s", c.Model)
		}
		if !AllowsTrustedLocal(c) {
			t.Errorf("expected AllowsTrustedLocal(claude %s) to be true", c.Model)
		}
	}

	// Missing binary configuration
	missingCfg := model.Config{
		Providers: []model.ProviderConfig{
			{ID: "codex", Path: filepath.Join(tempDir, "non_existent_codex")},
			{ID: "claude", Path: filepath.Join(tempDir, "non_existent_claude")},
		},
	}
	missingReg := New(missingCfg)
	for _, c := range missingReg.Candidates {
		if c.Provider == "codex" || c.Provider == "claude" {
			if c.Status != "disabled" && c.Status != "missing" {
				t.Errorf("expected missing candidate %s/%s to be disabled/missing, got %s", c.Provider, c.Model, c.Status)
			}
			if AllowsTrustedLocal(c) {
				t.Errorf("AllowsTrustedLocal must be false for disabled candidate %s/%s", c.Provider, c.Model)
			}
		}
	}
}

func TestRegistryRunDispatchCLI(t *testing.T) {
	tempDir := t.TempDir()
	realTemp, err := filepath.EvalSymlinks(tempDir)
	if err != nil {
		realTemp = tempDir
	}

	// 1. Mock Codex binary
	mockCodex := filepath.Join(realTemp, "mock_codex")
	codexScript := `#!/bin/sh
echo '{"type":"thread.started","thread_id":"sess-codex-dispatch"}'
echo '{"type":"item.completed","item":{"type":"agent_message","text":"Codex dispatched successfully"}}'
echo '{"type":"turn.completed","usage":{"input_tokens":50,"output_tokens":25}}'
exit 0
`
	if err := os.WriteFile(mockCodex, []byte(codexScript), 0755); err != nil {
		t.Fatalf("failed to write mock codex: %v", err)
	}

	// 2. Mock Claude binary
	mockClaude := filepath.Join(realTemp, "mock_claude")
	claudeScript := `#!/bin/sh
cat << 'EOF'
{
  "session_id": "sess-claude-dispatch",
  "result": "Claude dispatched successfully",
  "subtype": "success",
  "usage": {
    "input_tokens": 120,
    "output_tokens": 40
  }
}
EOF
exit 0
`
	if err := os.WriteFile(mockClaude, []byte(claudeScript), 0755); err != nil {
		t.Fatalf("failed to write mock claude: %v", err)
	}

	cfg := model.Config{
		Providers: []model.ProviderConfig{
			{ID: "codex", Path: mockCodex, Model: "gpt-6.1-sol", Speed: "fast"},
			{ID: "claude", Path: mockClaude, Model: "claude-opus-5-5", Speed: "fast"},
		},
	}
	reg := New(cfg)

	var codexCap, claudeCap model.Capability
	for _, c := range reg.Candidates {
		if c.Provider == "codex" && c.Model == "gpt-6.1-sol" {
			codexCap = c
		}
		if c.Provider == "claude" && c.Model == "claude-opus-5-5" {
			claudeCap = c
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Test Codex Run dispatch
	taskCodex := model.Task{Objective: "Test codex dispatch", Mode: "read_only"}
	outCodex, err := reg.Run(ctx, realTemp, codexCap, taskCodex, nil, nil)
	if err != nil {
		t.Fatalf("unexpected error in codex Run: %v", err)
	}
	if outCodex.Codex == nil {
		t.Fatal("expected out.Codex to be populated")
	}
	if outCodex.Summary != "Codex dispatched successfully" {
		t.Errorf("expected 'Codex dispatched successfully', got %q", outCodex.Summary)
	}
	if outCodex.Codex.SessionSHA256 != "sess-codex-dispatch" {
		t.Errorf("expected session sess-codex-dispatch, got %s", outCodex.Codex.SessionSHA256)
	}

	// Test Claude Run dispatch
	taskClaude := model.Task{Objective: "Test claude dispatch", Mode: "read_only"}
	outClaude, err := reg.Run(ctx, realTemp, claudeCap, taskClaude, nil, nil)
	if err != nil {
		t.Fatalf("unexpected error in claude Run: %v", err)
	}
	if outClaude.Claude == nil {
		t.Fatal("expected out.Claude to be populated")
	}
	if outClaude.Summary != "Claude dispatched successfully" {
		t.Errorf("expected 'Claude dispatched successfully', got %q", outClaude.Summary)
	}
	if outClaude.Claude.SessionSHA256 != "sess-claude-dispatch" {
		t.Errorf("expected session sess-claude-dispatch, got %s", outClaude.Claude.SessionSHA256)
	}
}

func TestRegistryRunTrustedLocalCLI(t *testing.T) {
	tempDir := t.TempDir()
	realTemp, err := filepath.EvalSymlinks(tempDir)
	if err != nil {
		realTemp = tempDir
	}
	workDir := filepath.Join(realTemp, "worktree")
	if err := os.MkdirAll(workDir, 0755); err != nil {
		t.Fatalf("failed to create worktree: %v", err)
	}

	mockCodex := filepath.Join(realTemp, "mock_codex")
	codexScript := `#!/bin/sh
echo '{"type":"thread.started","thread_id":"sess-codex-trusted"}'
echo '{"type":"item.completed","item":{"type":"agent_message","text":"Codex trusted patch executed"}}'
echo '{"type":"turn.completed","usage":{"input_tokens":60,"output_tokens":30}}'
exit 0
`
	if err := os.WriteFile(mockCodex, []byte(codexScript), 0755); err != nil {
		t.Fatalf("failed to write mock codex: %v", err)
	}

	mockClaude := filepath.Join(realTemp, "mock_claude")
	claudeScript := `#!/bin/sh
cat << 'EOF'
{
  "session_id": "sess-claude-trusted",
  "result": "Claude trusted patch executed",
  "subtype": "success",
  "usage": {
    "input_tokens": 80,
    "output_tokens": 40
  }
}
EOF
exit 0
`
	if err := os.WriteFile(mockClaude, []byte(claudeScript), 0755); err != nil {
		t.Fatalf("failed to write mock claude: %v", err)
	}

	cfg := model.Config{
		Providers: []model.ProviderConfig{
			{ID: "codex", Path: mockCodex, Model: "gpt-6.1-sol", Speed: "fast"},
			{ID: "claude", Path: mockClaude, Model: "claude-opus-5-5", Speed: "fast"},
		},
	}
	reg := New(cfg)

	var codexCap, claudeCap model.Capability
	for _, c := range reg.Candidates {
		if c.Provider == "codex" && c.Model == "gpt-6.1-sol" {
			codexCap = c
		}
		if c.Provider == "claude" && c.Model == "claude-opus-5-5" {
			claudeCap = c
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	task := model.Task{
		Objective:        "Apply trusted patch",
		Mode:             "patch",
		ExecutionProfile: model.TrustedLocalProfile,
		DataClass:        "synthetic",
	}

	// 1. RunTrustedLocal for Codex
	outCodex, err := reg.RunTrustedLocal(ctx, realTemp, workDir, codexCap, task, []string{"pkg/mod.go"})
	if err != nil {
		t.Fatalf("unexpected error in codex RunTrustedLocal: %v", err)
	}
	if outCodex.Codex == nil {
		t.Fatal("expected out.Codex to be populated")
	}
	if outCodex.Summary != "Codex trusted patch executed" {
		t.Errorf("expected 'Codex trusted patch executed', got %q", outCodex.Summary)
	}

	// 2. RunTrustedLocal for Claude
	outClaude, err := reg.RunTrustedLocal(ctx, realTemp, workDir, claudeCap, task, []string{"pkg/mod.go"})
	if err != nil {
		t.Fatalf("unexpected error in claude RunTrustedLocal: %v", err)
	}
	if outClaude.Claude == nil {
		t.Fatal("expected out.Claude to be populated")
	}
	if outClaude.Summary != "Claude trusted patch executed" {
		t.Errorf("expected 'Claude trusted patch executed', got %q", outClaude.Summary)
	}
}
