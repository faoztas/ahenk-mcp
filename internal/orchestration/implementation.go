// Copyright 2026 Furkan Anıl Öztaş
// SPDX-License-Identifier: Apache-2.0

package orchestration

import (
	"context"
	"errors"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/faoztas/ahenk-mcp/internal/execution"
	"github.com/faoztas/ahenk-mcp/internal/model"
	"github.com/faoztas/ahenk-mcp/internal/providers"
)

// Exercise the writer artifact lifecycle with a supervised offline fixture.
// Live writers require a separately verified provider and test boundary.
func (e *Engine) runFixtureImplementation(ctx context.Context, dir string, c model.Capability, t model.Task,
	snapshot execution.Snapshot, files map[string]string, handoffs []model.Handoff,
) (out execution.FixtureInput, collected *execution.WorktreeResult, err error) {
	w, err := execution.PrepareWorktree(ctx, dir, snapshot, t)
	if err != nil {
		return out, nil, err
	}
	defer func() {
		if cleanupErr := w.Close(); cleanupErr != nil {
			err = cleanupErr
		}
	}()
	out, err = e.reg.Run(ctx, dir, c, t, files, handoffs)
	if err != nil || out.Failure != "" {
		return out, nil, err
	}
	if err = w.ApplyPatch(ctx, []byte(out.Patch)); err != nil {
		return out, nil, err
	}
	result, err := w.Collect(ctx, e.scratch)
	if err != nil {
		return out, nil, err
	}
	if !result.SourceUnchanged {
		return out, nil, model.Error("SNAPSHOT_CHANGED", "source changed during the private worktree attempt")
	}
	out.Patch = string(result.Patch)
	return out, &result, nil
}

func (e *Engine) runTrustedImplementation(ctx context.Context, dir string, c model.Capability, t model.Task,
	snapshot execution.Snapshot, files map[string]string, policy *model.TrustedLocalPolicy, checkpoint *model.Checkpoint,
) (out execution.FixtureInput, collected *execution.WorktreeResult, checks []model.CheckEvidence, err error) {
	writePaths := slices.DeleteFunc(slices.Clone(policy.WritePaths), func(name string) bool {
		return !model.InScope(name, t.AllowedPaths)
	})
	for _, criterion := range t.Acceptance {
		id := criterion.CheckID
		if id != "" && !slices.ContainsFunc(checks, func(check model.CheckEvidence) bool { return check.ID == id }) {
			checks = append(checks, model.CheckEvidence{ID: id, Test: policy.GoChecks[id], Status: model.NotRun, ExitStatus: -1})
		}
	}
	if len(writePaths) == 0 || len(checks) == 0 {
		return out, nil, checks, model.Error("POLICY_DENIED", "trusted-local requires an approved write path and registered check")
	}
	tool, err := execution.GoCheckTool()
	if err != nil {
		return out, nil, checks, err
	}
	reads := slices.Clone(writePaths)
	hasTest := false
	for name := range files {
		if !model.ValidTrustedLocalPath(name) {
			return out, nil, checks, model.Error("POLICY_DENIED", "unsupported trusted-local snapshot path syntax")
		}
		if !slices.Contains(reads, name) {
			reads = append(reads, name)
		}
		hasTest = hasTest || !strings.Contains(name, "/") && strings.HasSuffix(name, "_test.go")
	}
	if _, hasModule := files["go.mod"]; !hasModule || !hasTest || len(reads) > 128 {
		return out, nil, checks, model.Error("DETERMINISTIC_CHECK_NOT_CONFIGURED", "trusted-local requires a scoped Go module and root test files")
	}
	slices.Sort(reads)
	providerTask := t
	providerTask.AllowedPaths = reads
	w, err := execution.PrepareWorktree(ctx, dir, snapshot, t)
	if err != nil {
		return out, nil, checks, err
	}
	defer func() {
		err = errors.Join(err, w.Close())
	}()
	if t.CheckpointObjective != "" && checkpoint == nil {
		providerTask.Objective = "Make only the following partial change, then finish so Ahenk can store a checkpoint. Leave remaining work for the next provider. " + t.CheckpointObjective
	}
	if checkpoint != nil {
		if err := e.restoreCheckpoint(ctx, w, snapshot.ID, checkpoint, writePaths); err != nil {
			return out, nil, checks, err
		}
		providerTask.Objective = "Continue from the verified partial edits already in this worktree. Complete the original objective and all checks. " + t.Objective
	}
	out, err = e.reg.RunTrustedLocal(ctx, dir, w.Dir, c, providerTask, writePaths)
	if err != nil {
		if ctx.Err() != nil && !errors.Is(err, execution.ErrCleanup) {
			verify, cancel := context.WithTimeout(context.WithoutCancel(ctx), 8*time.Second)
			partial, collectErr := w.Collect(verify, e.scratch)
			cancel()
			if collectErr == nil && partial.SourceUnchanged && checkpointScope(partial.Preimages, writePaths) {
				return out, &partial, checks, err
			}
		}
		return out, nil, checks, err
	}
	result, err := w.Collect(ctx, e.scratch)
	if err != nil {
		return out, nil, checks, err
	}
	for name := range result.Preimages {
		if !slices.Contains(writePaths, name) {
			return out, nil, checks, model.Error("POLICY_DENIED", "native edit changed a protected file")
		}
	}
	if !result.SourceUnchanged {
		return out, nil, checks, model.Error("SNAPSHOT_CHANGED", "source changed during the trusted-local attempt")
	}
	if t.CheckpointObjective != "" && checkpoint == nil {
		return out, &result, checks, model.Error("CHECKPOINT_READY", "verified partial change is ready to persist")
	}
	verifiedChecks, err := execution.RunGoChecks(ctx, dir, w.Dir, tool, checks)
	if err != nil {
		return out, nil, checks, err
	}
	if len(verifiedChecks) != len(checks) {
		return out, nil, checks, model.Error("CHECK_FAILED", "registered Go check evidence is incomplete")
	}
	for i, check := range verifiedChecks {
		if check.ID != checks[i].ID || check.Test != checks[i].Test || check.Status != "PASSED" || check.ExitStatus != 0 {
			return out, nil, verifiedChecks, model.Error("CHECK_FAILED", "a registered Go check did not pass")
		}
	}
	after, err := w.Collect(ctx, e.scratch)
	if err != nil {
		return out, nil, verifiedChecks, err
	}
	if after.ArtifactSHA256 != result.ArtifactSHA256 || !after.SourceUnchanged {
		return out, nil, verifiedChecks, model.Error("SNAPSHOT_CHANGED", "Go verification changed the worktree or source")
	}
	out.Patch = string(after.Patch)
	return out, &after, verifiedChecks, nil
}

func checkpointScope(preimages map[string]string, writePaths []string) bool {
	if len(preimages) == 0 {
		return false
	}
	for name := range preimages {
		if !slices.Contains(writePaths, name) {
			return false
		}
	}
	return true
}

func (e *Engine) restoreCheckpoint(ctx context.Context, w *execution.Worktree, snapshot string, checkpoint *model.Checkpoint, writePaths []string) error {
	if checkpoint.SnapshotID != snapshot || checkpoint.SourceFingerprint != w.SourceFingerprint() ||
		!checkpointScope(checkpoint.Preimages, writePaths) {
		return model.Error("SNAPSHOT_CHANGED", "checkpoint no longer matches the source or write policy")
	}
	patch, err := execution.ReadVerifiedDiff(e.artifacts, &model.Result{
		ArtifactID: checkpoint.ArtifactID, ArtifactSHA256: checkpoint.SHA256,
	}, checkpoint.ArtifactID, execution.MaxArtifact)
	if err != nil {
		return err
	}
	if err := w.ApplyPatch(ctx, patch); err != nil {
		return err
	}
	restored, err := w.Collect(ctx, e.scratch)
	if err != nil {
		return err
	}
	if !restored.SourceUnchanged || restored.ArtifactSHA256 != checkpoint.SHA256 || !maps.Equal(restored.Preimages, checkpoint.Preimages) {
		return model.Error("SNAPSHOT_CHANGED", "restored checkpoint did not match its source preimages and artifact")
	}
	return nil
}

func latestCheckpoint(j model.Job) *model.Checkpoint {
	if j.Task.ExecutionProfile == model.TrustedLocalProfile && j.ResumeRequest != "" {
		for _, attempt := range slices.Backward(j.Attempts) {
			if attempt.Checkpoint != nil {
				return attempt.Checkpoint
			}
		}
	}
	return nil
}

// A planned recovery must change provider families. A repository can still
// use its existing same-provider recovery policy for unplanned interruptions.
func checkpointProvider(j model.Job) string {
	if j.Task.CheckpointObjective != "" && j.ResumeRequest != "" {
		for _, attempt := range slices.Backward(j.Attempts) {
			if attempt.Checkpoint != nil {
				return providers.Family(attempt.Provider)
			}
		}
	}
	return ""
}
