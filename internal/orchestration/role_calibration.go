// Copyright 2026 Furkan Anıl Öztaş
// SPDX-License-Identifier: Apache-2.0

package orchestration

import (
	"encoding/json/v2"
	"slices"
	"time"

	"github.com/faoztas/ahenk-mcp/internal/model"
)

const roleFixture = "bounded-role-suite-v1"

var calibratedRoles = []string{"architecture", "planning", "reasoning", "research"}

const rolePrompt = `Solve four synthetic exercises using only these facts. Return one JSON object with exactly these keys and fields. If a summary wrapper is required, put the JSON as the summary string.
architecture: select "transaction" or "queue" as pattern. A ledger must atomically update both rows and acknowledge only after durability. Both rows are in one database. The queue alternative provides eventual delivery without atomic updates. Set "preserves" to the guarantee from "atomicity" or "throughput" that determines the choice, and "tradeoff" to "shared_database" or "independent_scaling".
planning: give a topological array of the four unique steps "snapshot", "edit", "test", "review". Edit requires snapshot. Test requires edit. Review requires test and snapshot.
reasoning: give "accepted", "reserved", and "remaining" as integers. Of 18 items, reject 7. Reserve twice the number rejected against a capacity of 19. Remaining is capacity minus reserved.
research: sources S1 and S2 discuss the same plant. S1, issued 2024, reports 20 units at a dry site. S2, issued 2025, reports 12 units at a wet site. Return "latest" as source ID, "quantity" as its integer result, and "condition" as "wet" or "dry". Do not combine different site conditions.`

func verifyRoleCalibration(summary string) (*model.RoleCalibrationEvidence, error) {
	var answer struct {
		Architecture struct {
			Pattern   string `json:"pattern"`
			Preserves string `json:"preserves"`
			Tradeoff  string `json:"tradeoff"`
		} `json:"architecture"`
		Planning  []string `json:"planning"`
		Reasoning struct {
			Accepted  int `json:"accepted"`
			Reserved  int `json:"reserved"`
			Remaining int `json:"remaining"`
		} `json:"reasoning"`
		Research struct {
			Latest    string `json:"latest"`
			Quantity  int    `json:"quantity"`
			Condition string `json:"condition"`
		} `json:"research"`
	}
	if len(summary) > 2048 || model.ContainsSecret([]byte(summary)) ||
		json.Unmarshal([]byte(summary), &answer, json.RejectUnknownMembers(true)) != nil ||
		answer.Architecture.Pattern != "transaction" || answer.Architecture.Preserves != "atomicity" ||
		answer.Architecture.Tradeoff != "shared_database" ||
		!slices.Equal(answer.Planning, []string{"snapshot", "edit", "test", "review"}) ||
		answer.Reasoning.Accepted != 11 || answer.Reasoning.Reserved != 14 || answer.Reasoning.Remaining != 5 ||
		answer.Research.Latest != "S2" || answer.Research.Quantity != 12 || answer.Research.Condition != "wet" {
		return nil, model.Error("ROLE_CALIBRATION_FAILED", "the bounded role fixture did not pass every deterministic check")
	}
	return &model.RoleCalibrationEvidence{Fixture: roleFixture, Roles: slices.Clone(calibratedRoles), ResponseSHA256: model.Hash([]byte(summary))}, nil
}

func (e *Engine) routeQuality(c model.Capability, role string, now time.Time) model.QualityEvidence {
	base := qualityFor(c, role)
	if base.Tier > 0 || !slices.Contains(calibratedRoles, role) || c.Fingerprint == "" {
		return base
	}
	for _, j := range e.jobs {
		if !j.Task.RoleCalibration || j.Status != model.Completed || j.Result == nil || j.Result.RoleCalibration == nil ||
			j.Result.RoleCalibration.Fixture != roleFixture || !slices.Contains(j.Result.RoleCalibration.Roles, role) {
			continue
		}
		for _, a := range j.Attempts {
			if a.Provider != c.Provider || a.Model != c.Model || a.Fingerprint != c.Fingerprint || a.Code != "" ||
				a.FinishedAt == nil || a.FinishedAt.After(now) {
				continue
			}
			if a.Provider == "opencode" && openCodeRoute(a.Model, a.OpenCodeAccount, a.OpenCodeTransport, a.OpenCodeEndpoint) !=
				openCodeRoute(c.Model, c.OpenCodeAccount, c.OpenCodeTransport, c.OpenCodeEndpoint) {
				continue
			}
			return model.QualityEvidence{Role: role, Tier: 1, Source: "LIVE_DETERMINISTIC_ROLE_CALIBRATION", EvidenceSHA256: j.Result.RoleCalibration.ResponseSHA256}
		}
	}
	return base
}

func (e *Engine) RoleQualities(c model.Capability) []model.QualityEvidence {
	e.mu.Lock()
	defer e.mu.Unlock()
	rows := slices.Clone(c.Quality)
	now := time.Now().UTC()
	for _, role := range calibratedRoles {
		if q := e.routeQuality(c, role, now); q.Tier > 0 &&
			!slices.ContainsFunc(rows, func(x model.QualityEvidence) bool { return x.Role == role }) {
			rows = append(rows, q)
		}
	}
	return rows
}
