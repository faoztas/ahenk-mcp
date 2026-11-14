package orchestration

import (
	"strings"
	"testing"
	"time"
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
