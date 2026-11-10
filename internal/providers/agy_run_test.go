// Copyright 2026 Furkan Anıl Öztaş
// SPDX-License-Identifier: Apache-2.0

package providers

import (
	"context"
	"encoding/json"
	"errors"
	"os/exec"
	"slices"
	"testing"
	"time"

	"github.com/faoztas/ahenk-mcp/internal/model"
)

func TestAGYStreamSendsOnePromptOnlyAfterGate(t *testing.T) {
	const modelID = "gemini-3.8-flash-medium"
	root := t.TempDir()
	for _, tc := range []struct {
		name, model, cwd, mode string
		tools                  []string
		cost                   bool
		fault                  string
		terminal               string
	}{
		{"scoped nonempty inventory", modelID, root, "request-review", []string{"view_file"}, true, "", "success"},
		{"wrong model", "wrong", root, "request-review", []string{"view_file"}, true, "AGY_INIT_MISMATCH", ""},
		{"wrong directory", modelID, "/outside", "request-review", []string{"view_file"}, true, "AGY_INIT_MISMATCH", ""},
		{"unsafe mode", modelID, root, "always-proceed", []string{"view_file"}, true, "AGY_INIT_MISMATCH", ""},
		{"missing cost proof", modelID, root, "request-review", []string{"view_file"}, false, "ZERO_COST_UNVERIFIED", ""},
		{"native inventory", modelID, root, "request-review", []string{"view_file", "send_message", "invoke_subagent", "run_command", "ask_permission"}, true, "", "success"},
		{"tool finish", modelID, root, "request-review", []string{"finish", "send_message"}, true, "", "finish"},
		{"native finish step", modelID, root, "request-review", []string{"finish", "send_message"}, true, "", "native_finish"},
		{"native error step", modelID, root, "request-review", []string{"finish"}, true, "PERMISSION_DENIED", "native_error"},
		{"native tool execution", modelID, root, "request-review", []string{"view_file", "send_message"}, true, "PERMISSION_DENIED", "tool"},
		{"missing terminal usage", modelID, root, "request-review", []string{"view_file"}, true, "INVALID_OUTPUT", "no_usage"},
		{"nonzero exit after result", modelID, root, "request-review", []string{"view_file"}, true, "INVALID_OUTPUT", "nonzero"},
		{"unknown event", modelID, root, "request-review", []string{"view_file"}, true, "INVALID_OUTPUT", "unknown_event"},
		{"unexpected tool field", modelID, root, "request-review", []string{"view_file"}, true, "INVALID_OUTPUT", "unexpected_tool"},
		{"empty exit after prompt", modelID, root, "request-review", []string{"view_file"}, true, "INVALID_OUTPUT", "missing"},
		{"soft denial", modelID, root, "request-review", []string{"view_file"}, true, "PERMISSION_DENIED", "denied"},
		{"terminal denied actions", modelID, root, "request-review", []string{"view_file"}, true, "PERMISSION_DENIED", "denied_actions"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var event agyEvent
			event.Event, event.Session = "init", "session"
			event.Init.CWD, event.Init.Model, event.Init.Agent, event.Init.PermissionMode, event.Init.Tools = tc.cwd, tc.model, agyRestrictedAgent, tc.mode, &tc.tools
			line, _ := json.Marshal(event)
			result := `{"event":"result","result":{"conversation_id":"session","status":"SUCCESS","response":"accepted","usage":{"total_tokens":5}}}`
			switch tc.terminal {
			case "missing":
				result = ""
			case "unknown_event":
				result = `{"event":"opaque-provider-value","unexpected":"opaque-tool-data"}`
			case "unexpected_tool":
				result = `{"event":"step_update","step_update":{"conversation_id":"session","step_type":"agent_response","state":"DONE","tool_info":{"parameters":{"CommandLine":"opaque-tool-data"}}}}`
			case "no_usage":
				result = `{"event":"result","result":{"conversation_id":"session","status":"SUCCESS","response":"accepted"}}`
			case "native_finish":
				result = `{"event":"step_update","step_update":{"conversation_id":"session","step_type":"system_message","state":"DONE"}}` + "\n" +
					`{"event":"step_update","step_update":{"conversation_id":"session","step_type":"finish","state":"DONE"}}` + "\n" + result
			case "native_error":
				result = `{"event":"step_update","step_update":{"conversation_id":"session","step_type":"error_message","state":"DONE","text_delta":"permission check failed"}}`
			case "finish":
				result = `{"event":"step_update","step_update":{"conversation_id":"session","step_type":"tool","state":"DONE","tool_name":"finish","tool_info":{"name":"finish"}}}` + "\n" + result
			case "denied_actions":
				result = `{"event":"result","result":{"conversation_id":"session","status":"SUCCESS","response":"accepted","usage":{"total_tokens":5},"denied_actions":[{"action":"command"}]}}`
			case "tool":
				result = `{"event":"step_update","step_update":{"conversation_id":"session","step_type":"tool","state":"DONE","tool_name":"send_message","tool_info":{"parameters":{"TargetConversationId":"another-job"}}}}` + "\n" + result
			}
			script := `printf '%s\n' "$1"; IFS= read -r first || exit 0
case "$first" in *'"event":"user"'*'"content":"fixture-marker"'*) ;; *) exit 2;; esac
if [ "$3" = denied ]; then printf 'permission denied\n' >&2; fi
if [ "$3" = nonzero ]; then printf '%s\n' "$2"; exit 4; fi
if [ -n "$2" ]; then printf '%s\n' "$2"; IFS= read -r extra && exit 3; fi
exit 0`
			cmd := exec.Command("/bin/sh", "-c", script, "worker", string(line), result, tc.terminal)
			ev := &model.AGYRunEvidence{}
			if tc.cost {
				ev.Cost = &model.CostEvidence{Status: "DOCUMENTED_COST_PROTECTION", Documented: "VERIFIED"}
			}
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			out, err := runAGYStream(ctx, cmd, modelID, AGYPolicy{Root: root, Agent: agyRestrictedAgent, Profile: "restricted"}, "fixture-marker", ev)
			if tc.fault == "" {
				if ev.NativeFinishObserved != (tc.terminal == "native_finish") {
					t.Fatalf("unexpected native finish evidence: %+v", ev)
				}
				if !slices.Equal(ev.Tools, tc.tools) {
					t.Fatalf("tool inventory = %v, want %v", ev.Tools, tc.tools)
				}
				if err != nil || !ev.PromptSent || !ev.StartupVerified || !ev.TerminalVerified || out.Text != "accepted" || ev.TotalTokens != 5 {
					t.Fatalf("unexpected result %+v, evidence %+v, err %v", out, ev, err)
				}
				return
			}
			var fault *model.Fault
			if !errors.As(err, &fault) || fault.Code != tc.fault || ev.PromptSent != (tc.terminal != "") {
				t.Fatalf("expected %s, got %+v, evidence %+v", tc.fault, err, ev)
			}
			sources := map[string]string{
				"denied": "AGY_STDERR_REJECTED", "denied_actions": "AGY_TERMINAL_DENIED_ACTIONS", "tool": "AGY_TOOL_PROFILE_DENIED",
				"missing": "AGY_TERMINAL_MISSING", "no_usage": "AGY_TERMINAL_USAGE_INVALID", "nonzero": "AGY_PROCESS_EXIT",
				"unknown_event": "AGY_EVENT_UNSUPPORTED", "unexpected_tool": "AGY_STEP_UNEXPECTED_TOOL", "native_error": "AGY_STEP_ERROR",
			}
			if want, ok := sources[tc.terminal]; ok && fault.ProviderReason != want {
				t.Fatalf("expected denial source %q, got %q", want, fault.ProviderReason)
			}
			if tc.terminal == "unknown_event" && ev.LastEvent.Event != "other" {
				t.Fatalf("expected redacted event label, got %+v", ev.LastEvent)
			}
			if tc.terminal == "unexpected_tool" && (ev.LastEvent.Step != "agent_response" || !ev.LastEvent.HasToolParameters) {
				t.Fatalf("expected sanitized tool field evidence, got %+v", ev.LastEvent)
			}
			if tc.terminal == "tool" && ev.RejectedStep != "send_message" {
				t.Fatalf("expected known rejection label, got %q", ev.RejectedStep)
			}
		})
	}
}

func TestAGYStreamCancelsWithoutStartupOrPrompt(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	cmd := exec.Command("/bin/sh", "-c", "exec /bin/sleep 5")
	ev := &model.AGYRunEvidence{}
	_, err := runAGYStream(ctx, cmd, "model", AGYPolicy{Root: "/fixture"}, "fixture-marker", ev)
	if !errors.Is(err, context.DeadlineExceeded) || ev.PromptSent {
		t.Fatalf("expected cancellation without a prompt, got %v (%+v)", err, ev)
	}
}

func TestAGYSubscriptionGateRejectsStaleOrExhaustedWindows(t *testing.T) {
	now := time.Now()
	reset := now.Add(time.Hour)
	remaining, exhausted := 90.0, 0.0
	rows := []Quota{
		{Provider: "agy", AccountMatch: "SAME_NATIVE_CLI_CONTEXT", Bucket: "gemini-5h", Family: "gemini", Source: "agy_usage", ObservedAt: now, ResetAt: &reset, Remaining: &remaining},
		{Provider: "agy", AccountMatch: "SAME_NATIVE_CLI_CONTEXT", Bucket: "gemini-weekly", Family: "gemini", Source: "agy_usage", ObservedAt: now, ResetAt: &reset, Remaining: &remaining},
	}
	if err := checkAGYSubscriptionQuota(rows, now); err != nil {
		t.Fatal(err)
	}
	if err := checkAGYSubscriptionQuota(rows, now.Add(time.Hour)); err == nil {
		t.Fatal("stale quota accepted")
	}
	rows[0].AccountMatch = "UNKNOWN"
	if fault := checkAGYSubscriptionQuota(rows, now); fault == nil || fault.Code != "ZERO_COST_UNVERIFIED" {
		t.Fatalf("expected unbound quota denial, got %v", fault)
	}
	rows[0].AccountMatch = "SAME_NATIVE_CLI_CONTEXT"
	rows[0].Remaining = &exhausted
	if fault := checkAGYSubscriptionQuota(rows, now); fault == nil || fault.Code != "QUOTA_EXHAUSTED" {
		t.Fatalf("expected exhausted quota, got %v", fault)
	}
}
