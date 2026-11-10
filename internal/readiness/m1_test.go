// Copyright 2026 Furkan Anıl Öztaş
// SPDX-License-Identifier: Apache-2.0

package readiness

import (
	"crypto/rand"
	"strings"
	"testing"
	"time"

	"github.com/faoztas/ahenk-mcp/internal/model"
)

func TestM1RequestConcealsChallengeFromReceiver(t *testing.T) {
	marker := "ahenk-handoff-" + rand.Text()
	request := m1Batch("m1", "m1-"+rand.Text(), marker)
	got, err := validateM1Request(request, "parent")
	if err != nil || got != marker {
		t.Fatalf("prepared request rejected: %v", err)
	}
	if !markerDeclared(request.Items[0].Task, marker) || markerDeclared(request.Items[1].Task, marker) ||
		strings.Contains(request.Items[1].Objective, marker) || request.Items[0].PreferredProvider != "" || request.Items[1].PreferredProvider != "" ||
		request.Items[0].PreferredModel != "" || request.Items[1].PreferredModel != "" {
		t.Fatal("receiver learned the challenge or automatic routing was overridden")
	}
	request.Items[1].Acceptance[0].Description = "Return " + marker
	if _, err := validateM1Request(request, "parent"); err == nil {
		t.Fatal("altered request disclosed the answer")
	}
}

func TestM1CanUseExactModelOnIndependentTransport(t *testing.T) {
	now := time.Now()
	start, finish := now.Add(-2*time.Second), now.Add(-time.Second)
	c := model.Capability{Provider: "opencode", Model: "opencode/glm-5.3-flash-free", OpenCodeTransport: model.OpenCodeNativeReadAsk}
	verified := Gate{Ready: true, Observation: &Evidence{Model: c.Model, OpenCodeTransport: c.OpenCodeTransport,
		ObservedAt: &start, FinishedAt: &finish}}
	history := FreeGate{Model: c.Model, Scope: "model", Reason: "RATE_LIMIT", ObservedAt: now.Add(-time.Minute),
		RetryAt: now.Add(time.Hour), OpenCodeTransport: model.OpenCodeStateless}
	report := Report{AGY: verified}
	ready := openCodeModelReport(c, verified, []FreeGate{history}, now)
	if ready.Recovery != "RECORDED_GATE_OTHER_TRANSPORT" || ready.ActiveGate != nil {
		t.Fatalf("legacy rate limit widened to the native transport: %+v", ready)
	}
	if err := requireProviderSmokes(report.withOpenCode(ready), now); err != nil {
		t.Fatalf("verified independent native route was blocked: %v", err)
	}
	for _, scope := range []string{"model", "pool", "account"} {
		asserted := history
		asserted.Scope, asserted.ProviderScope = scope, scope
		gated := openCodeModelReport(c, verified, []FreeGate{history, asserted}, now)
		if gated.ActiveGate == nil || requireProviderSmokes(report.withOpenCode(gated), now) == nil {
			t.Errorf("explicit provider scope %q was bypassed: %+v", scope, gated)
		}
	}
}

func TestM1DiffMustOnlyAddExactMarker(t *testing.T) {
	marker := "ahenk-handoff-" + rand.Text()
	diff := "--- a/m1-marker.txt\n+++ b/m1-marker.txt\n@@ -1 +1,2 @@\n AHENK_M1_SEED\n+AHENK_HANDOFF_MARKER=" + marker + "\n"
	if !exactM1Diff(diff, marker) || !exactM1Diff("diff --git a/m1-marker.txt b/m1-marker.txt\nindex 1234567..abcdef0 100644\n"+diff, marker) {
		t.Fatal("valid marker diff was rejected")
	}
	for _, invalid := range []string{
		diff + "+other edit\n",
		strings.ReplaceAll(diff, marker, "ahenk-handoff-"+rand.Text()),
		strings.ReplaceAll(diff, "m1-marker.txt", "other.txt"),
		"diff --git a/m1-marker.txt b/m1-marker.txt\nold mode 100644\nnew mode 100755\n" + diff,
	} {
		if exactM1Diff(invalid, marker) {
			t.Fatal("altered diff was accepted")
		}
	}
}

func TestM1RequiresElapsedCooldownAndBothEarlierSmokes(t *testing.T) {
	now := time.Now()
	past := now.Add(-time.Second)
	future := now.Add(time.Second)
	verified := Gate{Ready: true, Observation: &Evidence{ObservedAt: &past, FinishedAt: &past}}
	report := Report{AGY: verified, OpenCode: verified}
	if err := requireProviderSmokes(report, now); err == nil {
		t.Fatal("M1 treated a missing cooldown record as elapsed")
	}
	report.OpenCodeRecovery = "RECORDED_GATE_ELAPSED"
	report.OpenCodeRecoveryGate = &FreeGate{RetryAt: past}
	if err := requireProviderSmokes(report, now); err != nil {
		t.Fatal(err)
	}
	report.OpenCodeRecoveryGate.RetryAt = now
	if err := requireProviderSmokes(report, now); err == nil {
		t.Fatal("M1 accepted an OpenCode smoke that predated cooldown expiry")
	}
	report.OpenCodeRecoveryGate.RetryAt = past
	report.OpenCode.Observation = &Evidence{FinishedAt: &future}
	if err := requireProviderSmokes(report, now); err == nil {
		t.Fatal("M1 accepted a smoke that completed after batch creation")
	}
	report.OpenCode = Gate{Ready: false, Observation: &Evidence{Kind: "fixture", JobStatus: model.Completed, FinishedAt: &past}}
	if err := requireProviderSmokes(report, now); err == nil {
		t.Fatal("M1 accepted a fixture as live evidence")
	}
}

func TestM1CanUseIndependentModelDuringPreviousModelCooldown(t *testing.T) {
	now := time.Now()
	rejected := now.Add(-time.Minute)
	start, finish := now.Add(-2*time.Second), now.Add(-time.Second)
	verified := Gate{Ready: true, Observation: &Evidence{Model: "opencode/longcat-2.5-free", ObservedAt: &start, FinishedAt: &finish}}
	report := Report{AGY: verified, OpenCode: verified, OpenCodeRecovery: "RECORDED_GATE_OTHER_MODEL",
		OpenCodeRecoveryGate: &FreeGate{Scope: "model", Model: "opencode/deepseek-v4.1-flash-free", ObservedAt: rejected, RetryAt: now.Add(time.Hour)}}
	if err := requireProviderSmokes(report, now); err != nil {
		t.Fatalf("exact alternate after rejected model was blocked: %v", err)
	}
	report.OpenCode.Observation.Model = report.OpenCodeRecoveryGate.Model
	if requireProviderSmokes(report, now) == nil {
		t.Fatal("the rate-limited model was accepted during its own cooldown")
	}
	report.OpenCode.Observation.Model = "opencode/longcat-2.5-free"
	report.OpenCodeRecoveryGate.Scope = "pool"
	if requireProviderSmokes(report, now) == nil {
		t.Fatal("an explicit shared scope was treated as a model-only cooldown")
	}
	report.OpenCodeRecoveryGate.Scope = "model"
	report.OpenCodeRecoveryGate.ObservedAt = now
	if err := requireProviderSmokes(report, now); err != nil {
		t.Fatalf("a later unrelated rate limit invalidated the selected model's smoke: %v", err)
	}
}
