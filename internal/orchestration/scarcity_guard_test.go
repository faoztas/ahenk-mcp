package orchestration

import (
	"strings"
	"testing"
	"time"

	"github.com/faoztas/ahenk-mcp/internal/providers"
)

func TestOpusScarcityGuardAndLunaRepositioning(t *testing.T) {
	// 1. Opus denied for routine tasks: coding, debugging, planning, research
	routineTasks := []string{"coding", "debugging", "planning", "research"}
	for _, task := range routineTasks {
		err := ValidateModelAdmission(task, "antigravity/claude-opus-5-5", 1.0)
		if err == nil || !strings.Contains(err.Error(), "OPUS_SCARCITY_GATE_TASK_DENIED") {
			t.Errorf("task %s: expected OPUS_SCARCITY_GATE_TASK_DENIED, got %v", task, err)
		}
	}

	// 2. Opus admitted for architecture, code_review, agent_orchestration with health > 0.20
	allowedTasks := []string{"architecture", "code_review", "agent_orchestration"}
	for _, task := range allowedTasks {
		err := ValidateModelAdmission(task, "antigravity/claude-opus-5-5", 0.50)
		if err != nil {
			t.Errorf("task %s: expected allowed Opus to pass, got %v", task, err)
		}
	}

	// 3. Opus rejected for architecture when reserve <= 0.20
	err := ValidateModelAdmission("architecture", "antigravity/claude-opus-5-5", 0.20)
	if err == nil || !strings.Contains(err.Error(), "OPUS_RESERVE_DEPLETED") {
		t.Errorf("expected OPUS_RESERVE_DEPLETED at 0.20, got %v", err)
	}

	err = ValidateModelAdmission("architecture", "antigravity/claude-opus-5-5", 0.19)
	if err == nil || !strings.Contains(err.Error(), "OPUS_RESERVE_DEPLETED") {
		t.Errorf("expected OPUS_RESERVE_DEPLETED at 0.19, got %v", err)
	}

	// 4. Luna denied as primary for coding and debugging
	for _, cat := range []string{"coding", "debugging"} {
		matrix := FallbackMatrix(cat)
		if matrix[0] == "codex/gpt-6-luna" {
			t.Errorf("category %s: luna must not be primary", cat)
		}
	}

	// 5. Luna rejected for coding and debugging via ValidateModelAdmission
	err = ValidateModelAdmission("coding", "codex/gpt-6-luna", 1.0)
	if err == nil || !strings.Contains(err.Error(), "LUNA_CODING_DENIED") {
		t.Errorf("expected LUNA_CODING_DENIED for coding, got %v", err)
	}

	err = ValidateModelAdmission("debugging", "codex/gpt-6-luna", 1.0)
	if err == nil || !strings.Contains(err.Error(), "LUNA_CODING_DENIED") {
		t.Errorf("expected LUNA_CODING_DENIED for debugging, got %v", err)
	}

	// 6. Luna admitted for research
	err = ValidateModelAdmission("research", "codex/gpt-6-luna", 1.0)
	if err != nil {
		t.Errorf("expected luna for research to pass, got %v", err)
	}

	// 7. Other normal models pass
	err = ValidateModelAdmission("coding", "codex/gpt-6.1-sol", 1.0)
	if err != nil {
		t.Errorf("expected standard model admission to pass, got %v", err)
	}
}

func TestOpusDualWindowScarcityMin(t *testing.T) {
	// P0-3: Validate that when evaluating Opus reserve health, the most restrictive (min) window is used.
	now := time.Now().UTC()
	e := &Engine{
		quotas: make(map[string][]providers.Quota),
	}

	h50 := 50.0
	h10 := 10.0 // depleted (<= 20)
	h80 := 80.0

	// Case 1: 5h is healthy (80%), weekly is depleted (10%) -> the admission guard uses 0.10
	e.quotas["agy"] = []providers.Quota{
		{AccountMatch: "MATCHED", Unit: "percent", State: "KNOWN", Bucket: providers.BarAgyOpus5h, Remaining: &h80, ObservedAt: now},
		{AccountMatch: "MATCHED", Unit: "percent", State: "KNOWN", Bucket: providers.BarAgyOpus, Remaining: &h10, ObservedAt: now},
	}
	health := e.opusAdmissionHealth(now)
	if health != 0.10 {
		t.Fatalf("expected min health 0.10, got %v", health)
	}
	if err := ValidateModelAdmission("architecture", "antigravity/claude-opus-5-5", health); err == nil {
		t.Fatalf("expected OPUS_RESERVE_DEPLETED when weekly window is depleted")
	}

	// Case 2: 5h is depleted (10%), weekly is healthy (80%) -> the admission guard uses 0.10
	e.quotas["agy"] = []providers.Quota{
		{AccountMatch: "MATCHED", Unit: "percent", State: "KNOWN", Bucket: providers.BarAgyOpus5h, Remaining: &h10, ObservedAt: now},
		{AccountMatch: "MATCHED", Unit: "percent", State: "KNOWN", Bucket: providers.BarAgyOpus, Remaining: &h80, ObservedAt: now},
	}
	health = e.opusAdmissionHealth(now)
	if health != 0.10 {
		t.Fatalf("expected min health 0.10, got %v", health)
	}
	if err := ValidateModelAdmission("architecture", "antigravity/claude-opus-5-5", health); err == nil {
		t.Fatalf("expected OPUS_RESERVE_DEPLETED when 5h window is depleted")
	}

	// Case 3: Both windows healthy (50% and 80%) -> min health is 0.50, admission passes
	e.quotas["agy"] = []providers.Quota{
		{AccountMatch: "MATCHED", Unit: "percent", State: "KNOWN", Bucket: providers.BarAgyOpus5h, Remaining: &h50, ObservedAt: now},
		{AccountMatch: "MATCHED", Unit: "percent", State: "KNOWN", Bucket: providers.BarAgyOpus, Remaining: &h80, ObservedAt: now},
	}
	health = e.opusAdmissionHealth(now)
	if health != 0.50 {
		t.Fatalf("expected min health 0.50, got %v", health)
	}
	if err := ValidateModelAdmission("architecture", "antigravity/claude-opus-5-5", health); err != nil {
		t.Fatalf("expected admission to succeed, got %v", err)
	}
}
