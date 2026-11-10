// Copyright 2026 Furkan Anıl Öztaş
// SPDX-License-Identifier: Apache-2.0

package providers

import (
	"slices"
	"time"

	"github.com/faoztas/ahenk-mcp/internal/model"
)

func (r *Registry) addAuditedModels(inventory *model.ModelInventory, now time.Time) {
	if inventory == nil || !inventory.Fresh(now) {
		return
	}
	expiry := inventory.ObservedAt.Add(24 * time.Hour)
	baseline := slices.Clone(r.Candidates)
	openCodeApproved := slices.ContainsFunc(baseline, func(c model.Capability) bool {
		return c.Provider == "opencode" && c.Status == "available" && OpenCodeIdentityMatches(c.Model, c.Fingerprint)
	})
	for _, row := range inventory.Models {
		if row.Inference != "PASS" || !row.Terminal || row.Actual != row.Model {
			continue
		}
		c := model.Capability{Provider: row.Provider, Model: row.Model, Fingerprint: row.Binary, Status: "available",
			Reason: "AUDIT_VERIFIED_READ_ONLY", AuditValidUntil: &expiry, AuditLatencyMS: row.LatencyMS}
		switch row.Provider {
		case "agy":
			if !r.agy.EnableSynthetic || r.agy.ExpectedBinarySHA256 != row.Binary || row.Binary != agy153Fingerprint ||
				AGYModelFamily(row.Model) == "" || !model.ValidID(row.Model) || row.Cost != "SUBSCRIPTION_PRECHECK" ||
				row.Interface != "headless_json" {
				continue
			}
			c.Mode = "headless"
			c.Profiles = []model.ExecutionProfile{
				{Name: "restricted", Status: "preflight_required"},
				{Name: "analysis", Status: "preflight_required"},
			}
		case "opencode":
			if !openCodeApproved || !OpenCodeIdentityMatches(row.Model, row.Binary) || row.Interface != "chat" ||
				row.RuntimeCost != "ZERO_REPORTED" ||
				(row.Cost != "VERIFIED_FREE_INSTALLED" && row.Cost != "VERIFIED_FREE_PUBLIC_PIN") {
				continue
			}
			c.Mode, c.OpenCodeTransport, c.OpenCodeAccount = "tool_free", model.OpenCodeNativeReadAsk, model.OpenCodeAnonymous
		default:
			continue
		}
		if !slices.ContainsFunc(r.Candidates, func(existing model.Capability) bool {
			return existing.Provider == c.Provider && existing.Model == c.Model &&
				(existing.Provider != "opencode" || model.OpenCodeTransportID(existing.OpenCodeTransport) == c.OpenCodeTransport)
		}) {
			r.Candidates = append(r.Candidates, c)
		}
	}
}

func AuditRouteAllows(c model.Capability, t model.Task, now time.Time) bool {
	return c.AuditValidUntil == nil || now.Before(*c.AuditValidUntil) && t.Mode == "read_only" &&
		t.DataClass == "synthetic" && t.Risk != "critical" &&
		slices.Contains([]string{"", "analysis", "restricted"}, t.ExecutionProfile)
}
