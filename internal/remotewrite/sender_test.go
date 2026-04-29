package remotewrite

import (
	"context"
	"errors"
	mathrand "math/rand"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/prometheus/prompb"
)

type recordingSenderObserver struct {
	mu            sync.Mutex
	sent          int
	failed        map[string]int
	dropped       map[string]int
	retries       int
	lastSend      time.Time
	outcomes      []string
	outcomeCounts map[string]int
}

func newRecordingSenderObserver() *recordingSenderObserver {
	return &recordingSenderObserver{
		failed:        map[string]int{},
		dropped:       map[string]int{},
		outcomeCounts: map[string]int{},
	}
}

func (r *recordingSenderObserver) OnSendOutcome(o string, _ time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.outcomes = append(r.outcomes, o)
	r.outcomeCounts[o]++
}
func (r *recordingSenderObserver) OnSamplesSent(n int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sent += n
}
func (r *recordingSenderObserver) OnSamplesFailed(reason string, n int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.failed[reason] += n
}
func (r *recordingSenderObserver) OnSamplesDropped(reason string, n int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.dropped[reason] += n
}
func (r *recordingSenderObserver) OnRetry() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.retries++
}
func (r *recordingSenderObserver) OnLastSend(t time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lastSend = t
}

func smallBatch() []prompb.TimeSeries {
	return []prompb.TimeSeries{{
		Labels:  []prompb.Label{{Name: "__name__", Value: "x"}},
		Samples: []prompb.Sample{{Value: 1, Timestamp: time.Now().UnixMilli()}},
	}}
}

func TestSender_RetriesUntilSuccess(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := calls.Add(1)
		if n <= 5 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	obs := newRecordingSenderObserver()
	q := NewQueue(10, nil)
	cfg := EndpointConfig{
		Name:           "test",
		URL:            srv.URL,
		Timeout:        time.Second,
		MaxAttempts:    20,
		MaxElapsed:     30 * time.Second,
		InitialBackoff: 1 * time.Millisecond,
		MaxBackoff:     5 * time.Millisecond,
	}
	s := NewSender(cfg, q, srv.Client(), nil, obs, nil)

	q.Enqueue(smallBatch())
	q.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	s.Run(ctx)

	if obs.sent != 1 {
		t.Errorf("sent = %d, want 1", obs.sent)
	}
	if obs.retries < 5 {
		t.Errorf("retries = %d, want >= 5", obs.retries)
	}
	if calls.Load() != 6 {
		t.Errorf("server saw %d calls, want 6", calls.Load())
	}
}

func TestSender_NonRetryableDropped(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer srv.Close()

	obs := newRecordingSenderObserver()
	q := NewQueue(10, nil)
	br := NewCircuitBreaker(BreakerConfig{FailureThreshold: 5}, nil)
	cfg := EndpointConfig{
		Name:           "test",
		URL:            srv.URL,
		Timeout:        time.Second,
		MaxAttempts:    5,
		InitialBackoff: 1 * time.Millisecond,
		MaxBackoff:     2 * time.Millisecond,
	}
	s := NewSender(cfg, q, srv.Client(), br, obs, nil)
	q.Enqueue(smallBatch())
	q.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	s.Run(ctx)

	if obs.sent != 0 {
		t.Errorf("sent = %d, want 0", obs.sent)
	}
	if obs.failed["non_retryable"] != 1 {
		t.Errorf("failed[non_retryable] = %d, want 1", obs.failed["non_retryable"])
	}
	if obs.retries != 0 {
		t.Errorf("retries = %d, want 0 for non-retryable", obs.retries)
	}
}

func TestSender_RetryBudgetExhausted(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	obs := newRecordingSenderObserver()
	q := NewQueue(10, nil)
	br := NewCircuitBreaker(BreakerConfig{FailureThreshold: 100}, nil)
	cfg := EndpointConfig{
		Name:           "test",
		URL:            srv.URL,
		Timeout:        100 * time.Millisecond,
		MaxAttempts:    3,
		InitialBackoff: 1 * time.Millisecond,
		MaxBackoff:     2 * time.Millisecond,
	}
	s := NewSender(cfg, q, srv.Client(), br, obs, nil)
	q.Enqueue(smallBatch())
	q.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	s.Run(ctx)

	if obs.failed["retry_budget_exhausted"] != 1 {
		t.Errorf("failed[retry_budget_exhausted] = %d, want 1", obs.failed["retry_budget_exhausted"])
	}
	if br.State() != BreakerClosed {
		t.Errorf("breaker should not trip on retryable errors, state=%d", br.State())
	}
}

func TestSender_BreakerOpenDropsBatch(t *testing.T) {
	obs := newRecordingSenderObserver()
	q := NewQueue(10, nil)
	br := NewCircuitBreaker(BreakerConfig{FailureThreshold: 1, Cooldown: time.Hour}, nil)
	br.RecordFailure() // force open

	cfg := EndpointConfig{
		Name:           "test",
		URL:            "http://127.0.0.1:1", // never reached
		Timeout:        time.Second,
		MaxAttempts:    1,
		InitialBackoff: time.Millisecond,
		MaxBackoff:     time.Millisecond,
	}
	s := NewSender(cfg, q, http.DefaultClient, br, obs, nil)

	q.Enqueue(smallBatch())
	q.Close()

	s.Run(context.Background())

	if obs.dropped["breaker_open"] != 1 {
		t.Errorf("dropped[breaker_open] = %d, want 1", obs.dropped["breaker_open"])
	}
}

func TestComputeBackoff_FullJitterBounds(t *testing.T) {
	const iterations = 1000
	cfg := EndpointConfig{
		InitialBackoff: 100 * time.Millisecond,
		MaxBackoff:     10 * time.Second,
	}
	s := &Sender{cfg: cfg, rng: mathrand.New(mathrand.NewSource(42))}

	// At attempt 3, upper bound should be min(100ms * 2^3, 10s) = 800ms.
	const attempt = 3
	upper := time.Duration(int64(cfg.InitialBackoff) << attempt)
	if upper > cfg.MaxBackoff {
		upper = cfg.MaxBackoff
	}

	var maxObs time.Duration
	var total time.Duration
	for i := 0; i < iterations; i++ {
		d := s.computeBackoff(attempt, nil)
		if d < 0 {
			t.Fatalf("negative backoff: %v", d)
		}
		if d > upper {
			t.Fatalf("backoff %v exceeds upper bound %v", d, upper)
		}
		if d > maxObs {
			maxObs = d
		}
		total += d
	}
	mean := total / iterations
	// Full jitter: expected mean ~ upper/2.
	if mean < upper/4 || mean > upper*3/4 {
		t.Errorf("mean = %v, want roughly upper/2 = %v", mean, upper/2)
	}
	if maxObs < upper/2 {
		t.Errorf("max observed %v is suspiciously low (upper=%v)", maxObs, upper)
	}
}

func TestComputeBackoff_HonorsRetryAfter(t *testing.T) {
	s := &Sender{
		cfg: EndpointConfig{
			InitialBackoff: time.Millisecond,
			MaxBackoff:     10 * time.Second,
		},
		rng: mathrand.New(mathrand.NewSource(1)),
	}
	err := &ErrRetryable{RetryAfter: 2 * time.Second}
	for i := 0; i < 20; i++ {
		d := s.computeBackoff(0, err)
		if d < 2*time.Second {
			t.Errorf("backoff %v < Retry-After 2s", d)
		}
	}
}

func TestSender_RedactedErrorPaths(t *testing.T) {
	// Verify embedded URL credentials are redacted in error paths the
	// sender logs. We don't capture the log output here; we just exercise
	// redactErr with a representative net/http style error.
	err := errors.New("Post \"https://user:secret@mimir.example.com/push\": connection refused")
	got := redactErr(err)
	if strings.Contains(got, "secret") || strings.Contains(got, "user:") {
		t.Errorf("credentials leaked in redacted error: %q", got)
	}
	if !strings.Contains(got, "REDACTED:REDACTED") {
		t.Errorf("expected REDACTED:REDACTED in output, got %q", got)
	}
}

func TestSender_429RetryAfterHonored(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := calls.Add(1)
		if n == 1 {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	obs := newRecordingSenderObserver()
	q := NewQueue(10, nil)
	cfg := EndpointConfig{
		Name:           "test",
		URL:            srv.URL,
		Timeout:        time.Second,
		MaxAttempts:    3,
		MaxElapsed:     10 * time.Second,
		InitialBackoff: time.Microsecond,
		MaxBackoff:     2 * time.Second,
	}
	s := NewSender(cfg, q, srv.Client(), nil, obs, nil)

	q.Enqueue(smallBatch())
	q.Close()

	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	s.Run(ctx)
	elapsed := time.Since(start)

	if obs.sent != 1 {
		t.Errorf("sent = %d, want 1", obs.sent)
	}
	// Should have waited at least ~1s due to Retry-After.
	if elapsed < 800*time.Millisecond {
		t.Errorf("elapsed = %v, want >= ~1s (Retry-After honored)", elapsed)
	}
}
