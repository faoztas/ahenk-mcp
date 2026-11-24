// Copyright 2026 Furkan Anıl Öztaş
// SPDX-License-Identifier: Apache-2.0

package ipc

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"testing/iotest"
	"time"

	"github.com/faoztas/ahenk-mcp/internal/model"
)

type harnessRequest struct {
	AttemptID    string   `json:"attempt_id,omitempty"`
	CheckID      string   `json:"check_id,omitempty"`
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
	t.Helper()
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

func TestHarnessServerCloseWaitsForAndClosesConnections(t *testing.T) {
	tempDir := t.TempDir()
	worktree := filepath.Join(tempDir, "work")
	if err := os.MkdirAll(worktree, 0700); err != nil {
		t.Fatalf("failed to create worktree: %v", err)
	}
	if err := os.WriteFile(filepath.Join(worktree, "a.txt"), []byte("ok"), 0644); err != nil {
		t.Fatalf("failed to write test file: %v", err)
	}

	h, err := StartHarness(context.Background(), worktree, []string{"a.txt"}, 1)
	if err != nil {
		t.Fatalf("failed to start harness: %v", err)
	}

	conn, err := DialHarness(h.SocketPath(), 2*time.Second)
	if err != nil {
		_ = h.Close()
		t.Fatalf("failed to dial harness: %v", err)
	}
	defer conn.Close()

	// Ensure handleConn is active by sending one request and reading its response.
	if err := json.NewEncoder(conn).Encode(harnessRequest{Tool: "read_file", Path: "a.txt"}); err != nil {
		_ = h.Close()
		t.Fatalf("encode: %v", err)
	}
	var resp harnessResponse
	if err := json.NewDecoder(conn).Decode(&resp); err != nil {
		_ = h.Close()
		t.Fatalf("decode: %v", err)
	}
	if !resp.Success || resp.Data != "ok" {
		_ = h.Close()
		t.Fatalf("unexpected resp: %+v", resp)
	}

	closeDone := make(chan error, 1)
	go func() {
		closeDone <- h.Close()
	}()

	select {
	case err := <-closeDone:
		if err != nil {
			t.Fatalf("HarnessServer.Close returned error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("HarnessServer.Close hung with active idle client connection")
	}

	// Verify active client connection was closed by Close().
	_ = conn.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
	buf := make([]byte, 1)
	if _, err := conn.Read(buf); err == nil {
		t.Fatal("expected idle client connection to be closed on HarnessServer.Close")
	} else if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
		t.Fatalf("expected connection EOF/close, got timeout: %v", err)
	}
}

func TestHarnessServer_CheckRecursionRespectsConfigEnv(t *testing.T) {
	s := NewHarnessServer(HarnessConfig{
		WorktreeRoot: t.TempDir(),
		MaxDepth:     2,
		CurrentDepth: 0,
		Env:          []string{"AHENK_CHILD_SESSION=1", "AHENK_CHILD_DEPTH=1", "AHENK_MAX_DEPTH=1"},
	})
	err := s.CheckRecursion(nil)
	if err == nil {
		t.Fatal("expected CheckRecursion(nil) to deny based on s.cfg.Env")
	}
	var fault *model.Fault
	if !errors.As(err, &fault) || fault.Gate != "RECURSIVE_DELEGATION_DENIED" {
		t.Fatalf("expected RECURSIVE_DELEGATION_DENIED gate fault, got: %v", err)
	}
}

func TestHarnessServer_ReadFileSizeLimit(t *testing.T) {
	tempDir := t.TempDir()
	worktree := filepath.Join(tempDir, "work")
	if err := os.MkdirAll(worktree, 0700); err != nil {
		t.Fatalf("failed to create worktree: %v", err)
	}
	bigFile := filepath.Join(worktree, "huge.bin")
	// 4 MiB + 1 KiB exceeds the 4 MiB bound.
	if err := os.WriteFile(bigFile, make([]byte, (4<<20)+1024), 0644); err != nil {
		t.Fatalf("failed to write huge file: %v", err)
	}

	h, err := StartHarness(context.Background(), worktree, []string{"huge.bin"}, 1)
	if err != nil {
		t.Fatalf("failed to start harness: %v", err)
	}
	defer h.Close()

	if _, err := h.ReadFile("huge.bin"); err == nil {
		t.Fatal("expected ReadFile on >4 MiB file to return a bounded error")
	}
}

func TestHarnessServer_JSONRPCErrorShape(t *testing.T) {
	tempDir := t.TempDir()
	worktree := filepath.Join(tempDir, "work")
	if err := os.MkdirAll(worktree, 0700); err != nil {
		t.Fatalf("failed to create worktree: %v", err)
	}

	h, err := StartHarness(context.Background(), worktree, []string{"hello.txt"}, 1)
	if err != nil {
		t.Fatalf("failed to start harness: %v", err)
	}
	defer h.Close()

	conn, err := DialHarness(h.SocketPath(), 2*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	req := harnessRequest{
		JSONRPC: "2.0",
		ID:      42,
		Method:  "read_file",
		Params:  map[string]any{"path": "../../etc/passwd"},
	}
	if err := json.NewEncoder(conn).Encode(req); err != nil {
		t.Fatalf("encode: %v", err)
	}

	var rpcResp struct {
		JSONRPC string           `json:"jsonrpc"`
		ID      any              `json:"id"`
		Result  any              `json:"result,omitempty"`
		Error   *HarnessRPCError `json:"error,omitempty"`
	}
	if err := json.NewDecoder(conn).Decode(&rpcResp); err != nil {
		t.Fatalf("expected structured JSON-RPC error object, decode failed: %v", err)
	}
	if rpcResp.JSONRPC != "2.0" {
		t.Fatalf("expected jsonrpc=2.0, got %q", rpcResp.JSONRPC)
	}
	if rpcResp.Error == nil {
		t.Fatalf("expected non-nil error object in JSON-RPC response: %+v", rpcResp)
	}
	if rpcResp.Error.Code == 0 || rpcResp.Error.Message == "" {
		t.Fatalf("expected populated error {code, message}, got %+v", rpcResp.Error)
	}
}

func TestHarnessCheckOnlyRegisteredIDs(t *testing.T) {
	root := privateHarnessDir(t)
	approved := "fixture.format"
	checks := map[string]HarnessCheck{
		approved: func(ctx context.Context) error {
			return os.WriteFile(filepath.Join(root, "ran"), []byte("ok"), 0600)
		},
		"fixture.fail": func(context.Context) error { return errors.New("fixture failure") },
	}
	h, err := StartHarnessWithConfig(t.Context(), HarnessConfig{WorktreeRoot: root, AttemptID: "one", Checks: checks})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	checks["late"] = checks[approved]
	for _, req := range []harnessRequest{
		{Tool: "check", AttemptID: "one", CheckID: "late"},
		{Tool: "check", AttemptID: "one", CheckID: "unknown"},
		{Tool: "check", AttemptID: "other", CheckID: approved},
		{Tool: "check", CheckID: approved},
	} {
		resp := sendHarnessCall(t, h.SocketPath(), req)
		if resp.Success || !strings.Contains(resp.Error, "POLICY_DENIED") {
			t.Errorf("denied check: %+v", resp)
		}
	}
	if _, err := os.Stat(filepath.Join(root, "ran")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("denied check ran: %v", err)
	}
	resp := sendHarnessCall(t, h.SocketPath(), harnessRequest{Method: "tools/call", Params: map[string]any{"name": "check", "arguments": map[string]string{"attempt_id": "one", "check_id": approved}}})
	if !resp.Success {
		t.Errorf("registered check failed: %+v", resp)
	}
	if b, err := os.ReadFile(filepath.Join(root, "ran")); err != nil || string(b) != "ok" {
		t.Errorf("missing check result: %q, %v", b, err)
	}
	resp = sendHarnessCall(t, h.SocketPath(), harnessRequest{Tool: "check", AttemptID: "one", CheckID: "fixture.fail"})
	if resp.Success || !strings.Contains(resp.Error, "CHECK_FAILED") {
		t.Errorf("failed registered check: %+v", resp)
	}
	if h.IsFinished() || h.IsSuccess() {
		t.Fatal("check changed attempt completion")
	}
}

func TestHarnessAttemptScopeKeepsFilesAndSessionsSeparate(t *testing.T) {
	root := privateHarnessDir(t)
	for _, file := range []string{"inside", "other"} {
		if err := os.WriteFile(filepath.Join(root, file), []byte(file), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink("other", filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	h, err := StartHarnessWithConfig(t.Context(), HarnessConfig{WorktreeRoot: root, AttemptID: "one", CurrentDepth: 1, MaxDepth: 1, AllowedPaths: []string{"inside", "link"}})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	for _, req := range []harnessRequest{
		{Tool: "read_file", AttemptID: "two", Path: "inside"},
		{Tool: "edit_file", AttemptID: "two", Path: "inside", Content: "changed"},
		{Tool: "finish", AttemptID: "two", Summary: "wrong"},
		{Tool: "read_file", AttemptID: "one", Path: "other"},
		{Tool: "edit_file", AttemptID: "one", Path: "other", Content: "changed"},
		{Tool: "read_file", AttemptID: "one", Path: "link"},
		{Tool: "edit_file", AttemptID: "one", Path: "link", Content: "changed"},
		{Tool: "delegate_task", AttemptID: "one"},
	} {
		resp := sendHarnessCall(t, h.SocketPath(), req)
		if resp.Success || resp.Data != "" || resp.Error == "" {
			t.Errorf("allowed %+v: %+v", req, resp)
		}
	}
	if h.IsFinished() || h.Summary() != "" {
		t.Fatal("denied request finished attempt")
	}
	for _, file := range []string{"inside", "other"} {
		if b, err := os.ReadFile(filepath.Join(root, file)); err != nil || string(b) != file {
			t.Errorf("denied request modified %s: %q, %v", file, b, err)
		}
	}
	if r := sendHarnessCall(t, h.SocketPath(), harnessRequest{Tool: "read_file", AttemptID: "one", Path: "inside"}); !r.Success || r.Data != "inside" {
		t.Errorf("scoped read: %+v", r)
	}
}

func TestHarnessClosesConnectionsAndPrivateSocketOnAttemptExit(t *testing.T) {
	for _, exit := range []string{"eof", "cancel", "timeout"} {
		t.Run(exit, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), strings.Repeat("long", 32))
			if err := os.Mkdir(root, 0700); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if exit == "timeout" {
				var stop context.CancelFunc
				ctx, stop = context.WithTimeout(ctx, 1200*time.Millisecond)
				defer stop()
			}
			h, err := StartHarness(ctx, root, []string{"."}, 1)
			if err != nil {
				t.Fatal(err)
			}
			defer h.Close()
			conn, err := net.DialTimeout("unix", h.SocketPath(), time.Second)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			if exit == "eof" {
				if err := h.Close(); err != nil {
					t.Fatal(err)
				}
			} else if exit == "cancel" {
				cancel()
			}
			if err := conn.SetReadDeadline(time.Now().Add(4 * time.Second)); err != nil {
				t.Fatal(err)
			}
			if _, err := conn.Read(make([]byte, 1)); err == nil {
				t.Error("connection remains usable")
			} else if e, ok := err.(net.Error); ok && e.Timeout() {
				t.Fatal("attempt exit left an idle connection open")
			}
			if err := h.Close(); err != nil {
				t.Fatalf("explicit cleanup after attempt exit: %v", err)
			}
			for _, path := range []string{h.SocketPath(), h.ActualSocketPath(), h.aliasDir} {
				if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
					t.Errorf("socket remains at %s: %v", path, err)
				}
			}
		})
	}
}

func TestHarnessAttemptPinsWorktreeAndRefusesSharedFiles(t *testing.T) {
	base := filepath.Join(t.TempDir(), ".workspace")
	if err := os.Mkdir(base, 0700); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(base, "work")
	other := filepath.Join(base, "other")
	for _, dir := range []string{root, other} {
		if err := os.Mkdir(dir, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "note"), []byte(filepath.Base(dir)), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Link(filepath.Join(other, "note"), filepath.Join(root, "shared")); err != nil {
		t.Fatal(err)
	}
	h, err := StartHarnessWithConfig(t.Context(), HarnessConfig{WorktreeRoot: root, SocketPath: filepath.Join(base, "private.sock"), AllowedPaths: []string{"note", "shared"}, AttemptID: "one"})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	if r := sendHarnessCall(t, h.SocketPath(), harnessRequest{Tool: "edit_file", AttemptID: "one", Path: "shared", Content: "changed"}); r.Success {
		t.Error("attempt wrote a shared file")
	}
	if err := os.Rename(root, filepath.Join(base, "original")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(other, root); err != nil {
		t.Fatal(err)
	}
	r := sendHarnessCall(t, h.SocketPath(), harnessRequest{Tool: "read_file", AttemptID: "one", Path: "note"})
	if r.Success && r.Data != "work" {
		t.Errorf("read replacement worktree: %+v", r)
	}
	if b, err := os.ReadFile(filepath.Join(other, "note")); err != nil || string(b) != "other" {
		t.Errorf("shared file changed: %q %v", b, err)
	}
}

func TestHarnessCancelInterruptsRegisteredCheck(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	started, stopped := make(chan struct{}), make(chan struct{})
	h, err := StartHarnessWithConfig(ctx, HarnessConfig{WorktreeRoot: privateHarnessDir(t), AttemptID: "one", Checks: map[string]HarnessCheck{"wait": func(ctx context.Context) error {
		close(started)
		<-ctx.Done()
		close(stopped)
		return ctx.Err()
	}}})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	conn, err := DialHarness(h.SocketPath(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := json.NewEncoder(conn).Encode(harnessRequest{Tool: "check", AttemptID: "one", CheckID: "wait"}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("check never started")
	}
	cancel()
	select {
	case <-stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("check outlived attempt cancellation")
	}
}

func TestHarnessAttemptFinishIsTerminal(t *testing.T) {
	root := privateHarnessDir(t)
	h, err := StartHarnessWithConfig(t.Context(), HarnessConfig{WorktreeRoot: root, AttemptID: "one", AllowedPaths: []string{"note"}, ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	edit := harnessRequest{Tool: "edit_file", AttemptID: "one", Path: "note", Content: "updated"}
	if r := sendHarnessCall(t, h.SocketPath(), edit); r.Success {
		t.Fatal("read-only attempt wrote a file")
	}
	if r := sendHarnessCall(t, h.SocketPath(), harnessRequest{Tool: "finish", AttemptID: "one", Summary: "first"}); !r.Success {
		t.Fatal(r)
	}
	if r := sendHarnessCall(t, h.SocketPath(), harnessRequest{Tool: "finish", AttemptID: "one", Summary: "second"}); r.Success || h.Summary() != "first" {
		t.Fatal("worker rewrote its completion report")
	}
	if _, err := os.Stat(filepath.Join(root, "note")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("read-only attempt changed worktree")
	}
}

func TestHarnessClosePreservesReplacementSocketEntry(t *testing.T) {
	h, err := StartHarness(t.Context(), privateHarnessDir(t), nil, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	path := h.ActualSocketPath()
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("replacement"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := h.Close(); err == nil {
		t.Fatal("replaced endpoint accepted as clean shutdown")
	}
	if b, err := os.ReadFile(path); err != nil || string(b) != "replacement" {
		t.Fatalf("replacement lost: %q %v", b, err)
	}
}

func privateHarnessDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestHarnessRequiresIdentityForRegisteredChecks(t *testing.T) {
	root := privateHarnessDir(t)
	h, err := StartHarnessWithConfig(t.Context(), HarnessConfig{WorktreeRoot: root, Checks: map[string]HarnessCheck{"fixture": func(context.Context) error { return nil }}})
	if err == nil {
		h.Close()
		t.Fatal("started an unbound check endpoint")
	}
	files, err := os.ReadDir(root)
	if err != nil || len(files) != 0 {
		t.Fatalf("attempt root after rejection: %v %v", files, err)
	}
	h, err = StartHarness(t.Context(), root, nil, 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestHarnessScopedSiblingSocketsRemainIndependent(t *testing.T) {
	workspace := filepath.Join(t.TempDir(), ".workspace")
	for _, id := range []string{"one", "two"} {
		if err := os.MkdirAll(filepath.Join(workspace, id), 0700); err != nil {
			t.Fatal(err)
		}
	}
	start := func(id string) *HarnessServer {
		h, err := StartHarnessWithConfig(t.Context(), HarnessConfig{WorktreeRoot: filepath.Join(workspace, id), AttemptID: id})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = h.Close() })
		return h
	}
	first, second := start("one"), start("two")
	info, err := os.Lstat(second.ActualSocketPath())
	if err != nil || info.Mode()&os.ModeSocket == 0 || info.Mode().Perm() != 0600 || int(info.Sys().(*syscall.Stat_t).Uid) != os.Geteuid() {
		t.Fatalf("unsafe attempt socket: %v %v", info, err)
	}
	if first.ActualSocketPath() == second.ActualSocketPath() {
		t.Fatal("siblings share endpoint")
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	if r := sendHarnessCall(t, second.SocketPath(), harnessRequest{Tool: "finish", AttemptID: "two", Summary: "two done"}); !r.Success || second.Summary() != "two done" {
		t.Fatal(r)
	}
}

func TestHarnessRejectsUnownedOrUnconfinedSocketParent(t *testing.T) {
	info, err := os.Lstat(privateHarnessDir(t))
	if err != nil {
		t.Fatal(err)
	}
	if harnessDirectoryOwnedBy(info, os.Geteuid()+1) {
		t.Fatal("accepted private directory owned by another identity")
	}
	for _, scenario := range []string{"workspace symlink", "worktree below workspace symlink", "foreign explicit parent", "shared writable parent", "nonprivate parent"} {
		t.Run(scenario, func(t *testing.T) {
			root, external := privateHarnessDir(t), privateHarnessDir(t)
			cfg := HarnessConfig{WorktreeRoot: root, AttemptID: "one"}
			switch scenario {
			case "workspace symlink":
				if err := os.Symlink(external, filepath.Join(root, ".workspace")); err != nil {
					t.Fatal(err)
				}
			case "worktree below workspace symlink":
				if err := os.Symlink(external, filepath.Join(root, ".workspace")); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(filepath.Join(external, "work"), 0700); err != nil {
					t.Fatal(err)
				}
				cfg.WorktreeRoot = filepath.Join(root, ".workspace", "work")
			case "foreign explicit parent":
				cfg.SocketPath = filepath.Join(external, "socket")
			case "shared writable parent":
				if err := os.Chmod(root, 0777); err != nil {
					t.Fatal(err)
				}
			case "nonprivate parent":
				if err := os.Chmod(root, 0755); err != nil {
					t.Fatal(err)
				}
			}
			h, err := StartHarnessWithConfig(t.Context(), cfg)
			if err == nil {
				h.Close()
				t.Fatal("opened endpoint in unsafe parent")
			}
			if err := filepath.WalkDir(external, func(path string, d os.DirEntry, err error) error {
				if err == nil && d.Type()&os.ModeSocket != 0 {
					t.Errorf("socket outside attempt: %s", path)
				}
				return err
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestHarnessBoundsFramesAndEdits(t *testing.T) {
	root := filepath.Join(privateHarnessDir(t), "work")
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(root, "note")
	if err := os.WriteFile(p, []byte("before"), 0600); err != nil {
		t.Fatal(err)
	}
	h, err := StartHarness(t.Context(), root, []string{"note"}, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	if err := h.EditFile("note", strings.Repeat("a", maxHarnessReadBytes+1), "before"); err == nil {
		t.Error("oversize edit accepted")
	}
	c, err := net.Dial("unix", h.SocketPath())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	enc, dec := json.NewEncoder(c), json.NewDecoder(c)
	for range 3 {
		if err := enc.Encode(HarnessRequest{Tool: "read_file", Path: "note"}); err != nil {
			t.Fatal(err)
		}
		var resp HarnessResponse
		if err := dec.Decode(&resp); err != nil || !resp.Success || resp.Data != "before" {
			t.Errorf("read after rejected edit: %v", err)
			break
		}
	}
	// An ignored padding member still counts against the frame budget.
	err = enc.Encode(map[string]string{"tool": "edit_file", "path": "note", "content": "oversized frame", "padding": strings.Repeat("x", 9<<20)})
	if err == nil {
		var resp HarnessResponse
		if dec.Decode(&resp) == nil && resp.Success {
			t.Error("oversized frame dispatched")
		}
	}
	if b, _ := os.ReadFile(p); string(b) != "before" {
		t.Error("oversize input modified original")
	}
}

func TestHarnessPartialEditPreservesOriginal(t *testing.T) {
	dir := privateHarnessDir(t)
	parent, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer parent.Close()
	if err := os.WriteFile(filepath.Join(dir, "note"), []byte("before"), 0600); err != nil {
		t.Fatal(err)
	}
	injected := errors.New("fixture write source failure")
	for _, name := range []string{"note", "new"} {
		input := io.MultiReader(strings.NewReader("partial"), iotest.ErrReader(injected))
		if err := publishHarnessEdit(parent, name, input, ""); !errors.Is(err, injected) {
			t.Fatal("expected injected error", err)
		}
	}
	data, err := parent.ReadFile("note")
	if err != nil || string(data) != "before" {
		t.Fatal("original changed", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 {
		t.Fatal("partial edit left an entry", err)
	}
}
