// Copyright 2026 Furkan Anıl Öztaş
// SPDX-License-Identifier: Apache-2.0

package execution

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/faoztas/ahenk-mcp/internal/model"
)

const MaxFile = 256 << 10
const MaxContext = 128 << 10
const MaxArtifact = 512 << 10
const maxSnapshot = 8 << 20

type Version struct {
	Mode  string `json:"mode"`
	Hash  string `json:"sha256"`
	Bytes []byte `json:"bytes"`
}

type File struct {
	Path      string   `json:"path"`
	Base      *Version `json:"base,omitempty"`
	Index     *Version `json:"index,omitempty"`
	Effective *Version `json:"effective,omitempty"`
	Untracked bool     `json:"untracked,omitempty"`
}

type Delta struct {
	Path       string `json:"path"`
	Before     string `json:"before,omitempty"`
	After      string `json:"after,omitempty"`
	BeforeMode string `json:"before_mode,omitempty"`
	AfterMode  string `json:"after_mode,omitempty"`
}

type Snapshot struct {
	observed     string
	ID           string    `json:"snapshot_id"`
	BaseSHA      string    `json:"base_sha"`
	CommonDir    string    `json:"common_dir"`
	Root         string    `json:"root"`
	CapturedAt   time.Time `json:"captured_at"`
	Mode         string    `json:"mode"`
	PolicyDigest string    `json:"policy_digest"`
	ScopeDigest  string    `json:"scope_digest"`
	Dirty        bool      `json:"scope_dirty"`
	Files        []File    `json:"files"`
	StagedDiff   []Delta   `json:"staged_diff"`
	UnstagedDiff []Delta   `json:"unstaged_diff"`
	StagedSHA    string    `json:"staged_sha256"`
	UnstagedSHA  string    `json:"unstaged_sha256"`
}

type Limited struct {
	bytes.Buffer
	Limit int
}

func (b *Limited) Write(p []byte) (int, error) {
	if len(p) > b.Limit-b.Len() {
		return 0, errors.New("output limit exceeded")
	}
	return b.Buffer.Write(p)
}

func git(ctx context.Context, dir string, stdin []byte, args ...string) ([]byte, error) {
	base := []string{"--no-replace-objects", "--literal-pathspecs", "-c", "core.hooksPath=/dev/null",
		"-c", "core.fsmonitor=false", "-c", "core.excludesFile=/dev/null", "-c", "core.attributesFile=/dev/null", "-c", "protocol.allow=never"}
	cmd := exec.CommandContext(ctx, "/usr/bin/git", append(base, args...)...)
	cmd.Dir = dir
	cmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=/var/empty", "LC_ALL=C", "GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_ATTR_NOSYSTEM=1", "GIT_OPTIONAL_LOCKS=0", "GIT_TERMINAL_PROMPT=0", "GIT_NO_LAZY_FETCH=1", "GIT_CEILING_DIRECTORIES=" + filepath.Dir(dir)}
	cmd.Stdin = bytes.NewReader(stdin)
	out := Limited{Limit: maxSnapshot}
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

type entry struct{ mode, oid string }

func entries(raw []byte, index bool) (map[string]entry, error) {
	out := map[string]entry{}
	for line := range bytes.SplitSeq(raw, []byte{0}) {
		if len(line) == 0 {
			continue
		}
		header, name, ok := bytes.Cut(line, []byte{'\t'})
		fields := strings.Fields(string(header))
		if !ok || len(fields) != 3 || !model.SafePath(string(name)) {
			return nil, model.Error("POLICY_DENIED", "scope contains a prohibited path")
		}
		mode, oid := fields[0], fields[2]
		if index {
			if fields[2] != "0" {
				return nil, model.Error("WORKSPACE_ENTRY_UNSUPPORTED", "unmerged index entry")
			}
			oid = fields[1]
		}
		if mode != "100644" && mode != "100755" {
			return nil, model.Error("WORKSPACE_ENTRY_UNSUPPORTED", "non-regular Git entry in scope")
		}
		out[string(name)] = entry{mode, oid}
	}
	return out, nil
}

func blob(ctx context.Context, dir string, e entry) (*Version, error) {
	if e.oid == "" {
		return nil, nil
	}
	size, err := git(ctx, dir, nil, "cat-file", "-s", e.oid)
	var n int
	if err != nil {
		return nil, err
	}
	if _, err = fmt.Sscan(string(size), &n); err != nil || n > MaxFile {
		return nil, model.Error("CONTEXT_LIMIT", "Git blob exceeds file limit")
	}
	data, err := git(ctx, dir, nil, "cat-file", "blob", e.oid)
	if err != nil {
		return nil, err
	}
	return version(e.mode, data)
}

func version(mode string, data []byte) (*Version, error) {
	if !utf8.Valid(data) || bytes.IndexByte(data, 0) >= 0 {
		return nil, model.Error("WORKSPACE_ENTRY_UNSUPPORTED", "binary content is outside the text snapshot contract")
	}
	if model.ContainsSecret(data) {
		return nil, model.GateError("POLICY_DENIED", "SECRET_DETECTED", "secret pattern detected in snapshot", "remove_secret_before_retry")
	}
	return &Version{Mode: mode, Hash: model.Hash(data), Bytes: data}, nil
}

func workfile(root *os.Root, name string) (*Version, error) {
	parts := strings.Split(name, "/")
	for i := range parts {
		st, err := root.Lstat(strings.Join(parts[:i+1], "/"))
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		if i < len(parts)-1 && !st.IsDir() || i == len(parts)-1 && !st.Mode().IsRegular() {
			return nil, model.Error("WORKSPACE_ENTRY_UNSUPPORTED", "symlink or non-regular worktree entry")
		}
	}
	// Nonblocking open avoids hanging if a regular file is replaced with a FIFO.
	f, err := root.OpenFile(name, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !st.Mode().IsRegular() || st.Sys().(*syscall.Stat_t).Nlink != 1 {
		return nil, model.Error("WORKSPACE_ENTRY_UNSUPPORTED", "non-regular or hard-linked worktree file")
	}
	b, err := io.ReadAll(io.LimitReader(f, MaxFile+1))
	if err != nil {
		return nil, err
	}
	if len(b) > MaxFile {
		return nil, model.Error("CONTEXT_LIMIT", "worktree file exceeds file limit")
	}
	mode := "100644"
	if st.Mode()&0100 != 0 {
		mode = "100755"
	}
	return version(mode, b)
}

func writeWorkfile(root *os.Root, name string, v *Version) error {
	if err := root.MkdirAll(filepath.Dir(name), 0700); err != nil {
		return err
	}
	mode := fs.FileMode(0600)
	if v.Mode == "100755" {
		mode = 0700
	}
	f, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	_, err = f.Write(v.Bytes)
	return errors.Join(err, f.Close())
}

func same(a, b *Version) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Hash == b.Hash && a.Mode == b.Mode
}

func delta(path string, a, b *Version) Delta {
	d := Delta{Path: path}
	if a != nil {
		d.Before = a.Hash
		d.BeforeMode = a.Mode
	}
	if b != nil {
		d.After = b.Hash
		d.AfterMode = b.Mode
	}
	return d
}

func Capture(ctx context.Context, repo model.Repo, t model.Task) (Snapshot, error) {
	for range 2 {
		a, err := capture(ctx, repo, t)
		if err != nil {
			return Snapshot{}, err
		}
		b, err := capture(ctx, repo, t)
		if err != nil {
			return Snapshot{}, err
		}
		if a.ID == b.ID && a.observed == b.observed {
			a.CapturedAt = time.Now().UTC()
			return a, nil
		}
	}
	return Snapshot{}, model.Error("SNAPSHOT_CHANGED", "scoped worktree changed during capture")
}

func capture(ctx context.Context, repo model.Repo, t model.Task) (Snapshot, error) {
	var s Snapshot
	real, err := filepath.EvalSymlinks(repo.Root)
	if err != nil || real != filepath.Clean(repo.Root) {
		return s, model.Error("REPO_MISMATCH", "repository must use its canonical path")
	}
	rootOut, err := git(ctx, real, nil, "rev-parse", "--show-toplevel")
	if err != nil || strings.TrimSpace(string(rootOut)) != real {
		return s, model.Error("REPO_MISMATCH", "configured root does not match Git worktree")
	}
	common, err := git(ctx, real, nil, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return s, err
	}
	s.CommonDir, err = filepath.EvalSymlinks(strings.TrimSpace(string(common)))
	if err != nil {
		return s, err
	}
	head, err := git(ctx, real, nil, "rev-parse", "--verify", "HEAD^{commit}")
	if err != nil {
		return s, model.Error("WORKSPACE_ENTRY_UNSUPPORTED", "repository needs a committed HEAD")
	}
	s.BaseSHA = strings.TrimSpace(string(head))
	s.Root = real
	s.Mode = t.WorkspaceMode
	s.PolicyDigest = model.Digest(repo)
	s.ScopeDigest = model.Digest(t.AllowedPaths)
	tree, err := git(ctx, real, nil, append([]string{"ls-tree", "-r", "-z", s.BaseSHA, "--"}, t.AllowedPaths...)...)
	if err != nil {
		return s, err
	}
	base, err := entries(tree, false)
	if err != nil {
		return s, err
	}
	if repo.PrivateSource != nil {
		return privateSnapshot(ctx, s, repo.PrivateSource, t, base)
	}
	idx, err := git(ctx, real, nil, append([]string{"ls-files", "--stage", "-z", "--"}, t.AllowedPaths...)...)
	if err != nil {
		return s, err
	}
	index, err := entries(idx, true)
	if err != nil {
		return s, err
	}
	others, err := git(ctx, real, nil, append([]string{"ls-files", "--others", "--exclude-standard", "-z", "--"}, t.AllowedPaths...)...)
	if err != nil {
		return s, err
	}
	untracked := map[string]bool{}
	for name := range strings.SplitSeq(string(others), "\x00") {
		if name != "" {
			untracked[name] = true
		}
	}
	all := map[string]bool{}
	for name := range base {
		all[name] = true
	}
	for name := range index {
		all[name] = true
	}
	for _, name := range t.UntrackedPaths {
		if !untracked[name] || all[name] {
			return s, model.Error("INVALID_INPUT", "selected untracked file is missing, ignored, or tracked")
		}
		all[name] = true
	}
	root, err := os.OpenRoot(real)
	if err != nil {
		return s, err
	}
	defer root.Close()
	names := make([]string, 0, len(all))
	for name := range all {
		names = append(names, name)
	}
	slices.Sort(names)
	total := 0
	s.Dirty = len(untracked) > 0
	workingState := map[string]Delta{}
	for _, name := range names {
		if err := ctx.Err(); err != nil {
			return s, err
		}
		if !model.SafePath(name) || !model.InScope(name, t.AllowedPaths) {
			return s, model.Error("POLICY_DENIED", "snapshot path rejected")
		}
		f := File{Path: name, Untracked: untracked[name] && base[name].oid == "" && index[name].oid == ""}
		f.Base, err = blob(ctx, real, base[name])
		if err != nil {
			return s, err
		}
		f.Index = f.Base
		if index[name] != base[name] {
			f.Index, err = blob(ctx, real, index[name])
			if err != nil {
				return s, err
			}
		}
		working, err := workfile(root, name)
		if err != nil {
			return s, err
		}
		if f.Untracked && working == nil {
			return s, model.Error("SNAPSHOT_CHANGED", "selected untracked file disappeared")
		}
		workingState[name] = delta(name, f.Index, working)
		s.Dirty = s.Dirty || !same(f.Base, f.Index) || !same(f.Index, working)
		f.Effective = f.Base
		if t.WorkspaceMode == "include_working_changes" {
			f.Effective = working
			if !same(f.Base, f.Index) {
				s.StagedDiff = append(s.StagedDiff, delta(name, f.Base, f.Index))
			}
			if !f.Untracked && !same(f.Index, working) {
				s.UnstagedDiff = append(s.UnstagedDiff, delta(name, f.Index, working))
			}
		} else {
			f.Index = f.Base
		}
		for _, v := range []*Version{f.Base, f.Index, f.Effective} {
			if v != nil {
				total += len(v.Bytes)
			}
		}
		if total > maxSnapshot {
			return s, model.Error("CONTEXT_LIMIT", "snapshot exceeds size limit")
		}
		s.Files = append(s.Files, f)
	}
	s.StagedSHA = model.Digest(s.StagedDiff)
	s.UnstagedSHA = model.Digest(s.UnstagedDiff)
	s.observed = model.Digest(workingState)
	s.ID = s.digest()
	return s, nil
}

func privateSnapshot(ctx context.Context, s Snapshot, policy *model.PrivateSourcePolicy, t model.Task, tree map[string]entry) (Snapshot, error) {
	if policy == nil {
		return Snapshot{}, model.GateError("POLICY_DENIED", "PRIVATE_GRANT_REQUIRED", "private snapshot requires an operator grant", "configure_private_source_grant")
	}
	if !policy.ConsentApproved() {
		return Snapshot{}, model.GateError("POLICY_DENIED", "OWNER_CONSENT_REQUIRED", "private snapshot requires explicit owner consent", "set_owner_consent_in_private_source_grant")
	}
	if !policy.Active(time.Now()) {
		return Snapshot{}, model.GateError("POLICY_DENIED", "GRANT_EXPIRED", "private source grant has expired", "renew_private_source_grant")
	}
	if t.DataClass != model.PrivateSourceData || s.Mode != "head_only" || len(t.UntrackedPaths) != 0 {
		return Snapshot{}, model.GateError("POLICY_DENIED", "PRIVATE_MODE_UNSUPPORTED", "private snapshot requires a scoped HEAD-only task", "use_scoped_head_only_private_task")
	}
	if len(tree) != len(t.AllowedPaths) {
		return Snapshot{}, model.GateError("POLICY_DENIED", "PATH_NOT_APPROVED", "private snapshot requires exact regular files in HEAD", "restrict_allowed_paths_to_approved_files")
	}
	for _, name := range t.AllowedPaths {
		if _, ok := policy.Files[name]; !ok || tree[name].mode != "100644" {
			return Snapshot{}, model.GateError("POLICY_DENIED", "PATH_NOT_APPROVED", "private source must be an approved regular non-executable file", "restrict_allowed_paths_to_approved_files")
		}
		v, err := blob(ctx, s.Root, tree[name])
		if err != nil {
			return Snapshot{}, err
		}
		if model.ContainsPersonalData(v.Bytes) {
			return Snapshot{}, model.GateError("POLICY_DENIED", "PERSONAL_DATA_CLASSIFIED", "private source file contains classified personal data", "remove_personal_data_before_retry")
		}
		if v.Hash != policy.Files[name] {
			return Snapshot{}, model.GateError("POLICY_DENIED", "DIGEST_MISMATCH", "private source file has changed since approval", "reapprove_file_digest_in_private_source_grant")
		}
		s.Files = append(s.Files, File{Path: name, Base: v, Index: v, Effective: v})
	}
	files, err := s.Context(t.AllowedPaths)
	if err != nil {
		return Snapshot{}, err
	}
	if fault := policy.CheckContext(t.AllowedPaths, files); fault != nil {
		return Snapshot{}, fault
	}
	s.ID = s.digest()
	return s, nil
}

func (s Snapshot) digest() string {
	s.ID = ""
	s.CapturedAt = time.Time{}
	return model.Digest(s)
}

func SaveJSON(dir, name string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return Save(dir, name, b)
}

func Save(dir, name string, b []byte) error {
	f, err := os.CreateTemp(dir, ".pending-")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(b); err == nil {
		err = f.Sync()
	}
	err = errors.Join(err, f.Close())
	if err != nil {
		return err
	}
	if err := os.Rename(f.Name(), filepath.Join(dir, name)); err != nil {
		return err
	}
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

func LoadSnapshot(dir, id string) (Snapshot, error) {
	var s Snapshot
	if len(id) != 64 || !model.ValidID(id) {
		return s, errors.New("invalid snapshot ID")
	}
	f, err := os.Open(filepath.Join(dir, id+".json"))
	if err != nil {
		return s, err
	}
	defer f.Close()
	err = json.NewDecoder(io.LimitReader(f, maxSnapshot*2)).Decode(&s)
	if err == nil && (s.ID != id || s.digest() != id) {
		err = errors.New("snapshot identity mismatch")
	}
	return s, err
}

func (s Snapshot) Context(paths []string) (map[string]string, error) {
	out := map[string]string{}
	n := 0
	for _, f := range s.Files {
		if f.Effective == nil || !model.InScope(f.Path, paths) {
			continue
		}
		n += len(f.Effective.Bytes) + len(f.Path)
		if n > MaxContext {
			return nil, model.Error("CONTEXT_LIMIT", "model context exceeds size limit")
		}
		if !model.SafePath(f.Path) || model.ContainsSecret(f.Effective.Bytes) || model.Hash(f.Effective.Bytes) != f.Effective.Hash {
			return nil, model.Error("POLICY_DENIED", "snapshot content failed egress check")
		}
		out[f.Path] = string(f.Effective.Bytes)
	}
	return out, nil
}

func VerifyPatch(ctx context.Context, temp string, s Snapshot, t model.Task, patch []byte) (map[string]string, error) {
	return verifyPatch(ctx, temp, s, t, patch, nil)
}

func verifyPatch(ctx context.Context, temp string, s Snapshot, t model.Task, patch []byte, expected map[string]*Version) (map[string]string, error) {
	if len(patch) == 0 || len(patch) > MaxArtifact || !utf8.Valid(patch) || model.ContainsSecret(patch) {
		return nil, model.Error("INVALID_OUTPUT", "missing, oversized or sensitive patch")
	}
	dir, err := os.MkdirTemp(temp, "verify-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	// A disposable materialization keeps Git's shared metadata outside patch verification.
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	for _, f := range s.Files {
		if f.Effective == nil || !model.SafePath(f.Path) || !model.InScope(f.Path, t.AllowedPaths) {
			continue
		}
		if model.Hash(f.Effective.Bytes) != f.Effective.Hash {
			return nil, errors.New("snapshot hash mismatch")
		}
		if err := writeWorkfile(root, f.Path, f.Effective); err != nil {
			return nil, err
		}
	}
	for line := range strings.SplitSeq(string(patch), "\n") {
		if strings.HasPrefix(line, "diff --git ") {
			fields := strings.Fields(line)
			if len(fields) != 4 || !strings.HasPrefix(fields[2], "a/") || !strings.HasPrefix(fields[3], "b/") ||
				fields[2][2:] != fields[3][2:] || !model.SafePath(fields[2][2:]) || !model.InScope(fields[2][2:], t.AllowedPaths) {
				return nil, model.Error("POLICY_DENIED", "patch header is outside scope or renames a path")
			}
		}
		if strings.HasPrefix(line, "rename ") || strings.HasPrefix(line, "copy ") || line == "GIT binary patch" {
			return nil, model.Error("INVALID_OUTPUT", "rename, copy and binary patches are unsupported")
		}
		for _, prefix := range []string{"old mode ", "new mode ", "new file mode ", "deleted file mode "} {
			if mode, ok := strings.CutPrefix(line, prefix); ok && mode != "100644" && mode != "100755" {
				return nil, model.Error("INVALID_OUTPUT", "non-regular patch mode")
			}
		}
	}
	stats, err := git(ctx, dir, patch, "apply", "--numstat", "-z", "-")
	if err != nil || len(stats) == 0 {
		return nil, model.Error("INVALID_OUTPUT", "invalid unified diff")
	}
	preimages := map[string]string{}
	for row := range strings.SplitSeq(string(stats), "\x00") {
		if row == "" {
			continue
		}
		parts := strings.SplitN(row, "\t", 3)
		if len(parts) != 3 || !model.SafePath(parts[2]) || !model.InScope(parts[2], t.AllowedPaths) || parts[0] == "-" || parts[1] == "-" {
			return nil, model.Error("POLICY_DENIED", "patch touches a prohibited path or entry")
		}
		name := parts[2]
		preimages[name] = "absent"
		for _, f := range s.Files {
			if f.Path == name && f.Effective != nil {
				preimages[name] = f.Effective.Hash
				break
			}
		}
	}
	if len(preimages) == 0 {
		return nil, model.Error("INVALID_OUTPUT", "patch has no changes")
	}
	if _, err := git(ctx, dir, patch, "apply", "--check", "--whitespace=error", "-"); err != nil {
		return nil, model.Error("INVALID_OUTPUT", "patch does not apply cleanly to the snapshot")
	}
	if _, err := git(ctx, dir, patch, "apply", "--whitespace=error", "-"); err != nil {
		return nil, model.Error("INVALID_OUTPUT", "patch could not be materialized")
	}
	for name := range preimages {
		actual, err := workfile(root, name)
		if err != nil {
			return nil, err
		}
		if expected != nil && !same(actual, expected[name]) {
			return nil, model.Error("INVALID_OUTPUT", "patch did not reproduce the implementation postimage")
		}
	}
	return preimages, nil
}

func SourceDrift(ctx context.Context, s Snapshot, preimages map[string]string) bool {
	if real, err := filepath.EvalSymlinks(s.Root); err != nil || real != s.Root {
		return true
	}
	root, err := os.OpenRoot(s.Root)
	if err != nil {
		return true
	}
	defer root.Close()
	for name, wanted := range preimages {
		if ctx.Err() != nil || !model.SafePath(name) {
			return true
		}
		v, err := workfile(root, name)
		if err != nil || v == nil && wanted != "absent" || v != nil && v.Hash != wanted {
			return true
		}
		if v != nil {
			for _, f := range s.Files {
				if f.Path == name && (f.Effective == nil || f.Effective.Mode != v.Mode) {
					return true
				}
			}
		}
	}
	return false
}

// TakeSnapshot creates a scoped Snapshot of the target repository root.
func TakeSnapshot(ctx context.Context, root, id, mode string, paths []string) (Snapshot, error) {
	repo := model.Repo{Root: root, AllowedPaths: paths}
	task := model.Task{WorkspaceMode: mode, AllowedPaths: paths}
	snap, err := Capture(ctx, repo, task)
	if err != nil {
		return Snapshot{}, err
	}
	if snap.ID == "" && id != "" {
		snap.ID = id
	}
	return snap, nil
}

// VerifySnapshot verifies that the repository working tree has not drifted from the snapshot.
func VerifySnapshot(ctx context.Context, s Snapshot) (bool, error) {
	if s.Root == "" {
		return false, errors.New("empty snapshot root")
	}
	real, err := filepath.EvalSymlinks(s.Root)
	if err != nil || real != s.Root {
		return false, nil
	}
	root, err := os.OpenRoot(s.Root)
	if err != nil {
		return false, nil
	}
	defer root.Close()
	for _, f := range s.Files {
		if !model.SafePath(f.Path) {
			continue
		}
		v, err := workfile(root, f.Path)
		if err != nil {
			return false, nil
		}
		if f.Effective == nil {
			if v != nil {
				return false, nil
			}
		} else {
			if v == nil || v.Hash != f.Effective.Hash || v.Mode != f.Effective.Mode {
				return false, nil
			}
		}
	}
	return true, nil
}
