// Copyright 2026 Furkan Anıl Öztaş
// SPDX-License-Identifier: Apache-2.0

package providers

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"

	"github.com/faoztas/ahenk-mcp/internal/execution"
	"github.com/faoztas/ahenk-mcp/internal/model"
)

const (
	codexBarPath        = "/Applications/CodexBar.app/Contents/Helpers/CodexBarCLI"
	codexBarFingerprint = "0c37d6b892f2435266a842d426b06bf8bd9afd0443f5f81ef2a117d66703366e"
)

// ReadCodexBar runs the existing account's documented usage command. Only
// normalized quota windows leave this function.
func ReadCodexBar(ctx context.Context, provider string, agy model.ProviderConfig, expectedAccount string) []Quota {
	unknown := func(reason string) []Quota {
		q := UnknownQuota(provider)
		q.Reason = reason
		return []Quota{q}
	}
	fp, err := Fingerprint(codexBarPath)
	if err != nil || fp != codexBarFingerprint ||
		exec.CommandContext(ctx, "/usr/bin/codesign", "--verify", "--strict", codexBarPath).Run() != nil {
		return unknown("CODEXBAR_BINARY_UNVERIFIED")
	}
	source, providerArg := "oauth", "codex"
	agyPath := ""
	switch provider {
	case "codex":
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
	cmd := exec.Command(codexBarPath, "usage", "--provider", providerArg, "--source", source, "--format", "json", "--json-only", "--no-credits")
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
	env = append(env, "PATH="+dir+":/usr/bin:/bin:/usr/sbin:/sbin", "LC_ALL=C", "TMPDIR="+dir,
		"BROWSER="+launcher, "AGY_CLI_DISABLE_AUTO_UPDATE=true")
	for _, key := range []string{"HOME", "USER", "LOGNAME"} {
		if value, ok := os.LookupEnv(key); ok {
			env = append(env, key+"="+value)
		}
	}
	return env
}
