// Copyright 2026 Furkan Anıl Öztaş
// SPDX-License-Identifier: Apache-2.0

package providers

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
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
	if slices.Contains(args, "-p") || strings.Contains(joined, "Refactor parsing logic") {
		t.Errorf("prompt should not be passed in CLI args (use stdin instead): %s", joined)
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

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
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

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
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

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
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

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
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

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
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

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
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

func TestRunClaudeCLI_StdinPromptPassed(t *testing.T) {
	tempDir := t.TempDir()
	stdinLog := filepath.Join(tempDir, "stdin.log")
	mockBin := filepath.Join(tempDir, "mock_claude_stdin")
	script := `#!/bin/sh
cat > "` + stdinLog + `"
cat << 'EOF'
{
  "session_id": "sess-stdin-1",
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

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	testPrompt := "Secure prompt passed via stdin to avoid ARG_MAX and ps snooping"
	_, err := RunClaudeCLI(ctx, tempDir, "claude-3-7-sonnet", testPrompt, SandboxModeWorkspaceWrite, mockBin)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	content, err := os.ReadFile(stdinLog)
	if err != nil {
		t.Fatalf("failed to read stdin log: %v", err)
	}
	if string(content) != testPrompt {
		t.Errorf("expected prompt in stdin, got: %q", string(content))
	}
}

func TestRunClaudeCLI_NonZeroExitWithResultReturnsError(t *testing.T) {
	tempDir := t.TempDir()
	mockBin := filepath.Join(tempDir, "mock_claude_fail_result")
	script := `#!/bin/sh
cat << 'EOF'
{
  "session_id": "sess-claude-fail",
  "result": "partial output before non-zero exit",
  "subtype": "success",
  "usage": {
    "input_tokens": 10,
    "output_tokens": 5
  }
}
EOF
echo "unexpected subprocess failure" >&2
exit 1
`
	if err := os.WriteFile(mockBin, []byte(script), 0755); err != nil {
		t.Fatalf("failed to write mock binary: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	_, err := RunClaudeCLI(ctx, tempDir, "claude-3-7-sonnet", "Task", SandboxModeWorkspaceWrite, mockBin)
	if err == nil {
		t.Fatal("expected error on non-zero exit even with non-empty JSON result, got nil")
	}

	var fault *model.Fault
	if !errors.As(err, &fault) || fault.Code != "EXECUTION_FAILED" {
		t.Fatalf("expected EXECUTION_FAILED fault, got %v", err)
	}
}

func TestRunClaudeCLI_OversizedStdoutBounded(t *testing.T) {
	tempDir := t.TempDir()
	mockBin := filepath.Join(tempDir, "mock_claude_oversized")
	script := `#!/bin/sh
printf '{"session_id":"sess-big","subtype":"success","result":"'
dd if=/dev/zero bs=65536 count=130 2>/dev/null | tr '\0' 'a'
printf '"}\n'
exit 0
`
	if err := os.WriteFile(mockBin, []byte(script), 0755); err != nil {
		t.Fatalf("failed to write mock binary: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	_, err := RunClaudeCLI(ctx, tempDir, "claude-3-7-sonnet", "Big output", SandboxModeReadOnly, mockBin)
	if err == nil {
		t.Fatal("expected INVALID_OUTPUT error for >8 MiB stdout, got nil")
	}

	var fault *model.Fault
	if !errors.As(err, &fault) || fault.Code != "INVALID_OUTPUT" {
		t.Fatalf("expected INVALID_OUTPUT fault, got %v", err)
	}
}

func TestRunClaudeCLI_PropagatesChildDepth(t *testing.T) {
	t.Setenv("AHENK_CHILD_SESSION", "1")
	t.Setenv("AHENK_CHILD_DEPTH", "1")
	t.Setenv("AHENK_MAX_DEPTH", "3")

	tempDir := t.TempDir()
	envLog := filepath.Join(tempDir, "env.log")
	mockBin := filepath.Join(tempDir, "mock_claude_depth")
	script := `#!/bin/sh
printf "AHENK_CHILD_DEPTH=%s\nAHENK_MAX_DEPTH=%s\n" "$AHENK_CHILD_DEPTH" "$AHENK_MAX_DEPTH" > "` + envLog + `"
cat << 'EOF'
{
  "session_id": "sess-claude-depth",
  "result": "ok",
  "subtype": "success",
  "usage": {
    "input_tokens": 5,
    "output_tokens": 5
  }
}
EOF
exit 0
`
	if err := os.WriteFile(mockBin, []byte(script), 0755); err != nil {
		t.Fatalf("failed to write mock binary: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	if _, err := RunClaudeCLI(ctx, tempDir, "claude-3-7-sonnet", "Depth test", SandboxModeReadOnly, mockBin); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	got, err := os.ReadFile(envLog)
	if err != nil {
		t.Fatalf("failed to read env log: %v", err)
	}
	want := "AHENK_CHILD_DEPTH=2\nAHENK_MAX_DEPTH=3\n"
	if string(got) != want {
		t.Fatalf("child env mismatch:\ngot:\n%s\nwant:\n%s", string(got), want)
	}
}

func TestRunClaudeCLI_EmptyStdoutNonZeroExitClassifiesStderr(t *testing.T) {
	cases := []struct {
		name     string
		stderr   string
		wantCode string
	}{
		{
			name:     "stderr rate limit returns RATE_LIMIT",
			stderr:   "429 rate_limit exceeded",
			wantCode: "RATE_LIMIT",
		},
		{
			name:     "stderr auth error returns AUTH_REQUIRED",
			stderr:   "401 unauthorized: invalid credentials",
			wantCode: "AUTH_REQUIRED",
		},
		{
			name:     "stderr crash returns EXECUTION_FAILED",
			stderr:   "fatal crash: segmentation fault",
			wantCode: "EXECUTION_FAILED",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tempDir := t.TempDir()
			mockBin := filepath.Join(tempDir, "mock_claude_stderr")
			script := "#!/bin/sh\necho \"" + tc.stderr + "\" >&2\nexit 1\n"
			if err := os.WriteFile(mockBin, []byte(script), 0755); err != nil {
				t.Fatalf("failed to write mock binary: %v", err)
			}

			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()

			_, err := RunClaudeCLI(ctx, tempDir, "claude-3-7-sonnet", "Task", SandboxModeReadOnly, mockBin)
			if err == nil {
				t.Fatal("expected error, got nil")
			}

			var fault *model.Fault
			if !errors.As(err, &fault) || fault.Code != tc.wantCode {
				t.Fatalf("expected %s fault, got %v", tc.wantCode, err)
			}
		})
	}
}

func TestRunClaudeCLI_NonZeroExitEmptyStderrWithErrorJSONResult(t *testing.T) {
	tempDir := t.TempDir()
	mockBin := filepath.Join(tempDir, "mock_claude_err_result")
	script := `#!/bin/sh
cat << 'EOF'
{
  "is_error": true,
  "result": "tool execution rejected by policy"
}
EOF
exit 1
`
	if err := os.WriteFile(mockBin, []byte(script), 0755); err != nil {
		t.Fatalf("failed to write mock binary: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	_, err := RunClaudeCLI(ctx, tempDir, "claude-3-7-sonnet", "Task", SandboxModeReadOnly, mockBin)
	if err == nil {
		t.Fatal("expected error, got nil")
	}

	var fault *model.Fault
	if !errors.As(err, &fault) || fault.Code != "EXECUTION_FAILED" {
		t.Fatalf("expected EXECUTION_FAILED fault, got %v", err)
	}
	if !strings.Contains(fault.Message, "tool execution rejected by policy") {
		t.Fatalf("expected parsed result message in fault, got %q", fault.Message)
	}
}

func TestClaudeCLIArgs_WithEffort(t *testing.T) {
	cases := []struct {
		name     string
		effort   string
		wantFlag string
	}{
		{"empty omits effort", "", ""},
		{"low passed", "low", "--effort low"},
		{"medium passed", "medium", "--effort medium"},
		{"high passed", "high", "--effort high"},
		{"max passed", "max", "--effort max"},
		{"xhigh maps to max", "xhigh", "--effort max"},
		{"trimmed and lowercased", "  HIGH ", "--effort high"},
		{"injection rejected", `high --malicious`, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			args := BuildClaudeArgsWithEffort("claude-opus-5-5", tc.effort, SandboxModeReadOnly)
			joined := strings.Join(args, " ")
			if tc.wantFlag == "" {
				if strings.Contains(joined, "--effort") || strings.Contains(joined, "malicious") {
					t.Errorf("unexpected --effort or injection in args: %s", joined)
				}
			} else if !strings.Contains(joined, tc.wantFlag) {
				t.Errorf("expected %q in args, got: %s", tc.wantFlag, joined)
			}
		})
	}
}
