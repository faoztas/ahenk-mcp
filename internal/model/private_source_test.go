// Copyright 2026 Furkan Anıl Öztaş
// SPDX-License-Identifier: Apache-2.0

package model

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestPrivateSourceAdmission(t *testing.T) {
	const path = "internal/example.go"
	digest := Hash([]byte("package example\n"))
	route := PrivateRoute{Provider: "opencode", Model: "opencode/glm-5.3-flash-free",
		Fingerprint: strings.Repeat("a", 64), Transport: OpenCodeNativeReadAsk, DataHandlingReviewed: true}
	denyConsent := false
	allowConsent := true
	for _, tc := range []struct {
		name     string
		change   func(*Repo, *Task)
		want     bool
		wantGate string
	}{
		{"approved", func(*Repo, *Task) {}, true, ""},
		{"explicit consent approved", func(r *Repo, _ *Task) { r.PrivateSource.OwnerConsent = &allowConsent }, true, ""},
		{"no opt in", func(r *Repo, _ *Task) { r.PrivateSource = nil }, false, "PRIVATE_GRANT_REQUIRED"},
		{"missing owner consent", func(r *Repo, _ *Task) { r.PrivateSource.OwnerConsent = &denyConsent }, false, "OWNER_CONSENT_REQUIRED"},
		{"expired", func(r *Repo, _ *Task) { r.PrivateSource.ExpiresAt = time.Now().Add(-time.Hour) }, false, "GRANT_EXPIRED"},
		{"unapproved path", func(_ *Repo, t *Task) { t.AllowedPaths = []string{"internal/other.go"} }, false, "PATH_NOT_APPROVED"},
		{"directory expansion", func(_ *Repo, t *Task) { t.AllowedPaths = []string{"internal"} }, false, "PATH_NOT_APPROVED"},
		{"working changes", func(_ *Repo, t *Task) { t.WorkspaceMode = "include_working_changes" }, false, "PRIVATE_MODE_UNSUPPORTED"},
		{"synthetic relabel", func(_ *Repo, t *Task) { t.DataClass = "synthetic" }, false, "DATA_CLASS_MISMATCH"},
		{"public relabel", func(_ *Repo, t *Task) { t.DataClass = "public" }, false, "DATA_CLASS_MISMATCH"},
		{"changed objective", func(_ *Repo, t *Task) { t.Objective += " unreviewed" }, false, "BRIEF_NOT_APPROVED"},
		{"changed criterion", func(_ *Repo, t *Task) { t.Acceptance[0].Description += " unreviewed" }, false, "BRIEF_NOT_APPROVED"},
		{"secret in objective", func(_ *Repo, t *Task) { t.Objective += " sk-proj-1234567890abcdefghij" }, false, "SECRET_DETECTED"},
		{"personal data in objective", func(_ *Repo, t *Task) { t.Objective += " user@gmail.com" }, false, "PERSONAL_DATA_CLASSIFIED"},
		{"patch without writer isolation", func(_ *Repo, t *Task) { t.Mode = "patch" }, false, "WRITER_ISOLATION_UNVERIFIED"},
		{"unapproved provider", func(_ *Repo, t *Task) { t.PreferredProvider = "agy" }, false, "PRIVATE_SOURCE_ROUTE_DENIED"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := Repo{AllowedPaths: []string{"internal"}, Providers: []string{"opencode"},
				PrivateSource: &PrivateSourcePolicy{ExpiresAt: time.Now().Add(time.Hour),
					Files: map[string]string{path: digest}, Routes: []PrivateRoute{route}}}
			task := Task{Objective: "Summarize the approved file", Acceptance: []Check{{ID: "analysis", Description: "Give one fact"}},
				AllowedPaths: []string{path}, Mode: "read_only", DataClass: "repository_internal"}
			repo.PrivateSource.Briefs = []string{PrivateBriefDigest(task.Objective, task.Acceptance)}
			tc.change(&repo, &task)
			fault := task.Validate(repo)
			if (fault == nil) != tc.want {
				t.Fatalf("admission = %v; want success %t", fault, tc.want)
			}
			if fault != nil && tc.wantGate != "" && fault.Gate != tc.wantGate {
				t.Fatalf("gate = %q; want %q (fault: %+v)", fault.Gate, tc.wantGate, fault)
			}
		})
	}
}

func TestPrivateSourceAccountAttestationAndGates(t *testing.T) {
	files := map[string]string{"main.go": "package main\n"}
	agyRoute := PrivateRoute{Provider: "agy", Model: "gemini-3.8-flash-medium",
		Fingerprint: strings.Repeat("c", 64), ExpectedAccount: "owner@example.test", DataHandlingReviewed: true}
	p := &PrivateSourcePolicy{ExpiresAt: time.Now().Add(time.Hour),
		Files: map[string]string{"main.go": Hash([]byte(files["main.go"]))}, Routes: []PrivateRoute{agyRoute}}
	task := Task{Mode: "read_only", DataClass: "repository_internal", AllowedPaths: []string{"main.go"}}

	unattested := Capability{Provider: "agy", Model: agyRoute.Model, Fingerprint: agyRoute.Fingerprint, Status: "available"}
	if ok, gate := p.RouteDecision(unattested, task, time.Now()); ok || gate != "ACCOUNT_ATTESTATION_REQUIRED" {
		t.Fatalf("unattested AGY decision = (%t, %q); want (false, ACCOUNT_ATTESTATION_REQUIRED)", ok, gate)
	}
	wrongAccount := unattested
	wrongAccount.AttestedAccount = "other@example.test"
	if ok, gate := p.RouteDecision(wrongAccount, task, time.Now()); ok || gate != "ACCOUNT_MISMATCH" {
		t.Fatalf("wrong account decision = (%t, %q); want (false, ACCOUNT_MISMATCH)", ok, gate)
	}
	attested := unattested
	attested.AttestedAccount = "owner@example.test"
	if ok, gate := p.RouteDecision(attested, task, time.Now()); !ok || gate != "" {
		t.Fatalf("attested account decision = (%t, %q); want (true, \"\")", ok, gate)
	}
	if fault := p.CheckContext([]string{"main.go"}, map[string]string{"main.go": "package main\n// user@gmail.com\n"}); fault == nil || fault.Gate != "PERSONAL_DATA_CLASSIFIED" {
		t.Fatalf("personal data context check = %+v; want PERSONAL_DATA_CLASSIFIED", fault)
	}
	if fault := p.CheckContext([]string{"main.go"}, map[string]string{"main.go": "package main // changed\n"}); fault == nil || fault.Gate != "DIGEST_MISMATCH" {
		t.Fatalf("digest mismatch context check = %+v; want DIGEST_MISMATCH", fault)
	}
}

func TestPrivateSourceExactEgress(t *testing.T) {
	files := map[string]string{"main.go": "package main\n"}
	route := PrivateRoute{Provider: "opencode", Model: "opencode/glm-5.3-flash-free",
		Fingerprint: strings.Repeat("a", 64), Transport: OpenCodeNativeReadAsk, DataHandlingReviewed: true}
	p := &PrivateSourcePolicy{ExpiresAt: time.Now().Add(time.Hour),
		Files: map[string]string{"main.go": Hash([]byte(files["main.go"]))}, Routes: []PrivateRoute{route}}
	c := Capability{Provider: route.Provider, Model: route.Model, Fingerprint: route.Fingerprint,
		OpenCodeTransport: route.Transport, OpenCodeEndpoint: OpenCodeFreeEndpoint, OpenCodeAccount: OpenCodeAnonymous, Mode: "tool_free"}
	task := Task{Mode: "read_only", DataClass: "repository_internal", AllowedPaths: []string{"main.go"}}
	if !p.Allows(c, task, time.Now()) || p.CheckContext(task.AllowedPaths, files) != nil {
		t.Fatal("approved route and exact contents denied")
	}
	for _, change := range []func(*Capability){
		func(c *Capability) { c.Fingerprint = strings.Repeat("b", 64) },
		func(c *Capability) { c.Model = "opencode/other" },
		func(c *Capability) { c.OpenCodeTransport = OpenCodeStateless },
		func(c *Capability) { c.OpenCodeAccount = "different" },
		func(c *Capability) { c.OpenCodeEndpoint = "different" },
		func(c *Capability) { now := time.Now(); c.AuditValidUntil = &now },
	} {
		other := c
		change(&other)
		if p.Allows(other, task, time.Now()) {
			t.Fatal("route substitution accepted")
		}
	}
	if p.Allows(c, task, p.ExpiresAt) {
		t.Fatal("expired route accepted")
	}
	for _, invalid := range []map[string]string{
		{}, {"main.go": "changed"}, {"main.go": files["main.go"], "extra.go": "package main"},
	} {
		if p.CheckContext(task.AllowedPaths, invalid) == nil {
			t.Fatal("scope or byte substitution accepted")
		}
	}
}

func TestPrivateDispatchGrantRequiresCurrentExactIdentity(t *testing.T) {
	const path = "internal/example.go"
	const content = "package example\n"
	const account = "owner@example.test"
	now := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name     string
		change   func(*PrivateSourcePolicy, *Task, *Capability)
		wantGate string
	}{
		{"exact grant", func(*PrivateSourcePolicy, *Task, *Capability) {}, ""},
		{"matching account digest", func(_ *PrivateSourcePolicy, _ *Task, c *Capability) {
			c.AttestedAccountSHA256 = Hash([]byte(account))
		}, ""},
		{"provider mismatch", func(_ *PrivateSourcePolicy, _ *Task, c *Capability) { c.Provider = "opencode" }, "PRIVATE_SOURCE_ROUTE_DENIED"},
		{"provider alias mismatch", func(_ *PrivateSourcePolicy, _ *Task, c *Capability) { c.Provider = "antigravity" }, "PRIVATE_SOURCE_ROUTE_DENIED"},
		{"requested alias mismatch", func(_ *PrivateSourcePolicy, task *Task, _ *Capability) { task.PreferredProvider = "antigravity" }, "PRIVATE_SOURCE_ROUTE_DENIED"},
		{"model mismatch", func(_ *PrivateSourcePolicy, _ *Task, c *Capability) { c.Model = "different-model" }, "PRIVATE_SOURCE_ROUTE_DENIED"},
		{"account mismatch", func(_ *PrivateSourcePolicy, _ *Task, c *Capability) { c.AttestedAccount = "other@example.test" }, "ACCOUNT_MISMATCH"},
		{"account attestation missing", func(_ *PrivateSourcePolicy, _ *Task, c *Capability) { c.AttestedAccount = "" }, "ACCOUNT_ATTESTATION_REQUIRED"},
		{"conflicting account digest", func(_ *PrivateSourcePolicy, _ *Task, c *Capability) {
			c.AttestedAccountSHA256 = Hash([]byte("other@example.test"))
		}, "ACCOUNT_MISMATCH"},
		{"binary drift", func(_ *PrivateSourcePolicy, _ *Task, c *Capability) { c.Fingerprint = Hash([]byte("different binary")) }, "BINARY_CHANGED"},
		{"expired at dispatch", func(p *PrivateSourcePolicy, _ *Task, _ *Capability) { p.ExpiresAt = now }, "GRANT_EXPIRED"},
		{"owner consent revoked", func(p *PrivateSourcePolicy, _ *Task, _ *Capability) { *p.OwnerConsent = false }, "OWNER_CONSENT_REQUIRED"},
		{"unapproved path", func(_ *PrivateSourcePolicy, task *Task, _ *Capability) {
			task.AllowedPaths = []string{"internal/unapproved.go"}
		}, "PATH_NOT_APPROVED"},
		{"synthetic data class", func(_ *PrivateSourcePolicy, task *Task, _ *Capability) { task.DataClass = "synthetic" }, "PRIVATE_SOURCE_ROUTE_DENIED"},
		{"critical read-only risk", func(_ *PrivateSourcePolicy, task *Task, _ *Capability) { task.Risk = "critical" }, "PRIVATE_SOURCE_ROUTE_DENIED"},
		{"data handling unreviewed", func(p *PrivateSourcePolicy, _ *Task, _ *Capability) { p.Routes[0].DataHandlingReviewed = false }, "DATA_HANDLING_UNREVIEWED"},
		{"context digest revoked", func(p *PrivateSourcePolicy, _ *Task, _ *Capability) {
			p.Files[path] = Hash([]byte("package example // newly approved\n"))
		}, "DIGEST_MISMATCH"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			task := Task{Objective: "Summarize the approved file", Acceptance: []Check{{ID: "summary", Description: "Give one fact"}},
				Mode: "read_only", DataClass: PrivateSourceData, AllowedPaths: []string{path}}
			route := PrivateRoute{Provider: "agy", Model: "gemini-3.8-flash-medium", Fingerprint: Hash([]byte("approved binary")),
				ExpectedAccount: account, DataHandlingReviewed: true}
			consent := true
			policy := &PrivateSourcePolicy{OwnerConsent: &consent, ExpiresAt: now.Add(time.Hour), Routes: []PrivateRoute{route},
				Files: map[string]string{path: Hash([]byte(content))}, Briefs: []string{PrivateBriefDigest(task.Objective, task.Acceptance)}}
			capability := Capability{Provider: route.Provider, Model: route.Model, Fingerprint: route.Fingerprint,
				AttestedAccount: account, Status: "available"}
			tc.change(policy, &task, &capability)

			calls := 0
			// Apply the model gates in the same order as Registry.RunPrivate,
			// with a spy in place of the worker and a fixed dispatch clock.
			dispatch := func() string {
				if ok, gate := policy.RouteDecision(capability, task, now); !ok {
					return gate
				}
				if fault := policy.CheckRequest(task, now); fault != nil {
					return fault.Gate
				}
				if fault := policy.CheckContext(task.AllowedPaths, map[string]string{path: content}); fault != nil {
					return fault.Gate
				}
				calls++
				return ""
			}
			gate := dispatch()
			wantCalls := 0
			if tc.wantGate == "" {
				wantCalls = 1
			}
			if gate != tc.wantGate || calls != wantCalls {
				t.Fatalf("dispatch gate = %q, worker calls = %d; want %q and %d", gate, calls, tc.wantGate, wantCalls)
			}
		})
	}
}

func TestPrivateStatePhysicalOverlap(t *testing.T) {
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	repo := filepath.Join(base, "PrivateRepo")
	state := filepath.Join(repo, "state")
	separate := filepath.Join(base, "separate")
	for _, path := range []string{repo, state, separate} {
		if err := os.Mkdir(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	if privateStateSeparate(repo, state) || privateStateSeparate(state, repo) || !privateStateSeparate(repo, separate) {
		t.Fatal("physical private state boundary rejected separate dirs or admitted nested dirs")
	}
	alias := filepath.Join(base, "privaterepo", "state")
	if _, err := os.Stat(alias); err == nil && privateStateSeparate(repo, alias) {
		t.Fatal("case alias bypassed the private state boundary")
	}
}

func TestPrivateSourceCriticalKeywordRejectedInReadOnly(t *testing.T) {
	const path = "internal/example.go"
	digest := Hash([]byte("package example\n"))
	route := PrivateRoute{Provider: "opencode", Model: "opencode/glm-5.3-flash-free",
		Fingerprint: strings.Repeat("a", 64), Transport: OpenCodeNativeReadAsk, DataHandlingReviewed: true}
	for _, keyword := range []string{"security", "financial"} {
		t.Run(keyword, func(t *testing.T) {
			repo := Repo{AllowedPaths: []string{"internal"}, Providers: []string{"opencode"},
				PrivateSource: &PrivateSourcePolicy{ExpiresAt: time.Now().Add(time.Hour),
					Files: map[string]string{path: digest}, Routes: []PrivateRoute{route}}}
			task := Task{
				Objective:    "Audit " + keyword + " handling in approved file",
				Acceptance:   []Check{{ID: "analysis", Description: "Give one fact"}},
				AllowedPaths: []string{path},
				Mode:         "read_only",
				DataClass:    PrivateSourceData,
			}
			repo.PrivateSource.Briefs = []string{PrivateBriefDigest(task.Objective, task.Acceptance)}
			fault := task.Validate(repo)
			if fault == nil || fault.Gate != "PRIVATE_MODE_UNSUPPORTED" {
				t.Fatalf("expected PRIVATE_MODE_UNSUPPORTED for %s keyword in private read_only task, got %+v", keyword, fault)
			}
		})
	}
}

func TestPrivateRouteAccountMatchesCanonicalAndCaseInsensitive(t *testing.T) {
	const email = "Owner@Example.com"
	const lowerEmail = "owner@example.com"

	t.Run("trim and case-insensitive raw account match", func(t *testing.T) {
		r := PrivateRoute{Provider: "agy", ExpectedAccount: " " + email + " "}
		c := Capability{Provider: "agy", AttestedAccount: " " + lowerEmail + " "}
		if ok, gate := r.AccountMatches(c); !ok || gate != "" {
			t.Fatalf("expected case-insensitive trimmed match, got (%t, %q)", ok, gate)
		}
	})

	t.Run("canonical lowercase SHA256 matches mixed-case raw AttestedAccount", func(t *testing.T) {
		r := PrivateRoute{Provider: "agy", ExpectedAccountSHA256: Hash([]byte(lowerEmail))}
		c := Capability{Provider: "agy", AttestedAccount: email}
		if ok, gate := r.AccountMatches(c); !ok || gate != "" {
			t.Fatalf("expected canonical lowercase SHA256 match, got (%t, %q)", ok, gate)
		}
	})

	t.Run("backwards-compatible exact raw SHA256 matches mixed-case AttestedAccount", func(t *testing.T) {
		r := PrivateRoute{Provider: "agy", ExpectedAccountSHA256: Hash([]byte(email))}
		c := Capability{Provider: "agy", AttestedAccount: email}
		if ok, gate := r.AccountMatches(c); !ok || gate != "" {
			t.Fatalf("expected backwards-compatible exact SHA256 match, got (%t, %q)", ok, gate)
		}
	})

	t.Run("uppercase hex SHA256 normalized", func(t *testing.T) {
		r := PrivateRoute{Provider: "agy", ExpectedAccountSHA256: strings.ToUpper(Hash([]byte(lowerEmail)))}
		c := Capability{Provider: "agy", AttestedAccountSHA256: Hash([]byte(lowerEmail))}
		if ok, gate := r.AccountMatches(c); !ok || gate != "" {
			t.Fatalf("expected case-insensitive hex SHA256 match, got (%t, %q)", ok, gate)
		}
	})

	t.Run("mismatched account still denied", func(t *testing.T) {
		r := PrivateRoute{Provider: "agy", ExpectedAccount: email}
		c := Capability{Provider: "agy", AttestedAccount: "other@example.com"}
		if ok, gate := r.AccountMatches(c); ok || gate != "ACCOUNT_MISMATCH" {
			t.Fatalf("expected ACCOUNT_MISMATCH, got (%t, %q)", ok, gate)
		}
	})
}

func TestOpenCodeAccountFallbackChecksAttestation(t *testing.T) {
	c := Capability{Provider: "opencode", OpenCodeAccount: OpenCodeAnonymous, AttestedAccountSHA256: Hash([]byte("different"))}
	for _, grant := range []PrivateRoute{{}, {ExpectedAccount: OpenCodeAnonymous}, {ExpectedAccountSHA256: c.AttestedAccountSHA256}} {
		if ok, gate := grant.AccountMatches(c); ok || gate != "ACCOUNT_MISMATCH" {
			t.Error("conflicting fallback digest accepted", grant)
		}
	}
	c.AttestedAccountSHA256 = Hash([]byte(OpenCodeAnonymous))
	if ok, _ := (PrivateRoute{}).AccountMatches(c); !ok {
		t.Fatal("valid anonymous fallback denied")
	}
}
