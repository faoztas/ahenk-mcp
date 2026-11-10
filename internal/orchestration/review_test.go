// Copyright 2026 Furkan Anıl Öztaş
// SPDX-License-Identifier: Apache-2.0

package orchestration

import (
	"context"
	"testing"
	"time"

	"github.com/faoztas/ahenk-mcp/internal/model"
)

func TestReviewCannotApproveFailedCheckOrAssertArtifactIdentity(t *testing.T) {
	handoff := model.Handoff{
		Manifest: model.HandoffManifest{SHA256: "artifact", SourceJobID: "source", SourceAttempt: 1},
		Review:   &model.ReviewInput{CheckStatus: model.Failed},
	}
	result, err := reviewResult(`{"verdict":"APPROVED","findings":[]}`, handoff, nil)
	if err != nil || result.Verdict != model.Blocked || result.ArtifactSHA256 != "artifact" || result.CheckStatus != model.Failed {
		t.Fatalf("failed deterministic check overruled: %v %v", result, err)
	}
	for _, input := range []string{
		`{"verdict":"CHANGES_REQUESTED","verdict":"APPROVED","findings":[]}`,
		`{"verdict":"APPROVED","findings":[],"artifact_sha256":"foreign"}`,
		`{"verdict":"APPROVED","findings":[{"severity":"critical","message":"A boundary is missing."}]}`,
		`{"verdict":"CHANGES_REQUESTED","findings":[]}`,
		`{"verdict":"CHANGES_REQUESTED","findings":[{"severity":"low","path":"../outside","message":"Invalid path."}]}`,
	} {
		if _, err := reviewResult(input, handoff, []string{"scope"}); err == nil {
			t.Fatalf("invalid review accepted: %s", input)
		}
	}
}

func TestWriterConflictAcrossAliases(t *testing.T) {
	first := model.Job{ID: "first", RepoID: "source", Task: model.Task{Mode: "patch", AllowedPaths: []string{"scope"}}}
	e := Engine{
		cfg: model.Config{Repositories: map[string]model.Repo{
			"source": {Root: "/fixture"}, "alias": {Root: "/fixture"}, "other": {Root: "/another"},
		}},
		active: map[string]context.CancelFunc{first.ID: func() {}},
		jobs:   map[string]model.Job{first.ID: first},
	}
	task := model.Task{Mode: "patch", AllowedPaths: []string{"scope/file.go"}}
	if !e.writerConflict(model.Job{RepoID: "alias", Task: task}) || e.writerConflict(model.Job{RepoID: "other", Task: task}) {
		t.Fatal("writer overlap should be keyed to the configured canonical checkout")
	}
	task.AllowedPaths = []string{"unrelated/file.go"}
	if e.writerConflict(model.Job{RepoID: "alias", Task: task}) {
		t.Fatal("disjoint paths in the same checkout were blocked")
	}
}

func TestTrustedReviewRequiresRecordedSuccessfulCheck(t *testing.T) {
	source := model.Job{Task: model.Task{
		ExecutionProfile: model.TrustedLocalProfile,
		Acceptance:       []model.Check{{CheckID: "target"}},
	}, Result: &model.Result{}}
	if got := reviewCheckStatus(source); got != model.Failed {
		t.Fatalf("review admitted without a check: %s", got)
	}
	source.Result.Checks = []model.CheckEvidence{{ID: "target", Status: "PASSED", ExitStatus: 1}}
	if got := reviewCheckStatus(source); got != model.Failed {
		t.Fatalf("review admitted after a nonzero test exit: %s", got)
	}
	source.Result.Checks[0].ExitStatus = 0
	if got := reviewCheckStatus(source); got != "PASSED" {
		t.Fatalf("verified test was not admitted: %s", got)
	}
}

func TestReviewAcceptsOneJSONFenceAndBoundsMalformedRetry(t *testing.T) {
	handoff := model.Handoff{Manifest: model.HandoffManifest{SHA256: "artifact"}, Review: &model.ReviewInput{CheckStatus: "PASSED"}}
	const body = `{"verdict":"CHANGES_REQUESTED","findings":[{"severity":"high","path":"calc.go","message":"150 rounds down."}]}`
	fenced := "```json\n" + body + "\n```"
	result, err := reviewResult(fenced, handoff, []string{"calc.go"})
	if err != nil || result.Verdict != "CHANGES_REQUESTED" || result.ArtifactSHA256 != "artifact" {
		t.Fatalf("verified fenced review rejected: %v %v", result, err)
	}
	if _, err := reviewResult(fenced+"\nextra", handoff, []string{"calc.go"}); err == nil {
		t.Fatal("extra commentary was accepted as structured evidence")
	}
	at := time.Now()
	j := model.Job{Task: model.Task{ExecutionProfile: "review"}, HandoffFrom: []model.HandoffFrom{{Kind: "diff"}},
		Attempts: []model.Attempt{{Code: "INVALID_OUTPUT", FinishedAt: &at, OpenCode: &model.OpenCodeRunEvidence{TerminalVerified: true}}}}
	if !canRetryReview(j) {
		t.Fatal("verified terminal reviewer cannot retry a malformed verdict")
	}
	j.Attempts = append(j.Attempts, j.Attempts[0])
	if canRetryReview(j) {
		t.Fatal("review allowed more than one format retry")
	}
}
