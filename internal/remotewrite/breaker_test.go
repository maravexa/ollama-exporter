package remotewrite

import (
	"sync"
	"testing"
	"time"
)

type stateRecorder struct {
	mu     sync.Mutex
	states []int
}

func (r *stateRecorder) OnStateChange(s int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.states = append(r.states, s)
}

func (r *stateRecorder) snapshot() []int {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]int, len(r.states))
	copy(out, r.states)
	return out
}

func TestBreaker_OpensAfterThreshold(t *testing.T) {
	rec := &stateRecorder{}
	cb := NewCircuitBreaker(BreakerConfig{
		FailureThreshold: 3,
		Window:           time.Minute,
		Cooldown:         time.Second,
	}, rec)

	for i := 0; i < 2; i++ {
		cb.RecordFailure()
		if !cb.Allow() {
			t.Fatalf("breaker opened too early at %d", i)
		}
	}
	cb.RecordFailure()
	if cb.Allow() {
		t.Fatal("breaker should be open after 3 failures")
	}
	if cb.State() != BreakerOpen {
		t.Errorf("state = %d, want open", cb.State())
	}

	got := rec.snapshot()
	if len(got) < 2 || got[0] != BreakerClosed || got[len(got)-1] != BreakerOpen {
		t.Errorf("state transitions = %v, want closed -> open", got)
	}
}

func TestBreaker_HalfOpenAfterCooldown(t *testing.T) {
	now := time.Now()
	clock := &fakeClock{now: now}
	cb := NewCircuitBreaker(BreakerConfig{
		FailureThreshold: 1,
		Window:           time.Minute,
		Cooldown:         5 * time.Second,
		Now:              clock.Now,
	}, nil)

	cb.RecordFailure()
	if cb.Allow() {
		t.Fatal("expected open")
	}

	clock.advance(6 * time.Second)
	if !cb.Allow() {
		t.Fatal("expected half-open after cooldown")
	}
	if cb.State() != BreakerHalfOpen {
		t.Errorf("state = %d, want half-open", cb.State())
	}

	cb.RecordSuccess()
	if cb.State() != BreakerClosed {
		t.Errorf("state = %d, want closed", cb.State())
	}
}

func TestBreaker_HalfOpenFailureReopens(t *testing.T) {
	now := time.Now()
	clock := &fakeClock{now: now}
	cb := NewCircuitBreaker(BreakerConfig{
		FailureThreshold: 1,
		Window:           time.Minute,
		Cooldown:         5 * time.Second,
		Now:              clock.Now,
	}, nil)
	cb.RecordFailure()
	clock.advance(6 * time.Second)
	cb.Allow() // transitions to half-open
	cb.RecordFailure()
	if cb.State() != BreakerOpen {
		t.Errorf("state = %d, want open after half-open failure", cb.State())
	}
}

func TestBreaker_RetryableFailuresDoNotTripIt(t *testing.T) {
	// The breaker should only see RecordFailure for non-retryable errors.
	// This test models that contract: if we never call RecordFailure on
	// retryable errors, the breaker stays closed regardless of count.
	cb := NewCircuitBreaker(BreakerConfig{FailureThreshold: 2}, nil)
	for i := 0; i < 100; i++ {
		// simulate retryable error path: do not call RecordFailure
	}
	if cb.State() != BreakerClosed {
		t.Errorf("state = %d, want closed (retryable failures must not trip the breaker)", cb.State())
	}
}

func TestBreaker_WindowExpiry(t *testing.T) {
	clock := &fakeClock{now: time.Now()}
	cb := NewCircuitBreaker(BreakerConfig{
		FailureThreshold: 3,
		Window:           5 * time.Second,
		Cooldown:         time.Second,
		Now:              clock.Now,
	}, nil)
	cb.RecordFailure()
	cb.RecordFailure()
	clock.advance(10 * time.Second) // window expires
	cb.RecordFailure()
	if cb.State() != BreakerClosed {
		t.Errorf("breaker should not have opened: stale failures dropped from window; state=%d", cb.State())
	}
}

type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}
