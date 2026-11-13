package orchestration

import (
	"strings"
	"sync"
	"time"
)

type CircuitState string

const (
	CircuitClosed   CircuitState = "CLOSED"
	CircuitOpen     CircuitState = "OPEN"
	CircuitHalfOpen CircuitState = "HALF_OPEN"
)

// NormalizeProvider canonicalizes provider names across aliases and prefixes.
func NormalizeProvider(provider string) string {
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "antigravity", "agy", "agy-headless", "agy-acp", "daniiislo":
		return "agy"
	case "codex", "codex-cli":
		return "codex"
	case "claude", "claude-cli":
		return "claude"
	case "opencode", "opencode-native":
		return "opencode"
	default:
		return provider
	}
}

func normalizeProvider(provider string) string {
	return NormalizeProvider(provider)
}

const defaultProbeTimeout = 30 * time.Second

// CircuitBreaker manages provider health states and zero-latency failover cooldowns.
type CircuitBreaker struct {
	mu           sync.RWMutex
	cooldown     map[string]time.Time
	probing      map[string]bool
	probeExpiry  map[string]time.Time
	probeTimeout time.Duration
}

// NewCircuitBreaker creates a new thread-safe CircuitBreaker instance.
func NewCircuitBreaker() *CircuitBreaker {
	return &CircuitBreaker{
		cooldown:     make(map[string]time.Time),
		probing:      make(map[string]bool),
		probeExpiry:  make(map[string]time.Time),
		probeTimeout: defaultProbeTimeout,
	}
}

// Trip opens the circuit for the specified provider for duration dur.
func (cb *CircuitBreaker) Trip(provider string, dur time.Duration) {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	p := normalizeProvider(provider)
	cb.cooldown[p] = time.Now().Add(dur)
	delete(cb.probing, p)
	delete(cb.probeExpiry, p)
}

// State returns the current CircuitState for provider.
func (cb *CircuitBreaker) State(provider string) CircuitState {
	cb.mu.RLock()
	defer cb.mu.RUnlock()
	p := normalizeProvider(provider)
	until, ok := cb.cooldown[p]
	if !ok {
		return CircuitClosed
	}
	now := time.Now()
	if now.Before(until) {
		return CircuitOpen
	}
	if cb.probing[p] {
		if exp, okExp := cb.probeExpiry[p]; okExp && now.Before(exp) {
			return CircuitOpen
		}
	}
	return CircuitHalfOpen
}

// Eligible reports whether provider is eligible for routing (i.e. not in CircuitOpen state).
// It is read-only and does NOT acquire or consume a half-open probe lock.
func (cb *CircuitBreaker) Eligible(provider string) bool {
	if cb == nil {
		return true
	}
	return cb.State(provider) != CircuitOpen
}

// IsProbing reports whether provider currently has an active probe lock.
func (cb *CircuitBreaker) IsProbing(provider string) bool {
	if cb == nil {
		return false
	}
	cb.mu.RLock()
	defer cb.mu.RUnlock()
	p := normalizeProvider(provider)
	return cb.probing[p] && time.Now().Before(cb.probeExpiry[p])
}

// Allow determines if a request to provider should be permitted.
// In CLOSED state, all requests are allowed.
// In OPEN state, requests are blocked.
// In HALF_OPEN state (after cooldown expiration), a single probe is admitted.
func (cb *CircuitBreaker) Allow(provider string) bool {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	p := normalizeProvider(provider)
	until, ok := cb.cooldown[p]
	now := time.Now()
	if !ok {
		return true
	}
	if now.Before(until) {
		return false
	}
	// In HALF_OPEN: admit a single probe if not currently probing or if previous probe expired.
	isProbing := cb.probing[p] && now.Before(cb.probeExpiry[p])
	if !isProbing {
		cb.probing[p] = true
		timeout := cb.probeTimeout
		if timeout <= 0 {
			timeout = defaultProbeTimeout
		}
		cb.probeExpiry[p] = now.Add(timeout)
		return true
	}
	return false
}

// Release clears the probing lock for provider if it was being probed, allowing immediate retry.
func (cb *CircuitBreaker) Release(provider string) {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	p := normalizeProvider(provider)
	delete(cb.probing, p)
	delete(cb.probeExpiry, p)
}

// Success resets the circuit breaker for provider to CLOSED state.
func (cb *CircuitBreaker) Success(provider string) {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	p := normalizeProvider(provider)
	delete(cb.cooldown, p)
	delete(cb.probing, p)
	delete(cb.probeExpiry, p)
}

// ActiveTripped returns all providers currently in OPEN or active probing state along with their deadline.
func (cb *CircuitBreaker) ActiveTripped() map[string]time.Time {
	cb.mu.RLock()
	defer cb.mu.RUnlock()
	now := time.Now()
	res := make(map[string]time.Time)
	for p, until := range cb.cooldown {
		if now.Before(until) {
			res[p] = until
		} else if cb.probing[p] {
			if exp, ok := cb.probeExpiry[p]; ok && now.Before(exp) {
				res[p] = exp
			}
		}
	}
	return res
}
