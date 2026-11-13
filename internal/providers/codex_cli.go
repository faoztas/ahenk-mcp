// Copyright 2026 Furkan Anıl Öztaş
// SPDX-License-Identifier: Apache-2.0

package providers

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"github.com/faoztas/ahenk-mcp/internal/model"
)

// SandboxMode represents sandbox execution constraints for Codex CLI.
type SandboxMode string

const (
	SandboxModeReadOnly      SandboxMode = "read-only"
	SandboxModeWorkspaceWrite SandboxMode = "workspace-write"
)

// CLIRunEvidence is an alias to model.CLIRunEvidence for package convenience.
type CLIRunEvidence = model.CLIRunEvidence

// CodexJSONLEvent captures an individual event object from Codex CLI's streaming JSONL output.
type CodexJSONLEvent struct {
	Type     string         `json:"type"`
	ThreadID string         `json:"thread_id,omitempty"`
	Item     map[string]any `json:"item,omitempty"`
	Usage    map[string]any `json:"usage,omitempty"`
	Error    map[string]any `json:"error,omitempty"`
	Message  string         `json:"message,omitempty"`
	Raw      map[string]any `json:"-"`
}

func (e *CodexJSONLEvent) UnmarshalJSON(data []byte) error {
	type Alias CodexJSONLEvent
	var a Alias
	if err := json.Unmarshal(data, &a); err != nil {
		return err
	}
	*e = CodexJSONLEvent(a)
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err == nil {
		e.Raw = raw
	}
	return nil
}

// CLIRunResult holds the summarized and raw results of a Codex CLI execution.
type CLIRunResult struct {
	Output       string
	InputTokens  int
	OutputTokens int
	Evidence     model.CLIRunEvidence
	RawEvents    []CodexJSONLEvent
}

// BuildCodexArgs constructs the CLI argument list based on model, effort, sandbox mode, and work directory.
func BuildCodexArgs(modelID, effort string, sandbox SandboxMode, work string) ([]string, error) {
	if effort == "" {
		effort = "medium"
	}
	args := []string{"exec", "--json", "--ephemeral", "--skip-git-repo-check"}
	if sandbox == SandboxModeReadOnly {
		args = append(args, "--sandbox", "read-only")
	} else {
		args = append(args, "--full-auto", "--sandbox", "workspace-write")
	}
	if modelID != "" {
		args = append(args, "-m", modelID)
	}
	args = append(args, "-c", `model_reasoning_effort="`+effort+`"`, "-c", "mcp_servers.ahenk.enabled=false")
	if work != "" {
		args = append(args, "-C", work)
	}
	return args, nil
}

// codexArgs builds the codex exec argument list for compatibility.
func codexArgs(modelID, effort, mode, work string) ([]string, error) {
	sb := SandboxModeWorkspaceWrite
	if mode == "read_only" || mode == "analysis" || mode == "review" {
		sb = SandboxModeReadOnly
	}
	return BuildCodexArgs(modelID, effort, sb, work)
}

func codexEnv(base []string, tempDir string, currentDepth, maxDepth int) []string {
	return ChildEnv(base, tempDir, currentDepth, maxDepth)
}

// RunCodexCLI executes the native codex CLI in a subprocess with process group isolation and streaming JSONL parsing.
func RunCodexCLI(ctx context.Context, worktreeDir string, modelID string, effort string, prompt string, sandbox SandboxMode, binPath string) (*CLIRunResult, error) {
	if binPath == "" {
		binPath = "codex"
	}
	args, err := BuildCodexArgs(modelID, effort, sandbox, worktreeDir)
	if err != nil {
		return nil, err
	}

	cmd := exec.Command(binPath, args...)
	if worktreeDir != "" {
		cmd.Dir = worktreeDir
	}
	cmd.Env = codexEnv(os.Environ(), worktreeDir, 0, 1)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	if prompt != "" {
		cmd.Stdin = strings.NewReader(prompt)
	}

	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return nil, model.Error("CODEX_LAUNCH_FAILED", err.Error())
	}
	var stderrBuf bytes.Buffer
	cmd.Stderr = &stderrBuf

	if err := cmd.Start(); err != nil {
		return nil, model.Error("CODEX_LAUNCH_FAILED", err.Error())
	}

	pgid, err := syscall.Getpgid(cmd.Process.Pid)
	if err != nil {
		pgid = cmd.Process.Pid
	}

	var (
		rawEvents []CodexJSONLEvent
		evidence  model.CLIRunEvidence
		streamErr error
		outputBuf strings.Builder
	)

	stdoutDone := make(chan struct{})
	go func() {
		defer close(stdoutDone)
		sc := bufio.NewScanner(stdoutPipe)
		sc.Buffer(make([]byte, 1024*1024), 1024*1024)
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			if line == "" {
				continue
			}
			var ev CodexJSONLEvent
			if err := json.Unmarshal([]byte(line), &ev); err != nil {
				continue
			}
			rawEvents = append(rawEvents, ev)

			switch ev.Type {
			case "thread.started":
				if ev.ThreadID != "" {
					evidence.SessionSHA256 = ev.ThreadID
					evidence.StartupVerified = true
				}
			case "item.completed":
				if ev.Item != nil {
					if itemType, _ := ev.Item["type"].(string); itemType == "agent_message" {
						if txt, ok := ev.Item["text"].(string); ok {
							evidence.Summary = txt
							outputBuf.WriteString(txt)
						}
					}
				}
			case "turn.completed":
				if ev.Usage != nil {
					if v, ok := ev.Usage["input_tokens"].(float64); ok {
						evidence.InputTokens = int64(v)
					}
					if v, ok := ev.Usage["output_tokens"].(float64); ok {
						evidence.OutputTokens = int64(v)
					}
				}
				evidence.TerminalVerified = true
			case "turn.failed", "error":
				msg := ""
				if ev.Error != nil {
					msg, _ = ev.Error["message"].(string)
				}
				if ev.Message != "" && msg == "" {
					msg = ev.Message
				}
				low := strings.ToLower(msg)
				if strings.Contains(low, "429") || strings.Contains(low, "rate limit") || strings.Contains(low, "usage_limit") {
					streamErr = model.Error("RATE_LIMIT", "RATE_LIMIT: "+msg)
				} else if strings.Contains(low, "401") || strings.Contains(low, "unauthorized") || strings.Contains(low, "not logged in") {
					streamErr = model.Error("AUTH_REQUIRED", msg)
				} else {
					streamErr = model.Error("INVALID_OUTPUT", msg)
				}
			}
		}
	}()

	waitDone := make(chan error, 1)
	go func() {
		<-stdoutDone
		waitDone <- cmd.Wait()
	}()

	select {
	case <-ctx.Done():
		_ = syscall.Kill(-pgid, syscall.SIGTERM)
		select {
		case <-waitDone:
		case <-time.After(3 * time.Second):
			_ = syscall.Kill(-pgid, syscall.SIGKILL)
			<-waitDone
		}
		return nil, ctx.Err()

	case waitErr := <-waitDone:
		if streamErr != nil {
			return nil, streamErr
		}
		if waitErr != nil && evidence.Summary == "" {
			msg := stderrBuf.String()
			if msg == "" {
				msg = waitErr.Error()
			}
			low := strings.ToLower(msg)
			if strings.Contains(low, "429") || strings.Contains(low, "rate limit") || strings.Contains(low, "usage_limit") {
				return nil, model.Error("RATE_LIMIT", "RATE_LIMIT: "+msg)
			}
			return nil, model.Error("EXECUTION_FAILED", msg)
		}

		res := &CLIRunResult{
			Output:       evidence.Summary,
			InputTokens:  int(evidence.InputTokens),
			OutputTokens: int(evidence.OutputTokens),
			Evidence:     evidence,
			RawEvents:    rawEvents,
		}
		return res, nil
	}
}

// ParseCodexJSONL parses codex JSONL output into evidence.
func ParseCodexJSONL(data []byte) (model.CLIRunEvidence, error) {
	ev := model.CLIRunEvidence{}
	sc := bufio.NewScanner(strings.NewReader(string(data)))
	sc.Buffer(make([]byte, 1024*1024), 1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var obj map[string]any
		if err := json.Unmarshal([]byte(line), &obj); err != nil {
			continue
		}
		t, _ := obj["type"].(string)
		switch t {
		case "thread.started":
			if id, ok := obj["thread_id"].(string); ok {
				ev.SessionSHA256 = id
				ev.StartupVerified = true
			}
		case "item.completed":
			item, _ := obj["item"].(map[string]any)
			if item != nil {
				if item["type"] == "agent_message" {
					if txt, ok := item["text"].(string); ok {
						ev.Summary = txt
					}
				}
			}
		case "turn.completed":
			if usage, ok := obj["usage"].(map[string]any); ok {
				if v, ok := usage["input_tokens"].(float64); ok {
					ev.InputTokens = int64(v)
				}
				if v, ok := usage["output_tokens"].(float64); ok {
					ev.OutputTokens = int64(v)
				}
			}
			ev.TerminalVerified = true
		case "turn.failed", "error":
			msg := ""
			if e, ok := obj["error"].(map[string]any); ok {
				msg, _ = e["message"].(string)
			}
			if m, ok := obj["message"].(string); ok && msg == "" {
				msg = m
			}
			low := strings.ToLower(msg)
			if strings.Contains(low, "429") || strings.Contains(low, "rate limit") || strings.Contains(low, "usage_limit") {
				return ev, model.Error("RATE_LIMIT", "RATE_LIMIT: "+msg)
			}
			if strings.Contains(low, "401") || strings.Contains(low, "unauthorized") || strings.Contains(low, "not logged in") {
				return ev, model.Error("AUTH_REQUIRED", msg)
			}
			return ev, model.Error("INVALID_OUTPUT", msg)
		}
	}
	return ev, nil
}
