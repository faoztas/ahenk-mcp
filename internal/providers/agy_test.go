// Copyright 2026 Furkan Anıl Öztaş
// SPDX-License-Identifier: Apache-2.0

package providers

import (
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/faoztas/ahenk-mcp/internal/model"
)

const agyInit = `{"event":"init","conversation_id":"main","init":{"cwd":"/fixture","model":"gemini","permission_mode":"request-review","tools":["send_message","invoke_subagent","view_file","write_to_file","run_command","web_search"]}}`
const agyDone = `{"event":"result","result":{"conversation_id":"main","status":"SUCCESS","response":"done","usage":{"total_tokens":45}}}`

func TestAGYProfileVerifiesEffects(t *testing.T) {
	policy := AGYPolicy{Root: "/fixture", Profile: "analysis", Paths: []string{"scope"},
		Tools:                        []string{"view_file", "invoke_subagent", "send_message", "write_to_file", "run_command"},
		ChildAndMessageScopeVerified: true}
	read := `{"event":"step_update","step_update":{"conversation_id":"main","step_type":"tool","state":"DONE","tool_name":"view_file","tool_info":{"name":"view_file","parameters":{"AbsolutePath":"/fixture/scope/a"}}}}`
	child := `{"event":"step_update","step_update":{"conversation_id":"main","step_type":"tool","state":"DONE","tool_name":"invoke_subagent","subagent_info":{"subagents":[{"conversation_id":"child","workspace_uris":["file:///fixture"]}]}}}`
	message := `{"event":"step_update","step_update":{"conversation_id":"main","step_type":"tool","state":"DONE","tool_name":"send_message","tool_info":{"name":"send_message","parameters":{"TargetConversationId":"child"}}}}`
	write := `{"event":"step_update","step_update":{"conversation_id":"main","step_type":"tool","state":"DONE","tool_name":"write_to_file","tool_info":{"name":"write_to_file","parameters":{"TargetFile":"/fixture/scope/a"}}}}`
	command := `{"event":"step_update","step_update":{"conversation_id":"main","step_type":"tool","state":"DONE","tool_name":"run_command","tool_info":{"name":"run_command","parameters":{"CommandLine":"go test ./...","Cwd":"/fixture"}}}}`
	parse := func(p AGYPolicy, stderr string, events ...string) (AGYResult, error) {
		lines := slices.Concat([]string{agyInit}, events, []string{agyDone})
		return ParseAGY([]byte(strings.Join(lines, "\n")), []byte(stderr), "gemini", p)
	}
	if out, err := parse(policy, "", read, child, message); err != nil || out.Text != "done" || out.Usage.Total != 45 {
		t.Fatalf("permitted run: %v, %v", out, err)
	}
	restricted := AGYPolicy{Root: policy.Root, Profile: "restricted"}
	if _, err := parse(restricted, ""); err != nil {
		t.Fatalf("an advertised registry must not disqualify an otherwise permitted restricted run: %v", err)
	}
	for _, tc := range []struct {
		name   string
		policy AGYPolicy
		stderr string
		events []string
	}{
		{"scope", policy, "", []string{strings.ReplaceAll(read, "/fixture/scope/a", "/outside/a")}},
		{"foreign session", policy, "", []string{child, strings.ReplaceAll(message, `"child"`, `"other-job"`)}},
		{"ambiguous message target", policy, "", []string{child, strings.ReplaceAll(message, `"TargetConversationId":"child"`, `"TargetConversationId":"child","TargetAgentId":"other-job"`)}},
		{"tool-level denial", policy, "", []string{strings.ReplaceAll(read, `"parameters":`, `"error":{"type":"permission_denied"},"parameters":`)}},
		{"denied tool state", policy, "", []string{strings.ReplaceAll(read, `"state":"DONE"`, `"state":"DENIED"`)}},
		{"foreign workspace", policy, "", []string{strings.ReplaceAll(child, "file:///fixture", "file:///outside")}},
		{"unowned child events", policy, "", []string{strings.ReplaceAll(read, `"main"`, `"child"`)}},
		{"read-only write", policy, "", []string{write}},
		{"read-only command", policy, "", []string{command}},
		{"restricted tool effect", restricted, "", []string{read}},
		{"soft denial", policy, "permission denied in headless", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parse(tc.policy, tc.stderr, tc.events...)
			var fault *model.Fault
			if !errors.As(err, &fault) || fault.Code != "PERMISSION_DENIED" {
				t.Fatalf("expected permission denial, got %v", err)
			}
		})
	}
	writer := policy
	writer.Profile = "implementation"
	writer.Commands = []string{"go test ./..."}
	if _, err := parse(writer, "", write, command); err == nil {
		t.Fatal("writer without verified isolation accepted")
	}
	writer.WriterIsolationVerified = true
	if _, err := parse(writer, "", write, command); err != nil {
		t.Fatalf("scoped writer fixture was rejected: %v", err)
	}
	if _, err := parse(writer, "", strings.ReplaceAll(command, "go test ./...", "go test ./... && other")); err == nil {
		t.Fatal("command outside exact allowlist accepted")
	}
}

func TestAGYRestrictedFinishParameters(t *testing.T) {
	policy := AGYPolicy{Root: "/fixture", Profile: "restricted"}
	for _, tc := range []struct {
		name, parameters string
		allowed          bool
	}{
		{"empty", `{}`, true},
		{"null", `null`, true},
		{"structured result", `{"summary":"done","patch":"a synthetic patch"}`, true},
		{"unknown action", `{"summary":"done","action":{"tool_name":"run_command"}}`, false},
		{"nested action", `{"summary":{"CommandLine":"other"},"patch":"synthetic"}`, false},
		{"array", `{"summary":"done","patch":[{"TargetFile":"/outside"}]}`, false},
		{"duplicate", `{"summary":{"CommandLine":"other"},"summary":"done"}`, false},
		{"duplicate string", `{"summary":"first","summary":"done"}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			step := `{"event":"step_update","step_update":{"conversation_id":"main","step_type":"tool","state":"DONE","tool_name":"finish","tool_info":{"name":"finish","parameters":` + tc.parameters + `}}}`
			_, err := ParseAGY([]byte(strings.Join([]string{agyInit, step, agyDone}, "\n")), nil, "gemini", policy)
			if tc.allowed {
				if err != nil {
					t.Fatalf("expected output-only finish, got %v", err)
				}
				return
			}
			var fault *model.Fault
			if !errors.As(err, &fault) || fault.Code != "PERMISSION_DENIED" || fault.ProviderReason != "AGY_TOOL_PROFILE_DENIED" {
				t.Fatalf("expected finish scope denial, got %v", err)
			}
		})
	}
}

func TestAGYNativeFinishRejectsSideEffects(t *testing.T) {
	policy := AGYPolicy{Root: "/fixture", Profile: "restricted"}
	for _, tc := range []struct {
		name, kind, payload, source string
	}{
		{"finish renamed command", "finish", `,"tool_name":"run_command"`, "AGY_FINISH_SCOPE_DENIED"},
		{"finish nested action", "finish", `,"tool_info":{"parameters":{"summary":{"CommandLine":"run"}}}`, "AGY_FINISH_SCOPE_DENIED"},
		{"finish child metadata", "finish", `,"subagent_info":{"subagents":[{"conversation_id":"other","workspace_uris":["file:///fixture"]}]}`, "AGY_FINISH_SCOPE_DENIED"},
		{"system message tool output", "system_message", `,"tool_info":{"output":"something"}`, "AGY_STEP_UNEXPECTED_TOOL"},
		{"unknown kind", "unknown", "", "AGY_STEP_UNSUPPORTED"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			step := `{"event":"step_update","step_update":{"conversation_id":"main","step_type":"` + tc.kind + `","state":"DONE"` + tc.payload + `}}`
			_, err := ParseAGY([]byte(strings.Join([]string{agyInit, step, agyDone}, "\n")), nil, "gemini", policy)
			var fault *model.Fault
			if !errors.As(err, &fault) || fault.ProviderReason != tc.source {
				t.Fatalf("expected %s, got %v", tc.source, err)
			}
		})
	}
}

func TestAGYCostArgvAndClosedGate(t *testing.T) {
	for _, work := range []string{"", "/fixture"} {
		args, err := agyArgs("gemini-test", work)
		if err != nil || len(args) != 6 || !slices.Equal(args[:3], []string{"--model", "gemini-test", "--settings"}) {
			t.Fatalf("unexpected AGY arguments: %v, %v", args, err)
		}
		var settings struct {
			UseG1Credits         *bool   `json:"useG1Credits"`
			ModelProvider        *string `json:"modelProvider"`
			DisableSlashCommands *bool   `json:"disableSlashCommands"`
		}
		if err := json.Unmarshal([]byte(args[3]), &settings); err != nil ||
			settings.UseG1Credits == nil || *settings.UseG1Credits ||
			settings.ModelProvider == nil || *settings.ModelProvider != "" ||
			settings.DisableSlashCommands == nil || *settings.DisableSlashCommands != (work != "") {
			t.Fatalf("unsafe settings: %s", args[3])
		}
	}
	env := vendorUsageEnv(nil, "/fixture")
	if !slices.Contains(env, "AGY_CLI_DISABLE_AUTO_UPDATE=true") ||
		slices.ContainsFunc(env, func(v string) bool {
			return strings.HasPrefix(v, "GEMINI_API_KEY=") || strings.HasPrefix(v, "AGY_ADC_AUTH=") || strings.HasPrefix(v, "GOOGLE_APPLICATION_CREDENTIALS=")
		}) {
		t.Fatal("unsafe AGY environment")
	}
	r := New(model.Config{})
	c := r.Candidates[0]
	if c.Status == "available" || c.Reason != "ZERO_COST_UNVERIFIED" {
		t.Fatal(c)
	}
	_, err := r.Run(t.Context(), t.TempDir(), c, model.Task{}, nil, nil)
	var fault *model.Fault
	if !errors.As(err, &fault) || fault.Code != "ZERO_COST_UNVERIFIED" {
		t.Fatal(err)
	}
}

func TestAGYTrustedLocalAuditsNativeFileAliases(t *testing.T) {
	policy := AGYPolicy{
		Root: "/fixture", Profile: model.TrustedLocalProfile,
		Paths: []string{"calc.go", "calc_test.go"}, WritePaths: []string{"calc.go"},
		Tools: []string{"read_file", "write_file", "edit_file"},
	}
	event := func(name, path string) string {
		return `{"event":"step_update","step_update":{"conversation_id":"main","step_type":"tool","state":"DONE","tool_name":"` + name +
			`","tool_info":{"name":"` + name + `","parameters":{"file_path":"` + path + `"}}}}`
	}
	var stream agyStreamState
	evidence := new(model.AGYRunEvidence)
	for _, line := range []string{agyInit, event("read_file", "/fixture/calc_test.go"), event("write_file", "calc.go")} {
		if err := stream.accept([]byte(line), "gemini", policy, evidence); err != nil {
			t.Fatal(err)
		}
	}
	if evidence.ObservedTools["read_file"] != 1 || evidence.ObservedTools["write_file"] != 1 {
		t.Fatalf("native operations not recorded: %v", evidence.ObservedTools)
	}
	for _, line := range []string{event("write_file", "calc_test.go"), event("read_file", "/outside/calc.go"), event("run_command", "calc.go")} {
		var fault *model.Fault
		if err := stream.accept([]byte(line), "gemini", policy, evidence); !errors.As(err, &fault) || fault.Code != "PERMISSION_DENIED" {
			t.Fatalf("out-of-policy tool accepted: %v", err)
		}
	}
}

func TestAGYNativeInvocationErrorClassification(t *testing.T) {
	for _, tc := range []struct{ state, message, code, reason string }{
		{"ERROR", "", "INVALID_OUTPUT", "AGY_CHILD_INVOCATION_ERROR"},
		{"DENIED", "", "PERMISSION_DENIED", "AGY_CHILD_PERMISSION_DENIED"},
		{"ERROR", "unknown agent", "MODEL_UNAVAILABLE", "AGY_CHILD_CONFIGURATION_INVALID"},
		{"ERROR", "unsupported invocation", "MODEL_UNAVAILABLE", "AGY_CHILD_INVOCATION_UNSUPPORTED"},
		{"ERROR", "deadline exceeded", "ATTEMPT_TIMEOUT", "AGY_CHILD_TIMEOUT"},
	} {
		if code, reason := agyNativeInvocationFailure(tc.state, tc.message); code != tc.code || reason != tc.reason {
			t.Errorf("%s %q: got %s / %s", tc.state, tc.message, code, reason)
		}
	}
}

func TestAGY154WorkerPinDoesNotInheritEarlierEvidence(t *testing.T) {
	if !AGYWorkerIdentityMatches(agySyntheticModel, agy154Fingerprint) ||
		AGYIdentityMatches(agySyntheticModel, agy154Fingerprint) ||
		AGYWorkerIdentityMatches("unverified-model", agy154Fingerprint) {
		t.Fatal("AGY 1.5.4 identity crossed its verified base-model boundary")
	}
}
