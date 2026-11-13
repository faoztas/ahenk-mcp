// Copyright 2026 Furkan Anıl Öztaş
// SPDX-License-Identifier: Apache-2.0

package providers

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/faoztas/ahenk-mcp/internal/model"
)

func TestCodexCLIArgs_SandboxModes(t *testing.T) {
	// 1. Read-only sandbox mode
	roArgs, err := BuildCodexArgs("gpt-5-sol", "medium", SandboxModeReadOnly, "/tmp/work")
	if err != nil {
		t.Fatalf("unexpected error building read-only args: %v", err)
	}
	roJoined := strings.Join(roArgs, " ")
	if strings.Contains(roJoined, "--full-auto") {
		t.Errorf("read-only mode should not contain --full-auto: %s", roJoined)
	}
	if !strings.Contains(roJoined, "--sandbox read-only") {
		t.Errorf("read-only mode should specify '--sandbox read-only': %s", roJoined)
	}
	if !strings.Contains(roJoined, "-C /tmp/work") {
		t.Errorf("args should include working directory: %s", roJoined)
	}

	// 2. Workspace-write sandbox mode
	rwArgs, err := BuildCodexArgs("gpt-5-sol", "high", SandboxModeWorkspaceWrite, "/tmp/work")
	if err != nil {
		t.Fatalf("unexpected error building workspace-write args: %v", err)
	}
	rwJoined := strings.Join(rwArgs, " ")
	if !strings.Contains(rwJoined, "--full-auto") {
		t.Errorf("workspace-write mode must contain --full-auto: %s", rwJoined)
	}
	if !strings.Contains(rwJoined, "--sandbox workspace-write") {
		t.Errorf("workspace-write mode must specify '--sandbox workspace-write': %s", rwJoined)
	}
	if !strings.Contains(rwJoined, `model_reasoning_effort="high"`) {
		t.Errorf("args should contain reasoning effort: %s", rwJoined)
	}
}

func TestRunCodexCLI_Success(t *testing.T) {
	tempDir := t.TempDir()
	mockBin := filepath.Join(tempDir, "mock_codex")
	script := `#!/bin/sh
echo '{"type":"thread.started","thread_id":"sess-codex-123"}'
echo '{"type":"item.completed","item":{"type":"agent_message","text":"Refactoring completed successfully"}}'
echo '{"type":"turn.completed","usage":{"input_tokens":150,"output_tokens":42}}'
exit 0
`
	if err := os.WriteFile(mockBin, []byte(script), 0755); err != nil {
		t.Fatalf("failed to write mock binary: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	res, err := RunCodexCLI(ctx, tempDir, "gpt-5-sol", "medium", "Refactor network parser", SandboxModeWorkspaceWrite, mockBin)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res == nil {
		t.Fatal("expected non-nil CLIRunResult")
	}

	if res.Output != "Refactoring completed successfully" {
		t.Errorf("expected output 'Refactoring completed successfully', got %q", res.Output)
	}
	if res.InputTokens != 150 || res.OutputTokens != 42 {
		t.Errorf("unexpected tokens: %d in / %d out (expected 150/42)", res.InputTokens, res.OutputTokens)
	}
	if res.Evidence.SessionSHA256 != "sess-codex-123" {
		t.Errorf("expected session sess-codex-123, got %s", res.Evidence.SessionSHA256)
	}
	if !res.Evidence.StartupVerified {
		t.Error("expected StartupVerified to be true")
	}
	if !res.Evidence.TerminalVerified {
		t.Error("expected TerminalVerified to be true")
	}
	if len(res.RawEvents) != 3 {
		t.Errorf("expected 3 raw events, got %d", len(res.RawEvents))
	}
}

func TestRunCodexCLI_RateLimitError(t *testing.T) {
	tempDir := t.TempDir()
	mockBin := filepath.Join(tempDir, "mock_codex_429")
	script := `#!/bin/sh
echo '{"type":"thread.started","thread_id":"sess-codex-429"}'
echo '{"type":"turn.failed","error":{"message":"usage_limit_reached (429): rate limit exceeded"}}'
exit 1
`
	if err := os.WriteFile(mockBin, []byte(script), 0755); err != nil {
		t.Fatalf("failed to write mock binary: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, err := RunCodexCLI(ctx, tempDir, "gpt-5-sol", "high", "Heavy task", SandboxModeWorkspaceWrite, mockBin)
	if err == nil {
		t.Fatal("expected error, got nil")
	}

	var fault *model.Fault
	if !errors.As(err, &fault) || fault.Code != "RATE_LIMIT" {
		if !strings.Contains(err.Error(), "RATE_LIMIT") {
			t.Fatalf("expected RATE_LIMIT error, got %v", err)
		}
	}
}

func TestRunCodexCLI_ProcessGroupTermination_NoOrphans(t *testing.T) {
	tempDir := t.TempDir()
	childPidFile := filepath.Join(tempDir, "child.pid")
	mockBin := filepath.Join(tempDir, "mock_codex_slow")

	// Script spawns a background grandchild sleep and records its PID
	script := `#!/bin/sh
trap 'exit 0' TERM
sleep 60 &
echo $! > "` + childPidFile + `"
echo '{"type":"thread.started","thread_id":"sess-codex-slow"}'
while true; do
  sleep 0.05
done
`
	if err := os.WriteFile(mockBin, []byte(script), 0755); err != nil {
		t.Fatalf("failed to write mock binary: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err := RunCodexCLI(ctx, tempDir, "gpt-5-sol", "low", "Timeout task", SandboxModeWorkspaceWrite, mockBin)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected timeout error, got nil")
	}
	if elapsed > 4*time.Second {
		t.Errorf("cleanup took too long: %v (expected <= 4s)", elapsed)
	}

	// Verify child process was killed and no orphan remains
	pidBytes, readErr := os.ReadFile(childPidFile)
	if readErr == nil {
		childPidStr := strings.TrimSpace(string(pidBytes))
		if childPid, parseErr := strconv.Atoi(childPidStr); parseErr == nil && childPid > 0 {
			// Give OS a moment to reap
			time.Sleep(100 * time.Millisecond)
			killErr := syscall.Kill(childPid, 0)
			if killErr == nil {
				// Clean up manually if test failed to avoid leaving rogue sleep
				_ = syscall.Kill(childPid, syscall.SIGKILL)
				t.Fatalf("orphan child process %d is still alive", childPid)
			}
		}
	}
}

func TestRunCodexCLI_SandboxFlagInvocation(t *testing.T) {
	tempDir := t.TempDir()
	logFile := filepath.Join(tempDir, "args.log")
	mockBin := filepath.Join(tempDir, "mock_codex_args")

	script := `#!/bin/sh
echo "$@" > "` + logFile + `"
echo '{"type":"thread.started","thread_id":"sess-1"}'
echo '{"type":"item.completed","item":{"type":"agent_message","text":"ok"}}'
echo '{"type":"turn.completed","usage":{"input_tokens":1,"output_tokens":1}}'
exit 0
`
	if err := os.WriteFile(mockBin, []byte(script), 0755); err != nil {
		t.Fatalf("failed to write mock binary: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// 1. Run with SandboxModeReadOnly
	_, err := RunCodexCLI(ctx, tempDir, "gpt-5-sol", "low", "Inspect", SandboxModeReadOnly, mockBin)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	logged, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatalf("failed to read log: %v", err)
	}
	roArgs := string(logged)
	if !strings.Contains(roArgs, "--sandbox read-only") {
		t.Errorf("expected '--sandbox read-only' in args: %s", roArgs)
	}
	if strings.Contains(roArgs, "--full-auto") {
		t.Errorf("read-only mode should not contain --full-auto: %s", roArgs)
	}

	// 2. Run with SandboxModeWorkspaceWrite
	_, err = RunCodexCLI(ctx, tempDir, "gpt-5-sol", "low", "Edit", SandboxModeWorkspaceWrite, mockBin)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	logged, err = os.ReadFile(logFile)
	if err != nil {
		t.Fatalf("failed to read log: %v", err)
	}
	rwArgs := string(logged)
	if !strings.Contains(rwArgs, "--sandbox workspace-write") {
		t.Errorf("expected '--sandbox workspace-write' in args: %s", rwArgs)
	}
	if !strings.Contains(rwArgs, "--full-auto") {
		t.Errorf("expected '--full-auto' in args: %s", rwArgs)
	}
}
