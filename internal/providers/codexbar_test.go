// Copyright 2026 Furkan Anıl Öztaş
// SPDX-License-Identifier: Apache-2.0

package providers

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/faoztas/ahenk-mcp/internal/model"
)

// fakeCodesign installs a stub codesign binary and returns its path. The stub
// reports the given team identifier and honors FAKE_CODESIGN_VERIFY_OK.
func fakeCodesign(t *testing.T, team string, verifyOK bool) string {
	t.Helper()
	script := filepath.Join(t.TempDir(), "codesign")
	body := "#!/bin/sh\n" +
		"if [ \"$1\" = \"--verify\" ]; then\n" +
		"  if [ -n \"$FAKE_CODESIGN_VERIFY_OK\" ]; then exit 0; fi\n" +
		"  exit 1\n" +
		"fi\n" +
		"if [ \"$1\" = \"-dv\" ]; then\n" +
		"  echo \"TeamIdentifier=$FAKE_CODESIGN_TEAM\" >&2\n" +
		"  exit 0\n" +
		"fi\n" +
		"exit 1\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FAKE_CODESIGN_TEAM", team)
	if verifyOK {
		t.Setenv("FAKE_CODESIGN_VERIFY_OK", "1")
	}
	return script
}

func stubCodexBarEnv(t *testing.T, codesign string, hashes map[string]string) {
	t.Helper()
	oldCodesign, oldHashes := codesignBin, knownCodexBarHashes
	codesignBin, knownCodexBarHashes = codesign, hashes
	t.Cleanup(func() { codesignBin, knownCodexBarHashes = oldCodesign, oldHashes })
}

func unverifiedFault(t *testing.T, err error) bool {
	t.Helper()
	var fault *model.Fault
	return err != nil && errors.As(err, &fault) && fault.Code == "CODEXBAR_BINARY_UNVERIFIED"
}

func TestResolveCodexBarBinaryFollowsSymlinks(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "CodexBarCLI")
	if err := os.WriteFile(real, []byte("binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "codexbar")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	if canonical, err := filepath.EvalSymlinks(real); err != nil {
		t.Fatal(err)
	} else {
		real = canonical
	}
	resolved, err := resolveCodexBarBinary(link)
	if err != nil || filepath.Clean(resolved) != filepath.Clean(real) {
		t.Fatalf("symlink was not resolved to the app helper: %q %v", resolved, err)
	}
	if _, err := resolveCodexBarBinary(filepath.Join(dir, "missing")); err == nil {
		t.Fatal("missing binary resolved")
	}
}

func TestVerifyCodexBarBinaryAllowlist(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	bin := filepath.Join(dir, "CodexBarCLI")
	if err := os.WriteFile(bin, []byte("codexbar-0.84.0-fixture"), 0o755); err != nil {
		t.Fatal(err)
	}
	if resolved, err := filepath.EvalSymlinks(bin); err != nil {
		t.Fatal(err)
	} else {
		bin = resolved
	}
	fp, err := Fingerprint(bin)
	if err != nil {
		t.Fatal(err)
	}
	stubCodexBarEnv(t, fakeCodesign(t, "Y5PE65HELJ", true), map[string]string{
		"0c37d6b892f2435266a842d426b06bf8bd9afd0443f5f81ef2a117d66703366e": "0.83.0", fp: "0.84.0",
	})
	path, hash, attestation, err := verifyCodexBarBinary(ctx, bin, "")
	if err != nil || path != bin || hash != fp || attestation != "0.84.0" {
		t.Fatalf("pinned 0.84.0 binary rejected: %q %q %q %v", path, hash, attestation, err)
	}
	// A codesign-verified binary with an unknown hash is accepted with an
	// explicit unpinned attestation instead of locking quota reads.
	knownCodexBarHashes = map[string]string{"0c37d6b892f2435266a842d426b06bf8bd9afd0443f5f81ef2a117d66703366e": "0.83.0"}
	_, _, attestation, err = verifyCodexBarBinary(ctx, bin, "")
	if err != nil || attestation != "UNPINNED_CODESIGNED" {
		t.Fatalf("codesigned new version was not accepted with telemetry: %q %v", attestation, err)
	}
	// A configured pin must match the actual binary hash.
	if _, _, _, err = verifyCodexBarBinary(ctx, bin, strings.Repeat("a", 64)); !unverifiedFault(t, err) {
		t.Fatalf("configured pin mismatch accepted: %v", err)
	}
	if _, _, attestation, err = verifyCodexBarBinary(ctx, bin, fp); err != nil || attestation != "CONFIGURED" {
		t.Fatalf("configured pin match failed: %q %v", attestation, err)
	}
}

func TestVerifyCodexBarBinaryRejectsUnverified(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	bin := filepath.Join(dir, "CodexBarCLI")
	if err := os.WriteFile(bin, []byte("fixture"), 0o755); err != nil {
		t.Fatal(err)
	}
	if resolved, err := filepath.EvalSymlinks(bin); err != nil {
		t.Fatal(err)
	} else {
		bin = resolved
	}
	if _, _, _, err := verifyCodexBarBinary(ctx, bin, ""); !unverifiedFault(t, err) {
		t.Fatalf("failed strict verification accepted: %v", err)
	}
	stubCodexBarEnv(t, fakeCodesign(t, "WRONGTEAM0000", true), map[string]string{})
	if _, _, _, err := verifyCodexBarBinary(ctx, bin, ""); !unverifiedFault(t, err) {
		t.Fatalf("foreign signing team accepted: %v", err)
	}
	if _, _, _, err := verifyCodexBarBinary(ctx, filepath.Join(dir, "missing"), ""); !unverifiedFault(t, err) {
		t.Fatalf("missing binary accepted: %v", err)
	}
}

func TestDecodeCodexBarFourBars(t *testing.T) {
	rawCodex := `{"provider":"codex","source":"oauth","usage":{"updatedAt":"2026-11-12T12:00:00Z","primary":{"usedPercent":25.0,"windowMinutes":300},"secondary":{"usedPercent":10.0,"windowMinutes":10080}}}`
	now := time.Date(2026, 11, 12, 12, 1, 0, 0, time.UTC)
	quotas, err := DecodeCodexBar(strings.NewReader(rawCodex), "codex", now)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(quotas) != 2 {
		t.Fatalf("expected 2 quotas, got %d", len(quotas))
	}
	if quotas[0].Bucket != BarCodexBurst || quotas[0].WindowMinutes != 300 || *quotas[0].Remaining != 75.0 {
		t.Errorf("unexpected primary quota: %+v", quotas[0])
	}
	if quotas[1].Bucket != BarCodexWeekly || quotas[1].WindowMinutes != 10080 || *quotas[1].Remaining != 90.0 {
		t.Errorf("unexpected secondary quota: %+v", quotas[1])
	}

	rawAGY := `{"provider":"antigravity","source":"cli","usage":{"updatedAt":"2026-11-12T12:00:00Z","extraRateWindows":[{"id":"gemini-5h","window":{"usedPercent":15.0,"windowMinutes":300}},{"id":"3p-weekly","window":{"usedPercent":80.0,"windowMinutes":7200}}]}}`
	agyQuotas, err := DecodeCodexBar(strings.NewReader(rawAGY), "antigravity", now)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(agyQuotas) != 2 {
		t.Fatalf("expected 2 AGY quotas, got %d", len(agyQuotas))
	}
	if agyQuotas[0].Bucket != BarAgyGemini || *agyQuotas[0].Remaining != 85.0 {
		t.Errorf("unexpected agy gemini quota: %+v", agyQuotas[0])
	}
	if agyQuotas[1].Bucket != BarAgyOpus || *agyQuotas[1].Remaining != 20.0 {
		t.Errorf("unexpected agy opus quota: %+v", agyQuotas[1])
	}
}

func TestCodexBarSecurityInvariants(t *testing.T) {
	// 1. Verify environment sanitization excludes all API keys
	env := vendorUsageEnv([]string{
		"OPENAI_API_KEY=sk-leaked",
		"ANTHROPIC_API_KEY=sk-ant-leaked",
		"GEMINI_API_KEY=leaked",
		"HOME=/Users/tester",
		"USER=tester",
	}, t.TempDir())

	for _, e := range env {
		if strings.Contains(e, "OPENAI_API_KEY") || strings.Contains(e, "ANTHROPIC_API_KEY") || strings.Contains(e, "GEMINI_API_KEY") {
			t.Errorf("API key leaked in vendorUsageEnv: %s", e)
		}
		if strings.HasPrefix(e, "BROWSER=") && !strings.HasSuffix(e, "/open") {
			t.Errorf("BROWSER override not set properly: %s", e)
		}
	}

	// 2. Mock binary verification rejection
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	fakeBin := filepath.Join(t.TempDir(), "fake-codexbar")
	if err := os.WriteFile(fakeBin, []byte("#!/bin/sh\necho '{}'"), 0755); err != nil {
		t.Fatal(err)
	}
	_, _, _, err := verifyCodexBarBinary(ctx, fakeBin, "")
	if err == nil || !strings.Contains(err.Error(), "CODEXBAR_BINARY_UNVERIFIED") {
		t.Errorf("expected CODEXBAR_BINARY_UNVERIFIED, got %v", err)
	}
}

func TestClaudeQuotaAffectsRoutingOnlyAfterBoundSource(t *testing.T) {
	now := time.Date(2026, 11, 12, 9, 45, 0, 0, time.UTC)
	fixture := codexBar084Fixture(t, "codexbar_084_claude.json")
	const email = "fixture@example.invalid"
	account := model.Hash([]byte(email))
	other := model.Hash([]byte("other@example.invalid"))
	decode := func(raw, source, digest string) ([]Quota, error) {
		return decodeCodexBar(strings.NewReader(raw), "claude", source, digest, now)
	}

	t.Run("exact OAuth binding", func(t *testing.T) {
		rows, err := decode(strings.Replace(fixture, email, " Fixture@EXAMPLE.INVALID ", 1), "oauth", strings.ToUpper(account))
		if err != nil || len(rows) != 2 {
			t.Fatalf("expected two Claude quota windows, got %d: %v", len(rows), err)
		}
		windows := []struct {
			bucket    string
			minutes   int
			remaining float64
			reset     time.Time
		}{
			{"primary", 300, 77.5, time.Date(2026, 11, 12, 14, 0, 0, 0, time.UTC)},
			{"secondary", 10080, 91.7, time.Date(2026, 11, 16, 0, 0, 0, 0, time.UTC)},
		}
		for i, want := range windows {
			q := rows[i]
			if q.Provider != "claude" || q.Family != "" || q.Source != "oauth" || q.RequestedSource != "oauth" ||
				q.AccountMatch != "MATCHED" || q.Confidence != "exact" || q.Unit != "percent" ||
				q.Bucket != want.bucket || q.WindowMinutes != want.minutes || q.State != "KNOWN" ||
				q.Remaining == nil || *q.Remaining != want.remaining || q.ResetAt == nil || !q.ResetAt.Equal(want.reset) ||
				!q.ObservedAt.Equal(now.Add(-5*time.Minute)) {
				t.Fatalf("unexpected normalized quota metadata for %s: %+v", want.bucket, q)
			}
			encoded, err := json.Marshal(q)
			if err != nil || strings.Contains(strings.ToLower(string(encoded)), email) ||
				strings.Contains(strings.ToLower(string(encoded)), account) {
				t.Fatal("quota serialization failed or exposed account identity")
			}
		}

		// Consumers must recheck freshness on cached rows.
		if q := rows[0].At(now.Add(10*time.Minute + time.Nanosecond)); q.Remaining != nil || q.State != "STALE" {
			t.Fatal("cached Claude quota survived its maximum age")
		}
		nearReset := strings.Replace(fixture, "2026-11-12T14:00:00Z", "2026-11-12T09:46:00Z", 1)
		rows, err = decode(nearReset, "oauth", account)
		if err != nil || len(rows) != 2 || rows[0].Remaining == nil {
			t.Fatal("expected usable quota before reset")
		}
		if q := rows[0].At(now.Add(time.Minute)); q.Remaining != nil || q.State != "STALE" {
			t.Fatal("cached Claude quota survived its reset")
		}
	})

	t.Run("verified near exhaustion", func(t *testing.T) {
		for _, tc := range []struct{ used, state string }{{"99.99", "KNOWN"}, {"100", "EXHAUSTED"}} {
			raw := strings.Replace(fixture, `"usedPercent":22.5`, `"usedPercent":`+tc.used, 1)
			rows, err := decode(raw, "oauth", account)
			if err != nil || len(rows) != 2 || rows[0].State != tc.state || rows[0].Remaining == nil {
				t.Fatalf("used %s: want %s with measured balance, got %+v %v", tc.used, tc.state, rows, err)
			}
			if left := *rows[0].Remaining; tc.used == "99.99" && (left <= 0 || left > 0.011) || tc.used == "100" && left != 0 {
				t.Fatalf("used %s: unexpected remaining balance %v", tc.used, left)
			}
		}
	})

	t.Run("unproved export", func(t *testing.T) {
		for _, tc := range []struct{ name, old, replacement, source, digest string }{
			{"unmapped config", "", "", "oauth", ""},
			{"unattested export", "", "", "", account},
			{"mismatch", "", "", "oauth", other},
			{"missing identity", email, "", "oauth", account},
			{"identity provider mismatch", `"providerID":"claude"`, `"providerID":"codex"`, "oauth", account},
			{"source fallback", `"source":"oauth"`, `"source":"web"`, "oauth", account},
			{"source alias", `"source":"oauth"`, `"source":"OAuth"`, "oauth", account},
			{"untrusted requested source", `"source":"oauth"`, `"source":"cli"`, "cli", account},
			{"inexact evidence", `"dataConfidence":"exact"`, `"dataConfidence":"estimated"`, "oauth", account},
			{"stale", "2026-11-12T09:40:00Z", "2026-11-12T09:29:59Z", "oauth", account},
			{"future", "2026-11-12T09:40:00Z", "2026-11-12T09:45:01Z", "oauth", account},
			{"alias collision", `"secondary":`, `"extraRateWindows":[{"id":"primary","window":{"usedPercent":99,"windowMinutes":300}}],"secondary":`, "oauth", account},
		} {
			t.Run(tc.name, func(t *testing.T) {
				raw := strings.Replace(fixture, tc.old, tc.replacement, 1)
				rows, err := decode(raw, tc.source, tc.digest)
				if err != nil || len(rows) == 0 {
					t.Fatalf("expected UNKNOWN quota: %v", err)
				}
				for _, q := range rows {
					if q.Provider != "claude" || q.State != "UNKNOWN" || q.Remaining != nil {
						t.Fatalf("unproved Claude quota was admitted: %+v", q)
					}
				}
			})
		}
	})

	t.Run("unproved primary window", func(t *testing.T) {
		for _, tc := range []struct{ name, old, replacement string }{
			{"expired reset", "2026-11-12T14:00:00Z", "2026-11-12T09:45:00Z"},
			{"absent reset", `,"resetsAt":"2026-11-12T14:00:00Z"`, ""},
			{"implausible reset", "2026-11-12T14:00:00Z", "2026-11-12T15:00:00Z"},
			{"wrong duration", `"windowMinutes":300`, `"windowMinutes":10080`},
			{"missing percentage", `"usedPercent":22.5`, `"usedPercent":null`},
			{"out of range percentage", `"usedPercent":22.5`, `"usedPercent":101`},
			{"placeholder", `"usedPercent":22.5`, `"usedPercent":22.5,"isSyntheticPlaceholder":true`},
			{"unknown usage", `"usedPercent":22.5`, `"usedPercent":22.5,"usageKnown":false`},
		} {
			t.Run(tc.name, func(t *testing.T) {
				rows, err := decode(strings.Replace(fixture, tc.old, tc.replacement, 1), "oauth", account)
				if err != nil || len(rows) != 2 || rows[0].State != "UNKNOWN" || rows[0].Remaining != nil ||
					rows[1].Bucket != "secondary" || rows[1].State != "KNOWN" || rows[1].Remaining == nil {
					t.Fatalf("expected UNKNOWN primary and intact secondary: %+v %v", rows, err)
				}
			})
		}
	})

	t.Run("malformed export", func(t *testing.T) {
		for _, tc := range []struct{ name, raw string }{
			{"broken JSON", "["},
			{"invalid reset", strings.Replace(fixture, "2026-11-12T14:00:00Z", "invalid", 1)},
			{"duplicate source", strings.Replace(fixture, `"source":"oauth"`, `"source":"web","source":"oauth"`, 1)},
			{"duplicate primary", strings.Replace(fixture, `"primary":`, `"primary":null,"primary":`, 1)},
		} {
			t.Run(tc.name, func(t *testing.T) {
				rows, err := decode(tc.raw, "oauth", account)
				if len(rows) != 0 || err == nil || err.Error() != "invalid CodexBar export" {
					t.Fatal("expected a sanitized decoder error and no quota")
				}
			})
		}
	})
}
