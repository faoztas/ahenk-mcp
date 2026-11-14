// Copyright 2026 Furkan Anıl Öztaş
// SPDX-License-Identifier: Apache-2.0

package readiness

import (
	"os"
	"runtime"
	"slices"

	"github.com/faoztas/ahenk-mcp/internal/model"
)

// LocalRunners reports provider-free local verification capabilities separately
// from external provider delegation.
func LocalRunners(cfg model.Config) []model.LocalRunnerCapability {
	swiftStatus := "unavailable"
	swiftReason := "DARWIN_SANDBOX_OR_SWIFT_UNAVAILABLE"
	if runtime.GOOS == "darwin" {
		if _, err := os.Stat("/usr/bin/sandbox-exec"); err == nil {
			if _, err := os.Stat("/usr/bin/swift"); err == nil {
				swiftStatus = "available"
				swiftReason = "DIGEST_PINNED_SWIFTPM_HEAD_ONLY"
			}
		}
	}
	simStatus := "unavailable"
	simReason := "XCRUN_OR_SANDBOX_UNAVAILABLE"
	if swiftStatus == "available" {
		if _, err := os.Stat("/usr/bin/xcrun"); err == nil {
			simStatus = "available"
			simReason = "INNER_SIMULATOR_SANDBOX_VERIFIED_AT_DISPATCH"
		}
	}
	xcodeRunner := model.LocalRunnerCapability{
		Runner:          "xcode-scheme",
		Status:          "unavailable",
		Reason:          "XCODE_SCHEME_RUNNER_UNVERIFIED",
		NextAction:      "route_xcode_scheme_and_ui_acceptance_to_authorized_local_owner",
		NetworkBoundary: "UNVERIFIED",
	}
	if cfg.LocalXcodeRunner != nil && cfg.LocalXcodeRunner.Verified && len(cfg.LocalXcodeRunner.Schemes) > 0 {
		xcodeRunner = model.LocalRunnerCapability{
			Runner:          "xcode-scheme",
			Status:          "available",
			Reason:          "VERIFIED_LOCAL_XCODE_SCHEME_RUNNER",
			NetworkBoundary: "VERIFIED_LOCAL_RUNNER",
			Schemes:         slices.Clone(cfg.LocalXcodeRunner.Schemes),
			AcceptanceModes: slices.Clone(cfg.LocalXcodeRunner.AcceptanceModes),
		}
	}
	return []model.LocalRunnerCapability{
		{
			Runner:          "swiftpm",
			Status:          swiftStatus,
			Reason:          swiftReason,
			NetworkBoundary: "HOST_IP_DENIED",
			AcceptanceModes: []string{"unit"},
		},
		{
			Runner:          "simulator-xctest",
			Status:          simStatus,
			Reason:          simReason,
			NetworkBoundary: "SIMULATOR_INNER_IP_DENIED",
			AcceptanceModes: []string{"standalone_xctest"},
		},
		xcodeRunner,
	}
}
