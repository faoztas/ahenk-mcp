// Copyright 2026 Furkan Anıl Öztaş
// SPDX-License-Identifier: Apache-2.0

package orchestration

import (
	"sync/atomic"
	"testing"
	"time"

	"github.com/faoztas/ahenk-mcp/internal/model"
	"github.com/faoztas/ahenk-mcp/internal/providers"
)

func TestQuotaCacheServesMemoryWithinTTLAndRefreshesInBackground(t *testing.T) {
	now := time.Now().UTC()
	remaining := 90.0
	e := &Engine{
		cfg: model.Config{},
		quotas: map[string][]providers.Quota{"codex": {
			{Bucket: "primary", WindowMinutes: 300, Unit: "percent", AccountMatch: "MATCHED", State: "KNOWN", Remaining: &remaining, ObservedAt: now},
		}},
		quotaRefreshedAt: now,
	}
	var calls atomic.Int64
	e.readQuotas = func() {
		calls.Add(1)
		e.mu.Lock()
		e.quotaRefreshedAt = time.Now().UTC()
		e.mu.Unlock()
	}

	first := e.Quota("codex")
	if len(first) != 1 || first[0].Remaining == nil || *first[0].Remaining != 90 {
		t.Fatalf("cached quota snapshot was not served: %+v", first)
	}
	if calls.Load() != 0 || e.quotaRefreshing.Load() {
		t.Fatalf("a CLI refresh started within the TTL: calls=%d", calls.Load())
	}

	e.mu.Lock()
	e.quotaRefreshedAt = now.Add(-quotaRefreshTTL - time.Second)
	e.mu.Unlock()
	second := e.Quota("codex")
	if len(second) != 1 || second[0].Remaining == nil || *second[0].Remaining != 90 {
		t.Fatalf("stale cache was not served without blocking: %+v", second)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && (calls.Load() != 1 || e.quotaRefreshing.Load()) {
		time.Sleep(2 * time.Millisecond)
	}
	if calls.Load() != 1 || e.quotaRefreshing.Load() {
		t.Fatalf("background refresh did not run exactly once: calls=%d refreshing=%v", calls.Load(), e.quotaRefreshing.Load())
	}
	// Additional reads while the refresh is in flight or fresh must not
	// spawn another refresh.
	_ = e.Quota("codex")
	time.Sleep(20 * time.Millisecond)
	if calls.Load() != 1 {
		t.Fatalf("cache triggered more than one refresh: calls=%d", calls.Load())
	}
}

func TestQuotaCacheFirstRefreshIsNeverBlockedByCaller(t *testing.T) {
	now := time.Now().UTC()
	remaining := 80.0
	e := &Engine{
		cfg: model.Config{},
		quotas: map[string][]providers.Quota{"agy": {
			{Family: "gemini", Bucket: "gemini-5h", Unit: "percent", AccountMatch: "MATCHED", State: "KNOWN", Remaining: &remaining, ObservedAt: now},
		}},
		quotaRefreshedAt: now,
	}
	slow := make(chan struct{})
	e.readQuotas = func() { <-slow }
	e.mu.Lock()
	e.quotaRefreshedAt = now.Add(-quotaRefreshTTL - time.Second)
	e.mu.Unlock()
	start := time.Now()
	rows := e.Quota("agy")
	if elapsed := time.Since(start); elapsed > 50*time.Millisecond {
		t.Fatalf("quota read blocked on the background refresh: %v", elapsed)
	}
	if len(rows) != 1 || rows[0].Remaining == nil || *rows[0].Remaining != 80 {
		t.Fatalf("wrong cached rows: %+v", rows)
	}
	close(slow)
}
