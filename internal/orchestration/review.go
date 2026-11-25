// Copyright 2026 Furkan Anıl Öztaş
// SPDX-License-Identifier: Apache-2.0

package orchestration

import (
	json "encoding/json/v2"
	"slices"
	"strings"
	"time"

	"github.com/faoztas/ahenk-mcp/internal/model"
)

func validateReviews(batch model.Batch, keys map[string]int) error {
	for _, item := range batch.Items {
		if item.ExecutionProfile != "review" {
			continue
		}
		if batch.Independence != "distinct_provider" || item.Mode != "read_only" ||
			(item.DataClass != "synthetic" && item.DataClass != model.PrivateSourceData) || len(item.HandoffFrom) != 1 || item.HandoffFrom[0].Kind != "diff" {
			return model.Error("INVALID_INPUT", "independent review requires one verified diff and a distinct provider")
		}
		source, ok := keys[item.HandoffFrom[0].Key]
		if !ok || batch.Items[source].Mode != "patch" || batch.Items[source].DataClass != item.DataClass {
			return model.Error("INVALID_INPUT", "independent review requires a matching patch dependency")
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
	if source.Task.Risk == "critical" || source.Task.ExecutionProfile == model.TrustedLocalProfile || source.Task.DataClass == model.PrivateSourceData {
		return model.Failed
	}
	return model.NotRun
}

// Recheck private acceptance under the scheduler lock. Artifact bytes can
// change between resolving the review handoff and completing its batch.
func (e *Engine) privateBatchReviewsBound(parent model.Job) bool {
	policy := e.cfg.Repositories[parent.RepoID].PrivateSource
	approved := make(map[string]bool)
	for _, id := range parent.Children {
		reviewer := e.jobs[id]
		if reviewer.Task.ExecutionProfile != "review" {
			continue
		}
		if !parent.Distinct || !e.repoAllowed(parent) || !policy.Active(time.Now()) || !policy.WriterIsolationVerified || !policy.AllowWrite {
			return false
		}
		sourceID := e.boundPrivateReviewSource(reviewer, policy)
		if sourceID == "" {
			return false
		}
		approved[sourceID] = true
	}
	return !slices.ContainsFunc(parent.Children, func(id string) bool {
		return e.jobs[id].Task.Mode == "patch" && !approved[id]
	})
}

func (e *Engine) boundPrivateReviewSource(reviewer model.Job, policy *model.PrivateSourcePolicy) string {
	if reviewer.Result == nil || reviewer.Result.Review == nil || len(reviewer.Attempts) == 0 {
		return ""
	}
	attempt := reviewer.Attempts[len(reviewer.Attempts)-1]
	if attempt.FinishedAt == nil || attempt.Code != "" || attempt.Provider == "" || e.distinctTaken(reviewer, attempt.Provider) {
		return ""
	}
	handoffs, err := e.resolveHandoffs(reviewer)
	if err != nil || len(handoffs) != 1 || handoffs[0].Review == nil ||
		model.Digest(attempt.Handoffs) != model.Digest(handoffManifests(handoffs)) {
		return ""
	}
	manifest := handoffs[0].Manifest
	review := reviewer.Result.Review
	if review.Verdict != "APPROVED" || review.ArtifactSHA256 != manifest.SHA256 || review.SourceJobID != manifest.SourceJobID ||
		review.SourceAttempt != manifest.SourceAttempt || review.CheckStatus != "PASSED" || handoffs[0].Review.CheckStatus != "PASSED" {
		return ""
	}
	writer := e.jobs[manifest.SourceJobID]
	if e.privateWriter == nil || !slices.Contains(writer.Result.Verification, "SYNTHETIC_PRIVATE_WRITE") || strings.TrimSpace(writer.Attempts[len(writer.Attempts)-1].Provider) == "" ||
		!slices.Contains(writer.Result.Verification, "SOURCE_SCOPE_AND_INDEX_UNCHANGED") {
		return ""
	}
	for _, criterion := range writer.Task.Acceptance {
		if criterion.CheckID == "" {
			continue
		}
		name := policy.GoChecks[criterion.CheckID]
		if name == "" || !slices.ContainsFunc(writer.Result.Checks, func(check model.CheckEvidence) bool {
			return check.ID == criterion.CheckID && check.Test == name && check.Status == "PASSED" && check.ExitStatus == 0
		}) {
			return ""
		}
	}
	return writer.ID
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
