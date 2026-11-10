// Copyright 2026 Furkan Anıl Öztaş
// SPDX-License-Identifier: Apache-2.0

package model

import (
	"fmt"
	"slices"
	"time"
)

type CapacityPolicy struct {
	Mode             string  `json:"mode,omitempty"`
	AGYWeeklyReserve float64 `json:"agy_weekly_min_remaining_pct_while_codex_available,omitempty"`
	CodexLow         float64 `json:"codex_low_pct,omitempty"`
	CodexCritical    float64 `json:"codex_critical_pct,omitempty"`
}

func (p CapacityPolicy) Defaults() CapacityPolicy {
	if p.Mode == "" {
		p.Mode = "quality_first"
	}
	if p.AGYWeeklyReserve == 0 {
		p.AGYWeeklyReserve = 15
	}
	if p.CodexLow == 0 {
		p.CodexLow = 20
	}
	if p.CodexCritical == 0 {
		p.CodexCritical = 10
	}
	return p
}

func (p CapacityPolicy) validate() error {
	p = p.Defaults()
	if !slices.Contains([]string{"quality_first", "balanced", "conserve"}, p.Mode) ||
		p.AGYWeeklyReserve < 1 || p.AGYWeeklyReserve >= 100 ||
		p.CodexCritical < 1 || p.CodexLow > 100 || p.CodexCritical >= p.CodexLow {
		return fmt.Errorf("invalid capacity policy")
	}
	return nil
}

// QualityEvidence describes a demonstrated role and the strength of its
// verification. An inference check never grants implementation or review.
type QualityEvidence struct {
	EvidenceSHA256 string `json:"evidence_sha256,omitempty"`
	Role           string `json:"role"`
	Tier           int    `json:"tier"`
	Source         string `json:"source"`
}

type CapacityWarning struct {
	RetryAt       *time.Time `json:"retry_at,omitempty"`
	Cause         string     `json:"cause,omitempty"`
	AGYState      string     `json:"agy_state,omitempty"`
	OpenCodeState string     `json:"opencode_state,omitempty"`
	AssessedAt    time.Time  `json:"assessed_at"`
	RetrySource   string     `json:"retry_source,omitempty"`
	ID            string     `json:"id"`
	Code          string     `json:"code"`
	Severity      string     `json:"severity"`
	Provider      string     `json:"provider,omitempty"`
	Model         string     `json:"model,omitempty"`
	Bucket        string     `json:"bucket,omitempty"`
	Scope         string     `json:"scope,omitempty"`
	ObservedAt    *time.Time `json:"observed_at,omitempty"`
	Remaining     *float64   `json:"remaining_percent"`
	ResetAt       *time.Time `json:"reset_at,omitempty"`
	Threshold     float64    `json:"threshold_percent,omitempty"`
	Consequence   string     `json:"consequence"`
	Alternative   string     `json:"alternative,omitempty"`
	Message       string     `json:"message"`
	NextAction    string     `json:"next_action"`
}
