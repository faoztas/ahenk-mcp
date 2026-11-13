// Copyright 2026 Furkan Anıl Öztaş
// SPDX-License-Identifier: Apache-2.0

package execution

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/faoztas/ahenk-mcp/internal/model"
)

func TestPrivateSourceCaptureHEADOnly(t *testing.T) {
	ctx := context.Background()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) {
		t.Helper()
		if _, err := git(ctx, dir, nil, args...); err != nil {
			t.Fatal(err)
		}
	}
	write := func(path, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, path), []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	commit := func() {
		run("-c", "user.name=Fixture", "-c", "user.email=fixture@example.test", "-c", "commit.gpgsign=false", "commit", "-qm", "Synthetic fixture")
	}
	run("init", "-q")
	const approved = "package example\n"
	write("approved.go", approved)
	write("other.go", "package example // excluded\n")
	run("add", "--", "approved.go", "other.go")
	commit()
	write("approved.go", "package example // staged but never read\n")
	run("add", "--", "approved.go")
	write("approved.go", "package example // unstaged but never read\n")

	policy := &model.PrivateSourcePolicy{ExpiresAt: time.Now().Add(time.Hour),
		Files: map[string]string{"approved.go": model.Hash([]byte(approved))}}
	repo := model.Repo{Root: dir, PrivateSource: policy}
	task := model.Task{DataClass: model.PrivateSourceData, WorkspaceMode: "head_only", AllowedPaths: []string{"approved.go"}}
	snapshot, err := Capture(ctx, repo, task)
	if err != nil {
		t.Fatal(err)
	}
	files, err := snapshot.Context(task.AllowedPaths)
	if err != nil || len(files) != 1 || files["approved.go"] != approved {
		t.Fatalf("captured something other than approved HEAD bytes: %v", err)
	}
	commit()
	if _, err := Capture(ctx, repo, task); err == nil {
		t.Fatal("accepted changed HEAD content")
	} else if f, ok := err.(*model.Fault); !ok || f.Gate != "DIGEST_MISMATCH" || f.NextAction == "" {
		t.Fatalf("expected DIGEST_MISMATCH gate fault, got %v", err)
	}

	const withPersonal = "package example // contact owner@yarinlik.com\n"
	write("approved.go", withPersonal)
	run("add", "--", "approved.go")
	commit()
	policy.Files["approved.go"] = model.Hash([]byte(withPersonal))
	if _, err := Capture(ctx, repo, task); err == nil {
		t.Fatal("accepted personal data in HEAD")
	} else if f, ok := err.(*model.Fault); !ok || f.Code != "POLICY_DENIED" || f.Gate != "PERSONAL_DATA_CLASSIFIED" {
		t.Fatalf("expected PERSONAL_DATA_CLASSIFIED gate fault, got %v", err)
	}
}
