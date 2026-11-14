// Copyright 2026 Furkan Anıl Öztaş
// SPDX-License-Identifier: Apache-2.0

package execution

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"
)

const swiftBundlePlist = `<?xml version="1.0" encoding="UTF-8"?>
<plist version="1.0"><dict>
<key>CFBundleIdentifier</key><string>org.ahenk.LocalTests</string>
<key>CFBundleExecutable</key><string>AhenkLocalTests</string>
<key>CFBundlePackageType</key><string>BNDL</string>
</dict></plist>
`

var swiftDeviceID = regexp.MustCompile(`^[0-9A-Fa-f]{8}(-[0-9A-Fa-f]{4}){3}-[0-9A-Fa-f]{12}$`)

func (e swiftEnvironment) simctl(ctx context.Context, args ...string) (string, int, error) {
	env := []string{"PATH=/usr/bin:/bin", "HOME=" + e.home, "TMPDIR=/private/tmp", "LC_ALL=C", "LANG=C"}
	if args[0] == "spawn" {
		env = append(env, "SIMCTL_CHILD_HOME="+e.dir, "SIMCTL_CHILD_TMPDIR="+e.dir,
			"SIMCTL_CHILD_LC_ALL=C", "SIMCTL_CHILD_LANG=C")
	}
	return e.command(ctx, env, append([]string{"/usr/bin/xcrun", "simctl"}, args...)...)
}

func (e swiftEnvironment) xcodePath(ctx context.Context, args ...string) (string, error) {
	output, _, err := e.command(ctx, e.env(), args...)
	if err != nil {
		return "", err
	}
	value := strings.TrimSpace(output)
	if !filepath.IsAbs(value) || strings.ContainsAny(value, "\r\n") {
		return "", errors.New("Xcode tool or SDK location could not be verified")
	}
	return value, nil
}

func (e swiftEnvironment) bootXCTestSimulator(ctx context.Context, device string) error {
	if _, _, err := e.simctl(ctx, "boot", device); err != nil {
		if errors.Is(err, ErrCleanup) {
			return err
		}
		return errors.New("could not boot the disposable iOS Simulator")
	}
	// Standalone XCTest needs process launch services. The home screen's
	// bootstatus milestones can continue after this launch path is usable.
	ready, stop := context.WithTimeout(ctx, 90*time.Second)
	defer stop()
	for ready.Err() == nil {
		probe, cancel := context.WithTimeout(ready, 8*time.Second)
		_, _, err := e.simctl(probe, "spawn", device, "/usr/bin/true")
		cancel()
		if err == nil || errors.Is(err, ErrCleanup) {
			return err
		}
		select {
		case <-ready.Done():
		case <-time.After(time.Second):
		}
	}
	return errors.New("disposable iOS Simulator did not become ready for XCTest")
}

func (e swiftEnvironment) removeXCTestSimulator(ctx context.Context, device string) error {
	cleanup, stop := context.WithTimeout(context.WithoutCancel(ctx), 35*time.Second)
	defer stop()
	shutdown, cancel := context.WithTimeout(cleanup, 12*time.Second)
	e.simctl(shutdown, "shutdown", device)
	cancel()
	for cleanup.Err() == nil {
		attempt, cancel := context.WithTimeout(cleanup, 8*time.Second)
		_, _, err := e.simctl(attempt, "delete", device)
		cancel()
		if err == nil {
			return nil
		}
		list, cancel := context.WithTimeout(cleanup, 5*time.Second)
		output, _, err := e.simctl(list, "list", "devices", "-j")
		cancel()
		if err == nil && json.Valid([]byte(output)) && !strings.Contains(strings.ToLower(output), strconv.Quote(strings.ToLower(device))) {
			return nil
		}
		select {
		case <-cleanup.Done():
		case <-time.After(time.Second):
		}
	}
	return ErrCleanup
}

func (e swiftEnvironment) runSimulatorSwift(ctx context.Context, cfg localSwiftConfig, work string, result LocalSwiftResult) (_ LocalSwiftResult, err error) {
	developer, err := e.xcodePath(ctx, "/usr/bin/xcode-select", "-p")
	if err != nil {
		return result, err
	}
	sdk, err := e.xcodePath(ctx, "/usr/bin/xcrun", "--sdk", "iphonesimulator", "--show-sdk-path")
	if err != nil {
		return result, err
	}
	platform := filepath.Join(developer, "Platforms/iPhoneSimulator.platform/Developer")
	xctest := filepath.Join(platform, "Library/Xcode/Agents/xctest")
	swiftc := filepath.Join(developer, "Toolchains/XcodeDefault.xctoolchain/usr/bin/swiftc")

	result.Phase = "SIMULATOR_BOOT"
	name := "Ahenk-XCTest-" + filepath.Base(e.dir)
	if err := Save(e.dir, "simulator-name", []byte(name)); err != nil {
		return result, err
	}
	output, _, err := e.simctl(ctx, "create", name, cfg.DeviceType, cfg.Runtime)
	device := strings.TrimSpace(output)
	if err != nil || !swiftDeviceID.MatchString(device) {
		if errors.Is(err, ErrCleanup) || ctx.Err() != nil {
			return result, errors.Join(ErrCleanup, errors.New("Simulator creation could not be confirmed for "+name))
		}
		return result, errors.New("could not create a disposable iOS Simulator")
	}
	defer func() {
		err = errors.Join(err, e.removeXCTestSimulator(ctx, device))
	}()
	if err := Save(e.dir, "simulator-id", []byte(device)); err != nil {
		return result, err
	}
	if err := e.bootXCTestSimulator(ctx, device); err != nil {
		return result, err
	}
	output, _, err = e.simctl(ctx, "getenv", device, "SIMULATOR_ROOT")
	runtimeRoot := strings.TrimSpace(output)
	if err != nil || !filepath.IsAbs(runtimeRoot) || strings.ContainsAny(runtimeRoot, "\r\n") {
		return result, errors.New("Simulator runtime root could not be verified")
	}
	control := func(ctx context.Context, args ...string) (string, int, error) {
		return e.simctl(ctx, append([]string{"spawn", device}, args...)...)
	}
	restricted := func(ctx context.Context, args ...string) (string, int, error) {
		prefix := []string{"spawn", device, "/usr/bin/sandbox-exec", "-D", "OPERATOR_HOME=" + e.home,
			"-D", "WORK=" + e.dir, "-p", swiftPolicy + "\n(deny process-fork)",
			// Restore the Simulator dyld root after the platform launcher clears it.
			"/usr/bin/env", "DYLD_ROOT_PATH=" + runtimeRoot}
		return e.simctl(ctx, append(prefix, args...)...)
	}
	result.Phase = "SIMULATOR_NETWORK"
	if err := verifySwiftNetwork(ctx, control, restricted); err != nil {
		if errors.Is(err, ErrCleanup) {
			return result, err
		}
		return result, nil
	}
	result.NetworkBoundary = "HOST_AND_SIMULATOR_IP_DENIED"
	result.Phase = "BUILD"

	bundle := filepath.Join(e.dir, "AhenkLocalTests.xctest")
	if err := os.Mkdir(bundle, 0700); err != nil {
		return result, err
	}
	if err := os.WriteFile(filepath.Join(bundle, "Info.plist"), []byte(swiftBundlePlist), 0600); err != nil {
		return result, err
	}
	arch := "x86_64"
	if runtime.GOARCH == "arm64" {
		arch = "arm64"
	}
	lib, frameworks := filepath.Join(platform, "usr/lib"), filepath.Join(platform, "Library/Frameworks")
	args := []string{swiftc, "-sdk", sdk, "-target", arch + "-apple-ios16.0-simulator", "-module-cache-path", filepath.Join(e.dir, "modules"),
		"-I", lib, "-F", frameworks, "-L", lib, "-Xlinker", "-rpath", "-Xlinker", frameworks, "-Xlinker", "-rpath", "-Xlinker", lib,
		"-emit-library", "-module-name", "AhenkLocalTests", "-o", filepath.Join(bundle, "AhenkLocalTests")}
	for _, name := range slices.Sorted(maps.Keys(cfg.Files)) {
		args = append(args, filepath.Join(work, name))
	}
	if _, code, err := e.sandbox(ctx, args...); err != nil {
		return swiftResult(result, "", code, err)
	}
	result.Phase = "XCTEST"
	output, code, err := restricted(ctx, xctest, bundle)
	return swiftResult(result, output, code, err)
}
