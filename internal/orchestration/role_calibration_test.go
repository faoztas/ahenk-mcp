// Copyright 2026 Furkan Anıl Öztaş
// SPDX-License-Identifier: Apache-2.0

package orchestration

import (
	"strings"
	"testing"
	"time"

	"github.com/faoztas/ahenk-mcp/internal/model"
)

func TestRoleCalibrationRequiresEveryDeterministicCheck(t *testing.T) {
	const correct = `{"architecture":{"pattern":"transaction","preserves":"atomicity","tradeoff":"shared_database"},"planning":["snapshot","edit","test","review"],"reasoning":{"accepted":11,"reserved":14,"remaining":5},"research":{"latest":"S2","quantity":12,"condition":"wet"}}`
	proof, err := verifyRoleCalibration(correct)
	if err != nil || len(proof.Roles) != 4 || proof.ResponseSHA256 != model.Hash([]byte(correct)) {
		t.Fatalf("%+v %v", proof, err)
	}
	for _, pair := range [][2]string{{"transaction", "queue"}, {`"edit","test"`, `"test","edit"`}, {`"remaining":5`, `"remaining":19`}, {`"condition":"wet"`, `"condition":"dry"`}} {
		if _, err := verifyRoleCalibration(strings.Replace(correct, pair[0], pair[1], 1)); err == nil {
			t.Fatal("incorrect role accepted")
		}
	}
	now := time.Now()
	c := glmRoute()
	e := Engine{jobs: map[string]model.Job{"fixture": {Task: model.Task{RoleCalibration: true}, Status: model.Completed,
		Result: &model.Result{RoleCalibration: proof}, Attempts: []model.Attempt{glmSuccess(now)}}}}
	for _, role := range calibratedRoles {
		if q := e.routeQuality(c, role, now); q.Tier != 1 {
			t.Fatal("verified role absent", role)
		}
	}
	c.OpenCodeEndpoint = "https://different.invalid"
	if q := e.routeQuality(c, "planning", now); q.Tier != 0 {
		t.Fatal("role transferred to another endpoint")
	}
}
