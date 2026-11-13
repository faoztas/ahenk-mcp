// Copyright 2026 Furkan Anıl Öztaş
// SPDX-License-Identifier: Apache-2.0

package orchestration

import (
	"testing"
	"time"
)

func refreshQuotas(e *Engine) {
	fresh := time.Now().UTC()
	for i := range e.quotas["agy"] {
		e.quotas["agy"][i].ObservedAt = fresh
		e.quotas["agy"][i].ResetAt = timePointer(fresh.Add(time.Duration(e.quotas["agy"][i].WindowMinutes) * time.Minute))
	}
	for i := range e.quotas["codex"] {
		e.quotas["codex"][i].ObservedAt = fresh
		e.quotas["codex"][i].ResetAt = timePointer(fresh.Add(time.Duration(e.quotas["codex"][i].WindowMinutes) * time.Minute))
	}
}

func TestCapacitySnapshotReportsFamilyHeadroom(t *testing.T) {
	now := capacityClock
	e, _ := capacityFixture(now)
	refreshQuotas(e)
	snapshot, _, _ := e.Capacity("demo")
	var gemini *HeadroomResult
	for i := range snapshot.Headroom {
		if snapshot.Headroom[i].Family == "gemini" {
			gemini = &snapshot.Headroom[i]
		}
	}
	if gemini == nil || gemini.ProviderMargin != 0.80 || gemini.Effective != 0.80 || gemini.State != "HEALTHY" {
		t.Fatalf("gemini family headroom missing from the snapshot: %+v", snapshot.Headroom)
	}
	var codex *HeadroomResult
	for i := range snapshot.Headroom {
		if snapshot.Headroom[i].Family == "codex" {
			codex = &snapshot.Headroom[i]
		}
	}
	if codex == nil || codex.SessionMargin != 0.60 || codex.WeeklyMargin != 0.70 {
		t.Fatalf("codex controller headroom missing: %+v", snapshot.Headroom)
	}
}

func TestCapacityHeadroomWarnings(t *testing.T) {
	e, _ := capacityFixture(capacityClock)
	fresh := time.Now().UTC()
	degraded := 10.0
	e.quotas["agy"][0] = quotaFixture("agy", "gemini-5h", "gemini", 300, degraded, fresh)
	e.quotas["agy"][1] = quotaFixture("agy", "gemini-weekly", "gemini", weeklyMinutes, degraded, fresh)
	_, warnings, _ := e.Capacity("demo")
	w := requireWarning(t, warnings, "HEADROOM_DEGRADED")
	if w.Provider != "agy" || w.Scope != "gemini" || w.Severity != "warning" {
		t.Fatalf("wrong degraded headroom warning: %+v", w)
	}

	critical := 3.0
	e.quotas["agy"][0] = quotaFixture("agy", "gemini-5h", "gemini", 300, critical, fresh)
	e.quotas["agy"][1] = quotaFixture("agy", "gemini-weekly", "gemini", weeklyMinutes, critical, fresh)
	_, warnings, _ = e.Capacity("demo")
	w = requireWarning(t, warnings, "QUOTA_CRITICAL_HEADROOM")
	if w.Provider != "agy" || w.Scope != "gemini" || w.Severity != "critical" {
		t.Fatalf("wrong critical headroom warning: %+v", w)
	}
}
