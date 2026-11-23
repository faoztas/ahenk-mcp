// Copyright 2026 Furkan Anıl Öztaş
// SPDX-License-Identifier: Apache-2.0

package providers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"github.com/faoztas/ahenk-mcp/internal/model"
)

const maxClaudeStdoutBytes = 8 << 20

var allowedClaudeEfforts = map[string]bool{
	"low":    true,
	"medium": true,
	"high":   true,
	"max":    true,
}

func sanitizeClaudeEffort(effort string) string {
	normalized := strings.ToLower(strings.TrimSpace(effort))
	if normalized == "xhigh" {
		return "max"
	}
	if allowedClaudeEfforts[normalized] {
		return normalized
	}
	return ""
}

// BuildClaudeArgs constructs the command-line arguments for the Claude CLI subprocess.
func BuildClaudeArgs(modelID string, _ string, sandbox SandboxMode) []string {
	return BuildClaudeArgsWithEffort(modelID, "", sandbox)
}

// BuildClaudeArgsWithEffort constructs the command-line arguments for the Claude CLI subprocess with optional reasoning effort.
func BuildClaudeArgsWithEffort(modelID, effort string, sandbox SandboxMode) []string {
	args := []string{"--output-format", "json"}
	if modelID != "" {
		args = append(args, "--model", modelID)
	}
	if sanitized := sanitizeClaudeEffort(effort); sanitized != "" {
		args = append(args, "--effort", sanitized)
	}
	if sandbox == SandboxModeReadOnly {
		args = append(args, "--permission-mode", "plan", "--tools", "Read,Grep,Glob")
	} else {
		args = append(args, "--permission-mode", "acceptEdits", "--allowedTools", "Read,Edit,Write,Grep,Glob", "--disallowedTools", "Bash,WebFetch,WebSearch,NotebookEdit")
	}
	args = append(args, "--strict-mcp-config", "--mcp-config", `{"mcpServers":{}}`)
	return args
}

func claudeArgs(modelID, mode string) ([]string, error) {
	sb := SandboxModeWorkspaceWrite
	if mode == "read_only" || mode == "analysis" || mode == "review" {
		sb = SandboxModeReadOnly
	}
	return BuildClaudeArgs(modelID, "", sb), nil
}

func claudeEnv(base []string, tempDir string, currentDepth, maxDepth int) []string {
	return ChildEnv(base, tempDir, currentDepth, maxDepth)
}

// RunClaudeCLI executes the Claude CLI in a subprocess with process group isolation and JSON parsing.
func RunClaudeCLI(ctx context.Context, worktreeDir string, modelID string, prompt string, sandbox SandboxMode, binPath string) (*CLIRunResult, error) {
	return RunClaudeCLIWithEffort(ctx, worktreeDir, modelID, "", prompt, sandbox, binPath)
}

// RunClaudeCLIWithEffort executes the Claude CLI in a subprocess with process group isolation, effort propagation, and JSON parsing.
func RunClaudeCLIWithEffort(ctx context.Context, worktreeDir string, modelID string, effort string, prompt string, sandbox SandboxMode, binPath string) (*CLIRunResult, error) {
	if binPath == "" {
		binPath = "claude"
	}
	args := BuildClaudeArgsWithEffort(modelID, effort, sandbox)

	cmd := exec.Command(binPath, args...)
	if worktreeDir != "" {
		cmd.Dir = worktreeDir
	}
	currentDepth, maxDepth := DepthFromEnv(os.Environ())
	cmd.Env = claudeEnv(os.Environ(), worktreeDir, currentDepth, maxDepth)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	if prompt != "" {
		cmd.Stdin = strings.NewReader(prompt)
	}

	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return nil, model.Error("CLAUDE_LAUNCH_FAILED", err.Error())
	}
	var stderrBuf bytes.Buffer
	cmd.Stderr = &stderrBuf

	if err := cmd.Start(); err != nil {
		return nil, model.Error("CLAUDE_LAUNCH_FAILED", err.Error())
	}

	pgid, err := syscall.Getpgid(cmd.Process.Pid)
	if err != nil {
		pgid = cmd.Process.Pid
	}

	var stdoutBuf bytes.Buffer
	var stdoutOverflow bool
	stdoutDone := make(chan struct{})
	go func() {
		defer close(stdoutDone)
		_, _ = io.Copy(&stdoutBuf, io.LimitReader(stdoutPipe, maxClaudeStdoutBytes+1))
		if stdoutBuf.Len() > maxClaudeStdoutBytes {
			stdoutOverflow = true
			_, _ = io.Copy(io.Discard, stdoutPipe)
		}
	}()

	waitDone := make(chan error, 1)
	go func() {
		<-stdoutDone
		waitDone <- cmd.Wait()
	}()

	select {
	case <-ctx.Done():
		if pgid > 1 {
			_ = syscall.Kill(-pgid, syscall.SIGTERM)
		}
		select {
		case <-waitDone:
		case <-time.After(3 * time.Second):
			if pgid > 1 {
				_ = syscall.Kill(-pgid, syscall.SIGKILL)
			}
			<-waitDone
		}
		return nil, ctx.Err()

	case waitErr := <-waitDone:
		if stdoutOverflow {
			return nil, model.Error("INVALID_OUTPUT", "claude stdout exceeded 8 MiB limit")
		}
		evidence, parseErr := ParseClaudeJSON(stdoutBuf.Bytes())
		if parseErr != nil {
			var fault *model.Fault
			if waitErr == nil || (errors.As(parseErr, &fault) && (fault.Code == "RATE_LIMIT" || fault.Code == "AUTH_REQUIRED")) {
				return nil, parseErr
			}
			fallback := ""
			if errors.As(parseErr, &fault) && evidence.Summary != "" {
				fallback = evidence.Summary
			}
			return nil, classifyClaudeFailure(stderrBuf.String(), fallback, waitErr)
		}

		if waitErr != nil {
			return nil, classifyClaudeFailure(stderrBuf.String(), "", waitErr)
		}

		res := &CLIRunResult{
			Output:       evidence.Summary,
			InputTokens:  int(evidence.InputTokens),
			OutputTokens: int(evidence.OutputTokens),
			Evidence:     evidence,
		}
		return res, nil
	}
}

func classifyClaudeFailure(stderr, fallback string, waitErr error) error {
	msg := strings.TrimSpace(stderr)
	if msg == "" {
		msg = strings.TrimSpace(fallback)
	}
	if msg == "" {
		msg = waitErr.Error()
	}
	low := strings.ToLower(msg)
	if strings.Contains(low, "rate_limit") || strings.Contains(low, "overloaded") || strings.Contains(low, "429") || strings.Contains(low, "529") || strings.Contains(low, "too many") {
		return model.Error("RATE_LIMIT", msg)
	}
	if strings.Contains(low, "authentication") || strings.Contains(low, "credit balance") || strings.Contains(low, "unauthorized") || strings.Contains(low, "invalid_api_key") || strings.Contains(low, "401") {
		return model.Error("AUTH_REQUIRED", msg)
	}
	return model.Error("EXECUTION_FAILED", msg)
}

// ParseClaudeJSON parses claude JSON output.
func ParseClaudeJSON(data []byte) (model.CLIRunEvidence, error) {
	ev := model.CLIRunEvidence{}
	text := strings.TrimSpace(string(data))
	if text == "" {
		return ev, model.Error("INVALID_OUTPUT", "no json output")
	}

	var obj map[string]any
	if err := json.Unmarshal([]byte(text), &obj); err != nil {
		lines := strings.Split(text, "\n")
		for i := len(lines) - 1; i >= 0; i-- {
			l := strings.TrimSpace(lines[i])
			if l == "" {
				continue
			}
			if json.Unmarshal([]byte(l), &obj) == nil {
				break
			}
		}
	}
	if obj == nil {
		return ev, model.Error("INVALID_OUTPUT", "no json output")
	}

	if sid, ok := obj["session_id"].(string); ok {
		ev.SessionSHA256 = sid
		ev.StartupVerified = true
	}

	var resultStr string
	if res, ok := obj["result"].(string); ok {
		resultStr = res
		ev.Summary = res
	}

	if usage, ok := obj["usage"].(map[string]any); ok {
		if v, ok := usage["input_tokens"].(float64); ok {
			ev.InputTokens = int64(v)
		}
		if v, ok := usage["output_tokens"].(float64); ok {
			ev.OutputTokens = int64(v)
		}
	}

	isErr, _ := obj["is_error"].(bool)
	typ, _ := obj["type"].(string)
	subtype, _ := obj["subtype"].(string)

	var errType, errMsg string
	if errObj, ok := obj["error"].(map[string]any); ok {
		errType, _ = errObj["type"].(string)
		errMsg, _ = errObj["message"].(string)
	} else if errStr, ok := obj["error"].(string); ok {
		errMsg = errStr
	}

	allLow := strings.ToLower(strings.Join([]string{typ, subtype, errType, errMsg, resultStr}, " "))

	if isErr || typ == "error" || (subtype != "" && subtype != "success") || errType != "" || errMsg != "" {
		msg := resultStr
		if msg == "" {
			msg = errMsg
		}
		if msg == "" {
			msg = subtype
		}
		if msg == "" {
			msg = errType
		}

		if strings.Contains(allLow, "rate_limit") || strings.Contains(allLow, "overloaded") || strings.Contains(allLow, "429") || strings.Contains(allLow, "529") || strings.Contains(allLow, "too many") {
			return ev, model.Error("RATE_LIMIT", msg)
		}
		if strings.Contains(allLow, "authentication") || strings.Contains(allLow, "credit balance") || strings.Contains(allLow, "unauthorized") || strings.Contains(allLow, "invalid_api_key") || strings.Contains(allLow, "401") {
			return ev, model.Error("AUTH_REQUIRED", msg)
		}
		if msg == "" {
			return ev, model.Error("INVALID_OUTPUT", "empty result")
		}
		return ev, model.Error("INVALID_OUTPUT", msg)
	}

	if ev.Summary == "" {
		return ev, model.Error("INVALID_OUTPUT", "empty result")
	}
	ev.TerminalVerified = true
	return ev, nil
}
