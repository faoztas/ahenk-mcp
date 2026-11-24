// Copyright 2026 Furkan Anıl Öztaş
// SPDX-License-Identifier: Apache-2.0

package ipc

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
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

const maxHarnessReadBytes = 4 << 20

// HarnessCheck runs a controller-registered check with its attempt context.
type HarnessCheck func(context.Context) error

// HarnessConfig specifies the sandbox boundary and policy for an isolated subagent harness.
type HarnessConfig struct {
	// AttemptID binds requests and read access to this attempt and its AllowedPaths.
	AttemptID    string
	Checks       map[string]HarnessCheck `json:"-"`
	ReadOnly     bool
	WorktreeRoot string
	AllowedPaths []string
	MaxDepth     int
	CurrentDepth int
	SocketPath   string
	Env          []string
}

// HarnessRequest represents an incoming command or JSON-RPC tool invocation.
type HarnessRequest struct {
	AttemptID    string          `json:"attempt_id,omitempty"`
	CheckID      string          `json:"check_id,omitempty"`
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
	Success bool             `json:"success"`
	Data    string           `json:"data,omitempty"`
}

// HarnessServer serves an isolated two-way tool harness over a Unix domain socket.
type HarnessServer struct {
	cfg          HarnessConfig
	root         *os.Root
	socketParent *os.Root
	socketInfo   os.FileInfo
	requestMu    sync.Mutex
	listener     net.Listener
	sockPath     string
	dialPath     string
	aliasDir     string
	closeOnce    sync.Once
	closeErr     error
	summary      string
	finished     bool
	success      bool
	mu           sync.Mutex
	connMu       sync.Mutex
	conns        map[net.Conn]struct{}
	closed       bool
	ctx          context.Context
	cancel       context.CancelFunc
	wg           sync.WaitGroup
}

// Harness is an alias for HarnessServer.
type Harness = HarnessServer

// NewHarnessServer initializes a new harness server with the provided configuration.
func NewHarnessServer(cfg HarnessConfig) *HarnessServer {
	cfg.WorktreeRoot = filepath.Clean(cfg.WorktreeRoot)
	cfg.AllowedPaths = slices.Clone(cfg.AllowedPaths)
	cfg.Env = slices.Clone(cfg.Env)
	cfg.Checks = maps.Clone(cfg.Checks)
	return &HarnessServer{cfg: cfg, conns: make(map[net.Conn]struct{})}
}

// Start listens on the configured Unix domain socket and begins serving connections.
func (s *HarnessServer) Start(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(s.cfg.Checks) > 0 && s.cfg.AttemptID == "" {
		return errors.New("POLICY_DENIED: registered checks require an attempt identity")
	}
	actualPath, parent, err := openHarnessSocketParent(s.cfg)
	if err != nil {
		return err
	}
	dialPath, aliasDir, err := harnessDialPath(actualPath)
	if err != nil {
		return errors.Join(err, parent.Close())
	}

	root, err := os.OpenRoot(s.cfg.WorktreeRoot)
	if err != nil {
		return errors.Join(err, parent.Close(), removeHarnessAlias(aliasDir))
	}
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: dialPath, Net: "unix"})
	if err != nil {
		return errors.Join(err, root.Close(), parent.Close(), removeHarnessAlias(aliasDir))
	}
	// Own the unlink explicitly so attempt cleanup can report its failure.
	ln.SetUnlinkOnClose(false)
	name := filepath.Base(actualPath)
	err = parent.Chmod(name, 0600)
	info, statErr := parent.Lstat(name)
	err = errors.Join(err, statErr)
	if err == nil && (info.Mode()&os.ModeSocket == 0 || info.Mode().Perm() != 0600 || int(info.Sys().(*syscall.Stat_t).Uid) != os.Geteuid()) {
		err = errors.New("POLICY_DENIED: harness socket must be private and owned by the current user")
	}
	if err != nil {
		return errors.Join(err, ln.Close(), parent.Remove(name), parent.Close(), root.Close(), removeHarnessAlias(aliasDir))
	}
	s.root = root
	s.socketParent = parent
	s.socketInfo = info
	s.listener = ln
	s.sockPath = actualPath
	s.dialPath = dialPath
	s.aliasDir = aliasDir

	hctx, cancel := context.WithCancel(ctx)
	s.ctx = hctx
	s.cancel = cancel

	s.wg.Add(1)
	go s.acceptLoop()
	context.AfterFunc(hctx, func() { _ = s.Close() })
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
	return s.dialPath
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
	s.closeOnce.Do(func() {
		if s.cancel != nil {
			s.cancel()
		}
		s.connMu.Lock()
		s.closed = true
		for conn := range s.conns {
			_ = conn.Close()
		}
		s.connMu.Unlock()
		if s.listener != nil {
			s.closeErr = s.listener.Close()
		}
		s.wg.Wait()
		if s.root != nil {
			s.closeErr = errors.Join(s.closeErr, s.root.Close())
		}
		if s.socketParent != nil {
			name := filepath.Base(s.sockPath)
			info, err := s.socketParent.Lstat(name)
			if err == nil {
				if info.Mode()&os.ModeSocket != 0 && os.SameFile(info, s.socketInfo) {
					err = s.socketParent.Remove(name)
				} else {
					err = errors.New("harness socket changed before cleanup")
				}
			}
			s.closeErr = errors.Join(s.closeErr, err, s.socketParent.Close())
		}
		s.closeErr = errors.Join(s.closeErr, removeHarnessAlias(s.aliasDir))
	})
	return s.closeErr
}

func (s *HarnessServer) acceptLoop() {
	defer s.wg.Done()
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			s.connMu.Lock()
			closed := s.closed
			s.connMu.Unlock()
			if s.ctx.Err() != nil || closed {
				return
			}
			continue
		}
		s.connMu.Lock()
		if s.closed {
			s.connMu.Unlock()
			_ = conn.Close()
			return
		}
		if s.conns == nil {
			s.conns = make(map[net.Conn]struct{})
		}
		s.conns[conn] = struct{}{}
		s.connMu.Unlock()

		s.wg.Add(1)
		go s.handleConn(conn)
	}
}

func (s *HarnessServer) handleConn(conn net.Conn) {
	defer s.wg.Done()
	defer func() {
		s.connMu.Lock()
		delete(s.conns, conn)
		s.connMu.Unlock()
		_ = conn.Close()
	}()
	// Carry decoder read-ahead into the next budget. Each complete JSON
	// value replenishes exactly the bytes it consumed, even when pipelined.
	const frameLimit = 2*maxHarnessReadBytes + 64<<10
	limited := &io.LimitedReader{R: conn, N: frameLimit}
	decoder := json.NewDecoder(limited)
	encoder := json.NewEncoder(conn)

	for {
		previous := decoder.InputOffset()
		var req HarnessRequest
		if err := decoder.Decode(&req); err != nil {
			return
		}
		limited.N += decoder.InputOffset() - previous
		resp := s.dispatch(req)
		if req.JSONRPC != "" || req.ID != nil {
			rpcResp := jsonRPCResponse{
				JSONRPC: "2.0",
				ID:      req.ID,
				Success: resp.Success,
				Data:    resp.Data,
			}
			if resp.Success {
				rpcResp.Result = map[string]any{"data": resp.Data, "success": true}
			} else {
				rpcResp.Error = &HarnessRPCError{
					Code:    harnessRPCErrorCode(resp.Error),
					Message: resp.Error,
				}
			}
			if err := encoder.Encode(rpcResp); err != nil {
				return
			}
			continue
		}
		if err := encoder.Encode(resp); err != nil {
			return
		}
	}
}

func harnessRPCErrorCode(msg string) int {
	switch {
	case strings.HasPrefix(msg, "UNKNOWN_TOOL:"):
		return -32601
	case strings.HasPrefix(msg, "POLICY_DENIED:"), strings.Contains(msg, "RECURSIVE_DELEGATION_DENIED"):
		return -32001
	case strings.HasPrefix(msg, "PRECONDITION_FAILED:"):
		return -32002
	default:
		return -32000
	}
}

// fileParent returns the target's pinned parent. Each OpenRoot is relative to
// its predecessor and must identify the directory inspected before opening.
func (s *HarnessServer) fileParent(relPath string, write, create bool) (*os.Root, string, error) {
	target, err := s.validatePath(relPath, write)
	if err != nil {
		return nil, "", err
	}
	var parent *os.Root
	if s.root == nil {
		parent, err = os.OpenRoot(s.cfg.WorktreeRoot)
	} else {
		parent, err = s.root.OpenRoot(".")
	}
	if err != nil {
		return nil, "", err
	}
	parts := strings.Split(target, string(filepath.Separator))
	for _, dir := range parts[:len(parts)-1] {
		if create {
			err := parent.Mkdir(dir, 0700)
			if err != nil && !errors.Is(err, os.ErrExist) {
				parent.Close()
				return nil, "", err
			}
		}
		info, err := parent.Lstat(dir)
		if err != nil {
			parent.Close()
			return nil, "", err
		}
		if !info.IsDir() {
			parent.Close()
			return nil, "", errors.New("POLICY_DENIED: path outside sandbox boundary")
		}
		next, err := parent.OpenRoot(dir)
		parent.Close()
		if err != nil {
			return nil, "", err
		}
		opened, err := next.Stat(".")
		if err != nil || !os.SameFile(info, opened) {
			next.Close()
			return nil, "", errors.New("POLICY_DENIED: directory changed")
		}
		parent = next
	}
	return parent, parts[len(parts)-1], nil
}

func openHarnessFile(parent *os.Root, name string, flags int) (*os.File, error) {
	expected, err := parent.Lstat(name)
	switch {
	case err == nil:
		if !expected.Mode().IsRegular() {
			return nil, errors.New("POLICY_DENIED: expected a regular file")
		}
		flags &^= os.O_CREATE
	case errors.Is(err, os.ErrNotExist) && flags&os.O_CREATE != 0:
		flags |= os.O_EXCL // A concurrently created entry must not be followed.
	default:
		return nil, err
	}
	f, err := parent.OpenFile(name, flags|syscall.O_NONBLOCK, 0644)
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Sys().(*syscall.Stat_t).Nlink != 1 || (expected != nil && !os.SameFile(expected, info)) {
		f.Close()
		return nil, errors.New("POLICY_DENIED: file changed or is shared")
	}
	return f, nil
}

// ReadFile safely reads a file from within the worktree boundary.
func (s *HarnessServer) ReadFile(relPath string) (string, error) {
	parent, name, err := s.fileParent(relPath, false, false)
	if err != nil {
		return "", err
	}
	defer parent.Close()
	f, err := openHarnessFile(parent, name, os.O_RDONLY)
	if err != nil {
		return "", err
	}
	defer f.Close()

	data, err := io.ReadAll(io.LimitReader(f, maxHarnessReadBytes+1))
	if err != nil {
		return "", err
	}
	if len(data) > maxHarnessReadBytes {
		return "", fmt.Errorf("POLICY_DENIED: file exceeds maximum read size (%d bytes)", maxHarnessReadBytes)
	}
	return string(data), nil
}

// EditFile safely modifies or creates a file within the AllowedPaths boundary.
func (s *HarnessServer) EditFile(relPath, content, oldContent string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if len(content) > maxHarnessReadBytes || len(oldContent) > maxHarnessReadBytes {
		return errors.New("POLICY_DENIED: edit exceeds maximum size")
	}
	if s.cfg.ReadOnly {
		return errors.New("POLICY_DENIED: read-only attempt")
	}
	parent, name, err := s.fileParent(relPath, true, oldContent == "")
	if err != nil {
		if oldContent != "" && errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("PRECONDITION_FAILED: target file does not exist: %w", err)
		}
		return err
	}
	defer parent.Close()

	return publishHarnessEdit(parent, name, strings.NewReader(content), oldContent)
}

// Stage under the pinned parent. A partial copy, failed precondition, or
// failed publish leaves the target untouched.
func publishHarnessEdit(parent *os.Root, name string, content io.Reader, oldContent string) error {
	flags := os.O_WRONLY
	if oldContent != "" {
		flags = os.O_RDWR
	}
	original, err := openHarnessFile(parent, name, flags)
	creating := errors.Is(err, os.ErrNotExist)
	if err != nil && !creating {
		return err
	}
	if creating && oldContent != "" {
		return errors.New("PRECONDITION_FAILED: target file does not exist")
	}
	mode := os.FileMode(0644)
	var info os.FileInfo
	if !creating {
		defer original.Close()
		info, err = original.Stat()
		if err != nil {
			return err
		}
		mode = info.Mode().Perm()
	}
	tmp := ".ahenk-edit-" + rand.Text()
	f, err := parent.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	defer parent.Remove(tmp)
	_, err = io.Copy(f, content)
	if err == nil && !creating {
		err = f.Chmod(mode)
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if creating {
		// Link fails atomically if another writer created the name.
		return parent.Link(tmp, name)
	}
	current, err := parent.Lstat(name)
	if err != nil || !os.SameFile(info, current) || current.Sys().(*syscall.Stat_t).Nlink != 1 {
		return errors.New("PRECONDITION_FAILED: target changed")
	}
	if oldContent != "" {
		data, err := io.ReadAll(io.LimitReader(original, int64(len(oldContent))+1))
		if err != nil {
			return err
		}
		if string(data) != oldContent {
			return errors.New("PRECONDITION_FAILED: existing content does not match old_content")
		}
	}
	return parent.Rename(tmp, name)
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
// Returns the relative path to open under the worktree handle.
func (s *HarnessServer) validatePath(relPath string, forWrite bool) (string, error) {
	if relPath == "" {
		return "", fmt.Errorf("POLICY_DENIED: path outside sandbox boundary")
	}
	clean := filepath.Clean(relPath)
	if strings.HasPrefix(clean, "..") || filepath.IsAbs(relPath) || filepath.IsAbs(clean) || !model.SafePath(clean) {
		return "", fmt.Errorf("POLICY_DENIED: path outside sandbox boundary")
	}

	if s.cfg.AttemptID != "" {
		if !model.InScope(clean, s.cfg.AllowedPaths) {
			return "", errors.New("POLICY_DENIED: path outside attempt scope")
		}
		return clean, nil
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

	if !model.SafePath(relToCanonical) {
		return "", fmt.Errorf("POLICY_DENIED: path outside sandbox boundary")
	}
	if forWrite {
		if info, err := os.Lstat(fullPath); err == nil && info.Mode()&os.ModeSymlink != 0 {
			return "", errors.New("POLICY_DENIED: symlink target not allowed")
		}
		relToCanonicalClean := filepath.Clean(relToCanonical)
		if !model.InScope(relToCanonicalClean, s.cfg.AllowedPaths) || !model.InScope(clean, s.cfg.AllowedPaths) {
			return "", fmt.Errorf("POLICY_DENIED: path not permitted for modification")
		}
	}
	return relToCanonical, nil
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
		if err := providers.CheckRecursionGuard(env, maxDepth); err != nil {
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
			if a, ok := toolCall.Arguments["attempt_id"].(string); ok {
				req.AttemptID = a
			}
			if c, ok := toolCall.Arguments["check_id"].(string); ok {
				req.CheckID = c
			}
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
			if a, ok := paramMap["attempt_id"].(string); ok && req.AttemptID == "" {
				req.AttemptID = a
			}
			if c, ok := paramMap["check_id"].(string); ok && req.CheckID == "" {
				req.CheckID = c
			}
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

	if s.cfg.AttemptID != "" && s.cfg.AttemptID != req.AttemptID {
		return HarnessResponse{Error: "POLICY_DENIED: attempt identity mismatch"}
	}
	if s.cfg.AttemptID != "" {
		s.requestMu.Lock()
		defer s.requestMu.Unlock()
		if s.IsFinished() {
			return HarnessResponse{Error: "PRECONDITION_FAILED: attempt already finished"}
		}
	}
	if err := s.ctx.Err(); err != nil {
		return HarnessResponse{Error: "POLICY_DENIED: attempt has ended"}
	}
	switch tool {
	case "check":
		check := s.cfg.Checks[req.CheckID]
		if check == nil || !model.ValidID(req.CheckID) {
			return HarnessResponse{Error: "POLICY_DENIED: unregistered check"}
		}
		if err := check(s.ctx); err != nil {
			return HarnessResponse{Error: "CHECK_FAILED: registered check failed"}
		}
		return HarnessResponse{Success: true}

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

func harnessDirectoryOwnedBy(info os.FileInfo, uid int) bool {
	return info.IsDir() && info.Mode().Perm()&0022 == 0 && int(info.Sys().(*syscall.Stat_t).Uid) == uid
}

func openHarnessSocketParent(cfg HarnessConfig) (string, *os.Root, error) {
	work, err := filepath.Abs(cfg.WorktreeRoot)
	if err != nil {
		return "", nil, err
	}
	// Normal dispatch keeps the socket beside .workspace/work. Every other
	// socket must be in the worktree itself or one of its own directories.
	boundary := work
	if parent := filepath.Dir(work); filepath.Base(parent) == ".workspace" {
		boundary = parent
	}
	workInfo, err := os.Lstat(work)
	if err != nil || !harnessDirectoryOwnedBy(workInfo, os.Geteuid()) {
		return "", nil, errors.New("POLICY_DENIED: unsafe harness worktree")
	}
	path := cfg.SocketPath
	if path == "" {
		dir := boundary
		child := filepath.Join(work, ".workspace")
		if boundary == work && filepath.Base(work) != ".workspace" {
			if _, err := os.Lstat(child); err == nil {
				dir = child
			} else if !errors.Is(err, os.ErrNotExist) {
				return "", nil, err
			}
		}
		name := ".harness.sock"
		if cfg.AttemptID != "" {
			digest := sha256.Sum256([]byte(cfg.AttemptID))
			name = fmt.Sprintf(".harness-%x.sock", digest[:8])
		}
		path = filepath.Join(dir, name)
	}
	path, err = filepath.Abs(path)
	if err != nil {
		return "", nil, err
	}
	dir := filepath.Dir(path)
	if _, within := model.Within(work, dir); !within && dir != boundary {
		return "", nil, errors.New("POLICY_DENIED: socket outside attempt")
	}
	var socketParent os.FileInfo
	for current := dir; ; current = filepath.Dir(current) {
		info, err := os.Lstat(current)
		if err != nil || !harnessDirectoryOwnedBy(info, os.Geteuid()) {
			return "", nil, errors.New("POLICY_DENIED: socket requires a real owner-controlled directory")
		}
		if current == dir {
			if info.Mode().Perm() != 0700 {
				return "", nil, errors.New("POLICY_DENIED: socket parent must be private")
			}
			socketParent = info
		}
		if current == boundary {
			break
		}
	}
	parent, err := os.OpenRoot(dir)
	if err != nil {
		return "", nil, err
	}
	opened, err := parent.Stat(".")
	if err != nil || !os.SameFile(socketParent, opened) || opened.Mode().Perm() != 0700 || !harnessDirectoryOwnedBy(opened, os.Geteuid()) {
		return "", nil, errors.Join(errors.New("POLICY_DENIED: socket directory changed"), parent.Close())
	}
	return path, parent, nil
}

// harnessDialPath keeps the socket itself in the attempt directory and gives
// long paths a dialable alias without changing the process working directory.
func harnessDialPath(socketPath string) (path, aliasDir string, err error) {
	if len(socketPath) < 104 {
		return socketPath, "", nil
	}
	socketPath, err = filepath.Abs(socketPath)
	if err != nil {
		return "", "", err
	}
	aliasDir, err = os.MkdirTemp("/tmp", "ahenk-harness-")
	if err != nil {
		return "", "", err
	}
	link := filepath.Join(aliasDir, "dir")
	if err := os.Symlink(filepath.Dir(socketPath), link); err != nil {
		return "", "", errors.Join(err, os.Remove(aliasDir))
	}
	return filepath.Join(link, filepath.Base(socketPath)), aliasDir, nil
}

func removeHarnessAlias(aliasDir string) error {
	if aliasDir == "" {
		return nil
	}
	return errors.Join(os.Remove(filepath.Join(aliasDir, "dir")), os.Remove(aliasDir))
}

// DialHarness connects to a harness socket, including canonical paths longer
// than the platform's Unix socket address.
func DialHarness(sockPath string, timeout time.Duration) (net.Conn, error) {
	path, aliasDir, err := harnessDialPath(sockPath)
	if err != nil {
		return nil, err
	}
	conn, dialErr := net.DialTimeout("unix", path, timeout)
	if err := removeHarnessAlias(aliasDir); err != nil {
		if conn != nil {
			_ = conn.Close()
		}
		return nil, errors.Join(dialErr, err)
	}
	return conn, dialErr
}
