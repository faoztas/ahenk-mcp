// Copyright 2026 Furkan Anıl Öztaş
// SPDX-License-Identifier: Apache-2.0

package orchestration

import (
	"testing"
	"time"

	"github.com/faoztas/ahenk-mcp/internal/model"
)

func TestDaniiisloAliasKeepsApprovedFallbackScope(t *testing.T) {
	t.Run("alias_family_normalizes_to_one_provider", func(t *testing.T) {
		aliases := []string{"antigravity", "agy", "agy-headless", "agy-acp", "daniiislo"}
		for _, alias := range aliases {
			if got := NormalizeProvider(alias); got != "agy" {
				t.Fatalf("alias %q normalized to %q, want agy", alias, got)
			}
		}
		for _, other := range []string{"codex", "codex-cli", "claude", "opencode", "unknown-provider"} {
			if NormalizeProvider(other) == "agy" && other != "agy" {
				t.Fatalf("unrelated provider %q collapsed into agy family", other)
			}
		}
	})

	t.Run("circuit_breaker_shares_alias_family", func(t *testing.T) {
		cb := NewCircuitBreaker()
		cb.Trip("daniiislo", time.Hour)
		aliases := []string{"antigravity", "agy", "agy-headless", "agy-acp", "daniiislo"}
		for _, alias := range aliases {
			if st := cb.State(alias); st != CircuitOpen {
				t.Fatalf("alias %q state = %s, want OPEN after daniiislo trip", alias, st)
			}
			if cb.Allow(alias) {
				t.Fatalf("alias %q admitted while family circuit is open", alias)
			}
		}
		cb.Success("antigravity")
		for _, alias := range aliases {
			if st := cb.State(alias); st != CircuitClosed {
				t.Fatalf("alias %q state = %s, want CLOSED after family recovery", alias, st)
			}
		}
	})

	t.Run("distinct_provider_counts_alias_family_once", func(t *testing.T) {
		e := Engine{jobs: map[string]model.Job{
			"parent": {ID: "parent", Distinct: true, Children: []string{"first", "second"}},
			"first":  {ID: "first", Parent: "parent", Attempts: []model.Attempt{{Number: 1, Provider: "agy-headless"}}},
			"second": {ID: "second", Parent: "parent"},
		}}
		for _, alias := range []string{"agy", "agy-acp", "daniiislo"} {
			if !e.distinctTaken(e.jobs["second"], alias) {
				t.Fatalf("same AGY family alias %q counted independently", alias)
			}
		}
		if e.distinctTaken(e.jobs["second"], "opencode") {
			t.Fatal("OpenCode must remain an independent provider family")
		}
	})

	t.Run("repo_allowlist_private_scope_stays_exact", func(t *testing.T) {
		privateGrant := &model.PrivateSourcePolicy{ExpiresAt: time.Now().Add(time.Hour)}
		nonPrivate := model.Repo{Providers: []string{"agy"}, PrivateSource: nil}
		for _, alias := range []string{"antigravity", "agy", "agy-headless", "agy-acp", "daniiislo"} {
			if !repoAllowsProvider(nonPrivate, alias) {
				t.Fatalf("non-private allowlist rejected family alias %q", alias)
			}
		}
		privateAgy := model.Repo{Providers: []string{"agy"}, PrivateSource: privateGrant}
		if !repoAllowsProvider(privateAgy, "agy") {
			t.Fatal("private allowlist rejected the exact approved provider")
		}
		if repoAllowsProvider(privateAgy, "daniiislo") || repoAllowsProvider(privateAgy, "agy-acp") ||
			repoAllowsProvider(privateAgy, "antigravity") || repoAllowsProvider(privateAgy, "agy-headless") {
			t.Fatal("private allowlist broadened fallback across AGY aliases")
		}
		privateDaniiislo := model.Repo{Providers: []string{"daniiislo"}, PrivateSource: privateGrant}
		if !repoAllowsProvider(privateDaniiislo, "daniiislo") {
			t.Fatal("private allowlist rejected the exact approved daniiislo provider")
		}
		if repoAllowsProvider(privateDaniiislo, "agy") {
			t.Fatal("private allowlist broadened daniiislo to the agy alias")
		}
	})

	t.Run("private_source_route_exact_provider_and_model", func(t *testing.T) {
		now := time.Now().UTC()
		future := now.Add(time.Hour)
		validHash := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
		policy := &model.PrivateSourcePolicy{
			OwnerConsent: nil,
			ExpiresAt:    future,
			Files:        map[string]string{"main.go": model.Hash([]byte("package main\n"))},
			Briefs:       []string{validHash},
			Routes: []model.PrivateRoute{{
				Provider:             "agy",
				Model:                "gemini-4-argon",
				Fingerprint:          validHash,
				DataHandlingReviewed: true,
			}},
		}
		task := model.Task{
			Mode:      "read_only",
			DataClass: model.PrivateSourceData,
		}
		approved := model.Capability{Provider: "agy", Model: "gemini-4-argon", Status: "available", Mode: "fixture", Fingerprint: validHash}
		ok, gate := policy.RouteDecision(approved, task, now)
		if !ok || gate != "" {
			t.Fatalf("approved exact route rejected: ok=%v gate=%q", ok, gate)
		}
		for _, alias := range []string{"antigravity", "agy-headless", "agy-acp", "daniiislo"} {
			candidate := model.Capability{Provider: alias, Model: "gemini-4-argon", Status: "available", Mode: "fixture", Fingerprint: validHash}
			if admitted, _ := policy.RouteDecision(candidate, task, now); admitted {
				t.Fatalf("private source route broadened to alias %q", alias)
			}
		}
		wrongModel := model.Capability{Provider: "agy", Model: "gemini-3.8-flash", Status: "available", Mode: "fixture", Fingerprint: validHash}
		if admitted, _ := policy.RouteDecision(wrongModel, task, now); admitted {
			t.Fatal("private source route ignored model mismatch")
		}
		expired := *policy
		expired.ExpiresAt = now.Add(-time.Minute)
		if admitted, _ := expired.RouteDecision(approved, task, now); admitted {
			t.Fatal("expired private grant still admitted a route")
		}
	})

	t.Run("private_source_check_request_exact_route", func(t *testing.T) {
		now := time.Now().UTC()
		future := now.Add(time.Hour)
		validHash := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
		policy := &model.PrivateSourcePolicy{
			ExpiresAt: future,
			Files:     map[string]string{"main.go": model.Hash([]byte("package main\n"))},
			Briefs:    []string{validHash},
			Routes: []model.PrivateRoute{{
				Provider:             "daniiislo",
				Model:                "gemini-4-argon",
				Fingerprint:          validHash,
				DataHandlingReviewed: true,
			}},
		}
		task := model.Task{
			Mode:              "read_only",
			DataClass:         model.PrivateSourceData,
			PreferredProvider: "agy",
		}
		if fault := policy.CheckRequest(task, now); fault == nil {
			t.Fatal("preferred provider mismatch was not denied")
		}
		task.PreferredProvider = "daniiislo"
		task.PreferredModel = "gemini-4-argon"
		task.AllowedPaths = []string{"main.go"}
		task.Objective = "synthetic"
		task.Acceptance = []model.Check{}
		if fault := policy.CheckRequest(task, now); fault == nil {
			t.Fatal("unapproved brief was not denied")
		}
	})
}
