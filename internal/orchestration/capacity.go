// Copyright 2026 Furkan Anıl Öztaş
// SPDX-License-Identifier: Apache-2.0

package orchestration

import (
	"cmp"
	"slices"
	"strings"
	"time"

	"github.com/faoztas/ahenk-mcp/internal/model"
	"github.com/faoztas/ahenk-mcp/internal/providers"
)

type CapacitySnapshot struct {
	Codex    []providers.Quota `json:"codex"`
	AGY      []providers.Quota `json:"agy"`
	OpenCode []providers.Quota `json:"opencode_free"`
}

func timePointer(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}

func earlierReset(current, next *time.Time, now time.Time) *time.Time {
	if next != nil && next.After(now) && (current == nil || next.Before(*current)) {
		return next
	}
	return current
}

func (e *Engine) lastWorkerResults(c model.Capability) (success, failure time.Time, code string) {
	for _, j := range e.jobs {
		for _, a := range j.Attempts {
			if a.FinishedAt == nil || a.Provider != c.Provider || a.Model != c.Model ||
				c.Fingerprint != "" && a.Fingerprint != c.Fingerprint ||
				c.Provider == "opencode" && !OpenCodeGateApplies("model", a.Model, a.OpenCodeAccount, a.OpenCodeTransport, c) {
				continue
			}
			if (a.Code == "" || a.Code == "CHECKPOINT_READY") && a.FinishedAt.After(success) &&
				(a.AGY != nil && a.AGY.TerminalVerified || a.OpenCode != nil && a.OpenCode.TerminalVerified) {
				success = *a.FinishedAt
			}
			if a.Code != "" && a.Code != "CHECKPOINT_READY" && a.FinishedAt.After(failure) {
				failure, code = *a.FinishedAt, a.Code
			}
		}
	}
	return
}

func (e *Engine) workerReliability(c model.Capability, now time.Time) int {
	if c.Mode == "" || c.Mode == "fixture" {
		return 0
	}
	success, failure, _ := e.lastWorkerResults(c)
	cutoff := now.Add(-24 * time.Hour)
	if failure.After(success) && failure.After(cutoff) {
		return -1
	}
	if success.After(cutoff) {
		return 1
	}
	return 0
}

func normalizedQuota(q providers.Quota, policy model.CapacityPolicy) providers.Quota {
	q.State = quotaState(q, policy)
	if q.State == "UNKNOWN" || q.State == "STALE" {
		q.Remaining = nil
		if q.AccountMatch != "MATCHED" {
			q.ResetAt = nil
		}
	}
	return q
}

func (e *Engine) capacitySnapshot(now time.Time, repoID string) CapacitySnapshot {
	policy := e.cfg.CapacityPolicy.Defaults()
	codex := e.quotas["codex"]
	five := controllerWindow(codex, 300, now)
	weekly := controllerWindow(codex, weeklyMinutes, now)
	out := CapacitySnapshot{Codex: []providers.Quota{normalizedQuota(five, policy), normalizedQuota(weekly, policy)},
		AGY: []providers.Quota{}, OpenCode: []providers.Quota{}}
	for _, q := range codex {
		if strings.HasPrefix(q.Bucket, "extra:") || q.WindowMinutes > 0 && q.Bucket != five.Bucket && q.Bucket != weekly.Bucket {
			out.Codex = append(out.Codex, normalizedQuota(q.At(now), policy))
		}
	}
	for _, family := range []string{"gemini", "claude_gpt", "gemini_argon"} {
		reserve := e.agyReserve(model.Job{RepoID: repoID}, family, now, 2*time.Minute, weekly)
		for _, id := range providers.AGYFamilyBuckets(family) {
			q := providers.UnknownQuota("agy")
			q.Bucket, q.Family, q.WindowMinutes = id, family, 300
			if strings.HasSuffix(id, "weekly") {
				q.WindowMinutes = weeklyMinutes
			}
			for _, observed := range e.quotas["agy"] {
				if observed.Bucket == id && observed.Family == family {
					q = observed.At(now)
					break
				}
			}
			if reserve.reason == reserveReason && reserve.weekly.Bucket == id {
				q = reserve.weekly
				q.State = "PROTECTED_RESERVE"
			} else {
				q = normalizedQuota(q, policy)
			}
			out.AGY = append(out.AGY, q)
		}
	}
	for _, c := range e.reg.Candidates {
		if c.Provider != "opencode" || c.Mode == "fixture" {
			continue
		}
		if repoID != "" && !slices.Contains(e.cfg.Repositories[repoID].Providers, c.Provider) {
			continue
		}
		q := providers.UnknownQuota("opencode")
		q.Model, q.OpenCodeAccount, q.OpenCodeTransport, q.OpenCodeProvider = c.Model, c.OpenCodeAccount, c.OpenCodeTransport, "opencode"
		q.Bucket, q.Quality, q.Cost = "anonymous_free", c.Quality, c.Cost
		success, failure, code := e.lastWorkerResults(c)
		confidence := "live_attempt"
		if inventory := e.cfg.ModelAudit; inventory != nil && inventory.ObservedAt.After(success) &&
			!inventory.ObservedAt.After(now) && slices.ContainsFunc(inventory.Models, func(row model.ModelAudit) bool {
			return row.Provider == c.Provider && row.Model == c.Model && row.Binary == c.Fingerprint &&
				row.Actual == c.Model && row.Inference == "PASS" && row.Terminal && row.RuntimeCost == "ZERO_REPORTED"
		}) {
			success, confidence = inventory.ObservedAt, "live_inference_audit"
		}
		q.LastSuccess, q.LastFailure, q.LastFailureCode = timePointer(success), timePointer(failure), code
		scope, gate := e.openCodeGate(c, now)
		q.Endpoint, q.RecoveryState = c.OpenCodeEndpoint, e.openCodeRecovery(c, now)
		if scope != "" {
			q.CooldownScope = openCodeScope(gate.assertedScope)
		}
		switch {
		case c.Status != "available" || !workerCostAllowed(c) || c.AuditValidUntil != nil && !now.Before(*c.AuditValidUntil):
			q.State, q.Reason = "UNAVAILABLE", c.Reason
			if c.AuditValidUntil != nil && !now.Before(*c.AuditValidUntil) {
				q.Reason = "AUDIT_ROLE_OR_FRESHNESS_GATE"
			}
		case scope != "":
			q.State, q.Reason, q.HealthScope, q.RetryAt = "UNAVAILABLE", gate.code, scope, &gate.until
			q.ResetAt = gate.resetAt
			if gate.code == "RATE_LIMIT" {
				q.State = "RATE_LIMITED"
			}
			q.ObservedAt, q.Confidence, q.RetrySource = gate.observed, "provider_evidence", gate.source
		case success.After(failure) && !success.After(now) && now.Sub(success) <= 24*time.Hour:
			q.State, q.ObservedAt, q.Confidence = "HEALTHY", success, confidence
		case c.AuditValidUntil != nil && failure.IsZero():
			q.State, q.ObservedAt, q.Confidence = "HEALTHY", c.AuditValidUntil.Add(-24*time.Hour), "live_inference_audit"
		default:
			q.Reason, q.Confidence = "PREFLIGHT_REQUIRED", "unverified"
		}
		out.OpenCode = append(out.OpenCode, q)
	}
	if inventory := e.cfg.ModelAudit; inventory != nil && (repoID == "" || slices.Contains(e.cfg.Repositories[repoID].Providers, "opencode")) {
		for _, row := range inventory.Models {
			if row.Provider != "opencode" || slices.ContainsFunc(out.OpenCode, func(q providers.Quota) bool { return q.Model == row.Model }) {
				continue
			}
			q := providers.UnknownQuota("opencode")
			q.Model, q.Bucket, q.OpenCodeProvider, q.OpenCodeAccount = row.Model, "anonymous_free", "opencode", model.OpenCodeAnonymous
			q.State, q.Reason, q.ObservedAt, q.Confidence = "UNAVAILABLE", "AUDIT_ROUTE_NOT_ADMITTED", inventory.ObservedAt, "catalog_history"
			if row.Inference == "PASS" {
				q.LastSuccess = timePointer(inventory.ObservedAt)
			}
			if row.Inference != "PASS" && row.LatencyMS > 0 {
				q.LastFailure, q.LastFailureCode = timePointer(inventory.ObservedAt), row.Inference
			}
			out.OpenCode = append(out.OpenCode, q)
		}
	}
	slices.SortStableFunc(out.OpenCode, func(a, b providers.Quota) int {
		return cmp.Or(cmp.Compare(a.Model, b.Model), cmp.Compare(a.OpenCodeTransport, b.OpenCodeTransport))
	})
	return out
}

func (e *Engine) Capacity(repoID string) (CapacitySnapshot, []model.CapacityWarning, []model.Decision) {
	e.mu.Lock()
	defer e.mu.Unlock()
	now := time.Now().UTC()
	var routes []model.Decision
	if repoID != "" {
		routes = e.previewRoute(model.Job{RepoID: repoID}, now)
	} else {
		for id := range e.cfg.Repositories {
			routes = append(routes, e.previewRoute(model.Job{RepoID: id}, now)...)
		}
	}
	return e.capacitySnapshot(now, repoID), e.capacityWarnings("", routes, "CAPABILITY_CHECK", now), routes
}

func (e *Engine) previewRoute(j model.Job, now time.Time) []model.Decision {
	if j.Task.Mode == "" {
		j.Task = model.Task{Mode: "read_only", DataClass: "synthetic", TaskClass: "utility"}
	}
	if j.DeadlineAt.IsZero() {
		j.DeadlineAt = now.Add(10 * time.Minute)
	}
	e.route(&j, now)
	for i := range j.Route {
		if j.Route[i].Reason == "SELECTED" {
			j.Route[i].Reason = "AVAILABLE_CANDIDATE"
		}
	}
	return j.Route
}

func (e *Engine) JobCapacityWarnings(id string) []model.CapacityWarning {
	e.mu.Lock()
	defer e.mu.Unlock()
	j, ok := e.jobs[id]
	if !ok || !e.repoAllowed(j) {
		return nil
	}
	now := time.Now().UTC()
	if len(j.Children) == 0 {
		routes := j.Route
		if len(routes) == 0 && !model.Terminal(j.Status) {
			routes = e.previewRoute(j, now)
		}
		return e.capacityWarnings(id, routes, j.Status, now)
	}
	var out []model.CapacityWarning
	for _, id := range j.Children {
		child := e.jobs[id]
		if !e.repoAllowed(child) {
			continue
		}
		routes := child.Route
		if len(routes) == 0 && !model.Terminal(child.Status) {
			routes = e.previewRoute(child, now)
		}
		out = append(out, e.capacityWarnings(j.ID, routes, child.Status, now)...)
	}
	return dedupeWarnings(out)
}

func dedupeWarnings(rows []model.CapacityWarning) []model.CapacityWarning {
	seen := map[string]bool{}
	return slices.DeleteFunc(rows, func(w model.CapacityWarning) bool {
		duplicate := seen[w.ID]
		seen[w.ID] = true
		return duplicate
	})
}

func (e *Engine) capacityWarnings(id string, routes []model.Decision, consequence string, now time.Time) []model.CapacityWarning {
	var out []model.CapacityWarning
	policy := e.cfg.CapacityPolicy.Defaults()
	appendWarning := func(w model.CapacityWarning) {
		w.Consequence, w.AssessedAt = consequence, now
		w.ID = model.Digest([]any{id, w.Code, w.Severity, w.Provider, w.Model, w.Bucket, w.Scope, w.ResetAt, w.RetryAt, w.Alternative, w.AGYState, w.OpenCodeState})[:24]
		out = append(out, w)
	}
	five := controllerWindow(e.quotas["codex"], 300, now)
	weekly := controllerWindow(e.quotas["codex"], weeklyMinutes, now)
	var next *time.Time
	for _, q := range []providers.Quota{five, weekly} {
		w := model.CapacityWarning{Provider: "codex", Bucket: q.Bucket, ObservedAt: timePointer(q.ObservedAt), ResetAt: q.ResetAt, Remaining: q.Remaining}
		switch state := quotaState(q, policy); state {
		case "LOW", "CRITICAL":
			w.Code, w.Severity = "CODEX_CONTROLLER_LOW", strings.ToLower(state)
			w.Message = "The observed Codex controller window is low. Further coordination may be interrupted."
			w.NextAction = "Use compact batches and inspect verified results from eligible workers."
		case "EXHAUSTED":
			w.Code, w.Severity = "CODEX_CONTROLLER_EXHAUSTED", "critical"
			w.Message, w.NextAction = "The observed Codex controller window is exhausted.", "Wait for a verified reset. Already accepted jobs may continue."
			next = earlierReset(next, q.ResetAt, now)
		case "UNKNOWN", "STALE":
			w.Code, w.Severity, w.Remaining = "CAPACITY_UNVERIFIED", "info", nil
			if q.AccountMatch != "MATCHED" {
				w.ResetAt = nil
			}
			w.Message, w.NextAction = "The Codex controller window has no fresh matched balance.", "Refresh the configured account binding or check the controller directly."
		default:
			continue
		}
		appendWarning(w)
	}
	var selected *model.Decision
	openEligible, agyEligible, canOffload, waiting := false, false, false, false
	openRelevant, agyRelevant, external := false, false, false
	for i := range routes {
		d := &routes[i]
		external = external || d.Provider == "agy" || d.Provider == "opencode"
		if d.Reason == "SELECTED" {
			selected = d
		}
		if d.Provider == "opencode" && d.Reason != "REPO_PROVIDER_POLICY" {
			openRelevant = true
		}
		if d.Provider == "agy" && d.Reason != "REPO_PROVIDER_POLICY" {
			agyRelevant = true
		}
		if !d.PolicyEligible {
			continue
		}
		if d.RetryAt != nil && !d.RetryAt.After(now) && d.Reason != "SELECTED" && d.Reason != "FALLBACK_CANDIDATE" {
			waiting = true
		}
		switch d.Reason {
		case "AGY_FAMILY_IN_FLIGHT", "OPENCODE_MODEL_IN_FLIGHT":
			waiting = true
		case "SELECTED", "FALLBACK_CANDIDATE", "AVAILABLE_CANDIDATE":
			canOffload = true
			openEligible = openEligible || d.Provider == "opencode"
			agyEligible = agyEligible || d.Provider == "agy"
		}
		if d.Reason != "AGY_FAMILY_IN_FLIGHT" && d.Reason != "OPENCODE_MODEL_IN_FLIGHT" {
			next = earlierReset(next, d.RetryAt, now)
		}
	}
	for _, d := range routes {
		if !d.PolicyEligible {
			continue
		}
		if d.CapacityState == "PROTECTED_RESERVE" && (d.QuotaResetAt == nil || d.QuotaResetAt.After(now)) {
			alternative := "NO_EQUIVALENT_OPENCODE_ROUTE"
			if openEligible {
				alternative = "EQUIVALENT_OPENCODE_ROUTE"
			}
			if selected != nil {
				alternative += ":SELECTED_" + strings.ToUpper(selected.Provider)
			}
			appendWarning(model.CapacityWarning{Code: reserveReason, Severity: "warning", Provider: "agy", Bucket: d.QuotaBucket,
				Scope: providers.AGYModelFamily(d.Model), ObservedAt: d.QuotaObservedAt, Remaining: d.QuotaRemaining, ResetAt: d.QuotaResetAt,
				Threshold: d.WeeklyThreshold, Alternative: alternative, Cause: d.Reason, Message: "AGY weekly capacity is held for the configured reserve and estimated consumption.",
				NextAction: "Use an equivalent route, wait for a verified reset, or supply an authorized task override."})
		}
		if d.Provider == "opencode" && d.Reason == "COOLDOWN" && d.RetryAt != nil && d.RetryAt.After(now) {
			modelID := d.Model
			if d.HealthScope == "pool" || d.HealthScope == "account" {
				modelID = ""
			}
			appendWarning(model.CapacityWarning{Code: "OPENCODE_FREE_UNAVAILABLE", Severity: "warning", Provider: "opencode", Model: modelID,
				Scope: d.HealthScope, ObservedAt: d.QuotaObservedAt, RetryAt: d.RetryAt, RetrySource: d.RetrySource, Cause: d.CapacityState,
				Message: "OpenCode Free has a model or confirmed shared cooldown.", NextAction: "Honor the cooldown and use only an eligible independent route."})
		}
	}
	if openRelevant && !openEligible && !slices.ContainsFunc(out, func(w model.CapacityWarning) bool { return w.Code == "OPENCODE_FREE_UNAVAILABLE" }) && !waiting {
		appendWarning(model.CapacityWarning{Code: "OPENCODE_FREE_UNAVAILABLE", Severity: "warning", Provider: "opencode", Scope: "task",
			Message: "OpenCode Free has no eligible route for this task.", NextAction: "Inspect the rejected routes for capability, policy or availability details."})
	}
	if agyRelevant && slices.ContainsFunc(routes, func(d model.Decision) bool {
		return d.Provider == "agy" && d.PolicyEligible && slices.Contains(d.WarningCodes, "CAPACITY_UNVERIFIED")
	}) {
		appendWarning(model.CapacityWarning{Code: "CAPACITY_UNVERIFIED", Severity: "info", Provider: "agy",
			Message: "Some eligible AGY capacity is unverified in CodexBar.", NextAction: "The native quota and cost preflight is required before sending a prompt."})
	}
	controllerUsable := verifiedQuota(five) && *five.Remaining > 0 && verifiedQuota(weekly) && *weekly.Remaining > 0 &&
		!slices.ContainsFunc(e.quotas["codex"], func(q providers.Quota) bool {
			q = q.At(now)
			return strings.HasPrefix(q.Bucket, "extra:") && verifiedQuota(q) && *q.Remaining == 0
		})
	fiveExhausted := verifiedQuota(five) && *five.Remaining == 0
	exhausted := fiveExhausted || verifiedQuota(weekly) && *weekly.Remaining == 0
	agyState := "NO_ELIGIBLE_ROUTE"
	if agyEligible {
		agyState = "PREFLIGHT_REQUIRED"
	}
	if !agyEligible && slices.ContainsFunc(routes, func(d model.Decision) bool {
		return d.Provider == "agy" && d.PolicyEligible && d.CapacityState == "PROTECTED_RESERVE"
	}) {
		agyState = "PROTECTED_RESERVE"
	} else if !agyEligible && slices.ContainsFunc(routes, func(d model.Decision) bool {
		return d.Provider == "agy" && d.PolicyEligible && d.CapacityState == "EXHAUSTED"
	}) {
		agyState = "EXHAUSTED"
	}
	openState := "NO_ELIGIBLE_ROUTE"
	if openEligible {
		openState = "PREFLIGHT_REQUIRED"
	}
	if !openEligible && slices.ContainsFunc(routes, func(d model.Decision) bool {
		return d.Provider == "opencode" && d.PolicyEligible && d.CapacityState == "RATE_LIMITED" && d.RetryAt != nil && d.RetryAt.After(now)
	}) {
		openState = "RATE_LIMITED"
	}
	if fiveExhausted && !openEligible && openRelevant && !waiting {
		appendWarning(model.CapacityWarning{Code: "CODEX_5H_EXHAUSTED_OPENCODE_UNAVAILABLE", Severity: "critical", Provider: "codex",
			Bucket: five.Bucket, ObservedAt: timePointer(five.ObservedAt), Remaining: five.Remaining, ResetAt: next, AGYState: agyState, OpenCodeState: openState,
			Message:    "The Codex five-hour window is exhausted and OpenCode Free has no eligible route.",
			NextAction: "Wait for a verified reset. Already accepted AGY work may continue under its own policy."})
	}
	if !canOffload && !waiting && external {
		switch {
		case controllerUsable:
			appendWarning(model.CapacityWarning{Code: "CODEX_ONLY_WARNING", Severity: "warning", Provider: "codex", ResetAt: next,
				ObservedAt: timePointer(five.ObservedAt), Remaining: five.Remaining, Bucket: five.Bucket, AGYState: agyState, OpenCodeState: openState,
				Message:    "No eligible external worker is currently available. Continuing in Codex App will consume Codex quota.",
				NextAction: "Inspect the external route reasons or wait for the next verified recovery."})
		case exhausted:
			appendWarning(model.CapacityWarning{Code: "ALL_ELIGIBLE_CAPACITY_BLOCKED", Severity: "critical", ResetAt: next,
				ObservedAt: timePointer(five.ObservedAt), Remaining: five.Remaining, Bucket: five.Bucket, AGYState: agyState, OpenCodeState: openState,
				Message:    "The controller is exhausted and no eligible external worker is available.",
				NextAction: "Wait for the earliest verified recovery or inspect the blocked route reasons."})
		}
	}
	return dedupeWarnings(out)
}

func (e *Engine) recordCapacityCodes(j *model.Job, now time.Time) {
	j.CapacityWarningCodes = nil
	for _, w := range e.capacityWarnings(j.ID, j.Route, j.Status, now) {
		if !slices.Contains(j.CapacityWarningCodes, w.Code) {
			j.CapacityWarningCodes = append(j.CapacityWarningCodes, w.Code)
		}
	}
}
