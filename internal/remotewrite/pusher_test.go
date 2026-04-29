package remotewrite

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

func TestPusher_GathersAndFansOut(t *testing.T) {
	var posts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		posts.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	reg := prometheus.NewRegistry()
	c := prometheus.NewCounter(prometheus.CounterOpts{Name: "pushertest_total"})
	reg.MustRegister(c)
	c.Inc()

	q := NewQueue(10, nil)
	cfg := EndpointConfig{
		Name:           "test",
		URL:            srv.URL,
		Timeout:        time.Second,
		MaxAttempts:    1,
		InitialBackoff: time.Millisecond,
		MaxBackoff:     time.Millisecond,
	}
	s := NewSender(cfg, q, srv.Client(), nil, nil, nil)
	p := NewPusher(PusherConfig{
		FlushInterval: 50 * time.Millisecond,
		Gatherer:      reg,
		DrainTimeout:  time.Second,
	}, []*Sender{s})

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	if err := p.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if posts.Load() < 2 {
		t.Errorf("posts = %d, want >= 2 over 500ms with 50ms interval", posts.Load())
	}
}

func TestPusher_DrainTimeoutCancelsInFlight(t *testing.T) {
	// Receiver hangs until either the request context cancels (expected
	// when the sender drops the connection) or the test signals quit.
	// The quit channel guarantees httptest.Server.Close() returns even if
	// the request-context cancellation propagation is sluggish.
	quit := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-quit:
		}
	}))
	defer func() {
		close(quit)
		srv.Close()
	}()

	reg := prometheus.NewRegistry()
	g := prometheus.NewGauge(prometheus.GaugeOpts{Name: "drain_test"})
	reg.MustRegister(g)
	g.Set(1)

	q := NewQueue(10, nil)
	cfg := EndpointConfig{
		Name:           "test",
		URL:            srv.URL,
		Timeout:        5 * time.Second,
		MaxAttempts:    1,
		InitialBackoff: time.Millisecond,
		MaxBackoff:     time.Millisecond,
	}
	s := NewSender(cfg, q, srv.Client(), nil, nil, nil)
	p := NewPusher(PusherConfig{
		FlushInterval: 50 * time.Millisecond,
		Gatherer:      reg,
		DrainTimeout:  100 * time.Millisecond,
	}, []*Sender{s})

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(120 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	_ = p.Run(ctx)
	elapsed := time.Since(start)

	// Should return within drain timeout + small overhead, not wait for the
	// hung receiver indefinitely.
	if elapsed > 2*time.Second {
		t.Errorf("Run blocked for %v; drain timeout did not cancel in-flight send", elapsed)
	}
}
