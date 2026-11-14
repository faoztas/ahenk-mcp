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
