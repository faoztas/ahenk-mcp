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
	e.refreshQuotasAsync()
	for {
		select {
		case <-e.ctx.Done():
			return
		case <-ticker.C:
			e.refreshQuotasAsync()
		}
	}
}

// readQuotasOnce refreshes every configured CodexBar provider synchronously
// and stamps the cache. Providers without an account binding are not read.
func (e *Engine) readQuotasOnce() {
	if !e.cfg.EnableCodexBar || e.ctx.Err() != nil {
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

// refreshQuotasAsync refreshes stale quota snapshots in the background for
// engines with CodexBar enabled.
func (e *Engine) refreshQuotasAsync() {
	if !e.cfg.EnableCodexBar || e.readQuotas == nil || e.quotaRefreshing.Load() {
		return
	}
	if e.ctx != nil && e.ctx.Err() != nil {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed || time.Since(e.quotaRefreshedAt) < quotaRefreshTTL || !e.quotaRefreshing.CompareAndSwap(false, true) {
		return
	}
	e.wg.Go(func() {
		defer e.quotaRefreshing.Store(false)
		e.readQuotas()
	})
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
			q = q.At(now)
			if id != "opencode" && (q.Provider != "" && q.Provider != id || !verifiedQuota(q)) {
				q.Remaining, q.ResetAt = nil, nil
				if q.AccountMatch != "MATCHED" || q.State != "STALE" {
					q.State = "UNKNOWN"
				}
			}
			out = append(out, q)
		}
	}
	return out
}

func (e *Engine) HealthPercent(bar string) float64 {
	if e == nil {
		return 1.0
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	now := time.Now().UTC()
	return e.healthPercentLocked(bar, now)
}

// scopedQuota selects verified evidence within one provider and window scope.
func scopedQuota(rows []providers.Quota, provider string, now time.Time, matches func(providers.Quota) bool) providers.Quota {
	result := providers.UnknownQuota(provider)
	for _, raw := range rows {
		if raw.Provider != "" && raw.Provider != provider || raw.AccountMatch != "MATCHED" || !matches(raw) {
			continue
		}
		q := raw.At(now)
		if verifiedQuota(q) {
			if result.Remaining == nil || *q.Remaining < *result.Remaining ||
				*q.Remaining == 0 && *result.Remaining == 0 && q.ResetAt != nil && result.ResetAt != nil && q.ResetAt.After(*result.ResetAt) {
				result = q
			}
		} else if q.State == "STALE" && result.Remaining == nil {
			result = q
			result.Remaining, result.ResetAt = nil, nil
		}
	}
	return result
}

// The parser binds MATCHED rows to the configured observer account. If a
// worker attests a different identity, its remaining quota is unknown.
func (e *Engine) quotaAccountMatches(c model.Capability) bool {
	binding := e.cfg.CodexBarAccountSHA256[normalizeProvider(c.Provider)]
	if binding == "" || (c.AttestedAccount == "" && c.AttestedAccountSHA256 == "") {
		return false
	}
	ok, _ := (model.PrivateRoute{ExpectedAccountSHA256: binding}).AccountMatches(c)
	return ok
}

func workerQuota(rows []providers.Quota, provider string, now time.Time) providers.Quota {
	return scopedQuota(rows, provider, now, func(q providers.Quota) bool {
		switch provider {
		case "codex":
			return q.Bucket == providers.BarCodexBurst || q.Bucket == providers.BarCodexWeekly
		case "claude":
			return q.Bucket == "primary" || q.Bucket == "secondary" || q.Bucket == "tertiary"
		}
		return false
	})
}

func observerBucket(bucket string) string {
	switch bucket {
	case "gemini-5h":
		return providers.BarAgyGemini
	case "3p-5h":
		return providers.BarAgyOpus5h
	case "3p-weekly":
		return providers.BarAgyOpus
	default:
		return bucket
	}
}

func (e *Engine) barQuotaLocked(bar string, now time.Time) providers.Quota {
	provider := "agy"
	switch bar {
	case providers.BarCodexBurst, providers.BarCodexWeekly:
		provider = "codex"
	case providers.BarOpenCodeFree:
		provider = "opencode"
	}
	return scopedQuota(e.quotas[provider], provider, now, func(q providers.Quota) bool {
		return observerBucket(q.Bucket) == observerBucket(bar)
	})
}

func (e *Engine) opusAdmissionHealth(now time.Time) float64 {
	return min(e.healthPercentLocked(providers.BarAgyOpus5h, now), e.healthPercentLocked(providers.BarAgyOpus, now))
}

func (e *Engine) hasFreshQuotaLocked(bar string, now time.Time) bool {
	return verifiedQuota(e.barQuotaLocked(bar, now))
}

func (e *Engine) healthPercentLocked(bar string, now time.Time) float64 {
	if q := e.barQuotaLocked(bar, now); verifiedQuota(q) {
		return *q.Remaining / 100
	}

	if bar == providers.BarOpenCodeFree {
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
	return healthState(e.HealthPercent(bar))
}

func healthState(health float64) string {
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

// ActiveCircuitBreakers lists every currently open provider cooldown,
// circuit breaker, and OpenCode health gate for diagnostics and routing summary.
func (e *Engine) ActiveCircuitBreakers() []map[string]any {
	if e == nil {
		return []map[string]any{}
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	now := time.Now().UTC()
	out := make([]map[string]any, 0)
	type breakerKey struct {
		provider string
		model    string
		scope    string
	}
	seen := make(map[breakerKey]int)
	add := func(provider, modelID, scope, code string, until time.Time) {
		norm := normalizeProvider(provider)
		k := breakerKey{provider: norm, model: modelID, scope: scope}
		entry := map[string]any{
			"provider": norm,
			"model":    modelID,
			"scope":    scope,
			"code":     code,
			"state":    "OPEN",
			"until":    until.UTC().Format(time.RFC3339),
		}
		if idx, exists := seen[k]; exists {
			if prev, ok := out[idx]["until"].(string); ok {
				if prevTime, err := time.Parse(time.RFC3339, prev); err == nil && until.After(prevTime) {
					out[idx] = entry
				}
			}
			return
		}
		seen[k] = len(out)
		out = append(out, entry)
	}
	for _, p := range []string{"agy", "codex", "claude", "copilot"} {
		if until := e.cooldown[p]; until.After(now) {
			add(p, "", "provider", "COOLDOWN", until)
		}
	}
	if e.cb != nil {
		for p, until := range e.cb.ActiveTripped() {
			if until.After(now) {
				add(p, "", "provider", "CIRCUIT_OPEN", until)
			}
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
	q := scopedQuota(rows, "agy", now, func(q providers.Quota) bool {
		return q.Family == family && (minutes == 0 || q.WindowMinutes == minutes)
	})
	if q.State == "UNKNOWN" {
		return providers.Quota{}
	}
	return q
}

type QuotaBar struct {
	Bucket    string       `json:"bucket"`
	Provider  string       `json:"provider"`
	Remaining *float64     `json:"remaining_percent"`
	Health    *float64     `json:"health_pct"`
	State     string       `json:"state"`
	Circuit   CircuitState `json:"circuit_state"`
}

// QuotaBars reports observed windows at one instant. An unobserved bar has
// no numeric balance even when routing policy still permits an attempt.
func (e *Engine) QuotaBars(now time.Time) []QuotaBar {
	bars := []QuotaBar{
		{Bucket: providers.BarCodexBurst, Provider: "codex"},
		{Bucket: providers.BarCodexWeekly, Provider: "codex"},
		{Bucket: providers.BarAgyGemini, Provider: "antigravity"},
		{Bucket: providers.BarAgyOpus5h, Provider: "antigravity"},
		{Bucket: providers.BarAgyOpus, Provider: "antigravity"},
		{Bucket: providers.BarAgyArgon, Provider: "antigravity"},
		{Bucket: providers.BarOpenCodeFree, Provider: "opencode"},
	}
	if e != nil {
		e.mu.Lock()
		defer e.mu.Unlock()
	}
	for i := range bars {
		b := &bars[i]
		if e == nil {
			b.State, b.Circuit = "UNKNOWN", "UNKNOWN"
			continue
		}
		q := e.barQuotaLocked(b.Bucket, now)
		b.State, b.Circuit = q.State, e.cb.stateAt(b.Provider, now)
		if verifiedQuota(q) {
			h := *q.Remaining / 100
			b.Remaining, b.Health, b.State = q.Remaining, &h, healthState(h)
		}
	}
	return bars
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
