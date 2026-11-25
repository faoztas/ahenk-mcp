// Copyright 2026 Furkan Anıl Öztaş
// SPDX-License-Identifier: Apache-2.0

package providers

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/faoztas/ahenk-mcp/internal/model"
)

func TestBudgetKeepsHardDeadline(t *testing.T) {
	now := time.Now()
	task := model.Task{Priority: "background", Mode: "read_only"}
	c := model.Capability{Provider: "opencode", Speed: "slow"}
	estimate := Estimate(c, task)
	if estimate != 420*time.Second {
		t.Fatal(estimate)
	}
	if _, ok := Budget(now, now.Add(120*time.Second), task, estimate); ok {
		t.Fatal("slow candidate admitted past hard deadline")
	}
	if b, ok := Budget(now, now.Add(600*time.Second), task, estimate); !ok || b <= 120*time.Second || b > 580*time.Second {
		t.Fatal(b, ok)
	}
}

func TestFixtureImplementationBudget(t *testing.T) {
	now := time.Now()
	c := model.Capability{Speed: "fixture"}
	for _, tc := range []struct {
		name               string
		task               model.Task
		deadline, estimate time.Duration
	}{
		{"plain patch", model.Task{Mode: "patch"}, 30 * time.Second, 5 * time.Second},
		{"worktree patch", model.Task{Mode: "patch", ExecutionProfile: "implementation"}, 60 * time.Second, 25 * time.Second},
		{"checked worktree patch", model.Task{Mode: "patch", ExecutionProfile: "implementation",
			Acceptance: []model.Check{{CheckID: "go-check"}}}, 120 * time.Second, 90 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			estimate := Estimate(c, tc.task)
			budget, fits := Budget(now, now.Add(tc.deadline), tc.task, estimate)
			if estimate != tc.estimate || !fits || budget < tc.estimate || budget > tc.deadline-20*time.Second {
				t.Fatalf("estimate %v, budget %v, fits %v", estimate, budget, fits)
			}
		})
	}
}

func TestDecodeCodexBarSeparatesWindowsAndUnknown(t *testing.T) {
	now := time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)
	input := "[{\"provider\":\"antigravity\",\"account\":\"redacted@example.invalid\",\"usage\":{\"updatedAt\":\"2026-11-09T11:59:00Z\",\"primary\":{\"usedPercent\":100,\"usageKnown\":false},\"secondary\":{\"usedPercent\":75},\"tertiary\":{\"usedPercent\":100}}}]"
	rows, err := decodeCodexBar(strings.NewReader(input), "antigravity", "cli", "", now)
	if err != nil || len(rows) != 3 {
		t.Fatal(rows, err)
	}
	if rows[0].Remaining != nil || rows[0].State != "UNKNOWN" || rows[1].Remaining == nil ||
		*rows[1].Remaining != 25 || rows[2].State != "EXHAUSTED" || rows[2].Provider != "agy" {
		t.Fatal(rows)
	}
	rows, err = decodeCodexBar(strings.NewReader(input), "antigravity", "cli", "", now.Add(time.Hour))
	if err != nil || rows[1].Remaining != nil || rows[1].State != "STALE" {
		t.Fatal(rows, err)
	}
}

func TestLiveQuotaShapeKeepsAGYFamiliesSeparate(t *testing.T) {
	now := time.Date(2026, 11, 9, 18, 30, 0, 0, time.UTC)
	payload := `[{"provider":"antigravity","usage":{"updatedAt":"2026-11-09T18:25:00Z",
 "primary":{"windowMinutes":300,"usedPercent":100},
 "extraRateWindows":[
 {"id":"antigravity-quota-summary-gemini-5h","window":{"windowMinutes":300,"usedPercent":100,"resetsAt":"2026-11-09T23:00:00Z"}},
 {"id":"antigravity-quota-summary-gemini-weekly","window":{"windowMinutes":10080,"usedPercent":0}},
 {"id":"antigravity-quota-summary-3p-5h","window":{"windowMinutes":300,"usedPercent":0}},
 {"id":"antigravity-quota-summary-3p-weekly","window":{"windowMinutes":10080,"usedPercent":10}},
 {"id":"antigravity-quota-summary-gemini-argon-wtu-weekly","window":{"windowMinutes":10080,"usedPercent":30}},
 {"id":"antigravity-quota-summary-foreign","window":{"windowMinutes":10080,"usedPercent":100}}]}}]`
	rows, err := decodeCodexBar(strings.NewReader(payload), "antigravity", "cli", "", now)
	if err != nil || len(rows) != 6 ||
		rows[0].Family != "gemini" || rows[0].State != "EXHAUSTED" || rows[0].ResetAt == nil ||
		rows[2].Family != "claude_gpt" || rows[4].Family != "gemini_argon" || rows[5].Family != "" {
		t.Fatalf("wrong bucket normalization: %+v, %v", rows, err)
	}
	if got := rows[0].At(now.Add(time.Hour)); got.State != "STALE" || got.Remaining != nil {
		t.Fatalf("stale exhausted quota remained eligible for routing: %+v", got)
	}
}

func TestAGYUsageReportDoesNotAttestCreditSetting(t *testing.T) {
	now := time.Date(2026, 11, 9, 18, 30, 0, 0, time.UTC)
	raw := []byte(`{"status":"SUCCESS","command":{"name":"usage","data":{"groups":[{"buckets":[
 {"id":"gemini-5h","remaining_fraction":0.42,"reset_time":"2026-11-09T23:00:00Z"},
 {"id":"gemini-argon-wtu-weekly","remaining_fraction":1}
 ]}]}}}`)
	rows, ok := decodeAGYUsage(raw, now)
	if !ok || len(rows) != 2 || rows[0].Remaining == nil || *rows[0].Remaining != 42 ||
		rows[0].Family != "gemini" || rows[1].Family != "gemini_argon" ||
		agyUnverifiedCost().Status != "ZERO_COST_UNVERIFIED" {
		t.Fatalf("unexpected usage or cost proof: %+v %v", rows, ok)
	}
}

func TestCodexBarHonorsNamedKnownAndActualSource(t *testing.T) {
	now := time.Date(2026, 11, 9, 20, 8, 0, 0, time.UTC)
	agy := `{"provider":"antigravity","source":"app","usage":{"updatedAt":"2026-11-09T20:07:00Z",
 "identity":{"providerID":"antigravity","accountEmail":"fixture@example.invalid"},
 "extraRateWindows":[
 {"id":"antigravity-quota-summary-gemini-5h","usageKnown":false,"window":{"usedPercent":0,"windowMinutes":300,"resetsAt":"2026-11-10T01:00:00Z"}},
 {"id":"antigravity-quota-summary-3p-weekly","window":{"usedPercent":0,"windowMinutes":10080,"isSyntheticPlaceholder":true}},
 {"id":"antigravity-quota-summary-gemini-weekly","window":{"usedPercent":0,"windowMinutes":10080}}
 ]}}`
	rows, err := decodeCodexBar(strings.NewReader(agy), "antigravity", "cli", "", now)
	if err != nil || len(rows) != 3 || rows[0].State != "UNKNOWN" || rows[0].ResetAt == nil ||
		rows[1].State != "UNKNOWN" || rows[2].State != "KNOWN" || rows[2].Remaining == nil || *rows[2].Remaining != 100 ||
		rows[2].Source != "app" || rows[2].RequestedSource != "cli" || rows[2].AccountMatch != "UNKNOWN" {
		t.Fatalf("wrong AGY evidence: %+v %v", rows, err)
	}
	offline, err := DecodeCodexBar(strings.NewReader(agy), "antigravity", now)
	if err != nil || offline[2].Remaining != nil {
		t.Fatalf("an unversioned export inferred measurement: %+v %v", offline, err)
	}
	codex := `{"provider":"codex","source":"oauth","usage":{"updatedAt":"2026-11-09T20:07:00Z","dataConfidence":"exact",
 "secondary":{"usedPercent":2,"windowMinutes":10080}}}`
	rows, err = decodeCodexBar(strings.NewReader(codex), "codex", "oauth", "", now)
	if err != nil || len(rows) != 1 || rows[0].Remaining == nil || *rows[0].Remaining != 98 ||
		rows[0].Confidence != "exact" || rows[0].AccountMatch != "UNKNOWN" {
		t.Fatalf("wrong Codex evidence: %+v %v", rows, err)
	}
	rows, err = decodeCodexBar(strings.NewReader(strings.Replace(agy, `"providerID":"antigravity"`, `"providerID":"codex"`, 1)), "antigravity", "cli", "", now)
	if err != nil || len(rows) != 1 || rows[0].State != "UNKNOWN" {
		t.Fatalf("mismatched provider identity: %+v %v", rows, err)
	}
}

func TestCodexBarBindsOnlyVerifiedSourceAndAccount(t *testing.T) {
	now := time.Date(2026, 11, 9, 20, 8, 0, 0, time.UTC)
	account := model.Hash([]byte("fixture@example.invalid"))
	other := model.Hash([]byte("different@example.invalid"))
	agy := `{"provider":"antigravity","source":"app","usage":{"updatedAt":"2026-11-09T20:07:00Z",
 "identity":{"providerID":"antigravity","accountEmail":" Fixture@EXAMPLE.INVALID "},
 "extraRateWindows":[{"id":"antigravity-quota-summary-3p-weekly","window":{"usedPercent":0,"windowMinutes":10080}}]}}`
	codex := `{"provider":"codex","source":"oauth","usage":{"updatedAt":"2026-11-09T20:07:00Z",
 "identity":{"providerID":"codex","accountEmail":"fixture@example.invalid"},
 "secondary":{"usedPercent":2,"windowMinutes":10080}}}`
	for _, tc := range []struct{ name, raw, provider, source, digest, want string }{
		{"app", agy, "antigravity", "cli", account, "MATCHED"},
		{"wrong account", agy, "antigravity", "cli", other, "MISMATCH"},
		{"missing identity", strings.ReplaceAll(agy, `"accountEmail":" Fixture@EXAMPLE.INVALID "`, `"accountEmail":null`), "antigravity", "cli", account, "UNKNOWN"},
		{"untrusted source", strings.ReplaceAll(agy, `"source":"app"`, `"source":"web"`), "antigravity", "cli", account, "UNKNOWN"},
		{"offline export", agy, "antigravity", "", account, "UNKNOWN"},
		{"controller", codex, "codex", "oauth", account, "MATCHED"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rows, err := decodeCodexBar(strings.NewReader(tc.raw), tc.provider, tc.source, tc.digest, now)
			if err != nil || len(rows) != 1 || rows[0].AccountMatch != tc.want {
				t.Fatalf("wrong account binding: %+v %v", rows, err)
			}
		})
	}
}

func TestCodexExtraWindowDoesNotCreateAccountSession(t *testing.T) {
	now := time.Date(2026, 11, 10, 16, 0, 0, 0, time.UTC)
	input := `[{"provider":"codex","source":"oauth","usage":{"updatedAt":"2026-11-10T15:59:00Z",
	"identity":{"providerID":"codex","accountEmail":"synthetic@example.invalid"},
	"secondary":{"usedPercent":20,"windowMinutes":10080},
	"extraRateWindows":[{"id":"model-specific","window":{"usedPercent":100,"windowMinutes":300}}]}}]`
	rows, err := decodeCodexBar(strings.NewReader(input), "codex", "oauth", model.Hash([]byte("synthetic@example.invalid")), now)
	if err != nil || len(rows) != 2 || (rows[0].Bucket != "secondary" && rows[0].Bucket != BarCodexWeekly) || rows[1].Bucket != "extra:model-specific" ||
		rows[1].State != "EXHAUSTED" || rows[1].AccountMatch != "MATCHED" {
		t.Fatalf("wrong account/model window split: %+v %v", rows, err)
	}
}

func TestAGYNativeWeeklyFloorAppliesToExactFamily(t *testing.T) {
	gemini, argon := 16.0, 70.0
	rows := []Quota{
		{Family: "gemini", Bucket: "gemini-weekly", WindowMinutes: 10080, Remaining: &gemini},
		{Family: "gemini_argon", Bucket: "gemini-argon-wtu-weekly", WindowMinutes: 10080, Remaining: &argon},
	}
	if got := agyWeeklyReserve(rows, "gemini", 17); got == nil || got.Bucket != "gemini-weekly" ||
		agyWeeklyReserve(rows, "gemini_argon", 17) != nil || agyWeeklyReserve(rows, "gemini", 0) != nil {
		t.Fatal("weekly floor was applied to the wrong worker")
	}
}

func TestDefaultSpeedAdmitsDefaultReadOnlyBudget(t *testing.T) {
	tempDir := t.TempDir()
	codexPath := createDummyExecutable(t, tempDir, "codex")
	claudePath := createDummyExecutable(t, tempDir, "claude")

	now := time.Now().UTC()
	cfg := model.Config{
		EnableFixture: true,
		Providers: []model.ProviderConfig{
			{ID: "agy", Model: "gemini-3.8-flash-medium"},
			{ID: "opencode", Model: "opencode/glm-5.3-flash-free", OpenCodeTransport: model.OpenCodeNativeReadAsk},
			{ID: "codex", Path: codexPath, Model: "custom-codex-default"},
			{ID: "codex", Path: codexPath, Model: "custom-codex-slow", Speed: "slow"},
			{ID: "claude", Path: claudePath, Model: "custom-claude-default"},
			{ID: "claude", Path: claudePath, Model: "custom-claude-slow", Speed: "slow"},
		},
		ModelAudit: &model.ModelInventory{
			Fixture:    model.ModelAuditFixture,
			ObservedAt: now,
			Models: []model.ModelAudit{
				{
					Provider:  "agy",
					Model:     "gemini-4-argon-low",
					Actual:    "gemini-4-argon-low",
					Binary:    agy153Fingerprint,
					Inference: "PASS",
					Cost:      "SUBSCRIPTION_PRECHECK",
					Interface: "headless_json",
					Terminal:  true,
					LatencyMS: 1200,
				},
				{
					Provider:  "agy",
					Model:     "gemini-4-argon-high",
					Actual:    "gemini-4-argon-high",
					Binary:    agy153Fingerprint,
					Inference: "PASS",
					Cost:      "SUBSCRIPTION_PRECHECK",
					Interface: "headless_json",
					Terminal:  true,
					LatencyMS: 35000,
				},
			},
		},
	}

	// Enable audited AGY models by matching synthetic config on a separate registry check
	// and also verify New(cfg) directly on agy/opencode/codex/claude.
	reg := New(cfg)
	readOnly := model.Task{Mode: "read_only"}
	deadline := now.Add(120 * time.Second)

	findCap := func(provider, modelID string) model.Capability {
		t.Helper()
		for _, c := range reg.Candidates {
			if c.Provider == provider && (modelID == "" || c.Model == modelID) {
				return c
			}
		}
		t.Fatalf("missing candidate %s/%s in %+v", provider, modelID, reg.Candidates)
		return model.Capability{}
	}

	for _, tc := range []struct {
		provider string
		modelID  string
		wantFast bool
	}{
		{"agy", "gemini-3.8-flash-medium", true},
		{"opencode", "opencode/glm-5.3-flash-free", true},
		{"codex", "custom-codex-default", true},
		{"claude", "custom-claude-default", true},
		{"codex", "custom-codex-slow", false},
		{"claude", "custom-claude-slow", false},
	} {
		c := findCap(tc.provider, tc.modelID)
		est := Estimate(c, readOnly)
		_, ok := Budget(now, deadline, readOnly, est)
		if tc.wantFast {
			if c.Speed != "fast" {
				t.Errorf("%s/%s: expected Speed=fast, got %q", tc.provider, tc.modelID, c.Speed)
			}
			if est != 60*time.Second || !ok {
				t.Errorf("%s/%s: expected fast 60s estimate admitted under 120s deadline, got est=%v ok=%v", tc.provider, tc.modelID, est, ok)
			}
		} else {
			if c.Speed != "slow" {
				t.Errorf("%s/%s: expected Speed=slow, got %q", tc.provider, tc.modelID, c.Speed)
			}
			if est != 240*time.Second || ok {
				t.Errorf("%s/%s: expected slow 240s estimate rejected under 120s deadline, got est=%v ok=%v", tc.provider, tc.modelID, est, ok)
			}
		}
	}

	fixCap := findCap("fixture-a", "synthetic")
	if fixCap.Speed != "fixture" || Estimate(fixCap, readOnly) != 5*time.Second {
		t.Errorf("fixture speed changed unexpectedly: %+v", fixCap)
	}

	// Verify audited models via addAuditedModels honor fast default and high-latency slow hint.
	auditReg := Registry{agy: model.ProviderConfig{EnableSynthetic: true, ExpectedBinarySHA256: agy153Fingerprint}}
	auditReg.addAuditedModels(cfg.ModelAudit, now)
	if len(auditReg.Candidates) != 2 {
		t.Fatalf("expected 2 audited candidates, got %d", len(auditReg.Candidates))
	}
	fastAudit := auditReg.Candidates[0]
	slowAudit := auditReg.Candidates[1]
	if fastAudit.Speed != "fast" || Estimate(fastAudit, readOnly) != 60*time.Second {
		t.Errorf("fast audited model: got speed=%q est=%v", fastAudit.Speed, Estimate(fastAudit, readOnly))
	}
	if _, ok := Budget(now, deadline, readOnly, Estimate(fastAudit, readOnly)); !ok {
		t.Errorf("fast audited model was rejected by Budget under 120s deadline")
	}
	if slowAudit.Speed != "slow" || Estimate(slowAudit, readOnly) != 240*time.Second {
		t.Errorf("slow audited model: got speed=%q est=%v", slowAudit.Speed, Estimate(slowAudit, readOnly))
	}
}

func TestRegistryMultiEntryCLIProviders(t *testing.T) {
	tempDir := t.TempDir()
	codexPath := createDummyExecutable(t, tempDir, "codex")
	claudePath := createDummyExecutable(t, tempDir, "claude")

	cfg := model.Config{
		Providers: []model.ProviderConfig{
			{ID: "codex", Path: codexPath, Model: "codex-alpha", Speed: "slow"},
			{ID: "codex", Path: codexPath, Model: "codex-beta"},
			{ID: "codex", Model: "codex-gamma", Speed: "fast"},
			{ID: "claude", Path: claudePath, Model: "claude-alpha", Speed: "slow"},
			{ID: "claude", Path: claudePath, Model: "claude-beta"},
			{ID: "claude", Model: "claude-gamma", Speed: "fast"},
		},
	}

	reg := New(cfg)
	want := map[string]string{
		"codex/codex-alpha":   "slow",
		"codex/codex-beta":    "fast",
		"codex/codex-gamma":   "fast",
		"claude/claude-alpha": "slow",
		"claude/claude-beta":  "fast",
		"claude/claude-gamma": "fast",
	}
	got := map[string]model.Capability{}
	for _, c := range reg.Candidates {
		if c.Provider == "codex" || c.Provider == "claude" {
			got[c.Provider+"/"+c.Model] = c
		}
	}
	for key, wantSpeed := range want {
		c, ok := got[key]
		if !ok {
			t.Fatalf("missing multi-entry model %s", key)
		}
		if c.Status != "available" {
			t.Errorf("%s: expected available, got %s (reason: %s)", key, c.Status, c.Reason)
		}
		if c.Speed != wantSpeed {
			t.Errorf("%s: expected Speed=%q, got %q", key, wantSpeed, c.Speed)
		}
		if !AllowsTrustedLocal(c) {
			t.Errorf("%s: expected AllowsTrustedLocal=true", key)
		}
	}
}

func TestRegistryTargetEffortPropagationCLI(t *testing.T) {
	tempDir := t.TempDir()
	realTemp, err := filepath.EvalSymlinks(tempDir)
	if err != nil {
		realTemp = tempDir
	}
	workDir := filepath.Join(realTemp, "worktree")
	if err := os.MkdirAll(workDir, 0755); err != nil {
		t.Fatalf("failed to create worktree: %v", err)
	}

	codexLog := filepath.Join(realTemp, "codex_args.log")
	mockCodex := filepath.Join(realTemp, "mock_codex_effort")
	codexScript := `#!/bin/sh
echo "$@" >> "` + codexLog + `"
echo '{"type":"thread.started","thread_id":"sess-codex-effort"}'
echo '{"type":"item.completed","item":{"type":"agent_message","text":"codex ok"}}'
echo '{"type":"turn.completed","usage":{"input_tokens":10,"output_tokens":5}}'
exit 0
`
	if err := os.WriteFile(mockCodex, []byte(codexScript), 0755); err != nil {
		t.Fatalf("failed to write mock codex: %v", err)
	}

	claudeLog := filepath.Join(realTemp, "claude_args.log")
	mockClaude := filepath.Join(realTemp, "mock_claude_effort")
	claudeScript := `#!/bin/sh
echo "$@" >> "` + claudeLog + `"
cat << 'EOF'
{
  "session_id": "sess-claude-effort",
  "result": "claude ok",
  "subtype": "success",
  "usage": {"input_tokens": 10, "output_tokens": 5}
}
EOF
exit 0
`
	if err := os.WriteFile(mockClaude, []byte(claudeScript), 0755); err != nil {
		t.Fatalf("failed to write mock claude: %v", err)
	}

	reg := New(model.Config{
		Providers: []model.ProviderConfig{
			{ID: "codex", Path: mockCodex, Model: "gpt-6.1-sol"},
			{ID: "claude", Path: mockClaude, Model: "claude-opus-5-5"},
		},
	})

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

	// 1. Codex TargetEffort="max" -> model_reasoning_effort="xhigh"
	if _, err := reg.Run(ctx, realTemp, codexCap, model.Task{Objective: "Check max effort", Mode: "read_only", TargetEffort: "max"}, nil, nil); err != nil {
		t.Fatalf("codex Run failed: %v", err)
	}
	codexArgsBytes, err := os.ReadFile(codexLog)
	if err != nil {
		t.Fatalf("failed to read codex log: %v", err)
	}
	if !strings.Contains(string(codexArgsBytes), `model_reasoning_effort="xhigh"`) {
		t.Errorf("expected model_reasoning_effort=\"xhigh\" for TargetEffort=max, got: %s", string(codexArgsBytes))
	}

	// 2. Claude Run with TargetEffort="high" and RunTrustedLocal with TargetEffort="max"
	if _, err := reg.Run(ctx, realTemp, claudeCap, model.Task{Objective: "Check claude effort", Mode: "read_only", TargetEffort: "high"}, nil, nil); err != nil {
		t.Fatalf("claude Run failed: %v", err)
	}
	if _, err := reg.RunTrustedLocal(ctx, realTemp, workDir, claudeCap, model.Task{
		Objective:        "Patch with max effort",
		Mode:             "patch",
		ExecutionProfile: model.TrustedLocalProfile,
		DataClass:        "synthetic",
		TargetEffort:     "max",
	}, []string{"mod.go"}); err != nil {
		t.Fatalf("claude RunTrustedLocal failed: %v", err)
	}
	claudeArgsBytes, err := os.ReadFile(claudeLog)
	if err != nil {
		t.Fatalf("failed to read claude log: %v", err)
	}
	claudeLogged := string(claudeArgsBytes)
	if !strings.Contains(claudeLogged, "--effort high") {
		t.Errorf("expected '--effort high' in claude Run args, got: %s", claudeLogged)
	}
	if !strings.Contains(claudeLogged, "--effort max") {
		t.Errorf("expected '--effort max' in claude RunTrustedLocal args, got: %s", claudeLogged)
	}
}

// A CLI can accept custom models even when their effort settings are unknown.
func TestProfileCustomModelDoesNotReceiveEffortFlag(t *testing.T) {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "mock")
	log := path + ".args"
	body := "#!/bin/sh\nprintf '%s\\n' \"$@\" > '" + log + "'\nprintf '%s\\n' '{\"type\":\"thread.started\",\"thread_id\":\"t\"}' '{\"type\":\"item.completed\",\"item\":{\"type\":\"agent_message\",\"text\":\"ok\"}}' '{\"type\":\"turn.completed\",\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}'\n"
	if err := os.WriteFile(path, []byte(body), 0700); err != nil {
		t.Fatal(err)
	}
	reg := New(model.Config{Providers: []model.ProviderConfig{{ID: "codex", Model: "custom", Path: path}, {ID: "claude", Path: path}}})
	c := model.Capability{Provider: "codex", Model: "custom", Mode: "cli", Status: "available"}
	_, err = reg.Run(context.Background(), dir, c, model.Task{Mode: "read_only", RoutingProfile: "quality", TargetEffort: "max", Objective: "test"}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	args, err := os.ReadFile(log)
	if err != nil || strings.Contains(string(args), "model_reasoning_effort") {
		t.Fatalf("custom model arguments %q error %v", args, err)
	}
}

func TestProfileQuotaBindsConfiguredCLIWorker(t *testing.T) {
	path := filepath.Join(t.TempDir(), "worker")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0700); err != nil {
		t.Fatal(err)
	}
	cfg := model.Config{Providers: []model.ProviderConfig{
		{ID: "codex", Path: path, Model: "gpt-6.1-sol", AttestedAccountSHA256: model.Hash([]byte("model-specific"))},
		{ID: "codex", Path: path, AttestedAccount: "default"},
		{ID: "codex", Path: path, Model: "custom", AttestedAccount: "other", ExpectedBinarySHA256: model.Hash([]byte("different binary"))},
	}}
	seen := map[string]bool{}
	for _, c := range New(cfg).Candidates {
		if c.Provider != "codex" {
			continue
		}
		seen[c.Model] = true
		switch c.Model {
		case "custom":
			if c.AttestedAccount != "" || c.AttestedAccountSHA256 != "" {
				t.Fatal("mismatched binary retained attestation")
			}
		case "gpt-6.1-sol":
			if c.AttestedAccount != "" || c.AttestedAccountSHA256 != cfg.Providers[0].AttestedAccountSHA256 {
				t.Fatal("model-specific worker lost attestation")
			}
		default:
			if c.AttestedAccount != "default" || c.AttestedAccountSHA256 != "" {
				t.Fatal("default worker lost attestation", c.Model)
			}
		}
	}
	if !seen["custom"] || !seen["gpt-6.1-sol"] || !seen["gpt-6-astra"] {
		t.Fatal(seen)
	}
}

func TestAGYFiveDayNativePreflightReserve(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	marker := filepath.Join(dir, "worker-started")
	binary := filepath.Join(dir, "fixture-worker")
	if err := os.WriteFile(binary, []byte("#!/bin/sh\ntouch '"+marker+"'\n"), 0700); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	reset := now.Add(time.Hour)
	for _, left := range []float64{12, 80, 0} {
		rows := []Quota{{Provider: "agy", Bucket: "3p-weekly", Family: "claude_gpt", WindowMinutes: 7200, Source: "agy_usage", AccountMatch: "SAME_NATIVE_CLI_CONTEXT", State: "KNOWN", Remaining: &left, ObservedAt: now, ResetAt: &reset}}
		short := rows[0]
		short.Bucket = "3p-5h"
		short.WindowMinutes = 300
		full := 100.
		short.Remaining = &full
		rows = append(rows, short)
		if fault := checkAGYFamilyQuota(rows, "claude_gpt", 0, now); left == 0 {
			if fault == nil || fault.Code != "QUOTA_EXHAUSTED" {
				t.Fatal("zero admitted")
			}
			continue
		} else if fault != nil {
			t.Fatal(fault)
		}
		if got := agyWeeklyReserve(rows, "claude_gpt", 17); (got != nil) != (left == 12) {
			t.Errorf("native %g reserve %+v", left, got)
		}
		if left == 12 {
			req := agyRequest{WeeklyFloor: 17, Audited: true, Task: model.Task{Mode: "read_only", DataClass: "synthetic"}, Config: model.ProviderConfig{ID: "agy", AttestedAccount: "fixture-account", Model: "claude-fixture", EnableSynthetic: true, Path: binary, ExpectedBinarySHA256: agy153Fingerprint}}
			out, err := runAGYAttemptWithUsage(t.Context(), req, func(context.Context, model.ProviderConfig, string, bool) (AGYProbe, error) {
				return AGYProbe{Admission: agySmokeAllowed, Quotas: rows}, nil
			})
			var fault *model.Fault
			if !errors.As(err, &fault) || fault.Code != "AGY_WEEKLY_RESERVE_PROTECTED" || out.AGY.WeeklyReserve == nil || out.AGY.PromptSent {
				t.Fatal("missing pre-dispatch reserve rejection", err)
			}
			if out.AGY.ReserveAccountSHA256 != "" {
				t.Fatal("unidentified usage sample acquired the configured account")
			}
			if _, err := os.Stat(marker); !os.IsNotExist(err) {
				t.Fatal("blocked worker was started", err)
			}
		}
	}
}
