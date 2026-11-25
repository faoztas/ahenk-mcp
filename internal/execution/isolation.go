// Copyright 2026 Furkan Anıl Öztaş
// SPDX-License-Identifier: Apache-2.0

package execution

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/faoztas/ahenk-mcp/internal/model"
)

// PrivateWorkerHostIsolated is the evidence kind for an approved backend that
// ran the worker with OS-enforced file, process, and network boundaries.
const PrivateWorkerHostIsolated = "host_isolated"

// PrivateWorkerIsolationEvidence contains observations for one disposable run.
// An independent, trusted collector must record canonical absolute paths while
// the backend is running, exact worktree-relative write paths, and SHA-256
// digests of the same external sentinel before and after the run. The process
// and network fields require observed denials against working host controls.
// The registered checks come from operator policy and the result comes from
// the independent check runner. Worker-supplied claims cannot serve as evidence.
type PrivateWorkerIsolationEvidence struct {
	Backend                  string
	WorktreeRoot             string
	WritableRoot             string
	AllowedWritePaths        []string
	WrittenPaths             []string
	ExternalFilePath         string
	ExternalFileBeforeSHA256 string
	ExternalFileAfterSHA256  string
	HostProcessAccessDenied  bool
	NetworkEgressDenied      bool
	CleanupConfirmed         bool
	CheckKind                string
	RegisteredGoChecks       map[string]string
	RegisteredSwiftChecks    map[string]string
	Check                    model.CheckEvidence
}

var swiftCheckSelector = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*/test[A-Za-z0-9_]+$`)

// VerifyPrivateWorkerIsolation validates the evidence of an isolated backend
// run. A plain Git worktree provides Git separation only.
func VerifyPrivateWorkerIsolation(ev PrivateWorkerIsolationEvidence) error {
	if ev.Backend != PrivateWorkerHostIsolated {
		return errors.New("private worker requires evidence from a host-isolated backend")
	}
	if !filepath.IsAbs(ev.WorktreeRoot) || filepath.Clean(ev.WorktreeRoot) != ev.WorktreeRoot ||
		filepath.Dir(ev.WorktreeRoot) == ev.WorktreeRoot || ev.WritableRoot != ev.WorktreeRoot ||
		len(ev.AllowedWritePaths) == 0 || len(ev.WrittenPaths) == 0 {
		return errors.New("private worker requires an observed scoped write in its disposable worktree")
	}
	for _, path := range ev.AllowedWritePaths {
		if path == "." || !model.ValidTrustedLocalPath(path) {
			return errors.New("private worker write scope requires exact safe paths")
		}
	}
	for _, path := range ev.WrittenPaths {
		if !slices.Contains(ev.AllowedWritePaths, path) {
			return errors.New("private worker wrote outside the allowed worktree paths")
		}
	}
	_, sentinelInWorktree := model.Within(ev.WorktreeRoot, ev.ExternalFilePath)
	_, containsWorktree := model.Within(ev.ExternalFilePath, ev.WorktreeRoot)
	digest, err := hex.DecodeString(ev.ExternalFileBeforeSHA256)
	if !filepath.IsAbs(ev.ExternalFilePath) || filepath.Clean(ev.ExternalFilePath) != ev.ExternalFilePath || sentinelInWorktree || containsWorktree || strings.ToLower(ev.ExternalFileBeforeSHA256) != ev.ExternalFileBeforeSHA256 ||
		err != nil || len(digest) != sha256.Size || ev.ExternalFileBeforeSHA256 != ev.ExternalFileAfterSHA256 {
		return errors.New("private worker external file sentinel is missing or changed")
	}
	if !ev.HostProcessAccessDenied || !ev.NetworkEgressDenied {
		return errors.New("private worker requires observed process and network denials")
	}
	if !ev.CleanupConfirmed {
		return errors.New("private worker cleanup was not confirmed")
	}
	check := ev.Check
	if !model.ValidID(check.ID) || check.Test == "" || check.Status != "PASSED" || check.ExitStatus != 0 {
		return errors.New("private worker requires a passing registered check")
	}
	switch ev.CheckKind {
	case "go":
		if model.ValidGoCheck(check.Test) && ev.RegisteredGoChecks[check.ID] == check.Test {
			return nil
		}
	case "swift":
		if swiftCheckSelector.MatchString(check.Test) && ev.RegisteredSwiftChecks[check.ID] == check.Test {
			return nil
		}
	}
	return errors.New("private worker check did not match a registered Go or Swift check")
}
