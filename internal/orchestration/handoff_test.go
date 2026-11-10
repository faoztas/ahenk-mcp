// Copyright 2026 Furkan Anıl Öztaş
// SPDX-License-Identifier: Apache-2.0

package orchestration

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/faoztas/ahenk-mcp/internal/model"
)

func TestHandoffRejectsChangedContentAndPinsRetries(t *testing.T) {
	dir := t.TempDir()
	e := Engine{artifacts: dir, jobs: map[string]model.Job{}}
	contents := []byte("diff fixture")
	id := "source"
	file := filepath.Join(dir, id+".diff")
	if err := os.WriteFile(file, contents, 0600); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	task := model.Task{DataClass: "synthetic", AllowedPaths: []string{"fixture.txt"}}
	e.jobs[id] = model.Job{ID: id, Key: "first", Parent: "parent", Status: model.Completed, SnapshotID: "snapshot", Task: task,
		Attempts: []model.Attempt{{Number: 1, FinishedAt: &now}}, Result: &model.Result{
			Summary: "summary fixture", SummarySHA256: model.Hash([]byte("summary fixture")),
			ArtifactID: id, ArtifactSHA256: model.Hash(contents), Verification: []string{"PATCH_APPLIES_TO_SNAPSHOT"},
			Preimages: map[string]string{"fixture.txt": "preimage"},
		}}
	j := model.Job{Parent: "parent", SnapshotID: "snapshot", Task: task, DependsOn: []string{id},
		HandoffFrom: []model.HandoffFrom{{Key: "first", Kind: "diff"}, {Key: "first", Kind: "summary"}}}
	verified, err := e.resolveHandoffs(j)
	if err != nil || len(verified) != 2 || verified[1].Content != "summary fixture" {
		t.Fatalf("handoff should resolve both verified payloads: %v %v", verified, err)
	}
	j.Attempts = []model.Attempt{{Handoffs: handoffManifests(verified)}}
	if _, err := e.resolveHandoffs(j); err != nil {
		t.Fatalf("unchanged retry was blocked: %v", err)
	}
	if err := os.WriteFile(file, []byte("altered"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := e.resolveHandoffs(j); err == nil {
		t.Fatal("altered artifact was accepted")
	}
	e.jobs[id].Result.ArtifactSHA256 = model.Hash([]byte("altered"))
	if _, err := e.resolveHandoffs(j); err == nil {
		t.Fatal("retry accepted a different manifest")
	}
}

func TestDistinctProviderCountsAGYAdaptersAsOne(t *testing.T) {
	e := Engine{jobs: map[string]model.Job{
		"parent": {ID: "parent", Distinct: true, Children: []string{"first", "second"}},
		"first":  {ID: "first", Parent: "parent", Attempts: []model.Attempt{{Provider: "agy-headless"}}},
		"second": {ID: "second", Parent: "parent"},
	}}
	for _, alias := range []string{"agy", "agy-acp", "daniiislo"} {
		if !e.distinctTaken(e.jobs["second"], alias) {
			t.Fatalf("same provider counted independently: %s", alias)
		}
	}
	if e.distinctTaken(e.jobs["second"], "opencode") {
		t.Fatal("OpenCode should remain independent")
	}
}

func TestAutomaticReviewBindsArtifactAndRejectsImplementerFamily(t *testing.T) {
	parent, repo := testPlanningParent()
	children, err := buildPlan(parent, repo, testPlan, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	writer, reviewer := children[1], children[2]
	parent.Children = []string{children[0].ID, writer.ID, reviewer.ID}
	writer.SnapshotID, reviewer.SnapshotID = "snapshot", "snapshot"
	writer.Status = model.Completed
	now := time.Now()
	writer.Attempts = []model.Attempt{{Number: 1, Provider: "agy", FinishedAt: &now}}
	diff := []byte("synthetic scoped diff")
	writer.Result = &model.Result{ArtifactID: writer.ID, ArtifactSHA256: model.Hash(diff),
		Verification: []string{"PATCH_APPLIES_TO_SNAPSHOT"}, Preimages: map[string]string{"calc.go": "preimage"},
		Checks: []model.CheckEvidence{{ID: "test", Status: "PASSED", ExitStatus: 0}}}
	e := Engine{artifacts: t.TempDir(), jobs: map[string]model.Job{parent.ID: parent, writer.ID: writer, reviewer.ID: reviewer}}
	if err := e.saveArtifact(writer.ID, diff); err != nil {
		t.Fatal(err)
	}
	if !e.distinctTaken(reviewer, "agy-headless") || e.distinctTaken(reviewer, "opencode") {
		t.Fatal("incorrect automatic review provider independence")
	}
	handoffs, err := e.resolveHandoffs(reviewer)
	if err != nil || len(handoffs) != 1 || handoffs[0].Review == nil ||
		handoffs[0].Manifest.SHA256 != model.Hash(diff) || handoffs[0].Review.CheckStatus != "PASSED" {
		t.Fatalf("automatic review did not bind verified evidence: %+v %v", handoffs, err)
	}
	writer.Result.Checks[0].ExitStatus = 1
	if _, err := e.resolveHandoffs(reviewer); err == nil {
		t.Fatal("review accepted a failed deterministic check")
	}
}
