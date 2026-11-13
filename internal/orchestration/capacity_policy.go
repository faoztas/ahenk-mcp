// Copyright 2026 Furkan Anıl Öztaş
// SPDX-License-Identifier: Apache-2.0

package orchestration

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/faoztas/ahenk-mcp/internal/execution"
	"github.com/faoztas/ahenk-mcp/internal/model"
	"github.com/faoztas/ahenk-mcp/internal/providers"
)

const (
	weeklyMinutes    = 10080
	reserveReason    = "AGY_WEEKLY_RESERVE_PROTECTED"
	quotaHistoryFile = "agy-quota-history.json"
)

func routingRole(t model.Task) string {
	switch {
	case t.Mode == "patch" && (t.ExecutionProfile == model.TrustedLocalProfile ||
		t.ExecutionProfile == "implementation" || t.TaskClass == "implementation"):
		return "implementation"
	case t.ExecutionProfile == "review":
		return "review"
	}
	switch t.TaskClass {
	case "architecture", "planning", "review", "reasoning", "research", "implementation":
		return t.TaskClass
	default:
		return "inference"
	}
}

func routingMode(strategy string, policy model.CapacityPolicy) string {
	switch strategy {
	case "quality_first", "balanced", "conserve":
		return strategy
	case "quota_saving":
		return "conserve"
	default:
		return policy.Defaults().Mode
	}
}

func jobQualityRole(j model.Job) string {
	if j.QualityRole != "" {
		return j.QualityRole
	}
	// Older jobs used review as the default label for unclassified reads.
	if j.Task.TaskClass == "review" && j.Task.ExecutionProfile != "review" {
		j.Task.TaskClass = ""
	}
	return routingRole(j.Task)
}

func qualityFor(c model.Capability, role string) model.QualityEvidence {
	best := model.QualityEvidence{Role: role}
	for _, q := range c.Quality {
		if q.Role == role && q.Tier > best.Tier {
			best = q
		}
	}
	if c.Mode == "fixture" && slices.Contains([]string{"inference", "review", "implementation"}, role) ||
		c.Mode == "" && role == "inference" && len(c.Quality) == 0 {
		return model.QualityEvidence{Role: role, Tier: 1, Source: "OFFLINE_FIXTURE"}
	}
	if (c.Provider == "codex" || c.Provider == "claude") && len(c.Quality) == 0 {
		return model.QualityEvidence{Role: role, Tier: 1, Source: "CLI_RUNNER"}
	}
	return best
}

func workerCostAllowed(c model.Capability) bool {
	if c.Mode == "fixture" || c.Mode == "" && c.Cost == nil {
		return true
	}
	if c.Provider == "codex" || c.Provider == "claude" {
		return true
	}
	if c.Cost == nil {
		return false
	}
	switch c.Provider {
	case "agy":
		return c.Cost.Policy == "SUBSCRIPTION_ONLY" && providers.AGYModelFamily(c.Model) != "" &&
			(providers.AGYWorkerIdentityMatches(c.Model, c.Fingerprint) ||
				c.AuditValidUntil != nil && providers.AGYIdentityMatches("gemini-3.8-flash-medium", c.Fingerprint))
	case "opencode":
		return c.Cost.Policy == "FREE_ONLY" &&
			(c.Cost.Status == "CATALOG_PREFLIGHT_REQUIRED" || c.Cost.Status == "ZERO_PRICE_CATALOG") && model.OpenCodeAccountID(c.OpenCodeAccount) == model.OpenCodeAnonymous &&
			providers.OpenCodeIdentityMatches(c.Model, c.Fingerprint)
	default:
		return false
	}
}

func verifiedQuota(q providers.Quota) bool {
	return q.AccountMatch == "MATCHED" && q.Unit == "percent" && q.Remaining != nil &&
		*q.Remaining >= 0 && *q.Remaining <= 100 && (q.State == "KNOWN" || q.State == "EXHAUSTED")
}

func controllerWindow(rows []providers.Quota, minutes int, now time.Time) providers.Quota {
	q := providers.UnknownQuota("codex")
	q.Bucket, q.WindowMinutes = "weekly", minutes
	if minutes == 300 {
		q.Bucket = "five_hour"
	}
	for _, raw := range rows {
		// Extra model windows are exposed separately. The controller's active
		// model is not known, so they cannot stand in for account windows.
		if raw.WindowMinutes != minutes || strings.HasPrefix(raw.Bucket, "extra:") {
			continue
		}
		row := raw.At(now)
		if q.ObservedAt.IsZero() || verifiedQuota(row) &&
			(!verifiedQuota(q) || *row.Remaining < *q.Remaining) {
			q = row
		}
	}
	return q
}

func quotaState(q providers.Quota, policy model.CapacityPolicy) string {
	switch {
	case q.State == "STALE":
		return "STALE"
	case !verifiedQuota(q):
		return "UNKNOWN"
	case *q.Remaining == 0:
		return "EXHAUSTED"
	case *q.Remaining <= policy.CodexCritical:
		return "CRITICAL"
	case *q.Remaining <= policy.CodexLow:
		return "LOW"
	default:
		return "HEALTHY"
	}
}

type quotaHistory struct {
	Binding string            `json:"binding"`
	Rows    []providers.Quota `json:"rows"`
}

func (e *Engine) quotaHistoryBinding() string {
	if binding := e.cfg.CodexBarAccountSHA256["agy"]; binding != "" {
		return model.Digest([]string{"agy-capacity-history-v1", e.db.Dir, binding})
	}
	return ""
}

func (e *Engine) loadQuotaHistory() {
	binding := e.quotaHistoryBinding()
	if binding == "" {
		return
	}
	path := filepath.Join(e.db.Dir, quotaHistoryFile)
	st, err := os.Lstat(path)
	if err != nil || !st.Mode().IsRegular() || st.Mode().Perm()&0077 != 0 || st.Size() > 64<<10 {
		return
	}
	data, err := os.ReadFile(path)
	var history quotaHistory
	if err == nil && json.Unmarshal(data, &history) == nil && history.Binding == binding && len(history.Rows) <= 40 {
		e.agyHistory = history.Rows
	}
}

// rememberQuota runs under the scheduler lock. Only a fresh, matched account
// sample enters private pacing history. Repeated observations add no entries.
func (e *Engine) rememberQuota(provider string, rows []providers.Quota, now time.Time) {
	e.quotas[provider] = rows
	if provider != "agy" {
		return
	}
	changed := false
	for _, raw := range rows {
		q := raw.At(now)
		if !verifiedQuota(q) || q.Family == "" || q.ResetAt == nil {
			continue
		}
		if slices.ContainsFunc(e.agyHistory, func(p providers.Quota) bool { return p.Bucket == q.Bucket && p.ObservedAt.Equal(q.ObservedAt) }) {
			continue
		}
		e.agyHistory = append(e.agyHistory, q)
		changed = true
	}
	if !changed {
		return
	}
	slices.SortStableFunc(e.agyHistory, func(a, b providers.Quota) int { return a.ObservedAt.Compare(b.ObservedAt) })
	e.agyHistory = slices.Clone(e.agyHistory[max(0, len(e.agyHistory)-40):])
	if e.db != nil {
		if binding := e.quotaHistoryBinding(); binding != "" {
			// Routing remains conservative if history cannot be stored.
			_ = execution.SaveJSON(e.db.Dir, quotaHistoryFile, quotaHistory{binding, e.agyHistory})
		}
	}
}

// burn returns the average percentage points per hour for this exact reset
// window. Its units are local to the bucket and never compared with Codex.
func (e *Engine) burn(q providers.Quota) float64 {
	if !verifiedQuota(q) || q.ResetAt == nil {
		return 0
	}
	for _, prev := range e.agyHistory {
		age := q.ObservedAt.Sub(prev.ObservedAt)
		if age < 4*time.Minute || age > time.Hour || prev.Bucket != q.Bucket || !verifiedQuota(prev) ||
			prev.ResetAt == nil || !prev.ResetAt.Equal(*q.ResetAt) {
			continue
		}
		return max(0, (*prev.Remaining-*q.Remaining)/age.Hours())
	}
	return 0
}

type reserveAdmission struct {
	weekly                   providers.Quota
	threshold, floor, buffer float64
	override                 bool
	reason                   string
}

func (e *Engine) agyReserve(j model.Job, family string, now time.Time, budget time.Duration, controller providers.Quota) reserveAdmission {
	policy := e.cfg.CapacityPolicy.Defaults()
	r := reserveAdmission{threshold: policy.AGYWeeklyReserve, weekly: tightestQuota(e.quotas["agy"], family, weeklyMinutes, now)}
	if r.weekly.Bucket == "" {
		r.weekly = providers.UnknownQuota("agy")
		r.weekly.Family, r.weekly.WindowMinutes = family, weeklyMinutes
		for _, bucket := range providers.AGYFamilyBuckets(family) {
			if strings.HasSuffix(bucket, "weekly") {
				r.weekly.Bucket = bucket
			}
		}
	}
	// An unknown controller signal keeps the reserve in place. Only a
	// verified empty controller weekly window releases the weekly policy.
	r.override = j.Task.AGYReserveOverride != "" && e.cfg.Repositories[j.RepoID].AllowAGYReserveOverride
	rate := e.burn(r.weekly)
	r.buffer = max(2, budget.Minutes()/2, rate*budget.Hours())
	if r.override || verifiedQuota(controller) && *controller.Remaining == 0 {
		return r
	}
	since := r.weekly.ObservedAt
	if since.IsZero() {
		since = now.Add(-5 * time.Minute)
	}
	r.floor = policy.AGYWeeklyReserve + r.buffer
	for _, job := range e.jobs {
		for _, a := range job.Attempts {
			if a.Provider != "agy" || providers.AGYModelFamily(a.Model) != family {
				continue
			}
			overlapsSample := !a.StartedAt.Before(since) || a.FinishedAt != nil && a.FinishedAt.After(since) ||
				a.FinishedAt == nil && e.active[job.ID] != nil && a.DeadlineAt.After(since)
			if overlapsSample && !a.StartedAt.After(now) && (a.AGY == nil || a.AGY.PromptSent) {
				r.floor += a.AGYWeeklyBuffer
			}
			if a.AGY != nil && a.AGY.WeeklyReserve != nil {
				ev := a.AGY.WeeklyReserve
				if ev.ObservedAt.After(r.weekly.ObservedAt) && !ev.ObservedAt.After(now) &&
					ev.ResetAt != nil && ev.ResetAt.After(now) && now.Sub(ev.ObservedAt) <= 5*time.Minute {
					r.weekly = providers.Quota{Provider: "agy", Family: family, Bucket: ev.Bucket, WindowMinutes: weeklyMinutes,
						Unit: "percent", State: "KNOWN", Remaining: &ev.Remaining, ObservedAt: ev.ObservedAt,
						ResetAt: ev.ResetAt, Source: "agy_usage", AccountMatch: "SAME_NATIVE_CLI_CONTEXT"}
				}
			}
		}
	}
	if r.weekly.Remaining == nil || *r.weekly.Remaining == 0 {
		return r
	}
	if *r.weekly.Remaining <= r.floor {
		r.reason = reserveReason
		return r
	}
	// Discretionary work yields when either same-family window suggests a
	// trajectory beyond the weekly reserve. Important work still passes
	// the absolute reserve check above.
	session := tightestQuota(e.quotas["agy"], family, 300, now)
	rate = max(rate, e.burn(session))
	if verifiedQuota(controller) && j.Task.Priority != "high" &&
		(j.Task.Priority == "background" || j.Task.TaskClass == "utility") &&
		r.weekly.ResetAt != nil && rate*r.weekly.ResetAt.Sub(now).Hours() >= *r.weekly.Remaining-r.floor {
		r.reason = "AGY_WEEKLY_PACING"
	}
	return r
}
