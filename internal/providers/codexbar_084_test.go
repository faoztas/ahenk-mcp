package providers

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// codexBar084Fixture reads one of the recorded 0.84.0 dashboard exports.
func codexBar084Fixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestDecodeCodexBar084AntigravityWindows(t *testing.T) {
	now := time.Date(2026, 11, 12, 9, 45, 0, 0, time.UTC)
	rows, err := decodeCodexBar(strings.NewReader(codexBar084Fixture(t, "codexbar_084_antigravity.json")), "antigravity", "cli", "", now)
	if err != nil || len(rows) != 5 {
		t.Fatalf("wrong antigravity window count: %d %v", len(rows), err)
	}
	want := map[string]struct {
		family  string
		minutes int
		remain  float64
	}{
		"gemini-5h":               {"gemini", 300, 7.2},
		"gemini-weekly":           {"gemini", 10080, 4.9},
		"3p-5h":                   {"claude_gpt", 300, 100},
		"3p-weekly":               {"claude_gpt", 10080, 53.9},
		"gemini-argon-wtu-weekly": {"gemini_argon", 10080, 47.9},
	}
	for _, row := range rows {
		if row.Bucket == BarAgyOpus || row.Bucket == BarAgyOpus5h {
			if row.WindowMinutes == 300 {
				if row.Family != "claude_gpt" || row.Remaining == nil || *row.Remaining != 100 {
					t.Fatalf("wrong 3p-5h mapping: %+v", row)
				}
				continue
			} else if row.WindowMinutes == 10080 {
				if row.Family != "claude_gpt" || row.Remaining == nil || *row.Remaining != 53.9 {
					t.Fatalf("wrong 3p-weekly mapping: %+v", row)
				}
				continue
			}
		}
		bucketKey := row.Bucket
		if bucketKey == BarAgyGemini {
			bucketKey = "gemini-5h"
		}
		w, ok := want[bucketKey]
		if !ok {
			t.Fatalf("unexpected bucket %q", row.Bucket)
		}
		if row.Family != w.family || row.WindowMinutes != w.minutes || row.Remaining == nil || *row.Remaining != w.remain {
			t.Fatalf("wrong window mapping for %q: family=%s minutes=%d remain=%v", row.Bucket, row.Family, row.WindowMinutes, row.Remaining)
		}
	}
}

func TestDecodeCodexBar084CodexWindows(t *testing.T) {
	now := time.Date(2026, 11, 12, 9, 45, 0, 0, time.UTC)
	rows, err := decodeCodexBar(strings.NewReader(codexbar084codex(t)), "codex", "oauth", "", now)
	if err != nil || len(rows) != 2 ||
		(rows[0].Bucket != "primary" && rows[0].Bucket != BarCodexBurst) || rows[0].WindowMinutes != 300 || *rows[0].Remaining != 87.6 ||
		(rows[1].Bucket != "secondary" && rows[1].Bucket != BarCodexWeekly) || rows[1].WindowMinutes != 10080 || *rows[1].Remaining != 68.3 {
		t.Fatalf("wrong codex windows: %+v %v", rows, err)
	}
}

func codexbar084codex(t *testing.T) string { return codexBar084Fixture(t, "codexbar_084_codex.json") }

func TestDecodeCodexBar084CopilotAndClaude(t *testing.T) {
	now := time.Date(2026, 11, 12, 9, 45, 0, 0, time.UTC)
	// Copilot: the monthly premium window is fully consumed.
	rows, err := decodeCodexBar(strings.NewReader(codexBar084Fixture(t, "codexbar_084_copilot.json")), "copilot", "oauth", "", now)
	if err != nil || len(rows) != 1 || rows[0].State != "EXHAUSTED" || *rows[0].Remaining != 0 || rows[0].Provider != "copilot" {
		t.Fatalf("wrong copilot windows: %+v %v", rows, err)
	}
	// Claude: session and weekly windows are decoded like Codex.
	rows, err = decodeCodexBar(strings.NewReader(codexBar084Fixture(t, "codexbar_084_claude.json")), "claude", "oauth", "", now)
	if err != nil || len(rows) != 2 ||
		rows[0].Bucket != "primary" || *rows[0].Remaining != 77.5 ||
		rows[1].Bucket != "secondary" || *rows[1].Remaining != 91.7 || rows[1].Provider != "claude" {
		t.Fatalf("wrong claude windows: %+v %v", rows, err)
	}
}

func TestFPTierClassification(t *testing.T) {
	for _, tc := range []struct {
		provider, model, tier string
	}{
		{"opencode", "opencode/glm-5.3-flash-free", "free"},
		{"agy", "gemini-3.8-flash-medium", "cheap"},
		{"agy", "claude-sonnet-4-6", "balanced"},
		{"agy", "gemini-4-argon", "hard"},
		{"codex", "gpt-5.4", "hard"},
		{"copilot", "claude-gpt-5", "cheap"},
	} {
		if got := FPTier(tc.provider, tc.model); got != tc.tier {
			t.Fatalf("FPTier(%s, %s) = %q, want %q", tc.provider, tc.model, got, tc.tier)
		}
	}
}
