// Copyright 2026 Furkan Anıl Öztaş
// SPDX-License-Identifier: Apache-2.0

package execution

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/faoztas/ahenk-mcp/internal/model"
)

type FixtureInput struct {
	AGY         *model.AGYRunEvidence      `json:"agy,omitempty"`
	OpenCode    *model.OpenCodeRunEvidence `json:"opencode,omitempty"`
	Codex       *model.CLIRunEvidence      `json:"codex,omitempty"`
	Claude      *model.CLIRunEvidence      `json:"claude,omitempty"`
	ACP         *model.ACPRunEvidence      `json:"acp,omitempty"`
	Local       *model.LocalRunEvidence    `json:"local,omitempty"`
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
	return runSupervised(ctx, dir, command, env, in, out, 7*time.Second)
}

func runSupervised(ctx context.Context, dir, command string, env []string, in, out any, cleanupWait time.Duration) error {
	self, err := os.Executable()
	if err != nil {
		return err
	}
	cmd := exec.Command(self, command)
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
	lease, err := OpenWorkerLease(dir)
	if err != nil {
		return err
	}
	defer lease.Close()
	cmd.ExtraFiles = []*os.File{lease}
	if err = cmd.Start(); err != nil {
		return errors.Join(err, MarkWorkerCleaned(dir, lease))
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	if err = json.NewEncoder(p).Encode(in); err != nil {
		p.Close()
		<-done
		if !workerCleanupConfirmed(dir, lease) {
			return errors.Join(ErrCleanup, err)
		}
		return err
	}
	select {
	case err = <-done:
		if !workerCleanupConfirmed(dir, lease) {
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
			if !workerCleanupConfirmed(dir, lease) {
				return ErrCleanup
			}
			if err != nil {
				return err
			}
			return ctx.Err()
		case <-time.After(cleanupWait):
			// The worker group may differ from the supervisor group.
			// Keep the attempt quarantined when the watchdog did not report cleanup.
			return ErrCleanup
		}
	}
}

var ErrCleanup = errors.New("worker cleanup could not be confirmed")

const (
	workerLock    = ".worker.lock"
	workerCleaned = ".worker.cleaned"
)

// NewAttemptDir persists an unlocked lease and cleanup marker before the
// scheduler records an attempt. Together they also represent a never-started worker.
func NewAttemptDir(dir string) error {
	if err := os.Mkdir(dir, 0700); err != nil {
		return err
	}
	if err := Save(dir, workerLock, nil); err != nil {
		return err
	}
	return Save(dir, workerCleaned, nil)
}

// OpenWorkerLease holds the same inode across sequential workers. Removing
// the cleanup marker durably before launch fences a crash even if the lock
// becomes unlocked or unlinked.
func OpenWorkerLease(dir string) (*os.File, error) {
	path := filepath.Join(dir, workerLock)
	const flags = os.O_RDWR | syscall.O_NOFOLLOW | syscall.O_NONBLOCK
	f, err := os.OpenFile(path, flags, 0)
	fresh := os.IsNotExist(err)
	if fresh {
		// Provider probes also use fresh temporary directories outside the scheduler.
		if _, err := os.Lstat(filepath.Join(dir, workerCleaned)); !os.IsNotExist(err) {
			return nil, ErrCleanup
		}
		f, err = os.OpenFile(path, flags|os.O_CREATE|os.O_EXCL, 0600)
	}
	if err != nil {
		return nil, errors.Join(ErrCleanup, err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, errors.Join(ErrCleanup, err)
	}
	if !fresh && !workerCleanupConfirmed(dir, f) {
		f.Close()
		return nil, ErrCleanup
	}
	if !fresh {
		if err := os.Remove(filepath.Join(dir, workerCleaned)); err != nil {
			f.Close()
			return nil, errors.Join(ErrCleanup, err)
		}
	}
	d, err := os.Open(dir)
	if err == nil {
		err = errors.Join(d.Sync(), d.Close())
	}
	if err != nil {
		f.Close()
		return nil, errors.Join(ErrCleanup, err)
	}
	return f, nil
}

func privateLeaseFile(info os.FileInfo) bool {
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		return false
	}
	stat := info.Sys().(*syscall.Stat_t)
	return stat.Nlink == 1 && int(stat.Uid) == os.Geteuid()
}

func workerLeaseIntact(dir string, lease *os.File) bool {
	held, err := lease.Stat()
	if err != nil || !privateLeaseFile(held) {
		return false
	}
	current, err := os.Lstat(filepath.Join(dir, workerLock))
	return err == nil && os.SameFile(held, current)
}

func workerCleanupConfirmed(dir string, lease *os.File) bool {
	if !workerLeaseIntact(dir, lease) {
		return false
	}
	marker, err := os.Lstat(filepath.Join(dir, workerCleaned))
	return err == nil && privateLeaseFile(marker)
}

func MarkWorkerCleaned(dir string, lease *os.File) error {
	if !workerLeaseIntact(dir, lease) {
		return ErrCleanup
	}
	if err := Save(dir, workerCleaned, nil); err != nil {
		return errors.Join(ErrCleanup, err)
	}
	if !workerCleanupConfirmed(dir, lease) {
		return ErrCleanup
	}
	return nil
}

func LeaseReleased(dir string) bool {
	f, err := os.OpenFile(filepath.Join(dir, workerLock), os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return false
	}
	defer f.Close()
	return syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) == nil && workerCleanupConfirmed(dir, f)
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
		return errors.Join(err, MarkWorkerCleaned(".", lease))
	case <-eof:
		if err := StopProcessGroup(cmd.Process.Pid); err != nil {
			return err
		}
		<-done
		return MarkWorkerCleaned(".", lease)
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
	if marker := MarkWorkerCleaned(".", lease); marker != nil {
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

var (
	selfBinaryOnce   sync.Once
	selfBinarySHA256 string
)

func SelfBinarySHA256() string {
	selfBinaryOnce.Do(func() {
		exe, err := os.Executable()
		if err != nil {
			return
		}
		real, err := filepath.EvalSymlinks(exe)
		if err != nil {
			real = exe
		}
		f, err := os.Open(real)
		if err != nil {
			return
		}
		defer f.Close()
		h := sha256.New()
		if _, err := io.Copy(h, f); err == nil {
			selfBinarySHA256 = hex.EncodeToString(h.Sum(nil))
		}
	})
	return selfBinarySHA256
}
