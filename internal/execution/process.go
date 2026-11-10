// Copyright 2026 Furkan Anıl Öztaş
// SPDX-License-Identifier: Apache-2.0

package execution

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"

	"github.com/faoztas/ahenk-mcp/internal/model"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"
)

type FixtureInput struct {
	AGY         *model.AGYRunEvidence      `json:"agy,omitempty"`
	OpenCode    *model.OpenCodeRunEvidence `json:"opencode,omitempty"`
	Summary     string                     `json:"summary"`
	Patch       string                     `json:"patch,omitempty"`
	DelayMillis int                        `json:"delay_ms,omitempty"`
	Failure     string                     `json:"failure,omitempty"`
}

func fixtureEnv() []string {
	return []string{"PATH=/usr/bin:/bin", "HOME=/var/empty", "LC_ALL=C"}
}

// RunFixture holds the supervisor's stdin open for the life of the request.
// Closing it, including through a controller crash, is a stop signal.
func RunFixture(ctx context.Context, dir string, in FixtureInput) (FixtureInput, error) {
	var out FixtureInput
	err := RunSupervised(ctx, dir, "_supervise_fixture", fixtureEnv(), in, &out)
	return out, err
}

// RunSupervised leases a worker to this process. The supervisor owns cleanup
// when its controller exits or cancels.
func RunSupervised(ctx context.Context, dir, command string, env []string, in, out any) error {
	self, err := os.Executable()
	if err != nil {
		return err
	}
	lease, err := os.OpenFile(filepath.Join(dir, ".worker.lock"), os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer lease.Close()
	if err := syscall.Flock(int(lease.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return err
	}
	cmd := exec.Command(self, command)
	cmd.ExtraFiles = []*os.File{lease}
	cmd.Env = env
	cmd.Dir = dir
	// Pass ownership of the watchdog pipe to the controller only.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	p, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	defer p.Close()
	buf := &Limited{Limit: MaxArtifact * 2}
	cmd.Stdout = buf
	if err = cmd.Start(); err != nil {
		return err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	if err = json.NewEncoder(p).Encode(in); err != nil {
		p.Close()
		<-done
		return err
	}
	select {
	case err = <-done:
		if _, markerErr := os.Stat(filepath.Join(dir, ".worker.cleaned")); markerErr != nil {
			return ErrCleanup
		}
		if err != nil {
			return err
		}
		return json.Unmarshal(buf.Bytes(), out)
	case <-ctx.Done():
		p.Close()
		select {
		case err = <-done:
			if _, markerErr := os.Stat(filepath.Join(dir, ".worker.cleaned")); markerErr != nil {
				return ErrCleanup
			}
			if err != nil {
				return err
			}
			return ctx.Err()
		case <-time.After(7 * time.Second):
			// The worker group may differ from the supervisor group.
			// Keep the attempt quarantined when the watchdog did not report cleanup.
			return ErrCleanup
		}
	}
}

var ErrCleanup = errors.New("worker cleanup could not be confirmed")

func LeaseReleased(dir string) bool {
	f, err := os.OpenFile(filepath.Join(dir, ".worker.lock"), os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if os.IsNotExist(err) {
		return true
	}
	if err != nil {
		return false
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Sys().(*syscall.Stat_t).Nlink != 1 {
		return false
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return false
	}
	marker, err := os.Lstat(filepath.Join(dir, ".worker.cleaned"))
	return err == nil && marker.Mode().IsRegular() && marker.Sys().(*syscall.Stat_t).Nlink == 1
}

func SuperviseFixture() error {
	lease := os.NewFile(3, "worker lease")
	defer lease.Close()
	syscall.CloseOnExec(3)
	r := bufio.NewReader(io.LimitReader(os.Stdin, int64(MaxArtifact*2)))
	line, err := r.ReadBytes('\n')
	if err != nil {
		return err
	}
	var in FixtureInput
	if err := json.Unmarshal(line, &in); err != nil {
		return err
	}
	self, err := os.Executable()
	if err != nil {
		return err
	}
	cmd := exec.Command(self, "_fixture_worker")
	cmd.Env = fixtureEnv()
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Stdin = bytes.NewReader(line)
	cmd.Stdout = os.Stdout
	if err := cmd.Start(); err != nil {
		return err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	eof := make(chan struct{})
	go func() { io.Copy(io.Discard, r); close(eof) }()
	select {
	case err := <-done:
		if stopErr := StopProcessGroup(cmd.Process.Pid); stopErr != nil {
			return stopErr
		}
		return errors.Join(err, Save(".", ".worker.cleaned", nil))
	case <-eof:
		if err := StopProcessGroup(cmd.Process.Pid); err != nil {
			return err
		}
		<-done
		return Save(".", ".worker.cleaned", nil)
	}
}

// Supervise reads one request and cancels its worker on controller EOF.
func Supervise(request any, run func(context.Context) (any, error)) error {
	lease := os.NewFile(3, "worker lease")
	defer lease.Close()
	syscall.CloseOnExec(3)
	r := bufio.NewReader(io.LimitReader(os.Stdin, int64(MaxArtifact*2)))
	line, err := r.ReadBytes('\n')
	if err != nil {
		return err
	}
	if err := json.Unmarshal(line, request); err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _, _ = io.Copy(io.Discard, r); cancel() }()
	result, err := run(ctx)
	if errors.Is(err, ErrCleanup) {
		return err
	}
	if marker := Save(".", ".worker.cleaned", nil); marker != nil {
		return marker
	}
	if err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(result)
}

func StopProcessGroup(pid int) error {
	if err := syscall.Kill(-pid, syscall.SIGTERM); errors.Is(err, syscall.ESRCH) {
		return nil
	} else if err != nil {
		return err
	}
	deadline := time.Now().Add(4 * time.Second)
	killed := false
	for time.Now().Before(deadline) {
		if errors.Is(syscall.Kill(-pid, 0), syscall.ESRCH) {
			return nil
		}
		if !killed && time.Until(deadline) < 2*time.Second {
			if err := syscall.Kill(-pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
				return err
			}
			killed = true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return ErrCleanup
}

func FixtureWorker() error {
	var in FixtureInput
	if err := json.NewDecoder(io.LimitReader(os.Stdin, int64(MaxArtifact*2))).Decode(&in); err != nil {
		return err
	}
	if in.DelayMillis < 0 || in.DelayMillis > 1800000 {
		return errors.New("invalid fixture delay")
	}
	time.Sleep(time.Duration(in.DelayMillis) * time.Millisecond)
	return json.NewEncoder(os.Stdout).Encode(in)
}
