// Copyright 2026 Furkan Anıl Öztaş
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/faoztas/ahenk-mcp/internal/model"
)

// ErrInvalidRoutingPreference identifies a scope or record that cannot be trusted.
var ErrInvalidRoutingPreference = errors.New("invalid routing preference")

const routingPreferenceLimit = int64(len("price_performance\n"))

// ReadRoutingPreference reads a repository's operator profile. An empty result
// with no error means there is no saved preference.
func ReadRoutingPreference(stateDir, repoID, repoRoot string) (string, error) {
	name, repoRoot, err := routingPreferenceScope(repoID, repoRoot)
	if err != nil {
		return "", err
	}
	root, err := openRoutingPreferenceState(stateDir, repoRoot, false)
	if root == nil || err != nil {
		return "", err
	}
	defer root.Close()
	if err := checkRoutingPreferenceFile(root, name); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", nil
		}
		return "", err
	}
	f, err := root.OpenFile(name, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil // A concurrent reset removed the record after Lstat.
	}
	if err != nil {
		return "", fmt.Errorf("%w: cannot open profile record", ErrInvalidRoutingPreference)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !privateRoutingPreferenceFile(info) {
		return "", fmt.Errorf("%w: unsafe profile record", ErrInvalidRoutingPreference)
	}
	data, err := io.ReadAll(io.LimitReader(f, routingPreferenceLimit+1))
	profile, complete := strings.CutSuffix(string(data), "\n")
	if err != nil || !complete || !validRoutingPreferenceProfile(profile) {
		return "", fmt.Errorf("%w: malformed profile record", ErrInvalidRoutingPreference)
	}
	return profile, nil
}

// SaveRoutingPreference atomically replaces the operator profile. A nil
// profile removes it. This operation uses neither the scheduler nor SQLite.
func SaveRoutingPreference(stateDir, repoID, repoRoot string, profile *string) error {
	value := ""
	if profile != nil {
		value = *profile
		if !validRoutingPreferenceProfile(value) {
			return fmt.Errorf("%w: unsupported profile", ErrInvalidRoutingPreference)
		}
	}
	name, repoRoot, err := routingPreferenceScope(repoID, repoRoot)
	if err != nil {
		return err
	}
	root, err := openRoutingPreferenceState(stateDir, repoRoot, profile != nil)
	if root == nil || err != nil {
		return err
	}
	defer root.Close()
	if err := checkRoutingPreferenceFile(root, name); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	dir, err := root.Open(".")
	if err != nil {
		return err
	}
	defer dir.Close()
	if profile == nil {
		if err := root.Remove(name); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil
			}
			return err
		}
	} else {
		temp := ".routing-preference-" + rand.Text()
		f, err := root.OpenFile(temp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if err != nil {
			return err
		}
		defer root.Remove(temp)
		if err = f.Chmod(0600); err == nil {
			_, err = io.WriteString(f, value+"\n")
		}
		if err == nil {
			err = f.Sync()
		}
		if err := errors.Join(err, f.Close()); err != nil {
			return err
		}
		if err := root.Rename(temp, name); err != nil {
			return err
		}
	}
	return dir.Sync()
}

func routingPreferenceScope(id, repoRoot string) (string, string, error) {
	if !model.ValidID(id) || !filepath.IsAbs(repoRoot) {
		return "", "", fmt.Errorf("%w: invalid repository scope", ErrInvalidRoutingPreference)
	}
	canonical, err := filepath.EvalSymlinks(repoRoot)
	if err != nil {
		return "", "", fmt.Errorf("%w: cannot resolve repository root", ErrInvalidRoutingPreference)
	}
	info, err := os.Stat(canonical)
	if err != nil || !info.IsDir() {
		return "", "", fmt.Errorf("%w: repository root must be a directory", ErrInvalidRoutingPreference)
	}
	digest := sha256.Sum256([]byte(id + "\x00" + canonical))
	return fmt.Sprintf("routing-preference-%x", digest), canonical, nil
}

// RoutingPreferenceStateDir resolves the existing ancestor before a CLI or web
// operation can create state. The final component remains subject to the
// preference store's ownership, permissions and symlink checks.
func RoutingPreferenceStateDir(cfg model.Config) (string, error) {
	dir := cfg.StateDir
	if err := validateRoutingPreferenceStatePath(dir); err != nil {
		return "", err
	}
	path, err := routingPreferenceCreationPath(filepath.Clean(dir))
	if err != nil {
		return "", fmt.Errorf("%w: cannot resolve state directory", ErrInvalidRoutingPreference)
	}
	info, err := os.Lstat(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) || err == nil && info.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("%w: unsafe state directory", ErrInvalidRoutingPreference)
	}
	for _, repo := range cfg.Repositories {
		root, err := filepath.EvalSymlinks(repo.Root)
		if err != nil || routingPreferenceStateInsideRepo(root, path) {
			return "", fmt.Errorf("%w: state directory must be outside registered repositories", ErrInvalidRoutingPreference)
		}
	}
	return path, nil
}

func validateRoutingPreferenceStatePath(dir string) error {
	if !filepath.IsAbs(dir) {
		return fmt.Errorf("%w: state directory must be absolute", ErrInvalidRoutingPreference)
	}
	for component := range strings.SplitSeq(dir, string(filepath.Separator)) {
		if component == ".." {
			return fmt.Errorf("%w: state directory contains a parent component", ErrInvalidRoutingPreference)
		}
	}
	return nil
}

func openRoutingPreferenceState(dir, repoRoot string, create bool) (*os.Root, error) {
	if err := validateRoutingPreferenceStatePath(dir); err != nil {
		return nil, err
	}
	dir = filepath.Clean(dir)
	if _, inside := model.Within(repoRoot, dir); inside {
		return nil, fmt.Errorf("%w: state directory is inside the repository", ErrInvalidRoutingPreference)
	}
	info, err := os.Lstat(dir)
	if errors.Is(err, os.ErrNotExist) {
		dir, err = routingPreferenceCreationPath(dir)
		if err != nil {
			return nil, fmt.Errorf("%w: cannot resolve state directory", ErrInvalidRoutingPreference)
		}
		if routingPreferenceStateInsideRepo(repoRoot, dir) {
			return nil, fmt.Errorf("%w: state directory is inside the repository", ErrInvalidRoutingPreference)
		}
		if !create {
			return nil, nil
		}
		if err := os.MkdirAll(dir, 0700); err != nil {
			return nil, err
		}
		info, err = os.Lstat(dir)
	}
	if err != nil || !info.IsDir() {
		return nil, fmt.Errorf("%w: unsafe state directory", ErrInvalidRoutingPreference)
	}
	actual, err := stateDirectory(dir)
	if err != nil {
		return nil, fmt.Errorf("%w: unsafe state directory", ErrInvalidRoutingPreference)
	}
	if routingPreferenceStateInsideRepo(repoRoot, actual) {
		return nil, fmt.Errorf("%w: state directory is inside the repository", ErrInvalidRoutingPreference)
	}
	root, err := os.OpenRoot(actual)
	if err != nil {
		return nil, fmt.Errorf("%w: cannot open state directory", ErrInvalidRoutingPreference)
	}
	opened, err := root.Stat(".")
	if err != nil || opened.Mode().Perm() != 0700 || !os.SameFile(info, opened) || opened.Sys().(*syscall.Stat_t).Uid != uint32(os.Geteuid()) {
		root.Close()
		return nil, fmt.Errorf("%w: unsafe state directory", ErrInvalidRoutingPreference)
	}
	return root, nil
}

func routingPreferenceStateInsideRepo(repoRoot, statePath string) bool {
	if _, inside := model.Within(repoRoot, statePath); inside {
		return true
	}
	// A case-insensitive filesystem may use different spellings for the same directory.
	separator := string(filepath.Separator)
	repoDepth := strings.Count(repoRoot, separator)
	ancestorDepth := strings.Count(statePath, separator)
	if ancestorDepth < repoDepth {
		return false
	}
	for ; ancestorDepth > repoDepth; ancestorDepth-- {
		statePath = filepath.Dir(statePath)
	}
	repo, repoErr := os.Stat(repoRoot)
	ancestor, ancestorErr := os.Stat(statePath)
	return repoErr == nil && ancestorErr == nil && os.SameFile(repo, ancestor)
}

// Resolve the existing ancestor before creating any of the missing directories.
func routingPreferenceCreationPath(dir string) (string, error) {
	for parent := filepath.Dir(dir); ; parent = filepath.Dir(parent) {
		_, err := os.Lstat(parent)
		if errors.Is(err, os.ErrNotExist) && parent != filepath.Dir(parent) {
			continue
		}
		if err != nil {
			return "", err
		}
		actual, err := filepath.EvalSymlinks(parent)
		if err != nil {
			return "", err
		}
		missing, err := filepath.Rel(parent, dir)
		if err != nil {
			return "", err
		}
		return filepath.Join(actual, missing), nil
	}
}

func checkRoutingPreferenceFile(root *os.Root, name string) error {
	info, err := root.Lstat(name)
	if errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err != nil || !privateRoutingPreferenceFile(info) {
		return fmt.Errorf("%w: unsafe profile record", ErrInvalidRoutingPreference)
	}
	return nil
}

func privateRoutingPreferenceFile(info os.FileInfo) bool {
	st := info.Sys().(*syscall.Stat_t)
	// An open reader may legitimately be holding an inode after a rename or reset.
	return info.Mode().IsRegular() && info.Mode().Perm() == 0600 && st.Uid == uint32(os.Geteuid()) && st.Nlink <= 1
}

func validRoutingPreferenceProfile(profile string) bool {
	switch profile {
	case "price_performance", "quality", "price":
		return true
	default:
		return false
	}
}
