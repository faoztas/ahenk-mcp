// Copyright 2026 Furkan Anıl Öztaş
// SPDX-License-Identifier: Apache-2.0

package providers

import (
	"errors"
	"strings"
	"testing"

	"github.com/faoztas/ahenk-mcp/internal/model"
)

func TestChildEnvSecretFiltering(t *testing.T) {
	baseEnv := []string{
		"PATH=/usr/bin:/bin",
		"HOME=/Users/alice",
		"USER=alice",
		"LANG=en_US.UTF-8",
		"AWS_ACCESS_KEY_ID=AKIAIOSFODNN7EXAMPLE",
		"aws_secret_access_key=wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY",
		"GITHUB_TOKEN=ghp_exampletoken1234567890",
		"gh_enterprise_token=ghe_secrettoken",
		"OPENAI_API_KEY=sk-test-key-123456",
		"openai_org=org-123", // starts with OPENAI_
		"ANTHROPIC_API_KEY=sk-ant-123",
		"GEMINI_API_KEY=AIzaSyFakeKey",
		"GOOGLE_API_KEY=AIzaSyAnotherKey",
		"GOOGLE_APPLICATION_CREDENTIALS=/path/to/creds.json",
		"CODEX_SESSION_TOKEN=codex_token_val",
		"claude_auth_token=claude_val",
		"OPENCODE_SECRET=opencode_val",
		"TOKEN_SERVICE=token_val",
		"SECRET_BACKUP_KEY=backup_secret",
		"password_hash=argon2id_secret",
		"KEY_PAIR=pem_content",
		"AGY_ADC_AUTH=adc_token",
	}

	sanitized := ChildEnv(baseEnv, "/tmp", 0, 1)

	envMap := make(map[string]string)
	for _, e := range sanitized {
		k, v, ok := strings.Cut(e, "=")
		if ok {
			envMap[k] = v
		}
	}

	// Safe variables must remain intact
	safeVars := []string{"PATH", "HOME", "USER", "LANG"}
	for _, v := range safeVars {
		if _, exists := envMap[v]; !exists {
			t.Errorf("expected safe variable %s to be preserved in ChildEnv", v)
		}
	}

	// Recursion variables must be injected
	if envMap["AHENK_CHILD_SESSION"] != "1" {
		t.Errorf("expected AHENK_CHILD_SESSION=1, got %s", envMap["AHENK_CHILD_SESSION"])
	}
	if envMap["AHENK_CHILD_DEPTH"] != "1" {
		t.Errorf("expected AHENK_CHILD_DEPTH=1, got %s", envMap["AHENK_CHILD_DEPTH"])
	}
	if envMap["AHENK_MAX_DEPTH"] != "1" {
		t.Errorf("expected AHENK_MAX_DEPTH=1, got %s", envMap["AHENK_MAX_DEPTH"])
	}

	// All sensitive keys must be stripped
	blockedVars := []string{
		"AWS_ACCESS_KEY_ID",
		"aws_secret_access_key",
		"GITHUB_TOKEN",
		"gh_enterprise_token",
		"OPENAI_API_KEY",
		"openai_org",
		"ANTHROPIC_API_KEY",
		"GEMINI_API_KEY",
		"GOOGLE_API_KEY",
		"GOOGLE_APPLICATION_CREDENTIALS",
		"CODEX_SESSION_TOKEN",
		"claude_auth_token",
		"OPENCODE_SECRET",
		"TOKEN_SERVICE",
		"SECRET_BACKUP_KEY",
		"password_hash",
		"KEY_PAIR",
		"AGY_ADC_AUTH",
	}

	for _, k := range blockedVars {
		for _, e := range sanitized {
			varName, _, _ := strings.Cut(e, "=")
			if strings.EqualFold(varName, k) {
				t.Errorf("secret variable %s was not filtered from ChildEnv", varName)
			}
		}
	}
}

func TestChildEnvSecretFiltering_Suffixes(t *testing.T) {
	baseEnv := []string{
		"PATH=/usr/bin:/bin",
		"HOME=/Users/alice",
		"GOPATH=/Users/alice/go",
		"KEYBOARD_LAYOUT=us",
		"MY_API_KEY=sk-my-api-key",
		"CUSTOM_TOKEN=tok-custom",
		"APP_SECRET=sec-app",
		"DB_PASSWORD=pw-db",
		"SIGNING_KEY=key-signing",
		"TOKEN=exact-token",
		"SECRET=exact-secret",
		"PASSWORD=exact-password",
		"KEY=exact-key",
		"API_KEY=exact-api-key",
	}

	sanitized := ChildEnv(baseEnv, "/tmp", 0, 1)

	envMap := make(map[string]string)
	for _, e := range sanitized {
		k, v, ok := strings.Cut(e, "=")
		if ok {
			envMap[k] = v
		}
	}

	safeVars := map[string]string{
		"PATH":            "/usr/bin:/bin",
		"HOME":            "/Users/alice",
		"GOPATH":          "/Users/alice/go",
		"KEYBOARD_LAYOUT": "us",
	}
	for k, want := range safeVars {
		got, exists := envMap[k]
		if !exists {
			t.Errorf("expected benign variable %s to be preserved in ChildEnv", k)
		} else if got != want {
			t.Errorf("expected benign variable %s=%q, got %q", k, want, got)
		}
	}

	blockedVars := []string{
		"MY_API_KEY",
		"CUSTOM_TOKEN",
		"APP_SECRET",
		"DB_PASSWORD",
		"SIGNING_KEY",
		"TOKEN",
		"SECRET",
		"PASSWORD",
		"KEY",
		"API_KEY",
	}
	for _, k := range blockedVars {
		if _, exists := envMap[k]; exists {
			t.Errorf("secret variable %s was not filtered from ChildEnv", k)
		}
	}
}

func TestCheckRecursionGuard_EnvMaxDepthCannotBeBypassed(t *testing.T) {
	env := []string{
		"AHENK_CHILD_SESSION=1",
		"AHENK_CHILD_DEPTH=1",
		"AHENK_MAX_DEPTH=1",
	}
	err := CheckRecursionGuard(env, 99)
	if err == nil {
		t.Fatal("expected CheckRecursionGuard to deny when AHENK_MAX_DEPTH=1 even with requested maxDepth=99")
	}
	var fault *model.Fault
	if !errors.As(err, &fault) || fault.Code != "POLICY_DENIED" || fault.Gate != "RECURSIVE_DELEGATION_DENIED" {
		t.Fatalf("expected POLICY_DENIED / RECURSIVE_DELEGATION_DENIED, got: %#v", err)
	}

	// Fail closed if AHENK_CHILD_SESSION=1 with missing or invalid depth.
	for _, badEnv := range [][]string{
		{"AHENK_CHILD_SESSION=1", "AHENK_MAX_DEPTH=5"},
		{"AHENK_CHILD_SESSION=1", "AHENK_CHILD_DEPTH=0", "AHENK_MAX_DEPTH=5"},
		{"AHENK_CHILD_SESSION=1", "AHENK_CHILD_DEPTH=invalid", "AHENK_MAX_DEPTH=5"},
	} {
		err := CheckRecursionGuard(badEnv, 5)
		if err == nil {
			t.Fatalf("expected fail-closed denial for %v, got nil", badEnv)
		}
		var badFault *model.Fault
		if !errors.As(err, &badFault) || badFault.Code != "POLICY_DENIED" || badFault.Gate != "RECURSIVE_DELEGATION_DENIED" {
			t.Fatalf("expected POLICY_DENIED / RECURSIVE_DELEGATION_DENIED for %v, got: %#v", badEnv, err)
		}
	}

	// Allowed when current depth < effective max depth.
	allowedEnv := []string{
		"AHENK_CHILD_SESSION=1",
		"AHENK_CHILD_DEPTH=1",
		"AHENK_MAX_DEPTH=3",
	}
	if err := CheckRecursionGuard(allowedEnv, 3); err != nil {
		t.Fatalf("expected nested delegation at depth 1 < 3 to be allowed, got %v", err)
	}

	// DepthFromEnv and EffectiveMaxDepth behavior.
	cur, maxD := DepthFromEnv([]string{"AHENK_CHILD_DEPTH=2", "AHENK_MAX_DEPTH=4"})
	if cur != 2 || maxD != 4 {
		t.Fatalf("DepthFromEnv returned (%d, %d), want (2, 4)", cur, maxD)
	}
	if eff := EffectiveMaxDepth(env, 99); eff != 1 {
		t.Fatalf("EffectiveMaxDepth returned %d, want 1", eff)
	}
	if eff := EffectiveMaxDepth([]string{"AHENK_MAX_DEPTH=5"}, 2); eff != 2 {
		t.Fatalf("EffectiveMaxDepth returned %d, want 2", eff)
	}

	// ChildEnv must cap maxDepth when parent env has a lower positive AHENK_MAX_DEPTH.
	child := ChildEnv([]string{"AHENK_CHILD_SESSION=1", "AHENK_CHILD_DEPTH=1", "AHENK_MAX_DEPTH=2"}, "/tmp", 1, 99)
	var gotDepth, gotMax string
	var maxCount int
	for _, e := range child {
		k, v, ok := strings.Cut(e, "=")
		if !ok {
			continue
		}
		if k == "AHENK_CHILD_DEPTH" {
			gotDepth = v
		}
		if k == "AHENK_MAX_DEPTH" {
			gotMax = v
			maxCount++
		}
	}
	if gotDepth != "2" || gotMax != "2" || maxCount != 1 {
		t.Fatalf("ChildEnv depth/max mismatch: depth=%q max=%q count=%d", gotDepth, gotMax, maxCount)
	}
}
