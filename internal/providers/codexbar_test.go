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
