package orchestration

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/faoztas/ahenk-mcp/internal/model"
)

func TestCircuitBreakerLifecycle(t *testing.T) {
	cb := NewCircuitBreaker()
	if st := cb.State("codex"); st != CircuitClosed {
		t.Fatalf("expected initial state CLOSED, got %s", st)
	}
	if !cb.Allow("codex") {
		t.Fatalf("expected CLOSED breaker to allow requests")
	}

	// Trip with 50ms cooldown
	cb.Trip("codex", 50*time.Millisecond)
	if st := cb.State("codex"); st != CircuitOpen {
		t.Fatalf("expected state OPEN, got %s", st)
	}
	if cb.Allow("codex") {
		t.Fatalf("expected OPEN breaker to block requests")
	}

	// Wait for cooldown expiration -> HALF_OPEN
	time.Sleep(60 * time.Millisecond)
	if st := cb.State("codex"); st != CircuitHalfOpen {
		t.Fatalf("expected state HALF_OPEN after cooldown, got %s", st)
	}
	if !cb.Allow("codex") {
		t.Fatalf("expected HALF_OPEN to admit single probe")
	}

	// Record success -> CLOSED
	cb.Success("codex")
	if st := cb.State("codex"); st != CircuitClosed {
		t.Fatalf("expected state CLOSED after success, got %s", st)
	}
}

func TestCircuitBreakerProbeRelease(t *testing.T) {
	cb := NewCircuitBreaker()
	cb.Trip("codex", 10*time.Millisecond)
	time.Sleep(15 * time.Millisecond)

	// In HALF_OPEN state, first Allow admits probe
	if !cb.Allow("codex") {
		t.Fatalf("expected first probe to be admitted")
	}
	// Second Allow should be blocked while probing
	if cb.Allow("codex") {
		t.Fatalf("expected second probe to be blocked while probing is active")
	}

	// Release probe
	cb.Release("codex")

	// After release, a new probe should be admitted immediately
	if !cb.Allow("codex") {
		t.Fatalf("expected probe to be admitted after Release")
	}
}

func TestCircuitBreakerEligibleReadOnly(t *testing.T) {
	cb := NewCircuitBreaker()
	if !cb.Eligible("codex") {
		t.Fatalf("expected CLOSED breaker to be eligible")
	}

	cb.Trip("codex", 20*time.Millisecond)
	if cb.Eligible("codex") {
		t.Fatalf("expected OPEN breaker to NOT be eligible")
	}

	time.Sleep(25 * time.Millisecond)
	// In HALF_OPEN: Eligible should return true WITHOUT consuming the probe!
	if !cb.Eligible("codex") {
		t.Fatalf("expected HALF_OPEN breaker to be eligible")
	}
	if !cb.Eligible("codex") {
		t.Fatalf("subsequent Eligible call should still be true (read-only, does not consume probe)")
	}
	if st := cb.State("codex"); st != CircuitHalfOpen {
		t.Fatalf("expected state to remain HALF_OPEN, got %s", st)
	}
	if cb.IsProbing("codex") {
		t.Fatalf("expected probing to be false after Eligible checks")
	}

	// Now Allow consumes the probe
	if !cb.Allow("codex") {
		t.Fatalf("expected Allow to succeed after Eligible checks")
	}
	if !cb.IsProbing("codex") {
		t.Fatalf("expected probing to be true after Allow consumes probe")
	}
	// While probing is active, another caller's Eligible should be false
	if cb.Eligible("codex") {
		t.Fatalf("expected in-flight probing to NOT be eligible for other callers")
	}
}

func TestCircuitBreakerProbeExpiryTTL(t *testing.T) {
	cb := NewCircuitBreaker()
	cb.probeTimeout = 20 * time.Millisecond
	cb.Trip("codex", 10*time.Millisecond)
	time.Sleep(15 * time.Millisecond)

	// First probe admitted
	if !cb.Allow("codex") {
		t.Fatalf("expected first probe to be admitted")
	}
	// Immediate second probe blocked
	if cb.Allow("codex") {
		t.Fatalf("expected probe to be blocked")
	}

	// Wait for probe timeout
	time.Sleep(25 * time.Millisecond)

	// Probe expired, should admit a new probe
	if !cb.Allow("codex") {
		t.Fatalf("expected new probe to be admitted after probe timeout expiration")
	}
}

func TestCircuitBreakerActiveTrippedVisibility(t *testing.T) {
	cb := NewCircuitBreaker()
	cb.Trip("codex", 100*time.Millisecond)
	cb.Trip("agy", 10*time.Millisecond)

	tripped := cb.ActiveTripped()
	if _, ok := tripped["codex"]; !ok {
		t.Fatalf("expected codex to be in ActiveTripped during cooldown")
	}
	if _, ok := tripped["agy"]; !ok {
		t.Fatalf("expected agy to be in ActiveTripped during cooldown")
	}

	// Wait for agy cooldown to expire
	time.Sleep(15 * time.Millisecond)

	// Before Allow, agy cooldown passed, not probing yet
	tripped = cb.ActiveTripped()
	if _, ok := tripped["agy"]; ok {
		t.Fatalf("expected agy to NOT be in ActiveTripped before probe admits")
	}

	// Admit probe for agy
	if !cb.Allow("agy") {
		t.Fatalf("expected agy probe to be admitted")
	}

	// While agy is probing, it MUST be visible in ActiveTripped!
	tripped = cb.ActiveTripped()
	exp, ok := tripped["agy"]
	if !ok {
		t.Fatalf("expected probing provider agy to be visible in ActiveTripped")
	}
	if !exp.After(time.Now()) {
		t.Fatalf("expected probing expiry to be in the future, got %v", exp)
	}

	// Success removes agy from ActiveTripped
	cb.Success("agy")
	tripped = cb.ActiveTripped()
	if _, ok := tripped["agy"]; ok {
		t.Fatalf("expected agy to be removed after Success")
	}
}

func TestEngineCircuitBreakerProbeReleasedOnRouteEliminationAndFinish(t *testing.T) {
	cb := NewCircuitBreaker()
	cb.Trip("codex", 10*time.Millisecond)
	time.Sleep(15 * time.Millisecond)

	// In HALF_OPEN state, codex should be eligible and allow one probe
	if cb.State("codex") != CircuitHalfOpen {
		t.Fatalf("expected codex to be HALF_OPEN")
	}

	eng := &Engine{
		cb:       cb,
		cooldown: make(map[string]time.Time),
	}

	// 1. In finish(), if an attempt fails with non-rate-limit error, probe lock is released
	if !cb.Allow("codex") {
		t.Fatalf("expected probe to be admitted")
	}
	if !cb.IsProbing("codex") {
		t.Fatalf("expected codex to be probing")
	}

	j := model.Job{
		ID: "job-fail-test",
		Attempts: []model.Attempt{
			{Number: 1, Provider: "codex"},
		},
	}
	eng.finish(context.Background(), j, errors.New("SYNTAX_ERROR"))

	if cb.IsProbing("codex") {
		t.Fatalf("expected codex probe to be released after finish() with non-rate-limit error")
	}
}

