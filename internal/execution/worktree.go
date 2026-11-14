// Copyright 2026 Furkan Anıl Öztaş
// SPDX-License-Identifier: Apache-2.0

package execution

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"

	"github.com/faoztas/ahenk-mcp/internal/model"
)

const privateWorkspace = ".workspace"

// Worktree is private Git storage for one attempt. It does not contain a
// checkout or object database from the source repository. The caller must
// independently verify the worker's filesystem and command permissions.
type Worktree struct {
	Dir           string
	HarnessSocket string
	attemptDir    string
	identity      fs.FileInfo
	gitLinkHash   string
	snapshot      Snapshot
	task          model.Task
	before        map[string]*Version
	source        sourceIdentity
}

type WorktreeResult struct {
	SourceFingerprint string
	Patch             []byte
	Preimages         map[string]string
	Postimages        map[string]string
	ArtifactSHA256    string
	SourceUnchanged   bool
}

type sourceIdentity struct {
	head, index, files string
}

func PrepareWorktree(ctx context.Context, attemptDir string, s Snapshot, t model.Task) (w *Worktree, result error) {
	if s.ID == "" || s.digest() != s.ID || len(t.AllowedPaths) == 0 {
		return nil, model.Error("SNAPSHOT_CHANGED", "invalid implementation snapshot or scope")
	}
	for _, name := range t.AllowedPaths {
		if !model.SafePath(name) {
			return nil, model.Error("POLICY_DENIED", "invalid implementation path scope")
		}
	}
	real, err := filepath.EvalSymlinks(attemptDir)
	if err != nil || !filepath.IsAbs(real) || real != filepath.Clean(attemptDir) {
		return nil, model.Error("POLICY_DENIED", "attempt directory must use its canonical absolute path")
	}
	dir, err := os.Stat(real)
	if err != nil || !dir.IsDir() || dir.Mode().Perm() != 0700 || int(dir.Sys().(*syscall.Stat_t).Uid) != os.Geteuid() {
		return nil, model.Error("POLICY_DENIED", "attempt directory must be private and owned by the current user")
	}
	for _, other := range []string{s.Root, s.CommonDir} {
		_, within := model.Within(other, real)
		_, contains := model.Within(real, other)
		if within || contains {
			return nil, model.Error("POLICY_DENIED", "attempt directory overlaps the source repository")
		}
	}
	t.AllowedPaths = slices.Clone(t.AllowedPaths)
	source, err := observeSource(ctx, s, t.AllowedPaths)
	if err != nil {
		return nil, err
	}
	before := make(map[string]*Version)
	for _, file := range s.Files {
		if !model.InScope(file.Path, t.AllowedPaths) || file.Effective == nil {
			continue
		}
		v := file.Effective
		if !model.SafePath(file.Path) || (v.Mode != "100644" && v.Mode != "100755") ||
			len(v.Bytes) > MaxFile || model.Hash(v.Bytes) != v.Hash {
			return nil, model.Error("SNAPSHOT_CHANGED", "invalid scoped snapshot entry")
		}
		if _, err := version(v.Mode, v.Bytes); err != nil {
			return nil, err
		}
		before[file.Path] = &Version{Mode: v.Mode, Hash: v.Hash, Bytes: slices.Clone(v.Bytes)}
	}

	if !worktreeLeaseReleased(real) {
		return nil, ErrCleanup
	}
	private := filepath.Join(real, privateWorkspace)
	if err := os.Mkdir(private, 0700); err != nil {
		return nil, err
	}
	identity, err := os.Lstat(private)
	if err != nil {
		return nil, errors.Join(err, ErrCleanup)
	}
	work := &Worktree{
		Dir: filepath.Join(private, "work"), attemptDir: real, identity: identity,
		snapshot: s, task: t, before: before, source: source,
		HarnessSocket: filepath.Join(private, ".harness.sock"),
	}
	defer func() {
		if result != nil {
			result = errors.Join(result, work.Close())
		}
	}()
	seed := filepath.Join(private, "seed.git")
	if err := initPrivateGit(ctx, seed); err != nil {
		return nil, err
	}
	tree, err := writePrivateTree(ctx, seed, before)
	if err != nil {
		return nil, err
	}
	commit, err := git(ctx, seed, []byte("Ahenk snapshot seed\n"),
		"-c", "user.name=Ahenk", "-c", "user.email=fixture@example.test",
		"-c", "commit.gpgSign=false", "commit-tree", tree)
	if err != nil {
		return nil, err
	}
	if _, err := git(ctx, seed, nil, "worktree", "add", "--detach", "--no-checkout", work.Dir, strings.TrimSpace(string(commit))); err != nil {
		return nil, err
	}
	if _, err := git(ctx, work.Dir, nil, "read-tree", "HEAD"); err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(work.Dir)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	for name, v := range before {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if err := writeWorkfile(root, name, v); err != nil {
			return nil, err
		}
	}
	link, err := digestGitLink(root)
	if err != nil {
		return nil, err
	}
	work.gitLinkHash = link
	checkout, err := work.readWorkspace(ctx)
	if err != nil {
		return nil, err
	}
	if len(checkout) != len(before) {
		return nil, model.Error("SNAPSHOT_CHANGED", "private worktree differs from the snapshot")
	}
	for name, v := range before {
		if !same(v, checkout[name]) {
			return nil, model.Error("SNAPSHOT_CHANGED", "private worktree changed snapshot bytes or file mode")
		}
	}
	return work, nil
}

func initPrivateGit(ctx context.Context, dir string) error {
	if err := os.Mkdir(dir, 0700); err != nil {
		return err
	}
	_, err := git(ctx, dir, nil, "init", "--bare", "--template=")
	return err
}

// Write exact bytes, including files ignored by the project and files that
// would be transformed by checkout attributes.
func writePrivateTree(ctx context.Context, dir string, files map[string]*Version) (string, error) {
	if _, err := git(ctx, dir, nil, "read-tree", "--empty"); err != nil {
		return "", err
	}
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	slices.Sort(names)
	var index bytes.Buffer
	for _, name := range names {
		v := files[name]
		oid, err := git(ctx, dir, v.Bytes, "hash-object", "-w", "--no-filters", "--stdin")
		if err != nil {
			return "", err
		}
		fmt.Fprintf(&index, "%s %s\t%s\x00", v.Mode, strings.TrimSpace(string(oid)), name)
	}
	if _, err := git(ctx, dir, index.Bytes(), "update-index", "-z", "--index-info"); err != nil {
		return "", err
	}
	tree, err := git(ctx, dir, nil, "write-tree")
	return strings.TrimSpace(string(tree)), err
}

func digestGitLink(root *os.Root) (string, error) {
	f, err := root.OpenFile(".git", os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return "", err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() || st.Size() > 4096 || st.Sys().(*syscall.Stat_t).Nlink != 1 {
		return "", model.Error("POLICY_DENIED", "invalid private worktree Git metadata")
	}
	data, err := io.ReadAll(io.LimitReader(f, 4097))
	if err != nil || len(data) > 4096 || !bytes.HasPrefix(data, []byte("gitdir: ")) {
		return "", model.Error("POLICY_DENIED", "invalid private worktree Git link")
	}
	return model.Hash(data), nil
}

func (w *Worktree) readWorkspace(ctx context.Context) (map[string]*Version, error) {
	info, err := os.Lstat(filepath.Dir(w.Dir))
	if err != nil || !os.SameFile(w.identity, info) {
		return nil, model.Error("POLICY_DENIED", "attempt no longer owns the worktree")
	}
	dir, err := filepath.EvalSymlinks(w.Dir)
	if err != nil || dir != w.Dir {
		return nil, model.Error("POLICY_DENIED", "worktree path was removed or redirected")
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	if hash, err := digestGitLink(root); err != nil || hash != w.gitLinkHash {
		return nil, model.Error("POLICY_DENIED", "worktree Git link changed during execution")
	}

	files := make(map[string]*Version)
	size := 0
	visits := 0
	err = fs.WalkDir(root.FS(), ".", func(name string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		visits++
		if visits > 8192 {
			return model.Error("CONTEXT_LIMIT", "worktree contains too many entries")
		}
		if name == "." || name == ".git" {
			return nil
		}
		if !model.SafePath(name) {
			return model.Error("POLICY_DENIED", "worktree contains a prohibited path")
		}
		if entry.IsDir() {
			if model.InScope(name, w.task.AllowedPaths) ||
				slices.ContainsFunc(w.task.AllowedPaths, func(allowed string) bool {
					return strings.HasPrefix(allowed, name+"/")
				}) {
				return nil
			}
			return model.Error("POLICY_DENIED", "native edit changed a protected file")
		}
		if !model.InScope(name, w.task.AllowedPaths) {
			return model.Error("POLICY_DENIED", "native edit changed a protected file")
		}
		v, err := workfile(root, name)
		if err != nil {
			return err
		}
		if v == nil {
			return model.Error("SNAPSHOT_CHANGED", "worktree entry changed during collection")
		}
		size += len(v.Bytes) + len(name)
		if size > maxSnapshot {
			return model.Error("CONTEXT_LIMIT", "worktree exceeds the snapshot size limit")
		}
		files[name] = v
		return nil
	})
	return files, err
}

// ApplyPatch exercises the worktree lifecycle with a supervised offline
// fixture. Live workers edit through their own verified permissions.
func (w *Worktree) ApplyPatch(ctx context.Context, patch []byte) error {
	if !worktreeLeaseReleased(w.attemptDir) {
		return ErrCleanup
	}
	files, err := w.readWorkspace(ctx)
	if err != nil {
		return err
	}
	if len(files) != len(w.before) {
		return model.Error("SNAPSHOT_CHANGED", "fixture worktree changed before patch application")
	}
	for name, v := range w.before {
		if !same(v, files[name]) {
			return model.Error("SNAPSHOT_CHANGED", "fixture worktree changed before patch application")
		}
	}
	if _, err := VerifyPatch(ctx, w.attemptDir, w.snapshot, w.task, patch); err != nil {
		return err
	}
	_, err = git(ctx, w.Dir, patch, "apply", "--whitespace=error", "-")
	return err
}

// Collect only runs after process cleanup is confirmed. Diff generation never
// invokes Git against metadata that the provider could have modified.
func (w *Worktree) Collect(ctx context.Context, scratch string) (WorktreeResult, error) {
	var out WorktreeResult
	if !worktreeLeaseReleased(w.attemptDir) {
		return out, ErrCleanup
	}
	source, err := observeSource(ctx, w.snapshot, w.task.AllowedPaths)
	if err != nil {
		return out, err
	}
	if source != w.source {
		return out, model.Error("SNAPSHOT_CHANGED", "source checkout, HEAD or index changed during implementation")
	}
	out.SourceUnchanged = true
	out.SourceFingerprint = w.SourceFingerprint()
	after, err := w.readWorkspace(ctx)
	if err != nil {
		return out, err
	}
	scratch, err = filepath.EvalSymlinks(scratch)
	if err != nil || !filepath.IsAbs(scratch) {
		return out, model.Error("POLICY_DENIED", "verification directory must resolve to an absolute path")
	}
	for _, root := range []string{w.snapshot.Root, w.snapshot.CommonDir, w.Dir} {
		if _, inside := model.Within(root, scratch); inside {
			return out, model.Error("POLICY_DENIED", "verification directory overlaps a worktree or source Git metadata")
		}
	}
	temp, err := os.MkdirTemp(scratch, "worker-diff-")
	if err != nil {
		return out, err
	}
	defer os.RemoveAll(temp)
	repo := filepath.Join(temp, "verify.git")
	if err := initPrivateGit(ctx, repo); err != nil {
		return out, err
	}
	base, err := writePrivateTree(ctx, repo, w.before)
	if err != nil {
		return out, err
	}
	final, err := writePrivateTree(ctx, repo, after)
	if err != nil {
		return out, err
	}
	patch, err := git(ctx, repo, nil, "diff", "--no-ext-diff", "--no-textconv", "--no-renames",
		"--text", "--src-prefix=a/", "--dst-prefix=b/", base, final, "--")
	if err != nil {
		return out, err
	}
	preimages, err := verifyPatch(ctx, temp, w.snapshot, w.task, patch, after)
	if err != nil {
		return out, err
	}
	postimages := make(map[string]string, len(preimages))
	for name := range preimages {
		postimages[name] = "absent"
		if v := after[name]; v != nil {
			postimages[name] = v.Hash
		}
	}
	out.Patch, out.Preimages, out.Postimages = patch, preimages, postimages
	out.ArtifactSHA256 = model.Hash(patch)
	return out, nil
}

func observeSource(ctx context.Context, s Snapshot, paths []string) (sourceIdentity, error) {
	var out sourceIdentity
	mismatch := model.Error("REPO_MISMATCH", "implementation source no longer matches the snapshot repository")
	if real, err := filepath.EvalSymlinks(s.Root); err != nil || real != s.Root {
		return out, mismatch
	}
	top, err := git(ctx, s.Root, nil, "rev-parse", "--show-toplevel")
	if err != nil || strings.TrimSpace(string(top)) != s.Root {
		return out, mismatch
	}
	common, err := git(ctx, s.Root, nil, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return out, mismatch
	}
	if real, err := filepath.EvalSymlinks(strings.TrimSpace(string(common))); err != nil || real != s.CommonDir {
		return out, mismatch
	}
	head, err := git(ctx, s.Root, nil, "rev-parse", "--verify", "HEAD^{commit}")
	if err != nil {
		return out, mismatch
	}
	out.head = strings.TrimSpace(string(head))
	index, err := git(ctx, s.Root, nil, "rev-parse", "--path-format=absolute", "--git-path", "index")
	if err != nil {
		return out, mismatch
	}
	rawIndex, err := digestIndex(s.CommonDir, strings.TrimSpace(string(index)))
	if err != nil {
		return out, err
	}
	logicalIndex, err := git(ctx, s.Root, nil, "ls-files", "--stage", "-z")
	if err != nil {
		return out, err
	}
	out.index = model.Digest([]string{rawIndex, model.Hash(logicalIndex)})
	root, err := os.OpenRoot(s.Root)
	if err != nil {
		return out, err
	}
	defer root.Close()
	// Compare scoped file contents that were approved for the snapshot. Use
	// names only for other entries so unselected files stay unread.
	approved := make(map[string]bool)
	files := map[string]Delta{}
	for _, file := range s.Files {
		if !model.SafePath(file.Path) || !model.InScope(file.Path, paths) {
			continue
		}
		v, err := workfile(root, file.Path)
		if err != nil {
			return out, err
		}
		approved[file.Path] = true
		files[file.Path] = delta(file.Path, nil, v)
	}
	scoped := append([]string{"ls-files", "--cached", "--others", "--exclude-standard", "-z", "--"}, paths...)
	listed, err := git(ctx, s.Root, nil, scoped...)
	if err != nil {
		return out, err
	}
	for name := range strings.SplitSeq(string(listed), "\x00") {
		if name != "" && model.SafePath(name) && !approved[name] {
			files[name] = delta(name, nil, nil)
		}
	}
	out.files = model.Digest(files)
	return out, nil
}

func digestIndex(common, path string) (string, error) {
	rel, inside := model.Within(common, path)
	if !inside || filepath.Base(rel) != "index" {
		return "", model.Error("REPO_MISMATCH", "source Git index is outside its common directory")
	}
	root, err := os.OpenRoot(common)
	if err != nil {
		return "", err
	}
	defer root.Close()
	f, err := root.OpenFile(rel, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if errors.Is(err, fs.ErrNotExist) {
		return "absent", nil
	}
	if err != nil {
		return "", err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() || st.Sys().(*syscall.Stat_t).Nlink != 1 {
		return "", model.Error("REPO_MISMATCH", "source Git index is not a regular file")
	}
	const maxIndex = 64 << 20
	hash := sha256.New()
	n, err := io.Copy(hash, io.LimitReader(f, maxIndex+1))
	if err != nil || n > maxIndex {
		return "", model.Error("CONTEXT_LIMIT", "source Git index could not be verified within the size limit")
	}
	return fmt.Sprintf("%x", hash.Sum(nil)), nil
}

func (w *Worktree) SourceFingerprint() string {
	return model.Digest([]string{w.source.head, w.source.index, w.source.files})
}

// HarnessSocketPath returns the path to the isolated .workspace/.harness.sock socket.
func (w *Worktree) HarnessSocketPath() string {
	if w.HarnessSocket != "" {
		return w.HarnessSocket
	}
	return filepath.Join(filepath.Dir(w.Dir), ".harness.sock")
}

func (w *Worktree) Close() error {
	if !worktreeLeaseReleased(w.attemptDir) {
		return ErrCleanup
	}
	info, err := os.Lstat(filepath.Join(w.attemptDir, privateWorkspace))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil || !os.SameFile(w.identity, info) {
		return ErrCleanup
	}
	return RecoverWorktree(w.attemptDir)
}

// RecoverWorktree is safe after a restart when the supervisor's lease and
// cleanup marker prove that no worker is using the attempt.
func RecoverWorktree(attemptDir string) error {
	info, err := os.Lstat(filepath.Join(attemptDir, privateWorkspace))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil || !info.IsDir() || !worktreeLeaseReleased(attemptDir) {
		return ErrCleanup
	}
	real, err := filepath.EvalSymlinks(attemptDir)
	if err != nil || real != attemptDir {
		return ErrCleanup
	}
	root, err := os.OpenRoot(real)
	if err != nil {
		return ErrCleanup
	}
	defer root.Close()
	if err := root.RemoveAll(privateWorkspace); err != nil {
		return ErrCleanup
	}
	return nil
}

func worktreeLeaseReleased(attemptDir string) bool {
	if !LeaseReleased(attemptDir) {
		return false
	}
	check := filepath.Join(attemptDir, goCheckDir)
	info, err := os.Lstat(check)
	if errors.Is(err, fs.ErrNotExist) {
		_, err := os.Lstat(filepath.Join(attemptDir, goCheckIntent))
		return errors.Is(err, fs.ErrNotExist)
	}
	return err == nil && info.IsDir() && LeaseReleased(check)
}
