// Copyright 2026 Furkan Anıl Öztaş
// SPDX-License-Identifier: Apache-2.0

package orchestration

import (
	"math"
	"testing"
	"time"

	"github.com/faoztas/ahenk-mcp/internal/providers"
)

func TestComputeHeadroomHealthy(t *testing.T) {
	session := providers.Quota{Remaining: pointerQuota(80)}
	weekly := providers.Quota{Remaining: pointerQuota(90)}
	h := ComputeHeadroom(&session, &weekly)
	if h.SessionMargin != 0.80 || h.WeeklyMargin != 0.90 ||
		math.Abs(h.ProviderMargin-0.80) > 1e-9 || h.Effective != h.ProviderMargin ||
		h.State != "HEALTHY" || h.CriticalWarning {
		t.Fatalf("wrong healthy headroom: %+v", h)
	}
}

func TestComputeHeadroomWeeklyOnly(t *testing.T) {
	weekly := providers.Quota{Remaining: pointerQuota(52)}
	h := ComputeHeadroom(nil, &weekly)
	if h.SessionMargin != 1.0 || h.ProviderMargin != 0.52 || h.Effective != 0.52 || h.State != "HEALTHY" {
		t.Fatalf("wrong weekly-only headroom: %+v", h)
	}
}

func TestComputeHeadroomQuadraticPenalty(t *testing.T) {
	session := providers.Quota{Remaining: pointerQuota(10)}
	weekly := providers.Quota{Remaining: pointerQuota(20)}
	h := ComputeHeadroom(&session, &weekly)
	// H_provider = min(0.10, 0.60*0.20 + 0.40*0.10) = min(0.10, 0.16) = 0.10
	// H_effective = 0.10 * (0.10 / 0.15)
	want := 0.10 * (0.10 / 0.15)
	if math.Abs(h.ProviderMargin-0.10) > 1e-9 || h.State != "DEGRADED" ||
		math.Abs(h.Effective-want) > 1e-9 || !h.CriticalWarning {
		t.Fatalf("wrong degraded headroom: %+v (want effective %v)", h, want)
	}
}

func TestComputeHeadroomCriticalThreshold(t *testing.T) {
	session := providers.Quota{Remaining: pointerQuota(4)}
	h := ComputeHeadroom(&session, nil)
	if h.ProviderMargin != 0.04 || h.Effective != 0.0 || h.State != "CRITICAL" || !h.CriticalWarning {
		t.Fatalf("wrong critical headroom: %+v", h)
	}
}

func TestComputeHeadroomUnknown(t *testing.T) {
	h := ComputeHeadroom(nil, nil)
	if h.ProviderMargin != 1.0 || h.Effective != 1.0 || h.State != "HEALTHY" {
		t.Fatalf("unknown quota must default to full headroom: %+v", h)
	}
}

func TestFamilyHeadroomPicksTightestWindows(t *testing.T) {
	now := time.Now()
	zero, spare, low := 0.0, 75.0, 12.0
	e := Engine{quotas: map[string][]providers.Quota{"agy": {
		{Family: "gemini", Bucket: "gemini-5h", Unit: "percent", AccountMatch: "MATCHED", State: "EXHAUSTED", Remaining: &zero, ObservedAt: now},
		{Family: "gemini", Bucket: "gemini-weekly", Unit: "percent", AccountMatch: "MATCHED", State: "KNOWN", Remaining: &spare, ObservedAt: now},
		{Family: "gemini_argon", Bucket: "gemini-argon-wtu-weekly", Unit: "percent", AccountMatch: "MATCHED", State: "KNOWN", Remaining: &low, ObservedAt: now},
	}}}
	h, ok := e.familyHeadroom("gemini", now)
	if !ok || h.SessionMargin != 0.0 || h.WeeklyMargin != 0.75 || h.Effective != 0.0 || h.State != "CRITICAL" {
		t.Fatalf("wrong gemini family headroom: %+v ok=%v", h, ok)
	}
	h, ok = e.familyHeadroom("gemini_argon", now)
	if !ok || h.SessionMargin != 1.0 || h.ProviderMargin != 0.12 || h.State != "DEGRADED" {
		t.Fatalf("wrong argon family headroom: %+v ok=%v", h, ok)
	}
	if _, ok := e.familyHeadroom("missing", now); ok {
		t.Fatal("unknown family reported headroom")
	}
}

func pointerQuota(v float64) *float64 { return &v }
