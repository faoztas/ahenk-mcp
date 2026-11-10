// Copyright 2026 Furkan Anıl Öztaş
// SPDX-License-Identifier: Apache-2.0

package model

import "testing"

func TestTrustedLocalRequiresExplicitRepositoryAndRegisteredCheck(t *testing.T) {
	task := Task{ExecutionProfile: TrustedLocalProfile, Objective: "Edit the synthetic fixture", Mode: "patch",
		DataClass: "synthetic", AllowedPaths: []string{"calc.go"},
		Acceptance: []Check{{ID: "test", Description: "Run the registered test", CheckID: "target"}}}
	repo := Repo{AllowedPaths: []string{"calc.go"}}
	if err := task.Validate(repo); err == nil {
		t.Fatal("trusted-local accepted without repository opt-in")
	}
	repo.TrustedLocal = &TrustedLocalPolicy{WritePaths: []string{"calc.go"}, GoChecks: map[string]string{"target": "TestCalc"}}
	if err := task.Validate(repo); err != nil {
		t.Fatal(err)
	}
	task.Acceptance[0].CheckID = "other"
	if err := task.Validate(repo); err == nil {
		t.Fatal("trusted-local accepted unregistered check")
	}
}
