// Copyright 2026 Furkan Anıl Öztaş
// SPDX-License-Identifier: Apache-2.0

package ipc

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type harnessRequest struct {
	JSONRPC      string   `json:"jsonrpc,omitempty"`
	ID           any      `json:"id,omitempty"`
	Method       string   `json:"method,omitempty"`
	Params       any      `json:"params,omitempty"`
	Tool         string   `json:"tool,omitempty"`
	Path         string   `json:"path,omitempty"`
	Data         string   `json:"data,omitempty"`
	Content      string   `json:"content,omitempty"`
	OldContent   string   `json:"old_content,omitempty"`
	Summary      string   `json:"summary,omitempty"`
	Success      *bool    `json:"success,omitempty"`
	CurrentDepth int      `json:"current_depth,omitempty"`
	MaxDepth     int      `json:"max_depth,omitempty"`
	Env          []string `json:"env,omitempty"`
}

type harnessResponse struct {
	JSONRPC string `json:"jsonrpc,omitempty"`
	ID      any    `json:"id,omitempty"`
	Result  any    `json:"result,omitempty"`
	Error   string `json:"error,omitempty"`
	Success bool   `json:"success"`
	Data    string `json:"data,omitempty"`
}

func sendHarnessCall(t *testing.T, sockPath string, req harnessRequest) harnessResponse {
	conn, err := net.DialTimeout("unix", sockPath, 2*time.Second)
	if err != nil {
		t.Fatalf("failed to dial harness socket: %v", err)
	}
	defer conn.Close()

	if err := json.NewEncoder(conn).Encode(req); err != nil {
		t.Fatalf("failed to encode harness request: %v", err)
	}

	var resp harnessResponse
	if err := json.NewDecoder(conn).Decode(&resp); err != nil {
		t.Fatalf("failed to decode harness response: %v", err)
	}
	return resp
}

func TestHarnessReadFileSuccess(t *testing.T) {
	tempDir := t.TempDir()
	worktree := filepath.Join(tempDir, "work")
	if err := os.MkdirAll(worktree, 0700); err != nil {
		t.Fatalf("failed to create worktree: %v", err)
	}
	targetFile := filepath.Join(worktree, "pkg", "parser.go")
	if err := os.MkdirAll(filepath.Dir(targetFile), 0700); err != nil {
		t.Fatalf("failed to create pkg dir: %v", err)
	}
	if err := os.WriteFile(targetFile, []byte("package parser\n"), 0644); err != nil {
		t.Fatalf("failed to write test file: %v", err)
	}

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
	if err := os.MkdirAll(worktree, 0700); err != nil {
		t.Fatalf("failed to create worktree: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	h, err := StartHarness(ctx, worktree, []string{"pkg/parser.go"}, 1)
	if err != nil {
		t.Fatalf("failed to start harness: %v", err)
	}
	defer h.Close()

	// Relative path traversal attempt
	resp := sendHarnessCall(t, h.SocketPath(), harnessRequest{
		Tool: "read_file",
		Path: "../../etc/passwd",
	})
	if resp.Success || resp.Error != "POLICY_DENIED: path outside sandbox boundary" {
		t.Errorf("expected path traversal rejection, got: %+v", resp)
	}

	// Absolute path traversal attempt
	respAbs := sendHarnessCall(t, h.SocketPath(), harnessRequest{
		Tool: "read_file",
		Path: "/etc/shadow",
	})
	if respAbs.Success || respAbs.Error != "POLICY_DENIED: path outside sandbox boundary" {
		t.Errorf("expected absolute path rejection, got: %+v", respAbs)
	}
}

func TestHarnessEditFileAllowedAndProtectedDenied(t *testing.T) {
	tempDir := t.TempDir()
	worktree := filepath.Join(tempDir, "work")
	if err := os.MkdirAll(worktree, 0700); err != nil {
		t.Fatalf("failed to create worktree: %v", err)
	}
	allowedFile := filepath.Join(worktree, "pkg", "parser.go")
	if err := os.MkdirAll(filepath.Dir(allowedFile), 0700); err != nil {
		t.Fatalf("failed to create pkg dir: %v", err)
	}
	if err := os.WriteFile(allowedFile, []byte("package parser\n"), 0644); err != nil {
		t.Fatalf("failed to write allowed file: %v", err)
	}

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
	if respDenied.Success || respDenied.Error != "POLICY_DENIED: path not permitted for modification" {
		t.Errorf("expected prohibited file edit rejection, got: %+v", respDenied)
	}

	// 3. Path traversal edit outside sandbox
	respTraversal := sendHarnessCall(t, h.SocketPath(), harnessRequest{
		Tool: "edit_file",
		Path: "../../evil.sh",
		Data: "rm -rf /",
	})
	if respTraversal.Success || respTraversal.Error != "POLICY_DENIED: path outside sandbox boundary" {
		t.Errorf("expected traversal edit rejection, got: %+v", respTraversal)
	}
}

func TestHarnessEditFileOldContentMatch(t *testing.T) {
	tempDir := t.TempDir()
	worktree := filepath.Join(tempDir, "work")
	if err := os.MkdirAll(worktree, 0700); err != nil {
		t.Fatalf("failed to create worktree: %v", err)
	}
	targetFile := filepath.Join(worktree, "code.go")
	if err := os.WriteFile(targetFile, []byte("original\n"), 0644); err != nil {
		t.Fatalf("failed to write target file: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	h, err := StartHarness(ctx, worktree, []string{"code.go"}, 1)
	if err != nil {
		t.Fatalf("failed to start harness: %v", err)
	}
	defer h.Close()

	// Mismatched old_content should fail
	respMismatch := sendHarnessCall(t, h.SocketPath(), harnessRequest{
		Tool:       "edit_file",
		Path:       "code.go",
		Content:    "modified\n",
		OldContent: "wrong_previous\n",
	})
	if respMismatch.Success || !strings.Contains(respMismatch.Error, "PRECONDITION_FAILED") {
		t.Errorf("expected PRECONDITION_FAILED error, got: %+v", respMismatch)
	}

	// Matching old_content should succeed
	respMatch := sendHarnessCall(t, h.SocketPath(), harnessRequest{
		Tool:       "edit_file",
		Path:       "code.go",
		Content:    "modified\n",
		OldContent: "original\n",
	})
	if !respMatch.Success {
		t.Fatalf("expected successful edit, got: %+v", respMatch)
	}
	updated, _ := os.ReadFile(targetFile)
	if string(updated) != "modified\n" {
		t.Errorf("unexpected file content: %s", string(updated))
	}
}

func TestHarnessFinish(t *testing.T) {
	tempDir := t.TempDir()
	worktree := filepath.Join(tempDir, "work")
	if err := os.MkdirAll(worktree, 0700); err != nil {
		t.Fatalf("failed to create worktree: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	h, err := StartHarness(ctx, worktree, []string{"main.go"}, 1)
	if err != nil {
		t.Fatalf("failed to start harness: %v", err)
	}
	defer h.Close()

	tr := true
	resp := sendHarnessCall(t, h.SocketPath(), harnessRequest{
		Tool:    "finish",
		Summary: "Implemented feature successfully",
		Success: &tr,
	})
	if !resp.Success {
		t.Fatalf("expected finish to succeed, got: %+v", resp)
	}
	if h.Summary() != "Implemented feature successfully" {
		t.Errorf("expected summary to be recorded, got: %s", h.Summary())
	}
	if !h.IsSuccess() {
		t.Error("expected IsSuccess to be true")
	}
	if !h.IsFinished() {
		t.Error("expected IsFinished to be true")
	}
}

func TestHarnessRecursionGuardDenied(t *testing.T) {
	tempDir := t.TempDir()
	worktree := filepath.Join(tempDir, "work")
	if err := os.MkdirAll(worktree, 0700); err != nil {
		t.Fatalf("failed to create worktree: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cfg := HarnessConfig{
		WorktreeRoot: worktree,
		AllowedPaths: []string{"."},
		MaxDepth:     1,
		CurrentDepth: 1,
	}
	h, err := StartHarnessWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("failed to start harness: %v", err)
	}
	defer h.Close()

	resp := sendHarnessCall(t, h.SocketPath(), harnessRequest{
		Tool: "delegate",
	})
	if resp.Success || !strings.Contains(resp.Error, "RECURSIVE_DELEGATION_DENIED") {
		t.Errorf("expected RECURSIVE_DELEGATION_DENIED, got: %+v", resp)
	}

	// Client override attempt: sending current_depth=0 and max_depth=10 should NOT bypass server policy
	respOverride := sendHarnessCall(t, h.SocketPath(), harnessRequest{
		Tool:         "delegate",
		CurrentDepth: 0,
		MaxDepth:     10,
	})
	if respOverride.Success || !strings.Contains(respOverride.Error, "RECURSIVE_DELEGATION_DENIED") {
		t.Errorf("expected client override attempt to be DENIED, got: %+v", respOverride)
	}
}

func TestHarnessRecursionGuardEnvDenied(t *testing.T) {
	tempDir := t.TempDir()
	worktree := filepath.Join(tempDir, "work")
	if err := os.MkdirAll(worktree, 0700); err != nil {
		t.Fatalf("failed to create worktree: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	h, err := StartHarness(ctx, worktree, []string{"."}, 1)
	if err != nil {
		t.Fatalf("failed to start harness: %v", err)
	}
	defer h.Close()

	resp := sendHarnessCall(t, h.SocketPath(), harnessRequest{
		Tool: "delegate",
		Env:  []string{"AHENK_CHILD_SESSION=1", "AHENK_CHILD_DEPTH=1", "AHENK_MAX_DEPTH=1"},
	})
	if resp.Success || !strings.Contains(resp.Error, "RECURSIVE_DELEGATION_DENIED") {
		t.Errorf("expected RECURSIVE_DELEGATION_DENIED, got: %+v", resp)
	}
}

func TestHarnessJSONRPCProtocol(t *testing.T) {
	tempDir := t.TempDir()
	worktree := filepath.Join(tempDir, "work")
	if err := os.MkdirAll(worktree, 0700); err != nil {
		t.Fatalf("failed to create worktree: %v", err)
	}
	targetFile := filepath.Join(worktree, "hello.txt")
	if err := os.WriteFile(targetFile, []byte("hello world"), 0644); err != nil {
		t.Fatalf("failed to write test file: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	h, err := StartHarness(ctx, worktree, []string{"hello.txt"}, 1)
	if err != nil {
		t.Fatalf("failed to start harness: %v", err)
	}
	defer h.Close()

	// JSON-RPC call with method and params
	req := harnessRequest{
		JSONRPC: "2.0",
		ID:      101,
		Method:  "read_file",
		Params:  map[string]any{"path": "hello.txt"},
	}
	resp := sendHarnessCall(t, h.SocketPath(), req)
	if !resp.Success || resp.Data != "hello world" {
		t.Errorf("unexpected JSON-RPC response: %+v", resp)
	}
}

func TestHarnessIntermediateSymlinkDenied(t *testing.T) {
	tempDir := t.TempDir()
	worktree := filepath.Join(tempDir, "work")
	outsideDir := filepath.Join(tempDir, "outside")
	if err := os.MkdirAll(worktree, 0700); err != nil {
		t.Fatalf("failed to create worktree: %v", err)
	}
	if err := os.MkdirAll(outsideDir, 0700); err != nil {
		t.Fatalf("failed to create outside dir: %v", err)
	}

	outsideSecret := filepath.Join(outsideDir, "secret.txt")
	if err := os.WriteFile(outsideSecret, []byte("super_secret_token\n"), 0600); err != nil {
		t.Fatalf("failed to write outside secret: %v", err)
	}

	symlinkPath := filepath.Join(worktree, "escape_link")
	if err := os.Symlink(outsideDir, symlinkPath); err != nil {
		t.Fatalf("failed to create intermediate symlink: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	h, err := StartHarness(ctx, worktree, []string{"escape_link/new.txt"}, 1)
	if err != nil {
		t.Fatalf("failed to start harness: %v", err)
	}
	defer h.Close()

	// 1. Reading an existing file through intermediate symlink must be DENIED
	respRead := sendHarnessCall(t, h.SocketPath(), harnessRequest{
		Tool: "read_file",
		Path: "escape_link/secret.txt",
	})
	if respRead.Success || respRead.Error != "POLICY_DENIED: path outside sandbox boundary" {
		t.Errorf("expected intermediate symlink read to be denied, got: %+v", respRead)
	}

	// 2. Writing/creating a new file through intermediate symlink must be DENIED
	respEdit := sendHarnessCall(t, h.SocketPath(), harnessRequest{
		Tool: "edit_file",
		Path: "escape_link/new.txt",
		Data: "malicious write\n",
	})
	if respEdit.Success || respEdit.Error != "POLICY_DENIED: path outside sandbox boundary" {
		t.Errorf("expected intermediate symlink edit to be denied, got: %+v", respEdit)
	}
}

func TestHarnessAllowedPathsSymlinkBypassDenied(t *testing.T) {
	tempDir := t.TempDir()
	worktree := filepath.Join(tempDir, "work")
	if err := os.MkdirAll(filepath.Join(worktree, "allowed"), 0700); err != nil {
		t.Fatalf("failed to create allowed dir: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(worktree, "protected"), 0700); err != nil {
		t.Fatalf("failed to create protected dir: %v", err)
	}

	secretFile := filepath.Join(worktree, "protected", "secret.txt")
	if err := os.WriteFile(secretFile, []byte("ORIGINAL_SECRET"), 0644); err != nil {
		t.Fatalf("failed to write protected secret file: %v", err)
	}

	// 1. Symlink to existing protected file within allowed directory
	linkToFile := filepath.Join(worktree, "allowed", "link_secret.txt")
	if err := os.Symlink(filepath.Join("..", "protected", "secret.txt"), linkToFile); err != nil {
		t.Fatalf("failed to create symlink: %v", err)
	}

	// 2. Symlink to protected directory within allowed directory
	linkToDir := filepath.Join(worktree, "allowed", "link_dir")
	if err := os.Symlink(filepath.Join("..", "protected"), linkToDir); err != nil {
		t.Fatalf("failed to create symlink to dir: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	h, err := StartHarness(ctx, worktree, []string{"allowed"}, 1)
	if err != nil {
		t.Fatalf("failed to start harness: %v", err)
	}
	defer h.Close()

	// Attempt 1: Edit existing protected file through symlink in allowed directory
	respFile := sendHarnessCall(t, h.SocketPath(), harnessRequest{
		Tool: "edit_file",
		Path: "allowed/link_secret.txt",
		Data: "TAMPERED_SECRET",
	})
	if respFile.Success || !strings.Contains(respFile.Error, "POLICY_DENIED") {
		t.Errorf("expected POLICY_DENIED on symlink bypass to protected file, got: %+v", respFile)
	}

	// Verify protected file was NOT modified
	content, err := os.ReadFile(secretFile)
	if err != nil {
		t.Fatalf("failed to read protected file: %v", err)
	}
	if string(content) != "ORIGINAL_SECRET" {
		t.Errorf("protected file content was tampered! got: %s", string(content))
	}

	// Attempt 2: Create new file inside protected directory through directory symlink in allowed
	respNewFile := sendHarnessCall(t, h.SocketPath(), harnessRequest{
		Tool: "edit_file",
		Path: "allowed/link_dir/tampered.txt",
		Data: "MALICIOUS_NEW_FILE",
	})
	if respNewFile.Success || !strings.Contains(respNewFile.Error, "POLICY_DENIED") {
		t.Errorf("expected POLICY_DENIED on creating file through directory symlink, got: %+v", respNewFile)
	}

	// Verify new file was NOT created in protected directory
	tamperedFile := filepath.Join(worktree, "protected", "tampered.txt")
	if _, err := os.Stat(tamperedFile); !os.IsNotExist(err) {
		t.Errorf("malicious file was created in protected dir through directory symlink!")
	}
}


