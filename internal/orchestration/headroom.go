// Copyright 2026 Furkan Anıl Öztaş
// SPDX-License-Identifier: Apache-2.0

package orchestration

import (
	"strings"
	"time"

	"github.com/faoztas/ahenk-mcp/internal/model"
	"github.com/faoztas/ahenk-mcp/internal/providers"
)

// Headroom thresholds. Below the critical threshold a window may not receive
// work at all; below the degraded threshold it pays a quadratic reserve
// penalty so near-exhausted windows lose routing priority.
const (
	quotaCriticalHeadroom = 0.05
	quotaDegradedHeadroom = 0.15
	headroomHealthy       = "HEALTHY"
	headroomDegraded      = "DEGRADED"
	headroomCritical      = "CRITICAL"
)

// HeadroomResult represents the normalized headroom assessment of a provider
// or model family. All margins are in [0.0, 1.0].
type HeadroomResult struct {
	Family          string     `json:"family"`
	SessionMargin   float64    `json:"session_margin"`   // H_5h
	WeeklyMargin    float64    `json:"weekly_margin"`    // H_weekly
	ProviderMargin  float64    `json:"provider_margin"`  // H_provider
	Effective       float64    `json:"effective"`        // H_effective
	State           string     `json:"state"`            // HEALTHY, DEGRADED, CRITICAL
	CriticalWarning bool       `json:"critical_warning"` // true if H_effective < 0.15
	ResetsAt        *time.Time `json:"resets_at,omitempty"`
}

// ComputeHeadroom blends the short- and long-lived quota windows of one
// family into a single normalized margin:
//
//	H_provider  = min(H_5h, 0.60*H_weekly + 0.40*H_5h)      (both windows known)
//	H_effective = 0                                          H_provider <  0.05
//	            = H_provider * (H_provider / 0.15)           0.05 <= H < 0.15
//	            = H_provider                                 H_provider >= 0.15
//
// Unknown windows default to full headroom so unverified quota never blocks
// a route by itself; the separate CAPACITY_UNVERIFIED gate covers that case.
func ComputeHeadroom(sessionWindow, weeklyWindow *providers.Quota) HeadroomResult {
	hSession, hWeekly := 1.0, 1.0
	hasSession, hasWeekly := false, false
	var reset *time.Time
	margin := func(w *providers.Quota) (float64, bool) {
		if w == nil || w.Remaining == nil {
			return 0, false
		}
		v := max(0.0, min(100.0, *w.Remaining)) / 100.0
		return v, true
	}
	if v, ok := margin(sessionWindow); ok {
		hSession, hasSession = v, true
		reset = earlierReset(reset, sessionWindow.ResetAt, time.Now().UTC())
	}
	if v, ok := margin(weeklyWindow); ok {
		hWeekly, hasWeekly = v, true
		reset = earlierReset(reset, weeklyWindow.ResetAt, time.Now().UTC())
	}
	hProvider := 1.0
	switch {
	case hasSession && hasWeekly:
		hProvider = min(hSession, 0.60*hWeekly+0.40*hSession)
	case hasSession:
		hProvider = hSession
	case hasWeekly:
		hProvider = hWeekly
	}
	hEffective := hProvider
	state := headroomHealthy
	switch {
	case hProvider < quotaCriticalHeadroom:
		hEffective, state = 0.0, headroomCritical
	case hProvider < quotaDegradedHeadroom:
		hEffective, state = hProvider*(hProvider/quotaDegradedHeadroom), headroomDegraded
	}
	return HeadroomResult{
		SessionMargin:   hSession,
		WeeklyMargin:    hWeekly,
		ProviderMargin:  hProvider,
		Effective:       hEffective,
		State:           state,
		CriticalWarning: hEffective < quotaDegradedHeadroom,
		ResetsAt:        reset,
	}
}

// effectiveTargetTier returns the explicit F/P routing tier of a task.
// "auto" and unknown values behave like the default (no explicit tier).
func effectiveTargetTier(t model.Task) string {
	switch t.TargetTier {
	case "free", "cheap", "balanced", "hard", "frontier":
		return t.TargetTier
	default:
		return ""
	}
}

// fpWeights returns the price/performance score weights for a target tier:
// headroom, quality and cost. Auto routing keeps quality dominant so the
// removal of the rigid QUALITY_TIER_BELOW_BEST lock does not silently
// downgrade evidence-backed routes; it only turns the old hard elimination
// into a soft, quota-aware preference.
func fpWeights(tier string) (wHeadroom, wQuality, wCost float64) {
	switch tier {
	case "free", "cheap":
		return 0.55, 0.20, 0.25
	case "hard", "frontier":
		return 0.20, 0.75, 0.05
	case "balanced":
		return 0.40, 0.35, 0.15
	default:
		return 0.40, 0.75, 0.05
	}
}

// fpCost normalizes the documented cost class of a route to [0, 1].
func fpCost(c model.Capability) float64 {
	switch providers.FPTier(c.Provider, c.Model) {
	case "free":
		return 0
	case "cheap":
		return 0.25
	case "balanced":
		return 0.6
	case "hard":
		return 1
	default:
		return 0.5
	}
}

// familyHeadroom blends the tightest observed 5h and weekly windows of one
// AGY model family. It reports false when no fresh matched window exists.
func (e *Engine) familyHeadroom(family string, now time.Time) (HeadroomResult, bool) {
	if family == "" {
		return HeadroomResult{}, false
	}
	session, weekly := providers.Quota{}, providers.Quota{}
	for _, q := range e.quotas["agy"] {
		if q.Family != family || q.AccountMatch != "MATCHED" {
			continue
		}
		q = q.At(now)
		if q.Unit != "percent" || q.State != "KNOWN" && q.State != "EXHAUSTED" || q.Remaining == nil {
			continue
		}
		tighter := func(current providers.Quota) bool {
			return current.Remaining == nil || *q.Remaining < *current.Remaining
		}
		switch {
		case q.WindowMinutes == 300 || strings.HasSuffix(q.Bucket, "5h"):
			if tighter(session) {
				session = q
			}
		case q.WindowMinutes == weeklyMinutes || strings.HasSuffix(q.Bucket, "weekly"):
			if tighter(weekly) {
				weekly = q
			}
		}
	}
	if session.Remaining == nil && weekly.Remaining == nil {
		return HeadroomResult{}, false
	}
	h := ComputeHeadroom(&session, &weekly)
	h.Family = family
	return h, true
}
