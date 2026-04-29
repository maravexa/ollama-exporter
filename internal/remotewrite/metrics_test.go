package remotewrite

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestSelfMetrics_RegisterOnceAndIncrement(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := newSelfMetrics(reg)

	qo := m.queueObserver("primary")
	qo.SetCapacity(100)
	qo.OnEnqueue(7)
	qo.OnDequeue(6)
	qo.OnDrop(ReasonQueueFull, 3)

	so := m.senderObserver("primary")
	so.OnSamplesSent(5)
	so.OnRetry()
	so.OnSendOutcome(OutcomeSuccess, 50*time.Millisecond)
	so.OnSamplesFailed(ReasonNonRetryable, 2)
	so.OnSamplesDropped(ReasonBreakerOpen, 4)
	so.OnLastSend(time.Unix(1700000000, 0))

	bo := m.breakerObserver("primary")
	bo.OnStateChange(BreakerOpen)

	if got := testutil.ToFloat64(m.queueCapacity.WithLabelValues("primary")); got != 100 {
		t.Errorf("queueCapacity = %v", got)
	}
	if got := testutil.ToFloat64(m.queueLength.WithLabelValues("primary")); got != 6 {
		t.Errorf("queueLength = %v", got)
	}
	if got := testutil.ToFloat64(m.samplesTotal.WithLabelValues("primary")); got != 5 {
		t.Errorf("samplesTotal = %v", got)
	}
	if got := testutil.ToFloat64(m.retriesTotal.WithLabelValues("primary")); got != 1 {
		t.Errorf("retriesTotal = %v", got)
	}
	if got := testutil.ToFloat64(m.samplesFailed.WithLabelValues("primary", ReasonNonRetryable)); got != 2 {
		t.Errorf("samplesFailed[non_retryable] = %v", got)
	}
	if got := testutil.ToFloat64(m.samplesDropped.WithLabelValues("primary", ReasonBreakerOpen)); got != 4 {
		t.Errorf("samplesDropped[breaker_open] = %v", got)
	}
	if got := testutil.ToFloat64(m.samplesDropped.WithLabelValues("primary", ReasonQueueFull)); got != 3 {
		t.Errorf("samplesDropped[queue_full] = %v", got)
	}
	if got := testutil.ToFloat64(m.breakerStateGauge.WithLabelValues("primary")); got != float64(BreakerOpen) {
		t.Errorf("breakerStateGauge = %v", got)
	}
	if got := testutil.ToFloat64(m.lastSendTS.WithLabelValues("primary")); got != float64(1700000000) {
		t.Errorf("lastSendTS = %v", got)
	}
}

func TestSelfMetrics_DistinctRegistriesNoConflict(t *testing.T) {
	regA := prometheus.NewRegistry()
	regB := prometheus.NewRegistry()
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("constructing two metric sets on separate registries panicked: %v", r)
		}
	}()
	_ = newSelfMetrics(regA)
	_ = newSelfMetrics(regB)
}

func TestSelfMetrics_ReasonAndOutcomeAreClosedSets(t *testing.T) {
	allowedReasons := map[string]struct{}{
		ReasonQueueFull:            {},
		ReasonNonRetryable:         {},
		ReasonRetryBudgetExhausted: {},
		ReasonBreakerOpen:          {},
		ReasonShutdownDrain:        {},
	}
	allowedOutcomes := map[string]struct{}{
		OutcomeSuccess:           {},
		OutcomeRetryableError:    {},
		OutcomeNonRetryableError: {},
		OutcomeTimeout:           {},
	}

	// Drive a Sender through every code path that emits these labels and
	// verify only allowed values appear in the metric families.
	srv503 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv503.Close()
	srv400 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer srv400.Close()
	srv200 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv200.Close()

	reg := prometheus.NewRegistry()
	m := newSelfMetrics(reg)

	for _, srv := range []*httptest.Server{srv503, srv400, srv200} {
		q := NewQueue(10, m.queueObserver(srv.URL))
		br := NewCircuitBreaker(BreakerConfig{FailureThreshold: 2, Cooldown: time.Hour}, m.breakerObserver(srv.URL))
		cfg := EndpointConfig{
			Name:           srv.URL,
			URL:            srv.URL,
			Timeout:        500 * time.Millisecond,
			MaxAttempts:    2,
			InitialBackoff: time.Millisecond,
			MaxBackoff:     2 * time.Millisecond,
		}
		s := NewSender(cfg, q, srv.Client(), br, m.senderObserver(srv.URL), nil)
		q.Enqueue(smallBatch())
		q.Close()
		s.Run(context.Background())
	}

	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	for _, mf := range families {
		switch mf.GetName() {
		case "ollama_exporter_remote_write_samples_failed_total",
			"ollama_exporter_remote_write_samples_dropped_total":
			for _, m := range mf.GetMetric() {
				for _, lp := range m.GetLabel() {
					if lp.GetName() != "reason" {
						continue
					}
					if _, ok := allowedReasons[lp.GetValue()]; !ok {
						t.Errorf("reason label value %q not in closed set", lp.GetValue())
					}
				}
			}
		case "ollama_exporter_remote_write_send_duration_seconds":
			for _, m := range mf.GetMetric() {
				for _, lp := range m.GetLabel() {
					if lp.GetName() != "outcome" {
						continue
					}
					if _, ok := allowedOutcomes[lp.GetValue()]; !ok {
						t.Errorf("outcome label value %q not in closed set", lp.GetValue())
					}
				}
			}
		}
	}
}
