package orchestration

import (
	"strings"
	"testing"
	"time"

	"github.com/faoztas/ahenk-mcp/internal/model"
)

func TestDualCloudOutageAndFreeTierBypass(t *testing.T) {
	cb := NewCircuitBreaker()
	// Simulate dual cloud outage
	cb.Trip("codex", 1*time.Hour)
	cb.Trip("antigravity", 1*time.Hour)

	stage := DetermineOperationalStage(0.0, 0.0, 1.0)
	if stage != Stage5DualCloudOutage {
		t.Fatalf("expected Stage5DualCloudOutage, got %s", stage)
	}

	// Coding candidates must fall through to OpenCode Free
	matrix := FallbackMatrix("coding")
	var selected string
	for _, m := range matrix {
		prefix := strings.Split(m, "/")[0]
		if cb.Allow(prefix) {
			selected = m
			break
		}
	}
	if selected != "opencode/crouton-alpha-free" {
		t.Fatalf("expected crouton-alpha-free during dual cloud outage, got %s", selected)
	}

	// Now simulate OpenCode 429 limit hit
	cb.Trip("opencode", 16600*time.Millisecond) // 16.6h represented as ms in test
	if cb.Allow("opencode") {
		t.Fatalf("opencode should be blocked by circuit breaker")
	}
}

func TestOpenCodeRateLimitUpdatesCircuitState(t *testing.T) {
	// 1. Per-model 429 during HALF_OPEN probe must release the probe lock and NOT trip provider-wide circuit
	t.Run("per_model_rate_limit_releases_probe_and_stays_model_scoped", func(t *testing.T) {
		now := time.Now().UTC()
		cb := NewCircuitBreaker()
		cb.Trip("opencode", 10*time.Millisecond)
		time.Sleep(15 * time.Millisecond)
		probeID, allowed := cb.Acquire("opencode")
		if !allowed {
			t.Fatalf("expected HALF_OPEN probe to be admitted")
		}
		if !cb.IsProbing("opencode") {
			t.Fatalf("expected opencode to be probing")
		}

		e := &Engine{
			cb:       cb,
			cooldown: make(map[string]time.Time),
			jobs:     map[string]model.Job{},
		}
		attempt := model.Attempt{
			ProbeID:                probeID,
			Number:                 1,
			Provider:               "opencode",
			Model:                  "opencode/free-a",
			Code:                   "RATE_LIMIT",
			ProviderRateLimitScope: "model",
			FinishedAt:             &now,
			RetryAfterMS:           60_000,
		}
		e.rememberOpenCodeHealth(&attempt)

		if cb.IsProbing("opencode") {
			t.Fatalf("per-model 429 left a dangling probe lock on opencode")
		}
		if st := cb.State("opencode"); st == CircuitOpen {
			t.Fatalf("per-model 429 must NOT trip provider-wide circuit breaker, got %s", st)
		}
	})

	// 2. Pool / account / provider-scoped 429 must trip e.cb via TripUntil and clear any probing lock
	for _, scope := range []string{"pool", "account", "provider"} {
		t.Run("shared_scope_"+scope+"_trips_circuit_breaker", func(t *testing.T) {
			finishedAt := time.Now().UTC().Add(-5 * time.Second)
			expectedUntil := finishedAt.Add(120 * time.Second)
			cb := NewCircuitBreaker()
			cb.Trip("opencode", 10*time.Millisecond)
			time.Sleep(15 * time.Millisecond)
			probeID, allowed := cb.Acquire("opencode")
			if !allowed {
				t.Fatalf("expected HALF_OPEN probe to be admitted")
			}

			e := &Engine{
				cb:       cb,
				cooldown: make(map[string]time.Time),
				jobs:     map[string]model.Job{},
			}
			attempt := model.Attempt{
				ProbeID:                probeID,
				Number:                 1,
				Provider:               "opencode",
				Model:                  "opencode/free-a",
				Code:                   "RATE_LIMIT",
				ProviderRateLimitScope: scope,
				FinishedAt:             &finishedAt,
				RetryAfterMS:           120_000,
			}
			e.rememberOpenCodeHealth(&attempt)

			if cb.IsProbing("opencode") {
				t.Fatalf("shared %s 429 left a dangling probe lock", scope)
			}
			if st := cb.State("opencode"); st != CircuitOpen {
				t.Fatalf("expected opencode circuit to be OPEN for scope %q, got %s", scope, st)
			}
			tripped := cb.ActiveTripped()
			gotUntil, ok := tripped["opencode"]
			if !ok {
				t.Fatalf("expected opencode in ActiveTripped for scope %q", scope)
			}
			if diff := gotUntil.Sub(expectedUntil); diff < -50*time.Millisecond || diff > 50*time.Millisecond {
				t.Fatalf("expected opencode cooldown until %v, got %v", expectedUntil, gotUntil)
			}
		})
	}

	// 3. Expired historical pool/provider 429 replayed via rememberOpenCodeHealth must NOT trip e.cb
	t.Run("expired_historical_shared_rate_limit_is_not_retripped", func(t *testing.T) {
		expiredAt := time.Now().UTC().Add(-10 * time.Minute)
		cb := NewCircuitBreaker()
		e := &Engine{
			cb:       cb,
			cooldown: make(map[string]time.Time),
			jobs:     map[string]model.Job{},
		}
		attempt := model.Attempt{
			Number:                 1,
			Provider:               "opencode",
			Model:                  "opencode/free-a",
			Code:                   "RATE_LIMIT",
			ProviderRateLimitScope: "provider",
			FinishedAt:             &expiredAt,
			RetryAfterMS:           60_000,
		}
		e.rememberOpenCodeHealth(&attempt)
		if st := cb.State("opencode"); st != CircuitClosed {
			t.Fatalf("expired historical opencode 429 re-tripped circuit breaker: got %s", st)
		}
		if len(cb.ActiveTripped()) != 0 {
			t.Fatalf("expected ActiveTripped to be empty, got %v", cb.ActiveTripped())
		}
	})
}
