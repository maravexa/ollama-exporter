package remotewrite

import (
	"sync"
	"time"
)

// Breaker state values, exposed as a gauge.
const (
	BreakerClosed   = 0
	BreakerOpen     = 1
	BreakerHalfOpen = 2
)

// CircuitBreaker is a sliding-window failure counter that opens when too
// many non-retryable failures occur within a time window, blocks calls
// during a cooldown, then probes once via half-open.
//
// Only non-retryable failures count against the breaker — retryable
// failures (5xx, network blips) are handled by exponential backoff.
type CircuitBreaker struct {
	failureThreshold int
	window           time.Duration
	cooldown         time.Duration
	now              func() time.Time
	observer         BreakerObserver

	mu       sync.Mutex
	state    int
	failures []time.Time
	openedAt time.Time
}

// BreakerConfig parameters; zero/negative values fall back to defaults.
type BreakerConfig struct {
	FailureThreshold int
	Window           time.Duration
	Cooldown         time.Duration
	// Now optionally overrides time.Now for deterministic tests.
	Now func() time.Time
}

// NewCircuitBreaker constructs a breaker. observer may be nil.
func NewCircuitBreaker(cfg BreakerConfig, observer BreakerObserver) *CircuitBreaker {
	if cfg.FailureThreshold < 1 {
		cfg.FailureThreshold = 5
	}
	if cfg.Window <= 0 {
		cfg.Window = time.Minute
	}
	if cfg.Cooldown <= 0 {
		cfg.Cooldown = 30 * time.Second
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if observer == nil {
		observer = nopBreakerObserver{}
	}
	cb := &CircuitBreaker{
		failureThreshold: cfg.FailureThreshold,
		window:           cfg.Window,
		cooldown:         cfg.Cooldown,
		now:              cfg.Now,
		observer:         observer,
		state:            BreakerClosed,
	}
	observer.OnStateChange(BreakerClosed)
	return cb
}

// Allow reports whether a send may proceed. Open breakers transition to
// half-open after the cooldown elapses, allowing one probe.
func (cb *CircuitBreaker) Allow() bool {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	if cb.state == BreakerOpen && cb.now().Sub(cb.openedAt) >= cb.cooldown {
		cb.setStateLocked(BreakerHalfOpen)
	}
	return cb.state != BreakerOpen
}

// RecordSuccess closes the breaker on a successful send.
func (cb *CircuitBreaker) RecordSuccess() {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	if cb.state != BreakerClosed {
		cb.setStateLocked(BreakerClosed)
	}
	cb.failures = cb.failures[:0]
}

// RecordFailure records a non-retryable failure. The breaker opens once
// failureThreshold failures have occurred within the rolling window.
// In half-open it re-opens immediately.
func (cb *CircuitBreaker) RecordFailure() {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	now := cb.now()
	if cb.state == BreakerHalfOpen {
		cb.openedAt = now
		cb.setStateLocked(BreakerOpen)
		return
	}

	cutoff := now.Add(-cb.window)
	pruned := cb.failures[:0]
	for _, t := range cb.failures {
		if t.After(cutoff) {
			pruned = append(pruned, t)
		}
	}
	cb.failures = append(pruned, now)

	if len(cb.failures) >= cb.failureThreshold {
		cb.openedAt = now
		cb.setStateLocked(BreakerOpen)
		cb.failures = cb.failures[:0]
	}
}

// State returns the current breaker state.
func (cb *CircuitBreaker) State() int {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	return cb.state
}

func (cb *CircuitBreaker) setStateLocked(s int) {
	if cb.state == s {
		return
	}
	cb.state = s
	cb.observer.OnStateChange(s)
}
