// Copyright 2026 Furkan Anıl Öztaş
// SPDX-License-Identifier: Apache-2.0

package mcpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"reflect"
	"slices"
	"strconv"
	"time"
	"unicode/utf8"

	"github.com/faoztas/ahenk-mcp/internal/execution"
	"github.com/faoztas/ahenk-mcp/internal/model"
	"github.com/faoztas/ahenk-mcp/internal/orchestration"
	"github.com/faoztas/ahenk-mcp/internal/providers"
	"github.com/faoztas/ahenk-mcp/internal/readiness"
	"github.com/faoztas/ahenk-mcp/internal/store"
	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type jobID struct {
	JobID string `json:"job_id"`
}
type resumeInput struct {
	JobID     string `json:"job_id"`
	RequestID string `json:"request_id"`
}
type listInput struct {
	RepoID string `json:"repo_id,omitempty"`
	Status string `json:"status,omitempty"`
	Cursor string `json:"cursor,omitempty"`
	Limit  int    `json:"limit,omitempty"`
}
type resultInput struct {
	JobID      string `json:"job_id"`
	View       string `json:"view,omitempty"`
	ArtifactID string `json:"artifact_id,omitempty"`
	Cursor     string `json:"cursor,omitempty"`
}
type capabilitiesInput struct {
	RepoID       string `json:"repo_id,omitempty"`
	Provider     string `json:"provider,omitempty"`
	IncludeQuota bool   `json:"include_quota,omitempty"`
	Cursor       string `json:"cursor,omitempty"`
}
type emptyInput struct{}
type response map[string]any

func New(e *orchestration.Engine, cfg model.Config, r providers.Registry) *mcp.Server {
	s := mcp.NewServer(&mcp.Implementation{Name: "ahenk", Version: model.Version}, nil)
	knownModel := func(provider, name string) bool {
		return slices.ContainsFunc(r.Candidates, func(c model.Capability) bool {
			return c.Status == "available" && c.Model == name &&
				(provider == "" || orchestration.NormalizeProvider(c.Provider) == orchestration.NormalizeProvider(provider))
		})
	}
	jobResponse := func(j model.Job) response {
		out := status(j)
		if j.ID == "" {
			return out
		}
		if warnings := e.JobCapacityWarnings(j.ID); len(warnings) != 0 {
			out["capacity_warnings"] = warnings
			if slices.ContainsFunc(warnings, func(w model.CapacityWarning) bool {
				return w.Provider == "codex" && (w.Severity == "low" || w.Severity == "critical")
			}) {
				out["next_poll_after_ms"] = 15000
			}
		}
		return out
	}
	jobDetailedResponse := func(ctx context.Context, j model.Job) response {
		out := status(j)
		if warnings := e.JobCapacityWarnings(j.ID); len(warnings) != 0 {
			out["capacity_warnings"] = warnings
		}
		if j.Result != nil {
			out["summary"] = j.Result.Summary
			out["verification"] = j.Result.Verification
			if j.Result.Cost != nil {
				out["cost"] = publicView{j.Result.Cost, publicCost}
			}
		}
		if len(j.Attempts) > 0 {
			last := j.Attempts[len(j.Attempts)-1]
			out["executed_provider"] = last.Provider
			out["executed_model"] = last.Model
		}
		if j.Diff != "" {
			out["diff"] = j.Diff
		} else if j.Task.Mode == "patch" && j.Result != nil && j.Result.ArtifactID != "" {
			patch, drift, err := e.Artifact(ctx, j, j.Result.ArtifactID)
			if err == nil {
				out["diff"] = string(patch)
				out["source_drift"] = drift
			}
		}
		if len(j.Artifacts) > 0 {
			out["artifacts"] = j.Artifacts
		}
		if j.Result != nil && len(j.Result.Checks) > 0 {
			out["checks"] = j.Result.Checks
			allPassed := true
			for _, chk := range j.Result.Checks {
				if chk.Status != "PASSED" || chk.ExitStatus != 0 {
					allPassed = false
					break
				}
			}
			if allPassed {
				out["check_summary"] = "PASSED"
			} else {
				out["check_summary"] = "FAILED"
			}
		}
		return out
	}

	handleDelegateTask := func(ctx context.Context, in model.DelegateTaskInput) (response, error) {
		effectiveMaxDepth := providers.EffectiveMaxDepth(os.Environ(), in.MaxDepth)
		if err := providers.CheckRecursionGuard(os.Environ(), effectiveMaxDepth); err != nil {
			return nil, err
		}
		if in.RepoID == "" && e != nil {
			repoID, rErr := e.ResolveRepo(in.Cwd)
			if rErr == nil {
				in.RepoID = repoID
			}
		}
		normCfg := &cfg
		if e != nil && in.RepoID != "" {
			if repo, ok := e.Repo(in.RepoID); ok {
				normCfg = &model.Config{Repositories: map[string]model.Repo{in.RepoID: repo}}
			}
		}
		sub, execMode, waitTimeout, err := in.Normalize(in.Cwd, normCfg)
		if err != nil {
			return nil, err
		}
		sub.Task.MaxDepth = effectiveMaxDepth

		var initial *model.InitialRoute
		var j model.Job
		isAuto := (in.Target == nil || in.Target.Provider == "" || in.Target.Provider == "auto") &&
			in.PreferredProvider == "" && in.PreferredModel == "" && sub.Task.PreferredProvider == "" && sub.Task.PreferredModel == ""
		if isAuto && e != nil {
			var complexityPtr *float64
			if in.HasComplexity || in.Complexity != 0 {
				c := in.Complexity
				complexityPtr = &c
			}
			j, initial, err = e.SubmitAutoRouted(sub, in.Category, complexityPtr)
		} else {
			j, err = e.SubmitRouted(sub, nil)
		}
		route := j.InitialRoute
		if err != nil {
			route = initial
		}
		var routeDecision map[string]any
		if route != nil {
			routeDecision = map[string]any{
				"selected_model": route.SelectedModel, "resolved_effort": route.ResolvedEffort,
				"reason": route.Reason, "task_category": route.TaskCategory, "quota_pressure": route.QuotaPressure,
			}
			if route.CapacityState != "" {
				routeDecision["capacity_state"] = route.CapacityState
				routeDecision["cost_class"] = route.CostClass
				if route.CapacityState == "UNKNOWN" || route.CapacityState == "STALE" {
					routeDecision["quota_pressure"] = nil
				}
			}
		}
		if err != nil {
			out := jobResponse(j)
			if routeDecision != nil {
				out["route_decision"] = routeDecision
			}
			return out, err
		}
		if execMode == "wait" {
			waitedJob, waitErr := e.Wait(ctx, j.ID, waitTimeout)
			out := jobDetailedResponse(ctx, waitedJob)
			if routeDecision != nil {
				out["route_decision"] = routeDecision
			}
			return out, waitErr
		}
		out := jobResponse(j)
		if routeDecision != nil {
			out["route_decision"] = routeDecision
		}
		return out, nil
	}

	add(s, e, "delegate_task", "Delegate an objective to a bounded subagent in an isolated worktree. A string task defaults to wait. A structured task or objective defaults to async. Set execution_mode to choose explicitly.", false, handleDelegateTask, delegateDualFormSchema)
	add(s, e, "spawn_subagent", "Spawn an autonomous subagent for isolated work. Alias for delegate_task.", false, handleDelegateTask, delegateDualFormSchema)
	add(s, e, "wait_jobs", "Wait for one or more subagent jobs to finish. Use after launching multiple parallel subagents with execution_mode:'async'.", false,
		func(ctx context.Context, in model.WaitJobsInput) (response, error) {
			norm, err := in.Normalize()
			if err != nil {
				return nil, err
			}
			timeout := time.Duration(norm.TimeoutSeconds) * time.Second
			jobs, err := e.WaitJobs(ctx, norm.JobIDs, norm.Mode, timeout)
			if err != nil {
				return nil, err
			}
			results := make([]response, 0, len(jobs))
			for _, j := range jobs {
				results = append(results, jobDetailedResponse(ctx, j))
			}
			return response{"jobs": results, "count": len(results)}, nil
		})
	add(s, e, "delegate_batch", "Queue up to eight tasks with dependencies and optional verified result handoffs.", false,
		func(ctx context.Context, input model.BatchInput) (response, error) {
			in := model.Batch(input)
			if repo, ok := e.Repo(in.RepoID); ok && repo.PrivateSource != nil {
				if !repo.PrivateSource.WriterIsolationVerified || !repo.PrivateSource.AllowWrite {
					return nil, model.GateError("POLICY_DENIED", "WRITER_ISOLATION_UNVERIFIED", "private source batch requires verified writer isolation and explicit write admission", "verify_writer_isolation_and_enable_allow_write")
				}
			}
			routes, admissionErr := readiness.M1Routes(ctx, cfg, r, in, time.Now().UTC())
			if routes != nil || admissionErr != nil {
				j, err := e.SubmitM1Batch(in, routes, admissionErr)
				return jobResponse(j), err
			}
			j, err := e.SubmitBatch(in)
			return jobResponse(j), err
		})
	add(s, e, "get_job_status", "Read a job status without waiting for completion.", false,
		func(_ context.Context, in jobID) (response, error) {
			if !model.ValidID(in.JobID) {
				return nil, model.Error("INVALID_INPUT", "invalid job_id")
			}
			j, err := e.Get(in.JobID)
			if err != nil {
				return nil, err
			}
			out := jobResponse(j)
			addRoutingEvidence(out, store.ProjectRoutingEvidence(j, knownModel))
			return out, nil
		})
	add(s, e, "list_jobs", "List durable jobs visible under current repository policies.", false,
		func(_ context.Context, in listInput) (response, error) {
			if in.Limit == 0 {
				in.Limit = 20
			}
			if in.Limit < 1 || in.Limit > 100 || (in.Cursor != "" && !model.ValidID(in.Cursor)) {
				return nil, model.Error("INVALID_INPUT", "invalid pagination")
			}
			if in.RepoID != "" && !model.ValidID(in.RepoID) {
				return nil, model.Error("INVALID_INPUT", "invalid repo_id")
			}
			if in.Status != "" && !model.ValidID(in.Status) {
				return nil, model.Error("INVALID_INPUT", "invalid status")
			}
			list, next := e.List(in.RepoID, in.Status, in.Cursor, in.Limit)
			items := make([]response, 0, len(list))
			for _, j := range list {
				items = append(items, status(j))
			}
			return response{"jobs": items, "next_cursor": next}, nil
		})
	add(s, e, "cancel_job", "Cancel a durable job and terminate its worker.", false,
		func(_ context.Context, in jobID) (response, error) {
			if !model.ValidID(in.JobID) {
				return nil, model.Error("INVALID_INPUT", "invalid job_id")
			}
			j, err := e.Cancel(in.JobID)
			return jobResponse(j), err
		})
	add(s, e, "resume_job", "Resume a paused or interrupted job, or retry one malformed review, with the original deadline and snapshot.", false,
		func(_ context.Context, in resumeInput) (response, error) {
			if !model.ValidID(in.JobID) || !model.ValidID(in.RequestID) {
				return nil, model.Error("INVALID_INPUT", "invalid job_id or request_id")
			}
			j, err := e.Resume(in.JobID, in.RequestID)
			return jobResponse(j), err
		})
	add(s, e, "get_job_result", "Read a summary, route evidence, or a bounded page of a verified diff.", false,
		func(ctx context.Context, in resultInput) (response, error) {
			if !model.ValidID(in.JobID) {
				return nil, model.Error("INVALID_INPUT", "invalid job_id")
			}
			j, err := e.Get(in.JobID)
			if err != nil {
				return nil, err
			}
			out := jobResponse(j)
			switch in.View {
			case "", "summary":
				out["result"] = publicView{j.Result, publicResult}
			case "route":
				out["route"] = publicView{j.Route, publicDecision}
				out["attempts"] = publicView{j.Attempts, publicAttempt}
				if j.PlanSHA256 != "" {
					out["plan_sha256"] = j.PlanSHA256
				}
				if len(j.CapacityWarningCodes) != 0 {
					out["routing_warning_codes"] = j.CapacityWarningCodes
				}
				if j.QualityRole != "" {
					out["required_quality_role"] = j.QualityRole
				}
				if j.RoutingMode != "" {
					out["routing_mode"] = j.RoutingMode
				}
			case "diff":
				if j.Result == nil || j.Result.ArtifactID == "" {
					return nil, model.Error("NOT_FOUND", "job has no verified diff artifact")
				}
				artifactID := in.ArtifactID
				if artifactID == "" {
					artifactID = j.Result.ArtifactID
				}
				if !model.ValidID(artifactID) {
					return nil, model.Error("INVALID_INPUT", "invalid artifact_id")
				}
				p, drift, err := e.Artifact(ctx, j, artifactID)
				if err != nil {
					return nil, err
				}
				start := 0
				if in.Cursor != "" {
					start, err = strconv.Atoi(in.Cursor)
					if err != nil || start < 0 || start >= len(p) || !utf8.RuneStart(p[start]) {
						return nil, model.Error("INVALID_INPUT", "invalid diff cursor")
					}
				}
				end := min(start+(64<<10), len(p))
				for end < len(p) && !utf8.RuneStart(p[end]) {
					end--
				}
				out["diff"] = string(p[start:end])
				out["source_drift"] = drift
				out["preimages"] = j.Result.Preimages
				if len(j.Result.Postimages) > 0 {
					out["postimages"] = j.Result.Postimages
				}
				out["artifact_sha256"] = j.Result.ArtifactSHA256
				if end < len(p) {
					out["next_cursor"] = strconv.Itoa(end)
				}
			default:
				return nil, model.Error("INVALID_INPUT", "invalid result view")
			}
			return out, nil
		})
	add(s, e, "list_capabilities", "Show scheduler and provider availability, including closed execution gates.", true,
		func(_ context.Context, in capabilitiesInput) (response, error) {
			if e == nil {
				return response{"scheduler_status": "state_busy", "next_action": "reconnect_after_owner_exits",
					"version": model.Version, "binary_sha256": execution.SelfBinarySHA256()}, nil
			}
			if in.RepoID != "" {
				if !e.HasRepo(in.RepoID) {
					return nil, model.GateError("REPO_NOT_REGISTERED", "REPO_NOT_REGISTERED", "repository is not registered", "register_repository_in_config")
				}
			}
			capacity, warnings, preview := e.Capacity(in.RepoID)
			repoProviders := e.RepoProviders(in.RepoID)
			items := make([]model.Capability, 0)
			for _, c := range r.Candidates {
				if in.Provider != "" && orchestration.NormalizeProvider(c.Provider) != orchestration.NormalizeProvider(in.Provider) {
					continue
				}
				if in.RepoID != "" && c.Status == "available" {
					cNorm := orchestration.NormalizeProvider(c.Provider)
					match := false
					for _, rp := range repoProviders {
						if orchestration.NormalizeProvider(rp) == cNorm {
							match = true
							break
						}
					}
					if !match {
						c.Status = "disabled"
						c.Reason = "REPO_PROVIDER_POLICY"
					}
				}
				if c.Status == "available" {
					c.Quality = e.RoleQualities(c)
					c.FPTier = providers.FPTier(c.Provider, c.Model)
					for _, d := range preview {
						if !sameRoute(c, d) {
							continue
						}
						if d.HeadroomState != "" && orchestration.NormalizeProvider(c.Provider) == "agy" {
							c.HeadroomPercent, c.HeadroomState = d.HeadroomPercent, d.HeadroomState
						} else if d.QuotaRemaining != nil {
							c.HeadroomPercent, c.HeadroomState = *d.QuotaRemaining, d.CapacityState
						} else if d.HealthScope != "" {
							c.HeadroomState = d.CapacityState
						}
						break
					}
				}
				items = append(items, c)
			}
			out := response{
				"scheduler_status": "active",
				"version":          model.Version,
				"binary_sha256":    execution.SelfBinarySHA256(),
				"providers":        publicView{items, publicCapability},
				"local_runners":    e.LocalRunners(),
				"routing_summary":  routingSummary(items, e.ActiveCircuitBreakers(), preview),
			}
			if in.RepoID != "" {
				out["repo_admission"] = e.RepoAdmission(in.RepoID)
			}
			if len(warnings) != 0 {
				out["capacity_warnings"] = warnings
			}
			if in.IncludeQuota {
				out["quota"] = publicView{e.Quota(in.Provider), publicQuota}
				out["capacity"] = publicView{capacity, publicCapacity}
				out["capacity_policy"] = cfg.CapacityPolicy.Defaults()
				if in.RepoID != "" {
					out["routing_preview"] = publicView{preview, publicDecision}
				}
			}
			return out, nil
		})

	add(s, e, "get_provider_quotas", "Read remaining percentages, states, and circuit states for all provider quota bars.", true,
		func(_ context.Context, _ emptyInput) (response, error) {
			return response{"bars": e.QuotaBars(time.Now().UTC())}, nil
		})

	add(s, e, "get_active_circuit_breakers", "Read active provider circuit breakers and cooldown deadlines.", true,
		func(_ context.Context, _ emptyInput) (response, error) {
			if e == nil {
				return response{"tripped_breakers": []map[string]any{}}, nil
			}
			return response{"tripped_breakers": e.ActiveCircuitBreakers()}, nil
		})

	add(s, e, "get_routing_audit", "Retrieve dynamic routing decision history and fallbacks for a given job ID.", true,
		func(_ context.Context, in jobID) (response, error) {
			if !model.ValidID(in.JobID) {
				return nil, model.Error("INVALID_INPUT", "invalid job_id")
			}
			if e == nil {
				return nil, model.GateError("STATE_BUSY", "STATE_BUSY", "another stdio session owns this state; reconnect after the owner exits", "reconnect_after_owner_exits")
			}
			j, history, err := e.GetWithRoutingAudit(in.JobID)
			if err != nil {
				return nil, err
			}
			if history == nil {
				history = []store.JobRoutingAudit{}
			}
			for i := range history {
				history[i] = store.ProjectLegacyRoutingAudit(history[i], knownModel)
			}
			evidence := store.ProjectRoutingEvidence(j, knownModel)
			out := response{"job_id": j.ID, "attempts": history, "status": j.Status}
			addRoutingEvidence(out, evidence)
			return out, nil
		})

	return s
}

func sameRoute(c model.Capability, d model.Decision) bool {
	return d.Provider == c.Provider && d.Model == c.Model && d.Fingerprint == c.Fingerprint &&
		d.OpenCodeAccount == c.OpenCodeAccount && d.OpenCodeTransport == c.OpenCodeTransport
}

// routingSummary recommends one available route per price/performance tier
// and lists the active circuit breakers.
func routingSummary(items []model.Capability, breakers []map[string]any, routes []model.Decision) response {
	if breakers == nil {
		breakers = []map[string]any{}
	}
	isCandidateHealthy := func(c model.Capability) bool {
		return slices.ContainsFunc(routes, func(d model.Decision) bool {
			return (d.Reason == "AVAILABLE_CANDIDATE" || d.Reason == "FALLBACK_CANDIDATE") &&
				sameRoute(c, d)
		})
	}
	summary := response{"active_circuit_breakers": breakers}

	for tier, key := range map[string]string{
		"free": "recommended_free", "cheap": "recommended_cheap",
		"balanced": "recommended_balanced", "hard": "recommended_frontier",
	} {
		best, bestHeadroom := "", -1.0
		for _, c := range items {
			if c.Status != "available" || c.FPTier != tier || !isCandidateHealthy(c) {
				continue
			}

			if c.HeadroomPercent > bestHeadroom {
				best, bestHeadroom = c.Provider+":"+c.Model, c.HeadroomPercent
			}
		}
		if best != "" {
			summary[key] = best
		}
	}
	return summary
}

func addRoutingEvidence(out response, evidence store.RoutingEvidence) {
	out["reason_code"] = evidence.ReasonCode
	if evidence.RoutingProfile != "" {
		out["routing_profile"] = evidence.RoutingProfile
	}
	out["routing_profile_source"] = evidence.RoutingProfileSource
	if evidence.FirstDecision != nil {
		out["first_decision"] = evidence.FirstDecision
	}
	out["execution_attempts"] = evidence.ExecutionAttempts
	if evidence.ExecutionAttemptsTruncated {
		out["execution_attempts_truncated"] = true
	}
}

func status(j model.Job) response {
	out := response{"job_id": j.ID, "repo_id": j.RepoID, "status": j.Status, "reason_code": j.Reason,
		"deadline_at": j.DeadlineAt, "attempt_count": len(j.Attempts), "snapshot_id": j.SnapshotID,
		"base_sha": j.BaseSHA, "scope_dirty": j.Dirty, "next_poll_after_ms": 5000}
	if j.Task.Priority == "background" {
		out["next_poll_after_ms"] = 15000
	}
	if j.Parent != "" {
		out["parent_id"] = j.Parent
		out["key"] = j.Key
	}
	if len(j.Children) > 0 {
		out["children"] = j.Children
	}
	return out
}

func add[I any](s *mcp.Server, e *orchestration.Engine, name, description string, diagnostic bool, fn func(context.Context, I) (response, error), adjust ...func(map[string]any)) {
	wire := schemaWire[I]()
	for _, adjustSchema := range adjust {
		adjustSchema(wire)
	}
	s.AddTool(&mcp.Tool{Name: name, Description: description, InputSchema: wire}, func(ctx context.Context, r *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		if e == nil && !diagnostic {
			return toolError(model.GateError("STATE_BUSY", "STATE_BUSY", "another stdio session owns this state; reconnect after the owner exits", "reconnect_after_owner_exits")), nil
		}
		if len(r.Params.Arguments) > 256<<10 {
			return toolError(model.Error("INVALID_INPUT", "invalid tool arguments")), nil
		}
		args := bytes.TrimSpace(r.Params.Arguments)
		if len(args) == 0 {
			args = []byte("{}")
		}
		d := json.NewDecoder(bytes.NewReader(args))
		var in I
		if len(args) > 256<<10 || args[0] != '{' || d.Decode(&in) != nil || d.Decode(new(any)) != io.EOF {
			return toolError(model.Error("INVALID_INPUT", "invalid tool arguments")), nil
		}
		out, err := fn(ctx, in)
		if err != nil {
			return toolError(err, out), nil
		}
		out["schema_version"] = 1
		summary := "Ahenk " + name
		if st, ok := out["status"].(string); ok {
			if _, job := out["job_id"]; job {
				switch st {
				case model.Created, model.Queued, model.Running, model.Verifying, model.RetryWait, model.Cancelling, model.Paused, model.Interrupted,
					model.Completed, model.Failed, model.Cancelled, model.TimedOut, model.Blocked, model.NotRun:
				default:
					st = "UNKNOWN"
					out["status"] = st
				}
			}
			summary += " " + st
		}
		if warnings, ok := out["capacity_warnings"].([]model.CapacityWarning); ok {
			for _, severity := range []string{"critical", "warning", "low", "info"} {
				i := slices.IndexFunc(warnings, func(w model.CapacityWarning) bool { return w.Severity == severity })
				if i >= 0 {
					summary += "\n" + warnings[i].Message
					break
				}
			}
		}
		return &mcp.CallToolResult{StructuredContent: out, Content: []mcp.Content{&mcp.TextContent{Text: summary}}}, nil
	})
}

// delegateDualFormSchema advertises the string-or-object forms that
// DelegateTaskInput's custom decoder accepts. Reflection-based inference only
// sees the Go field types (string and *TargetSpec), so without this adjustment
// discovery would reject the documented nested-task and string-target forms.
func delegateDualFormSchema(wire map[string]any) {
	props, ok := wire["properties"].(map[string]any)
	if !ok {
		return
	}
	if task, ok := props["task"].(map[string]any); ok {
		objectForm := schemaWire[model.Task]()
		// The decoder accepts partial task objects and Normalize fills defaults.
		delete(objectForm, "required")
		dual := map[string]any{
			"anyOf": []any{
				map[string]any{"type": []any{"string", "null"}},
				objectForm,
			},
		}
		if description, ok := task["description"]; ok {
			dual["description"] = description
		}
		props["task"] = dual
	}
	if target, ok := props["target"].(map[string]any); ok {
		description, hasDescription := target["description"]
		delete(target, "description")
		dual := map[string]any{
			"anyOf": []any{
				map[string]any{"type": []any{"string", "null"}},
				target,
			},
		}
		if hasDescription {
			dual["description"] = description
		}
		props["target"] = dual
	}
}

func schemaWire[T any]() map[string]any {
	var profiles []any
	for _, profile := range model.RoutingProfiles() {
		profiles = append(profiles, string(profile))
	}
	schema, err := jsonschema.For[T](&jsonschema.ForOptions{TypeSchemas: map[reflect.Type]*jsonschema.Schema{
		reflect.TypeFor[model.RoutingProfile](): {Type: "string", Enum: profiles},
	}})
	if err != nil {
		panic(err)
	}
	b, err := json.Marshal(schema)
	if err != nil {
		panic(err)
	}
	var wire map[string]any
	if err := json.Unmarshal(b, &wire); err != nil {
		panic(err)
	}
	return wire
}

// errorMetadataKeys are the non-sensitive handler fields retained alongside a
// structured tool error so callers can correlate failures with a dispatched job
// or route decision. Results, diffs, and repository metadata stay out.
var errorMetadataKeys = []string{"job_id", "route_decision"}

// preserveErrorMetadata copies allowlisted correlation fields into an error
// response without overwriting structured error fields.
func preserveErrorMetadata(wire, metadata response) {
	for _, key := range errorMetadataKeys {
		value, ok := metadata[key]
		if !ok || value == nil {
			continue
		}
		if s, ok := value.(string); ok && s == "" {
			continue
		}
		if _, exists := wire[key]; exists {
			continue
		}
		wire[key] = value
	}
}

func toolError(err error, metadata ...response) *mcp.CallToolResult {
	var f *model.Fault
	if !errors.As(err, &f) {
		f = model.Error("INTERNAL", "Ahenk could not complete the operation")
	}
	wire := response{"schema_version": 1, "code": f.Code, "message": f.Message, "retryable": f.Retryable}
	if f.Gate != "" {
		wire["gate"] = f.Gate
	}
	if f.NextAction != "" {
		wire["next_action"] = f.NextAction
	}
	if len(metadata) > 0 && metadata[0] != nil {
		preserveErrorMetadata(wire, metadata[0])
	}
	return &mcp.CallToolResult{IsError: true, StructuredContent: wire,
		Content: []mcp.Content{&mcp.TextContent{Text: f.Message}}}
}
