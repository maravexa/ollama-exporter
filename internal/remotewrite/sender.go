package remotewrite

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"log/slog"
	mathrand "math/rand"
	"net/http"
	"sync"
	"time"

	"github.com/prometheus/prometheus/prompb"
)

// EndpointConfig captures everything a Sender needs to know about a single
// remote write endpoint. Credentials are referenced via *_file paths
// resolved by the config layer; the sender receives only the resolved
// values.
type EndpointConfig struct {
	Name           string
	URL            string
	Timeout        time.Duration
	MaxAttempts    int
	MaxElapsed     time.Duration
	InitialBackoff time.Duration
	MaxBackoff     time.Duration
	Headers        map[string]string
	BasicAuthUser  string
	BasicAuthPass  string
	BearerToken    string
	BreakerCfg     BreakerConfig
	ExternalLabels Labels
}

// Sender owns the retry loop for a single endpoint. It dequeues batches
// from its queue, encodes them, and POSTs them to the configured URL with
// bounded retry budget and circuit-breaker protection.
type Sender struct {
	cfg      EndpointConfig
	queue    *Queue
	client   *http.Client
	breaker  *CircuitBreaker
	observer SenderObserver
	logger   *slog.Logger

	rng   *mathrand.Rand
	rngMu sync.Mutex
}

// NewSender constructs a sender. queue and client must be non-nil.
// breaker and observer may be nil (no-op stubs are substituted).
func NewSender(cfg EndpointConfig, queue *Queue, client *http.Client, breaker *CircuitBreaker, observer SenderObserver, logger *slog.Logger) *Sender {
	if observer == nil {
		observer = nopSenderObserver{}
	}
	if breaker == nil {
		breaker = NewCircuitBreaker(cfg.BreakerCfg, nil)
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Sender{
		cfg:      cfg,
		queue:    queue,
		client:   client,
		breaker:  breaker,
		observer: observer,
		logger:   logger.With("endpoint", cfg.Name),
		rng:      newSeededRand(),
	}
}

// Enqueue offers a batch to the underlying queue. Convenience wrapper.
func (s *Sender) Enqueue(ts []prompb.TimeSeries) {
	s.queue.Enqueue(ts)
}

// Run dequeues batches and sends them until ctx is cancelled or the queue
// is closed and drained. Terminal errors are logged per batch; this loop
// itself never propagates an error to the caller.
func (s *Sender) Run(ctx context.Context) {
	for {
		batch, err := s.queue.Dequeue(ctx)
		if err != nil {
			if errors.Is(err, ErrQueueClosed) ||
				errors.Is(err, context.Canceled) ||
				errors.Is(err, context.DeadlineExceeded) {
				return
			}
			s.logger.Error("dequeue error", "err", err)
			return
		}
		s.processBatch(ctx, batch)
	}
}

// processBatch handles one batch with full retry/breaker semantics.
func (s *Sender) processBatch(ctx context.Context, batch []prompb.TimeSeries) {
	if !s.breaker.Allow() {
		s.observer.OnSamplesDropped("breaker_open", len(batch))
		s.logger.Warn("circuit breaker open; dropping batch", "samples", len(batch))
		return
	}

	body, err := encodeWriteRequest(batch)
	if err != nil {
		s.observer.OnSamplesFailed("non_retryable", len(batch))
		s.logger.Error("encode batch", "err", redactErr(err))
		return
	}

	maxAttempts := s.cfg.MaxAttempts
	if maxAttempts < 1 {
		maxAttempts = 1
	}
	deadline := time.Time{}
	if s.cfg.MaxElapsed > 0 {
		deadline = time.Now().Add(s.cfg.MaxElapsed)
	}

	for attempt := 0; attempt < maxAttempts; attempt++ {
		sendCtx, cancel := s.sendContext(ctx)
		start := time.Now()
		err := sendWriteRequest(sendCtx, s.client, s.cfg.URL, body, s.composeHeaders())
		dur := time.Since(start)
		cancel()

		switch {
		case err == nil:
			s.observer.OnSamplesSent(len(batch))
			s.observer.OnSendOutcome("success", dur)
			s.observer.OnLastSend(time.Now())
			s.breaker.RecordSuccess()
			return
		case errors.Is(err, context.Canceled):
			s.observer.OnSendOutcome("timeout", dur)
			s.observer.OnSamplesDropped("shutdown_drain", len(batch))
			return
		case errors.Is(err, context.DeadlineExceeded):
			s.observer.OnSendOutcome("timeout", dur)
		case IsNonRetryable(err):
			s.observer.OnSendOutcome("non_retryable_error", dur)
			s.observer.OnSamplesFailed("non_retryable", len(batch))
			s.logger.Error("non-retryable error from receiver", "err", redactErr(err))
			s.breaker.RecordFailure()
			return
		case IsRetryable(err):
			s.observer.OnSendOutcome("retryable_error", dur)
		default:
			s.observer.OnSendOutcome("retryable_error", dur)
		}

		if attempt+1 >= maxAttempts {
			break
		}
		if !deadline.IsZero() && time.Now().After(deadline) {
			break
		}

		wait := s.computeBackoff(attempt, err)
		if !deadline.IsZero() {
			remaining := time.Until(deadline)
			if wait > remaining {
				wait = remaining
			}
			if wait <= 0 {
				break
			}
		}

		s.observer.OnRetry()
		s.logger.Debug("retrying batch", "attempt", attempt+1, "wait", wait, "err", redactErr(err))

		timer := time.NewTimer(wait)
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			s.observer.OnSamplesDropped("shutdown_drain", len(batch))
			return
		}
	}

	s.observer.OnSamplesFailed("retry_budget_exhausted", len(batch))
	s.logger.Warn("retry budget exhausted", "samples", len(batch))
}

// computeBackoff returns the next sleep using full jitter:
//
//	sleep = rand(0, base * 2^attempt), capped at MaxBackoff
//
// If the receiver returned Retry-After, we honor at minimum that delay
// (still capped at MaxBackoff to prevent a misbehaving server from
// stalling the sender indefinitely).
func (s *Sender) computeBackoff(attempt int, err error) time.Duration {
	base := s.cfg.InitialBackoff
	if base <= 0 {
		base = time.Second
	}
	maxB := s.cfg.MaxBackoff
	if maxB <= 0 {
		maxB = 30 * time.Second
	}

	// Clamp the attempt exponent so the left-shift cannot overflow base
	// (and so we don't allocate a giant Duration that wraps negative).
	exp := attempt
	if exp < 0 {
		exp = 0
	}
	if exp > 16 {
		exp = 16
	}
	upper := base << uint(exp) //nolint:gosec // exp is clamped to [0,16] above
	if upper > maxB || upper <= 0 {
		upper = maxB
	}

	s.rngMu.Lock()
	jittered := time.Duration(s.rng.Int63n(int64(upper) + 1))
	s.rngMu.Unlock()

	var rerr *ErrRetryable
	if errors.As(err, &rerr) && rerr.RetryAfter > 0 {
		floor := rerr.RetryAfter
		if floor > maxB {
			floor = maxB
		}
		if jittered < floor {
			jittered = floor
		}
	}
	return jittered
}

// composeHeaders merges Authorization, basic auth, and user headers.
// The wire layer prevents protocol headers from being overridden.
func (s *Sender) composeHeaders() map[string]string {
	out := make(map[string]string, len(s.cfg.Headers)+1)
	for k, v := range s.cfg.Headers {
		out[k] = v
	}
	if s.cfg.BearerToken != "" {
		out["Authorization"] = "Bearer " + s.cfg.BearerToken
	} else if s.cfg.BasicAuthUser != "" {
		out["Authorization"] = basicAuthHeader(s.cfg.BasicAuthUser, s.cfg.BasicAuthPass)
	}
	return out
}

// sendContext returns a per-attempt context bounded by Timeout (if set).
func (s *Sender) sendContext(parent context.Context) (context.Context, context.CancelFunc) {
	if s.cfg.Timeout > 0 {
		return context.WithTimeout(parent, s.cfg.Timeout)
	}
	return parent, func() {}
}

// newSeededRand seeds a math/rand source from crypto/rand so tests using
// the production constructor still get distinct jitter draws.
//
// The PRNG itself is math/rand (not crypto/rand) on purpose — backoff
// jitter is a load-shedding tactic, not a security primitive. gosec's
// G404 is suppressed below for that reason.
func newSeededRand() *mathrand.Rand {
	var seed [8]byte
	if _, err := rand.Read(seed[:]); err != nil {
		return mathrand.New(mathrand.NewSource(time.Now().UnixNano())) //nolint:gosec // jitter only, not a security primitive
	}
	return mathrand.New(mathrand.NewSource(int64(binary.LittleEndian.Uint64(seed[:])))) //nolint:gosec // jitter only, not a security primitive
}
