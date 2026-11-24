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
		return strings.ToLower(strings.TrimSpace(provider))
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
	probing      map[string]uint64
	nextProbeID  uint64
	probeExpiry  map[string]time.Time
	probeTimeout time.Duration
}

// NewCircuitBreaker creates a new thread-safe CircuitBreaker instance.
func NewCircuitBreaker() *CircuitBreaker {
	return &CircuitBreaker{
		cooldown:     make(map[string]time.Time),
		probing:      make(map[string]uint64),
		probeExpiry:  make(map[string]time.Time),
		probeTimeout: defaultProbeTimeout,
	}
}

// Trip opens the circuit for the specified provider for duration dur.
func (cb *CircuitBreaker) Trip(provider string, dur time.Duration) {
	if cb == nil {
		return
	}
	cb.mu.Lock()
	defer cb.mu.Unlock()
	p := normalizeProvider(provider)
	cb.cooldown[p] = time.Now().Add(dur)
	delete(cb.probing, p)
	delete(cb.probeExpiry, p)
}

// TripUntil opens the circuit for provider until the given deadline.
// It is a no-op if cb is nil or until is not after the current time.
func (cb *CircuitBreaker) TripUntil(provider string, until time.Time) {
	cb.tripUntil(provider, until, 0, false)
}

func (cb *CircuitBreaker) tripUntil(provider string, until time.Time, probeID uint64, fromAttempt bool) bool {
	if cb == nil || !until.After(time.Now()) {
		return false
	}
	cb.mu.Lock()
	defer cb.mu.Unlock()
	p := normalizeProvider(provider)
	if fromAttempt && cb.probing[p] != probeID {
		return false
	}
	if existing, ok := cb.cooldown[p]; !ok || until.After(existing) {
		cb.cooldown[p] = until
	}
	delete(cb.probing, p)
	delete(cb.probeExpiry, p)
	return true
}

// State returns the current CircuitState for provider.
func (cb *CircuitBreaker) State(provider string) CircuitState {
	return cb.stateAt(provider, time.Now())
}

func (cb *CircuitBreaker) stateAt(provider string, now time.Time) CircuitState {
	if cb == nil {
		return CircuitClosed
	}
	cb.mu.RLock()
	defer cb.mu.RUnlock()
	p := normalizeProvider(provider)
	until, ok := cb.cooldown[p]
	if !ok {
		return CircuitClosed
	}

	if now.Before(until) {
		return CircuitOpen
	}
	if cb.probing[p] != 0 {
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
	return cb.probing[p] != 0 && time.Now().Before(cb.probeExpiry[p])
}

// Allow determines if a request to provider should be permitted.
// In CLOSED state, all requests are allowed.
// In OPEN state, requests are blocked.
// In HALF_OPEN state (after cooldown expiration), a single probe is admitted.
func (cb *CircuitBreaker) Allow(provider string) bool {
	_, allowed := cb.Acquire(provider)
	return allowed
}

// Acquire returns the ownership ID for an admitted half-open probe.
// A closed circuit admits ordinary work with ID zero.
func (cb *CircuitBreaker) Acquire(provider string) (uint64, bool) {
	return cb.acquireAt(provider, time.Now())
}

func (cb *CircuitBreaker) acquireAt(provider string, now time.Time) (uint64, bool) {
	if cb == nil {
		return 0, true
	}
	cb.mu.Lock()
	defer cb.mu.Unlock()
	p := normalizeProvider(provider)
	until, ok := cb.cooldown[p]

	if !ok {
		return 0, true
	}
	if now.Before(until) {
		return 0, false
	}
	// In HALF_OPEN: admit a single probe if not currently probing or if previous probe expired.
	isProbing := cb.probing[p] != 0 && now.Before(cb.probeExpiry[p])
	if !isProbing {
		cb.nextProbeID++
		cb.probing[p] = cb.nextProbeID
		timeout := cb.probeTimeout
		if timeout <= 0 {
			timeout = defaultProbeTimeout
		}
		cb.probeExpiry[p] = now.Add(timeout)
		return cb.nextProbeID, true
	}
	return 0, false
}

// Release makes a half-open circuit eligible for its next probe.
func (cb *CircuitBreaker) Release(provider string, probeID uint64) {
	cb.complete(provider, probeID, false)
}

// Success records explicit provider recovery when no probe owns the circuit.
func (cb *CircuitBreaker) Success(provider string) {
	cb.complete(provider, 0, true)
}

func (cb *CircuitBreaker) probeSucceeded(provider string, probeID uint64) {
	if probeID == 0 {
		return
	}
	cb.complete(provider, probeID, true)
}

func (cb *CircuitBreaker) complete(provider string, probeID uint64, success bool) {
	if cb == nil {
		return
	}
	cb.mu.Lock()
	defer cb.mu.Unlock()
	p := normalizeProvider(provider)
	if cb.probing[p] != probeID {
		return
	}
	if success {
		delete(cb.cooldown, p)
	}
	delete(cb.probing, p)
	delete(cb.probeExpiry, p)
}

// ActiveTripped returns all providers currently in OPEN or active probing state along with their deadline.
func (cb *CircuitBreaker) ActiveTripped() map[string]time.Time {
	if cb == nil {
		return nil
	}
	cb.mu.RLock()
	defer cb.mu.RUnlock()
	now := time.Now()
	res := make(map[string]time.Time)
	for p, until := range cb.cooldown {
		if now.Before(until) {
			res[p] = until
		} else if cb.probing[p] != 0 {
			if exp, ok := cb.probeExpiry[p]; ok && now.Before(exp) {
				res[p] = exp
			}
		}
	}
	return res
}
