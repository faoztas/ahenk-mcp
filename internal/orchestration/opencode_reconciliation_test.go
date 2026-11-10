// Copyright 2026 Furkan Anıl Öztaş
// SPDX-License-Identifier: Apache-2.0

package orchestration

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/faoztas/ahenk-mcp/internal/model"
	"github.com/faoztas/ahenk-mcp/internal/store"
)

const (
	glModel = "opencode/glm-5.3-flash-free"
	glPin   = "bfd1f79c2fb005246c56d0e9fb472206c22782d065872d71c26f01f3eb3a0a82"
)

func glmRoute() model.Capability {
	return model.Capability{Provider: "opencode", Model: glModel, Fingerprint: glPin, Status: "available", Mode: "tool_free",
		OpenCodeAccount: model.OpenCodeAnonymous, OpenCodeTransport: model.OpenCodeNativeReadAsk, OpenCodeEndpoint: model.OpenCodeFreeEndpoint,
		Cost:    &model.CostEvidence{Policy: "FREE_ONLY", Status: "CATALOG_PREFLIGHT_REQUIRED"},
		Quality: []model.QualityEvidence{{Role: "inference", Tier: 1}}}
}
func glmFailure(at time.Time, scope string) model.Attempt {
	return model.Attempt{Number: 1, Provider: "opencode", Model: glModel, Fingerprint: glPin, Code: "RATE_LIMIT",
		OpenCodeAccount: model.OpenCodeAnonymous, OpenCodeTransport: model.OpenCodeNativeReadAsk,
		FinishedAt: &at, RetryAfterMS: 3_600_000, RetrySource: "provider_body", UpstreamStatus: 429, ProviderRateLimitScope: scope}
}
func glmSuccess(at time.Time) model.Attempt {
	zero := 0.0
	start := at.Add(-time.Second)
	return model.Attempt{Number: 1, Provider: "opencode", Model: glModel, Fingerprint: glPin,
		OpenCodeAccount: model.OpenCodeAnonymous, OpenCodeTransport: model.OpenCodeNativeReadAsk, StartedAt: start, FinishedAt: &at,
		OpenCode: &model.OpenCodeRunEvidence{GuardVerified: true, TerminalVerified: true, PromptSent: true, Model: glModel, ReportedCost: &zero}}
}

func TestOpenCodeReconciliationOC01OC02OC03OC10(t *testing.T) {
	now := capacityClock
	c := glmRoute()
	for _, tc := range []struct {
		name, account, endpoint, transport, fingerprint string
		clear                                           bool
	}{
		{name: "same verified route", clear: true},
		{name: "different account", account: "DIFFERENT"},
		{name: "different endpoint", endpoint: "https://example.invalid/v1"},
		{name: "desktop transport", transport: "desktop"},
		{name: "different binary", fingerprint: "different-pin"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := Engine{}
			old := glmFailure(now, "")
			next := glmSuccess(now.Add(time.Minute))
			if tc.account != "" {
				next.OpenCodeAccount = tc.account
			}
			if tc.endpoint != "" {
				next.OpenCodeEndpoint = tc.endpoint
			}
			if tc.transport != "" {
				next.OpenCodeTransport = tc.transport
			}
			if tc.fingerprint != "" {
				next.Fingerprint = tc.fingerprint
			}
			// Success can be read before an imported historical failure.
			e.rememberOpenCodeHealth(&next)
			e.rememberOpenCodeHealth(&old)
			if scope, _ := e.openCodeGate(c, now.Add(2*time.Minute)); (scope == "") != tc.clear {
				t.Fatalf("wrong route recovery: %q", scope)
			}
			if tc.clear {
				newer := glmFailure(now.Add(3*time.Minute), "")
				newer.RetryAfterMS = 60_000
				e.rememberOpenCodeHealth(&newer)
				e.rememberOpenCodeHealth(&old)
				e.rememberOpenCodeHealth(&next)
				if scope, gate := e.openCodeGate(c, now.Add(3*time.Minute)); scope != "model" || !gate.until.Equal(now.Add(4*time.Minute)) {
					t.Fatalf("old event replaced new rate limit: %+v", gate)
				}
			}
		})
	}
}

func TestOpenCodeRecheckOC04OC05OC08OC09OC12OC13(t *testing.T) {
	now := capacityClock
	e, j := capacityFixture(now)
	c := glmRoute()
	e.reg.Candidates[1] = c
	e.quotas["agy"][1] = quotaFixture("agy", "gemini-weekly", "gemini", weeklyMinutes, 15, now)
	old := glmFailure(now, "model")
	e.rememberOpenCodeHealth(&old)
	if ok, _ := e.openCodeRecheck(c, now.Add(time.Minute)); ok {
		t.Fatal("authoritative scope allowed a probe")
	}
	if ok, _ := e.openCodeRecheck(c, now.Add(time.Hour)); !ok {
		t.Fatal("expired cooldown did not permit revalidation")
	}
	if picked, _ := e.route(&j, now); picked.Provider != "" {
		t.Fatal("active gate was bypassed")
	}
	requireWarning(t, e.capacityWarnings(j.ID, j.Route, model.Blocked, now), "OPENCODE_FREE_UNAVAILABLE")
	other := c
	other.Model = "opencode/independent"
	if scope, _ := e.openCodeGate(other, now); scope != "" {
		t.Fatal("model gate crossed to independent model")
	}

	e = &Engine{jobs: map[string]model.Job{}, active: map[string]context.CancelFunc{}}
	old = glmFailure(now, "")
	e.rememberOpenCodeHealth(&old)
	if ok, _ := e.openCodeRecheck(c, now.Add(time.Minute)); !ok {
		t.Fatal("verified unscoped historical retry not recheckable")
	}
	attempt := glmSuccess(now.Add(time.Minute))
	attempt.FinishedAt = nil
	attempt.OpenCodeRecheck = true
	e.jobs["probe"] = model.Job{Attempts: []model.Attempt{attempt}}
	e.active["probe"] = func() {}
	if ok, _ := e.openCodeRecheck(c, now.Add(time.Minute)); ok {
		t.Fatal("concurrent probe admitted")
	}
	delete(e.active, "probe")
	if ok, _ := e.openCodeRecheck(c, now.Add(10*time.Minute)); ok {
		t.Fatal("second probe admitted during the same cooldown")
	}
}

func TestOpenCodeRecoveryPersistsWithoutSchemaChangeOC11OC12(t *testing.T) {
	now := capacityClock
	db, err := store.Open(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	e, j := capacityFixture(now)
	e.reg.Candidates[1] = glmRoute()
	e.quotas["agy"][1] = quotaFixture("agy", "gemini-weekly", "gemini", weeklyMinutes, 15, now)
	old := glmFailure(now, "")
	success := glmSuccess(now.Add(time.Minute))
	if _, err := db.Insert([]model.Job{{ID: "old", RepoID: "demo", Status: model.Blocked, Attempts: []model.Attempt{old}},
		{ID: "new", RepoID: "demo", Status: model.Completed, Attempts: []model.Attempt{success}}}, ""); err != nil {
		t.Fatal(err)
	}
	restored, err := New(e.cfg, db, e.reg)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	restored.mu.Lock()
	defer restored.mu.Unlock()
	restored.quotas = e.quotas
	if c, _ := restored.route(&j, now.Add(2*time.Minute)); c.Model != glModel {
		t.Fatalf("same-route recovery failed: %+v", j.Route)
	}
	if state := restored.openCodeRecovery(glmRoute(), now.Add(2*time.Minute)); state != "RECOVERED" {
		t.Fatal(state)
	}
	for _, w := range restored.capacityWarnings(j.ID, j.Route, model.Queued, now.Add(2*time.Minute)) {
		if w.Code == "CODEX_ONLY_WARNING" || w.Code == "OPENCODE_FREE_UNAVAILABLE" {
			t.Fatalf("stale warning: %+v", w)
		}
	}
}

func TestOpenCodeTransientIsNotQuotaOC15(t *testing.T) {
	now := capacityClock
	a := glmFailure(now, "")
	a.Code = "TEMPORARY_PROVIDER_ERROR"
	e := Engine{}
	e.rememberOpenCodeHealth(&a)
	if a.RetryAt == nil || a.RetryAt.After(now.Add(5*time.Minute)) {
		t.Fatal("temporary failure fabricated a long quota deadline")
	}
}
