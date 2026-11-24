// Copyright 2026 Furkan Anıl Öztaş
// SPDX-License-Identifier: Apache-2.0

package providers

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/faoztas/ahenk-mcp/internal/model"
)

// registryCandidates returns every capability announced by reg that matches.
func registryCandidates(reg Registry, match func(model.Capability) bool) []model.Capability {
	var out []model.Capability
	for _, candidate := range reg.Candidates {
		if match(candidate) {
			out = append(out, candidate)
		}
	}
	return out
}

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

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
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

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
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

// TestAGYACPRemainsUnavailableWithoutApprovedRoute pins G-027 / E3.1 as a
// decision guard: the registered AGY Headless capability stays visible, while
// an unapproved ACP provider entry never becomes an admitted route and default
// routing never starts an ACP process without an explicit approval/PoC gate.
func TestAGYACPRemainsUnavailableWithoutApprovedRoute(t *testing.T) {
	isHeadless := func(c model.Capability) bool { return c.Provider == "agy" && c.Mode == "headless" }
	isACP := func(c model.Capability) bool { return Family(c.Provider) == "acp" || c.Provider == "agy-acp" }

	base := New(model.Config{})
	headless := registryCandidates(base, isHeadless)
	if len(headless) == 0 {
		t.Fatal("G-027: the registered AGY Headless capability must stay visible")
	}
	if headless[0].Status == "available" {
		t.Fatalf("G-027: AGY Headless must not be admitted without its signed synthetic gate, got status %q", headless[0].Status)
	}
	if acp := registryCandidates(base, isACP); len(acp) != 0 {
		t.Fatalf("G-027: the default registry must not admit ACP routes, got %+v", acp)
	}

	// ACP is only an unapproved candidate. Its command must never run as a
	// side effect of registry construction or default routing.
	dir := t.TempDir()
	acpBinary := createDummyExecutable(t, dir, "agy-acp")
	marker := filepath.Join(filepath.Dir(acpBinary), "acp-started")
	if err := os.WriteFile(acpBinary, []byte("#!/bin/sh\n: > \""+marker+"\"\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("failed to write ACP probe: %v", err)
	}

	reg := New(model.Config{Providers: []model.ProviderConfig{
		{ID: "acp", Path: acpBinary, Model: "acp-default", ACPCommand: acpBinary, ACPArgs: []string{"--acp"}, EnableSynthetic: true},
		{ID: "agy-acp", Path: acpBinary, Model: agySyntheticModel, ACPCommand: acpBinary, ACPArgs: []string{"--acp"}, EnableSynthetic: true},
	}})
	if acp := registryCandidates(reg, isACP); len(acp) != 0 {
		t.Fatalf("G-027: unapproved ACP config was admitted without an approval/PoC gate: %+v", acp)
	}
	if headless = registryCandidates(reg, isHeadless); len(headless) == 0 {
		t.Fatal("G-027: unapproved ACP config removed the AGY Headless candidate")
	} else if headless[0].Status == "available" {
		t.Fatalf("G-027: unapproved ACP config admitted AGY Headless, got status %q", headless[0].Status)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	task := model.Task{Objective: "probe ACP default routing", Mode: "read_only", DataClass: "synthetic"}
	for _, provider := range []string{"acp", "agy-acp"} {
		synthetic := model.Capability{Provider: provider, Model: "acp-default", Status: "available", Mode: "acp"}
		_, err := reg.Run(ctx, dir, synthetic, task, nil, nil)
		if err == nil {
			t.Fatalf("G-027: default routing started %s without an approved ACP route", provider)
		}
		if fault, ok := err.(*model.Fault); !ok || fault.Code != "POLICY_DENIED" {
			t.Fatalf("G-027: expected POLICY_DENIED for %s, got %v", provider, err)
		}
		if _, err := os.Stat(marker); err == nil {
			t.Fatalf("G-027: the %s ACP command was executed without approval", provider)
		}
	}
}

// TestLocalProviderRemainsUnavailableUntilCapabilityVerified pins G-029 / E3.3
// as a decision guard: default config plus an external binary alone never opens
// a third local inference worker while capability, local egress boundary,
// latency and role evidence (or a verified synthetic pass) are missing.
func TestLocalProviderRemainsUnavailableUntilCapabilityVerified(t *testing.T) {
	dir := t.TempDir()
	localBinary := createDummyExecutable(t, dir, "local-inference")
	isLocalFamily := func(c model.Capability) bool { return Family(c.Provider) == "local" }

	for _, tc := range []struct {
		name string
		cfg  model.Config
	}{
		{"default config", model.Config{}},
		{"external local binary only", model.Config{Providers: []model.ProviderConfig{
			{ID: "ollama", Path: localBinary, Model: "llama-local"},
		}}},
		{"binary plus zero-cost claim", model.Config{Providers: []model.ProviderConfig{
			{ID: "local", Path: localBinary, Model: "local-inference", Speed: "fast", EnableSynthetic: true},
		}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reg := New(tc.cfg)
			for _, c := range reg.Candidates {
				if isLocalFamily(c) && c.Status == "available" {
					t.Fatalf("G-029: admitted a third local inference worker (%s/%s) without capability, egress, latency, role evidence or a verified synthetic pass",
						c.Provider, c.Model)
				}
			}
		})
	}

	// The local egress boundary is loopback-only and a non-loopback endpoint
	// does not become a local provider.
	for _, raw := range []string{"http://127.0.0.1:11434", "http://localhost:11434", "http://[::1]:11434"} {
		if err := RunLocalLLM(raw); err != nil {
			t.Errorf("G-029: loopback endpoint %q was rejected: %v", raw, err)
		}
	}
	for _, raw := range []string{"https://api.example.com/v1", "http://192.168.1.20:11434", "http://10.0.0.5:11434"} {
		err := RunLocalLLM(raw)
		if err == nil {
			t.Errorf("G-029: non-loopback endpoint %q was accepted without an egress boundary decision", raw)
			continue
		}
		if fault, ok := err.(*model.Fault); !ok || fault.Code != "PROVIDER_STARTUP_FAILED" {
			t.Errorf("G-029: expected PROVIDER_STARTUP_FAILED for %q, got %v", raw, err)
		}
	}

	// Cost evidence alone is not admission: LOCAL_ONLY plus an available status
	// still has no role, latency or synthetic capability proof.
	localCap := model.Capability{Provider: "local", Model: "local-inference", Status: "available", Mode: "local", Cost: LocalZeroCost()}
	if AllowsTrustedLocal(localCap) {
		t.Fatal("G-029: LOCAL_ONLY cost evidence admitted a trusted-local worker without verified capability")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := New(model.Config{}).Run(ctx, dir, localCap,
		model.Task{Objective: "probe local inference", Mode: "read_only", DataClass: "synthetic"}, nil, nil); err == nil {
		t.Fatal("G-029: default routing executed an unverified local inference worker")
	}
}

// TestCopilotStaysDisabledUntilEntitlementAndCostVerified pins G-030 / E3.4 as
// a decision guard: copilot remains disabled (CLAUDE_ONLY_UNAVAILABLE) in
// providers.New even when configured or paired with CodexBar copilot decode
// support, until Claude-only role entitlement and cost policy are verified.
func TestCopilotStaysDisabledUntilEntitlementAndCostVerified(t *testing.T) {
	dir := t.TempDir()
	copilotBinary := createDummyExecutable(t, dir, "copilot")

	// CodexBar copilot decoding and the cheap price tier are diagnostics only;
	// neither is worker entitlement or a verified cost policy.
	now := time.Date(2026, 11, 12, 9, 45, 0, 0, time.UTC)
	rows, err := decodeCodexBar(strings.NewReader(codexBar084Fixture(t, "codexbar_084_copilot.json")), "copilot", "oauth", "", now)
	if err != nil || len(rows) != 1 || rows[0].Provider != "copilot" {
		t.Fatalf("G-030: CodexBar copilot decode support must remain intact: rows=%+v err=%v", rows, err)
	}
	if FPTier("copilot", "claude-sonnet-5-5") == "" {
		t.Fatal("G-030: copilot price tier classification must remain intact")
	}

	providers := []model.ProviderConfig{
		{ID: "copilot", Path: copilotBinary, Model: "claude-sonnet-5-5", Speed: "fast", EnableSynthetic: true},
	}
	var reg Registry
	for _, tc := range []struct {
		name string
		cfg  model.Config
	}{
		{"default config", model.Config{}},
		{"configured copilot", model.Config{Providers: providers}},
		{"configured copilot with CodexBar", model.Config{EnableCodexBar: true, Providers: providers}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := New(tc.cfg)
			if tc.name == "configured copilot with CodexBar" {
				reg = r
			}
			candidates := registryCandidates(r, func(c model.Capability) bool { return c.Provider == "copilot" })
			if len(candidates) != 1 {
				t.Fatalf("G-030: expected exactly one copilot candidate, got %+v", candidates)
			}
			copilot := candidates[0]
			if copilot.Status != "disabled" || copilot.Reason != "CLAUDE_ONLY_UNAVAILABLE" {
				t.Fatalf("G-030: copilot must stay disabled (CLAUDE_ONLY_UNAVAILABLE) until Claude-only entitlement and cost policy are verified, got status=%q reason=%q",
					copilot.Status, copilot.Reason)
			}
			if copilot.Model != "" || copilot.Fingerprint != "" {
				t.Fatalf("G-030: copilot config was partially admitted into the registry: %+v", copilot)
			}
			if AllowsTrustedLocal(copilot) {
				t.Fatal("G-030: a disabled copilot candidate must not be trusted-local eligible")
			}
		})
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	attempt := model.Capability{Provider: "copilot", Model: "claude-sonnet-5-5", Status: "available", Mode: "cli"}
	if _, err := reg.Run(ctx, dir, attempt, model.Task{Objective: "probe copilot", Mode: "read_only", DataClass: "synthetic"}, nil, nil); err == nil {
		t.Fatal("G-030: copilot dispatched without verified entitlement and cost policy")
	}
}
