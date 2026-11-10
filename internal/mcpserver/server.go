// Copyright 2026 Furkan Anıl Öztaş
// SPDX-License-Identifier: Apache-2.0

package mcpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"slices"
	"strconv"
	"time"
	"unicode/utf8"

	"github.com/faoztas/ahenk-mcp/internal/model"
	"github.com/faoztas/ahenk-mcp/internal/orchestration"
	"github.com/faoztas/ahenk-mcp/internal/providers"
	"github.com/faoztas/ahenk-mcp/internal/readiness"
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
	add(s, e, "delegate_task", "Queue a bounded task and return its durable job ID.", false,
		func(_ context.Context, in model.Submission) (response, error) {
			j, err := e.Submit(in)
			return jobResponse(j), err
		})
	add(s, e, "delegate_batch", "Queue up to eight tasks with dependencies and optional verified result handoffs.", false,
		func(ctx context.Context, in model.Batch) (response, error) {
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
				return response{"scheduler_status": "state_busy"}, nil
			}
			if in.RepoID != "" {
				if _, ok := cfg.Repositories[in.RepoID]; !ok {
					return nil, model.Error("REPO_NOT_REGISTERED", "repository is not registered")
				}
			}
			items := make([]model.Capability, 0)
			for _, c := range r.Candidates {
				if in.Provider != "" && c.Provider != in.Provider {
					continue
				}
				if in.RepoID != "" && c.Status == "available" && !slices.Contains(cfg.Repositories[in.RepoID].Providers, c.Provider) {
					c.Status = "disabled"
					c.Reason = "REPO_PROVIDER_POLICY"
				}
				if c.Status == "available" {
					c.Quality = e.RoleQualities(c)
				}
				items = append(items, c)
			}
			out := response{"scheduler_status": "active", "providers": items}
			capacity, warnings, preview := e.Capacity(in.RepoID)
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
	return s
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
	closeObjects(wire)
	s.AddTool(&mcp.Tool{Name: name, Description: description, InputSchema: wire}, func(ctx context.Context, r *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		if e == nil && !diagnostic {
			return toolError(model.Error("STATE_BUSY", "another stdio session owns this state")), nil
		}
		args := r.Params.Arguments
		if len(args) == 0 {
			args = []byte("{}")
		}
		d := json.NewDecoder(bytes.NewReader(args))
		d.DisallowUnknownFields()
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
	return &mcp.CallToolResult{IsError: true, StructuredContent: response{"schema_version": 1, "code": f.Code, "message": f.Message, "retryable": f.Retryable},
		Content: []mcp.Content{&mcp.TextContent{Text: f.Message}}}
}
