// Copyright 2026 Furkan Anıl Öztaş
// SPDX-License-Identifier: Apache-2.0

package providers

import (
	"os"
	"strconv"
	"strings"

	"github.com/faoztas/ahenk-mcp/internal/model"
)

var blockedEnvPrefixes = []string{
	"AWS_",
	"GITHUB_",
	"GH_",
	"OPENAI_",
	"ANTHROPIC_",
	"GEMINI_",
	"GOOGLE_",
	"CODEX_",
	"CLAUDE_",
	"OPENCODE_",
	"TOKEN",
	"SECRET",
	"PASSWORD",
	"KEY",
}

func isBlockedSecretEnv(key string) bool {
	upper := strings.ToUpper(key)
	if upper == "AGY_ADC_AUTH" {
		return true
	}
	for _, p := range blockedEnvPrefixes {
		if strings.HasPrefix(upper, p) {
			return true
		}
	}
	return false
}

// ChildEnv builds the environment for a child CLI process, injecting recursion
// markers and stripping secret-bearing variables.
func ChildEnv(base []string, tempDir string, currentDepth, maxDepth int) []string {
	out := make([]string, 0, len(base)+8)
	for _, e := range base {
		k, _, ok := strings.Cut(e, "=")
		if !ok {
			out = append(out, e)
			continue
		}
		if isBlockedSecretEnv(k) {
			continue
		}
		out = append(out, e)
	}
	if maxDepth <= 0 {
		maxDepth = 1
	}
	out = append(out,
		"AHENK_CHILD_SESSION=1",
		"AHENK_CHILD_DEPTH="+strconv.Itoa(currentDepth+1),
		"AHENK_MAX_DEPTH="+strconv.Itoa(maxDepth),
	)
	if tempDir != "" {
		out = append(out, "BROWSER="+tempDir+"/open")
	}
	_ = os.Environ
	return out
}

// CheckRecursionGuard denies nested delegation when depth limit is reached.
func CheckRecursionGuard(env []string, maxDepth int) error {
	m := map[string]string{}
	for _, e := range env {
		k, v, ok := strings.Cut(e, "=")
		if ok {
			m[k] = v
		}
	}
	if m["AHENK_CHILD_SESSION"] != "1" {
		return nil
	}
	current, _ := strconv.Atoi(m["AHENK_CHILD_DEPTH"])
	if maxDepth <= 0 {
		maxDepth = 1
	}
	if current >= maxDepth {
		return model.GateError("POLICY_DENIED", "RECURSIVE_DELEGATION_DENIED",
			"subagent session cannot recursively delegate tasks (AHENK_CHILD_SESSION=1, depth limit reached)",
			"complete_task_directly_in_subagent")
	}
	return nil
}
