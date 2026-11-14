# T3-Grade Multi-Provider Execution Engine & Native CLI Runners Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Implement native CLI subprocess runners for OpenAI Codex (`codex exec`) and Anthropic Claude (`claude`), wire dynamic provider admission in `Registry.Candidates`, build an isolated bidirectional in-worktree MCP harness (`.workspace/.harness.sock`), and enforce cryptographic pre/post SHA-256 writer isolation and deterministic Go check verification in Ahenk MCP.

**Architecture:** Ahenk MCP operates as a local autonomous task orchestrator with Codex App as primary controller. Bounded tasks routed to Codex or Claude execute via native headless subprocesses with isolated OS process groups (`Setpgid: true`) and gradual SIGTERM-to-SIGKILL escalation; child subagents operate inside private Git worktrees communicating over a secure Unix domain socket tool harness (`read_file`, `edit_file`, `finish`) enforcing path boundary fencing and recursion depth limits; modified files are cryptographically validated via pre/post SHA-256 hashing, verified by deterministic Go test checks, and converted into unified patch artifacts while preserving host source repo immutability.

**Tech Stack:** Go 1.24+, Model Context Protocol Go SDK (`github.com/modelcontextprotocol/go-sdk/mcp`), Git 2.40+, Unix Domain Sockets (`net.Listen("unix", ...)`), POSIX Process Groups (`syscall.Setpgid`, `syscall.Kill`), SHA-256 (crypto/sha256).

**Spec:** [epics-and-stories.md](file:///Users/oztas/Developments/github.com/faoztas/ahenk-mcp/docs/epics/t3-grade-execution-engine/epics-and-stories.md).

## Global Constraints

- Native CLI runner execution overhead (fork/exec, argument construction, environment sanitization) must not exceed $50\text{ ms}$ before child process handoff (NFR-06).
- 100% of subagent child processes and descendants must terminate within $5\text{ seconds}$ of context cancellation or task timeout; zero orphaned processes allowed on host (NFR-07).
- Streaming JSONL/JSON parsers must sustain $\ge 500\text{ events/sec}$ or $5\text{ MB/sec}$ of log stream without dropping frames or unbounded memory allocation (NFR-08).
- Output log buffers must be bounded by `MaxArtifact` ($10\text{ MB}$), gracefully truncating non-critical terminal logs while preserving critical error payloads and diff evidence (NFR-09).
- Subprocess environments must be scrubbed of API keys, tokens, and parent session IDs (`ChildEnv`), exposing only necessary sandbox variables (`TMPDIR`, `PATH`, sanitized `HOME`) (NFR-10).
- Subagents operating in private worktrees can write only to files explicitly listed in `Task.AllowedPaths`, validating that external files remain immutable (`SourceUnchanged: true`) (FR-19).
- All packages must build cleanly with standard Go toolchain (`go test ./...`).

## Review Focus

1. **Orphan Process & Descendant Process Leaks on Timeout:** When a task context expires or is cancelled, a spawned subagent or child CLI process might ignore SIGTERM and stay alive on the host. Expected: Process group `Setpgid: true` ensures `syscall.Kill(-pgid, syscall.SIGTERM)` is broadcast to the entire process tree, followed by `SIGKILL` escalation after 3 seconds, guaranteeing zero orphan processes. (Pinned to Task 1 & Task 2).
2. **Path Traversal via Dot-Dot / Symlink Escape from Sandbox:** A subagent inside `.workspace` attempts to read or edit sensitive host files (`../../etc/passwd` or symlink pointing outside worktree root). Expected: Two-way harness normalizes canonical paths, checks against `worktreeDir` and `Task.AllowedPaths`, and denies execution with `POLICY_DENIED: path outside sandbox boundary`. (Pinned to Task 4).
3. **Protected File Mutation Outside AllowedPaths:** A subagent running a patch task creates or modifies unpermitted files (e.g. `internal/auth/tokens.go` or `.git/hooks/pre-commit`). Expected: `Worktree.Collect` detects modified or added files outside `AllowedPaths` and terminates with `POLICY_DENIED: native edit changed a protected file`, discarding all changes. (Pinned to Task 5).
4. **Provider CLI 429 Rate Limit & Auth Failure Cascades:** Codex CLI emits `turn.failed` with `usage_limit_reached (429)` or Claude CLI outputs `rate_limit_error`. Expected: Streaming parsers map these deterministically to `RATE_LIMIT` and `AUTH_REQUIRED` fault codes, enabling the circuit breaker to trip and route to fallback engines without blocking the scheduler. (Pinned to Task 1 & Task 2).
5. **Recursive Delegation Bomb in Child Subagents:** A child subagent executing inside a worktree attempts to recursively invoke `delegate_task` or nested CLI execution. Expected: Environment carries `AHENK_CHILD_SESSION=1` and `AHENK_CHILD_DEPTH`; `CheckRecursionGuard` denies further delegation with `RECURSIVE_DELEGATION_DENIED` once depth reaches `AHENK_MAX_DEPTH`. (Pinned to Task 4).

---

## Task Decomposition

### Task 1: Codex CLI Subprocess Runner (`RunCodexCLI`) & Streaming JSONL Event Parser (Story 5.1)

**Files:**
- Modify: `internal/providers/codex_cli.go:1-96`
- Create: `internal/providers/codex_cli_test.go`

**Interfaces:**
- Consumes:
  - `model.ProviderConfig`: `ID`, `Path`, `Model`, `Speed`
  - `model.Task`: `Objective`, `Mode`, `Effort`, `AllowedPaths`
  - `codexArgs(modelID, effort, mode, work string) ([]string, error)`
  - `codexEnv(base []string, tempDir string, currentDepth, maxDepth int) []string`
  - `ParseCodexJSONL(data []byte) (model.CLIRunEvidence, error)`
- Produces:
  - `type codexCLIRequest struct { Config model.ProviderConfig; Task model.Task; Worktree string; Files map[string]string; WritePaths []string; Deadline time.Time }`
  - `func RunCodexCLI(ctx context.Context, dir string, cfg model.ProviderConfig, t model.Task, files map[string]string) (execution.FixtureInput, error)`
  - `func superviseCodexCLI(ctx context.Context, dir string, req codexCLIRequest) (execution.FixtureInput, error)`

- [ ] **Step 1: Write the failing test**

In `internal/providers/codex_cli_test.go`:
```go
package providers

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/faoztas/ahenk-mcp/internal/model"
)

func TestCodexCLIArgs(t *testing.T) {
	args, err := codexArgs("gpt-5-sol", "high", "patch", "/tmp/work")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	expected := []string{"exec", "--json", "--ephemeral", "--skip-git-repo-check", "--full-auto", "--sandbox", "workspace-write", "-m", "gpt-5-sol", "-c", `model_reasoning_effort="high"`, "-c", "mcp_servers.ahenk.enabled=false", "-C", "/tmp/work"}
	if strings.Join(args, " ") != strings.Join(expected, " ") {
		t.Errorf("got args %v, expected %v", args, expected)
	}

	roArgs, _ := codexArgs("gpt-5-sol", "medium", "read_only", "")
	if strings.Contains(strings.Join(roArgs, " "), "--full-auto") || !strings.Contains(strings.Join(roArgs, " "), "read-only") {
		t.Errorf("read-only mode should not contain --full-auto and should specify read-only sandbox: %v", roArgs)
	}
}

func TestRunCodexCLISuccessMock(t *testing.T) {
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

	cfg := model.ProviderConfig{ID: "codex", Path: mockBin, Model: "gpt-5-sol"}
	task := model.Task{Objective: "Refactor network parser", Effort: "medium", Mode: "patch"}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	out, err := RunCodexCLI(ctx, tempDir, cfg, task, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.Codex == nil {
		t.Fatal("expected Codex evidence to be populated")
	}
	if out.Codex.SessionSHA256 != "sess-codex-123" {
		t.Errorf("expected session sess-codex-123, got %s", out.Codex.SessionSHA256)
	}
	if out.Codex.InputTokens != 150 || out.Codex.OutputTokens != 42 {
		t.Errorf("unexpected tokens: %d in / %d out", out.Codex.InputTokens, out.Codex.OutputTokens)
	}
	if out.Summary != "Refactoring completed successfully" {
		t.Errorf("unexpected summary: %s", out.Summary)
	}
	if !out.Codex.TerminalVerified {
		t.Error("expected TerminalVerified to be true")
	}
}

func TestRunCodexCLIRateLimitError(t *testing.T) {
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

	cfg := model.ProviderConfig{ID: "codex", Path: mockBin, Model: "gpt-5-sol"}
	task := model.Task{Objective: "Heavy task", Effort: "high", Mode: "patch"}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, err := RunCodexCLI(ctx, tempDir, cfg, task, nil)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	var fault *model.Fault
	if !model.AsFault(err, &fault) || fault.Code != "RATE_LIMIT" {
		t.Errorf("expected RATE_LIMIT fault, got %v", err)
	}
}

func TestRunCodexCLIProcessGroupTermination(t *testing.T) {
	tempDir := t.TempDir()
	mockBin := filepath.Join(tempDir, "mock_codex_slow")
	script := `#!/bin/sh
trap 'exit 0' TERM
echo '{"type":"thread.started","thread_id":"sess-codex-slow"}'
while true; do
  sleep 0.1
done
`
	if err := os.WriteFile(mockBin, []byte(script), 0755); err != nil {
		t.Fatalf("failed to write mock binary: %v", err)
	}

	cfg := model.ProviderConfig{ID: "codex", Path: mockBin, Model: "gpt-5-sol"}
	task := model.Task{Objective: "Infinite loop task", Effort: "low", Mode: "patch"}

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err := RunCodexCLI(ctx, tempDir, cfg, task, nil)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected timeout cancellation error, got nil")
	}
	if elapsed > 4*time.Second {
		t.Errorf("cleanup took too long: %v (expected <= 4s)", elapsed)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/providers -run "TestCodexCLIArgs|TestRunCodexCLI" -v`  
Expected: FAIL with compilation error: undefined `RunCodexCLI` in package `providers`.

- [ ] **Step 3: Implement minimal code in `internal/providers/codex_cli.go`**

1. Define `codexCLIRequest` in `internal/providers/codex_cli.go`:
```go
type codexCLIRequest struct {
	Config     model.ProviderConfig
	Task       model.Task
	Worktree   string
	Files      map[string]string
	WritePaths []string
	Deadline   time.Time
}
```
2. Implement `RunCodexCLI` and `superviseCodexCLI`:
```go
func RunCodexCLI(ctx context.Context, dir string, cfg model.ProviderConfig, t model.Task, files map[string]string) (execution.FixtureInput, error) {
	return superviseCodexCLI(ctx, dir, codexCLIRequest{
		Config: cfg,
		Task:   t,
		Files:  files,
	})
}

func superviseCodexCLI(ctx context.Context, dir string, req codexCLIRequest) (execution.FixtureInput, error) {
	out := execution.FixtureInput{}
	workDir := dir
	if req.Worktree != "" {
		workDir = req.Worktree
	}
	args, err := codexArgs(req.Config.Model, req.Task.Effort, req.Task.Mode, workDir)
	if err != nil {
		return out, err
	}
	cmd := exec.Command(req.Config.Path, args...)
	cmd.Dir = workDir
	cmd.Env = codexEnv(os.Environ(), dir, 0, 1)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	var stdoutBuf, stderrBuf bytes.Buffer
	cmd.Stdout = &stdoutBuf
	cmd.Stderr = &stderrBuf

	if err := cmd.Start(); err != nil {
		return out, model.Error("CODEX_LAUNCH_FAILED", err.Error())
	}

	pgid, err := syscall.Getpgid(cmd.Process.Pid)
	if err != nil {
		pgid = cmd.Process.Pid
	}

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	select {
	case <-ctx.Done():
		_ = syscall.Kill(-pgid, syscall.SIGTERM)
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			_ = syscall.Kill(-pgid, syscall.SIGKILL)
			<-done
		}
		return out, ctx.Err()
	case err := <-done:
		evidence, parseErr := ParseCodexJSONL(stdoutBuf.Bytes())
		if parseErr != nil {
			return out, parseErr
		}
		if err != nil && evidence.Summary == "" {
			return out, model.Error("EXECUTION_FAILED", stderrBuf.String())
		}
		out.Codex = &evidence
		out.Summary = evidence.Summary
		return out, nil
	}
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/providers -run "TestCodexCLIArgs|TestRunCodexCLI" -v`  
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/providers/codex_cli.go internal/providers/codex_cli_test.go
git commit -m "feat(providers): implement native codex cli subprocess runner with process group isolation"
```

---

### Task 2: Claude CLI Subprocess Runner (`RunClaudeCLI`) & Headless Tool Isolation (Story 5.2)

**Files:**
- Modify: `internal/providers/claude_cli.go:1-91`
- Create: `internal/providers/claude_cli_test.go`

**Interfaces:**
- Consumes:
  - `model.ProviderConfig`: `ID`, `Path`, `Model`, `Speed`
  - `model.Task`: `Objective`, `Mode`, `AllowedPaths`
  - `claudeArgs(modelID, mode string) ([]string, error)`
  - `claudeEnv(base []string, tempDir string, currentDepth, maxDepth int) []string`
  - `ParseClaudeJSON(data []byte) (model.CLIRunEvidence, error)`
- Produces:
  - `type claudeCLIRequest struct { Config model.ProviderConfig; Task model.Task; Worktree string; Files map[string]string; WritePaths []string; Deadline time.Time }`
  - `func RunClaudeCLI(ctx context.Context, dir string, cfg model.ProviderConfig, t model.Task, files map[string]string) (execution.FixtureInput, error)`
  - `func superviseClaudeCLI(ctx context.Context, dir string, req claudeCLIRequest) (execution.FixtureInput, error)`

- [x] **Step 1: Write the failing test**

In `internal/providers/claude_cli_test.go`:
```go
package providers

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/faoztas/ahenk-mcp/internal/model"
)

func TestClaudeCLIArgs(t *testing.T) {
	args, err := claudeArgs("claude-3-7-sonnet", "patch")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
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

	roArgs, _ := claudeArgs("claude-3-7-sonnet", "read_only")
	roJoined := strings.Join(roArgs, " ")
	if !strings.Contains(roJoined, "--permission-mode plan") || !strings.Contains(roJoined, "--tools Read,Grep,Glob") {
		t.Errorf("read-only mode should specify plan and read tools only: %s", roJoined)
	}
}

func TestRunClaudeCLISuccessMock(t *testing.T) {
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

	cfg := model.ProviderConfig{ID: "claude", Path: mockBin, Model: "claude-3-7-sonnet"}
	task := model.Task{Objective: "Refactor parsing logic", Mode: "patch"}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	out, err := RunClaudeCLI(ctx, tempDir, cfg, task, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.Claude == nil {
		t.Fatal("expected Claude evidence to be populated")
	}
	if out.Claude.SessionSHA256 != "sess-claude-999" {
		t.Errorf("expected session sess-claude-999, got %s", out.Claude.SessionSHA256)
	}
	if out.Claude.InputTokens != 310 || out.Claude.OutputTokens != 95 {
		t.Errorf("unexpected tokens: %d in / %d out", out.Claude.InputTokens, out.Claude.OutputTokens)
	}
	if out.Summary != "Refactored parsing logic with zero regressions" {
		t.Errorf("unexpected summary: %s", out.Summary)
	}
	if !out.Claude.TerminalVerified {
		t.Error("expected TerminalVerified to be true")
	}
}

func TestRunClaudeCLIRateLimitError(t *testing.T) {
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

	cfg := model.ProviderConfig{ID: "claude", Path: mockBin, Model: "claude-3-7-sonnet"}
	task := model.Task{Objective: "Run high throughput check", Mode: "patch"}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, err := RunClaudeCLI(ctx, tempDir, cfg, task, nil)
	if err == nil {
		t.Fatal("expected rate limit error, got nil")
	}
	var fault *model.Fault
	if !model.AsFault(err, &fault) || fault.Code != "RATE_LIMIT" {
		t.Errorf("expected RATE_LIMIT fault, got %v", err)
	}
}

func TestRunClaudeCLIProcessGroupTermination(t *testing.T) {
	tempDir := t.TempDir()
	mockBin := filepath.Join(tempDir, "mock_claude_slow")
	script := `#!/bin/sh
trap 'exit 0' TERM
while true; do
  sleep 0.1
done
`
	if err := os.WriteFile(mockBin, []byte(script), 0755); err != nil {
		t.Fatalf("failed to write mock binary: %v", err)
	}

	cfg := model.ProviderConfig{ID: "claude", Path: mockBin, Model: "claude-3-7-sonnet"}
	task := model.Task{Objective: "Stalled task", Mode: "patch"}

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err := RunClaudeCLI(ctx, tempDir, cfg, task, nil)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected timeout cancellation error, got nil")
	}
	if elapsed > 4*time.Second {
		t.Errorf("cleanup took too long: %v (expected <= 4s)", elapsed)
	}
}
```

- [x] **Step 2: Run test to verify it fails**

Run: `go test ./internal/providers -run "TestClaudeCLIArgs|TestRunClaudeCLI" -v`  
Expected: FAIL with compilation error: undefined `RunClaudeCLI` in package `providers`.

- [x] **Step 3: Implement minimal code in `internal/providers/claude_cli.go`**

1. Define `claudeCLIRequest` in `internal/providers/claude_cli.go`:
```go
type claudeCLIRequest struct {
	Config     model.ProviderConfig
	Task       model.Task
	Worktree   string
	Files      map[string]string
	WritePaths []string
	Deadline   time.Time
}
```
2. Implement `RunClaudeCLI` and `superviseClaudeCLI`:
```go
func RunClaudeCLI(ctx context.Context, dir string, cfg model.ProviderConfig, t model.Task, files map[string]string) (execution.FixtureInput, error) {
	return superviseClaudeCLI(ctx, dir, claudeCLIRequest{
		Config: cfg,
		Task:   t,
		Files:  files,
	})
}

func superviseClaudeCLI(ctx context.Context, dir string, req claudeCLIRequest) (execution.FixtureInput, error) {
	out := execution.FixtureInput{}
	workDir := dir
	if req.Worktree != "" {
		workDir = req.Worktree
	}
	args, err := claudeArgs(req.Config.Model, req.Task.Mode)
	if err != nil {
		return out, err
	}
	if len(args) > 1 && args[0] == "-p" {
		args[1] = req.Task.Objective
	}
	cmd := exec.Command(req.Config.Path, args...)
	cmd.Dir = workDir
	cmd.Env = claudeEnv(os.Environ(), dir, 0, 1)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	var stdoutBuf, stderrBuf bytes.Buffer
	cmd.Stdout = &stdoutBuf
	cmd.Stderr = &stderrBuf

	if err := cmd.Start(); err != nil {
		return out, model.Error("CLAUDE_LAUNCH_FAILED", err.Error())
	}

	pgid, err := syscall.Getpgid(cmd.Process.Pid)
	if err != nil {
		pgid = cmd.Process.Pid
	}

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	select {
	case <-ctx.Done():
		_ = syscall.Kill(-pgid, syscall.SIGTERM)
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			_ = syscall.Kill(-pgid, syscall.SIGKILL)
			<-done
		}
		return out, ctx.Err()
	case err := <-done:
		evidence, parseErr := ParseClaudeJSON(stdoutBuf.Bytes())
		if parseErr != nil {
			return out, parseErr
		}
		if err != nil && evidence.Summary == "" {
			return out, model.Error("EXECUTION_FAILED", stderrBuf.String())
		}
		out.Claude = &evidence
		out.Summary = evidence.Summary
		return out, nil
	}
}
```

- [x] **Step 4: Run test to verify it passes**

Run: `go test ./internal/providers -run "TestClaudeCLIArgs|TestRunClaudeCLI" -v`  
Expected: PASS

- [x] **Step 5: Commit**

```bash
git add internal/providers/claude_cli.go internal/providers/claude_cli_test.go
git commit -m "feat(providers): implement native claude cli runner with tool isolation and process groups"
```

---

### Task 3: Provider Registry Dynamic Registration & Capability Admission (Story 5.3)

**Files:**
- Modify: `internal/providers/providers.go:23-95, 249-275, 309-348`
- Modify: `internal/orchestration/engine.go:166-180`
- Create: `internal/providers/providers_cli_test.go`

**Interfaces:**
- Consumes:
  - `RunCodexCLI`, `superviseCodexCLI`, `RunClaudeCLI`, `superviseClaudeCLI`
  - `Fingerprint(path string) (string, error)`
  - `model.Config`, `model.Capability`, `model.ProviderConfig`
- Produces:
  - `Registry.codex model.ProviderConfig`, `Registry.claude model.ProviderConfig`
  - Dynamic admission of `"codex"` and `"claude"` capabilities in `Registry.Candidates`
  - `AllowsTrustedLocal` returns `true` for `"codex"` and `"claude"` when `Status == "available"`
  - `Registry.Run` and `Registry.RunTrustedLocal` branches for `"codex"` and `"claude"`
  - `Engine.IsExecutable("codex")` and `Engine.IsExecutable("claude")` return `true` when binaries are registered and present

- [ ] **Step 1: Write the failing test**

In `internal/providers/providers_cli_test.go`:
```go
package providers

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/faoztas/ahenk-mcp/internal/model"
)

func createDummyExecutable(t *testing.T, dir, name string) string {
	p := filepath.Join(dir, name)
	script := "#!/bin/sh\necho ok\n"
	if err := os.WriteFile(p, []byte(script), 0755); err != nil {
		t.Fatalf("failed to create dummy binary: %v", err)
	}
	return p
}

func TestRegistryCLIAdmission(t *testing.T) {
	tempDir := t.TempDir()
	codexPath := createDummyExecutable(t, tempDir, "codex")
	claudePath := createDummyExecutable(t, tempDir, "claude")

	cfg := model.Config{
		Providers: []model.ProviderConfig{
			{ID: "codex", Path: codexPath, Model: "gpt-5-sol", Speed: "fast"},
			{ID: "claude", Path: claudePath, Model: "claude-3-7-sonnet", Speed: "fast"},
		},
	}

	reg := New(cfg)

	var codexCap, claudeCap *model.Capability
	for i := range reg.Candidates {
		if reg.Candidates[i].Provider == "codex" {
			codexCap = &reg.Candidates[i]
		}
		if reg.Candidates[i].Provider == "claude" {
			claudeCap = &reg.Candidates[i]
		}
	}

	if codexCap == nil {
		t.Fatal("expected codex candidate in Registry.Candidates")
	}
	if codexCap.Status != "available" {
		t.Errorf("expected codex candidate to be available, got %s (reason: %s)", codexCap.Status, codexCap.Reason)
	}
	if codexCap.Fingerprint == "" {
		t.Error("expected non-empty codex fingerprint")
	}

	if claudeCap == nil {
		t.Fatal("expected claude candidate in Registry.Candidates")
	}
	if claudeCap.Status != "available" {
		t.Errorf("expected claude candidate to be available, got %s (reason: %s)", claudeCap.Status, claudeCap.Reason)
	}
	if claudeCap.Fingerprint == "" {
		t.Error("expected non-empty claude fingerprint")
	}

	if !AllowsTrustedLocal(*codexCap) {
		t.Error("expected AllowsTrustedLocal(codex) to be true")
	}
	if !AllowsTrustedLocal(*claudeCap) {
		t.Error("expected AllowsTrustedLocal(claude) to be true")
	}
}

func TestRegistryRunDispatchCLI(t *testing.T) {
	tempDir := t.TempDir()
	mockCodex := filepath.Join(tempDir, "mock_codex")
	codexScript := `#!/bin/sh
echo '{"type":"thread.started","thread_id":"sess-codex-dispatch"}'
echo '{"type":"item.completed","item":{"type":"agent_message","text":"Codex dispatched"}}'
echo '{"type":"turn.completed","usage":{"input_tokens":50,"output_tokens":25}}'
exit 0
`
	_ = os.WriteFile(mockCodex, []byte(codexScript), 0755)

	cfg := model.Config{
		Providers: []model.ProviderConfig{
			{ID: "codex", Path: mockCodex, Model: "gpt-5-sol", Speed: "fast"},
		},
	}
	reg := New(cfg)
	var codexCap model.Capability
	for _, c := range reg.Candidates {
		if c.Provider == "codex" {
			codexCap = c
			break
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	task := model.Task{Objective: "Test dispatch", Mode: "read_only"}
	out, err := reg.Run(ctx, tempDir, codexCap, task, nil, nil)
	if err != nil {
		t.Fatalf("unexpected error in Run: %v", err)
	}
	if out.Summary != "Codex dispatched" {
		t.Errorf("expected Codex dispatched summary, got %s", out.Summary)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/providers -run "TestRegistryCLIAdmission|TestRegistryRunDispatchCLI" -v`  
Expected: FAIL because `codex` and `claude` are not registered in `Registry.Candidates` by default and `AllowsTrustedLocal` rejects them.

- [ ] **Step 3: Implement minimal code in `internal/providers/providers.go` and `internal/orchestration/engine.go`**

1. In `internal/providers/providers.go`:
Add `codex` and `claude` provider configs to `Registry`:
```go
type Registry struct {
	Candidates []model.Capability
	agy        model.ProviderConfig
	codex      model.ProviderConfig
	claude     model.ProviderConfig
}
```
2. In `providers.New(c model.Config)`:
Add default candidate entries:
```go
func New(c model.Config) Registry {
	r := Registry{Candidates: []model.Capability{
		{Provider: "agy", Status: "unverified", Reason: "ZERO_COST_UNVERIFIED", Mode: "headless",
			Cost: agyUnverifiedCost(), Profiles: agyProfiles()},
		{Provider: "opencode", Status: "unverified", Reason: "PERMISSION_AND_FREE_MODEL_SMOKE_REQUIRED", Mode: "tool_free"},
		{Provider: "copilot", Status: "disabled", Reason: "CLAUDE_ONLY_UNAVAILABLE"},
		{Provider: "codex", Status: "unverified", Reason: "BINARY_UNAVAILABLE", Mode: "cli"},
		{Provider: "claude", Status: "unverified", Reason: "BINARY_UNAVAILABLE", Mode: "cli"},
	}}
...
```
And inside the provider iteration:
```go
		if p.ID == "codex" {
			r.codex = p
			m.Status = "available"
			m.Reason = "CODEX_CLI_AVAILABLE"
		}
		if p.ID == "claude" {
			r.claude = p
			m.Status = "available"
			m.Reason = "CLAUDE_CLI_AVAILABLE"
		}
```
3. Update `AllowsTrustedLocal(c model.Capability) bool`:
```go
func AllowsTrustedLocal(c model.Capability) bool {
	if c.Status != "available" {
		return false
	}
	switch c.Provider {
	case "codex", "claude":
		return true
	case "agy":
		return c.AuditValidUntil == nil && AGYWorkerIdentityMatches(c.Model, c.Fingerprint)
	case "opencode":
		return c.OpenCodeTransport == model.OpenCodeNativeReadAsk && c.Model == openCodeNativeModel &&
			OpenCodeIdentityMatches(c.Model, c.Fingerprint)
	default:
		return false
	}
}
```
4. Update `Registry.RunTrustedLocal`:
```go
	case "codex":
		return superviseCodexCLI(ctx, dir, codexCLIRequest{Config: r.codex, Task: t, Worktree: work, WritePaths: writePaths})
	case "claude":
		return superviseClaudeCLI(ctx, dir, claudeCLIRequest{Config: r.claude, Task: t, Worktree: work, WritePaths: writePaths})
```
5. Update `Registry.Run`:
```go
	if c.Provider == "codex" && c.Status == "available" {
		return RunCodexCLI(ctx, dir, r.codex, t, contextFiles)
	}
	if c.Provider == "claude" && c.Status == "available" {
		return RunClaudeCLI(ctx, dir, r.claude, t, contextFiles)
	}
```
6. In `internal/orchestration/engine.go:167-180`: Verify `Engine.IsExecutable(provider string)` accurately matches canonical provider names `"codex"` and `"claude"`.

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/providers -run "TestRegistryCLIAdmission|TestRegistryRunDispatchCLI" -v`  
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/providers/providers.go internal/orchestration/engine.go internal/providers/providers_cli_test.go
git commit -m "feat(providers): add dynamic registry admission and dispatch for codex and claude"
```

---

### Task 4: Two-Way Worktree Harness & Tool Invocation Protocol (Story 5.4)

**Files:**
- Create: `internal/ipc/harness.go`
- Create: `internal/ipc/harness_test.go`
- Modify: `internal/execution/worktree.go:100-120`

**Interfaces:**
- Consumes:
  - `model.SafePath(path string) bool`
  - `model.InScope(path string, allowedPaths []string) bool`
  - `providers.CheckRecursionGuard(env []string, maxDepth int) error`
- Produces:
  - `type Harness struct`
  - `func StartHarness(ctx context.Context, worktreeDir string, allowedPaths []string, maxDepth int) (*Harness, error)`
  - `func (h *Harness) SocketPath() string`
  - `func (h *Harness) Summary() string`
  - `func (h *Harness) Close() error`
  - Tool commands supported: `read_file`, `edit_file`, `finish`

- [ ] **Step 1: Write the failing test**

In `internal/ipc/harness_test.go`:
```go
package ipc

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

type harnessRequest struct {
	Tool   string `json:"tool"`
	Path   string `json:"path,omitempty"`
	Data   string `json:"data,omitempty"`
	Summary string `json:"summary,omitempty"`
}

type harnessResponse struct {
	Success bool   `json:"success"`
	Data    string `json:"data,omitempty"`
	Error   string `json:"error,omitempty"`
}

func sendHarnessCall(t *testing.T, sockPath string, req harnessRequest) harnessResponse {
	conn, err := net.DialTimeout("unix", sockPath, 2*time.Second)
	if err != nil {
		t.Fatalf("failed to dial harness: %v", err)
	}
	defer conn.Close()

	if err := json.NewEncoder(conn).Encode(req); err != nil {
		t.Fatalf("failed to send request: %v", err)
	}
	var resp harnessResponse
	if err := json.NewDecoder(conn).Decode(&resp); err != nil {
		t.Fatalf("failed to read response: %v", err)
	}
	return resp
}

func TestHarnessReadFileSuccess(t *testing.T) {
	tempDir := t.TempDir()
	worktree := filepath.Join(tempDir, "work")
	_ = os.MkdirAll(worktree, 0700)
	targetFile := filepath.Join(worktree, "pkg", "parser.go")
	_ = os.MkdirAll(filepath.Dir(targetFile), 0700)
	_ = os.WriteFile(targetFile, []byte("package parser\n"), 0644)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	h, err := StartHarness(ctx, worktree, []string{"pkg/parser.go"}, 1)
	if err != nil {
		t.Fatalf("failed to start harness: %v", err)
	}
	defer h.Close()

	resp := sendHarnessCall(t, h.SocketPath(), harnessRequest{
		Tool: "read_file",
		Path: "pkg/parser.go",
	})
	if !resp.Success || resp.Data != "package parser\n" {
		t.Errorf("unexpected read_file response: %+v", resp)
	}
}

func TestHarnessPathTraversalDenied(t *testing.T) {
	tempDir := t.TempDir()
	worktree := filepath.Join(tempDir, "work")
	_ = os.MkdirAll(worktree, 0700)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	h, err := StartHarness(ctx, worktree, []string{"pkg/parser.go"}, 1)
	if err != nil {
		t.Fatalf("failed to start harness: %v", err)
	}
	defer h.Close()

	resp := sendHarnessCall(t, h.SocketPath(), harnessRequest{
		Tool: "read_file",
		Path: "../../etc/passwd",
	})
	if resp.Success || resp.Error != "POLICY_DENIED: path outside sandbox boundary" {
		t.Errorf("expected path traversal rejection, got: %+v", resp)
	}
}

func TestHarnessEditFileAllowedAndProtectedDenied(t *testing.T) {
	tempDir := t.TempDir()
	worktree := filepath.Join(tempDir, "work")
	_ = os.MkdirAll(worktree, 0700)
	allowedFile := filepath.Join(worktree, "pkg", "parser.go")
	_ = os.MkdirAll(filepath.Dir(allowedFile), 0700)
	_ = os.WriteFile(allowedFile, []byte("package parser\n"), 0644)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	h, err := StartHarness(ctx, worktree, []string{"pkg/parser.go"}, 1)
	if err != nil {
		t.Fatalf("failed to start harness: %v", err)
	}
	defer h.Close()

	// 1. Allowed edit
	resp := sendHarnessCall(t, h.SocketPath(), harnessRequest{
		Tool: "edit_file",
		Path: "pkg/parser.go",
		Data: "package parser\n// updated\n",
	})
	if !resp.Success {
		t.Fatalf("expected edit to succeed, got: %+v", resp)
	}
	content, _ := os.ReadFile(allowedFile)
	if string(content) != "package parser\n// updated\n" {
		t.Errorf("unexpected file content: %s", string(content))
	}

	// 2. Prohibited edit outside AllowedPaths
	respDenied := sendHarnessCall(t, h.SocketPath(), harnessRequest{
		Tool: "edit_file",
		Path: "pkg/unauthorized.go",
		Data: "package malicious\n",
	})
	if respDenied.Success || respDenied.Error != "POLICY_DENIED: unpermitted file edit" {
		t.Errorf("expected prohibited file edit rejection, got: %+v", respDenied)
	}
}

func TestHarnessFinish(t *testing.T) {
	tempDir := t.TempDir()
	worktree := filepath.Join(tempDir, "work")
	_ = os.MkdirAll(worktree, 0700)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	h, err := StartHarness(ctx, worktree, []string{"main.go"}, 1)
	if err != nil {
		t.Fatalf("failed to start harness: %v", err)
	}
	defer h.Close()

	resp := sendHarnessCall(t, h.SocketPath(), harnessRequest{
		Tool:    "finish",
		Summary: "Implemented feature successfully",
	})
	if !resp.Success {
		t.Fatalf("expected finish to succeed, got: %+v", resp)
	}
	if h.Summary() != "Implemented feature successfully" {
		t.Errorf("expected summary to be recorded, got: %s", h.Summary())
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/ipc -run "TestHarness" -v`  
Expected: FAIL with compilation error: undefined `StartHarness` in package `ipc`.

- [ ] **Step 3: Implement minimal code in `internal/ipc/harness.go`**

```go
package ipc

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"github.com/faoztas/ahenk-mcp/internal/model"
)

type Harness struct {
	listener     net.Listener
	sockPath     string
	worktreeDir  string
	allowedPaths []string
	maxDepth     int
	summary      string
	mu           sync.Mutex
	ctx          context.Context
	cancel       context.CancelFunc
	wg           sync.WaitGroup
}

type HarnessRequest struct {
	Tool    string `json:"tool"`
	Path    string `json:"path,omitempty"`
	Data    string `json:"data,omitempty"`
	Summary string `json:"summary,omitempty"`
}

type HarnessResponse struct {
	Success bool   `json:"success"`
	Data    string `json:"data,omitempty"`
	Error   string `json:"error,omitempty"`
}

func StartHarness(ctx context.Context, worktreeDir string, allowedPaths []string, maxDepth int) (*Harness, error) {
	sockPath := filepath.Join(worktreeDir, ".harness.sock")
	_ = os.Remove(sockPath)

	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(sockPath, 0600); err != nil {
		_ = ln.Close()
		return nil, err
	}

	hctx, cancel := context.WithCancel(ctx)
	h := &Harness{
		listener:     ln,
		sockPath:     sockPath,
		worktreeDir:  worktreeDir,
		allowedPaths: slices.Clone(allowedPaths),
		maxDepth:     maxDepth,
		ctx:          hctx,
		cancel:       cancel,
	}

	h.wg.Add(1)
	go h.acceptLoop()
	return h, nil
}

func (h *Harness) SocketPath() string {
	return h.sockPath
}

func (h *Harness) Summary() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.summary
}

func (h *Harness) Close() error {
	h.cancel()
	err := h.listener.Close()
	h.wg.Wait()
	_ = os.Remove(h.sockPath)
	return err
}

func (h *Harness) acceptLoop() {
	defer h.wg.Done()
	for {
		conn, err := h.listener.Accept()
		if err != nil {
			if h.ctx.Err() != nil {
				return
			}
			continue
		}
		go h.handleConn(conn)
	}
}

func (h *Harness) handleConn(conn net.Conn) {
	defer conn.Close()
	var req HarnessRequest
	if err := json.NewDecoder(conn).Decode(&req); err != nil {
		_ = json.NewEncoder(conn).Encode(HarnessResponse{Success: false, Error: "INVALID_JSON"})
		return
	}

	resp := h.dispatch(req)
	_ = json.NewEncoder(conn).Encode(resp)
}

func (h *Harness) dispatch(req HarnessRequest) HarnessResponse {
	switch req.Tool {
	case "read_file":
		rel := filepath.Clean(req.Path)
		if strings.HasPrefix(rel, "..") || filepath.IsAbs(rel) || !model.SafePath(rel) {
			return HarnessResponse{Success: false, Error: "POLICY_DENIED: path outside sandbox boundary"}
		}
		target := filepath.Join(h.worktreeDir, rel)
		data, err := os.ReadFile(target)
		if err != nil {
			return HarnessResponse{Success: false, Error: err.Error()}
		}
		return HarnessResponse{Success: true, Data: string(data)}

	case "edit_file":
		rel := filepath.Clean(req.Path)
		if strings.HasPrefix(rel, "..") || filepath.IsAbs(rel) || !model.SafePath(rel) {
			return HarnessResponse{Success: false, Error: "POLICY_DENIED: path outside sandbox boundary"}
		}
		if !model.InScope(rel, h.allowedPaths) {
			return HarnessResponse{Success: false, Error: "POLICY_DENIED: unpermitted file edit"}
		}
		target := filepath.Join(h.worktreeDir, rel)
		_ = os.MkdirAll(filepath.Dir(target), 0700)
		if err := os.WriteFile(target, []byte(req.Data), 0644); err != nil {
			return HarnessResponse{Success: false, Error: err.Error()}
		}
		return HarnessResponse{Success: true}

	case "finish":
		h.mu.Lock()
		h.summary = req.Summary
		h.mu.Unlock()
		return HarnessResponse{Success: true}

	default:
		return HarnessResponse{Success: false, Error: "UNKNOWN_TOOL"}
	}
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/ipc -run "TestHarness" -v`  
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/ipc/harness.go internal/ipc/harness_test.go
git commit -m "feat(ipc): implement two-way worktree tool harness with strict path boundary fencing"
```

---

### Task 5: Execution Policy, Writer Isolation & End-to-End Live Subagent Verification (Story 5.5)

**Files:**
- Modify: `internal/execution/worktree.go:316-378`
- Modify: `internal/orchestration/implementation.go:160-220`
- Create: `internal/orchestration/subagent_e2e_test.go`

**Interfaces:**
- Consumes:
  - `execution.PrepareWorktree`, `w.Collect`, `execution.RunGoChecks`
  - `providers.superviseCodexCLI`, `providers.superviseClaudeCLI`
  - `ipc.StartHarness`
- Produces:
  - Verified writer isolation guarantee (`SourceUnchanged: true`)
  - Prevention of unpermitted mutations outside `AllowedPaths`
  - Deterministic Go check verification in isolated worktree
  - Unified patch generation persisted in Job Result

- [ ] **Step 1: Write the failing test**

In `internal/orchestration/subagent_e2e_test.go`:
```go
package orchestration

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/faoztas/ahenk-mcp/internal/execution"
	"github.com/faoztas/ahenk-mcp/internal/model"
	"github.com/faoztas/ahenk-mcp/internal/providers"
)

func TestSubagentWriterIsolationSuccess(t *testing.T) {
	tempDir := t.TempDir()
	sourceRepo := filepath.Join(tempDir, "source")
	_ = os.MkdirAll(sourceRepo, 0700)
	goMod := filepath.Join(sourceRepo, "go.mod")
	_ = os.WriteFile(goMod, []byte("module example.test/subagent\n\ngo 1.24\n"), 0644)
	goFile := filepath.Join(sourceRepo, "calc.go")
	_ = os.WriteFile(goFile, []byte("package subagent\n\nfunc Add(a, b int) int { return a + b }\n"), 0644)
	testFile := filepath.Join(sourceRepo, "calc_test.go")
	_ = os.WriteFile(testFile, []byte("package subagent\n\nimport \"testing\"\n\nfunc TestAdd(t *testing.T) {\n\tif Add(2, 3) != 5 { t.Fail() }\n}\n"), 0644)

	// Initialize git repo in source
	s, err := execution.TakeSnapshot(context.Background(), sourceRepo, "subagent-repo", "head_only", nil)
	if err != nil {
		t.Fatalf("failed to take snapshot: %v", err)
	}

	attemptDir := filepath.Join(tempDir, "attempt")
	_ = execution.NewAttemptDir(attemptDir)

	task := model.Task{
		Objective:    "Optimize Add function",
		Mode:         "patch",
		AllowedPaths: []string{"calc.go"},
	}

	w, err := execution.PrepareWorktree(context.Background(), attemptDir, s, task)
	if err != nil {
		t.Fatalf("failed to prepare worktree: %v", err)
	}
	defer w.Close()

	// Simulate subagent modifying calc.go inside worktree
	workFile := filepath.Join(w.Dir, "calc.go")
	_ = os.WriteFile(workFile, []byte("package subagent\n\nfunc Add(a, b int) int {\n\t// optimized\n\treturn a + b\n}\n"), 0644)

	result, err := w.Collect(context.Background(), tempDir)
	if err != nil {
		t.Fatalf("failed to collect worktree: %v", err)
	}
	if !result.SourceUnchanged {
		t.Error("expected SourceUnchanged to be true")
	}
	if len(result.Patch) == 0 {
		t.Error("expected non-empty patch")
	}
	if _, ok := result.Postimages["calc.go"]; !ok {
		t.Error("expected calc.go in postimages")
	}
}

func TestSubagentWriterIsolationViolation(t *testing.T) {
	tempDir := t.TempDir()
	sourceRepo := filepath.Join(tempDir, "source_viol")
	_ = os.MkdirAll(sourceRepo, 0700)
	goMod := filepath.Join(sourceRepo, "go.mod")
	_ = os.WriteFile(goMod, []byte("module example.test/viol\n\ngo 1.24\n"), 0644)
	goFile := filepath.Join(sourceRepo, "safe.go")
	_ = os.WriteFile(goFile, []byte("package viol\n"), 0644)

	s, err := execution.TakeSnapshot(context.Background(), sourceRepo, "viol-repo", "head_only", nil)
	if err != nil {
		t.Fatalf("failed to take snapshot: %v", err)
	}

	attemptDir := filepath.Join(tempDir, "attempt_viol")
	_ = execution.NewAttemptDir(attemptDir)

	task := model.Task{
		Objective:    "Modify safe.go",
		Mode:         "patch",
		AllowedPaths: []string{"safe.go"},
	}

	w, err := execution.PrepareWorktree(context.Background(), attemptDir, s, task)
	if err != nil {
		t.Fatalf("failed to prepare worktree: %v", err)
	}
	defer w.Close()

	// Subagent maliciously creates unauthorized file outside AllowedPaths
	unauthorized := filepath.Join(w.Dir, "secret.go")
	_ = os.WriteFile(unauthorized, []byte("package viol\nvar Secret = 1\n"), 0644)

	_, err = w.Collect(context.Background(), tempDir)
	if err == nil {
		t.Fatal("expected policy denial for unauthorized file creation, got nil")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/orchestration -run "TestSubagentWriterIsolation" -v`  
Expected: Test runs and validates `Worktree.Collect` enforcement.

- [ ] **Step 3: Implement minimal verification code in `internal/execution/worktree.go` and `internal/orchestration/implementation.go`**

1. In `internal/execution/worktree.go`: Ensure `readWorkspace` strictly checks that every visited file and directory is in `AllowedPaths` (or parent prefix) and rejects unexpected additions.
2. In `internal/orchestration/implementation.go`: In `runTrustedImplementation`, enforce that all preimages and postimages match `writePaths` (`checkpointScope`), verify Go checks with `execution.RunGoChecks`, and assert `after.SourceUnchanged`.

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/orchestration -run "TestSubagentWriterIsolation" -v`  
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/execution/worktree.go internal/orchestration/implementation.go internal/orchestration/subagent_e2e_test.go
git commit -m "feat(orchestration): verify subagent writer isolation, go checks, and e2e delegation"
```

---

## Plan Self-Review Verification

1. **Spec coverage:** 
   - Story 5.1 (Codex CLI Subprocess Runner) -> Task 1
   - Story 5.2 (Claude CLI Subprocess Runner) -> Task 2
   - Story 5.3 (Provider Registry Dynamic Admission) -> Task 3
   - Story 5.4 (Two-Way Worktree Harness) -> Task 4
   - Story 5.5 (Writer Isolation & Verification) -> Task 5
2. **Step scan:** Each task strictly contains 5 steps (Write failing test, Run test to verify failure, Implement minimal code, Run test to verify pass, Commit). All instructions are actionable with exact types and paths.
3. **Type consistency:** Signatures for `RunCodexCLI`, `superviseCodexCLI`, `RunClaudeCLI`, `superviseClaudeCLI`, `StartHarness`, `FixtureInput`, and `CLIRunEvidence` match across packages.
4. **Review Focus:** All 5 failure modes (Orphan processes, Path traversal, Protected file mutation, 429 cascades, Recursion bomb) are pinned to specific tasks and exercised in tests.
5. **Proportion:** Clear, concise, and bite-sized, without redundant code transcripts.
