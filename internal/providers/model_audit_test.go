// Copyright 2026 Furkan Anıl Öztaş
// SPDX-License-Identifier: Apache-2.0

package providers

import (
	"testing"
	"time"

	"github.com/faoztas/ahenk-mcp/internal/model"
)

func TestPublicAuditIncludesFreeAliasAndDecision(t *testing.T) {
	data := []byte(`{"opencode":{"models":{
	  "big-pickle":{"id":"big-pickle","type":"chat","limit":{"context":200000},"capabilities":{"tool_call":true},"cost":{"text":{"input":{"price":0},"output":{"price":0}}}},
	  "jev-free":{"id":"jev-free","type":"decision","cost":{"text":{"input":{"price":0},"output":{"price":0}}}},
	  "paid":{"id":"paid","type":"chat","cost":{"text":{"input":{"price":1},"output":{"price":2}}}}
	}}}`)
	rows := openCodeAuditRows(nil, publicAuditModels(data))
	if len(rows) != 2 || rows[0].Model != "opencode/big-pickle" || rows[0].Inference != "MODEL_UNAVAILABLE" ||
		rows[0].Context != 200000 || !rows[0].ToolMetadata || rows[1].Inference != "UNSUPPORTED_INTERFACE" {
		t.Fatalf("unexpected audit inventory: %+v", rows)
	}
}

func TestAuditedEvidenceAdmitsOnlyFreshReadOnlyRoutes(t *testing.T) {
	now := time.Now().UTC()
	reg := Registry{agy: model.ProviderConfig{EnableSynthetic: true, ExpectedBinarySHA256: agy153Fingerprint}}
	row := model.ModelAudit{Provider: "agy", Model: "gemini-4-argon-low", Actual: "gemini-4-argon-low", Binary: agy153Fingerprint,
		Inference: "PASS", Cost: "SUBSCRIPTION_PRECHECK", Interface: "headless_json", Terminal: true}
	reg.addAuditedModels(&model.ModelInventory{Fixture: model.ModelAuditFixture, ObservedAt: now, Models: []model.ModelAudit{row}}, now)
	if len(reg.Candidates) != 1 {
		t.Fatal("audited read-only model was not added")
	}
	c := reg.Candidates[0]
	task := model.Task{Mode: "read_only", DataClass: "synthetic"}
	if !AuditRouteAllows(c, task, now) || AuditRouteAllows(c, task, now.Add(24*time.Hour)) || AllowsHandoff(c, task) || AllowsTrustedLocal(c) {
		t.Fatal("audit evidence escaped its scope or freshness gate")
	}
	task.ExecutionProfile = "review"
	if AuditRouteAllows(c, task, now) {
		t.Fatal("text-only inference promoted an unaudited reviewer")
	}
}

func TestNativeContinuationRejectsChangedHistory(t *testing.T) {
	first := []openCodeNativeMessage{{ID: "first", Type: "user", Text: "synthetic"},
		{ID: "reply", Type: "assistant"}, {Type: "idle", Outcome: "succeeded"}}
	h := new(openCodeTurnHistory)
	if err := h.pin(first); err != nil {
		t.Fatal(err)
	}
	next := append(append([]openCodeNativeMessage(nil), first...), openCodeNativeMessage{ID: "second", Type: "user"})
	turn, err := h.next(next)
	if err != nil || len(turn) != 1 || turn[0].ID != "second" {
		t.Fatalf("verified continuation history was rejected: %v %v", turn, err)
	}
	next[0].Text = "changed"
	if _, err := h.next(next); err == nil {
		t.Fatal("changed prior turn accepted")
	}
}
