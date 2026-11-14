# Local Swift and XCTest

Run `ahenk local-test --config FILE` on macOS with full Xcode selected. The command runs directly on this Mac and returns a JSON result with its runner, phase, network boundary, test count and exit status. The operator selects the inputs in a private file, and the command sends no model requests.

The manifest must be a regular file owned by the operator with mode `0600` or `0400`. Use the canonical Git worktree root, the full current HEAD commit and a SHA-256 digest of each reviewed file. Calculate a digest from the committed bytes with `git show HEAD:path/to/file.swift | shasum -a 256`. Ahenk copies up to 128 approved regular UTF-8 files from the pinned commit into private scratch space. The limits are 256 KiB per file and 2 MiB in total. The original checkout and index are outside the test write scope.

For a macOS SwiftPM package, use this manifest with real file hashes and the full commit.

```json
{
  "root": "/absolute/path/to/repo",
  "commit": "<full-commit>",
  "runner": "swiftpm",
  "files": {
    "Package.swift": "<sha256>",
    "Tests/ExampleTests/ExampleTests.swift": "<sha256>"
  }
}
```

Include the package source, tests and any required local resources in `files`. Prepare the package so its dependencies are available from the approved local inputs. SwiftPM automatic dependency resolution and network access are disabled.

For logic-only iOS Simulator XCTest, use `"runner": "simulator-xctest"`, provide the XCTest and implementation Swift files, and add the installed runtime and device type. Ahenk compiles the approved files into one XCTest module and creates and deletes a disposable simulator for the run.

```json
{
  "root": "/absolute/path/to/repo",
  "commit": "<full-commit>",
  "runner": "simulator-xctest",
  "device_type": "com.apple.CoreSimulator.SimDeviceType.iPhone-17",
  "runtime": "com.apple.CoreSimulator.SimRuntime.iOS-26-5",
  "files": {"Tests/ExampleTests.swift": "<sha256>"}
}
```

App-hosted tests, UI tests and Xcode scheme execution require their own launch path and verification. The timeout is configurable with `--timeout`, up to 15 minutes. Cold Simulator boots and Swift module builds can take several minutes.

## Network enforcement and evidence

The host compiler and XCTest commands run under a macOS Seatbelt profile that denies network operations and Mach service lookups. Writes are limited to scratch space and `/dev/null`. Before accepting a run, Ahenk connects to its own IPv4 and IPv6 loopback listeners with a control process and requires an explicit permission denial from the restricted process. For iOS, the same paired checks run through CoreSimulator. Ahenk launches the sandbox inside `simctl spawn` before it loads XCTest. The guarded process restores the Simulator runtime path and executes Xcode's XCTest agent under the inherited profile.

The boundary covers the tested direct socket operations on the selected OS and runtime. Use reviewed, trusted test inputs. The supported launch path is the standalone XCTest agent. The command reports `NOT_RUN` if it cannot establish the required network boundary and `FAILED` for an unsuccessful test invocation. It requires a successful XCTest summary with at least one executed test to report `PASSED`. Raw compiler and test output stays out of the JSON response. A cleanup failure quarantines the scratch directory for inspection.

The synthetic [network regression fixture](../internal/execution/testdata/swift/NetworkBoundary.swift) attempts direct TCP connections and UDP sends on IPv4 and IPv6 from the XCTest process itself. The [integration findings](integration-findings.md#simulator-processes-do-not-inherit-the-simctl-clients-ip-deny-policy) preserve the reproduction for the original launch path.

## Verified runs

On Intel macOS 26.7.2 with Xcode 26.5 and iOS Simulator 26.5, both the macOS SwiftPM and iOS Simulator runners executed the two network regression tests and exited with status 0. The actual XCTest processes received permission errors on IPv4 and IPv6 TCP connects and UDP sends. Paired unrestricted and restricted probes also connected and received a permission denial, respectively, on a same-host non-loopback IPv4 address in the Simulator. The successful Simulator run deleted its disposable device. A canceled run removed its device and scratch directory.
