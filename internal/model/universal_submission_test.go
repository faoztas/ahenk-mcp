// Copyright 2026 Furkan Anıl Öztaş
// SPDX-License-Identifier: Apache-2.0

package model

import (
	"testing"
	"time"
)

func TestDelegateTaskInput_OnlyTask(t *testing.T) {
	in := DelegateTaskInput{
		Task: "Implement universal MCP subagent support",
	}
	sub, execMode, timeout, err := in.Normalize("", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if sub.Task.Objective != "Implement universal MCP subagent support" {
		t.Fatalf("expected objective to match task, got %q", sub.Task.Objective)
	}
	if execMode != "wait" {
		t.Fatalf("expected execution mode 'wait', got %q", execMode)
	}
	if timeout != 300*time.Second {
		t.Fatalf("expected default timeout 300s, got %v", timeout)
	}
	if !ValidID(sub.RequestID) {
		t.Fatalf("expected valid generated request_id, got %q", sub.RequestID)
	}
	if len(sub.Task.Acceptance) != 1 || sub.Task.Acceptance[0].ID == "" {
		t.Fatalf("expected default acceptance criterion, got %+v", sub.Task.Acceptance)
	}
	if len(sub.Task.AllowedPaths) != 1 || sub.Task.AllowedPaths[0] != "." {
		t.Fatalf("expected default allowed_paths ['.'], got %+v", sub.Task.AllowedPaths)
	}
	if sub.Task.Mode != "patch" {
		t.Fatalf("expected default mode 'patch', got %q", sub.Task.Mode)
	}
	if sub.Task.DataClass != "synthetic" {
		t.Fatalf("expected default data_class 'synthetic', got %q", sub.Task.DataClass)
	}
}

func TestDelegateTaskInput_AliasesAndOverrides(t *testing.T) {
	// 1. Objective alias
	inObj := DelegateTaskInput{
		Objective:     "Legacy objective with wait mode",
		ExecutionMode: "wait",
	}
	sub, execMode, _, err := inObj.Normalize("", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if sub.Task.Objective != "Legacy objective with wait mode" {
		t.Fatalf("expected objective from Objective field, got %q", sub.Task.Objective)
	}
	if execMode != "wait" {
		t.Fatalf("expected wait mode, got %q", execMode)
	}

	// 2. Mode alias for execution_mode
	inModeWait := DelegateTaskInput{
		Task: "Task with mode: wait",
		Mode: "wait",
	}
	_, execModeWait, _, err := inModeWait.Normalize("", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if execModeWait != "wait" {
		t.Fatalf("expected wait execution mode from Mode='wait', got %q", execModeWait)
	}

	inModeAsync := DelegateTaskInput{
		Task: "Task with mode: async",
		Mode: "async",
	}
	_, execModeAsync, _, err := inModeAsync.Normalize("", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if execModeAsync != "async" {
		t.Fatalf("expected async execution mode from Mode='async', got %q", execModeAsync)
	}

	// 3. Mode alias for task mode ("read_only" / "patch")
	inModeRO := DelegateTaskInput{
		Task: "Task with read_only",
		Mode: "read_only",
	}
	subRO, _, _, err := inModeRO.Normalize("", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if subRO.Task.Mode != "read_only" {
		t.Fatalf("expected task mode 'read_only', got %q", subRO.Task.Mode)
	}

	// 4. Role defaults (research -> read_only)
	inResearch := DelegateTaskInput{
		Task: "Research codebase",
		Role: "research",
	}
	subRes, _, _, err := inResearch.Normalize("", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if subRes.Task.Mode != "read_only" {
		t.Fatalf("expected 'read_only' for research role, got %q", subRes.Task.Mode)
	}

	// 5. Custom timeouts
	inTimeout := DelegateTaskInput{
		Task:               "Quick task",
		WaitTimeoutSeconds: 45,
	}
	_, _, timeout, err := inTimeout.Normalize("", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if timeout != 45*time.Second {
		t.Fatalf("expected 45s timeout, got %v", timeout)
	}
}

func TestDelegateTaskInput_LegacyCallersDefaultAsync(t *testing.T) {
	// A legacy caller provides repo_id, request_id, objective, acceptance, allowed_paths, mode, data_class
	// without task and without execution_mode.
	legacy := DelegateTaskInput{
		RepoID:       "demo",
		RequestID:    "legacy-req-1",
		Objective:    "Perform legacy task",
		Acceptance:   []Check{{ID: "c1", Description: "pass"}},
		AllowedPaths: []string{"calc.go"},
		Mode:         "patch",
		DataClass:    "synthetic",
	}
	sub, execMode, _, err := legacy.Normalize("", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if execMode != "async" {
		t.Fatalf("legacy callers without task/execution_mode must default to 'async', got %q", execMode)
	}
	if sub.RequestID != "legacy-req-1" {
		t.Fatalf("expected preserved request_id, got %q", sub.RequestID)
	}
	if sub.Task.Mode != "patch" {
		t.Fatalf("expected task mode 'patch', got %q", sub.Task.Mode)
	}
}

func TestDelegateTaskInput_ValidationErrors(t *testing.T) {
	// Both task and objective empty
	empty := DelegateTaskInput{}
	_, _, _, err := empty.Normalize("", nil)
	if err == nil {
		t.Fatalf("expected error when both task and objective are empty")
	}
}

func TestWaitJobsInput_Normalize(t *testing.T) {
	w := WaitJobsInput{
		JobIDs: []string{"job-1", "job-2"},
	}
	norm, err := w.Normalize()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if norm.Mode != "all" {
		t.Fatalf("expected default mode 'all', got %q", norm.Mode)
	}
	if norm.TimeoutSeconds != 300 {
		t.Fatalf("expected default timeout 300, got %d", norm.TimeoutSeconds)
	}

	// Empty JobIDs
	empty := WaitJobsInput{}
	if _, err := empty.Normalize(); err == nil {
		t.Fatalf("expected error for empty JobIDs")
	}
}
