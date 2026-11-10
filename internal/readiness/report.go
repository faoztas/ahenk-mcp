// Copyright 2026 Furkan Anıl Öztaş
// SPDX-License-Identifier: Apache-2.0

package readiness

import (
	"cmp"
	"context"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/faoztas/ahenk-mcp/internal/execution"
	"github.com/faoztas/ahenk-mcp/internal/model"
	"github.com/faoztas/ahenk-mcp/internal/orchestration"
	"github.com/faoztas/ahenk-mcp/internal/providers"
	"github.com/faoztas/ahenk-mcp/internal/store"
)

type Report struct {
	AsOf                 time.Time             `json:"as_of"`
	State                string                `json:"state"`
	Offline              Gate                  `json:"OFFLINE_CORE_READY"`
	AGY                  Gate                  `json:"AGY_LIVE_READY"`
	OpenCode             Gate                  `json:"OPENCODE_LIVE_READY"`
	MultiProvider        Gate                  `json:"MULTI_PROVIDER_LIVE_READY"`
	OpenCodeCapacity     string                `json:"opencode_capacity"`
	OpenCodeRecovery     string                `json:"opencode_recovery"`
	OpenCodeRecoveryGate *FreeGate             `json:"opencode_recovery_gate,omitempty"`
	OpenCodeFreeGate     *FreeGate             `json:"opencode_free_gate,omitempty"`
	OpenCodeModels       []OpenCodeModelReport `json:"opencode_models,omitempty"`
	OpenCodeSharedGates  []FreeGate            `json:"opencode_shared_gates,omitempty"`
}

type OpenCodeModelReport struct {
	Model             string    `json:"model"`
	Account           string    `json:"opencode_account"`
	OpenCodeTransport string    `json:"opencode_transport"`
	Configured        bool      `json:"configured"`
	Readiness         Gate      `json:"readiness"`
	Recovery          string    `json:"recovery"`
	RecoveryGate      *FreeGate `json:"recovery_gate,omitempty"`
	ActiveGate        *FreeGate `json:"active_gate,omitempty"`
}

type FreeGate struct {
	Endpoint          string     `json:"endpoint,omitempty"`
	SupersededAt      *time.Time `json:"superseded_at,omitempty"`
	ReplacedAt        *time.Time `json:"replaced_at,omitempty"`
	Account           string     `json:"opencode_account,omitempty"`
	OpenCodeTransport string     `json:"opencode_transport,omitempty"`
	Scope             string     `json:"scope"`
	RecordedScope     string     `json:"recorded_scope,omitempty"`
	ProviderScope     string     `json:"provider_scope,omitempty"`
	Model             string     `json:"model,omitempty"`
	Reason            string     `json:"reason"`
	ObservedAt        time.Time  `json:"observed_at,omitzero"`
	RetryAt           time.Time  `json:"retry_at"`
	Origin            string     `json:"origin"`
	JobID             string     `json:"source_job_id,omitempty"`
	Attempt           int        `json:"source_attempt,omitempty"`
}

type Gate struct {
	Ready       bool      `json:"ready"`
	Reason      string    `json:"reason"`
	Next        string    `json:"next,omitempty"`
	Observation *Evidence `json:"evidence,omitempty"`
}

type Evidence struct {
	Kind              string                     `json:"kind"`
	JobID             string                     `json:"job_id"`
	JobStatus         string                     `json:"job_status"`
	Children          []ChildEvidence            `json:"children,omitempty"`
	SnapshotID        string                     `json:"snapshot_id,omitempty"`
	Provider          string                     `json:"provider,omitempty"`
	Model             string                     `json:"model,omitempty"`
	Fingerprint       string                     `json:"fingerprint,omitempty"`
	Attempt           int                        `json:"attempt,omitempty"`
	ObservedAt        *time.Time                 `json:"observed_at,omitempty"`
	FinishedAt        *time.Time                 `json:"finished_at,omitempty"`
	CompletedAudit    bool                       `json:"completed_audit"`
	ArtifactSHA256    string                     `json:"artifact_sha256,omitempty"`
	SummarySHA256     string                     `json:"summary_sha256,omitempty"`
	AGY               *AGYEvidence               `json:"agy,omitempty"`
	OpenCodeTransport string                     `json:"opencode_transport,omitempty"`
	OpenCode          *model.OpenCodeRunEvidence `json:"opencode,omitempty"`
	Cost              *model.CostEvidence        `json:"cost,omitempty"`
	QuotaAtRoute      *RouteQuota                `json:"quota_at_route,omitempty"`
}

type AGYEvidence struct {
	PermissionMode    string                        `json:"permission_mode,omitempty"`
	ProviderReason    string                        `json:"provider_reason,omitempty"`
	RejectedStep      string                        `json:"rejected_step,omitempty"`
	LastEvent         *model.AGYStreamEventEvidence `json:"last_event,omitempty"`
	PromptSent        bool                          `json:"prompt_sent"`
	NativeFinish      bool                          `json:"native_finish_observed,omitempty"`
	StartupVerified   bool                          `json:"startup_verified"`
	TerminalVerified  bool                          `json:"terminal_verified"`
	SubscriptionQuota bool                          `json:"subscription_quota_verified"`
	TotalTokens       int64                         `json:"total_tokens,omitempty"`
}

type ChildEvidence struct {
	JobID             string `json:"job_id"`
	Provider          string `json:"provider"`
	Model             string `json:"model"`
	Fingerprint       string `json:"fingerprint,omitempty"`
	Attempt           int    `json:"attempt"`
	OpenCodeTransport string `json:"opencode_transport,omitempty"`
}

type RouteQuota struct {
	Bucket              string     `json:"bucket,omitempty"`
	Remaining           *float64   `json:"remaining_percent,omitempty"`
	ResetAt             *time.Time `json:"reset_at,omitempty"`
	ControllerRemaining *float64   `json:"controller_remaining_percent,omitempty"`
}

var (
	smokeMarker = regexp.MustCompile(`^ahenk-ready-[a-zA-Z0-9_-]{8,64}$`)
	diffMarker  = regexp.MustCompile(`(?m)^\+AHENK_HANDOFF_MARKER=(ahenk-handoff-[a-zA-Z0-9_-]{8,64})$`)
	digest      = regexp.MustCompile(`^[a-f0-9]{64}$`)
	reasonCode  = regexp.MustCompile(`^[A-Z][A-Z0-9_]{0,63}$`)
)

// Read examines local evidence. It neither probes a provider nor starts a scheduler.
func Read(ctx context.Context, cfg model.Config, now time.Time) (Report, error) {
	dir, jobs, imports, err := loadEvidence(ctx, cfg)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return Report{}, err
	}
	report := evaluate(cfg, providers.New(cfg), dir, jobs, imports, now)
	if errors.Is(err, os.ErrNotExist) {
		report.State = "MISSING"
	}
	return report, nil
}

func loadEvidence(ctx context.Context, cfg model.Config) (string, []store.JobEvidence, []store.OpenCodeCooldownImport, error) {
	dir, jobs, err := store.ReadEvidence(ctx, cfg.StateDir)
	if err != nil {
		return "", nil, nil, err
	}
	imports, err := store.ReadOpenCodeCooldownImports(dir)
	if err == nil {
		for _, record := range imports {
			if !providers.OpenCodeIdentityMatches(record.Model, record.Fingerprint) {
				return "", nil, nil, model.Error("OPENCODE_RECOVERY_NOT_VERIFIED", "unverified OpenCode cooldown provenance")
			}
		}
	}
	return dir, jobs, imports, err
}

func currentEvidence(cfg model.Config, jobs []store.JobEvidence) map[string]store.JobEvidence {
	current := make(map[string]store.JobEvidence, len(jobs))
	for _, j := range jobs {
		repo, ok := cfg.Repositories[j.RepoID]
		if ok && model.ValidID(j.ID) && j.PolicyDigest == model.Digest(repo) {
			current[j.ID] = j
		}
	}
	return current
}

func pilotAGY(reg providers.Registry) model.Capability {
	for _, c := range reg.Candidates {
		if c.Provider == "agy" {
			return c
		}
	}
	return model.Capability{}
}

func openCodeBatchCapability(parent store.JobEvidence, jobs map[string]store.JobEvidence, reg providers.Registry) model.Capability {
	for _, child := range parent.Children {
		j := jobs[child]
		if j.Task.Mode != "read_only" || len(j.Attempts) != 1 {
			continue
		}
		a := j.Attempts[0]
		for _, c := range reg.Candidates {
			if a.Provider == "opencode" && c.Fingerprint == a.Fingerprint &&
				orchestration.OpenCodeGateApplies("model", a.Model, a.OpenCodeAccount, a.OpenCodeTransport, c) {
				return c
			}
		}
	}
	return model.Capability{}
}

func evaluate(cfg model.Config, reg providers.Registry, dir string, jobs []store.JobEvidence, imports []store.OpenCodeCooldownImport, now time.Time) Report {
	current := currentEvidence(cfg, jobs)
	agy := pilotAGY(reg)
	report := Report{
		AsOf: now, State: "AVAILABLE", OpenCodeCapacity: "UNKNOWN", OpenCodeRecovery: "NOT_RECORDED",
		Offline:       Gate{Reason: "FIXTURE_BATCH_NOT_VERIFIED", Next: "Run the offline test suite and record a verified fixture diff handoff in this state."},
		AGY:           liveGate(agy, current),
		MultiProvider: Gate{Reason: "LIVE_PROVIDER_GATES_CLOSED", Next: "Verify each provider with one controlled synthetic smoke before submitting the live batch."},
	}
	gates := openCodeGateHistory(jobs, imports)
	report.OpenCodeSharedGates = activeOpenCodeSharedGates(gates, now)
	var bestScore int
	for _, c := range reg.Candidates {
		if c.Provider != "opencode" || c.Mode == "fixture" {
			continue
		}
		m := openCodeModelReport(c, liveGate(c, current), gates, now)
		if m.Model != "" {
			report.OpenCodeModels = append(report.OpenCodeModels, m)
		}
		candidate := report.withOpenCode(m)
		score := 1
		switch {
		case requireProviderSmokes(candidate, now) == nil:
			score = 5
		case m.Readiness.Ready:
			score = 4
		case c.Status == "available" && m.ActiveGate == nil:
			score = 3
		case c.Status == "available":
			score = 2
		}
		if score > bestScore {
			report, bestScore = candidate, score
		}
	}
	if bestScore == 0 {
		report.OpenCode = liveGate(model.Capability{}, current)
	}
	for _, gate := range gates {
		if gate.Model == "" || slices.ContainsFunc(report.OpenCodeModels, func(m OpenCodeModelReport) bool {
			return m.Model == gate.Model && m.Account == gate.Account && m.OpenCodeTransport == gate.OpenCodeTransport
		}) {
			continue
		}
		c := model.Capability{Provider: "opencode", Model: gate.Model, OpenCodeAccount: gate.Account,
			OpenCodeTransport: gate.OpenCodeTransport, Reason: "MODEL_NOT_CONFIGURED"}
		m := openCodeModelReport(c, liveGate(c, current), gates, now)
		m.Configured = false
		report.OpenCodeModels = append(report.OpenCodeModels, m)
	}
	slices.SortFunc(report.OpenCodeModels, func(a, b OpenCodeModelReport) int {
		return cmp.Or(cmp.Compare(a.Model, b.Model), cmp.Compare(a.Account, b.Account), cmp.Compare(a.OpenCodeTransport, b.OpenCodeTransport))
	})
	if requireProviderSmokes(report, now) == nil {
		report.MultiProvider = Gate{Reason: "LIVE_BATCH_NOT_VERIFIED", Next: "Run one distinct-provider batch with a verified synthetic diff and a receiver that returns the marker from the diff."}
	} else if report.AGY.Ready && report.OpenCode.Ready {
		report.MultiProvider = Gate{Reason: "OPENCODE_RECOVERY_NOT_VERIFIED", Next: "Verify a successful OpenCode smoke after its recorded rate-limit gate."}
	}
	for _, parent := range jobs {
		if _, ok := current[parent.ID]; !ok {
			continue
		}
		if !report.Offline.Ready {
			if proof := batchProof(dir, parent, current, model.Capability{}, model.Capability{}, true); proof != nil {
				report.Offline = Gate{Ready: true, Reason: "FIXTURE_DIFF_HANDOFF_VERIFIED", Observation: proof}
			}
		}
		if !report.MultiProvider.Ready {
			c := openCodeBatchCapability(parent, current, reg)
			if c.Model == "" || requireProviderSmokes(m1SmokesAt(agy, c, current, gates, parent.CreatedAt), parent.CreatedAt) != nil {
				continue
			}
			if proof := batchProof(dir, parent, current, agy, c, false); proof != nil {
				report.MultiProvider = Gate{Ready: true, Reason: "LIVE_DIFF_HANDOFF_VERIFIED", Observation: proof}
			}
		}
	}
	return report
}

func (r Report) withOpenCode(m OpenCodeModelReport) Report {
	r.OpenCode, r.OpenCodeRecovery = m.Readiness, m.Recovery
	r.OpenCodeRecoveryGate, r.OpenCodeFreeGate = m.RecoveryGate, m.ActiveGate
	return r
}

func (r Report) forOpenCode(c model.Capability) Report {
	for _, m := range r.OpenCodeModels {
		if m.Configured && orchestration.OpenCodeGateApplies("model", m.Model, m.Account, m.OpenCodeTransport, c) {
			return r.withOpenCode(m)
		}
	}
	return r.withOpenCode(OpenCodeModelReport{})
}

func openCodeGateHistory(jobs []store.JobEvidence, imports []store.OpenCodeCooldownImport) []FreeGate {
	type observation struct {
		jobID   string
		attempt model.Attempt
	}
	var observations []observation
	for _, j := range jobs {
		for _, a := range j.Attempts {
			if a.Provider == "opencode" && a.FinishedAt != nil {
				observations = append(observations, observation{j.ID, a})
			}
		}
	}
	slices.SortStableFunc(observations, func(a, b observation) int {
		return a.attempt.FinishedAt.Compare(*b.attempt.FinishedAt)
	})
	var gates []FreeGate
	var history orchestration.OpenCodeHealthHistory
	for _, observation := range observations {
		a := observation.attempt
		recordedScope := a.HealthScope
		history.Apply(&a)
		if a.HealthScope != "session" && orchestration.OpenCodeHealthScope(a) != "" && a.RetryAt != nil {
			gates = append(gates, FreeGate{Scope: a.HealthScope, RecordedScope: recordedScope,
				Endpoint: a.OpenCodeEndpoint, Account: model.OpenCodeAccountID(a.OpenCodeAccount), ProviderScope: openCodeProviderScope(a.ProviderRateLimitScope),
				OpenCodeTransport: model.OpenCodeTransportID(a.OpenCodeTransport),
				Model:             a.Model, Reason: safeReason(a.Code, "COOLDOWN"), ObservedAt: *a.FinishedAt, RetryAt: *a.RetryAt,
				Origin: "attempt", JobID: observation.jobID, Attempt: a.Number})
		}
	}
	for _, record := range imports {
		if record.ProviderRateLimitScope == "session" {
			continue
		}
		gates = append(gates, FreeGate{Scope: orchestration.OpenCodeRateLimitScope(record.ProviderRateLimitScope), Model: record.Model, Endpoint: record.Endpoint,
			Account: model.OpenCodeAccountID(record.OpenCodeAccount), RecordedScope: record.RecordedScope,
			OpenCodeTransport: model.OpenCodeTransportID(record.OpenCodeTransport),
			ProviderScope:     openCodeProviderScope(record.ProviderRateLimitScope),
			Reason:            "RATE_LIMIT", ObservedAt: record.FinishedAt,
			RetryAt: orchestration.OpenCodeImportRetryAt(record),
			Origin:  "verified_import", JobID: record.JobID, Attempt: record.Attempt})
	}
	successes := map[string][]model.Attempt{}
	for _, obs := range observations {
		a := obs.attempt
		if orchestration.OpenCodeVerifiedSuccess(a) {
			key := orchestration.OpenCodeRouteEvidenceKey(a.Model, a.OpenCodeAccount, a.OpenCodeTransport, a.OpenCodeEndpoint, "model", "")
			successes[key] = append(successes[key], a)
		}
	}
	for i := range gates {
		g := &gates[i]
		key := orchestration.OpenCodeRouteEvidenceKey(g.Model, g.Account, g.OpenCodeTransport, g.Endpoint, "model", "")
		for _, a := range successes[key] {
			if a.StartedAt.After(g.ObservedAt) {
				g.SupersededAt = a.FinishedAt
				break
			}
		}
	}
	slices.SortStableFunc(gates, func(a, b FreeGate) int { return a.ObservedAt.Compare(b.ObservedAt) })
	latest := map[string]int{}
	for i := range gates {
		g := &gates[i]
		key := orchestration.OpenCodeRouteEvidenceKey(g.Model, g.Account, g.OpenCodeTransport, g.Endpoint, g.Scope, g.ProviderScope)
		if k, ok := latest[key]; ok {
			old := &gates[k]
			if !g.ObservedAt.After(old.ObservedAt) && !g.RetryAt.After(old.RetryAt) ||
				old.Reason == "RATE_LIMIT" && g.Reason != "RATE_LIMIT" && old.activeAt(g.ObservedAt) {
				g.ReplacedAt = &g.ObservedAt
				continue
			}
			old.ReplacedAt = &g.ObservedAt
		}
		latest[key] = i
	}
	return gates
}

func (g FreeGate) activeAt(now time.Time) bool {
	return !g.ObservedAt.After(now) && g.RetryAt.After(now) &&
		(g.SupersededAt == nil || g.SupersededAt.After(now)) && (g.ReplacedAt == nil || g.ReplacedAt.After(now))
}

func activeOpenCodeSharedGates(gates []FreeGate, now time.Time) []FreeGate {
	var shared []FreeGate
	for _, gate := range gates {
		if gate.Scope == "model" || !gate.activeAt(now) {
			continue
		}
		c := model.Capability{Provider: "opencode", Model: gate.Model, OpenCodeAccount: gate.Account, OpenCodeEndpoint: gate.Endpoint}
		match := slices.IndexFunc(shared, func(other FreeGate) bool {
			return gate.Scope == other.Scope && other.applies(c)
		})
		if match < 0 {
			shared = append(shared, gate)
		} else if gate.RetryAt.After(shared[match].RetryAt) {
			shared[match] = gate
		}
	}
	return shared
}

func openCodeModelReport(c model.Capability, readiness Gate, gates []FreeGate, now time.Time) OpenCodeModelReport {
	m := OpenCodeModelReport{Model: c.Model, Account: model.OpenCodeAccountID(c.OpenCodeAccount),
		OpenCodeTransport: model.OpenCodeTransportID(c.OpenCodeTransport),
		Configured:        c.Model != "", Readiness: readiness, Recovery: "NOT_RECORDED"}
	var otherModelGate, otherTransportGate *FreeGate
	for _, gate := range gates {
		if gate.ObservedAt.After(now) || gate.ReplacedAt != nil && !gate.ReplacedAt.After(now) {
			continue
		}
		if gate.applies(c) {
			m.noteGate(gate, now)
		} else if gate.Scope == "model" && gate.Reason == "RATE_LIMIT" && gate.Model != "" &&
			orchestration.OpenCodeGateApplies("account", gate.Model, gate.Account, gate.OpenCodeTransport, c) {
			if gate.Model == c.Model {
				if otherTransportGate == nil || gate.RetryAt.After(otherTransportGate.RetryAt) {
					otherTransportGate = &gate
				}
			} else if otherModelGate == nil || gate.RetryAt.After(otherModelGate.RetryAt) {
				otherModelGate = &gate
			}
		}
	}
	if m.RecoveryGate == nil && otherTransportGate != nil {
		m.Recovery, m.RecoveryGate = "RECORDED_GATE_OTHER_TRANSPORT", otherTransportGate
	} else if m.RecoveryGate == nil && otherModelGate != nil {
		m.Recovery, m.RecoveryGate = "RECORDED_GATE_OTHER_MODEL", otherModelGate
	}
	if m.ActiveGate != nil {
		m.Readiness.Ready = false
		m.Readiness.Reason = m.ActiveGate.Reason
		m.Readiness.Next = "Wait until the recorded gate for this route ends before a controlled synthetic attempt."
	} else if !m.Readiness.Ready && m.Recovery == "NOT_RECORDED" && m.Configured {
		m.Readiness.Next = "Locate the authorized persistent state and verify the previous OpenCode cooldown before planning a controlled recovery attempt."
	}
	return m
}

func (g FreeGate) applies(c model.Capability) bool {
	endpoint := g.Endpoint
	if endpoint == "" {
		endpoint = model.OpenCodeFreeEndpoint
	}
	effective := c.OpenCodeEndpoint
	if effective == "" {
		effective = model.OpenCodeFreeEndpoint
	}
	if endpoint != effective {
		return false
	}
	transport := g.OpenCodeTransport
	if g.Scope == "model" && g.ProviderScope == "model" {
		transport = c.OpenCodeTransport
	}
	c.OpenCodeEndpoint = ""
	return orchestration.OpenCodeGateApplies(g.Scope, g.Model, g.Account, transport, c)
}

func openCodeProviderScope(scope string) string {
	switch scope {
	case "pool", "account", "model", "provider", "session":
		return scope
	default:
		return "UNKNOWN"
	}
}

func (r *OpenCodeModelReport) noteGate(gate FreeGate, now time.Time) {
	if gate.Reason == "RATE_LIMIT" &&
		(r.RecoveryGate == nil || gate.RetryAt.After(r.RecoveryGate.RetryAt)) {
		r.RecoveryGate = &gate
		r.Recovery = "RECORDED_GATE_ELAPSED"
		if gate.SupersededAt != nil && !gate.SupersededAt.After(now) {
			r.Recovery = "RECORDED_GATE_SUPERSEDED"
		} else if gate.activeAt(now) {
			r.Recovery = "RECORDED_GATE_ACTIVE"
		}
	}
	if gate.activeAt(now) && (r.ActiveGate == nil || gate.RetryAt.After(r.ActiveGate.RetryAt)) {
		r.ActiveGate = &gate
	}
}

func liveGate(c model.Capability, jobs map[string]store.JobEvidence) Gate {
	return liveGateAt(c, jobs, time.Time{})
}

func liveGateAt(c model.Capability, jobs map[string]store.JobEvidence, before time.Time) Gate {
	gate := Gate{Reason: "LIVE_SMOKE_NOT_VERIFIED", Next: "Run one controlled synthetic smoke and verify its exact marker and audit."}
	if c.Status != "available" {
		gate.Reason = safeReason(c.Reason, "PROVIDER_ADMISSION_UNVERIFIED")
		gate.Next = "Verify the pinned binary, exact model and provider admission configuration."
	}
	var latest store.JobEvidence
	var attempt model.Attempt
	var observedAt time.Time
	for _, j := range jobs {
		if j.Task.DataClass != "synthetic" {
			continue
		}
		for _, a := range j.Attempts {
			if !before.IsZero() && (a.FinishedAt == nil || a.FinishedAt.After(before)) {
				continue
			}
			if a.Provider == c.Provider && (c.Provider != "opencode" ||
				orchestration.OpenCodeGateApplies("model", a.Model, a.OpenCodeAccount, a.OpenCodeTransport, c)) &&
				(smokeRequested(j.Job) || j.Task.OpenCodeRecheck || a.Code != "") && a.StartedAt.After(observedAt) {
				latest, attempt, observedAt = j, a, a.StartedAt
			}
		}
		if before.IsZero() && len(j.Attempts) == 0 && j.Task.PreferredProvider == c.Provider && dispatchMatches(j.Job, c) &&
			model.Terminal(j.Status) && j.CreatedAt.After(observedAt) {
			latest, attempt, observedAt = j, model.Attempt{}, j.CreatedAt
		}
	}
	if latest.ID == "" {
		return gate
	}
	gate.Observation = attemptEvidence(latest, attempt, observedAt)
	if c.Status != "available" {
		return gate
	}
	if attempt.Code != "" || latest.Reason != "" {
		gate.Reason = safeReason(attempt.Code, safeReason(latest.Reason, "LIVE_SMOKE_FAILED"))
		gate.Next = "Resolve the recorded reason before considering another controlled synthetic attempt."
		return gate
	}
	recheck := latest.Task.OpenCodeRecheck && latest.Result != nil && latest.Result.Summary == model.OpenCodeRecheckMarker &&
		orchestration.OpenCodeVerifiedSuccess(attempt)
	if liveAttempt(latest, attempt, c) && (markerAccepted(latest.Job) || recheck) {
		return Gate{Ready: true, Reason: "LIVE_SMOKE_VERIFIED", Observation: gate.Observation}
	}
	return gate
}

func dispatchMatches(j model.Job, c model.Capability) bool {
	if c.Provider != "opencode" {
		return true
	}
	if j.Task.PreferredModel != c.Model {
		return false
	}
	hasDecision := false
	for _, d := range j.Route {
		if d.Provider == c.Provider && d.Model == c.Model {
			hasDecision = true
			if orchestration.OpenCodeGateApplies("model", d.Model, d.OpenCodeAccount, d.OpenCodeTransport, c) {
				return true
			}
		}
	}
	return !hasDecision && model.OpenCodeTransportID(c.OpenCodeTransport) == model.OpenCodeStateless
}

func safeReason(value, fallback string) string {
	if reasonCode.MatchString(value) {
		return value
	}
	return fallback
}

func attemptEvidence(j store.JobEvidence, a model.Attempt, at time.Time) *Evidence {
	kind := "dispatch"
	if a.Number > 0 {
		kind = "live_attempt"
	}
	e := &Evidence{Kind: kind, JobID: j.ID, JobStatus: j.Status, SnapshotID: j.SnapshotID, Provider: a.Provider, Model: a.Model,
		Fingerprint: a.Fingerprint, Attempt: a.Number, ObservedAt: &at, FinishedAt: a.FinishedAt, CompletedAudit: j.CompletedAudited}
	if a.Provider == "opencode" {
		e.OpenCodeTransport = model.OpenCodeTransportID(a.OpenCodeTransport)
		if openCodeRunVerified(a) {
			e.OpenCode = a.OpenCode
		}
	}
	if j.Result != nil && digest.MatchString(j.Result.SummarySHA256) {
		e.SummarySHA256 = j.Result.SummarySHA256
	}
	if a.AGY != nil {
		e.AGY = &AGYEvidence{PromptSent: a.AGY.PromptSent, StartupVerified: a.AGY.StartupVerified,
			TerminalVerified: a.AGY.TerminalVerified, SubscriptionQuota: a.AGY.SubscriptionQuotaVerified, TotalTokens: a.AGY.TotalTokens,
			RejectedStep: a.AGY.RejectedStep, LastEvent: a.AGY.LastEvent, NativeFinish: a.AGY.NativeFinishObserved}
		if model.ValidID(a.AGY.PermissionMode) {
			e.AGY.PermissionMode = a.AGY.PermissionMode
		}
		if strings.HasPrefix(a.ProviderReason, "AGY_") {
			e.AGY.ProviderReason = safeReason(a.ProviderReason, "")
		}
	}
	if a.Cost != nil && (agyCost(a.Cost) || openCodeCost(a.Cost)) {
		e.Cost = a.Cost
	}
	for _, d := range j.Route {
		if d.Provider == a.Provider && d.Model == a.Model && d.Fingerprint == a.Fingerprint &&
			model.OpenCodeAccountID(d.OpenCodeAccount) == model.OpenCodeAccountID(a.OpenCodeAccount) &&
			model.OpenCodeTransportID(d.OpenCodeTransport) == model.OpenCodeTransportID(a.OpenCodeTransport) &&
			(d.QuotaRemaining != nil || d.ControllerRemaining != nil) {
			e.QuotaAtRoute = &RouteQuota{Bucket: d.QuotaBucket, Remaining: d.QuotaRemaining, ResetAt: d.QuotaResetAt,
				ControllerRemaining: d.ControllerRemaining}
			break
		}
	}
	return e
}

func completed(j store.JobEvidence, a model.Attempt) bool {
	return j.Status == model.Completed && j.Reason == "" && j.RunningAudited && j.CompletedAudited &&
		j.Task.DataClass == "synthetic" && digest.MatchString(j.SnapshotID) && j.Result != nil &&
		j.Result.Summary != "" && j.Result.SummarySHA256 == model.Hash([]byte(j.Result.Summary)) &&
		hasChecks(j.Result, "SCOPED_CONTEXT", "PROCESS_EXIT") &&
		a.Number > 0 && a.Number == len(j.Attempts) && a.Code == "" && !a.StartedAt.IsZero() &&
		a.FinishedAt != nil && !a.FinishedAt.Before(a.StartedAt) && a.FinishedAt.Before(j.DeadlineAt)
}

func hasChecks(r *model.Result, checks ...string) bool {
	for _, check := range checks {
		if !slices.Contains(r.Verification, check) {
			return false
		}
	}
	return true
}

func agyCost(c *model.CostEvidence) bool {
	return c != nil && c.Policy == "SUBSCRIPTION_ONLY" && c.Status == "DOCUMENTED_COST_PROTECTION" &&
		c.Documented == "VERIFIED" && c.RuntimeVerified == "UNKNOWN" && c.Account == "SAME_NATIVE_CLI_CONTEXT"
}

func openCodeCost(c *model.CostEvidence) bool {
	return c != nil && c.Policy == "FREE_ONLY" && c.Status == "ZERO_PRICE_CATALOG" &&
		c.Documented == "VERIFIED" && c.RuntimeVerified == "VERIFIED" && c.Account == "ANONYMOUS"
}

func openCodeRunVerified(a model.Attempt) bool {
	e := a.OpenCode
	return model.OpenCodeTransportID(a.OpenCodeTransport) == model.OpenCodeNativeReadAsk && e != nil &&
		e.Model == a.Model && e.PromptSent && e.GuardVerified && e.TerminalVerified && e.NativeFinish == "stop" &&
		e.InputTokens > 0 && e.OutputTokens > 0 && e.ReasoningTokens >= 0 && e.ReportedCost != nil && *e.ReportedCost == 0
}

func liveAttempt(j store.JobEvidence, a model.Attempt, c model.Capability) bool {
	if c.Status != "available" || c.Provider != a.Provider || c.Model != a.Model || c.Fingerprint != a.Fingerprint ||
		!completed(j, a) || hasChecks(j.Result, "FIXTURE_OUTPUT") {
		return false
	}
	switch a.Provider {
	case "agy":
		e := a.AGY
		return providers.AGYIdentityMatches(a.Model, a.Fingerprint) && hasChecks(j.Result, "AGY_PROFILE_VERIFIED", "DOCUMENTED_COST_PROTECTION") &&
			e != nil && e.Model == a.Model && e.PermissionMode == "request-review" && e.StartupVerified &&
			e.PromptSent && e.TerminalVerified && e.TotalTokens > 0 && e.SubscriptionQuotaVerified &&
			agyCost(a.Cost) && agyCost(e.Cost) && agyCost(j.Result.Cost)
	case "opencode":
		transport := model.OpenCodeTransportID(a.OpenCodeTransport)
		return providers.OpenCodeIdentityMatches(a.Model, a.Fingerprint) && j.Task.Mode == "read_only" &&
			transport == model.OpenCodeTransportID(c.OpenCodeTransport) &&
			(transport == model.OpenCodeStateless && a.OpenCode == nil || openCodeRunVerified(a)) &&
			model.OpenCodeAccountID(a.OpenCodeAccount) == model.OpenCodeAnonymous &&
			model.OpenCodeAccountID(c.OpenCodeAccount) == model.OpenCodeAnonymous &&
			hasChecks(j.Result, "TOOL_FREE_TEXT", "ZERO_PRICE_CATALOG") && openCodeCost(a.Cost) && openCodeCost(j.Result.Cost)
	default:
		return false
	}
}

func markerAccepted(j model.Job) bool {
	if j.Task.Mode != "read_only" || j.Result == nil || !smokeMarker.MatchString(j.Result.Summary) {
		return false
	}
	return slices.ContainsFunc(j.Task.Acceptance, func(c model.Check) bool {
		return c.ID == "readiness" && c.Description == "Return exactly "+j.Result.Summary
	})
}

func smokeRequested(j model.Job) bool {
	return j.Task.Mode == "read_only" && slices.ContainsFunc(j.Task.Acceptance, func(c model.Check) bool {
		const prefix = "Return exactly "
		return c.ID == "readiness" && strings.HasPrefix(c.Description, prefix) && smokeMarker.MatchString(strings.TrimPrefix(c.Description, prefix))
	})
}

func fixtureAttempt(j store.JobEvidence, a model.Attempt) bool {
	return completed(j, a) && slices.Contains([]string{"fixture-a", "fixture-b", "fixture-c"}, a.Provider) &&
		a.Model == "synthetic" && hasChecks(j.Result, "FIXTURE_OUTPUT")
}

func batchProof(dir string, parent store.JobEvidence, jobs map[string]store.JobEvidence, agy, opencode model.Capability, fixture bool) *Evidence {
	if parent.Status != model.Completed || !parent.CompletedAudited || !parent.Distinct || len(parent.Children) != 2 {
		return nil
	}
	source, receiver := jobs[parent.Children[0]], jobs[parent.Children[1]]
	if receiver.Task.Mode == "patch" {
		source, receiver = receiver, source
	}
	if !sameBatch(parent.Job, source.Job) || !sameBatch(parent.Job, receiver.Job) || source.Task.Mode != "patch" ||
		receiver.Task.Mode != "read_only" || len(source.Attempts) == 0 || len(receiver.Attempts) == 0 {
		return nil
	}
	a := source.Attempts[len(source.Attempts)-1]
	b := receiver.Attempts[len(receiver.Attempts)-1]
	if fixture {
		if a.Provider == b.Provider || !fixtureAttempt(source, a) || !fixtureAttempt(receiver, b) {
			return nil
		}
	} else if len(source.Attempts) != 1 || len(receiver.Attempts) != 1 || !liveAttempt(source, a, agy) || !liveAttempt(receiver, b, opencode) {
		return nil
	}
	if !verifiedManifest(source.Job, receiver.Job, a, b) {
		return nil
	}
	s, err := execution.LoadSnapshot(filepath.Join(dir, "snapshots"), parent.SnapshotID)
	if err != nil || s.BaseSHA != parent.BaseSHA {
		return nil
	}
	diff, err := execution.ReadVerifiedDiff(filepath.Join(dir, "artifacts"), source.Result, source.Result.ArtifactID, 64<<10)
	if err != nil || len(diff) == 0 {
		return nil
	}
	kind := "live"
	if fixture {
		kind = "fixture"
		if receiver.Result.Summary != "Received verified diff "+source.Result.ArtifactSHA256 {
			return nil
		}
	} else {
		matches := diffMarker.FindAllSubmatch(diff, 2)
		if len(matches) != 1 || receiver.Result.Summary != string(matches[0][1]) ||
			!markerDeclared(source.Task, receiver.Result.Summary) || !markerOnlyInDiff(s, receiver.Job, b.Handoffs[0]) {
			return nil
		}
		if m1RequestID.MatchString(parent.RequestID) && !exactM1Diff(string(diff), receiver.Result.Summary) {
			return nil
		}
	}
	children := []ChildEvidence{
		{JobID: source.ID, Provider: a.Provider, Model: a.Model, Fingerprint: a.Fingerprint, Attempt: a.Number},
		{JobID: receiver.ID, Provider: b.Provider, Model: b.Model, Fingerprint: b.Fingerprint, Attempt: b.Number},
	}
	if b.Provider == "opencode" {
		children[1].OpenCodeTransport = model.OpenCodeTransportID(b.OpenCodeTransport)
	}
	return &Evidence{Kind: kind, JobID: parent.ID, JobStatus: parent.Status, Children: children, SnapshotID: parent.SnapshotID,
		CompletedAudit: true, ArtifactSHA256: source.Result.ArtifactSHA256, SummarySHA256: receiver.Result.SummarySHA256, ObservedAt: b.FinishedAt}
}

func sameBatch(parent, child model.Job) bool {
	return child.Parent == parent.ID && child.RepoID == parent.RepoID && child.SnapshotID == parent.SnapshotID &&
		child.BaseSHA == parent.BaseSHA && child.PolicyDigest == parent.PolicyDigest
}

func verifiedManifest(source, receiver model.Job, a, b model.Attempt) bool {
	if !hasChecks(source.Result, "PATCH_SCOPE", "PATCH_APPLIES_TO_SNAPSHOT") || source.Result.ArtifactID != source.ID ||
		!digest.MatchString(source.Result.ArtifactSHA256) || len(source.Result.Preimages) == 0 ||
		len(source.Task.Acceptance) == 0 || len(receiver.Task.Acceptance) == 0 || !slices.Contains(receiver.DependsOn, source.ID) ||
		!slices.Equal(receiver.HandoffFrom, []model.HandoffFrom{{Key: source.Key, Kind: "diff"}}) ||
		len(b.Handoffs) != 1 || b.StartedAt.Before(*a.FinishedAt) {
		return false
	}
	m := b.Handoffs[0]
	ids := make([]string, 0, len(source.Task.Acceptance))
	for _, check := range source.Task.Acceptance {
		ids = append(ids, check.ID)
	}
	return m.Kind == "diff" && m.SourceJobID == source.ID && m.SourceAttempt == a.Number &&
		m.SnapshotID == source.SnapshotID && m.SHA256 == source.Result.ArtifactSHA256 &&
		m.AcceptanceSHA256 == model.Digest(source.Task.Acceptance) && slices.Equal(m.AcceptanceIDs, ids)
}

func markerOnlyInDiff(s execution.Snapshot, receiver model.Job, manifest model.HandoffManifest) bool {
	marker := receiver.Result.Summary
	if strings.Contains(receiver.Task.Objective, marker) || slices.ContainsFunc(receiver.Task.Acceptance, func(c model.Check) bool {
		return strings.Contains(c.ID, marker) || strings.Contains(c.Description, marker) || strings.Contains(c.CheckID, marker)
	}) || slices.ContainsFunc(manifest.AcceptanceIDs, func(id string) bool {
		return strings.Contains(id, marker)
	}) {
		return false
	}
	files, err := s.Context(receiver.Task.AllowedPaths)
	if err != nil {
		return false
	}
	for path, contents := range files {
		if strings.Contains(path, marker) || strings.Contains(contents, marker) {
			return false
		}
	}
	return true
}
