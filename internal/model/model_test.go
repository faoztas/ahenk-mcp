// Copyright 2026 Furkan Anıl Öztaş
// SPDX-License-Identifier: Apache-2.0

package model

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
)

func TestDelegateTaskInput_UnmarshalJSON_MalformedTaskAndTarget(t *testing.T) {
	cases := []struct {
		name string
		raw  string
	}{
		{name: "task_number", raw: `{"task": 123}`},
		{name: "task_array", raw: `{"task": ["a"]}`},
		{name: "task_bad_field", raw: `{"task": {"objective": 123}}`},
		{name: "target_number", raw: `{"task": "ok", "target": 42}`},
		{name: "target_array", raw: `{"task": "ok", "target": ["codex"]}`},
		{name: "target_bad_field", raw: `{"task": "ok", "target": {"provider": 99}}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var in DelegateTaskInput
			if err := json.Unmarshal([]byte(tc.raw), &in); err == nil {
				t.Fatalf("expected error for %s (%s), got nil (in=%+v)", tc.name, tc.raw, in)
			}
		})
	}

	// Null task/target should not fail when objective is supplied.
	var nullIn DelegateTaskInput
	if err := json.Unmarshal([]byte(`{"objective":"ok","task":null,"target":null}`), &nullIn); err != nil {
		t.Fatalf("unexpected error for null task/target: %v", err)
	}
	if nullIn.Target != nil {
		t.Fatalf("expected nil Target for null target, got %+v", nullIn.Target)
	}
}

func TestDelegateTaskInput_UnmarshalJSON_ComplexityPresence(t *testing.T) {
	// 1. Omitted complexity -> HasComplexity false, Complexity 0
	var omitted DelegateTaskInput
	if err := json.Unmarshal([]byte(`{"task":"do work"}`), &omitted); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if omitted.HasComplexity {
		t.Fatalf("expected HasComplexity=false when omitted")
	}
	if omitted.Complexity != 0 {
		t.Fatalf("expected Complexity=0 when omitted, got %v", omitted.Complexity)
	}

	// 2. Explicit 0.0 at top-level -> HasComplexity true, Complexity 0.0
	var explicitZero DelegateTaskInput
	if err := json.Unmarshal([]byte(`{"task":"do work","complexity":0.0}`), &explicitZero); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !explicitZero.HasComplexity {
		t.Fatalf("expected HasComplexity=true when top-level complexity=0.0 is explicit")
	}
	if explicitZero.Complexity != 0.0 {
		t.Fatalf("expected Complexity=0.0, got %v", explicitZero.Complexity)
	}

	// 3. Explicit 0.0 inside nested task -> HasComplexity true, Complexity 0.0
	var nestedZero DelegateTaskInput
	if err := json.Unmarshal([]byte(`{"task":{"objective":"do work","complexity":0.0}}`), &nestedZero); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !nestedZero.HasComplexity {
		t.Fatalf("expected HasComplexity=true when nested task complexity=0.0 is explicit")
	}
	if nestedZero.Complexity != 0.0 {
		t.Fatalf("expected Complexity=0.0, got %v", nestedZero.Complexity)
	}

	// 4. Top-level explicit 0.0 overrides nested 0.8
	var topZeroNestedHigh DelegateTaskInput
	if err := json.Unmarshal([]byte(`{"complexity":0.0,"task":{"objective":"do work","complexity":0.8}}`), &topZeroNestedHigh); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !topZeroNestedHigh.HasComplexity || topZeroNestedHigh.Complexity != 0.0 {
		t.Fatalf("expected top-level complexity=0.0 to win, got HasComplexity=%v Complexity=%v",
			topZeroNestedHigh.HasComplexity, topZeroNestedHigh.Complexity)
	}
}

func TestDelegateTaskInput_UnmarshalJSON_PreservesExtraTaskFields(t *testing.T) {
	nestedJSON := `{
		"task": {
			"objective": "Verify nested task fields",
			"checkpoint_objective": "checkpoint-1",
			"follow_up": "continue step 2",
			"role_calibration": true,
			"opencode_recheck": true,
			"agy_weekly_reserve_override": "authorized-test",
			"native_child": true,
			"native_child_background": true,
			"task_class": "utility",
			"risk": "high",
			"complexity": 0.4
		}
	}`
	var in DelegateTaskInput
	if err := json.Unmarshal([]byte(nestedJSON), &in); err != nil {
		t.Fatalf("unmarshal nested task failed: %v", err)
	}
	sub, _, _, err := in.Normalize("", nil)
	if err != nil {
		t.Fatalf("normalize failed: %v", err)
	}
	if sub.Task.CheckpointObjective != "checkpoint-1" {
		t.Errorf("CheckpointObjective = %q, want checkpoint-1", sub.Task.CheckpointObjective)
	}
	if sub.Task.FollowUp != "continue step 2" {
		t.Errorf("FollowUp = %q, want 'continue step 2'", sub.Task.FollowUp)
	}
	if !sub.Task.RoleCalibration {
		t.Errorf("RoleCalibration = false, want true")
	}
	if !sub.Task.OpenCodeRecheck {
		t.Errorf("OpenCodeRecheck = false, want true")
	}
	if sub.Task.AGYReserveOverride != "authorized-test" {
		t.Errorf("AGYReserveOverride = %q, want authorized-test", sub.Task.AGYReserveOverride)
	}
	if !sub.Task.NativeChild {
		t.Errorf("NativeChild = false, want true")
	}
	if !sub.Task.NativeChildBackground {
		t.Errorf("NativeChildBackground = false, want true")
	}
	if sub.Task.TaskClass != "utility" {
		t.Errorf("TaskClass = %q, want utility", sub.Task.TaskClass)
	}
	if sub.Task.Risk != "high" {
		t.Errorf("Risk = %q, want high", sub.Task.Risk)
	}
	if sub.Task.Complexity != 0.4 {
		t.Errorf("Complexity = %v, want 0.4", sub.Task.Complexity)
	}

	// Also verify top-level Submission-style payload preserves all extra fields.
	topJSON := `{
		"objective": "Verify top-level fields",
		"checkpoint_objective": "checkpoint-top",
		"follow_up": "follow-top",
		"role_calibration": true,
		"opencode_recheck": true,
		"agy_weekly_reserve_override": "override-top",
		"native_child": true,
		"native_child_background": true,
		"task_class": "security",
		"risk": "critical"
	}`
	var topIn DelegateTaskInput
	if err := json.Unmarshal([]byte(topJSON), &topIn); err != nil {
		t.Fatalf("unmarshal top-level failed: %v", err)
	}
	topSub, _, _, err := topIn.Normalize("", nil)
	if err != nil {
		t.Fatalf("normalize top-level failed: %v", err)
	}
	if topSub.Task.CheckpointObjective != "checkpoint-top" ||
		topSub.Task.FollowUp != "follow-top" ||
		!topSub.Task.RoleCalibration ||
		!topSub.Task.OpenCodeRecheck ||
		topSub.Task.AGYReserveOverride != "override-top" ||
		!topSub.Task.NativeChild ||
		!topSub.Task.NativeChildBackground ||
		topSub.Task.TaskClass != "security" ||
		topSub.Task.Risk != "critical" {
		t.Fatalf("top-level extra fields not preserved: %+v", topSub.Task)
	}
}

func TestWaitJobsInput_Normalize_DeduplicateAndCap(t *testing.T) {
	// Deduplicates preserving order
	in := WaitJobsInput{
		JobIDs: []string{"job-2", "job-1", "job-2", "job-3", "job-1"},
	}
	norm, err := in.Normalize()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []string{"job-2", "job-1", "job-3"}
	if !slices.Equal(norm.JobIDs, want) {
		t.Fatalf("JobIDs = %v, want %v", norm.JobIDs, want)
	}

	// Rejects oversized lists (>32)
	oversized := make([]string, 33)
	for i := range oversized {
		oversized[i] = "job-" + strings.Repeat("a", 3) + string(rune('a'+i%26)) + string(rune('0'+i/26))
	}
	if _, err := (WaitJobsInput{JobIDs: oversized}).Normalize(); err == nil {
		t.Fatalf("expected error for oversized JobIDs (>32)")
	}
}
