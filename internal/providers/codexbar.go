// Copyright 2026 Furkan Anıl Öztaş
// SPDX-License-Identifier: Apache-2.0

package providers

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/faoztas/ahenk-mcp/internal/execution"
	"github.com/faoztas/ahenk-mcp/internal/model"
)

const (
	codexBarPath         = "/Applications/CodexBar.app/Contents/Helpers/CodexBarCLI"
	codexBarExpectedTeam = "Y5PE65HELJ"
	// An unpinned binary still passes every signature and identity check;
	// the attestation marks the missing hash pin for telemetry instead of
	// silently widening trust or blocking live quota reads.
	codexBarUnpinned = "UNPINNED_CODESIGNED"
)

var (
	codesignBin = "/usr/bin/codesign"
	// knownCodexBarHashes pins the audited CodexBar releases. New codesigned
	// builds from the expected team are accepted with codexBarUnpinned.
	knownCodexBarHashes = map[string]string{
		"0c37d6b892f2435266a842d426b06bf8bd9afd0443f5f81ef2a117d66703366e": "0.83.0",
		"c5e1202974689b0dee263d6d00405536dddd8ccdf59ee0cd51c07c3ecb2794ad": "0.84.0",
	}
)

// resolveCodexBarBinary resolves the configured or default CodexBar CLI path,
// following symlinks (for example ~/.local/bin/codexbar) to the app helper.
func resolveCodexBarBinary(customPath string) (string, error) {
	target := customPath
	if target == "" {
		target = codexBarPath
		if _, err := os.Stat(target); err != nil {
			if home, herr := os.UserHomeDir(); herr == nil {
				if link := filepath.Join(home, ".local", "bin", "codexbar"); func() bool {
					_, serr := os.Stat(link)
					return serr == nil
				}() {
					target = link
				}
			}
		}
	}
	resolved, err := filepath.EvalSymlinks(target)
	if err != nil {
		return "", err
	}
	if st, err := os.Stat(resolved); err != nil || st.IsDir() {
		return "", os.ErrNotExist
	}
	return resolved, nil
}

// codexBarTeamID reads the signing team identifier from codesign.
func codexBarTeamID(ctx context.Context, path string) (string, error) {
	cmd := exec.CommandContext(ctx, codesignBin, "-dv", path)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", err
	}
	for _, line := range strings.Split(stderr.String(), "\n") {
		if team, ok := strings.CutPrefix(strings.TrimSpace(line), "TeamIdentifier="); ok &&
			team != "" && team != "not set" {
			return team, nil
		}
	}
	return "", errors.New("no team identifier")
}

// verifyCodexBarBinary resolves the CodexBar CLI, verifies its macOS code
// signature and team identifier, and pins its SHA-256 against the allowlist
// or a configured pin. It returns the resolved path, the binary fingerprint
// and the attestation marker ("0.83.0", "0.84.0", "CONFIGURED" or
// "UNPINNED_CODESIGNED").
func verifyCodexBarBinary(ctx context.Context, customPath, customHash string) (string, string, string, error) {
	unverified := func(msg string) (string, string, string, error) {
		return "", "", "", model.Error("CODEXBAR_BINARY_UNVERIFIED", "CODEXBAR_BINARY_UNVERIFIED: "+msg)
	}
	resolved, err := resolveCodexBarBinary(customPath)
	if err != nil {
		return unverified("CodexBar CLI not found")
	}
	if err := exec.CommandContext(ctx, codesignBin, "--verify", "--strict", resolved).Run(); err != nil {
		return unverified("CodexBar CLI failed strict codesign verification")
	}
	team, err := codexBarTeamID(ctx, resolved)
	if err != nil {
		return unverified("CodexBar CLI signing identity could not be read")
	}
	if team != codexBarExpectedTeam {
		return unverified("CodexBar CLI was signed by an unexpected team")
	}
	fp, err := Fingerprint(resolved)
	if err != nil {
		return unverified("CodexBar CLI fingerprint failed")
	}
	if version, pinned := knownCodexBarHashes[fp]; pinned {
		return resolved, fp, version, nil
	}
	if customHash != "" {
		if fp == customHash {
			return resolved, fp, "CONFIGURED", nil
		}
		return unverified("CodexBar CLI does not match the configured binary pin")
	}
	return resolved, fp, codexBarUnpinned, nil
}

// ReadCodexBar runs the existing account's documented usage command. Only
// normalized quota windows leave this function.
func ReadCodexBar(ctx context.Context, provider string, agy model.ProviderConfig, expectedAccount, binarySHA256 string) []Quota {
	unknown := func(reason string) []Quota {
		q := UnknownQuota(provider)
		q.Reason = reason
		return []Quota{q}
	}
	cliPath, _, _, err := verifyCodexBarBinary(ctx, "", binarySHA256)
	if err != nil {
		return unknown("CODEXBAR_BINARY_UNVERIFIED")
	}
	source, providerArg := "oauth", "codex"
	agyPath := ""
	switch provider {
	case "codex":
	case "claude", "copilot":
		source, providerArg = "oauth", provider
	case "agy":
		source, providerArg = "cli", "antigravity"
		if _, err := verifyAGY(ctx, agy); err != nil {
			return unknown("AGY_BINARY_UNVERIFIED")
		}
		agyPath = agy.Path
	default:
		return unknown("USAGE_UNSUPPORTED")
	}
	dir, err := os.MkdirTemp("", "ahenk-codexbar-")
	if err != nil {
		return unknown("USAGE_UNAVAILABLE")
	}
	defer os.RemoveAll(dir)
	cmd := exec.Command(cliPath, "usage", "--provider", providerArg, "--source", source, "--format", "json", "--json-only", "--no-credits")
	// The explicit path disables CodexBar's ambient AGY search. An empty
	// override means CodexBar must not launch AGY for the Codex query.
	cmd.Env = []string{"ANTIGRAVITY_CLI_PATH=" + agyPath}
	stdout, _, err := runUsage(ctx, cmd, dir)
	if agyPath != "" {
		checkCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 4*time.Second)
		defer cancel()
		if _, err := verifyAGY(checkCtx, agy); err != nil {
			return unknown("AGY_BINARY_UNVERIFIED")
		}
	}
	if err != nil || ctx.Err() != nil {
		return unknown("USAGE_UNAVAILABLE")
	}
	rows, err := decodeCodexBar(bytes.NewReader(stdout), providerArg, source, expectedAccount, time.Now().UTC())
	if err != nil {
		return unknown("USAGE_UNAVAILABLE")
	}
	for i := range rows {
		if rows[i].State == "UNKNOWN" {
			rows[i].Reason = "USAGE_UNAVAILABLE"
		}
	}
	return rows
}

// runUsage uses the native account store through the vendor executable. It
// starts without an interactive terminal and blocks browser launch via PATH.
func runUsage(ctx context.Context, cmd *exec.Cmd, dir string) ([]byte, []byte, error) {
	launcher := filepath.Join(dir, "open")
	if err := os.Symlink("/usr/bin/false", launcher); err != nil {
		return nil, nil, err
	}
	cmd.Dir = dir
	cmd.Env = vendorUsageEnv(cmd.Env, dir)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.WaitDelay = 2 * time.Second
	stdout, stderr := &execution.Limited{Limit: 1 << 20}, &execution.Limited{Limit: 32 << 10}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, nil, err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if execution.StopProcessGroup(cmd.Process.Pid) != nil {
			return nil, nil, execution.ErrCleanup
		}
		return stdout.Bytes(), stderr.Bytes(), err
	case <-ctx.Done():
		if execution.StopProcessGroup(cmd.Process.Pid) != nil {
			return nil, nil, execution.ErrCleanup
		}
		<-done
		return nil, nil, ctx.Err()
	}
}

// Only the existing native user context is inherited. API keys and ADC/Vertex
// overrides in the controller environment cannot select an alternate route.
func vendorUsageEnv(env []string, dir string) []string {
	launcher := filepath.Join(dir, "open")
	clean := []string{
		"PATH=" + dir + ":/usr/bin:/bin:/usr/sbin:/sbin",
		"LC_ALL=C",
		"TMPDIR=" + dir,
		"BROWSER=" + launcher,
		"AGY_CLI_DISABLE_AUTO_UPDATE=true",
	}
	for _, e := range env {
		if strings.HasPrefix(e, "ANTIGRAVITY_CLI_PATH=") {
			clean = append(clean, e)
		}
	}
	for _, key := range []string{"HOME", "USER", "LOGNAME"} {
		if value, ok := os.LookupEnv(key); ok {
			clean = append(clean, key+"="+value)
		}
	}
	return clean
}
