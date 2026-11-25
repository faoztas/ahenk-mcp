// Copyright 2026 Furkan Anıl Öztaş
// SPDX-License-Identifier: Apache-2.0

package orchestration

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/faoztas/ahenk-mcp/internal/model"
	"github.com/faoztas/ahenk-mcp/internal/providers"
)

func TestAdmissionUnattestedWorker(t *testing.T) {
	now := capacityClock
	e, j := capacityFixture(now)
	e.cfg.Repositories["demo"] = model.Repo{Providers: []string{"codex"}}
	e.reg.Candidates = []model.Capability{{Provider: "codex", Model: "gpt-6.1-sol", Status: "available", Speed: "fast"}}
	e.quotas["codex"] = []providers.Quota{quotaFixture("codex", providers.BarCodexWeekly, "", weeklyMinutes, 0, now)}
	for _, binding := range []string{"", model.Hash([]byte("fixture-account"))} {
		e.cfg.CodexBarAccountSHA256 = map[string]string{"codex": binding}
		got, _ := e.route(&j, now)
		d := j.Route[0]
		if got.Provider != "codex" || d.QuotaRemaining != nil || d.QuotaResetAt != nil || d.RetryAt != nil || d.ControllerRemaining == nil || *d.ControllerRemaining != 0 {
			t.Errorf("unattested route %+v", d)
		}
	}
}

func TestAdmissionDispatchResolvedModel(t *testing.T) {
	for _, tc := range []struct{ provider, model, category, reason string }{
		{"agy", "claude-opus-5-5-medium", "coding", "OPUS_SCARCITY_GATE_TASK_DENIED"},
		{"agy", "claude-opus-5-5-medium", "architecture", "OPUS_RESERVE_DEPLETED"},
		{"codex", "gpt-6-luna", "debugging", "LUNA_CODING_DENIED"},
	} {
		t.Run(tc.model+tc.category, func(t *testing.T) {
			now := capacityClock
			e, j := capacityFixture(now)
			e.reg.Candidates = e.reg.Candidates[:1]
			c := &e.reg.Candidates[0]
			c.Provider, c.Model = tc.provider, tc.model
			c.AttestedAccount = "fixture-account"
			e.cfg.CodexBarAccountSHA256 = map[string]string{tc.provider: model.Hash([]byte(c.AttestedAccount))}
			e.cfg.Repositories["demo"] = model.Repo{Providers: []string{tc.provider}}
			e.quotas["agy"] = []providers.Quota{quotaFixture("agy", providers.BarAgyOpus5h, "claude_gpt", 300, 19, now)}
			j.Task.Category = tc.category
			j.Task.PreferredProvider, j.Task.PreferredModel = tc.provider, tc.model
			for _, prior := range [][]model.Attempt{nil, {{Provider: "prior"}}} {
				j.Attempts = prior
				got, _ := e.route(&j, now)
				if got.Provider != "" || j.Route[0].Reason != tc.reason {
					t.Fatalf("dispatch %+v", j.Route)
				}
			}
		})
	}
}

func TestAdmissionOpusDiagnosticWindows(t *testing.T) {
	now := capacityClock
	e, _ := capacityFixture(now)
	e.quotas["agy"] = []providers.Quota{quotaFixture("agy", providers.BarAgyOpus5h, "claude_gpt", 300, 19, now), quotaFixture("agy", providers.BarAgyOpus, "claude_gpt", 7200, 83, now)}
	want := map[string]float64{providers.BarAgyOpus5h: 19, providers.BarAgyOpus: 83}
	for _, b := range e.QuotaBars(now) {
		if n, ok := want[b.Bucket]; ok {
			if b.Remaining == nil || *b.Remaining != n {
				t.Errorf("wrong bar %+v", b)
			}
			delete(want, b.Bucket)
		}
	}
	if len(want) > 0 {
		t.Errorf("missing %v", want)
	}
	q := tightestQuota(e.quotas["agy"], "claude_gpt", 0, now.Add(16*time.Minute))
	if q.State != "STALE" || q.Remaining != nil || q.ResetAt != nil || q.Family != "claude_gpt" {
		t.Errorf("stale family %+v", q)
	}
}

func TestAdmissionLateProbeCompletion(t *testing.T) {
	for _, outcome := range []string{"release", "success", "rate_limit", "ordinary_success", "recovery"} {
		cb := NewCircuitBreaker()
		at := capacityClock
		cb.cooldown["agy"] = at
		first, okA := cb.acquireAt("agy", at)
		second, okB := cb.acquireAt("antigravity", at.Add(defaultProbeTimeout))
		if !okA || !okB || first == second {
			t.Fatal("probe acquisitions failed")
		}
		switch outcome {
		case "release":
			cb.Release("agy", first)
		case "success":
			cb.probeSucceeded("agy", first)
		case "rate_limit":
			cb.tripUntil("agy", time.Now().Add(time.Hour), first, true)
		case "ordinary_success":
			cb.probeSucceeded("agy", 0)
		case "recovery":
			cb.Success("antigravity")
		}
		if got := cb.stateAt("agy", at.Add(defaultProbeTimeout)); got != CircuitOpen {
			t.Errorf("late completion cleared replacement: %v", got)
		}
		cb.probeSucceeded("agy", second)
		if got := cb.stateAt("agy", at.Add(defaultProbeTimeout)); got != CircuitClosed {
			t.Errorf("owned success: %v", got)
		}
	}
}

func TestAdmissionLateRateLimitCannotBlockReplacement(t *testing.T) {
	at := time.Now()
	e, j := capacityFixture(at)
	e.cfg.Repositories["demo"] = model.Repo{Providers: []string{"agy"}}
	e.cooldown = map[string]time.Time{}
	e.cb = NewCircuitBreaker()
	e.cb.cooldown["agy"] = at
	first, okA := e.cb.acquireAt("agy", at)
	replacedAt := at.Add(defaultProbeTimeout)
	second, okB := e.cb.acquireAt("agy", replacedAt)
	if !okA || !okB || first == second {
		t.Fatal("probe acquisitions failed")
	}
	late := model.Attempt{Provider: "agy", ProbeID: first, Code: "RATE_LIMIT", FinishedAt: &replacedAt}
	e.rememberRateLimit(&late)
	e.cb.probeSucceeded("agy", second)
	if got, _ := e.route(&j, replacedAt); got.Provider != "agy" {
		t.Fatalf("superseded probe blocked routing after replacement success: %+v", j.Route)
	}
	saved, err := json.Marshal(late)
	if err != nil {
		t.Fatal(err)
	}
	var recovered model.Attempt
	if err := json.Unmarshal(saved, &recovered); err != nil {
		t.Fatal(err)
	}
	e.cb = NewCircuitBreaker()
	e.rememberRateLimit(&recovered)
	if !recovered.RateLimitSuppressed || !e.cooldown["agy"].IsZero() {
		t.Fatalf("superseded cooldown replayed after restart: %+v", e.cooldown)
	}
}

func (e *Engine) route(j *model.Job, now time.Time) (model.Capability, time.Duration) {
	c, budget, _ := e.routeWithMode(j, now, false)
	return c, budget
}
