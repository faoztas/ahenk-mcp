// Copyright 2026 Furkan Anıl Öztaş
// SPDX-License-Identifier: Apache-2.0

package providers

import (
	"strings"
	"testing"
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
