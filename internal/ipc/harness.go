// Copyright 2026 Furkan Anıl Öztaş
// SPDX-License-Identifier: Apache-2.0

package ipc

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/faoztas/ahenk-mcp/internal/model"
	"github.com/faoztas/ahenk-mcp/internal/providers"
)

var chdirHarnessMu sync.Mutex

// HarnessConfig specifies the sandbox boundary and policy for an isolated subagent harness.
type HarnessConfig struct {
	WorktreeRoot string
	AllowedPaths []string
	MaxDepth     int
	CurrentDepth int
	SocketPath   string
	Env          []string
}

// HarnessRequest represents an incoming command or JSON-RPC tool invocation.
type HarnessRequest struct {
	JSONRPC      string          `json:"jsonrpc,omitempty"`
	ID           any             `json:"id,omitempty"`
	Method       string          `json:"method,omitempty"`
	Params       json.RawMessage `json:"params,omitempty"`
	Tool         string          `json:"tool,omitempty"`
	Path         string          `json:"path,omitempty"`
	Data         string          `json:"data,omitempty"`
	Content      string          `json:"content,omitempty"`
	OldContent   string          `json:"old_content,omitempty"`
	Summary      string          `json:"summary,omitempty"`
	Success      *bool           `json:"success,omitempty"`
	CurrentDepth int             `json:"current_depth,omitempty"`
	MaxDepth     int             `json:"max_depth,omitempty"`
	Env          []string        `json:"env,omitempty"`
}

// HarnessRPCError represents a JSON-RPC 2.0 error response object.
type HarnessRPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// HarnessResponse represents the outgoing execution result over the harness socket.
type HarnessResponse struct {
	JSONRPC string `json:"jsonrpc,omitempty"`
	ID      any    `json:"id,omitempty"`
	Result  any    `json:"result,omitempty"`
	Success bool   `json:"success"`
	Data    string `json:"data,omitempty"`
	Error   string `json:"error,omitempty"`
}

type jsonRPCResponse struct {
	JSONRPC string           `json:"jsonrpc"`
	ID      any              `json:"id"`
	Result  any              `json:"result,omitempty"`
	Error   *HarnessRPCError `json:"error,omitempty"`
}

// HarnessServer serves an isolated two-way tool harness over a Unix domain socket.
type HarnessServer struct {
	cfg         HarnessConfig
	listener    net.Listener
	sockPath    string
	symlinkPath string
	summary     string
	finished    bool
	success     bool
	mu          sync.Mutex
	ctx         context.Context
	cancel      context.CancelFunc
	wg          sync.WaitGroup
}

// Harness is an alias for HarnessServer.
type Harness = HarnessServer

// NewHarnessServer initializes a new harness server with the provided configuration.
func NewHarnessServer(cfg HarnessConfig) *HarnessServer {
	return &HarnessServer{
		cfg: HarnessConfig{
			WorktreeRoot: filepath.Clean(cfg.WorktreeRoot),
			AllowedPaths: slices.Clone(cfg.AllowedPaths),
			MaxDepth:     cfg.MaxDepth,
			CurrentDepth: cfg.CurrentDepth,
			SocketPath:   cfg.SocketPath,
			Env:          slices.Clone(cfg.Env),
		},
	}
}

// Start listens on the configured Unix domain socket and begins serving connections.
func (s *HarnessServer) Start(ctx context.Context) error {
	actualPath := determineSocketPath(s.cfg)
	if err := os.MkdirAll(filepath.Dir(actualPath), 0700); err != nil {
		return err
	}
	_ = os.Remove(actualPath)

	ln, err := listenHarnessUnix(actualPath)
	if err != nil {
		return err
	}
	if err := os.Chmod(actualPath, 0600); err != nil {
		_ = ln.Close()
		return err
	}
	s.listener = ln
	s.sockPath = actualPath

	if len(actualPath) >= 104 {
		h := sha256.Sum256([]byte(actualPath))
		shortPath := filepath.Join("/tmp", fmt.Sprintf("ah_%x.sock", h[:8]))
		_ = os.Remove(shortPath)
		if err := os.Symlink(actualPath, shortPath); err == nil {
			s.symlinkPath = shortPath
		}
	}

	hctx, cancel := context.WithCancel(ctx)
	s.ctx = hctx
	s.cancel = cancel

	s.wg.Add(1)
	go s.acceptLoop()
	return nil
}

// StartHarness creates and starts a HarnessServer for the given worktree directory.
func StartHarness(ctx context.Context, worktreeDir string, allowedPaths []string, maxDepth int) (*HarnessServer, error) {
	s := NewHarnessServer(HarnessConfig{
		WorktreeRoot: worktreeDir,
		AllowedPaths: allowedPaths,
		MaxDepth:     maxDepth,
	})
	if err := s.Start(ctx); err != nil {
		return nil, err
	}
	return s, nil
}

// StartHarnessWithConfig creates and starts a HarnessServer with a full HarnessConfig.
func StartHarnessWithConfig(ctx context.Context, cfg HarnessConfig) (*HarnessServer, error) {
	s := NewHarnessServer(cfg)
	if err := s.Start(ctx); err != nil {
		return nil, err
	}
	return s, nil
}

// SocketPath returns the dialable Unix domain socket path (using a short symlink if needed for Darwin's 104-byte limit).
func (s *HarnessServer) SocketPath() string {
	if s.symlinkPath != "" {
		return s.symlinkPath
	}
	return s.sockPath
}

// ActualSocketPath returns the exact file system location of the socket in the worktree.
func (s *HarnessServer) ActualSocketPath() string {
	return s.sockPath
}

// Summary returns the final summary reported by the subagent.
func (s *HarnessServer) Summary() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.summary
}

// IsSuccess reports whether the subagent finished with success=true.
func (s *HarnessServer) IsSuccess() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.success
}

// IsFinished reports whether finish has been called.
func (s *HarnessServer) IsFinished() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.finished
}

// Close terminates listener and active connections, cleaning up the socket file and any symlink.
func (s *HarnessServer) Close() error {
	if s.cancel != nil {
		s.cancel()
	}
	var err error
	if s.listener != nil {
		err = s.listener.Close()
	}
	s.wg.Wait()
	if s.symlinkPath != "" {
		_ = os.Remove(s.symlinkPath)
	}
	if s.sockPath != "" {
		_ = os.Remove(s.sockPath)
	}
	return err
}

func (s *HarnessServer) acceptLoop() {
	defer s.wg.Done()
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			if s.ctx.Err() != nil {
				return
			}
			continue
		}
		go s.handleConn(conn)
	}
}

func (s *HarnessServer) handleConn(conn net.Conn) {
	defer conn.Close()
	decoder := json.NewDecoder(conn)
	encoder := json.NewEncoder(conn)

	for {
		var req HarnessRequest
		if err := decoder.Decode(&req); err != nil {
			return
		}
		resp := s.dispatch(req)
		if req.JSONRPC != "" || req.ID != nil {
			resp.JSONRPC = "2.0"
			resp.ID = req.ID
			if resp.Success {
				resp.Result = map[string]any{"data": resp.Data, "success": true}
			}
		}
		if err := encoder.Encode(resp); err != nil {
			return
		}
	}
}

// ReadFile safely reads a file from within the worktree boundary.
func (s *HarnessServer) ReadFile(relPath string) (string, error) {
	resolvedPath, err := s.validatePath(relPath, false)
	if err != nil {
		return "", err
	}
	data, err := os.ReadFile(resolvedPath)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// EditFile safely modifies or creates a file within the AllowedPaths boundary.
func (s *HarnessServer) EditFile(relPath, content, oldContent string) error {
	resolvedPath, err := s.validatePath(relPath, true)
	if err != nil {
		return err
	}

	canonicalRoot, err := filepath.EvalSymlinks(s.cfg.WorktreeRoot)
	if err != nil {
		canonicalRoot = filepath.Clean(s.cfg.WorktreeRoot)
	}
	lexicalTarget := filepath.Join(canonicalRoot, filepath.Clean(relPath))

	// Reject if lexical path is a symlink
	if fi, err := os.Lstat(lexicalTarget); err == nil && fi.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("POLICY_DENIED: symlink target not allowed")
	}

	if oldContent != "" {
		existing, err := os.ReadFile(resolvedPath)
		if err != nil {
			return fmt.Errorf("PRECONDITION_FAILED: target file does not exist: %w", err)
		}
		if string(existing) != oldContent {
			return fmt.Errorf("PRECONDITION_FAILED: existing content does not match old_content")
		}
	}
	if err := os.MkdirAll(filepath.Dir(resolvedPath), 0700); err != nil {
		return err
	}

	f, err := os.OpenFile(resolvedPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC|syscall.O_NOFOLLOW, 0644)
	if err != nil {
		if errors.Is(err, syscall.ELOOP) {
			return fmt.Errorf("POLICY_DENIED: symlink target not allowed")
		}
		return err
	}
	defer f.Close()

	if _, err := f.Write([]byte(content)); err != nil {
		return err
	}
	return nil
}

// Finish records the subagent completion status and summary.
func (s *HarnessServer) Finish(summary string, success bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.summary = summary
	s.success = success
	s.finished = true
	return nil
}

// validatePath enforces strict sandbox path fencing and AllowedPaths write permissions.
// Returns canonical resolved path on success.
func (s *HarnessServer) validatePath(relPath string, forWrite bool) (string, error) {
	if relPath == "" {
		return "", fmt.Errorf("POLICY_DENIED: path outside sandbox boundary")
	}
	clean := filepath.Clean(relPath)
	if strings.HasPrefix(clean, "..") || filepath.IsAbs(relPath) || filepath.IsAbs(clean) || !model.SafePath(clean) {
		return "", fmt.Errorf("POLICY_DENIED: path outside sandbox boundary")
	}

	canonicalRoot, err := filepath.EvalSymlinks(s.cfg.WorktreeRoot)
	if err != nil {
		canonicalRoot = filepath.Clean(s.cfg.WorktreeRoot)
	}

	fullPath := filepath.Join(canonicalRoot, clean)
	rel, within := model.Within(canonicalRoot, fullPath)
	if !within || strings.HasPrefix(rel, "..") {
		return "", fmt.Errorf("POLICY_DENIED: path outside sandbox boundary")
	}

	// Resolve symlinks for target path or deepest existing ancestor
	var resolvedPath string
	resolved, err := filepath.EvalSymlinks(fullPath)
	if err == nil {
		resolvedPath = resolved
	} else if os.IsNotExist(err) || errors.Is(err, os.ErrNotExist) {
		// File does not exist yet (e.g. for write/create). Find closest existing ancestor.
		curr := filepath.Dir(fullPath)
		for {
			resCurr, errCurr := filepath.EvalSymlinks(curr)
			if errCurr == nil {
				relFromCurr, errRel := filepath.Rel(curr, fullPath)
				if errRel != nil {
					return "", fmt.Errorf("POLICY_DENIED: path outside sandbox boundary")
				}
				resolvedPath = filepath.Clean(filepath.Join(resCurr, relFromCurr))
				break
			}
			parent := filepath.Dir(curr)
			if parent == curr {
				return "", fmt.Errorf("POLICY_DENIED: path outside sandbox boundary")
			}
			curr = parent
		}
	} else {
		return "", fmt.Errorf("POLICY_DENIED: path outside sandbox boundary")
	}

	// Verify resolvedPath is within canonicalRoot
	relToCanonical, err := filepath.Rel(canonicalRoot, resolvedPath)
	if err != nil || strings.HasPrefix(relToCanonical, "..") {
		return "", fmt.Errorf("POLICY_DENIED: path outside sandbox boundary")
	}
	if _, within := model.Within(canonicalRoot, resolvedPath); !within {
		return "", fmt.Errorf("POLICY_DENIED: path outside sandbox boundary")
	}

	if forWrite {
		relToCanonicalClean := filepath.Clean(relToCanonical)
		if !model.InScope(relToCanonicalClean, s.cfg.AllowedPaths) || !model.InScope(clean, s.cfg.AllowedPaths) {
			return "", fmt.Errorf("POLICY_DENIED: path not permitted for modification")
		}
	}
	return resolvedPath, nil
}

// CheckRecursion verifies depth against maxDepth and environment recursion markers.
func (s *HarnessServer) CheckRecursion(env []string) error {
	maxDepth := s.cfg.MaxDepth
	if maxDepth <= 0 {
		maxDepth = 1
	}
	if s.cfg.CurrentDepth >= maxDepth {
		return model.GateError("POLICY_DENIED", "RECURSIVE_DELEGATION_DENIED",
			"subagent session cannot recursively delegate tasks (AHENK_CHILD_SESSION=1, depth limit reached)",
			"complete_task_directly_in_subagent")
	}
	if len(env) > 0 {
		return providers.CheckRecursionGuard(env, maxDepth)
	}
	return providers.CheckRecursionGuard(os.Environ(), maxDepth)
}

func (s *HarnessServer) checkRecursion(req HarnessRequest) error {
	maxDepth := s.cfg.MaxDepth
	if maxDepth <= 0 {
		maxDepth = 1
	}
	if req.MaxDepth > 0 && req.MaxDepth < maxDepth {
		maxDepth = req.MaxDepth
	}

	currentDepth := max(s.cfg.CurrentDepth, req.CurrentDepth)

	if currentDepth >= maxDepth {
		return model.GateError("POLICY_DENIED", "RECURSIVE_DELEGATION_DENIED",
			"subagent session cannot recursively delegate tasks (AHENK_CHILD_SESSION=1, depth limit reached)",
			"complete_task_directly_in_subagent")
	}

	if len(req.Env) > 0 {
		if err := providers.CheckRecursionGuard(req.Env, maxDepth); err != nil {
			return err
		}
	}

	if len(s.cfg.Env) > 0 {
		if err := providers.CheckRecursionGuard(s.cfg.Env, maxDepth); err != nil {
			return err
		}
	}

	return providers.CheckRecursionGuard(os.Environ(), maxDepth)
}

func (s *HarnessServer) dispatch(req HarnessRequest) HarnessResponse {
	tool := req.Tool
	if tool == "" && req.Method != "" {
		tool = req.Method
	}

	if tool == "tools/call" && len(req.Params) > 0 {
		var toolCall struct {
			Name      string         `json:"name"`
			Arguments map[string]any `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &toolCall); err == nil {
			tool = toolCall.Name
			if p, ok := toolCall.Arguments["path"].(string); ok {
				req.Path = p
			}
			if c, ok := toolCall.Arguments["content"].(string); ok {
				req.Content = c
			}
			if d, ok := toolCall.Arguments["data"].(string); ok {
				req.Data = d
			}
			if o, ok := toolCall.Arguments["old_content"].(string); ok {
				req.OldContent = o
			}
			if sm, ok := toolCall.Arguments["summary"].(string); ok {
				req.Summary = sm
			}
			if sc, ok := toolCall.Arguments["success"].(bool); ok {
				req.Success = &sc
			}
		}
	} else if len(req.Params) > 0 {
		var paramMap map[string]any
		if err := json.Unmarshal(req.Params, &paramMap); err == nil {
			if p, ok := paramMap["path"].(string); ok && req.Path == "" {
				req.Path = p
			}
			if c, ok := paramMap["content"].(string); ok && req.Content == "" {
				req.Content = c
			}
			if d, ok := paramMap["data"].(string); ok && req.Data == "" {
				req.Data = d
			}
			if o, ok := paramMap["old_content"].(string); ok && req.OldContent == "" {
				req.OldContent = o
			}
			if sm, ok := paramMap["summary"].(string); ok && req.Summary == "" {
				req.Summary = sm
			}
			if sc, ok := paramMap["success"].(bool); ok && req.Success == nil {
				req.Success = &sc
			}
		}
	}

	switch tool {
	case "read_file":
		data, err := s.ReadFile(req.Path)
		if err != nil {
			return HarnessResponse{Success: false, Error: err.Error()}
		}
		return HarnessResponse{Success: true, Data: data}

	case "edit_file":
		content := req.Content
		if content == "" && req.Data != "" {
			content = req.Data
		}
		if err := s.EditFile(req.Path, content, req.OldContent); err != nil {
			return HarnessResponse{Success: false, Error: err.Error()}
		}
		return HarnessResponse{Success: true}

	case "finish":
		success := true
		if req.Success != nil {
			success = *req.Success
		}
		if err := s.Finish(req.Summary, success); err != nil {
			return HarnessResponse{Success: false, Error: err.Error()}
		}
		return HarnessResponse{Success: true}

	case "delegate", "delegate_task":
		if err := s.checkRecursion(req); err != nil {
			msg := err.Error()
			if !strings.Contains(msg, "RECURSIVE_DELEGATION_DENIED") {
				msg = "RECURSIVE_DELEGATION_DENIED: " + msg
			}
			return HarnessResponse{Success: false, Error: msg}
		}
		return HarnessResponse{Success: false, Error: "DELEGATION_NOT_AVAILABLE"}

	default:
		return HarnessResponse{Success: false, Error: "UNKNOWN_TOOL: " + tool}
	}
}

func determineSocketPath(cfg HarnessConfig) string {
	if cfg.SocketPath != "" {
		return cfg.SocketPath
	}
	if filepath.Base(cfg.WorktreeRoot) == ".workspace" {
		return filepath.Join(cfg.WorktreeRoot, ".harness.sock")
	}
	workspaceDir := filepath.Join(cfg.WorktreeRoot, ".workspace")
	if fi, err := os.Stat(workspaceDir); err == nil && fi.IsDir() {
		return filepath.Join(workspaceDir, ".harness.sock")
	}
	if filepath.Base(filepath.Dir(cfg.WorktreeRoot)) == ".workspace" {
		return filepath.Join(filepath.Dir(cfg.WorktreeRoot), ".harness.sock")
	}
	return filepath.Join(cfg.WorktreeRoot, ".harness.sock")
}

func listenHarnessUnix(sockPath string) (net.Listener, error) {
	if len(sockPath) < 104 {
		return net.Listen("unix", sockPath)
	}
	dir := filepath.Dir(sockPath)
	base := filepath.Base(sockPath)
	chdirHarnessMu.Lock()
	defer chdirHarnessMu.Unlock()
	wd, err := os.Getwd()
	if err != nil {
		return nil, err
	}
	if err := os.Chdir(dir); err != nil {
		return nil, err
	}
	defer func() {
		_ = os.Chdir(wd)
	}()
	return net.Listen("unix", base)
}

// DialHarness connects to a harness socket, handling long path constraints on macOS.
func DialHarness(sockPath string, timeout time.Duration) (net.Conn, error) {
	if len(sockPath) < 104 {
		return net.DialTimeout("unix", sockPath, timeout)
	}
	dir := filepath.Dir(sockPath)
	base := filepath.Base(sockPath)
	chdirHarnessMu.Lock()
	defer chdirHarnessMu.Unlock()
	wd, err := os.Getwd()
	if err != nil {
		return nil, err
	}
	if err := os.Chdir(dir); err != nil {
		return nil, err
	}
	defer func() {
		_ = os.Chdir(wd)
	}()
	return net.DialTimeout("unix", base, timeout)
}
