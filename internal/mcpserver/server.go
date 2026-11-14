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
	"slices"
	"strconv"
	"strings"
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
				out["cost"] = j.Result.Cost
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
		maxDepth := in.MaxDepth
		if maxDepth <= 0 {
			maxDepth = 1
		}
		if err := providers.CheckRecursionGuard(os.Environ(), maxDepth); err != nil {
			return nil, err
		}
		if in.RepoID == "" {
			repoID, rErr := e.ResolveRepo(in.Cwd)
			if rErr == nil {
				in.RepoID = repoID
			}
		}
		sub, execMode, waitTimeout, err := in.Normalize(in.Cwd, &cfg)
		if err != nil {
			return nil, err
		}

		var routeDecision map[string]any
		isAuto := (in.Target == nil || in.Target.Provider == "" || in.Target.Provider == "auto") &&
			in.PreferredProvider == "" && in.PreferredModel == "" && sub.Task.PreferredProvider == "" && sub.Task.PreferredModel == ""
		if isAuto {
			category := in.Category
			if category == "" {
				category = "coding"
			}
			complexity := in.Complexity
			if complexity <= 0 {
				if category == "research" {
					complexity = 0.2
				} else {
					complexity = 0.9
				}
			}

			matrix := orchestration.FallbackMatrix(category)
			var selectedModel string
			var selectedProvider string
			var selectedHealth float64 = 1.0

			var allowedProviders []string
			if in.RepoID != "" {
				if repoCfg, ok := cfg.Repositories[in.RepoID]; ok {
					allowedProviders = repoCfg.Providers
				}
			}

			isProviderAllowed := func(prov string) bool {
				if len(allowedProviders) == 0 {
					return true
				}
				norm := orchestration.NormalizeProvider(prov)
				for _, ap := range allowedProviders {
					if orchestration.NormalizeProvider(ap) == norm {
						return true
					}
				}
				return false
			}

			for _, candidate := range matrix {
				parts := strings.Split(candidate, "/")
				candProv := parts[0]

				if !isProviderAllowed(candProv) {
					continue
				}

				if e != nil && !e.IsExecutable(candProv) {
					continue
				}

				health := 1.0
				switch orchestration.NormalizeProvider(candProv) {
				case "codex":
					health = e.HealthPercent(providers.BarCodexBurst)
				case "agy":
					health = e.HealthPercent(providers.BarAgyGemini)
				case "opencode":
					health = e.HealthPercent(providers.BarOpenCodeFree)
				}

				if health <= 0.0 {
					continue
				}

				if e.CircuitBreaker() != nil && !e.CircuitBreaker().Eligible(candProv) {
					continue
				}

				opusHealth := e.HealthPercent(providers.BarAgyOpus)
				if err := orchestration.ValidateModelAdmission(category, candidate, opusHealth); err != nil {
					continue
				}

				selectedModel = candidate
				selectedProvider = candProv
				selectedHealth = health
				break
			}

			if selectedModel != "" {
				effort, reason := orchestration.ResolveEffort(category, complexity, selectedHealth, selectedModel)

				routeDecision = map[string]any{
					"selected_model":  selectedModel,
					"resolved_effort": string(effort),
					"reason":          reason,
					"task_category":   category,
					"quota_pressure":  1.0 - selectedHealth,
				}

				modelName := selectedModel
				if parts := strings.SplitN(selectedModel, "/", 2); len(parts) == 2 {
					modelName = parts[1]
				}
				sub.Task.PreferredProvider = orchestration.NormalizeProvider(selectedProvider)
				sub.Task.PreferredModel = modelName
				sub.Task.TargetEffort = string(effort)
			}
		}

		dispatchStart := time.Now()
		j, err := e.Submit(sub)
		if err != nil {
			out := jobResponse(j)
			if routeDecision != nil {
				out["route_decision"] = routeDecision
			}
			return out, err
		}

		auditCreatedAt := time.Now().UTC()
		if routeDecision != nil && e.Store() != nil && j.ID != "" {
			_ = e.Store().RecordRoutingAudit(store.JobRoutingAudit{
				JobID:                   j.ID,
				TaskCategory:            routeDecision["task_category"].(string),
				AttemptNumber:           1,
				AssignedModel:           routeDecision["selected_model"].(string),
				AssignedEffort:          routeDecision["resolved_effort"].(string),
				RouteReason:             routeDecision["reason"].(string),
				QuotaPressureAtDispatch: routeDecision["quota_pressure"].(float64),
				DispatchStatus:          "DISPATCHED",
				LatencyMs:               time.Since(dispatchStart).Milliseconds(),
				CreatedAt:               auditCreatedAt,
			})
		}

		if execMode == "wait" {
			waitedJob, waitErr := e.Wait(ctx, j.ID, waitTimeout)
			latency := time.Since(dispatchStart).Milliseconds()
			dispatchStatus := "SUCCESS"
			if waitErr != nil || waitedJob.Status == model.Failed {
				dispatchStatus = "FAILED"
			}
			if routeDecision != nil && e.Store() != nil && j.ID != "" {
				_ = e.Store().RecordRoutingAudit(store.JobRoutingAudit{
					JobID:                   j.ID,
					TaskCategory:            routeDecision["task_category"].(string),
					AttemptNumber:           1,
					AssignedModel:           routeDecision["selected_model"].(string),
					AssignedEffort:          routeDecision["resolved_effort"].(string),
					RouteReason:             routeDecision["reason"].(string),
					QuotaPressureAtDispatch: routeDecision["quota_pressure"].(float64),
					DispatchStatus:          dispatchStatus,
					LatencyMs:               latency,
					CreatedAt:               auditCreatedAt,
				})
			}
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

	add(s, e, "delegate_task", "Delegate an objective to a bounded subagent. Executes in an isolated git worktree with verification and returns summary and verified diff. Defaults to synchronous execution (mode: wait).", false, handleDelegateTask)
	add(s, e, "spawn_subagent", "Spawn an autonomous subagent for isolated work. Alias for delegate_task.", false, handleDelegateTask)
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
		func(ctx context.Context, in model.Batch) (response, error) {
			if repo, ok := cfg.Repositories[in.RepoID]; ok && repo.PrivateSource != nil {
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
			j, err := e.Get(in.JobID)
			return jobResponse(j), err
		})
	add(s, e, "list_jobs", "List durable jobs visible under current repository policies.", false,
		func(_ context.Context, in listInput) (response, error) {
			if in.Limit == 0 {
				in.Limit = 20
			}
			if in.Limit < 1 || in.Limit > 100 || in.Cursor != "" && !model.ValidID(in.Cursor) {
				return nil, model.Error("INVALID_INPUT", "invalid pagination")
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
			j, err := e.Cancel(in.JobID)
			return jobResponse(j), err
		})
	add(s, e, "resume_job", "Resume a paused or interrupted job, or retry one malformed review, with the original deadline and snapshot.", false,
		func(_ context.Context, in resumeInput) (response, error) {
			j, err := e.Resume(in.JobID, in.RequestID)
			return jobResponse(j), err
		})
	add(s, e, "get_job_result", "Read a summary, route evidence, or a bounded page of a verified diff.", false,
		func(ctx context.Context, in resultInput) (response, error) {
			j, err := e.Get(in.JobID)
			if err != nil {
				return nil, err
			}
			out := jobResponse(j)
			switch in.View {
			case "", "summary":
				out["result"] = j.Result
			case "route":
				out["route"] = j.Route
				out["attempts"] = j.Attempts
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
				p, drift, err := e.Artifact(ctx, j, in.ArtifactID)
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
				if _, ok := cfg.Repositories[in.RepoID]; !ok {
					return nil, model.GateError("REPO_NOT_REGISTERED", "REPO_NOT_REGISTERED", "repository is not registered", "register_repository_in_config")
				}
			}
			capacity, warnings, preview := e.Capacity(in.RepoID)
			items := make([]model.Capability, 0)
			for _, c := range r.Candidates {
				if in.Provider != "" && orchestration.NormalizeProvider(c.Provider) != orchestration.NormalizeProvider(in.Provider) {
					continue
				}
				if in.RepoID != "" && c.Status == "available" {
					repoProviders := cfg.Repositories[in.RepoID].Providers
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
					if c.Provider == "agy" {
						if family := providers.AGYModelFamily(c.Model); family != "" {
							for _, h := range capacity.Headroom {
								if h.Family == family {
									c.HeadroomPercent, c.HeadroomState = h.Effective*100, h.State
									break
								}
							}
						}
					} else if c.Provider == "opencode" {
						c.HeadroomPercent, c.HeadroomState = 100, "HEALTHY"
					}
				}
				items = append(items, c)
			}
			out := response{
				"scheduler_status": "active",
				"version":          model.Version,
				"binary_sha256":    execution.SelfBinarySHA256(),
				"providers":        items,
				"local_runners":    readiness.LocalRunners(cfg),
				"routing_summary":  routingSummary(items, e.ActiveCircuitBreakers()),
			}
			if in.RepoID != "" {
				out["repo_admission"] = e.RepoAdmission(in.RepoID)
			}
			if len(warnings) != 0 {
				out["capacity_warnings"] = warnings
			}
			if in.IncludeQuota {
				out["quota"] = e.Quota(in.Provider)
				out["capacity"] = capacity
				out["capacity_policy"] = cfg.CapacityPolicy.Defaults()
				if in.RepoID != "" {
					out["routing_preview"] = preview
				}
			}
			return out, nil
		})

	add(s, e, "get_provider_quotas", "Read remaining percentages, states, and circuit states for all provider quota bars.", true,
		func(_ context.Context, _ emptyInput) (response, error) {
			trackedBars := []struct {
				Bucket   string
				Provider string
			}{
				{Bucket: providers.BarCodexBurst, Provider: "codex"},
				{Bucket: providers.BarCodexWeekly, Provider: "codex"},
				{Bucket: providers.BarAgyGemini, Provider: "antigravity"},
				{Bucket: providers.BarAgyOpus, Provider: "antigravity"},
				{Bucket: providers.BarOpenCodeFree, Provider: "opencode"},
			}
			bars := make([]map[string]any, 0, len(trackedBars))
			for _, b := range trackedBars {
				health := 1.0
				state := "HEALTHY"
				circuitState := "CLOSED"
				if e != nil {
					health = e.HealthPercent(b.Bucket)
					state = e.BarState(b.Bucket)
					if e.CircuitBreaker() != nil {
						circuitState = string(e.CircuitBreaker().State(b.Provider))
					}
				}
				bars = append(bars, map[string]any{
					"bucket":            b.Bucket,
					"provider":          b.Provider,
					"remaining_percent": health * 100.0,
					"health_pct":        health,
					"state":             state,
					"circuit_state":     circuitState,
				})
			}
			return response{"bars": bars}, nil
		})

	add(s, e, "get_active_circuit_breakers", "Read active provider circuit breakers and cooldown deadlines.", true,
		func(_ context.Context, _ emptyInput) (response, error) {
			tripped := make([]map[string]any, 0)
			if e != nil && e.CircuitBreaker() != nil {
				for p, until := range e.CircuitBreaker().ActiveTripped() {
					tripped = append(tripped, map[string]any{
						"provider":       p,
						"state":          "OPEN",
						"cooldown_until": until.Format(time.RFC3339),
					})
				}
			}
			if e != nil {
				for _, item := range e.ActiveCircuitBreakers() {
					tripped = append(tripped, item)
				}
			}
			return response{"tripped_breakers": tripped}, nil
		})

	add(s, e, "get_routing_audit", "Retrieve dynamic routing decision history and fallbacks for a given job ID.", true,
		func(_ context.Context, in jobID) (response, error) {
			if in.JobID == "" {
				return nil, model.Error("INVALID_INPUT", "job_id is required")
			}
			var history []store.JobRoutingAudit
			if e != nil && e.Store() != nil {
				var err error
				history, err = e.Store().GetRoutingAudit(in.JobID)
				if err != nil {
					return nil, err
				}
			}
			if history == nil {
				history = []store.JobRoutingAudit{}
			}
			return response{
				"job_id":   in.JobID,
				"attempts": history,
			}, nil
		})

	return s
}

// routingSummary recommends one available route per price/performance tier
// and lists the active circuit breakers.
func routingSummary(items []model.Capability, breakers []map[string]any) response {
	summary := response{"active_circuit_breakers": breakers}
	const criticalHeadroom = 5.0
	for tier, key := range map[string]string{
		"free": "recommended_free", "cheap": "recommended_cheap",
		"balanced": "recommended_balanced", "hard": "recommended_frontier",
	} {
		best, bestHeadroom := "", criticalHeadroom
		for _, c := range items {
			if c.Status != "available" || c.FPTier != tier {
				continue
			}
			if c.HeadroomPercent > bestHeadroom || best == "" && c.HeadroomState == "" {
				best, bestHeadroom = c.Provider+":"+c.Model, c.HeadroomPercent
			}
		}
		if best != "" {
			summary[key] = best
		}
	}
	return summary
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

func add[I any](s *mcp.Server, e *orchestration.Engine, name, description string, diagnostic bool, fn func(context.Context, I) (response, error)) {
	schema, err := jsonschema.For[I](nil)
	if err != nil {
		panic(err)
	}
	b, err := json.Marshal(schema)
	if err != nil {
		panic(err)
	}
	var wire map[string]any
	if err = json.Unmarshal(b, &wire); err != nil {
		panic(err)
	}
	s.AddTool(&mcp.Tool{Name: name, Description: description, InputSchema: wire}, func(ctx context.Context, r *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		if e == nil && !diagnostic {
			return toolError(model.GateError("STATE_BUSY", "STATE_BUSY", "another stdio session owns this state; reconnect after the owner exits", "reconnect_after_owner_exits")), nil
		}
		args := r.Params.Arguments
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
			return toolError(err), nil
		}
		out["schema_version"] = 1
		summary := "Ahenk " + name
		if st, ok := out["status"].(string); ok {
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

func closeObjects(node map[string]any) {
	if node["type"] == "object" {
		node["additionalProperties"] = false
	}
	for _, key := range []string{"properties", "$defs"} {
		if props, ok := node[key].(map[string]any); ok {
			for _, v := range props {
				if child, ok := v.(map[string]any); ok {
					closeObjects(child)
				}
			}
		}
	}
	if child, ok := node["items"].(map[string]any); ok {
		closeObjects(child)
	}
}

func toolError(err error) *mcp.CallToolResult {
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
	return &mcp.CallToolResult{IsError: true, StructuredContent: wire,
		Content: []mcp.Content{&mcp.TextContent{Text: f.Message}}}
}
