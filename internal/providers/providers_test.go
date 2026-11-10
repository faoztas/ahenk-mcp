// Copyright 2026 Furkan Anıl Öztaş
// SPDX-License-Identifier: Apache-2.0

package providers

import (
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
	if err != nil || len(rows) != 2 || rows[0].Bucket != "secondary" || rows[1].Bucket != "extra:model-specific" ||
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
