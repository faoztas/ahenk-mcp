// Copyright 2026 Furkan Anıl Öztaş
// SPDX-License-Identifier: Apache-2.0

package readiness

import (
	"context"
	"crypto/rand"
	"errors"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/faoztas/ahenk-mcp/internal/execution"
	"github.com/faoztas/ahenk-mcp/internal/model"
	"github.com/faoztas/ahenk-mcp/internal/providers"
	"github.com/faoztas/ahenk-mcp/internal/store"
)

const (
	m1Path         = "m1-marker.txt"
	m1Seed         = "AHENK_M1_SEED"
	m1MarkerPrefix = "AHENK_HANDOFF_MARKER="
	m1Criterion    = "Add exactly " + m1MarkerPrefix
	m1Deadline     = 900
)

var (
	m1RequestID = regexp.MustCompile(`^m1-[A-Z2-7]{26}$`)
	m1Challenge = regexp.MustCompile(`^ahenk-handoff-[A-Z2-7]{26}$`)
	diffIndex   = regexp.MustCompile(`^index [a-f0-9]{7,64}\.\.[a-f0-9]{7,64} 100644$`)
)

type M1Evidence struct {
	Batch             *Evidence             `json:"batch"`
	Source            *Evidence             `json:"source"`
	Receiver          *Evidence             `json:"receiver"`
	Handoff           model.HandoffManifest `json:"handoff"`
	MarkerSHA256      string                `json:"marker_sha256"`
	SourceUnchanged   bool                  `json:"source_unchanged"`
	OpenCodeUsage     string                `json:"opencode_usage"`
	OpenCodeModelEcho string                `json:"opencode_model_echo"`
}

// PrepareM1 builds one request for the native MCP client after both live
// smoke gates are proven in the configured state. It does not submit it.
func PrepareM1(ctx context.Context, cfg model.Config, repoID string, now time.Time) (model.Batch, error) {
	report, err := Read(ctx, cfg, now)
	if err != nil {
		return model.Batch{}, err
	}
	if err := requireProviderSmokes(report, now); err != nil {
		return model.Batch{}, err
	}
	if _, err := m1Snapshot(ctx, cfg, repoID); err != nil {
		return model.Batch{}, err
	}
	return m1Batch(repoID, "m1-"+rand.Text(), "ahenk-handoff-"+rand.Text()), nil
}

// M1Routes recognizes M1 requests and derives receiver admission from the
// scheduler's configured capabilities and independently verified local evidence.
func M1Routes(ctx context.Context, cfg model.Config, reg providers.Registry, request model.Batch, now time.Time) ([]model.OpenCodeRoute, error) {
	if !m1RequestID.MatchString(request.RequestID) {
		return nil, nil
	}
	if _, err := validateM1Request(request, "pending"); err != nil {
		return nil, model.Error("M1_REQUEST_MISMATCH", "invalid M1 batch shape")
	}
	dir, jobs, imports, err := loadEvidence(ctx, cfg)
	if err != nil {
		return nil, err
	}
	report := evaluate(cfg, reg, dir, jobs, imports, now)
	if err := requireProviderSmokes(report, now); err != nil {
		return nil, err
	}
	if _, err := m1Snapshot(ctx, cfg, request.RepoID); err != nil {
		return nil, err
	}
	var routes []model.OpenCodeRoute
	for _, c := range reg.Candidates {
		if c.Provider == "opencode" && c.Status == "available" && requireProviderSmokes(report.forOpenCode(c), now) == nil {
			routes = append(routes, model.OpenCodeRoute{Model: c.Model, Account: model.OpenCodeAccountID(c.OpenCodeAccount),
				Fingerprint: c.Fingerprint, OpenCodeTransport: model.OpenCodeTransportID(c.OpenCodeTransport)})
		}
	}
	if len(routes) == 0 {
		return nil, model.Error("M1_ROUTE_NOT_VERIFIED", "no OpenCode model has verified M1 admission")
	}
	return routes, nil
}

func m1SmokesAt(agy, opencode model.Capability, jobs map[string]store.JobEvidence, gates []FreeGate, before time.Time) Report {
	report := Report{AGY: liveGateAt(agy, jobs, before)}
	m := openCodeModelReport(opencode, liveGateAt(opencode, jobs, before), gates, before)
	return report.withOpenCode(m)
}

func requireProviderSmokes(report Report, before time.Time) error {
	if report.OpenCodeRecoveryGate == nil || (report.OpenCodeRecovery != "RECORDED_GATE_ELAPSED" &&
		report.OpenCodeRecovery != "RECORDED_GATE_OTHER_MODEL" && report.OpenCodeRecovery != "RECORDED_GATE_OTHER_TRANSPORT" &&
		report.OpenCodeRecovery != "RECORDED_GATE_SUPERSEDED") {
		return model.Error("OPENCODE_RECOVERY_NOT_VERIFIED", "M1 requires a recorded OpenCode rate limit and an unblocked selected route")
	}
	for _, gate := range []Gate{report.AGY, report.OpenCode} {
		if !gate.Ready || gate.Observation == nil || gate.Observation.FinishedAt == nil || gate.Observation.FinishedAt.After(before) {
			return model.Error("LIVE_PROVIDER_GATES_CLOSED", "M1 requires both verified live smokes in the same state before the batch")
		}
	}
	smoke, rateLimit := report.OpenCode.Observation, report.OpenCodeRecoveryGate
	if report.OpenCodeRecovery == "RECORDED_GATE_OTHER_MODEL" {
		if rateLimit.Scope != "model" || rateLimit.Model == "" || smoke.Model == "" || smoke.Model == rateLimit.Model || smoke.ObservedAt == nil {
			return model.Error("OPENCODE_RECOVERY_NOT_VERIFIED", "the verified smoke must belong to a different OpenCode model")
		}
		return nil
	}
	if report.OpenCodeRecovery == "RECORDED_GATE_OTHER_TRANSPORT" {
		if rateLimit.Scope != "model" || rateLimit.ProviderScope == "model" || rateLimit.Model == "" ||
			smoke.Model != rateLimit.Model || smoke.ObservedAt == nil ||
			model.OpenCodeTransportID(smoke.OpenCodeTransport) == model.OpenCodeTransportID(rateLimit.OpenCodeTransport) {
			return model.Error("OPENCODE_RECOVERY_NOT_VERIFIED", "the verified smoke must belong to a different OpenCode transport")
		}
		return nil
	}
	if report.OpenCodeRecovery == "RECORDED_GATE_SUPERSEDED" {
		if smoke.Model != rateLimit.Model || smoke.ObservedAt == nil || !smoke.ObservedAt.After(rateLimit.ObservedAt) {
			return model.Error("OPENCODE_RECOVERY_NOT_VERIFIED", "the verified smoke predates the same-route observation")
		}
		return nil
	}
	if smoke.ObservedAt == nil || smoke.ObservedAt.Before(rateLimit.RetryAt) {
		return model.Error("OPENCODE_RECOVERY_NOT_VERIFIED", "the verified OpenCode smoke predates the applicable recovery evidence")
	}
	return nil
}

func m1Batch(repoID, requestID, marker string) model.Batch {
	source := model.Task{
		Objective: "Append exactly the line " + m1MarkerPrefix + marker + " to the supplied " + m1Path +
			". Return a unified diff for that single addition with the seed line as context. Do not change the seed line.",
		Acceptance:   []model.Check{{ID: "handoff-marker", Description: m1Criterion + marker}},
		AllowedPaths: []string{m1Path}, Mode: "patch", DataClass: "synthetic", ExecutionProfile: "restricted", DeadlineSeconds: m1Deadline,
	}
	receiver := model.Task{
		Objective: "Read the verified diff handed off from the previous task. Return only the value after " +
			m1MarkerPrefix + " on the added line. Use the diff content.",
		Acceptance:   []model.Check{{ID: "handoff-response", Description: "Return exactly the marker value added by the diff"}},
		AllowedPaths: []string{m1Path}, Mode: "read_only", DataClass: "synthetic", DeadlineSeconds: m1Deadline,
	}
	return model.Batch{RepoID: repoID, RequestID: requestID, Independence: "distinct_provider", WorkspaceMode: "head_only",
		DeadlineSeconds: m1Deadline, Items: []model.Item{
			{Key: "produce", Task: source},
			{Key: "receive", Task: receiver, DependsOn: []string{"produce"}, HandoffFrom: []model.HandoffFrom{{Key: "produce", Kind: "diff"}}},
		}}
}

func markerDeclared(t model.Task, marker string) bool {
	return slices.Contains(t.Acceptance, model.Check{ID: "handoff-marker", Description: m1Criterion + marker})
}

func m1Snapshot(ctx context.Context, cfg model.Config, repoID string) (execution.Snapshot, error) {
	repo, ok := cfg.Repositories[repoID]
	if !ok || !slices.Equal(repo.AllowedPaths, []string{m1Path}) || len(repo.Providers) != 2 ||
		!slices.Contains(repo.Providers, "agy") || !slices.Contains(repo.Providers, "opencode") {
		return execution.Snapshot{}, model.Error("M1_REPO_POLICY", "M1 requires a dedicated synthetic policy with one marker file and only AGY and OpenCode")
	}
	s, err := execution.Capture(ctx, repo, model.Task{WorkspaceMode: "head_only", AllowedPaths: []string{m1Path, m1Path}})
	if err != nil {
		return execution.Snapshot{}, err
	}
	if s.Dirty || len(s.Files) != 1 || s.Files[0].Path != m1Path || s.Files[0].Effective == nil ||
		s.Files[0].Effective.Mode != "100644" || string(s.Files[0].Effective.Bytes) != m1Seed+"\n" {
		return execution.Snapshot{}, model.Error("M1_REPO_POLICY", "M1 requires a clean committed marker file containing the synthetic seed")
	}
	return s, nil
}

// VerifyM1 checks the exact request and parent without running a worker or
// trusting the provider's summary as a verification assertion.
func VerifyM1(ctx context.Context, cfg model.Config, request model.Batch, parentID string, now time.Time) (M1Evidence, error) {
	marker, err := validateM1Request(request, parentID)
	if err != nil {
		return M1Evidence{}, err
	}
	dir, jobs, imports, err := loadEvidence(ctx, cfg)
	if err != nil {
		return M1Evidence{}, err
	}
	reg := providers.New(cfg)
	current := currentEvidence(cfg, jobs)
	parent := current[parentID]
	if parent.ID == "" || parent.RequestID != request.RequestID || parent.RepoID != request.RepoID || len(parent.Children) != 2 {
		return M1Evidence{}, model.Error("M1_REQUEST_MISMATCH", "parent does not belong to the supplied M1 request")
	}
	opencode := openCodeBatchCapability(parent, current, reg)
	if parent.CreatedAt.After(now) {
		return M1Evidence{}, model.Error("M1_REQUEST_MISMATCH", "batch creation is later than the verification time")
	}
	if err := requireProviderSmokes(m1SmokesAt(pilotAGY(reg), opencode, current, openCodeGateHistory(jobs, imports), parent.CreatedAt), parent.CreatedAt); err != nil {
		return M1Evidence{}, err
	}
	source, receiver := current[parent.Children[0]], current[parent.Children[1]]
	if len(source.DependsOn) != 0 || len(source.HandoffFrom) != 0 || !slices.Equal(receiver.DependsOn, []string{source.ID}) {
		return M1Evidence{}, model.Error("M1_REQUEST_MISMATCH", "recorded dependencies differ from the original M1 request")
	}
	repo := cfg.Repositories[request.RepoID]
	for i, j := range []store.JobEvidence{source, receiver} {
		expected := request.Items[i]
		if expected.Task.Validate(repo) != nil || expected.Key != j.Key || model.Digest(expected.Task) != model.Digest(j.Task) {
			return M1Evidence{}, model.Error("M1_REQUEST_MISMATCH", "recorded child task differs from the original M1 request")
		}
	}
	proof := batchProof(dir, parent, current, pilotAGY(reg), opencode, false)
	if proof == nil {
		return M1Evidence{}, model.Error("M1_BATCH_UNVERIFIED", "live batch, terminal, audit or handoff evidence is incomplete")
	}
	s, err := m1Snapshot(ctx, cfg, request.RepoID)
	if err != nil {
		return M1Evidence{}, err
	}
	if s.ID != parent.SnapshotID {
		return M1Evidence{}, model.Error("M1_SOURCE_DRIFT", "the synthetic checkout, HEAD or scoped index changed")
	}
	diff, err := execution.ReadVerifiedDiff(filepath.Join(dir, "artifacts"), source.Result, source.ID, 64<<10)
	if err != nil || !exactM1Diff(string(diff), marker) || receiver.Result.Summary != marker {
		return M1Evidence{}, model.Error("M1_MARKER_MISMATCH", "the verified diff or response differs from the one exact requested marker addition")
	}
	a, b := source.Attempts[0], receiver.Attempts[0]
	exposure := "NOT_EXPOSED"
	if openCodeRunVerified(b) {
		exposure = "VERIFIED"
	}
	return M1Evidence{Batch: proof, Source: attemptEvidence(source, a, a.StartedAt), Receiver: attemptEvidence(receiver, b, b.StartedAt),
		Handoff: b.Handoffs[0], MarkerSHA256: model.Hash([]byte(marker)), SourceUnchanged: true,
		OpenCodeUsage: exposure, OpenCodeModelEcho: exposure}, nil
}

func validateM1Request(request model.Batch, parentID string) (string, error) {
	if model.ValidID(parentID) && model.ValidID(request.RepoID) && m1RequestID.MatchString(request.RequestID) &&
		len(request.Items) == 2 && len(request.Items[0].Acceptance) == 1 {
		marker, ok := strings.CutPrefix(request.Items[0].Acceptance[0].Description, m1Criterion)
		if ok && m1Challenge.MatchString(marker) && model.Digest(request) == model.Digest(m1Batch(request.RepoID, request.RequestID, marker)) {
			return marker, nil
		}
	}
	return "", errors.New("invalid M1 request or parent ID")
}

func exactM1Diff(diff, marker string) bool {
	lines := strings.Split(diff, "\n")
	if len(lines) > 0 && lines[0] == "diff --git a/"+m1Path+" b/"+m1Path {
		lines = lines[1:]
		if len(lines) > 0 && diffIndex.MatchString(lines[0]) {
			lines = lines[1:]
		}
	}
	if len(lines) > 2 && lines[2] == "@@ -1,1 +1,2 @@" {
		lines[2] = "@@ -1 +1,2 @@"
	}
	return slices.Equal(lines, []string{"--- a/" + m1Path, "+++ b/" + m1Path,
		"@@ -1 +1,2 @@", " " + m1Seed, "+" + m1MarkerPrefix + marker, ""})
}
