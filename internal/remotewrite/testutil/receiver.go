// Package testutil provides a mock Prometheus Remote Write receiver for
// tests. It decodes incoming snappy+protobuf payloads, stores observed
// series, and supports several configurable response modes.
package testutil

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"time"

	"github.com/golang/snappy"
	"github.com/prometheus/prometheus/prompb"
)

// ResponseMode controls how the receiver replies to inbound requests.
type ResponseMode int

const (
	// ModeSuccess always returns 200.
	ModeSuccess ResponseMode = iota
	// ModeFailNTimes returns FailStatus for the first N requests, then 200.
	ModeFailNTimes
	// ModeSlowLoris sleeps for SlowDuration before responding 200 — useful
	// for timeout tests.
	ModeSlowLoris
	// ModeRetryAfter returns 429 with a Retry-After header for the first
	// request, then 200.
	ModeRetryAfter
)

// MockReceiver is an httptest.Server that decodes PRW payloads.
type MockReceiver struct {
	*httptest.Server

	mu       sync.Mutex
	series   []prompb.TimeSeries
	requests int

	mode         ResponseMode
	failN        int
	failStatus   int
	slowDuration time.Duration
	retryAfter   int
}

// New returns a started MockReceiver in ModeSuccess.
func New() *MockReceiver {
	r := &MockReceiver{mode: ModeSuccess, failStatus: http.StatusServiceUnavailable}
	r.Server = httptest.NewServer(http.HandlerFunc(r.handle))
	return r
}

// Received returns a snapshot of every TimeSeries observed so far.
func (r *MockReceiver) Received() []prompb.TimeSeries {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]prompb.TimeSeries, len(r.series))
	copy(out, r.series)
	return out
}

// RequestCount returns the number of POSTs the receiver has handled.
func (r *MockReceiver) RequestCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.requests
}

// Reset discards all stored series and request counts.
func (r *MockReceiver) Reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.series = nil
	r.requests = 0
}

// SetMode reconfigures the response behavior.
func (r *MockReceiver) SetMode(mode ResponseMode) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.mode = mode
}

// FailNTimes makes the next n requests return status, then revert to 200.
func (r *MockReceiver) FailNTimes(n int, status int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.mode = ModeFailNTimes
	r.failN = n
	if status == 0 {
		status = http.StatusServiceUnavailable
	}
	r.failStatus = status
}

// SlowLoris configures the receiver to sleep d before responding.
func (r *MockReceiver) SlowLoris(d time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.mode = ModeSlowLoris
	r.slowDuration = d
}

// RetryAfter configures the next request to return 429 with the given
// Retry-After delta-seconds value.
func (r *MockReceiver) RetryAfter(seconds int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.mode = ModeRetryAfter
	r.retryAfter = seconds
}

func (r *MockReceiver) handle(w http.ResponseWriter, req *http.Request) {
	body, err := io.ReadAll(req.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	defer req.Body.Close()

	raw, err := snappy.Decode(nil, body)
	if err != nil {
		http.Error(w, "snappy decode: "+err.Error(), http.StatusBadRequest)
		return
	}
	var wr prompb.WriteRequest
	if err := wr.Unmarshal(raw); err != nil {
		http.Error(w, "unmarshal: "+err.Error(), http.StatusBadRequest)
		return
	}

	r.mu.Lock()
	r.requests++
	r.series = append(r.series, wr.Timeseries...)
	mode := r.mode
	failN := r.failN
	failStatus := r.failStatus
	slow := r.slowDuration
	retryAfter := r.retryAfter
	if mode == ModeFailNTimes && r.failN > 0 {
		r.failN--
	}
	if mode == ModeRetryAfter {
		// Single-shot.
		r.mode = ModeSuccess
	}
	r.mu.Unlock()

	switch mode {
	case ModeSuccess:
		w.WriteHeader(http.StatusOK)
	case ModeFailNTimes:
		if failN > 0 {
			w.WriteHeader(failStatus)
			return
		}
		w.WriteHeader(http.StatusOK)
	case ModeSlowLoris:
		select {
		case <-time.After(slow):
			w.WriteHeader(http.StatusOK)
		case <-req.Context().Done():
		}
	case ModeRetryAfter:
		w.Header().Set("Retry-After", strconv.Itoa(retryAfter))
		w.WriteHeader(http.StatusTooManyRequests)
	}
}
