// Copyright 2026 Furkan Anıl Öztaş
// SPDX-License-Identifier: Apache-2.0

package mcpserver

import (
	"testing"
)

func TestDiagnosticsMCPTools(t *testing.T) {
	server, _ := setupTestMCPServer(t)

	// 1. get_provider_quotas
	resQuotas, err := callMCPTool(server, "get_provider_quotas", map[string]any{})
	if err != nil {
		t.Fatalf("get_provider_quotas failed: %v", err)
	}
	bars, ok := resQuotas["bars"].([]any)
	if !ok || len(bars) == 0 {
		t.Fatalf("expected bars array, got %v", resQuotas)
	}

	// 2. get_active_circuit_breakers
	resBreakers, err := callMCPTool(server, "get_active_circuit_breakers", map[string]any{})
	if err != nil {
		t.Fatalf("get_active_circuit_breakers failed: %v", err)
	}
	if _, ok := resBreakers["tripped_breakers"]; !ok {
		t.Fatalf("expected tripped_breakers key, got %v", resBreakers)
	}

	// 3. get_routing_audit
	delOut, err := callMCPTool(server, "delegate_task", map[string]any{
		"task": map[string]any{
			"category":  "coding",
			"objective": "Audit test task",
		},
		"target":         "auto",
		"execution_mode": "async",
	})
	if err != nil {
		t.Fatalf("delegate_task failed: %v", err)
	}
	resAudit, err := callMCPTool(server, "get_routing_audit", map[string]any{"job_id": delOut["job_id"]})
	if err != nil {
		t.Fatalf("get_routing_audit failed: %v", err)
	}
	if _, ok := resAudit["attempts"]; !ok {
		t.Fatalf("expected attempts key, got %v", resAudit)
	}
	resUnknown, err := callMCPTool(server, "get_routing_audit", map[string]any{"job_id": "job-test-1"})
	if err != nil {
		t.Fatalf("get_routing_audit unknown failed: %v", err)
	}
	if resUnknown["code"] != "NOT_FOUND" {
		t.Fatalf("expected NOT_FOUND for unknown job_id, got %v", resUnknown)
	}
}
