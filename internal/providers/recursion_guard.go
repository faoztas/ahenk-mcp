// Copyright 2026 Furkan Anıl Öztaş
// SPDX-License-Identifier: Apache-2.0

package providers

import (
	"strconv"
	"strings"

	"github.com/faoztas/ahenk-mcp/internal/model"
)

var blockedEnvExact = map[string]bool{
	"AGY_ADC_AUTH": true,
	"TOKEN":        true,
	"SECRET":       true,
	"PASSWORD":     true,
	"KEY":          true,
	"API_KEY":      true,
}

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
	"TOKEN_",
	"SECRET_",
	"PASSWORD_",
	"KEY_",
	"API_KEY_",
}

var blockedEnvSuffixes = []string{
	"_API_KEY",
	"_KEY",
	"_TOKEN",
	"_SECRET",
	"_PASSWORD",
}

func isBlockedSecretEnv(key string) bool {
	upper := strings.ToUpper(strings.TrimSpace(key))
	if blockedEnvExact[upper] {
		return true
	}
	for _, p := range blockedEnvPrefixes {
		if strings.HasPrefix(upper, p) {
			return true
		}
	}
	for _, s := range blockedEnvSuffixes {
		if strings.HasSuffix(upper, s) {
			return true
		}
	}
	return false
}

// DepthFromEnv reads AHENK_CHILD_DEPTH and AHENK_MAX_DEPTH from env.
// If AHENK_CHILD_SESSION=1 is present without a positive AHENK_CHILD_DEPTH,
// currentDepth fails safe to 1.
func DepthFromEnv(env []string) (currentDepth, maxDepth int) {
	var childSession string
	for _, e := range env {
		k, v, ok := strings.Cut(e, "=")
		if !ok {
			continue
		}
		switch k {
		case "AHENK_CHILD_SESSION":
			childSession = strings.TrimSpace(v)
		case "AHENK_CHILD_DEPTH":
			if d, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && d > 0 {
				currentDepth = d
			} else {
				currentDepth = 0
			}
		case "AHENK_MAX_DEPTH":
			if m, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && m > 0 {
				maxDepth = m
			} else {
				maxDepth = 0
			}
		}
	}
	if childSession == "1" && currentDepth <= 0 {
		currentDepth = 1
	}
	return currentDepth, maxDepth
}

// EffectiveMaxDepth returns requestedMaxDepth clamped by any positive AHENK_MAX_DEPTH in env.
func EffectiveMaxDepth(env []string, requestedMaxDepth int) int {
	effective := requestedMaxDepth
	if effective <= 0 {
		effective = 1
	}
	_, envMax := DepthFromEnv(env)
	if envMax > 0 && envMax < effective {
		effective = envMax
	}
	return effective
}

// ChildEnv builds the environment for a child CLI process, injecting recursion
// markers and stripping secret-bearing variables.
func ChildEnv(base []string, tempDir string, currentDepth, maxDepth int) []string {
	maxDepth = EffectiveMaxDepth(base, maxDepth)
	if currentDepth < 0 {
		currentDepth = 0
	}
	out := make([]string, 0, len(base)+8)
	for _, e := range base {
		k, _, ok := strings.Cut(e, "=")
		if !ok {
			out = append(out, e)
			continue
		}
		if k == "AHENK_CHILD_SESSION" || k == "AHENK_CHILD_DEPTH" || k == "AHENK_MAX_DEPTH" {
			continue
		}
		if tempDir != "" && k == "BROWSER" {
			continue
		}
		if isBlockedSecretEnv(k) {
			continue
		}
		out = append(out, e)
	}
	out = append(out,
		"AHENK_CHILD_SESSION=1",
		"AHENK_CHILD_DEPTH="+strconv.Itoa(currentDepth+1),
		"AHENK_MAX_DEPTH="+strconv.Itoa(maxDepth),
	)
	if tempDir != "" {
		out = append(out, "BROWSER="+tempDir+"/open")
	}
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
	if strings.TrimSpace(m["AHENK_CHILD_SESSION"]) != "1" {
		return nil
	}
	current, err := strconv.Atoi(strings.TrimSpace(m["AHENK_CHILD_DEPTH"]))
	if err != nil || current <= 0 {
		return model.GateError("POLICY_DENIED", "RECURSIVE_DELEGATION_DENIED",
			"subagent session cannot recursively delegate tasks (AHENK_CHILD_SESSION=1, invalid or missing depth)",
			"complete_task_directly_in_subagent")
	}
	effectiveMax := EffectiveMaxDepth(env, maxDepth)
	if current >= effectiveMax {
		return model.GateError("POLICY_DENIED", "RECURSIVE_DELEGATION_DENIED",
			"subagent session cannot recursively delegate tasks (AHENK_CHILD_SESSION=1, depth limit reached)",
			"complete_task_directly_in_subagent")
	}
	return nil
}
