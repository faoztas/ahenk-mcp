// Copyright 2026 Furkan Anıl Öztaş
// SPDX-License-Identifier: Apache-2.0

package orchestration

import (
	"slices"
	"sort"
	"unicode/utf8"

	"github.com/faoztas/ahenk-mcp/internal/execution"
	"github.com/faoztas/ahenk-mcp/internal/model"
)

const maxHandoffBytes = 64 << 10

func validateHandoffs(item model.Item) error {
	seen := map[model.HandoffFrom]bool{}
	for _, ref := range item.HandoffFrom {
		if len(item.HandoffFrom) > 14 || !slices.Contains(item.DependsOn, ref.Key) ||
			(ref.Kind != "diff" && ref.Kind != "summary") || seen[ref] {
			return model.Error("INVALID_INPUT", "handoff must name a direct dependency and a unique supported output")
		}
		seen[ref] = true
	}
	return nil
}

// resolveHandoffs runs under the scheduler lock before a worker is assigned.
// The source result is immutable and a retry must resolve the same manifests.
func (e *Engine) resolveHandoffs(j model.Job) ([]model.Handoff, error) {
	if len(j.HandoffFrom) == 0 {
		return nil, nil
	}
	invalid := model.Error("HANDOFF_INVALID", "dependency output is missing, changed or outside the receiving task policy")
	if j.Parent == "" || j.SnapshotID == "" {
		return nil, invalid
	}
	handoffs := make([]model.Handoff, 0, len(j.HandoffFrom))
	remaining := maxHandoffBytes
	for _, ref := range j.HandoffFrom {
		var source model.Job
		for _, id := range j.DependsOn {
			if prior := e.jobs[id]; prior.Key == ref.Key {
				source = prior
				break
			}
		}
		if source.Status != model.Completed || source.Parent != j.Parent || source.RepoID != j.RepoID ||
			source.SnapshotID != j.SnapshotID || source.BaseSHA != j.BaseSHA ||
			source.PolicyDigest != j.PolicyDigest || source.Task.DataClass != j.Task.DataClass ||
			source.Result == nil || len(source.Result.Verification) == 0 || len(source.Attempts) == 0 {
			return nil, invalid
		}
		attempt := source.Attempts[len(source.Attempts)-1]
		if attempt.FinishedAt == nil || attempt.Code != "" {
			return nil, invalid
		}
		var content []byte
		var digest string
		switch ref.Kind {
		case "summary":
			for _, name := range source.Task.AllowedPaths {
				if !model.InScope(name, j.Task.AllowedPaths) {
					return nil, invalid
				}
			}
			content, digest = []byte(source.Result.Summary), source.Result.SummarySHA256
		case "diff":
			if !slices.Contains(source.Result.Verification, "PATCH_APPLIES_TO_SNAPSHOT") ||
				len(source.Result.Preimages) == 0 {
				return nil, invalid
			}
			for name := range source.Result.Preimages {
				if !model.SafePath(name) || !model.InScope(name, j.Task.AllowedPaths) {
					return nil, invalid
				}
			}
			var err error
			content, err = e.readVerifiedDiff(source, source.Result.ArtifactID, int64(remaining))
			if err != nil {
				return nil, invalid
			}
			digest = source.Result.ArtifactSHA256
		default:
			return nil, invalid
		}
		if len(content) == 0 || len(content) > remaining || !utf8.Valid(content) ||
			model.Hash(content) != digest || model.ContainsSecret(content) {
			return nil, invalid
		}
		remaining -= len(content)
		manifest := model.HandoffManifest{SourceJobID: source.ID, SourceAttempt: attempt.Number,
			SnapshotID: j.SnapshotID, Kind: ref.Kind, SHA256: digest, AcceptanceSHA256: model.Digest(source.Task.Acceptance)}
		for _, criterion := range source.Task.Acceptance {
			manifest.AcceptanceIDs = append(manifest.AcceptanceIDs, criterion.ID)
		}
		handoff := model.Handoff{Manifest: manifest, Content: string(content)}
		if j.Task.ExecutionProfile == "review" {
			checkStatus := reviewCheckStatus(source)
			if len(j.HandoffFrom) != 1 || ref.Kind != "diff" || !e.independentReviewRequested(source) || checkStatus == model.Failed {
				return nil, invalid
			}
			paths := make([]string, 0, len(source.Result.Preimages))
			for path := range source.Result.Preimages {
				paths = append(paths, path)
			}
			sort.Strings(paths)
			handoff.Review = &model.ReviewInput{Objective: source.Task.Objective, Acceptance: source.Task.Acceptance,
				ChangedPaths: paths, Verification: source.Result.Verification, CheckStatus: checkStatus, Checks: source.Result.Checks}
		}
		handoffs = append(handoffs, handoff)
	}
	if len(j.Attempts) > 0 && model.Digest(j.Attempts[0].Handoffs) != model.Digest(handoffManifests(handoffs)) {
		return nil, invalid
	}
	return handoffs, nil
}

func handoffManifests(handoffs []model.Handoff) []model.HandoffManifest {
	if len(handoffs) == 0 {
		return nil
	}
	manifests := make([]model.HandoffManifest, 0, len(handoffs))
	for _, handoff := range handoffs {
		manifests = append(manifests, handoff.Manifest)
	}
	return manifests
}

func (e *Engine) readVerifiedDiff(j model.Job, id string, limit int64) ([]byte, error) {
	return execution.ReadVerifiedDiff(e.artifacts, j.Result, id, limit)
}
