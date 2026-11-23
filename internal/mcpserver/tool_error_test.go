// Copyright 2026 Furkan Anıl Öztaş
// SPDX-License-Identifier: Apache-2.0

package mcpserver

import (
	"testing"

	"github.com/faoztas/ahenk-mcp/internal/model"
)

func TestServer_DelegateTask_ErrorPreservesRouteDecision(t *testing.T) {
	session, _ := setupTestMCPServer(t)

	out, err := callMCPTool(session, "delegate_task", map[string]any{
		"task":           "Dispatch with an invalid request id",
		"repo_id":        "test-repo",
		"request_id":     "../bad",
		"execution_mode": "async",
	})
	if err != nil {
		t.Fatalf("delegate_task call failed: %v", err)
	}
	if out["code"] != "INVALID_INPUT" {
		t.Fatalf("expected INVALID_INPUT from Submit, got %+v", out)
	}
	route, ok := out["route_decision"].(map[string]any)
	if !ok || route["selected_model"] == nil || route["selected_model"] == "" {
		t.Fatalf("expected route_decision metadata on handler error, got %+v", out)
	}
	// Only allowlisted correlation metadata may cross the error boundary.
	for _, leaked := range []string{"summary", "diff", "repo_id", "status", "checks"} {
		if _, has := out[leaked]; has {
			t.Fatalf("handler error must not leak %q: %+v", leaked, out)
		}
	}
}

func TestToolError_PreservesOnlySafeMetadata(t *testing.T) {
	meta := response{
		"job_id":         "job-abc",
		"route_decision": map[string]any{"selected_model": "codex/gpt-6.1-sol"},
		"summary":        "private summary",
		"diff":           "private diff",
		"repo_id":        "private-repo",
		"status":         model.Completed,
		"code":           "MUST_NOT_OVERWRITE",
	}
	res := toolError(model.Error("INVALID_INPUT", "bad input"), meta)
	wire, ok := res.StructuredContent.(response)
	if !ok {
		t.Fatalf("expected response StructuredContent, got %#v", res.StructuredContent)
	}
	if wire["job_id"] != "job-abc" {
		t.Fatalf("expected job_id preserved, got %+v", wire)
	}
	if _, ok := wire["route_decision"].(map[string]any); !ok {
		t.Fatalf("expected route_decision preserved, got %+v", wire)
	}
	if wire["code"] != "INVALID_INPUT" {
		t.Fatalf("structured error code was overwritten: %+v", wire)
	}
	if wire["message"] != "bad input" {
		t.Fatalf("structured error message was overwritten: %+v", wire)
	}
	for _, leaked := range []string{"summary", "diff", "repo_id", "status"} {
		if _, has := wire[leaked]; has {
			t.Fatalf("error metadata leaked %q: %+v", leaked, wire)
		}
	}

	empty := toolError(model.Error("INVALID_INPUT", "bad input"), response{"job_id": "", "route_decision": nil})
	emptyWire := empty.StructuredContent.(response)
	if _, has := emptyWire["job_id"]; has {
		t.Fatalf("empty job_id must be omitted: %+v", emptyWire)
	}
	if _, has := emptyWire["route_decision"]; has {
		t.Fatalf("nil route_decision must be omitted: %+v", emptyWire)
	}
}
