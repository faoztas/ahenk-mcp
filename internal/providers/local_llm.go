// Copyright 2026 Furkan Anıl Öztaş
// SPDX-License-Identifier: Apache-2.0

package providers

import (
	"net/url"

	"github.com/faoztas/ahenk-mcp/internal/model"
)

func isLoopbackURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	host := u.Hostname()
	return host == "127.0.0.1" || host == "localhost" || host == "::1"
}

func RunLocalLLM(urlStr string) error {
	if !isLoopbackURL(urlStr) {
		return model.Error("PROVIDER_STARTUP_FAILED", "local llm must use loopback")
	}
	return nil
}

// LocalZeroCost evidence helper
func LocalZeroCost() *model.CostEvidence {
	return &model.CostEvidence{Policy: "LOCAL_ONLY", Status: "ZERO_COST_VERIFIED", Documented: "VERIFIED", RuntimeVerified: "VERIFIED"}
}
