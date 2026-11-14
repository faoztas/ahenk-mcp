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

// quotaRefreshTTL bounds the age of the in-memory quota snapshot. CodexBar
// CLI reads take seconds, so callers always read the cached snapshot and a
// single background goroutine refreshes it once it ages past the TTL.
const quotaRefreshTTL = 60 * time.Second

func (e *Engine) refreshQuotas() {
	ticker := time.NewTicker(quotaRefreshTTL)
	defer ticker.Stop()
	if e.readQuotas != nil {
		e.readQuotas()
	} else {
		e.readQuotasOnce()
	}
	for {
		select {
		case <-e.ctx.Done():
			return
		case <-ticker.C:
			e.readQuotasOnce()
		}
	}
}

// readQuotasOnce refreshes every configured CodexBar provider synchronously
// and stamps the cache. Providers without an account binding are not read.
func (e *Engine) readQuotasOnce() {
	if e.ctx.Err() != nil {
		return
	}
	var agy model.ProviderConfig
	for _, provider := range e.cfg.Providers {
		if provider.ID == "agy" {
			agy = provider
			break
		}
	}
	for _, provider := range []string{"codex", "agy", "claude", "copilot"} {
		if e.ctx.Err() != nil {
			return
		}
		if provider != "codex" && provider != "agy" && e.cfg.CodexBarAccountSHA256[provider] == "" {
			continue
		}
		ctx, cancel := context.WithTimeout(e.ctx, 15*time.Second)
		rows := providers.ReadCodexBar(ctx, provider, agy, e.cfg.CodexBarAccountSHA256[provider], e.cfg.CodexBarBinarySHA256)
		cancel()
		e.mu.Lock()
		e.rememberQuota(provider, rows, time.Now().UTC())
		e.mu.Unlock()
	}
	e.mu.Lock()
	e.quotaRefreshedAt = time.Now().UTC()
	e.mu.Unlock()
}

// refreshQuotasAsync triggers at most one background refresh when the cached
// quota snapshot is older than the TTL. It never blocks the caller.
func (e *Engine) refreshQuotasAsync() {
	if e.readQuotas == nil || e.quotaRefreshing.Load() {
		return
	}
	if e.ctx != nil && e.ctx.Err() != nil {
		return
	}
	e.mu.Lock()
	stale := time.Since(e.quotaRefreshedAt) >= quotaRefreshTTL
	e.mu.Unlock()
	if !stale || !e.quotaRefreshing.CompareAndSwap(false, true) {
		return
	}
	go func() {
		defer e.quotaRefreshing.Store(false)
		e.readQuotas()
	}()
}

func (e *Engine) Quota(provider string) []providers.Quota {
	e.refreshQuotasAsync()
	e.mu.Lock()
	defer e.mu.Unlock()
	now := time.Now().UTC()
	out := make([]providers.Quota, 0, 8)
	for _, id := range []string{"codex", "agy", "opencode", "copilot", "claude"} {
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

func (e *Engine) HealthPercent(bar string) float64 {
	e.mu.Lock()
	defer e.mu.Unlock()
	if bar == providers.BarAgyOpus || bar == providers.BarAgyOpus5h {
		minHealth := 1.0
		found := false
		for _, rows := range e.quotas {
			for _, q := range rows {
				if (q.Bucket == providers.BarAgyOpus || q.Bucket == providers.BarAgyOpus5h) && q.Remaining != nil {
					pct := *q.Remaining / 100.0
					if pct < 0 {
						pct = 0.0
					} else if pct > 1.0 {
						pct = 1.0
					}
					if !found || pct < minHealth {
						minHealth = pct
						found = true
					}
				}
			}
		}
		if found {
			return minHealth
		}
		return 1.0
	}

	for _, rows := range e.quotas {
		for _, q := range rows {
			if q.Bucket == bar && q.Remaining != nil {
				pct := *q.Remaining / 100.0
				if pct < 0 {
					return 0.0
				}
				if pct > 1.0 {
					return 1.0
				}
				return pct
			}
		}
	}
	if bar == providers.BarOpenCodeFree {
		now := time.Now().UTC()
		rows := e.openCodeQuota(now)
		for _, q := range rows {
			if q.Bucket == bar && q.Remaining != nil {
				pct := *q.Remaining / 100.0
				if pct < 0 {
					return 0.0
				}
				if pct > 1.0 {
					return 1.0
				}
				return pct
			}
		}
		hasGate := false
		for _, gate := range e.openCodeHealth {
			if e.openCodeGateCurrent(gate, now) {
				hasGate = true
				break
			}
		}
		if hasGate {
			available := 0
			total := 0
			for _, c := range e.reg.Candidates {
				if c.Provider == "opencode" && c.Mode != "fixture" {
					total++
					routeID := openCodeRoute(c.Model, c.OpenCodeAccount, c.OpenCodeTransport, c.OpenCodeEndpoint)
					_, g := e.openCodeRouteGate(routeID, now)
					if g.until.IsZero() {
						available++
					}
				}
			}
			if total > 0 {
				return float64(available) / float64(total)
			}
			return 0.0
		}
	}
	return 1.0 // default unobserved to 1.0
}

func (e *Engine) BarState(bar string) string {
	health := e.HealthPercent(bar)
	if health <= 0.0 {
		return "EXHAUSTED"
	}
	if health < 0.30 {
		return "PRESSURE_HIGH"
	}
	if health < 0.70 {
		return "PRESSURE_MEDIUM"
	}
	return "HEALTHY"
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
		q.Bucket = providers.BarOpenCodeFree
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
		q.Bucket = providers.BarOpenCodeFree
		q.WindowMinutes = 996
		q.OpenCodeAccount, q.OpenCodeProvider = key.account, key.upstream
		q.Reason, q.HealthScope, q.ResetAt = gate.code, key.scope, &gate.until
		rem := 0.0
		q.Remaining = &rem
		q.State = "EXHAUSTED"
		q.ObservedAt = now
		rows = append(rows, q)
	}
	slices.SortFunc(rows, func(a, b providers.Quota) int {
		return cmp.Or(cmp.Compare(a.Model, b.Model), cmp.Compare(a.OpenCodeProvider, b.OpenCodeProvider),
			cmp.Compare(a.OpenCodeAccount, b.OpenCodeAccount), cmp.Compare(a.OpenCodeTransport, b.OpenCodeTransport),
			cmp.Compare(a.HealthScope, b.HealthScope))
	})
	return rows
}

// ActiveCircuitBreakers lists every currently open provider cooldown and
// OpenCode health gate for the routing summary.
func (e *Engine) ActiveCircuitBreakers() []map[string]any {
	e.mu.Lock()
	defer e.mu.Unlock()
	now := time.Now().UTC()
	var out []map[string]any
	add := func(provider, modelID, scope, code string, until time.Time) {
		out = append(out, map[string]any{"provider": provider, "model": modelID, "scope": scope, "code": code, "until": until})
	}
	for _, p := range []string{"agy", "codex", "claude", "copilot"} {
		if until := e.cooldown[p]; until.After(now) {
			add(p, "", "provider", "COOLDOWN", until)
		}
	}
	for key, gate := range e.openCodeHealth {
		if !e.openCodeGateCurrent(gate, now) {
			continue
		}
		switch key.scope {
		case "model":
			add("opencode", key.openCodeIdentity.model, "model", gate.code, gate.until)
		case "account", "pool", "provider":
			add("opencode", "", key.scope, gate.code, gate.until)
		}
	}
	slices.SortFunc(out, func(a, b map[string]any) int {
		return cmp.Or(
			cmp.Compare(a["provider"].(string), b["provider"].(string)),
			cmp.Compare(a["model"].(string), b["model"].(string)),
			cmp.Compare(a["scope"].(string), b["scope"].(string)))
	})
	return out
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
