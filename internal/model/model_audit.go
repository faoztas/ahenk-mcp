// Copyright 2026 Furkan Anıl Öztaş
// SPDX-License-Identifier: Apache-2.0

package model

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"syscall"
	"time"
)

const ModelAuditFixture = "go-integer-v1"

type ModelInventory struct {
	Fixture    string       `json:"fixture"`
	ObservedAt time.Time    `json:"observed_at"`
	Models     []ModelAudit `json:"models"`
}

type ModelAudit struct {
	EvidenceID   string      `json:"evidence_sha256,omitempty"`
	RateScope    string      `json:"rate_scope,omitempty"`
	Provider     string      `json:"provider"`
	Model        string      `json:"model"`
	Name         string      `json:"name,omitempty"`
	Family       string      `json:"family,omitempty"`
	Effort       string      `json:"effort,omitempty"`
	Interface    string      `json:"interface"`
	Binary       string      `json:"binary_sha256"`
	Source       string      `json:"source"`
	Cost         string      `json:"cost"`
	QuotaBuckets []string    `json:"quota_buckets,omitempty"`
	QuotaState   string      `json:"quota_state,omitempty"`
	Inference    string      `json:"inference"`
	Coding       string      `json:"coding"`
	Review       string      `json:"review"`
	Actual       string      `json:"actual_model,omitempty"`
	Terminal     bool        `json:"terminal_verified,omitempty"`
	LatencyMS    int64       `json:"latency_ms,omitempty"`
	InputTokens  int64       `json:"input_tokens,omitempty"`
	OutputTokens int64       `json:"output_tokens,omitempty"`
	TotalTokens  int64       `json:"total_tokens,omitempty"`
	RuntimeCost  string      `json:"runtime_cost,omitempty"`
	RetryAt      *time.Time  `json:"retry_at,omitempty"`
	Reason       string      `json:"reason,omitempty"`
	Role         string      `json:"recommended_role,omitempty"`
	Context      int         `json:"context_tokens,omitempty"`
	ToolMetadata bool        `json:"catalog_tool_use,omitempty"`
	Pricing      *AuditPrice `json:"pricing,omitempty"`
}

type AuditPrice struct {
	Input      *float64 `json:"input"`
	Output     *float64 `json:"output"`
	CacheRead  *float64 `json:"cache_read"`
	CacheWrite *float64 `json:"cache_write"`
}

func (m ModelInventory) EvidenceID(row ModelAudit) string {
	return Hash([]byte(strings.Join([]string{m.Fixture, m.ObservedAt.UTC().Format(time.RFC3339Nano), row.Provider, row.Model}, "\n")))
}

func (m ModelInventory) Fresh(now time.Time) bool {
	return m.Fixture == ModelAuditFixture && !m.ObservedAt.IsZero() && !m.ObservedAt.After(now) &&
		now.Sub(m.ObservedAt) < 24*time.Hour
}

func LoadModelInventory(path string) (*ModelInventory, error) {
	const maxSize = 512 << 10
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, fmt.Errorf("model audit inventory is unavailable or unsafe")
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Size() > maxSize {
		return nil, fmt.Errorf("model audit inventory must be a private regular file")
	}
	stat := info.Sys().(*syscall.Stat_t)
	if int(stat.Uid) != os.Geteuid() || stat.Nlink != 1 {
		return nil, fmt.Errorf("model audit inventory must be owned by the current user and have no other links")
	}
	var inventory ModelInventory
	input := &io.LimitedReader{R: f, N: maxSize + 1}
	d := json.NewDecoder(input)
	d.DisallowUnknownFields()
	if d.Decode(&inventory) != nil || d.Decode(new(any)) != io.EOF || input.N == 0 ||
		inventory.Fixture != ModelAuditFixture || len(inventory.Models) > 256 || inventory.ObservedAt.IsZero() {
		return nil, fmt.Errorf("invalid model audit inventory")
	}
	seen := map[[2]string]bool{}
	for _, row := range inventory.Models {
		key := [2]string{row.Provider, row.Model}
		if seen[key] {
			return nil, fmt.Errorf("duplicate model audit row")
		}
		seen[key] = true
	}
	return &inventory, nil
}
