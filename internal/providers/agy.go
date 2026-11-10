// Copyright 2026 Furkan Anıl Öztaş
// SPDX-License-Identifier: Apache-2.0

package providers

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/faoztas/ahenk-mcp/internal/execution"
	"github.com/faoztas/ahenk-mcp/internal/model"
)

const (
	agy151Fingerprint = "1f72c9a1c29b768fedd6d33150ebe5baa0f0692242c6286bd1669a1d183ec2d9"
	agy153Fingerprint = "dfaf54852553d89ee8f38b54502deb379f9fe3f194c7f7ea5db05466e4c4bd59"
	agy154Fingerprint = "80abff019289adf481215c544da080d6d5a31d2ef1af4cea1e42fa9aeb673e22"
	agySignature      = `anchor apple generic and certificate leaf[subject.OU] = "EQHXZ8M8AV"`
	agySmokeAllowed   = "ALLOW_RESTRICTED_SYNTHETIC"
)

// AGYIdentityMatches identifies the audited pilot model and executable.
func AGYIdentityMatches(modelID, fingerprint string) bool {
	return modelID == agySyntheticModel && fingerprint == agy153Fingerprint
}

// AGYWorkerIdentityMatches identifies the signed base worker binaries.
// Profile admission and evidence are checked separately for each version.
func AGYWorkerIdentityMatches(modelID, fingerprint string) bool {
	return AGYIdentityMatches(modelID, fingerprint) || modelID == agySyntheticModel && fingerprint == agy154Fingerprint
}

func Family(provider string) string {
	switch provider {
	case "agy-headless", "agy-acp", "daniiislo":
		return "agy"
	default:
		return provider
	}
}

func AllowsHandoff(c model.Capability, t model.Task) bool {
	return c.AuditValidUntil == nil && t.DataClass == "synthetic" && (c.Mode == "fixture" ||
		c.Provider == "opencode" && c.Mode == "tool_free" && t.Mode == "read_only" ||
		c.Provider == "agy" && AllowsAGYProfile(c, t))
}

func agyProfiles() []model.ExecutionProfile {
	return []model.ExecutionProfile{
		{Name: "restricted", Status: "unverified", Reason: "CONTEXT_AND_MESSAGE_SCOPE_UNVERIFIED"},
		{Name: "analysis", Status: "unverified", Reason: "SCOPED_SNAPSHOT_PILOT_UNAVAILABLE"},
		{Name: "review", Status: "unverified", Reason: "SCOPED_SNAPSHOT_PILOT_UNAVAILABLE"},
		{Name: "implementation", Status: "disabled", Reason: "WRITER_ISOLATION_UNVERIFIED"},
		{Name: model.TrustedLocalProfile, Status: "unverified", Reason: "TRUSTED_LOCAL_REPO_OPT_IN_REQUIRED"},
	}
}

func AllowsAGYProfile(c model.Capability, t model.Task) bool {
	if c.Fingerprint == agy154Fingerprint && t.ExecutionProfile != model.TrustedLocalProfile &&
		(t.Mode != "read_only" || t.ExecutionProfile == "review") {
		return false
	}
	name := t.ExecutionProfile
	if name == "" {
		switch {
		case t.Mode == "patch" || slices.ContainsFunc(c.Profiles, func(p model.ExecutionProfile) bool {
			return p.Name == "restricted" && p.Status == "preflight_required"
		}):
			name = "restricted"
		case t.TaskClass == "review":
			name = "review"
		default:
			name = "analysis"
		}
	}
	if t.DataClass != "synthetic" || (name == "analysis" || name == "review") && t.Mode != "read_only" {
		return false
	}
	return slices.ContainsFunc(c.Profiles, func(p model.ExecutionProfile) bool {
		return p.Name == name && (p.Status == "verified" || name != "implementation" && p.Status == "preflight_required")
	})
}

func agyUnverifiedCost() *model.CostEvidence {
	return &model.CostEvidence{Policy: "SUBSCRIPTION_ONLY", Status: "ZERO_COST_UNVERIFIED", Documented: "UNKNOWN", RuntimeVerified: "UNKNOWN"}
}

func agyArgs(modelID, work string) ([]string, error) {
	settings := map[string]any{
		"useG1Credits": false, "modelProvider": "", "agentMode": "", "toolPermission": "request-review",
		"allowNonWorkspaceAccess": false, "disableSlashCommands": work != "",
		"permissions": map[string]any{"deny": []string{"read_file(*)", "write_file(*)", "command(*)", "read_url(*)", "execute_url(*)", "mcp(*)"}},
	}
	if work != "" {
		settings["trustedWorkspaces"] = []string{work}
	}
	data, err := json.Marshal(settings)
	if err != nil {
		return nil, err
	}
	return []string{"--model", modelID, "--settings", string(data), "--log-file", os.DevNull}, nil
}

type AGYProbe struct {
	Fingerprint     string              `json:"fingerprint"`
	Model           string              `json:"model"`
	UsageCommand    bool                `json:"usage_command_succeeded"`
	Reason          string              `json:"reason"`
	Admission       string              `json:"admission"`
	AdmissionReason string              `json:"admission_reason,omitempty"`
	Cost            *model.CostEvidence `json:"cost"`
	Quotas          []Quota             `json:"quotas,omitempty"`
}

func verifyAGY(ctx context.Context, config model.ProviderConfig) (string, error) {
	if config.ID != "agy" || (config.ExpectedBinarySHA256 != agy151Fingerprint && config.ExpectedBinarySHA256 != agy153Fingerprint && config.ExpectedBinarySHA256 != agy154Fingerprint) {
		return "", model.Error("BINARY_UNVERIFIED", "AGY requires a supported pinned distribution")
	}
	return verifyAGYSignedBinary(ctx, config)
}

func verifyAGYSignedBinary(ctx context.Context, config model.ProviderConfig) (string, error) {
	fp, err := Fingerprint(config.Path)
	if err != nil || fp != config.ExpectedBinarySHA256 {
		return fp, model.Error("BINARY_CHANGED", "AGY executable does not match the audited binary")
	}
	if err := exec.CommandContext(ctx, "/usr/bin/codesign", "--verify", "--strict", "-R="+agySignature, config.Path).Run(); err != nil {
		return fp, model.Error("BINARY_UNVERIFIED", "AGY executable failed signature verification")
	}
	return fp, nil
}

// ProbeAGY calls the local /usage command without model inference. Its
// subscription windows do not attest the effective headless credit setting.
func ProbeAGY(ctx context.Context, config model.ProviderConfig, dir string) (AGYProbe, error) {
	return probeAGY(ctx, config, dir, false)
}

func probeAGY(ctx context.Context, config model.ProviderConfig, dir string, audited bool) (AGYProbe, error) {
	out := AGYProbe{Model: config.Model, Admission: "BLOCKED", Cost: agyUnverifiedCost()}
	if !model.ValidID(config.Model) {
		return out, model.Error("MODEL_UNVERIFIED", "AGY probe requires an exact model")
	}
	fp, err := verifyAGY(ctx, config)
	out.Fingerprint = fp
	if err != nil {
		return out, err
	}
	args, err := agyArgs(config.Model, "")
	if err != nil {
		return out, err
	}
	args = append(args, "--print-timeout", "8s", "--output-format", "json", "-p", "/usage")
	cmd := exec.Command(config.Path, args...)
	stdout, stderr, err := runUsage(ctx, cmd, dir)
	checkCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 4*time.Second)
	defer cancel()
	if _, check := verifyAGY(checkCtx, config); check != nil {
		return out, check
	}
	if ctx.Err() != nil {
		return out, ctx.Err()
	}
	if err == execution.ErrCleanup {
		return out, err
	}
	now := time.Now().UTC()
	out.Reason, out.AdmissionReason = "USAGE_UNAVAILABLE", "ZERO_COST_UNVERIFIED"
	if code := agyFailure(string(stderr)); code == "AUTH_REQUIRED" || code == "PERMISSION_DENIED" {
		out.Reason, out.AdmissionReason = code, code
		return out, nil
	}
	usage, ok := decodeAGYUsage(stdout, now)
	if err != nil || !ok {
		if agyFailure(string(stdout)) == "AUTH_REQUIRED" {
			out.Reason, out.AdmissionReason = "AUTH_REQUIRED", "AUTH_REQUIRED"
		}
		return out, nil
	}
	out.UsageCommand, out.Quotas = true, usage
	out.Reason = "EFFECTIVE_CREDITS_SETTING_NOT_ATTESTED"
	switch {
	case !config.EnableSynthetic || config.ExpectedBinarySHA256 != agy153Fingerprint && config.ExpectedBinarySHA256 != agy154Fingerprint:
		out.AdmissionReason = "POLICY_DENIED"
	case config.Model != agySyntheticModel && (!audited || AGYModelFamily(config.Model) == ""):
		out.AdmissionReason = "MODEL_UNVERIFIED"
	default:
		reserve := 0.0
		if audited {
			reserve = auditQuotaReserve
		}
		if fault := checkAGYFamilyQuota(out.Quotas, AGYModelFamily(config.Model), reserve, now); fault != nil {
			out.AdmissionReason = fault.Code
			break
		}
		out.Admission, out.AdmissionReason = agySmokeAllowed, ""
		out.Cost = &model.CostEvidence{Policy: "SUBSCRIPTION_ONLY", Status: "DOCUMENTED_COST_PROTECTION",
			Documented: "VERIFIED", RuntimeVerified: "UNKNOWN", Account: "SAME_NATIVE_CLI_CONTEXT"}
	}
	return out, nil
}

func decodeAGYUsage(stdout []byte, now time.Time) ([]Quota, bool) {
	var response struct {
		Command       json.RawMessage   `json:"command"`
		Status        string            `json:"status"`
		DeniedActions []json.RawMessage `json:"denied_actions"`
	}
	if json.Unmarshal(stdout, &response) != nil || response.Status != "SUCCESS" || len(response.DeniedActions) != 0 {
		return nil, false
	}
	var command struct {
		Name string `json:"name"`
		Data struct {
			Groups []struct {
				Buckets []struct {
					ID        string     `json:"id"`
					Remaining *float64   `json:"remaining_fraction"`
					Reset     *time.Time `json:"reset_time"`
				} `json:"buckets"`
			} `json:"groups"`
		} `json:"data"`
	}
	if json.Unmarshal(response.Command, &command) != nil {
		if json.Unmarshal(response.Command, &command.Name) != nil {
			return nil, false
		}
	}
	if strings.TrimPrefix(command.Name, "/") != "usage" {
		return nil, false
	}
	var quotas []Quota
	seen := map[string]bool{}
	for _, group := range command.Data.Groups {
		for _, bucket := range group.Buckets {
			family, window := agyBucket(bucket.ID)
			if family == "" {
				continue
			}
			if seen[bucket.ID] {
				return nil, false
			}
			seen[bucket.ID] = true
			q := Quota{Provider: "agy", Bucket: bucket.ID, Family: family, WindowMinutes: window, Unit: "percent",
				State: "UNKNOWN", Source: "agy_usage", AccountMatch: "SAME_NATIVE_CLI_CONTEXT", ObservedAt: now, ResetAt: bucket.Reset}
			if bucket.Remaining != nil && *bucket.Remaining >= 0 && *bucket.Remaining <= 1 {
				remaining := *bucket.Remaining * 100
				q.State, q.Remaining = "KNOWN", &remaining
				if remaining == 0 {
					q.State = "EXHAUSTED"
				}
			}
			quotas = append(quotas, q.At(now))
		}
	}
	return quotas, true
}

type AGYPolicy struct {
	NativeChildRole              string
	WritePaths                   []string
	Root                         string
	Agent                        string
	Profile                      string
	Paths                        []string
	Tools                        []string
	Commands                     []string
	ChildAndMessageScopeVerified bool
	WriterIsolationVerified      bool
}

type AGYResult struct {
	Model      string
	Session    string
	Text       string
	Structured json.RawMessage
	Usage      AGYUsage
}

type AGYUsage struct {
	Input  int64 `json:"input_tokens"`
	Output int64 `json:"output_tokens"`
	Total  int64 `json:"total_tokens"`
}

type agyTool struct {
	Name          string          `json:"name"`
	RawParameters json.RawMessage `json:"parameters"`
	Output        json.RawMessage `json:"output"`
	Error         *struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

type agyChild struct {
	TypeName     string   `json:"type_name"`
	Role         string   `json:"role"`
	Conversation string   `json:"conversation_id"`
	Workspaces   []string `json:"workspace_uris"`
}

func (p AGYPolicy) nativeChildLink(step agyStep) string {
	if len(step.Subagent.Children) != 1 {
		return "CHILD_METADATA_MISSING_OR_MULTIPLE"
	}
	child := step.Subagent.Children[0]
	if !model.ValidID(child.Conversation) {
		return "CHILD_ID_MISSING"
	}
	if len(child.Workspaces) != 1 {
		return "CHILD_WORKSPACE_MISSING_OR_MULTIPLE"
	}
	if !p.scopedNativeChild(step, map[string]bool{step.Session: true}) {
		return "CHILD_OWNERSHIP_UNVERIFIED"
	}
	return "CHILD_OWNERSHIP_VERIFIED"
}

func agyNativeInvocationFailure(state, message string) (string, string) {
	message = strings.ToLower(message)
	code := agyFailure(message)
	switch {
	case state == "DENIED" || code == "PERMISSION_DENIED":
		return "PERMISSION_DENIED", "AGY_CHILD_PERMISSION_DENIED"
	case strings.Contains(message, "unknown agent"), strings.Contains(message, "unknown subagent"),
		strings.Contains(message, "agent not found"), strings.Contains(message, "not a valid subagent"),
		strings.Contains(message, "invalid agent"), strings.Contains(message, "not eligible"):
		return "MODEL_UNAVAILABLE", "AGY_CHILD_CONFIGURATION_INVALID"
	case strings.Contains(message, "unsupported"), strings.Contains(message, "not supported"):
		return "MODEL_UNAVAILABLE", "AGY_CHILD_INVOCATION_UNSUPPORTED"
	case strings.Contains(message, "timeout"), strings.Contains(message, "deadline exceeded"):
		return "ATTEMPT_TIMEOUT", "AGY_CHILD_TIMEOUT"
	case code != "INVALID_OUTPUT":
		return code, "AGY_CHILD_PROVIDER_FAILURE"
	default:
		return "INVALID_OUTPUT", "AGY_CHILD_INVOCATION_ERROR"
	}
}

func agyFailure(s string) string {
	s = strings.ToLower(s)
	switch {
	case strings.Contains(s, "soft-denied"), strings.Contains(s, "soft_denied"), strings.Contains(s, "permission denied"),
		strings.Contains(s, "permission required"), strings.Contains(s, "permission_denied"),
		strings.Contains(s, "permission check failed"), strings.Contains(s, "user denied permission"),
		strings.Contains(s, "permission") && (strings.Contains(s, "cannot prompt") || strings.Contains(s, "not allowed in headless")):
		return "PERMISSION_DENIED"
	case strings.Contains(s, "authentication required"), strings.Contains(s, "auth_required"), strings.Contains(s, "not logged in"):
		return "AUTH_REQUIRED"
	case strings.Contains(s, "rate limit"), strings.Contains(s, "quota exceeded"), strings.Contains(s, "quota exhausted"),
		strings.Contains(s, "resource_exhausted"):
		return "RATE_LIMIT"
	default:
		return "INVALID_OUTPUT"
	}
}

func (p AGYPolicy) inScope(name string) bool {
	if !filepath.IsAbs(name) {
		return model.SafePath(name) && model.InScope(name, p.Paths)
	}
	relative, inside := model.Within(p.Root, name)
	return inside && filepath.Clean(name) == name && model.SafePath(relative) && model.InScope(relative, p.Paths)
}

func agyProviderFault(code, source, message string) *model.Fault {
	fault := model.Error(code, message)
	fault.ProviderReason = source
	return fault
}

// This checks observed effects. A live route also needs pre-dispatch enforcement
// evidence because observing an event cannot undo a tool action.
func (p AGYPolicy) checkTool(step agyStep, sessions map[string]bool) bool {
	name, tool, children := step.ToolName, step.Tool, step.Subagent.Children
	if tool.Name != "" && tool.Name != name || (name != "invoke_subagent" && len(children) != 0) {
		return false
	}
	if name == "finish" || p.Profile == "restricted" {
		// Finish only returns the response for the already-owned session.
		return name == "finish" && step.Kind == "tool" && step.safeFinish()
	}
	if !slices.Contains(p.Tools, name) {
		return false
	}
	switch name {
	case "read_file":
		return p.Profile == model.TrustedLocalProfile && p.trustedFileCall(step, false)
	case "view_file", "list_dir", "grep_search", "find_by_name":
		target := tool.param("AbsolutePath", "DirectoryPath", "SearchPath")
		return target != "" && p.inScope(target)
	case "write_file", "edit_file", "replace_file":
		return p.Profile == model.TrustedLocalProfile && p.trustedFileCall(step, true)
	case "write_to_file", "replace_file_content", "multi_replace_file_content":
		target := tool.param("TargetFile", "AbsolutePath")
		if p.Profile == model.TrustedLocalProfile {
			return p.trustedFileCall(step, true)
		}
		return p.Profile == "implementation" && p.WriterIsolationVerified && p.inScope(target)
	case "run_command":
		return (p.Profile == "implementation" || p.Profile == "review") && p.WriterIsolationVerified &&
			slices.Contains(p.Commands, tool.param("CommandLine")) && tool.param("Cwd") == p.Root
	case "invoke_subagent":
		if p.NativeChildRole != "" {
			return p.scopedNativeChild(step, sessions)
		}
		if !p.ChildAndMessageScopeVerified || len(sessions) >= 2 {
			return false
		}
		if step.State == "ACTIVE" && len(children) == 0 {
			return true
		}
		if len(children) != 1 ||
			!model.ValidID(children[0].Conversation) || sessions[children[0].Conversation] || len(children[0].Workspaces) != 1 {
			return false
		}
		u, err := url.Parse(children[0].Workspaces[0])
		if err != nil || u.Scheme != "file" || u.Host != "" || u.Path != p.Root || u.RawQuery != "" || u.Fragment != "" {
			return false
		}
		sessions[children[0].Conversation] = true
		return true
	case "send_message":
		return p.ChildAndMessageScopeVerified && sessions[tool.param("TargetConversationId", "TargetAgentId", "ConversationId", "conversation_id")]
	default:
		return false
	}
}

func (p AGYPolicy) scopedNativeChild(step agyStep, sessions map[string]bool) bool {
	if !p.ChildAndMessageScopeVerified || len(step.Subagent.Children) > 1 {
		return false
	}
	if len(step.Subagent.Children) == 0 {
		role := step.Tool.param("SubagentType", "AgentType", "TypeName", "subagent_type", "agent_type", "type_name", "agent", "name")
		workspace := step.Tool.param("WorkspaceMode", "Workspace", "workspace_mode", "workspace")
		return step.State == "ACTIVE" && role == p.NativeChildRole && (workspace == "" || workspace == "inherit")
	}
	child := step.Subagent.Children[0]
	role := child.TypeName
	if role == "" {
		role = child.Role
	}
	if role != "" && role != p.NativeChildRole {
		return false
	}
	if len(child.Workspaces) != 0 {
		if len(child.Workspaces) != 1 {
			return false
		}
		u, err := url.Parse(child.Workspaces[0])
		if err != nil || u.Scheme != "file" || u.Host != "" || u.Path != p.Root || u.RawQuery != "" || u.Fragment != "" {
			return false
		}
	}
	if role == "" || !model.ValidID(child.Conversation) || len(child.Workspaces) != 1 {
		return step.State == "ACTIVE"
	}
	if child.Conversation == step.Session || len(sessions) >= 2 && !sessions[child.Conversation] {
		return false
	}
	sessions[child.Conversation] = true
	return true
}

var agyPathKeys = []string{"TargetFile", "AbsolutePath", "FilePath", "Path", "path", "file_path",
	"absolute_path", "filePath", "absolutePath", "file", "target_file", "targetFile", "target_path", "targetPath",
	"filepath", "filename", "file_name", "relative_path", "relativePath", "paths", "file_paths", "filePaths", "files"}

func (p AGYPolicy) trustedFileCall(step agyStep, write bool) bool {
	raw := bytes.TrimSpace(step.Tool.RawParameters)
	if len(raw) != 0 && raw[0] == '"' {
		var decoded string
		if json.Unmarshal(raw, &decoded) != nil {
			return false
		}
		raw = []byte(decoded)
	}
	var fields map[string]json.RawMessage
	if len(raw) > 0 && json.Unmarshal(raw, &fields) != nil {
		return false
	}
	// The ACTIVE notification can precede argument assembly. A completed
	// call always needs an observed target; attempt permissions still apply.
	if len(fields) == 0 {
		return step.State == "ACTIVE"
	}
	var paths []string
	for _, key := range agyPathKeys {
		value, ok := fields[key]
		if !ok {
			continue
		}
		var path string
		if err := json.Unmarshal(value, &path); err == nil && path != "" {
			paths = append(paths, path)
			continue
		}
		var list []string
		if write || json.Unmarshal(value, &list) != nil || len(list) == 0 {
			return false
		}
		paths = append(paths, list...)
	}
	if len(paths) == 0 {
		return false
	}
	var target string
	for _, name := range paths {
		if strings.HasPrefix(name, "file://") {
			u, err := url.Parse(name)
			if err != nil || u.Host != "" || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" {
				return false
			}
			name = u.Path
		}
		if !p.inScope(name) {
			return false
		}
		if write {
			if filepath.IsAbs(name) {
				name, _ = model.Within(p.Root, name)
			}
			if target != "" && target != name || !slices.Contains(p.WritePaths, name) {
				return false
			}
			target = name
		}
	}
	return true
}

func agyParameterShape(raw json.RawMessage) string {
	if len(raw) == 0 {
		return "missing"
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return "non-object"
	}
	if len(fields) > 32 {
		return "object:many-keys"
	}
	keys := make([]string, 0, len(fields))
	for key := range fields {
		if slices.Contains(agyPathKeys, key) {
			keys = append(keys, key)
		} else {
			keys = append(keys, model.Hash([]byte(key))[:12])
		}
	}
	slices.Sort(keys)
	return "object:" + strings.Join(keys, ",")
}

// The pilot requests only the summary and patch strings in its output schema.
func agyFinishParametersAllowed(raw json.RawMessage) bool {
	if len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return true
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	if start, err := dec.Token(); err != nil || start != json.Delim('{') {
		return false
	}
	seen := map[string]bool{}
	for dec.More() {
		key, err := dec.Token()
		name, isString := key.(string)
		if err != nil || !isString || (name != "summary" && name != "patch") || seen[name] {
			return false
		}
		seen[name] = true
		value, err := dec.Token()
		if _, isString := value.(string); err != nil || !isString {
			return false
		}
	}
	end, err := dec.Token()
	return err == nil && end == json.Delim('}')
}

func (t agyTool) param(keys ...string) string {
	var fields map[string]json.RawMessage
	if json.Unmarshal(t.RawParameters, &fields) != nil {
		return ""
	}
	var value string
	for _, key := range keys {
		if field, ok := fields[key]; ok {
			var candidate string
			if json.Unmarshal(field, &candidate) != nil || candidate == "" || (value != "" && value != candidate) {
				return ""
			}
			value = candidate
		}
	}
	return value
}

type agyEvent struct {
	Event   string `json:"event"`
	Session string `json:"conversation_id"`
	Init    struct {
		CWD            string    `json:"cwd"`
		Model          string    `json:"model"`
		Agent          string    `json:"agent"`
		PermissionMode string    `json:"permission_mode"`
		Tools          *[]string `json:"tools"`
	} `json:"init"`
	Step   agyStep `json:"step_update"`
	Result struct {
		Session       string            `json:"conversation_id"`
		Status        string            `json:"status"`
		Response      string            `json:"response"`
		Error         string            `json:"error"`
		Structured    json.RawMessage   `json:"structured_output"`
		DeniedActions []json.RawMessage `json:"denied_actions"`
		Usage         *AGYUsage         `json:"usage"`
	} `json:"result"`
}

type agyStep struct {
	Index     *int    `json:"step_index"`
	Session   string  `json:"conversation_id"`
	Kind      string  `json:"step_type"`
	State     string  `json:"state"`
	TextDelta string  `json:"text_delta"`
	ToolName  string  `json:"tool_name"`
	Tool      agyTool `json:"tool_info"`
	Subagent  struct {
		Children []agyChild `json:"subagents"`
	} `json:"subagent_info"`
}

func (s agyStep) safeFinish() bool {
	return (s.ToolName == "" || s.ToolName == "finish") && (s.Tool.Name == "" || s.Tool.Name == "finish") &&
		len(s.Subagent.Children) == 0 && agyFinishParametersAllowed(s.Tool.RawParameters)
}

// Persist known protocol labels only. Tool arguments and provider text can
// contain sensitive data and do not belong in the attempt record.
func (s agyStep) rejectionLabel() string {
	switch s.ToolName {
	case "finish", "ask_permission", "ask_custom_permission", "ask_question",
		"send_message", "invoke_subagent", "view_file", "list_dir", "find_by_name", "grep_search",
		"run_command", "write_to_file", "replace_file_content", "multi_replace_file_content",
		"read_file", "write_file", "edit_file", "replace_file", "manage_task", "sed_file", "list_permissions", "list_resources", "read_resource", "call_mcp_tool",
		"define_subagent", "manage_subagents", "command_status", "run_workflow", "wait":
		return s.ToolName
	case "":
		switch s.Kind {
		case "user_input", "agent_response", "checkpoint", "finish", "system_message", "error_message", "unknown":
			return s.Kind
		}
	}
	return "other"
}

func (e agyEvent) describe() *model.AGYStreamEventEvidence {
	info := &model.AGYStreamEventEvidence{Event: "other"}
	switch e.Event {
	case "init":
		info.Event = "init"
	case "step_update":
		info.Event, info.Step = "step_update", e.Step.rejectionLabel()
		info.State = "other"
		switch e.Step.State {
		case "ACTIVE", "DONE", "ERROR", "DENIED":
			info.State = e.Step.State
		}
		info.HasToolParameters = len(e.Step.Tool.RawParameters) != 0
		info.HasToolOutput = len(e.Step.Tool.Output) != 0
		info.HasChildMetadata = len(e.Step.Subagent.Children) != 0
	case "result":
		info.Event = "result"
		info.HasUsage = e.Result.Usage != nil
		info.HasStructuredOutput = len(e.Result.Structured) != 0 && !bytes.Equal(e.Result.Structured, []byte("null"))
	}
	return info
}

type agyStreamState struct {
	childInvocation *int
	childText       strings.Builder
	result          AGYResult
	sessions        map[string]bool
	finished        bool
}

func (s *agyStreamState) accept(line []byte, expectedModel string, policy AGYPolicy, evidence *model.AGYRunEvidence) error {
	var e agyEvent
	err := json.Unmarshal(line, &e)
	if policy.NativeChildRole != "" && e.Event == "step_update" && e.Step.Kind == "subagent" &&
		e.Step.ToolName == "" && len(e.Step.Subagent.Children) != 0 {
		e.Step.ToolName = "invoke_subagent"
	}
	if policy.Profile == model.TrustedLocalProfile && e.Event == "step_update" && e.Step.Kind == "tool" && e.Step.ToolName == "" {
		e.Step.ToolName = e.Step.Tool.Name
	}
	if evidence != nil {
		if err == nil {
			evidence.LastEvent = e.describe()
			if policy.Profile == model.TrustedLocalProfile && e.Step.Kind == "tool" {
				evidence.LastEvent.ToolLabel = agyToolLabel(e.Step.ToolName)
				evidence.LastEvent.NestedToolLabel = agyToolLabel(e.Step.Tool.Name)
				evidence.LastEvent.ParameterShape = agyParameterShape(e.Step.Tool.RawParameters)
			}
		} else {
			evidence.LastEvent = &model.AGYStreamEventEvidence{Event: "malformed"}
		}
	}
	if s.result.Session == "" && (err != nil || e.Event != "init") {
		if code := agyFailure(e.Result.Error); e.Event == "result" && code != "INVALID_OUTPUT" {
			return agyProviderFault(code, "AGY_STARTUP_REJECTED", "AGY rejected startup")
		}
		return agyProviderFault("AGY_INIT_INVALID", "AGY_STARTUP_EVENT_INVALID", "AGY did not report a valid startup event")
	}
	if err != nil {
		return agyProviderFault("INVALID_OUTPUT", "AGY_EVENT_INVALID_JSON", "invalid AGY event JSON")
	}
	if s.finished {
		return agyProviderFault("INVALID_OUTPUT", "AGY_EVENT_AFTER_RESULT", "unexpected AGY event after result")
	}
	switch e.Event {
	case "init":
		if s.result.Session != "" || !model.ValidID(e.Session) || e.Init.Tools == nil || len(*e.Init.Tools) > 256 {
			return agyProviderFault("AGY_INIT_INVALID", "AGY_STARTUP_EVENT_INVALID", "AGY did not report a valid startup event")
		}
		if evidence != nil {
			if model.ValidID(e.Init.Model) {
				evidence.Model = e.Init.Model
			}
			if model.ValidID(e.Init.PermissionMode) {
				evidence.PermissionMode = e.Init.PermissionMode
			}
			// AGY reports its tool registry. Permission comes from the
			// selected agent and the observed actions within this attempt.
			for _, tool := range *e.Init.Tools {
				if model.ValidID(tool) {
					evidence.Tools = append(evidence.Tools, tool)
				}
			}
		}
		if e.Init.CWD != policy.Root || e.Init.Model != expectedModel || e.Init.PermissionMode != "request-review" ||
			policy.Agent != "" && e.Init.Agent != policy.Agent {
			return agyProviderFault("AGY_INIT_MISMATCH", "AGY_STARTUP_EVENT_MISMATCH", "AGY startup did not match the model, directory, agent, or permission mode")
		}
		if policy.NativeChildRole != "" && !slices.Contains(*e.Init.Tools, "invoke_subagent") {
			return model.Error("MODEL_UNAVAILABLE", "AGY did not advertise native child invocation")
		}
		s.sessions = map[string]bool{e.Session: true}
		s.result.Model, s.result.Session = e.Init.Model, e.Session
		if evidence != nil {
			evidence.StartupVerified = true
		}
	case "step_update":
		step := e.Step
		if !s.sessions[step.Session] {
			if evidence != nil {
				evidence.RejectedStep = step.rejectionLabel()
			}
			return agyProviderFault("PERMISSION_DENIED", "AGY_UNOWNED_SESSION", "AGY step belongs to an unowned session")
		}
		if policy.NativeChildRole != "" && step.Session != s.result.Session &&
			(step.Kind == "tool" || step.Kind == "subagent") && step.ToolName != "finish" {
			return model.Error("PERMISSION_DENIED", "AGY child attempted an unauthorized tool")
		}
		if policy.NativeChildRole != "" && step.ToolName == "invoke_subagent" && step.State != "ERROR" && step.State != "DENIED" {
			if step.Session != s.result.Session || step.Index == nil || *step.Index < 0 ||
				s.childInvocation != nil && *s.childInvocation != *step.Index {
				return agyProviderFault("PERMISSION_DENIED", "AGY_CHILD_INVOCATION_SCOPE", "AGY native child invocation is not the single owned parent step")
			}
			s.childInvocation = step.Index
		}
		if policy.NativeChildRole != "" && step.ToolName == "invoke_subagent" && (step.State == "ERROR" || step.State == "DENIED") {
			if evidence != nil {
				evidence.RejectedStep = "invoke_subagent"
				evidence.LastEvent.ChildLink = policy.nativeChildLink(step)
			}
			message := step.TextDelta + " " + string(step.Tool.Output)
			if step.Tool.Error != nil {
				message += " " + step.Tool.Error.Type + " " + step.Tool.Error.Message
			}
			code, reason := agyNativeInvocationFailure(step.State, message)
			return agyProviderFault(code, reason, "AGY native child invocation failed")
		}
		if step.Tool.Error != nil {
			if evidence != nil {
				evidence.RejectedStep = step.rejectionLabel()
			}
			return agyProviderFault(agyFailure(step.Tool.Error.Type+" "+step.Tool.Error.Message), "AGY_TOOL_ERROR", "AGY tool reported an error")
		}
		switch step.Kind {
		case "user_input", "agent_response", "checkpoint", "system_message":
			if step.ToolName != "" || step.Tool.Name != "" || len(step.Tool.RawParameters) != 0 ||
				len(step.Tool.Output) != 0 || len(step.Subagent.Children) != 0 {
				return agyProviderFault("INVALID_OUTPUT", "AGY_STEP_UNEXPECTED_TOOL", "unexpected AGY tool payload")
			}
			if step.State == "ERROR" || step.State == "DENIED" {
				return agyProviderFault(agyFailure(step.TextDelta), "AGY_STEP_ERROR", "AGY reported a failed step")
			}
			if policy.NativeChildRole != "" && step.Session != s.result.Session && step.Kind == "agent_response" && step.State == "DONE" {
				if s.childText.Len()+len(step.TextDelta) > 16384 || model.ContainsSecret([]byte(step.TextDelta)) {
					return model.Error("INVALID_OUTPUT", "AGY child result is oversized or sensitive")
				}
				s.childText.WriteString(step.TextDelta)
			}
		case "finish":
			// AGY 1.5.3 emits a native finish step separately from tool steps.
			if (step.State != "ACTIVE" && step.State != "DONE") || !step.safeFinish() {
				if evidence != nil {
					evidence.RejectedStep = step.rejectionLabel()
				}
				return agyProviderFault("PERMISSION_DENIED", "AGY_FINISH_SCOPE_DENIED", "AGY finish carried an unsupported payload")
			}
			if evidence != nil && step.State == "DONE" {
				if evidence.NativeChild != nil && step.Session != s.result.Session {
					s.finishChild(step, evidence)
				} else {
					evidence.NativeFinishObserved = true
				}
			}
		case "error_message":
			return agyProviderFault(agyFailure(step.TextDelta), "AGY_STEP_ERROR", "AGY reported an error step")
		case "tool", "subagent":
			if (step.State != "ACTIVE" && step.State != "DONE") || !policy.checkTool(step, s.sessions) {
				if evidence != nil {
					evidence.RejectedStep = step.rejectionLabel()
				}
				return agyProviderFault("PERMISSION_DENIED", "AGY_TOOL_PROFILE_DENIED", "AGY tool exceeded the execution profile")
			}
			if evidence != nil && policy.NativeChildRole != "" && step.ToolName == "invoke_subagent" && len(step.Subagent.Children) == 1 {
				child := step.Subagent.Children[0]
				if evidence.NativeChild == nil && child.Conversation != s.result.Session && s.sessions[child.Conversation] {
					evidence.NativeChild = &model.NativeChildEvidence{ParentSHA256: model.Hash([]byte(s.result.Session)),
						ChildSHA256: model.Hash([]byte(child.Conversation))}
				}
			}
			if evidence != nil && step.State == "DONE" {
				if evidence.ObservedTools == nil {
					evidence.ObservedTools = map[string]int{}
				}
				evidence.ObservedTools[step.ToolName]++
				if evidence.NativeChild != nil && step.ToolName == "send_message" {
					evidence.NativeChild.ScopedMessage = true
				}
				if evidence.NativeChild != nil && step.ToolName == "finish" && step.Session != s.result.Session {
					s.finishChild(step, evidence)
				}
			}
		default:
			return agyProviderFault("INVALID_OUTPUT", "AGY_STEP_UNSUPPORTED", "unknown AGY step")
		}
	case "result":
		if len(e.Result.DeniedActions) != 0 {
			return agyProviderFault("PERMISSION_DENIED", "AGY_TERMINAL_DENIED_ACTIONS", "AGY terminal result reported denied actions")
		}
		if e.Result.Session != s.result.Session || e.Result.Status != "SUCCESS" || e.Result.Error != "" ||
			(e.Result.Response == "" && (len(e.Result.Structured) == 0 || bytes.Equal(e.Result.Structured, []byte("null")))) {
			return agyProviderFault(agyFailure(e.Result.Error), "AGY_TERMINAL_REJECTED", "AGY did not emit a verified success")
		}
		s.finished = true
		s.result.Text, s.result.Structured = e.Result.Response, e.Result.Structured
		if e.Result.Usage != nil {
			s.result.Usage = *e.Result.Usage
		}
	default:
		return agyProviderFault("INVALID_OUTPUT", "AGY_EVENT_UNSUPPORTED", "unknown AGY event")
	}
	return nil
}

func (s *agyStreamState) finishChild(step agyStep, evidence *model.AGYRunEvidence) {
	summary := strings.TrimSpace(step.Tool.param("summary", "Summary"))
	if summary == "" {
		summary = strings.TrimSpace(s.childText.String())
	}
	if summary == "" || len(summary) > 16384 || model.ContainsSecret([]byte(summary)) {
		return
	}
	evidence.NativeChild.ChildFinished = true
	evidence.NativeChild.ResultSHA256 = model.Hash([]byte(summary))
}

func ParseAGY(stdout, stderr []byte, expectedModel string, policy AGYPolicy) (AGYResult, error) {
	var stream agyStreamState
	if code := agyFailure(string(stderr)); code != "INVALID_OUTPUT" {
		return stream.result, agyProviderFault(code, "AGY_STDERR_REJECTED", "AGY reported a rejected operation")
	}
	scanner := bufio.NewScanner(bytes.NewReader(stdout))
	scanner.Buffer(make([]byte, 4096), execution.MaxArtifact)
	for scanner.Scan() {
		if err := stream.accept(scanner.Bytes(), expectedModel, policy, nil); err != nil {
			return stream.result, err
		}
	}
	if scanner.Err() != nil || !stream.finished {
		return stream.result, agyProviderFault("INVALID_OUTPUT", "AGY_TERMINAL_MISSING", "AGY terminal event missing")
	}
	return stream.result, nil
}

// Tool labels are protocol identifiers. Keep opaque labels as hashes and
// exclude all tool parameters and outputs from persisted diagnostics.
func agyToolLabel(name string) string {
	if name == "" {
		return "missing"
	}
	if label := (agyStep{ToolName: name}).rejectionLabel(); label != "other" {
		return label
	}
	return model.Hash([]byte(name))
}
