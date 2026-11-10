// Copyright 2026 Furkan Anıl Öztaş
// SPDX-License-Identifier: Apache-2.0

package orchestration

import (
	json "encoding/json/v2"
	"slices"
	"strings"

	"github.com/faoztas/ahenk-mcp/internal/model"
)

func validateReviews(batch model.Batch, keys map[string]int) error {
	for _, item := range batch.Items {
		if item.ExecutionProfile != "review" {
			continue
		}
		if batch.Independence != "distinct_provider" || item.Mode != "read_only" ||
			item.DataClass != "synthetic" || len(item.HandoffFrom) != 1 || item.HandoffFrom[0].Kind != "diff" {
			return model.Error("INVALID_INPUT", "independent review requires one verified diff and a distinct provider")
		}
		source, ok := keys[item.HandoffFrom[0].Key]
		if !ok || batch.Items[source].Mode != "patch" || batch.Items[source].DataClass != "synthetic" {
			return model.Error("INVALID_INPUT", "independent review requires a synthetic patch dependency")
		}
	}
	return nil
}

// Bind the verdict to the resolved artifact. The provider supplies only the
// verdict and findings. It cannot assert artifact identity or check status.
func reviewResult(summary string, handoff model.Handoff, paths []string) (*model.ReviewResult, error) {
	invalid := model.Error("INVALID_OUTPUT", "invalid structured review")
	summary = strings.TrimSpace(summary)
	if body, ok := strings.CutPrefix(summary, "```json\n"); ok {
		var closed bool
		summary, closed = strings.CutSuffix(body, "\n```")
		if !closed {
			return nil, invalid
		}
	}
	var wire struct {
		Verdict  string                `json:"verdict"`
		Findings []model.ReviewFinding `json:"findings"`
	}
	if model.ContainsSecret([]byte(summary)) || json.Unmarshal([]byte(summary), &wire, json.RejectUnknownMembers(true)) != nil ||
		!slices.Contains([]string{"APPROVED", "CHANGES_REQUESTED", model.Blocked, model.NotRun}, wire.Verdict) ||
		wire.Findings == nil || len(wire.Findings) > 16 || wire.Verdict == "CHANGES_REQUESTED" && len(wire.Findings) == 0 {
		return nil, invalid
	}
	for _, finding := range wire.Findings {
		if !slices.Contains([]string{"low", "medium", "high", "critical"}, finding.Severity) ||
			strings.TrimSpace(finding.Message) == "" || len(finding.Message) > 1024 ||
			finding.Path != "" && (!model.SafePath(finding.Path) || !model.InScope(finding.Path, paths)) ||
			wire.Verdict == "APPROVED" && (finding.Severity == "high" || finding.Severity == "critical") {
			return nil, invalid
		}
	}
	checkStatus := handoff.Review.CheckStatus
	if checkStatus != model.NotRun && checkStatus != "PASSED" && wire.Verdict == "APPROVED" {
		wire.Verdict = model.Blocked
	}
	return &model.ReviewResult{
		Verdict: wire.Verdict, Findings: wire.Findings, ArtifactSHA256: handoff.Manifest.SHA256,
		SourceJobID: handoff.Manifest.SourceJobID, SourceAttempt: handoff.Manifest.SourceAttempt, CheckStatus: checkStatus,
	}, nil
}

func (e *Engine) independentReviewRequested(source model.Job) bool {
	parent := e.jobs[source.Parent]
	return (parent.Distinct || parent.Task.Decompose) && slices.ContainsFunc(parent.Children, func(id string) bool {
		reviewer := e.jobs[id]
		return reviewer.Task.ExecutionProfile == "review" && slices.Contains(reviewer.DependsOn, source.ID) &&
			len(reviewer.HandoffFrom) == 1 && reviewer.HandoffFrom[0].Kind == "diff" && reviewer.HandoffFrom[0].Key == source.Key
	})
}

func reviewCheckStatus(source model.Job) string {
	required := false
	for _, criterion := range source.Task.Acceptance {
		if criterion.CheckID == "" {
			continue
		}
		required = true
		if !slices.ContainsFunc(source.Result.Checks, func(check model.CheckEvidence) bool {
			return check.ID == criterion.CheckID && check.Status == "PASSED" && check.ExitStatus == 0
		}) {
			return model.Failed
		}
	}
	if required {
		return "PASSED"
	}
	if source.Task.Risk == "critical" || source.Task.ExecutionProfile == model.TrustedLocalProfile {
		return model.Failed
	}
	return model.NotRun
}

// A malformed review can be retried once. The original producer and its
// verified artifact remain terminal and are never redispatched.
func canRetryReview(j model.Job) bool {
	if j.Task.ExecutionProfile != "review" || len(j.HandoffFrom) != 1 || j.HandoffFrom[0].Kind != "diff" ||
		len(j.Attempts) != 1 || j.Attempts[0].Code != "INVALID_OUTPUT" || j.Attempts[0].FinishedAt == nil {
		return false
	}
	a := j.Attempts[0]
	return a.AGY != nil && a.AGY.TerminalVerified || a.OpenCode != nil && a.OpenCode.TerminalVerified
}
