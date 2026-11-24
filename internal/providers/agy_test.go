// Copyright 2026 Furkan Anıl Öztaş
// SPDX-License-Identifier: Apache-2.0

package providers

import (
	"context"
	"encoding/json"
	"errors"
	"os/exec"
	"slices"
	"strings"
	"testing"
	"time"

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

func TestAllowsAGYProfileRestrictedPatchRequiresM2A(t *testing.T) {
	profiles := []model.ExecutionProfile{
		{Name: "restricted", Status: "preflight_required"},
		{Name: "analysis", Status: "preflight_required"},
		{Name: model.TrustedLocalProfile, Status: "preflight_required"},
	}
	patchTask := model.Task{Mode: "patch", DataClass: "synthetic"}
	explicitRestrictedPatch := model.Task{Mode: "patch", DataClass: "synthetic", ExecutionProfile: "restricted"}
	trustedPatch := model.Task{Mode: "patch", DataClass: "synthetic", ExecutionProfile: model.TrustedLocalProfile}
	readTask := model.Task{Mode: "read_only", DataClass: "synthetic"}

	// Worker-only 1.5.4 binary must NOT allow restricted patch mode (implicit or explicit).
	cap154 := model.Capability{Provider: "agy", Model: agySyntheticModel, Fingerprint: agy154Fingerprint, Profiles: profiles}
	if AllowsAGYProfile(cap154, patchTask) {
		t.Fatal("expected agy154 to be rejected for implicit restricted patch task")
	}
	if AllowsAGYProfile(cap154, explicitRestrictedPatch) {
		t.Fatal("expected agy154 to be rejected for explicit restricted patch task")
	}
	if !AllowsAGYProfile(cap154, readTask) {
		t.Fatal("expected agy154 to allow synthetic read_only task")
	}
	if !AllowsAGYProfile(cap154, trustedPatch) {
		t.Fatal("expected agy154 to allow trusted-local patch task")
	}

	// Non-M2A model on 1.5.3 binary must also be rejected for restricted patch mode.
	cap153NonM2A := model.Capability{Provider: "agy", Model: "gemini-4-argon-low", Fingerprint: agy153Fingerprint, Profiles: profiles}
	if AllowsAGYProfile(cap153NonM2A, patchTask) {
		t.Fatal("expected non-M2A agy153 model to be rejected for restricted patch task")
	}

	// M2A model on 1.5.3 binary legitimately executes restricted patch mode.
	capM2A := model.Capability{Provider: "agy", Model: agySyntheticModel, Fingerprint: agy153Fingerprint, Profiles: profiles}
	if !AllowsAGYProfile(capM2A, patchTask) || !AllowsAGYProfile(capM2A, explicitRestrictedPatch) {
		t.Fatal("expected M2A agy153 model to allow restricted patch task")
	}
}

func TestAGYChildEvidenceRejectsMissingOrForeignTerminal(t *testing.T) {
	const root = "/fixture"
	policy := AGYPolicy{
		Root:                         root,
		Agent:                        agyNativeParent,
		Profile:                      "native_child",
		NativeChildRole:              agyNativeChild,
		ChildAndMessageScopeVerified: true,
		Tools:                        []string{"invoke_subagent", "send_message"},
	}
	initLine := `{"event":"init","conversation_id":"main","init":{"cwd":"/fixture","model":"` + agySyntheticModel + `","agent":"` + agyNativeParent + `","permission_mode":"request-review","tools":["invoke_subagent","send_message","finish"]}}`
	invokeActive := `{"event":"step_update","step_update":{"conversation_id":"main","step_index":1,"step_type":"tool","state":"ACTIVE","tool_name":"invoke_subagent","tool_info":{"name":"invoke_subagent","parameters":{"subagent_type":"` + agyNativeChild + `","workspace":"inherit"}}}}`
	invokeActiveChild := `{"event":"step_update","step_update":{"conversation_id":"main","step_index":1,"step_type":"subagent","state":"ACTIVE","subagent_info":{"subagents":[{"type_name":"` + agyNativeChild + `","conversation_id":"child-1","workspace_uris":["file:///fixture"]}]}}}`
	invokeDone := `{"event":"step_update","step_update":{"conversation_id":"main","step_index":1,"step_type":"subagent","state":"DONE","subagent_info":{"subagents":[{"type_name":"` + agyNativeChild + `","conversation_id":"child-1","workspace_uris":["file:///fixture"]}]}}}`
	childFinish := `{"event":"step_update","step_update":{"conversation_id":"child-1","step_type":"finish","state":"DONE","tool_info":{"parameters":{"summary":"42"}}}}`
	parentDone := `{"event":"result","result":{"conversation_id":"main","status":"SUCCESS","structured_output":{"summary":"42"},"usage":{"input_tokens":10,"output_tokens":5,"total_tokens":15}}}`

	runStream := func(t *testing.T, events ...string) (AGYResult, *model.AGYRunEvidence, error) {
		t.Helper()
		payload := strings.Join(events, "\n")
		script := `printf '%s\n' "$1"; IFS= read -r first || exit 0
case "$first" in *'"event":"user"'*'"content":"child-fixture"'*) ;; *) exit 2;; esac
if [ -n "$2" ]; then printf '%s\n' "$2"; IFS= read -r extra && exit 3; fi
exit 0`
		cmd := exec.Command("/bin/sh", "-c", script, "worker", initLine, payload)
		ev := &model.AGYRunEvidence{
			Cost: &model.CostEvidence{Status: "DOCUMENTED_COST_PROTECTION", Documented: "VERIFIED"},
		}
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		defer cancel()
		out, err := runAGYStream(ctx, cmd, agySyntheticModel, policy, "child-fixture", ev)
		return out, ev, err
	}

	t.Run("valid parent and child completion", func(t *testing.T) {
		events := []string{invokeActive, invokeDone, childFinish, parentDone}
		if res, err := ParseAGY([]byte(strings.Join(append([]string{initLine}, events...), "\n")), nil, agySyntheticModel, policy); err != nil || res.Session != "main" {
			t.Fatalf("ParseAGY rejected valid parent+child completion: %+v, %v", res, err)
		}
		_, ev, err := runStream(t, events...)
		if err != nil {
			t.Fatalf("runAGYStream rejected valid parent+child completion: %v", err)
		}
		if !ev.StartupVerified || !ev.TerminalVerified || ev.NativeChild == nil || !ev.NativeChild.ChildFinished ||
			ev.NativeChild.ParentSHA256 != model.Hash([]byte("main")) ||
			ev.NativeChild.ChildSHA256 != model.Hash([]byte("child-1")) ||
			ev.NativeChild.ResultSHA256 != model.Hash([]byte("42")) ||
			ev.ObservedTools["invoke_subagent"] != 1 {
			t.Fatalf("incomplete native child evidence: %+v", ev)
		}
	})

	for _, tc := range []struct {
		name          string
		events        []string
		wantCode      string
		wantReason    string
		wantChildLink string
		wantRejected  string
	}{
		{
			name:          "identityless subagent error without children metadata",
			events:        []string{`{"event":"step_update","step_update":{"conversation_id":"main","step_index":1,"step_type":"subagent","state":"ERROR"}}`},
			wantCode:      "INVALID_OUTPUT",
			wantReason:    "AGY_CHILD_INVOCATION_ERROR",
			wantChildLink: "CHILD_METADATA_MISSING_OR_MULTIPLE",
			wantRejected:  "invoke_subagent",
		},
		{
			name:          "identityless subagent error with empty child conversation_id",
			events:        []string{`{"event":"step_update","step_update":{"conversation_id":"main","step_index":1,"step_type":"subagent","state":"ERROR","subagent_info":{"subagents":[{"type_name":"` + agyNativeChild + `","workspace_uris":["file:///fixture"]}]}}}`},
			wantCode:      "INVALID_OUTPUT",
			wantReason:    "AGY_CHILD_INVOCATION_ERROR",
			wantChildLink: "CHILD_ID_MISSING",
			wantRejected:  "invoke_subagent",
		},
		{
			name:          "child invocation timeout without children metadata",
			events:        []string{`{"event":"step_update","step_update":{"conversation_id":"main","step_index":1,"step_type":"subagent","state":"ERROR","text_delta":"deadline exceeded"}}`},
			wantCode:      "ATTEMPT_TIMEOUT",
			wantReason:    "AGY_CHILD_TIMEOUT",
			wantChildLink: "CHILD_METADATA_MISSING_OR_MULTIPLE",
			wantRejected:  "invoke_subagent",
		},
		{
			name:       "error_message timeout",
			events:     []string{`{"event":"step_update","step_update":{"conversation_id":"main","step_type":"error_message","state":"DONE","text_delta":"child timeout"}}`},
			wantCode:   "ATTEMPT_TIMEOUT",
			wantReason: "AGY_STEP_ERROR",
		},
		{
			name:         "wrong workspace uri",
			events:       []string{strings.ReplaceAll(invokeDone, "file:///fixture", "file:///outside"), childFinish, parentDone},
			wantCode:     "PERMISSION_DENIED",
			wantReason:   "AGY_CHILD_WORKSPACE_URI_DENIED",
			wantRejected: "invoke_subagent",
		},
		{
			name:         "wrong workspace mode parameter",
			events:       []string{`{"event":"step_update","step_update":{"conversation_id":"main","step_index":1,"step_type":"tool","state":"DONE","tool_name":"invoke_subagent","tool_info":{"name":"invoke_subagent","parameters":{"workspace_mode":"branch"}},"subagent_info":{"subagents":[{"type_name":"` + agyNativeChild + `","conversation_id":"child-1","workspace_uris":["file:///fixture"]}]}}}`, childFinish, parentDone},
			wantCode:     "PERMISSION_DENIED",
			wantReason:   "AGY_CHILD_WORKSPACE_INVALID",
			wantRejected: "invoke_subagent",
		},
		{
			name:         "conflicting workspace aliases",
			events:       []string{`{"event":"step_update","step_update":{"conversation_id":"main","step_index":1,"step_type":"tool","state":"DONE","tool_name":"invoke_subagent","tool_info":{"name":"invoke_subagent","parameters":{"workspace_mode":"inherit","workspace":"branch"}},"subagent_info":{"subagents":[{"type_name":"` + agyNativeChild + `","conversation_id":"child-1","workspace_uris":["file:///fixture"]}]}}}`, childFinish, parentDone},
			wantCode:     "PERMISSION_DENIED",
			wantReason:   "AGY_CHILD_WORKSPACE_CONFLICT",
			wantRejected: "invoke_subagent",
		},
		{
			name:         "conflicting workspace aliases reversed order",
			events:       []string{`{"event":"step_update","step_update":{"conversation_id":"main","step_index":1,"step_type":"tool","state":"DONE","tool_name":"invoke_subagent","tool_info":{"name":"invoke_subagent","parameters":{"workspace_mode":"branch","workspace":"inherit"}},"subagent_info":{"subagents":[{"type_name":"` + agyNativeChild + `","conversation_id":"child-1","workspace_uris":["file:///fixture"]}]}}}`, childFinish, parentDone},
			wantCode:     "PERMISSION_DENIED",
			wantReason:   "AGY_CHILD_WORKSPACE_CONFLICT",
			wantRejected: "invoke_subagent",
		},
		{
			name:         "conflicting duplicate workspace json keys",
			events:       []string{`{"event":"step_update","step_update":{"conversation_id":"main","step_index":1,"step_type":"tool","state":"DONE","tool_name":"invoke_subagent","tool_info":{"name":"invoke_subagent","parameters":{"workspace_mode":"branch","workspace_mode":"inherit"}},"subagent_info":{"subagents":[{"type_name":"` + agyNativeChild + `","conversation_id":"child-1","workspace_uris":["file:///fixture"]}]}}}`, childFinish, parentDone},
			wantCode:     "PERMISSION_DENIED",
			wantReason:   "AGY_CHILD_WORKSPACE_CONFLICT",
			wantRejected: "invoke_subagent",
		},
		{
			name:         "conflicting non-string and inherit workspace aliases",
			events:       []string{`{"event":"step_update","step_update":{"conversation_id":"main","step_index":1,"step_type":"tool","state":"DONE","tool_name":"invoke_subagent","tool_info":{"name":"invoke_subagent","parameters":{"workspace_mode":123,"workspace":"inherit"}},"subagent_info":{"subagents":[{"type_name":"` + agyNativeChild + `","conversation_id":"child-1","workspace_uris":["file:///fixture"]}]}}}`, childFinish, parentDone},
			wantCode:     "PERMISSION_DENIED",
			wantReason:   "AGY_CHILD_WORKSPACE_CONFLICT",
			wantRejected: "invoke_subagent",
		},
		{
			name:         "conflicting null and empty workspace aliases",
			events:       []string{`{"event":"step_update","step_update":{"conversation_id":"main","step_index":1,"step_type":"tool","state":"DONE","tool_name":"invoke_subagent","tool_info":{"name":"invoke_subagent","parameters":{"workspace_mode":null,"workspace":""}},"subagent_info":{"subagents":[{"type_name":"` + agyNativeChild + `","conversation_id":"child-1","workspace_uris":["file:///fixture"]}]}}}`, childFinish, parentDone},
			wantCode:     "PERMISSION_DENIED",
			wantReason:   "AGY_CHILD_WORKSPACE_CONFLICT",
			wantRejected: "invoke_subagent",
		},
		{
			name:         "empty workspace mode parameter",
			events:       []string{`{"event":"step_update","step_update":{"conversation_id":"main","step_index":1,"step_type":"tool","state":"DONE","tool_name":"invoke_subagent","tool_info":{"name":"invoke_subagent","parameters":{"workspace_mode":""}},"subagent_info":{"subagents":[{"type_name":"` + agyNativeChild + `","conversation_id":"child-1","workspace_uris":["file:///fixture"]}]}}}`, childFinish, parentDone},
			wantCode:     "PERMISSION_DENIED",
			wantReason:   "AGY_CHILD_WORKSPACE_INVALID",
			wantRejected: "invoke_subagent",
		},
		{
			name:         "missing child id on completion",
			events:       []string{`{"event":"step_update","step_update":{"conversation_id":"main","step_index":1,"step_type":"subagent","state":"DONE","subagent_info":{"subagents":[{"type_name":"` + agyNativeChild + `","workspace_uris":["file:///fixture"]}]}}}`, childFinish, parentDone},
			wantCode:     "INVALID_OUTPUT",
			wantReason:   "AGY_CHILD_ID_MISSING",
			wantRejected: "invoke_subagent",
		},
		{
			name:         "multiple children on completion",
			events:       []string{`{"event":"step_update","step_update":{"conversation_id":"main","step_index":1,"step_type":"subagent","state":"DONE","subagent_info":{"subagents":[{"type_name":"` + agyNativeChild + `","conversation_id":"child-1","workspace_uris":["file:///fixture"]},{"type_name":"` + agyNativeChild + `","conversation_id":"child-2","workspace_uris":["file:///fixture"]}]}}}`, childFinish, parentDone},
			wantCode:     "PERMISSION_DENIED",
			wantReason:   "AGY_CHILD_MULTIPLE_CHILDREN",
			wantRejected: "invoke_subagent",
		},
		{
			name:         "duplicate invoke_subagent completion before child finish",
			events:       []string{invokeActive, invokeDone, invokeDone, childFinish, parentDone},
			wantCode:     "INVALID_OUTPUT",
			wantReason:   "AGY_CHILD_INVOCATION_DUPLICATE",
			wantRejected: "invoke_subagent",
		},
		{
			name:         "duplicate invoke_subagent completion after child finish",
			events:       []string{invokeActive, invokeDone, childFinish, invokeDone, parentDone},
			wantCode:     "INVALID_OUTPUT",
			wantReason:   "AGY_CHILD_INVOCATION_DUPLICATE",
			wantRejected: "invoke_subagent",
		},
		{
			name:         "foreign child terminal step",
			events:       []string{invokeDone, strings.ReplaceAll(childFinish, `"child-1"`, `"foreign-child"`), parentDone},
			wantCode:     "PERMISSION_DENIED",
			wantReason:   "AGY_UNOWNED_SESSION",
			wantRejected: "finish",
		},
		{
			name:       "foreign child terminal result event",
			events:     []string{invokeDone, childFinish, strings.ReplaceAll(parentDone, `"conversation_id":"main"`, `"conversation_id":"child-1"`)},
			wantCode:   "INVALID_OUTPUT",
			wantReason: "AGY_TERMINAL_REJECTED",
		},
		{
			name:       "child finishes without completed invoke_subagent",
			events:     []string{invokeActiveChild, childFinish, parentDone},
			wantCode:   "INVALID_OUTPUT",
			wantReason: "AGY_CHILD_RESULT_UNVERIFIED",
		},
		{
			name:       "parent result without child completion",
			events:     []string{invokeDone, parentDone},
			wantCode:   "INVALID_OUTPUT",
			wantReason: "AGY_CHILD_RESULT_UNVERIFIED",
		},
		{
			name:       "mismatched result digest",
			events:     []string{invokeDone, childFinish, strings.ReplaceAll(parentDone, `"summary":"42"`, `"summary":"99"`)},
			wantCode:   "INVALID_OUTPUT",
			wantReason: "AGY_CHILD_DIGEST_MISMATCH",
		},
		{
			name:       "conflicting second child finish",
			events:     []string{invokeDone, childFinish, strings.ReplaceAll(childFinish, `"summary":"42"`, `"summary":"99"`), strings.ReplaceAll(parentDone, `"summary":"42"`, `"summary":"99"`)},
			wantCode:   "INVALID_OUTPUT",
			wantReason: "AGY_CHILD_RESULT_UNVERIFIED",
		},
		{
			name:         "post-finish child response with summaryless second child finish",
			events:       []string{invokeDone, childFinish, `{"event":"step_update","step_update":{"conversation_id":"child-1","step_type":"agent_response","state":"DONE","text_delta":"99"}}`, `{"event":"step_update","step_update":{"conversation_id":"child-1","step_type":"finish","state":"DONE"}}`, parentDone},
			wantCode:     "INVALID_OUTPUT",
			wantReason:   "AGY_CHILD_RESULT_UNVERIFIED",
			wantRejected: "finish",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Run("ParseAGY", func(t *testing.T) {
				if _, parseErr := ParseAGY([]byte(strings.Join(append([]string{initLine}, tc.events...), "\n")), nil, agySyntheticModel, policy); parseErr == nil {
					t.Fatalf("ParseAGY unexpectedly accepted %s", tc.name)
				} else {
					var parseFault *model.Fault
					if !errors.As(parseErr, &parseFault) || parseFault.Code != tc.wantCode || parseFault.ProviderReason != tc.wantReason {
						t.Fatalf("ParseAGY fault = %+v, want %s/%s", parseErr, tc.wantCode, tc.wantReason)
					}
				}
			})

			t.Run("runAGYStream", func(t *testing.T) {
				_, ev, err := runStream(t, tc.events...)
				var fault *model.Fault
				if !errors.As(err, &fault) || fault.Code != tc.wantCode || fault.ProviderReason != tc.wantReason {
					t.Fatalf("runAGYStream fault = %+v, want %s/%s", err, tc.wantCode, tc.wantReason)
				}
				if ev.TerminalVerified {
					t.Fatalf("rejected child evidence must not mark TerminalVerified: %+v", ev)
				}
				if tc.wantChildLink != "" && (ev.LastEvent == nil || ev.LastEvent.ChildLink != tc.wantChildLink) {
					t.Fatalf("ChildLink = %+v, want %q", ev.LastEvent, tc.wantChildLink)
				}
				if tc.wantRejected != "" && ev.RejectedStep != tc.wantRejected {
					t.Fatalf("RejectedStep = %q, want %q", ev.RejectedStep, tc.wantRejected)
				}
			})
		})
	}
}

func TestAGYNativeAcceptanceRequiresLiveChildEvidence(t *testing.T) {
	parentHash := model.Hash([]byte("main"))
	childHash := model.Hash([]byte("child-1"))
	resultHash := model.Hash([]byte("42"))

	valid := func() AGYNativeChildAcceptance {
		return AGYNativeChildAcceptance{
			ExpectedParentHash: parentHash,
			ExpectedResultHash: resultHash,
			CleanupVerified:    true,
			Evidence: model.AGYRunEvidence{
				Profile:                   "native_child",
				Model:                     agySyntheticModel,
				PermissionMode:            "request-review",
				SubscriptionQuotaVerified: true,
				StartupVerified:           true,
				PromptSent:                true,
				TerminalVerified:          true,
				InputTokens:               10,
				OutputTokens:              5,
				TotalTokens:               15,
				ObservedTools:             map[string]int{"invoke_subagent": 1},
				Cost: &model.CostEvidence{
					Policy:     "SUBSCRIPTION_ONLY",
					Status:     "DOCUMENTED_COST_PROTECTION",
					Documented: "VERIFIED",
				},
				NativeChild: &model.NativeChildEvidence{
					ParentSHA256:  parentHash,
					ChildSHA256:   childHash,
					ChildFinished: true,
					ResultSHA256:  resultHash,
				},
			},
		}
	}

	if err := ValidateAGYNativeChildAcceptance(valid()); err != nil {
		t.Fatalf("schema-valid synthetic evidence rejected: %v", err)
	}

	for _, tc := range []struct {
		name       string
		mutate     func(*AGYNativeChildAcceptance)
		wantCode   string
		wantReason string
	}{
		{
			name: "parent only reply without native child evidence",
			mutate: func(in *AGYNativeChildAcceptance) {
				in.Evidence.NativeChild = nil
			},
			wantCode:   "INVALID_OUTPUT",
			wantReason: "AGY_CHILD_OWNERSHIP_MISSING",
		},
		{
			name: "missing provider-owned child id",
			mutate: func(in *AGYNativeChildAcceptance) {
				in.Evidence.NativeChild.ChildSHA256 = ""
			},
			wantCode:   "INVALID_OUTPUT",
			wantReason: "AGY_CHILD_ID_MISSING",
		},
		{
			name: "child identity aliases parent identity",
			mutate: func(in *AGYNativeChildAcceptance) {
				in.Evidence.NativeChild.ChildSHA256 = parentHash
			},
			wantCode:   "PERMISSION_DENIED",
			wantReason: "AGY_CHILD_IDENTITY_ALIAS",
		},
		{
			name: "mismatched parent ownership hash",
			mutate: func(in *AGYNativeChildAcceptance) {
				in.Evidence.NativeChild.ParentSHA256 = model.Hash([]byte("other-parent"))
			},
			wantCode:   "INVALID_OUTPUT",
			wantReason: "AGY_CHILD_PARENT_OWNERSHIP_INVALID",
		},
		{
			name: "missing completed invoke_subagent tool count",
			mutate: func(in *AGYNativeChildAcceptance) {
				in.Evidence.ObservedTools = map[string]int{}
			},
			wantCode:   "INVALID_OUTPUT",
			wantReason: "AGY_CHILD_INVOCATION_COUNT_INVALID",
		},
		{
			name: "missing terminal child completion",
			mutate: func(in *AGYNativeChildAcceptance) {
				in.Evidence.NativeChild.ChildFinished = false
			},
			wantCode:   "INVALID_OUTPUT",
			wantReason: "AGY_CHILD_RESULT_UNVERIFIED",
		},
		{
			name: "result digest mismatch",
			mutate: func(in *AGYNativeChildAcceptance) {
				in.Evidence.NativeChild.ResultSHA256 = model.Hash([]byte("wrong"))
			},
			wantCode:   "INVALID_OUTPUT",
			wantReason: "AGY_CHILD_DIGEST_MISMATCH",
		},
		{
			name: "missing cleanup verification",
			mutate: func(in *AGYNativeChildAcceptance) {
				in.CleanupVerified = false
			},
			wantCode:   "CLEANUP_UNVERIFIED",
			wantReason: "AGY_CHILD_CLEANUP_UNVERIFIED",
		},
		{
			name: "missing subscription cost protection",
			mutate: func(in *AGYNativeChildAcceptance) {
				in.Evidence.SubscriptionQuotaVerified = false
			},
			wantCode:   "ZERO_COST_UNVERIFIED",
			wantReason: "AGY_CHILD_COST_UNVERIFIED",
		},
		{
			name: "missing cost policy",
			mutate: func(in *AGYNativeChildAcceptance) {
				in.Evidence.Cost.Policy = ""
			},
			wantCode:   "ZERO_COST_UNVERIFIED",
			wantReason: "AGY_CHILD_COST_UNVERIFIED",
		},
		{
			name: "mismatched cost policy",
			mutate: func(in *AGYNativeChildAcceptance) {
				in.Evidence.Cost.Policy = "FREE_ONLY"
			},
			wantCode:   "ZERO_COST_UNVERIFIED",
			wantReason: "AGY_CHILD_COST_UNVERIFIED",
		},
		{
			name: "negative input tokens",
			mutate: func(in *AGYNativeChildAcceptance) {
				in.Evidence.InputTokens = -1
			},
			wantCode:   "INVALID_OUTPUT",
			wantReason: "AGY_CHILD_TERMINAL_UNVERIFIED",
		},
		{
			name: "negative output tokens",
			mutate: func(in *AGYNativeChildAcceptance) {
				in.Evidence.OutputTokens = -1
			},
			wantCode:   "INVALID_OUTPUT",
			wantReason: "AGY_CHILD_TERMINAL_UNVERIFIED",
		},
		{
			name: "input plus output exceeds total tokens",
			mutate: func(in *AGYNativeChildAcceptance) {
				in.Evidence.InputTokens = 10
				in.Evidence.OutputTokens = 6
				in.Evidence.TotalTokens = 15
			},
			wantCode:   "INVALID_OUTPUT",
			wantReason: "AGY_CHILD_TERMINAL_UNVERIFIED",
		},
		{
			name: "zero total tokens",
			mutate: func(in *AGYNativeChildAcceptance) {
				in.Evidence.InputTokens = 0
				in.Evidence.OutputTokens = 0
				in.Evidence.TotalTokens = 0
			},
			wantCode:   "INVALID_OUTPUT",
			wantReason: "AGY_CHILD_TERMINAL_UNVERIFIED",
		},
		{
			name: "mismatched expected parent hash",
			mutate: func(in *AGYNativeChildAcceptance) {
				in.ExpectedParentHash = model.Hash([]byte("wrong-session"))
			},
			wantCode:   "INVALID_OUTPUT",
			wantReason: "AGY_CHILD_PARENT_OWNERSHIP_INVALID",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := valid()
			tc.mutate(&in)
			err := ValidateAGYNativeChildAcceptance(in)
			var fault *model.Fault
			if !errors.As(err, &fault) || fault.Code != tc.wantCode || fault.ProviderReason != tc.wantReason {
				t.Fatalf("ValidateAGYNativeChildAcceptance = %+v, want %s/%s", err, tc.wantCode, tc.wantReason)
			}
		})
	}

	// Offline schema acceptance must NOT assert live G-013 availability or open the AGY gate.
	reg := New(model.Config{})
	c := reg.Candidates[0]
	if c.Provider != "agy" || c.Status == "available" || AllowsNativeChild(c, model.Task{NativeChild: true}) {
		t.Fatalf("offline acceptance must keep AGY G-013 gated, got candidate %+v", c)
	}
	reg154 := New(model.Config{Providers: []model.ProviderConfig{{
		ID: "agy", Model: agySyntheticModel, ExpectedBinarySHA256: agy154Fingerprint, EnableSynthetic: true,
	}}})
	c154 := reg154.Candidates[0]
	if c154.Status == "available" || AllowsNativeChild(c154, model.Task{NativeChild: true}) {
		t.Fatalf("configured AGY 1.5.4 without verified binary must keep G-013 gated, got %+v", c154)
	}
	cap154 := model.Capability{
		Provider:    "agy",
		Model:       agySyntheticModel,
		Fingerprint: agy154Fingerprint,
		Status:      "available",
		Mode:        "headless",
		Profiles:    agyProfiles(),
	}
	if AGYIdentityMatches(cap154.Model, cap154.Fingerprint) || AllowsNativeChild(cap154, model.Task{NativeChild: true}) {
		t.Fatalf("documented blocked AGY 1.5.4 identity must remain gated for native child, got %+v", cap154)
	}
	_, runErr := reg.Run(t.Context(), t.TempDir(), cap154, model.Task{DataClass: "synthetic", Mode: "read_only", NativeChild: true}, nil, nil)
	var gateFault *model.Fault
	if !errors.As(runErr, &gateFault) || gateFault.Code != "POLICY_DENIED" {
		t.Fatalf("expected POLICY_DENIED for AGY 1.5.4 native child gate, got %v", runErr)
	}
	cap153 := model.Capability{
		Provider:    "agy",
		Model:       agySyntheticModel,
		Fingerprint: agy153Fingerprint,
		Status:      "available",
		Mode:        "headless",
		Profiles:    agyProfiles(),
	}
	if AllowsNativeChild(cap153, model.Task{NativeChild: true, NativeChildBackground: true}) {
		t.Fatal("background native child must remain gated for AGY")
	}

	t.Run("attempt verifier derives parent hash from stream session", func(t *testing.T) {
		tampered := valid()
		tampered.Evidence.NativeChild.ParentSHA256 = model.Hash([]byte("tampered-parent"))
		err := verifyAGYAttemptNativeChild(&tampered.Evidence, "main", "42")
		var fault *model.Fault
		if !errors.As(err, &fault) || fault.Code != "INVALID_OUTPUT" || fault.ProviderReason != "AGY_CHILD_PARENT_OWNERSHIP_INVALID" {
			t.Fatalf("verifyAGYAttemptNativeChild = %+v, want INVALID_OUTPUT/AGY_CHILD_PARENT_OWNERSHIP_INVALID", err)
		}
		if tampered.Evidence.TerminalVerified {
			t.Fatal("failed attempt verification must clear TerminalVerified")
		}
		clean := valid()
		if err := verifyAGYAttemptNativeChild(&clean.Evidence, "main", "42"); err != nil {
			t.Fatalf("verifyAGYAttemptNativeChild rejected matching session: %v", err)
		}
	})
}
