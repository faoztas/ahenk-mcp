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

func TestClaudeCLIArgs(t *testing.T) {
	// 1. Workspace-write / patch mode
	args := BuildClaudeArgs("claude-3-7-sonnet", "Refactor parsing logic", SandboxModeWorkspaceWrite)
	joined := strings.Join(args, " ")

	if !strings.Contains(joined, "--permission-mode acceptEdits") {
		t.Errorf("patch mode should have acceptEdits permission mode: %s", joined)
	}
	if !strings.Contains(joined, "--allowedTools Read,Edit,Write,Grep,Glob") {
		t.Errorf("patch mode should allow safe tools: %s", joined)
	}
	if !strings.Contains(joined, "--disallowedTools Bash,WebFetch,WebSearch,NotebookEdit") {
		t.Errorf("patch mode should disallow dangerous tools: %s", joined)
	}
	if !strings.Contains(joined, "--strict-mcp-config") {
		t.Errorf("strict mcp config must be enforced: %s", joined)
	}
	if !strings.Contains(joined, `--mcp-config {"mcpServers":{}}`) {
		t.Errorf("mcp config should isolate headless servers: %s", joined)
	}
	if !strings.Contains(joined, "--model claude-3-7-sonnet") {
		t.Errorf("model flag missing: %s", joined)
	}
	if !strings.Contains(joined, "--output-format json") {
		t.Errorf("output-format flag missing: %s", joined)
	}
	if !strings.Contains(joined, "-p Refactor parsing logic") {
		t.Errorf("prompt flag missing or incorrect: %s", joined)
	}

	// 2. Read-only mode
	roArgs := BuildClaudeArgs("claude-3-7-sonnet", "Analyze architecture", SandboxModeReadOnly)
	roJoined := strings.Join(roArgs, " ")
	if !strings.Contains(roJoined, "--permission-mode plan") {
		t.Errorf("read-only mode should specify plan permission mode: %s", roJoined)
	}
	if !strings.Contains(roJoined, "--tools Read,Grep,Glob") {
		t.Errorf("read-only mode should specify read tools only: %s", roJoined)
	}
	if strings.Contains(roJoined, "--allowedTools") {
		t.Errorf("read-only mode should not contain --allowedTools: %s", roJoined)
	}
	if strings.Contains(roJoined, "--disallowedTools") {
		t.Errorf("read-only mode should not contain --disallowedTools: %s", roJoined)
	}
	if !strings.Contains(roJoined, "--strict-mcp-config") {
		t.Errorf("read-only mode must enforce strict mcp config: %s", roJoined)
	}

	// 3. Backward-compatible helper claudeArgs
	compatArgs, err := claudeArgs("claude-3-7-sonnet", "patch")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	compatJoined := strings.Join(compatArgs, " ")
	if !strings.Contains(compatJoined, "--permission-mode acceptEdits") {
		t.Errorf("compatArgs patch mode missing acceptEdits: %s", compatJoined)
	}
}

func TestRunClaudeCLI_Success(t *testing.T) {
	tempDir := t.TempDir()
	mockBin := filepath.Join(tempDir, "mock_claude")
	script := `#!/bin/sh
cat << 'EOF'
{
  "session_id": "sess-claude-999",
  "result": "Refactored parsing logic with zero regressions",
  "subtype": "success",
  "usage": {
    "input_tokens": 310,
    "output_tokens": 95
  }
}
EOF
exit 0
`
	if err := os.WriteFile(mockBin, []byte(script), 0755); err != nil {
		t.Fatalf("failed to write mock binary: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	res, err := RunClaudeCLI(ctx, tempDir, "claude-3-7-sonnet", "Refactor parsing logic", SandboxModeWorkspaceWrite, mockBin)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res == nil {
		t.Fatal("expected non-nil CLIRunResult")
	}

	if res.Output != "Refactored parsing logic with zero regressions" {
		t.Errorf("expected output 'Refactored parsing logic with zero regressions', got %q", res.Output)
	}
	if res.InputTokens != 310 || res.OutputTokens != 95 {
		t.Errorf("unexpected tokens: %d in / %d out (expected 310/95)", res.InputTokens, res.OutputTokens)
	}
	if res.Evidence.SessionSHA256 != "sess-claude-999" {
		t.Errorf("expected session sess-claude-999, got %s", res.Evidence.SessionSHA256)
	}
	if res.Evidence.InputTokens != 310 || res.Evidence.OutputTokens != 95 {
		t.Errorf("unexpected evidence tokens: %d in / %d out", res.Evidence.InputTokens, res.Evidence.OutputTokens)
	}
	if !res.Evidence.StartupVerified {
		t.Error("expected StartupVerified to be true")
	}
	if !res.Evidence.TerminalVerified {
		t.Error("expected TerminalVerified to be true")
	}
}

func TestRunClaudeCLI_RateLimitError(t *testing.T) {
	tempDir := t.TempDir()
	mockBin := filepath.Join(tempDir, "mock_claude_429")
	script := `#!/bin/sh
cat << 'EOF'
{
  "is_error": true,
  "subtype": "rate_limit_error",
  "result": "rate_limit_error: Organization quota exceeded (HTTP 429)"
}
EOF
exit 1
`
	if err := os.WriteFile(mockBin, []byte(script), 0755); err != nil {
		t.Fatalf("failed to write mock binary: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, err := RunClaudeCLI(ctx, tempDir, "claude-3-7-sonnet", "High throughput check", SandboxModeWorkspaceWrite, mockBin)
	if err == nil {
		t.Fatal("expected error, got nil")
	}

	var fault *model.Fault
	if !errors.As(err, &fault) || fault.Code != "RATE_LIMIT" {
		t.Fatalf("expected RATE_LIMIT fault, got %v", err)
	}
}

func TestRunClaudeCLI_OverloadedError_HTTP529(t *testing.T) {
	tempDir := t.TempDir()
	mockBin := filepath.Join(tempDir, "mock_claude_529")
	script := `#!/bin/sh
cat << 'EOF'
{
  "is_error": true,
  "subtype": "overloaded_error",
  "result": "overloaded_error: Claude is currently overloaded (HTTP 529)"
}
EOF
exit 1
`
	if err := os.WriteFile(mockBin, []byte(script), 0755); err != nil {
		t.Fatalf("failed to write mock binary: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, err := RunClaudeCLI(ctx, tempDir, "claude-3-7-sonnet", "Check status", SandboxModeWorkspaceWrite, mockBin)
	if err == nil {
		t.Fatal("expected error, got nil")
	}

	var fault *model.Fault
	if !errors.As(err, &fault) || fault.Code != "RATE_LIMIT" {
		t.Fatalf("expected RATE_LIMIT fault for overloaded 529, got %v", err)
	}
}

func TestRunClaudeCLI_AuthenticationError(t *testing.T) {
	tempDir := t.TempDir()
	mockBin := filepath.Join(tempDir, "mock_claude_auth")
	script := `#!/bin/sh
cat << 'EOF'
{
  "is_error": true,
  "subtype": "authentication_error",
  "result": "authentication_error: Invalid API key provided (HTTP 401)"
}
EOF
exit 1
`
	if err := os.WriteFile(mockBin, []byte(script), 0755); err != nil {
		t.Fatalf("failed to write mock binary: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, err := RunClaudeCLI(ctx, tempDir, "claude-3-7-sonnet", "Task", SandboxModeWorkspaceWrite, mockBin)
	if err == nil {
		t.Fatal("expected error, got nil")
	}

	var fault *model.Fault
	if !errors.As(err, &fault) || fault.Code != "AUTH_REQUIRED" {
		t.Fatalf("expected AUTH_REQUIRED fault, got %v", err)
	}
}

func TestRunClaudeCLI_InsufficientCredits(t *testing.T) {
	tempDir := t.TempDir()
	mockBin := filepath.Join(tempDir, "mock_claude_credits")
	script := `#!/bin/sh
cat << 'EOF'
{
  "is_error": true,
  "result": "Your credit balance is too low to access Claude API"
}
EOF
exit 1
`
	if err := os.WriteFile(mockBin, []byte(script), 0755); err != nil {
		t.Fatalf("failed to write mock binary: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, err := RunClaudeCLI(ctx, tempDir, "claude-3-7-sonnet", "Task", SandboxModeWorkspaceWrite, mockBin)
	if err == nil {
		t.Fatal("expected error, got nil")
	}

	var fault *model.Fault
	if !errors.As(err, &fault) || fault.Code != "AUTH_REQUIRED" {
		t.Fatalf("expected AUTH_REQUIRED fault for low credits, got %v", err)
	}
}

func TestRunClaudeCLI_ProcessGroupTermination_NoOrphans(t *testing.T) {
	tempDir := t.TempDir()
	childPidFile := filepath.Join(tempDir, "child.pid")
	mockBin := filepath.Join(tempDir, "mock_claude_slow")

	// Script spawns a background grandchild sleep and records its PID
	script := `#!/bin/sh
trap 'exit 0' TERM
sleep 60 &
echo $! > "` + childPidFile + `"
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
	_, err := RunClaudeCLI(ctx, tempDir, "claude-3-7-sonnet", "Stalled task", SandboxModeWorkspaceWrite, mockBin)
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

func TestRunClaudeCLI_SandboxFlagInvocation(t *testing.T) {
	tempDir := t.TempDir()
	logFile := filepath.Join(tempDir, "args.log")
	mockBin := filepath.Join(tempDir, "mock_claude_args")

	script := `#!/bin/sh
echo "$@" > "` + logFile + `"
cat << 'EOF'
{
  "session_id": "sess-sandbox-1",
  "result": "success",
  "subtype": "success",
  "usage": {
    "input_tokens": 10,
    "output_tokens": 10
  }
}
EOF
exit 0
`
	if err := os.WriteFile(mockBin, []byte(script), 0755); err != nil {
		t.Fatalf("failed to write mock binary: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// 1. Run with SandboxModeReadOnly
	_, err := RunClaudeCLI(ctx, tempDir, "claude-3-7-sonnet", "Inspect code", SandboxModeReadOnly, mockBin)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	logged, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatalf("failed to read log: %v", err)
	}
	roArgs := string(logged)
	if !strings.Contains(roArgs, "--permission-mode plan") {
		t.Errorf("expected '--permission-mode plan' in args: %s", roArgs)
	}
	if !strings.Contains(roArgs, "--tools Read,Grep,Glob") {
		t.Errorf("expected '--tools Read,Grep,Glob' in args: %s", roArgs)
	}
	if !strings.Contains(roArgs, "--strict-mcp-config") {
		t.Errorf("expected '--strict-mcp-config' in args: %s", roArgs)
	}
	if !strings.Contains(roArgs, `{"mcpServers":{}}`) {
		t.Errorf("expected mcp config empty dict in args: %s", roArgs)
	}

	// 2. Run with SandboxModeWorkspaceWrite
	_, err = RunClaudeCLI(ctx, tempDir, "claude-3-7-sonnet", "Edit code", SandboxModeWorkspaceWrite, mockBin)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	logged, err = os.ReadFile(logFile)
	if err != nil {
		t.Fatalf("failed to read log: %v", err)
	}
	rwArgs := string(logged)
	if !strings.Contains(rwArgs, "--permission-mode acceptEdits") {
		t.Errorf("expected '--permission-mode acceptEdits' in args: %s", rwArgs)
	}
	if !strings.Contains(rwArgs, "--allowedTools Read,Edit,Write,Grep,Glob") {
		t.Errorf("expected '--allowedTools Read,Edit,Write,Grep,Glob' in args: %s", rwArgs)
	}
	if !strings.Contains(rwArgs, "--disallowedTools Bash,WebFetch,WebSearch,NotebookEdit") {
		t.Errorf("expected '--disallowedTools Bash,WebFetch,WebSearch,NotebookEdit' in args: %s", rwArgs)
	}
}
