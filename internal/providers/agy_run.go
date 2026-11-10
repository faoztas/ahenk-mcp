// Copyright 2026 Furkan Anıl Öztaş
// SPDX-License-Identifier: Apache-2.0

package providers

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/faoztas/ahenk-mcp/internal/execution"
	"github.com/faoztas/ahenk-mcp/internal/model"
)

const agySyntheticModel = "gemini-3.8-flash-medium"
const agyRestrictedAgent = "ahenk-restricted"

const agyRestrictedDefinition = `---
name: ahenk-restricted
description: Ahenk synthetic text and patch output
mainAgent: true
subagent: false
tools: [finish]
inheritMcp: false
inheritCustomizations: false
commandExecutionPolicy: "off"
---
Return the requested structured result using only the synthetic text supplied.
`

const agyTrustedAgent = "ahenk-trusted-local"

const agyTrustedDefinition = `---
name: ahenk-trusted-local
description: Edit explicitly approved synthetic Go source files
mainAgent: true
subagent: false
tools: [read_file, edit_file, write_file, finish]
inheritMcp: false
inheritCustomizations: false
commandExecutionPolicy: "off"
---
Read and edit only the specified files in this synthetic worktree. Ahenk runs the registered tests after your edits.
`

const agyNativeParent = "ahenk-native-parent"
const agyNativeChild = "ahenk-native-child"

const agyNativeParentDefinition = `---
name: ahenk-native-parent
description: Delegate one bounded synthetic task to the local child
mainAgent: true
subagent: false
tools: [invoke_subagent, send_message, finish]
inheritMcp: false
inheritCustomizations: false
commandExecutionPolicy: "off"
---
Invoke only ahenk-native-child, once, with the inherited workspace and model.
Return the verified child answer as your summary.
`

const agyNativeChildDefinition = `---
name: ahenk-native-child
description: Return a compact result for a synthetic arithmetic task
mainAgent: false
subagent: true
model: inherit
tools: [finish]
inheritMcp: false
inheritCustomizations: false
commandExecutionPolicy: "off"
---
Use only the task text and finish with the requested answer.
`

type agyRequest struct {
	WeeklyFloor float64
	Audited     bool
	Worktree    string
	WritePaths  []string
	Config      model.ProviderConfig
	Task        model.Task
	Files       map[string]string
	Handoffs    []model.Handoff
	Deadline    time.Time
}

type agyReply struct {
	Output execution.FixtureInput
	Fault  *model.Fault
}

func runAGY(ctx context.Context, dir string, config model.ProviderConfig, t model.Task, files map[string]string, handoffs []model.Handoff) (execution.FixtureInput, error) {
	return superviseAGY(ctx, dir, agyRequest{Config: config, Task: t, Files: files, Handoffs: handoffs})
}

func superviseAGY(ctx context.Context, dir string, request agyRequest) (execution.FixtureInput, error) {
	deadline, ok := ctx.Deadline()
	if !ok {
		return execution.FixtureInput{}, model.Error("POLICY_DENIED", "AGY requires a bounded attempt")
	}
	var reply agyReply
	request.Deadline = deadline
	request.WeeklyFloor = request.Task.AGYWeeklyFloor
	err := execution.RunSupervised(ctx, dir, "_supervise_agy", vendorUsageEnv(nil, dir), request, &reply)
	if err == nil && reply.Fault != nil {
		err = reply.Fault
	}
	return reply.Output, err
}

func SuperviseAGY() error {
	var request agyRequest
	return execution.Supervise(&request, func(owner context.Context) (any, error) {
		ctx, cancel := context.WithDeadline(owner, request.Deadline)
		defer cancel()
		out, err := runAGYAttempt(ctx, request)
		if errors.Is(err, execution.ErrCleanup) {
			return nil, err
		}
		var fault *model.Fault
		if err != nil && !errors.As(err, &fault) {
			if errors.Is(err, context.DeadlineExceeded) {
				fault = model.Error("ATTEMPT_TIMEOUT", "AGY attempt deadline expired")
			} else if errors.Is(err, context.Canceled) {
				fault = model.Error("CANCELLED", "AGY attempt was cancelled")
			} else {
				fault = model.Error("AGY_WORKER_ERROR", "AGY worker could not complete its attempt")
			}
		}
		return agyReply{Output: out, Fault: fault}, nil
	})
}

func runAGYAttempt(ctx context.Context, req agyRequest) (out execution.FixtureInput, result error) {
	out.AGY = &model.AGYRunEvidence{Cost: agyUnverifiedCost()}
	t := req.Task
	trusted := t.ExecutionProfile == model.TrustedLocalProfile
	out.AGY.Profile = t.ExecutionProfile
	if out.AGY.Profile == "" {
		out.AGY.Profile = "restricted"
	}
	audited := req.Audited && t.Mode == "read_only" && !trusted &&
		req.Config.ExpectedBinarySHA256 == agy153Fingerprint && AGYModelFamily(req.Config.Model) != ""
	readOnly154 := req.Config.ExpectedBinarySHA256 == agy154Fingerprint &&
		AGYWorkerIdentityMatches(req.Config.Model, req.Config.ExpectedBinarySHA256) && t.Mode == "read_only" &&
		!trusted && !t.NativeChild && t.ExecutionProfile != "review" && len(req.Handoffs) == 0
	writer154 := trusted && req.Config.ExpectedBinarySHA256 == agy154Fingerprint && AGYWorkerIdentityMatches(req.Config.Model, req.Config.ExpectedBinarySHA256)
	if !req.Config.EnableSynthetic || !audited && !readOnly154 && !writer154 && !AGYIdentityMatches(req.Config.Model, req.Config.ExpectedBinarySHA256) ||
		t.DataClass != "synthetic" ||
		(t.Mode != "read_only" && t.Mode != "patch") ||
		(t.ExecutionProfile != "" && t.ExecutionProfile != "restricted" && !trusted &&
			!(t.Mode == "read_only" && (t.ExecutionProfile == "analysis" || t.ExecutionProfile == "review"))) ||
		trusted && (t.Mode != "patch" || len(req.WritePaths) == 0) {
		return out, model.Error("POLICY_DENIED", "AGY pilot requires a pinned model and a scoped synthetic task")
	}
	dir, err := os.Getwd()
	if err != nil {
		return out, err
	}
	usageDir, err := os.MkdirTemp(dir, "usage-")
	if err != nil {
		return out, err
	}
	defer os.RemoveAll(usageDir)
	usageCtx, stopUsage := context.WithTimeout(ctx, 15*time.Second)
	probe, err := probeAGY(usageCtx, req.Config, usageDir, audited)
	stopUsage()
	if err != nil {
		return out, err
	}
	out.AGY.Cost = probe.Cost
	if probe.Admission != agySmokeAllowed {
		return out, model.Error(probe.AdmissionReason, "AGY subscription-only preflight did not pass")
	}
	out.AGY.SubscriptionQuotaVerified = true
	if q := agyWeeklyReserve(probe.Quotas, AGYModelFamily(req.Config.Model), req.WeeklyFloor); q != nil {
		fault := model.Error("AGY_WEEKLY_RESERVE_PROTECTED", "AGY native weekly quota reached the protected reserve and dispatch buffer")
		fault.ProviderReason = q.Bucket
		out.AGY.WeeklyReserve = q
		return out, fault
	}

	runtime, err := os.MkdirTemp(dir, "agy-")
	if err != nil {
		return out, err
	}
	defer func() {
		if !errors.Is(result, execution.ErrCleanup) {
			os.RemoveAll(runtime)
		}
	}()
	work, agent, definition := runtime, agyRestrictedAgent, agyRestrictedDefinition
	policy := AGYPolicy{Root: work, Agent: agent, Profile: "restricted"}
	if trusted {
		work, agent, definition = req.Worktree, agyTrustedAgent, agyTrustedDefinition
		real, err := filepath.EvalSymlinks(work)
		if err != nil || real != work || work != filepath.Join(dir, ".workspace", "work") {
			return out, model.Error("POLICY_DENIED", "AGY trusted-local requires the private attempt worktree")
		}
		policy = AGYPolicy{Root: work, Agent: agent, Profile: model.TrustedLocalProfile, Paths: t.AllowedPaths,
			WritePaths: req.WritePaths, Tools: []string{"view_file", "read_file", "write_to_file", "write_file", "edit_file", "replace_file",
				"replace_file_content", "multi_replace_file_content"}}
	}
	if t.NativeChild {
		if !AGYIdentityMatches(req.Config.Model, req.Config.ExpectedBinarySHA256) || t.Mode != "read_only" || t.FollowUp != "" || len(req.Handoffs) != 0 {
			return out, model.Error("POLICY_DENIED", "AGY native child requires the pinned synthetic parent route")
		}
		agent, definition = agyNativeParent, agyNativeParentDefinition
		policy = AGYPolicy{Root: work, Agent: agent, Profile: "native_child", NativeChildRole: agyNativeChild,
			ChildAndMessageScopeVerified: true, Tools: []string{"invoke_subagent", "send_message"}}
	}
	agentRoot := filepath.Join(work, ".agents")
	if err := os.Mkdir(agentRoot, 0700); err != nil {
		return out, err
	}
	agents := filepath.Join(agentRoot, "agents")
	if err := os.Mkdir(agents, 0700); err != nil {
		return out, err
	}
	agentFile := filepath.Join(agents, agent+".md")
	if err := os.WriteFile(agentFile, []byte(definition), 0600); err != nil {
		return out, err
	}
	if t.NativeChild {
		if err := os.WriteFile(filepath.Join(agents, agyNativeChild+".md"), []byte(agyNativeChildDefinition), 0600); err != nil {
			return out, err
		}
	}
	if trusted {
		defer func() {
			if errors.Is(result, execution.ErrCleanup) {
				return
			}
			for _, path := range []string{agentFile, agents, agentRoot} {
				if err := os.Remove(path); err != nil && result == nil {
					result = model.Error("POLICY_DENIED", "AGY changed the attempt agent configuration")
				}
			}
		}()
	}
	if err := os.Symlink("/usr/bin/false", filepath.Join(runtime, "open")); err != nil {
		return out, err
	}
	prompt, err := json.Marshal(struct {
		Objective  string            `json:"objective"`
		Acceptance []model.Check     `json:"acceptance"`
		Files      map[string]string `json:"synthetic_files"`
		Handoffs   []model.Handoff   `json:"verified_handoffs,omitempty"`
	}{t.Objective, t.Acceptance, req.Files, req.Handoffs})
	if err != nil || len(prompt) > execution.MaxArtifact || model.ContainsSecret(prompt) {
		return out, model.Error("POLICY_DENIED", "AGY input is oversized or sensitive")
	}
	if _, err := verifyAGY(ctx, req.Config); err != nil {
		return out, err
	}
	defer func() {
		checkCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 4*time.Second)
		defer cancel()
		if _, err := verifyAGY(checkCtx, req.Config); err != nil && !errors.Is(result, execution.ErrCleanup) {
			result = err
		}
	}()
	args, err := agyArgs(req.Config.Model, work)
	if trusted && err == nil {
		err = agyTrustedPermissions(args, work, t.AllowedPaths, req.WritePaths)
	}
	if err != nil {
		return out, err
	}
	args = append(args, "--agent", agent, "--input-format", "stream-json", "--output-format", "stream-json",
		"--json-schema", `{"type":"object","properties":{"summary":{"type":"string"},"patch":{"type":"string"}},"required":["summary"],"additionalProperties":false}`)
	cmd := exec.Command(req.Config.Path, args...)
	cmd.Dir, cmd.Env = work, vendorUsageEnv(nil, runtime)
	instruction := "Produce the requested structured result using only the synthetic input. Return a unified diff in patch when asked."
	if len(req.Handoffs) == 1 && req.Handoffs[0].Review != nil {
		instruction = model.ReviewInstruction + " Put the JSON object as a JSON-encoded string in the summary field."
	}
	if trusted {
		paths, _ := json.Marshal(req.WritePaths)
		instruction = "First call read_file on the Go source, then call edit_file or write_file on the approved source path. " +
			"Use only read_file, edit_file, write_file, and finish. Skip task management, permission listing, and all other tools. " +
			"Ahenk will execute the registered Go tests after you finish. Keep tests and module files unchanged. " +
			"Return a concise summary and omit the patch field. Writable paths: " + string(paths)
	}
	if t.NativeChild {
		instruction = "Invoke ahenk-native-child exactly once with inherited workspace and model. " +
			"Pass the synthetic objective to the child. Wait for the actual child result and return its answer as the summary."
	}
	var followUp []string
	if t.FollowUp != "" {
		followUp = []string{t.FollowUp}
	}
	resultData, err := runAGYStream(ctx, cmd, req.Config.Model, policy, instruction+"\n"+string(prompt), out.AGY, followUp...)
	if err != nil {
		return out, err
	}
	payload := resultData.Structured
	if len(payload) == 0 || bytes.Equal(payload, []byte("null")) {
		payload = []byte(resultData.Text)
	}
	var reply struct {
		Summary string `json:"summary"`
		Patch   string `json:"patch"`
	}
	dec := json.NewDecoder(bytes.NewReader(payload))
	dec.DisallowUnknownFields()
	if dec.Decode(&reply) != nil || dec.Decode(new(any)) != io.EOF || reply.Summary == "" ||
		(t.Mode == "patch" && !trusted) != (reply.Patch != "") || model.ContainsSecret(payload) {
		return out, agyProviderFault("INVALID_OUTPUT", "AGY_ARTIFACT_INVALID", "AGY did not produce the expected scoped artifact")
	}
	if trusted {
		tools := out.AGY.ObservedTools
		if tools["view_file"]+tools["read_file"] == 0 ||
			tools["write_to_file"]+tools["write_file"]+tools["edit_file"]+tools["replace_file"]+
				tools["replace_file_content"]+tools["multi_replace_file_content"] == 0 {
			return out, model.Error("INVALID_OUTPUT", "AGY did not report both a completed read and a completed native edit")
		}
	}
	if t.NativeChild && (out.AGY.NativeChild == nil || !out.AGY.NativeChild.ChildFinished ||
		out.AGY.NativeChild.ResultSHA256 != model.Hash([]byte(strings.TrimSpace(reply.Summary)))) {
		return out, agyProviderFault("INVALID_OUTPUT", "AGY_CHILD_RESULT_UNVERIFIED", "AGY parent summary did not match an owned child terminal result")
	}
	out.Summary, out.Patch = reply.Summary, reply.Patch
	return out, nil
}

func agyTrustedPermissions(args []string, work string, reads, writes []string) error {
	settings := map[string]any{}
	if err := json.Unmarshal([]byte(args[3]), &settings); err != nil {
		return err
	}
	var allow []string
	deny := []string{"command(*)", "read_url(*)", "execute_url(*)", "mcp(*)", "write_file(.git)", "write_file(.agents/)"}
	for _, name := range reads {
		target := filepath.Join(work, name)
		allow = append(allow, "read_file("+target+")")
		if !slices.ContainsFunc(writes, func(write string) bool { return model.InScope(write, []string{name}) }) {
			deny = append(deny, "write_file("+target+")")
		}
	}
	for _, name := range writes {
		allow = append(allow, "write_file("+filepath.Join(work, name)+")")
	}
	settings["permissions"] = map[string][]string{
		"allow": allow,
		"deny":  deny,
	}
	data, err := json.Marshal(settings)
	args[3] = string(data)
	return err
}

// The caller has already verified every subscription window. Keep the
// adaptive floor separate from the provider's own exhaustion and cost gates.
func agyWeeklyReserve(rows []Quota, family string, floor float64) *model.AGYReserveEvidence {
	if floor <= 0 {
		return nil
	}
	for _, q := range rows {
		if q.Family == family && q.WindowMinutes == 10080 && q.Remaining != nil && *q.Remaining <= floor {
			return &model.AGYReserveEvidence{Bucket: q.Bucket, Remaining: *q.Remaining, ResetAt: q.ResetAt, ObservedAt: q.ObservedAt}
		}
	}
	return nil
}

const auditQuotaReserve = 15.0

func AGYFamilyBuckets(family string) []string {
	switch family {
	case "gemini":
		return []string{"gemini-5h", "gemini-weekly"}
	case "claude_gpt":
		return []string{"3p-5h", "3p-weekly"}
	case "gemini_argon":
		return []string{"gemini-argon-wtu-weekly"}
	default:
		return nil
	}
}

func checkAGYSubscriptionQuota(rows []Quota, now time.Time) *model.Fault {
	return checkAGYFamilyQuota(rows, "gemini", 0, now)
}

func checkAGYFamilyQuota(rows []Quota, family string, reserve float64, now time.Time) *model.Fault {
	buckets := AGYFamilyBuckets(family)
	if len(buckets) == 0 {
		return model.Error("ZERO_COST_UNVERIFIED", "AGY model has no verified subscription bucket")
	}
	for _, id := range buckets {
		i := slices.IndexFunc(rows, func(q Quota) bool { return q.Bucket == id })
		if i < 0 {
			return model.Error("ZERO_COST_UNVERIFIED", "AGY subscription windows are missing")
		}
		q := rows[i].At(now)
		if q.Provider != "agy" || q.Source != "agy_usage" || q.Family != family ||
			q.AccountMatch != "SAME_NATIVE_CLI_CONTEXT" || q.ObservedAt.IsZero() || q.ObservedAt.After(now) ||
			q.Remaining == nil || q.ResetAt == nil || *q.Remaining > 100 || (q.State != "" && q.State != "KNOWN" && q.State != "EXHAUSTED") {
			return model.Error("ZERO_COST_UNVERIFIED", "AGY subscription window is unverified")
		}
		if *q.Remaining <= 0 {
			return model.Error("QUOTA_EXHAUSTED", "AGY subscription quota is exhausted")
		}
		if *q.Remaining < reserve {
			return model.Error("QUOTA_RESERVE", "AGY subscription quota is below the audit reserve")
		}
	}
	return nil
}

func runAGYStream(ctx context.Context, cmd *exec.Cmd, modelID string, policy AGYPolicy, prompt string, evidence *model.AGYRunEvidence, followUp ...string) (result AGYResult, resultErr error) {
	if len(followUp) > 1 || len(followUp) == 1 && policy.Profile != "restricted" {
		return result, model.Error("POLICY_DENIED", "invalid AGY continuation policy")
	}
	prompts := append([]string{prompt}, followUp...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.WaitDelay = 2 * time.Second
	stderr := &execution.Limited{Limit: 32 << 10}
	cmd.Stderr = stderr
	reader, writer, err := os.Pipe()
	if err != nil {
		return result, err
	}
	defer reader.Close()
	defer writer.Close()
	cmd.Stdout = writer
	input, err := cmd.StdinPipe()
	if err != nil {
		return result, err
	}
	defer input.Close()
	if err := ctx.Err(); err != nil {
		return result, err
	}
	if err := cmd.Start(); err != nil {
		return result, err
	}
	_ = writer.Close()
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	waited := false
	defer func() {
		if execution.StopProcessGroup(cmd.Process.Pid) != nil {
			resultErr = execution.ErrCleanup
		}
		if !waited && resultErr != execution.ErrCleanup {
			<-done
		}
	}()
	scan := bufio.NewScanner(reader)
	scan.Buffer(make([]byte, 4096), execution.MaxArtifact)
	lines := make(chan []byte)
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		defer close(lines)
		for scan.Scan() {
			select {
			case lines <- bytes.Clone(scan.Bytes()):
			case <-stop:
				return
			}
		}
	}()
	var stream agyStreamState
	var transcriptSize, sentTurns, completedTurns int
	send := func() error {
		if evidence.Cost == nil || evidence.Cost.Status != "DOCUMENTED_COST_PROTECTION" || evidence.Cost.Documented != "VERIFIED" {
			return model.Error("ZERO_COST_UNVERIFIED", "AGY subscription-only preflight did not pass")
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		message := map[string]any{"event": "user", "message": map[string]string{"content": prompts[sentTurns]}}
		sent := make(chan error, 1)
		evidence.PromptSent = true
		go func() { sent <- json.NewEncoder(input).Encode(message) }()
		select {
		case err := <-sent:
			if err != nil {
				return agyProviderFault("INVALID_OUTPUT", "AGY_PROMPT_PIPE_FAILED", "AGY prompt pipe closed unexpectedly")
			}
			sentTurns++
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	for {
		select {
		case <-ctx.Done():
			return result, ctx.Err()
		case line, ok := <-lines:
			if !ok {
				if scan.Err() != nil {
					return result, agyProviderFault("INVALID_OUTPUT", "AGY_EVENT_LIMIT", "AGY event exceeded limits")
				}
				select {
				case exit := <-done:
					waited = true
					if code := agyFailure(string(stderr.Bytes())); code != "INVALID_OUTPUT" {
						return result, agyProviderFault(code, "AGY_STDERR_REJECTED", "AGY reported a rejected operation")
					}
					if exit != nil {
						return result, agyProviderFault("INVALID_OUTPUT", "AGY_PROCESS_EXIT", "AGY process did not exit cleanly")
					}
					if !stream.finished || completedTurns != len(prompts) {
						return result, agyProviderFault("INVALID_OUTPUT", "AGY_TERMINAL_MISSING", "AGY terminal event missing")
					}
					evidence.TerminalVerified = true
					return stream.result, nil
				case <-ctx.Done():
					return result, ctx.Err()
				}
			}
			transcriptSize += len(line) + 1
			if transcriptSize > execution.MaxArtifact*2 {
				return result, agyProviderFault("INVALID_OUTPUT", "AGY_TRANSCRIPT_LIMIT", "AGY transcript exceeded limits")
			}
			if err := stream.accept(line, modelID, policy, evidence); err != nil {
				return result, err
			}
			if sentTurns == 0 {
				if err := send(); err != nil {
					return result, err
				}
			}
			if stream.finished {
				usage := stream.result.Usage
				if usage.Total <= 0 || usage.Input < 0 || usage.Output < 0 || usage.Input > usage.Total-usage.Output {
					return result, agyProviderFault("INVALID_OUTPUT", "AGY_TERMINAL_USAGE_INVALID", "AGY terminal usage was missing or invalid")
				}
				evidence.InputTokens += usage.Input
				evidence.OutputTokens += usage.Output
				evidence.TotalTokens += usage.Total
				completedTurns++
				if len(prompts) == 2 {
					payload := stream.result.Structured
					if len(payload) == 0 || bytes.Equal(payload, []byte("null")) {
						payload = []byte(stream.result.Text)
					}
					var reply struct{ Summary, Patch string }
					if json.Unmarshal(payload, &reply) != nil || reply.Summary == "" || reply.Patch != "" || model.ContainsSecret(payload) {
						return result, model.Error("INVALID_OUTPUT", "AGY continuation turn returned invalid structured output")
					}
					if evidence.Session == nil {
						evidence.Session = &model.SessionEvidence{IDHash: model.Hash([]byte(stream.result.Session))}
					}
					evidence.Session.Turns = append(evidence.Session.Turns, model.SessionTurn{
						ReplySHA256: model.Hash([]byte(reply.Summary)), InputTokens: usage.Input,
						OutputTokens: usage.Output, TotalTokens: usage.Total,
					})
				}
				if sentTurns < len(prompts) {
					stream.finished = false
					stream.result = AGYResult{Session: stream.result.Session, Model: stream.result.Model}
					if err := send(); err != nil {
						return result, err
					}
				} else {
					_ = input.Close()
				}
			}
		}
	}
}
