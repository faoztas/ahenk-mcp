// Copyright 2026 Furkan Anıl Öztaş
// SPDX-License-Identifier: Apache-2.0

package orchestration

import (
	"cmp"
	"context"
	"slices"
	"time"

	"github.com/faoztas/ahenk-mcp/internal/model"
	"github.com/faoztas/ahenk-mcp/internal/providers"
)

func (e *Engine) refreshQuotas() {
	var agy model.ProviderConfig
	for _, provider := range e.cfg.Providers {
		if provider.ID == "agy" {
			agy = provider
			break
		}
	}
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()
	for {
		for _, provider := range []string{"codex", "agy"} {
			if e.ctx.Err() != nil {
				return
			}
			ctx, cancel := context.WithTimeout(e.ctx, 15*time.Second)
			rows := providers.ReadCodexBar(ctx, provider, agy, e.cfg.CodexBarAccountSHA256[provider])
			cancel()
			e.mu.Lock()
			e.rememberQuota(provider, rows, time.Now().UTC())
			e.mu.Unlock()
		}
		select {
		case <-e.ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (e *Engine) Quota(provider string) []providers.Quota {
	e.mu.Lock()
	defer e.mu.Unlock()
	now := time.Now().UTC()
	out := make([]providers.Quota, 0, 8)
	for _, id := range []string{"codex", "agy", "opencode", "copilot"} {
		if provider != "" && provider != id {
			continue
		}
		rows := e.quotas[id]
		if id == "opencode" {
			rows = e.openCodeQuota(now)
		}
		if len(rows) == 0 {
			rows = []providers.Quota{providers.UnknownQuota(id)}
		}
		for _, q := range rows {
			out = append(out, q.At(now))
		}
	}
	return out
}

func (e *Engine) openCodeQuota(now time.Time) []providers.Quota {
	var rows []providers.Quota
	seen := map[openCodeIdentity]bool{}
	reportedAsserted := map[openCodeHealthKey]bool{}
	addModel := func(id openCodeIdentity) {
		if id.model == "" || seen[id] {
			return
		}
		seen[id] = true
		q := providers.UnknownQuota("opencode")
		q.Model, q.OpenCodeAccount, q.OpenCodeProvider = id.model, id.account, id.upstream
		q.OpenCodeTransport = id.transport
		gate := e.openCodeHealth[openCodeKey("model", id)]
		assertedKey := openCodeObservedGateKey("model", "model", id)
		reportedAsserted[assertedKey] = true
		if asserted := e.openCodeHealth[assertedKey]; e.openCodeGateCurrent(asserted, now) && (!e.openCodeGateCurrent(gate, now) || asserted.until.After(gate.until)) {
			gate = asserted
		}
		if e.openCodeGateCurrent(gate, now) {
			q.Reason, q.HealthScope, q.ResetAt = gate.code, "model", &gate.until
		} else if e.openCodeInFlight(id) {
			q.Reason, q.HealthScope = "OPENCODE_MODEL_IN_FLIGHT", "model"
		}
		rows = append(rows, q)
	}
	for _, c := range e.reg.Candidates {
		if c.Provider == "opencode" && c.Mode != "fixture" {
			addModel(openCodeRoute(c.Model, c.OpenCodeAccount, c.OpenCodeTransport, c.OpenCodeEndpoint))
		}
	}
	for key, gate := range e.openCodeHealth {
		if !e.openCodeGateCurrent(gate, now) {
			continue
		}
		if key.scope == "model" {
			if !reportedAsserted[key] {
				addModel(key.openCodeIdentity)
			}
			continue
		}
		q := providers.UnknownQuota("opencode")
		q.OpenCodeAccount, q.OpenCodeProvider = key.account, key.upstream
		q.Reason, q.HealthScope, q.ResetAt = gate.code, key.scope, &gate.until
		rows = append(rows, q)
	}
	slices.SortFunc(rows, func(a, b providers.Quota) int {
		return cmp.Or(cmp.Compare(a.Model, b.Model), cmp.Compare(a.OpenCodeProvider, b.OpenCodeProvider),
			cmp.Compare(a.OpenCodeAccount, b.OpenCodeAccount), cmp.Compare(a.OpenCodeTransport, b.OpenCodeTransport),
			cmp.Compare(a.HealthScope, b.HealthScope))
	})
	return rows
}

func tightestQuota(rows []providers.Quota, family string, minutes int, now time.Time) providers.Quota {
	var result providers.Quota
	for _, q := range rows {
		q = q.At(now)
		if q.AccountMatch == "MATCHED" && q.Family == family && (minutes == 0 || q.WindowMinutes == minutes) &&
			q.Unit == "percent" && (q.State == "KNOWN" || q.State == "EXHAUSTED") && q.Remaining != nil &&
			(result.Remaining == nil || *q.Remaining < *result.Remaining) {
			result = q
		}
	}
	return result
}

// route holds the scheduler lock across selection and attempt reservation.
func (e *Engine) agyFamilyInFlight(family string) bool {
	if family == "" {
		return false
	}
	for id := range e.active {
		job := e.jobs[id]
		if len(job.Attempts) > 0 {
			a := job.Attempts[len(job.Attempts)-1]
			if a.FinishedAt == nil && providers.Family(a.Provider) == "agy" && providers.AGYModelFamily(a.Model) == family {
				return true
			}
		}
	}
	return false
}
