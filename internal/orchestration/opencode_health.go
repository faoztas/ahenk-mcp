// Copyright 2026 Furkan Anıl Öztaş
// SPDX-License-Identifier: Apache-2.0

package orchestration

import (
	"slices"
	"strings"
	"time"

	"github.com/faoztas/ahenk-mcp/internal/model"
	"github.com/faoztas/ahenk-mcp/internal/providers"
	"github.com/faoztas/ahenk-mcp/internal/store"
)

type openCodeIdentity struct {
	upstream  string
	account   string
	model     string
	transport string
	endpoint  string
}

func openCodeIdentityOf(modelID, account, transport string) openCodeIdentity {
	upstream, _, qualified := strings.Cut(modelID, "/")
	if !qualified {
		upstream = "opencode"
	}
	return openCodeIdentity{upstream: upstream, account: model.OpenCodeAccountID(account), model: modelID,
		transport: model.OpenCodeTransportID(transport), endpoint: model.OpenCodeFreeEndpoint}
}

func openCodeRoute(modelID, account, transport, endpoint string) openCodeIdentity {
	id := openCodeIdentityOf(modelID, account, transport)
	if endpoint != "" {
		id.endpoint = endpoint
	}
	return id
}

func OpenCodeGateApplies(scope, modelID, account, transport string, c model.Capability) bool {
	return c.Provider == "opencode" && openCodeKey(scope, openCodeIdentityOf(modelID, account, transport)) ==
		openCodeKey(scope, openCodeRoute(c.Model, c.OpenCodeAccount, c.OpenCodeTransport, c.OpenCodeEndpoint))
}

type openCodeHealthKey struct {
	openCodeIdentity
	scope string
}

func openCodeKey(scope string, identity openCodeIdentity) openCodeHealthKey {
	if scope == "account" || scope == "pool" || scope == "provider" {
		identity.model = ""
		identity.transport = ""
	}
	return openCodeHealthKey{openCodeIdentity: identity, scope: scope}
}

func openCodeObservedGateKey(scope, providerScope string, identity openCodeIdentity) openCodeHealthKey {
	if scope == "model" && providerScope == "model" {
		identity.transport = ""
	}
	return openCodeKey(scope, identity)
}

type openCodeHealth struct {
	resetAt       *time.Time
	origin        openCodeIdentity
	assertedScope string
	probe         bool
	suspect       bool
	source        string
	observed      time.Time
	until         time.Time
	code          string
}

func OpenCodeRateLimitUntil(finishedAt time.Time, retryAfterMS int64) time.Time {
	return finishedAt.Add(max(time.Minute, providerRetryDelay(retryAfterMS)))
}

func OpenCodeImportRetryAt(record store.OpenCodeCooldownImport) time.Time {
	if !record.RetryAt.IsZero() {
		return minTime(record.RetryAt, record.FinishedAt.Add(7*24*time.Hour))
	}
	return OpenCodeRateLimitUntil(record.FinishedAt, record.RetryAfterMS)
}

func providerRetryDelay(millis int64) time.Duration {
	return time.Duration(max(0, min(millis, int64((7*24*time.Hour)/time.Millisecond)))) * time.Millisecond
}

func openCodeFaultDelay(code string) (delay time.Duration, transient bool) {
	switch code {
	case "RATE_LIMIT":
		return time.Minute, false
	case "ACCESS_DENIED", "AUTH_REQUIRED", "ZERO_COST_UNAVAILABLE":
		return 15 * time.Minute, false
	case "MODEL_UNAVAILABLE", "ZERO_COST_UNVERIFIED":
		return 5 * time.Minute, false
	case "ATTEMPT_TIMEOUT", "PROVIDER_STARTUP_TIMEOUT", "PROVIDER_STARTUP_FAILED",
		"TEMPORARY_PROVIDER_ERROR", "OPENCODE_UPSTREAM_UNAVAILABLE":
		return 30 * time.Second, true
	default:
		return 0, false
	}
}

// OpenCodeHealthScope derives the current policy from provider evidence.
// The old locally assigned HealthScope does not establish a shared limit.
func OpenCodeHealthScope(a model.Attempt) string {
	delay, _ := openCodeFaultDelay(a.Code)
	if a.Provider != "opencode" || delay == 0 {
		return ""
	}
	switch a.Code {
	case "RATE_LIMIT", "ACCESS_DENIED", "AUTH_REQUIRED", "ZERO_COST_UNAVAILABLE":
		return OpenCodeRateLimitScope(a.ProviderRateLimitScope)
	default:
		return "model"
	}
}

func OpenCodeRateLimitScope(providerScope string) string {
	switch providerScope {
	case "pool", "account", "provider", "session":
		return providerScope
	default:
		return "model"
	}
}

// OpenCodeHealthHistory applies the same model-local transient policy during
// dispatch and when replaying finished attempts in chronological order.
type OpenCodeHealthHistory struct {
	transient map[openCodeIdentity][]time.Time
}

func (h *OpenCodeHealthHistory) Apply(a *model.Attempt) {
	if a.Provider != "opencode" || a.FinishedAt == nil {
		return
	}
	identity := openCodeRoute(a.Model, a.OpenCodeAccount, a.OpenCodeTransport, a.OpenCodeEndpoint)
	if a.Code == "" {
		delete(h.transient, identity)
		return
	}
	scope := OpenCodeHealthScope(*a)
	if scope == "" {
		return
	}
	a.CooldownScope = openCodeScope(a.ProviderRateLimitScope)
	if scope == "session" {
		a.HealthScope = "session"
		return
	}
	delay, transient := openCodeFaultDelay(a.Code)
	if transient {
		const window = 5 * time.Minute
		if h.transient == nil {
			h.transient = make(map[openCodeIdentity][]time.Time)
		}
		times := slices.DeleteFunc(h.transient[identity], func(t time.Time) bool {
			return a.FinishedAt.Sub(t) > window
		})
		times = append(times, *a.FinishedAt)
		// Only the last three failures can affect the current circuit decision.
		h.transient[identity] = times[max(0, len(times)-3):]
		if len(times) >= 3 {
			delay = window
		}
	}
	providerDelay := providerRetryDelay(a.RetryAfterMS)
	if transient {
		providerDelay = min(providerDelay, 5*time.Minute)
	}
	until := a.FinishedAt.Add(max(delay, providerDelay)).UTC()
	// Preserve recorded deadlines unless a legacy pooled transient must be
	// reconstructed from the exact model's own failures.
	if a.RetryAt != nil && (!transient || a.HealthScope == scope) {
		until = minTime(*a.RetryAt, a.FinishedAt.Add(7*24*time.Hour))
		if transient {
			until = minTime(until, a.FinishedAt.Add(5*time.Minute))
		}
	}
	a.HealthScope, a.RetryAt = scope, &until
}

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a.UTC()
	}
	return b.UTC()
}

func openCodeScope(scope string) string {
	switch scope {
	case "session":
		return "SESSION"
	case "model":
		return "MODEL"
	case "pool":
		return "SHARED_POOL"
	case "account":
		return "ACCOUNT"
	case "provider":
		return "PROVIDER"
	default:
		return "UNKNOWN"
	}
}

func (e *Engine) rememberOpenCodeHealth(a *model.Attempt) {
	e.openCodeHistory.Apply(a)
	e.recordOpenCodeObservation(a)
	if e.cb != nil && a.Code == "RATE_LIMIT" && (a.ProviderRateLimitScope == "provider" || a.ProviderRateLimitScope == "free-limit" || a.ProviderReason == "provider.free-limit" || a.RetryAfterMS >= 59_760_000) {
		cooldown := 59760 * time.Second
		if a.RetryAfterMS > 0 {
			cooldown = time.Duration(a.RetryAfterMS) * time.Millisecond
		} else if a.RetryAt != nil && a.FinishedAt != nil {
			cooldown = a.RetryAt.Sub(*a.FinishedAt)
		}
		e.cb.Trip("opencode", cooldown)
	}
}

// OpenCodeVerifiedSuccess is shared by dispatch and read-only diagnostics.
func OpenCodeVerifiedSuccess(a model.Attempt) bool {
	return a.Provider == "opencode" && a.FinishedAt != nil && a.Code == "" && a.OpenCode != nil && a.OpenCode.GuardVerified && a.OpenCode.TerminalVerified &&
		a.OpenCode.PromptSent && a.OpenCode.Model == a.Model && a.OpenCode.ReportedCost != nil && *a.OpenCode.ReportedCost == 0 &&
		!a.StartedAt.IsZero() && !a.FinishedAt.Before(a.StartedAt) &&
		providers.OpenCodeIdentityMatches(a.Model, a.Fingerprint) && model.OpenCodeAccountID(a.OpenCodeAccount) == model.OpenCodeAnonymous
}

func OpenCodeRouteEvidenceKey(modelID, account, transport, endpoint, scope, asserted string) string {
	id := openCodeRoute(modelID, account, transport, endpoint)
	key := openCodeObservedGateKey(scope, asserted, id)
	return model.Digest([]string{key.scope, key.upstream, key.account, key.endpoint, key.model, key.transport})
}

func (e *Engine) recordOpenCodeObservation(a *model.Attempt) {
	if a.Provider != "opencode" || a.FinishedAt == nil {
		return
	}
	id := openCodeRoute(a.Model, a.OpenCodeAccount, a.OpenCodeTransport, a.OpenCodeEndpoint)
	if OpenCodeVerifiedSuccess(*a) {
		if e.cb != nil {
			e.cb.Success("opencode")
		}
		if e.openCodeSuccess == nil {
			e.openCodeSuccess = make(map[openCodeIdentity]time.Time)
		}
		if a.StartedAt.After(e.openCodeSuccess[id]) {
			e.openCodeSuccess[id] = a.StartedAt
		}
		return
	}
	scope := OpenCodeHealthScope(*a)
	if scope == "" || scope == "session" || a.RetryAt == nil {
		return
	}
	var reset *time.Time
	for _, hint := range a.RetryHints {
		if hint.Valid && (hint.Source == "provider_reset_at" || hint.Source == "http_rate_limit_reset") &&
			hint.ResetAt != nil && (reset == nil || hint.ResetAt.After(*reset)) {
			reset = hint.ResetAt
		}
	}
	key := openCodeObservedGateKey(scope, a.ProviderRateLimitScope, id)
	e.rememberOpenCodeGate(key, openCodeHealth{resetAt: reset, origin: id, assertedScope: a.ProviderRateLimitScope, probe: a.OpenCodeRecheck,
		suspect: openCodeSuspect(*a), until: *a.RetryAt, code: a.Code, observed: *a.FinishedAt, source: a.RetrySource})
}

func openCodeSuspect(a model.Attempt) bool {
	return a.Code == "RATE_LIMIT" && a.ProviderRateLimitScope == "" &&
		a.UpstreamStatus == 429 && a.RetrySource == "provider_body" &&
		a.OpenCodeTransport == model.OpenCodeNativeReadAsk && a.RetryAfterMS > (5*time.Minute).Milliseconds()
}

func (e *Engine) rememberOpenCodeGate(key openCodeHealthKey, gate openCodeHealth) {
	if e.openCodeHealth == nil {
		e.openCodeHealth = make(map[openCodeHealthKey]openCodeHealth)
	}
	current := e.openCodeHealth[key]
	if current.code == "RATE_LIMIT" && gate.code != "RATE_LIMIT" && e.openCodeGateCurrent(current, gate.observed) {
		return
	}
	if gate.observed.After(current.observed) || gate.observed.Equal(current.observed) && gate.until.After(current.until) {
		e.openCodeHealth[key] = gate
	}
}

func (e *Engine) openCodeGateCurrent(g openCodeHealth, now time.Time) bool {
	return g.until.After(now) && !e.openCodeSuccess[g.origin].After(g.observed)
}

func openCodeGateKeys(id openCodeIdentity) []openCodeHealthKey {
	return []openCodeHealthKey{
		openCodeKey("pool", id), openCodeKey("account", id), openCodeKey("provider", id),
		openCodeObservedGateKey("model", "model", id), openCodeKey("model", id),
	}
}

func (e *Engine) openCodeGate(c model.Capability, now time.Time) (string, openCodeHealth) {
	return e.openCodeRouteGate(openCodeRoute(c.Model, c.OpenCodeAccount, c.OpenCodeTransport, c.OpenCodeEndpoint), now)
}

func (e *Engine) openCodeRouteGate(id openCodeIdentity, now time.Time) (string, openCodeHealth) {
	var scope string
	var gate openCodeHealth
	for _, key := range openCodeGateKeys(id) {
		health := e.openCodeHealth[key]
		if e.openCodeGateCurrent(health, now) && health.until.After(gate.until) {
			scope, gate = key.scope, health
		}
	}
	return scope, gate
}

func (e *Engine) openCodeRecovery(c model.Capability, now time.Time) string {
	id := openCodeRoute(c.Model, c.OpenCodeAccount, c.OpenCodeTransport, c.OpenCodeEndpoint)
	for jobID := range e.active {
		job := e.jobs[jobID]
		if n := len(job.Attempts); n > 0 {
			a := job.Attempts[n-1]
			if a.OpenCodeRecheck && a.FinishedAt == nil && openCodeRoute(a.Model, a.OpenCodeAccount, a.OpenCodeTransport, a.OpenCodeEndpoint) == id {
				return "REVALIDATING"
			}
		}
	}
	if _, gate := e.openCodeRouteGate(id, now); !gate.until.IsZero() {
		if gate.suspect && !gate.probe {
			return "COOLDOWN_SUSPECT"
		}
		return "COOLDOWN_ACTIVE"
	}
	for _, key := range openCodeGateKeys(id) {
		g := e.openCodeHealth[key]
		if !g.observed.IsZero() && e.openCodeSuccess[g.origin].After(g.observed) {
			return "RECOVERED"
		}
	}
	if !e.openCodeSuccess[id].IsZero() {
		return "AVAILABLE"
	}
	return "UNKNOWN"
}

// A diagnostic is an ordinary durable attempt with a fixed prompt. The
// scheduler lock covers this check and reservation, including after restart.
func (e *Engine) openCodeRecheck(c model.Capability, now time.Time) (allowed bool, retryAt *time.Time) {
	id := openCodeRoute(c.Model, c.OpenCodeAccount, c.OpenCodeTransport, c.OpenCodeEndpoint)
	if e.openCodeInFlight(id) {
		return false, nil
	}
	var active *time.Time
	for _, key := range openCodeGateKeys(id) {
		g := e.openCodeHealth[key]
		if !e.openCodeGateCurrent(g, now) {
			continue
		}
		if !g.suspect || g.probe || g.origin != id {
			return false, &g.until
		}
		active = &g.observed
	}
	for _, job := range e.jobs {
		for _, a := range job.Attempts {
			if !a.OpenCodeRecheck || openCodeRoute(a.Model, a.OpenCodeAccount, a.OpenCodeTransport, a.OpenCodeEndpoint) != id {
				continue
			}
			cadence := a.StartedAt.Add(5 * time.Minute)
			if cadence.After(now) || active != nil && !a.StartedAt.Before(*active) {
				return false, &cadence
			}
		}
	}
	return true, nil
}

func (e *Engine) openCodeInFlight(identity openCodeIdentity) bool {
	// Both transports can consume capacity from the same exact model.
	identity.transport = ""
	for id := range e.active {
		job := e.jobs[id]
		if len(job.Attempts) > 0 {
			last := job.Attempts[len(job.Attempts)-1]
			active := openCodeRoute(last.Model, last.OpenCodeAccount, "", last.OpenCodeEndpoint)
			active.transport = ""
			if last.Provider == "opencode" && last.FinishedAt == nil && active == identity {
				return true
			}
		}
	}
	return false
}
