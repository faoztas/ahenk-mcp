// Copyright 2026 Furkan Anıl Öztaş
// SPDX-License-Identifier: Apache-2.0

package execution

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"

	"github.com/faoztas/ahenk-mcp/internal/model"
)

const goCheckDir = ".go-check"

type goCheckRequest struct {
	Tool     string
	Work     string
	Checks   []model.CheckEvidence
	Deadline time.Time
}

func GoCheckTool() (string, error) {
	tool, err := exec.LookPath("go")
	if err != nil || !filepath.IsAbs(tool) {
		return "", model.Error("DETERMINISTIC_CHECK_NOT_CONFIGURED", "Go is unavailable in the local server path")
	}
	return filepath.EvalSymlinks(tool)
}

func RunGoChecks(ctx context.Context, attemptDir, work, tool string, checks []model.CheckEvidence) ([]model.CheckEvidence, error) {
	deadline, bounded := ctx.Deadline()
	if !bounded || !LeaseReleased(attemptDir) || work != filepath.Join(attemptDir, privateWorkspace, "work") {
		return nil, model.Error("POLICY_DENIED", "Go verification requires a released, bounded private worktree")
	}
	dir := filepath.Join(attemptDir, goCheckDir)
	if err := os.Mkdir(dir, 0700); err != nil {
		return nil, err
	}
	var out []model.CheckEvidence
	err := RunSupervised(ctx, dir, "_supervise_go_check", fixtureEnv(), goCheckRequest{
		Tool: tool, Work: work, Checks: checks, Deadline: deadline,
	}, &out)
	return out, err
}

func SuperviseGoChecks() error {
	var req goCheckRequest
	return Supervise(&req, func(owner context.Context) (any, error) {
		ctx, cancel := context.WithDeadline(owner, req.Deadline)
		defer cancel()
		dir, err := os.Getwd()
		if err != nil {
			return nil, err
		}
		work := filepath.Join(filepath.Dir(dir), privateWorkspace, "work")
		real, err := filepath.EvalSymlinks(work)
		if err != nil || work != req.Work || real != work || filepath.Base(dir) != goCheckDir || !filepath.IsAbs(req.Tool) {
			return nil, model.Error("POLICY_DENIED", "invalid private Go verification workspace")
		}
		for i := range req.Checks {
			check := &req.Checks[i]
			check.Status, check.ExitStatus = model.NotRun, -1
		}
		for i := range req.Checks {
			check := &req.Checks[i]
			if !model.ValidID(check.ID) || !model.ValidGoCheck(check.Test) || ctx.Err() != nil {
				break
			}
			if err := runGoCheck(ctx, req.Tool, work, dir, check); errors.Is(err, ErrCleanup) {
				return nil, err
			}
			if check.Status != "PASSED" {
				break
			}
		}
		return req.Checks, nil
	})
}

func runGoCheck(ctx context.Context, tool, work, dir string, check *model.CheckEvidence) error {
	cmd := exec.CommandContext(ctx, tool, "test", "-mod=readonly", "-json", "-count=1", "-run", "^"+check.Test+"$", ".")
	cmd.Dir = work
	cmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=" + dir, "TMPDIR=" + dir, "LC_ALL=C",
		"CGO_ENABLED=0", "GOWORK=off", "GOENV=off", "GOTOOLCHAIN=local", "GOPROXY=off", "GOSUMDB=off",
		"GOPATH=" + filepath.Join(dir, "gopath"), "GOCACHE=" + filepath.Join(dir, "cache")}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return StopProcessGroup(cmd.Process.Pid) }
	cmd.WaitDelay = 2 * time.Second
	stdout := &Limited{Limit: MaxArtifact}
	cmd.Stdout = stdout
	if err := cmd.Start(); err != nil {
		return err
	}
	err := cmd.Wait()
	if cleanup := StopProcessGroup(cmd.Process.Pid); cleanup != nil {
		return ErrCleanup
	}
	check.ExitStatus = cmd.ProcessState.ExitCode()
	check.Status = model.Failed
	if err == nil && goCheckPassed(stdout.Bytes(), check.Test) {
		check.Status = "PASSED"
	}
	return err
}

func goCheckPassed(data []byte, test string) bool {
	scan := bufio.NewScanner(bytes.NewReader(data))
	scan.Buffer(make([]byte, 4096), MaxArtifact)
	ran, passed, packagePassed := false, false, false
	for scan.Scan() {
		var e struct{ Action, Test, Package string }
		if json.Unmarshal(scan.Bytes(), &e) != nil {
			return false
		}
		switch {
		case e.Test == test && e.Action == "run":
			ran = true
		case e.Test == test && e.Action == "pass":
			passed = ran
		case e.Test == "" && e.Action == "pass" && e.Package != "":
			packagePassed = true
		case e.Action == "fail" || e.Test == test && e.Action == "skip":
			return false
		}
	}
	return scan.Err() == nil && passed && packagePassed
}
