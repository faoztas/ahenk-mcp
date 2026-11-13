// Copyright 2026 Furkan Anıl Öztaş
// SPDX-License-Identifier: Apache-2.0

package execution

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/faoztas/ahenk-mcp/internal/model"
)

const swiftPolicy = `(version 1)
(allow default)
(deny network*)
(deny mach-lookup)
(deny file-read* (subpath (param "OPERATOR_HOME")))
(deny file-write*)
(allow file-write* (subpath (param "WORK")) (literal "/dev/null"))`

type localSwiftConfig struct {
	Root       string            `json:"root"`
	Commit     string            `json:"commit"`
	Runner     string            `json:"runner"`
	Files      map[string]string `json:"files"`
	DeviceType string            `json:"device_type,omitempty"`
	Runtime    string            `json:"runtime,omitempty"`
}

type LocalSwiftResult struct {
	Runner          string `json:"runner"`
	Status          string `json:"status"`
	NetworkBoundary string `json:"network_boundary"`
	Phase           string `json:"phase"`
	Tests           int    `json:"tests"`
	ExitStatus      int    `json:"exit_status"`
}

type swiftRequest struct {
	Config   localSwiftConfig
	Home     string
	Deadline time.Time
}

func RunLocalSwift(ctx context.Context, file string) (LocalSwiftResult, error) {
	if runtime.GOOS != "darwin" {
		return LocalSwiftResult{}, errors.New("local Swift verification requires macOS")
	}
	deadline, bounded := ctx.Deadline()
	if !bounded {
		return LocalSwiftResult{}, errors.New("local Swift verification requires a deadline")
	}
	cfg, err := loadLocalSwift(file)
	if err != nil {
		return LocalSwiftResult{}, err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return LocalSwiftResult{}, errors.New("cannot resolve operator home")
	}
	home, err = filepath.EvalSymlinks(home)
	if err != nil || !filepath.IsAbs(home) || home == "/" {
		return LocalSwiftResult{}, errors.New("cannot verify operator home")
	}
	dir, err := os.MkdirTemp("/private/tmp", "ahenk-local-swift-")
	if err != nil {
		return LocalSwiftResult{}, err
	}
	var result LocalSwiftResult
	err = runSupervised(ctx, dir, "_supervise_local_swift", fixtureEnv(), swiftRequest{
		Config: cfg, Home: home, Deadline: deadline,
	}, &result, 45*time.Second)
	if !errors.Is(err, ErrCleanup) {
		if cleanup := os.RemoveAll(dir); cleanup != nil {
			err = ErrCleanup
		}
	}
	if errors.Is(err, ErrCleanup) {
		return result, errors.Join(err, errors.New("local Swift scratch quarantined at "+dir))
	}
	return result, err
}

func loadLocalSwift(file string) (localSwiftConfig, error) {
	var cfg localSwiftConfig
	f, err := os.OpenFile(file, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return cfg, errors.New("cannot open local Swift configuration")
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !privateLeaseFile(info) || info.Size() > 64<<10 {
		return cfg, errors.New("local Swift configuration must be a private regular file owned by the operator")
	}
	d := json.NewDecoder(io.LimitReader(f, 64<<10))
	d.DisallowUnknownFields()
	if d.Decode(&cfg) != nil || d.Decode(new(any)) != io.EOF {
		return cfg, errors.New("invalid local Swift configuration")
	}
	return cfg, cfg.validate()
}

func (c localSwiftConfig) validate() error {
	if !filepath.IsAbs(c.Root) || c.Root == "/" || (!swiftHash(c.Commit, 20) && !swiftHash(c.Commit, 32)) ||
		len(c.Files) == 0 || len(c.Files) > 128 {
		return errors.New("local Swift requires a canonical Git root, full commit and up to 128 approved files")
	}
	switch c.Runner {
	case "swiftpm":
		if c.Files["Package.swift"] == "" || c.Runtime != "" || c.DeviceType != "" {
			return errors.New("SwiftPM requires a root Package.swift and a macOS test target")
		}
	case "simulator-xctest":
		if !strings.HasPrefix(c.Runtime, "com.apple.CoreSimulator.SimRuntime.iOS-") || !model.ValidID(c.Runtime) ||
			!strings.HasPrefix(c.DeviceType, "com.apple.CoreSimulator.SimDeviceType.") || !model.ValidID(c.DeviceType) {
			return errors.New("Simulator XCTest requires an installed iOS runtime and device type")
		}
	default:
		return errors.New("runner must be swiftpm or simulator-xctest")
	}
	for name, hash := range c.Files {
		if !model.ValidTrustedLocalPath(name) || !swiftHash(hash, 32) ||
			(c.Runner == "simulator-xctest" && filepath.Ext(name) != ".swift") {
			return errors.New("local Swift requires exact safe source paths and SHA-256 digests")
		}
	}
	return nil
}

func swiftHash(value string, bytes int) bool {
	raw, err := hex.DecodeString(value)
	return err == nil && len(raw) == bytes && strings.ToLower(value) == value
}

func stageSwiftFiles(ctx context.Context, cfg localSwiftConfig, work string) error {
	real, err := filepath.EvalSymlinks(cfg.Root)
	if err != nil || real != cfg.Root {
		return errors.New("local Swift repository root must be canonical")
	}
	root, err := git(ctx, real, nil, "rev-parse", "--show-toplevel")
	if err != nil || strings.TrimSpace(string(root)) != real {
		return errors.New("local Swift repository does not match the Git worktree root")
	}
	head, err := git(ctx, real, nil, "rev-parse", "--verify", "HEAD^{commit}")
	if err != nil || strings.TrimSpace(string(head)) != cfg.Commit {
		return errors.New("local Swift HEAD differs from the approved commit")
	}
	names := slices.Sorted(maps.Keys(cfg.Files))
	raw, err := git(ctx, real, nil, append([]string{"ls-tree", "-r", "-z", cfg.Commit, "--"}, names...)...)
	if err != nil {
		return errors.New("cannot read approved Swift tree")
	}
	tree, err := entries(raw, false)
	if err != nil || len(tree) != len(names) {
		return errors.New("approved Swift inputs must be exact regular Git files")
	}
	total := 0
	for _, name := range names {
		if tree[name].mode != "100644" {
			return errors.New("approved Swift inputs must be non-executable Git files")
		}
		v, err := blob(ctx, real, tree[name])
		if err != nil || v.Hash != cfg.Files[name] || model.ContainsSecret(v.Bytes) {
			return errors.New("Swift input failed its content or digest check")
		}
		total += len(v.Bytes)
		if total > 2<<20 {
			return errors.New("approved Swift inputs exceed 2 MiB")
		}
		path := filepath.Join(work, name)
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			return err
		}
		if err := os.WriteFile(path, v.Bytes, 0600); err != nil {
			return err
		}
	}
	return nil
}

func SuperviseLocalSwift() error {
	var request swiftRequest
	return Supervise(&request, func(owner context.Context) (any, error) {
		ctx, cancel := context.WithDeadline(owner, request.Deadline)
		defer cancel()
		dir, err := os.Getwd()
		if err != nil {
			return nil, err
		}
		if err := request.Config.validate(); err != nil {
			return nil, err
		}
		return runLocalSwift(ctx, dir, request)
	})
}

type swiftEnvironment struct {
	dir  string
	home string
}

func (e swiftEnvironment) env() []string {
	return []string{"PATH=/usr/bin:/bin", "HOME=" + e.dir, "TMPDIR=" + e.dir,
		"LC_ALL=C", "LANG=C", "CLANG_MODULE_CACHE_PATH=" + filepath.Join(e.dir, "modules")}
}

func (e swiftEnvironment) command(ctx context.Context, env []string, args ...string) (string, int, error) {
	cmd := exec.CommandContext(ctx, args[0], args[1:]...)
	cmd.Env, cmd.Dir = env, e.dir
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stopped := make(chan error, 1)
	cmd.Cancel = func() error {
		err := stopSwiftTree(cmd.Process.Pid)
		stopped <- err
		return err
	}
	cmd.WaitDelay = 2 * time.Second
	out := &Limited{Limit: MaxArtifact}
	cmd.Stdout, cmd.Stderr = out, out
	if err := cmd.Start(); err != nil {
		return "", -1, err
	}
	err := cmd.Wait()
	select {
	case stopErr := <-stopped:
		if stopErr != nil {
			return "", -1, ErrCleanup
		}
	default:
	}
	if cleanup := StopProcessGroup(cmd.Process.Pid); cleanup != nil {
		return "", -1, ErrCleanup
	}
	return out.String(), cmd.ProcessState.ExitCode(), err
}

// Swift can put compiler children in separate process groups. On cancellation,
// freeze each parent before inspecting its direct children to keep them attached.
func stopSwiftTree(pid int) error {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	var stop func(int) error
	stop = func(pid int) error {
		if err := syscall.Kill(pid, syscall.SIGSTOP); err != nil {
			if errors.Is(err, syscall.ESRCH) {
				return nil
			}
			return ErrCleanup
		}
		cmd := exec.CommandContext(ctx, "/usr/bin/pgrep", "-P", strconv.Itoa(pid))
		cmd.Env = fixtureEnv()
		output, err := cmd.Output()
		var failure error
		var noChildren *exec.ExitError
		if err != nil && (!errors.As(err, &noChildren) || noChildren.ExitCode() != 1) {
			failure = ErrCleanup
		}
		for _, raw := range strings.Fields(string(output)) {
			child, parseErr := strconv.Atoi(raw)
			if parseErr != nil || child <= 1 {
				failure = ErrCleanup
				continue
			}
			if err := stop(child); err != nil {
				failure = ErrCleanup
			}
		}
		if err := syscall.Kill(pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
			failure = ErrCleanup
		}
		return failure
	}
	return stop(pid)
}

func (e swiftEnvironment) sandbox(ctx context.Context, args ...string) (string, int, error) {
	prefix := []string{"/usr/bin/sandbox-exec", "-D", "OPERATOR_HOME=" + e.home, "-D", "WORK=" + e.dir, "-p", swiftPolicy}
	return e.command(ctx, e.env(), append(prefix, args...)...)
}

type swiftCommand func(context.Context, ...string) (string, int, error)

func verifySwiftNetwork(ctx context.Context, control, restricted swiftCommand) error {
	for _, host := range []string{"127.0.0.1", "::1"} {
		listener, err := net.Listen("tcp", net.JoinHostPort(host, "0"))
		if err != nil {
			return err
		}
		port := strconv.Itoa(listener.Addr().(*net.TCPAddr).Port)
		args := []string{"/usr/bin/nc", "-v", "-n", "-z", "-w", "2", host, port}
		_, _, controlErr := control(ctx, args...)
		output, code, denied := restricted(ctx, args...)
		listener.Close()
		if errors.Is(controlErr, ErrCleanup) || errors.Is(denied, ErrCleanup) {
			return ErrCleanup
		}
		if controlErr != nil || denied == nil || code != 1 ||
			!strings.Contains(output, "nc: connect") || !strings.Contains(output, host+" port "+port) ||
			!strings.Contains(output, "Operation not permitted") && !strings.Contains(output, "Permission denied") {
			return errors.New("local Swift IP boundary could not be verified")
		}
	}
	return nil
}

var xctestCount = regexp.MustCompile(`(?m)^Test Suite 'All tests' passed at [^\r\n]+\r?\n[ \t]*Executed ([1-9][0-9]*) tests?, with 0 failures? `)

func swiftTestCount(output string, code int) int {
	if code != 0 {
		return 0
	}
	matches := xctestCount.FindAllStringSubmatch(output, -1)
	if len(matches) == 0 {
		return 0
	}
	n, _ := strconv.Atoi(matches[len(matches)-1][1])
	return n
}

func runLocalSwift(ctx context.Context, dir string, req swiftRequest) (LocalSwiftResult, error) {
	e := swiftEnvironment{dir: dir, home: req.Home}
	result := LocalSwiftResult{Runner: req.Config.Runner, Status: "NOT_RUN", NetworkBoundary: "UNVERIFIED", Phase: "HOST_NETWORK", ExitStatus: -1}
	host := func(ctx context.Context, args ...string) (string, int, error) {
		return e.command(ctx, e.env(), args...)
	}
	if err := verifySwiftNetwork(ctx, host, e.sandbox); err != nil {
		if errors.Is(err, ErrCleanup) {
			return result, err
		}
		return result, nil
	}
	work := filepath.Join(dir, "source")
	result.Phase = "APPROVED_INPUTS"
	if err := stageSwiftFiles(ctx, req.Config, work); err != nil {
		return result, nil
	}
	if req.Config.Runner == "simulator-xctest" {
		return e.runSimulatorSwift(ctx, req.Config, work, result)
	}
	result.NetworkBoundary = "HOST_IP_DENIED"
	result.Phase = "XCTEST"
	output, code, err := e.sandbox(ctx, "/usr/bin/swift", "test", "--package-path", work, "--scratch-path", filepath.Join(dir, "build"),
		"--build-system", "native", "--disable-sandbox", "--disable-automatic-resolution",
		"--disable-index-store", "--jobs", "2")
	return swiftResult(result, output, code, err)
}

func swiftResult(result LocalSwiftResult, output string, code int, err error) (LocalSwiftResult, error) {
	if errors.Is(err, ErrCleanup) {
		return result, err
	}
	result.Status, result.ExitStatus = model.Failed, code
	if result.Tests = swiftTestCount(output, code); err == nil && result.Tests > 0 {
		result.Status = "PASSED"
	}
	return result, nil
}
