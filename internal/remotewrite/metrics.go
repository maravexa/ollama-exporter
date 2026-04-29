package remotewrite

import (
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// Closed sets, used both for label values in the metrics emitted here and
// for cardinality assertions in tests.
const (
	ReasonQueueFull            = "queue_full"
	ReasonNonRetryable         = "non_retryable"
	ReasonRetryBudgetExhausted = "retry_budget_exhausted"
	ReasonBreakerOpen          = "breaker_open"
	ReasonShutdownDrain        = "shutdown_drain"

	OutcomeSuccess           = "success"
	OutcomeRetryableError    = "retryable_error"
	OutcomeNonRetryableError = "non_retryable_error"
	OutcomeTimeout           = "timeout"
)

// selfMetrics owns the Prometheus collectors that surface remote write
// internal state on /metrics. It is created once and shared across all
// endpoints (label-multiplexed via the `endpoint` label).
type selfMetrics struct {
	samplesTotal      *prometheus.CounterVec
	samplesFailed     *prometheus.CounterVec
	samplesDropped    *prometheus.CounterVec
	sendDuration      *prometheus.HistogramVec
	queueLength       *prometheus.GaugeVec
	queueCapacity     *prometheus.GaugeVec
	lastSendTS        *prometheus.GaugeVec
	retriesTotal      *prometheus.CounterVec
	breakerStateGauge *prometheus.GaugeVec
}

// metricsMu guards the package-level singleton so repeat constructions
// (e.g. in tests) reuse a single registration set per registry.
var (
	metricsMu    sync.Mutex
	metricsByReg = map[prometheus.Registerer]*selfMetrics{}
)

// newSelfMetrics returns the selfMetrics for reg, registering them on
// first use. Subsequent calls with the same reg return the cached
// instance — registering twice would panic.
func newSelfMetrics(reg prometheus.Registerer) *selfMetrics {
	metricsMu.Lock()
	defer metricsMu.Unlock()
	if reg == nil {
		// Anonymous registerer for tests that don't care.
		reg = prometheus.NewRegistry()
	}
	if m, ok := metricsByReg[reg]; ok {
		return m
	}
	m := &selfMetrics{
		samplesTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "ollama_exporter",
			Subsystem: "remote_write",
			Name:      "samples_total",
			Help:      "Cumulative number of samples successfully delivered to a remote write endpoint.",
		}, []string{"endpoint"}),
		samplesFailed: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "ollama_exporter",
			Subsystem: "remote_write",
			Name:      "samples_failed_total",
			Help:      "Cumulative number of samples that could not be delivered, by reason.",
		}, []string{"endpoint", "reason"}),
		samplesDropped: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "ollama_exporter",
			Subsystem: "remote_write",
			Name:      "samples_dropped_total",
			Help:      "Cumulative number of samples dropped before send, by reason.",
		}, []string{"endpoint", "reason"}),
		sendDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: "ollama_exporter",
			Subsystem: "remote_write",
			Name:      "send_duration_seconds",
			Help:      "Wall-clock duration of a single remote write attempt, by outcome.",
			Buckets:   prometheus.ExponentialBuckets(0.005, 2, 12),
		}, []string{"endpoint", "outcome"}),
		queueLength: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: "ollama_exporter",
			Subsystem: "remote_write",
			Name:      "queue_length",
			Help:      "Number of batches currently buffered for a remote write endpoint.",
		}, []string{"endpoint"}),
		queueCapacity: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: "ollama_exporter",
			Subsystem: "remote_write",
			Name:      "queue_capacity",
			Help:      "Configured maximum batch capacity for a remote write endpoint queue.",
		}, []string{"endpoint"}),
		lastSendTS: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: "ollama_exporter",
			Subsystem: "remote_write",
			Name:      "last_send_timestamp_seconds",
			Help:      "Wall-clock unix time of the last successful send to a remote write endpoint.",
		}, []string{"endpoint"}),
		retriesTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "ollama_exporter",
			Subsystem: "remote_write",
			Name:      "retries_total",
			Help:      "Cumulative number of retry attempts across all batches sent to a remote write endpoint.",
		}, []string{"endpoint"}),
		breakerStateGauge: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: "ollama_exporter",
			Subsystem: "remote_write",
			Name:      "circuit_breaker_state",
			Help:      "Circuit breaker state for a remote write endpoint (0=closed, 1=open, 2=half-open).",
		}, []string{"endpoint"}),
	}
	reg.MustRegister(
		m.samplesTotal,
		m.samplesFailed,
		m.samplesDropped,
		m.sendDuration,
		m.queueLength,
		m.queueCapacity,
		m.lastSendTS,
		m.retriesTotal,
		m.breakerStateGauge,
	)
	metricsByReg[reg] = m
	return m
}

func (m *selfMetrics) queueObserver(endpoint string) QueueObserver {
	return &queueObs{m: m, endpoint: endpoint}
}

func (m *selfMetrics) senderObserver(endpoint string) SenderObserver {
	return &senderObs{m: m, endpoint: endpoint}
}

func (m *selfMetrics) breakerObserver(endpoint string) BreakerObserver {
	return &breakerObs{m: m, endpoint: endpoint}
}

type queueObs struct {
	m        *selfMetrics
	endpoint string
}

func (o *queueObs) OnEnqueue(length int) {
	o.m.queueLength.WithLabelValues(o.endpoint).Set(float64(length))
}
func (o *queueObs) OnDequeue(length int) {
	o.m.queueLength.WithLabelValues(o.endpoint).Set(float64(length))
}
func (o *queueObs) SetCapacity(capacity int) {
	o.m.queueCapacity.WithLabelValues(o.endpoint).Set(float64(capacity))
}
func (o *queueObs) OnDrop(reason string, samplesDropped int) {
	o.m.samplesDropped.WithLabelValues(o.endpoint, reason).Add(float64(samplesDropped))
}

type senderObs struct {
	m        *selfMetrics
	endpoint string
}

func (o *senderObs) OnSendOutcome(outcome string, d time.Duration) {
	o.m.sendDuration.WithLabelValues(o.endpoint, outcome).Observe(d.Seconds())
}
func (o *senderObs) OnSamplesSent(n int) {
	o.m.samplesTotal.WithLabelValues(o.endpoint).Add(float64(n))
}
func (o *senderObs) OnSamplesFailed(reason string, n int) {
	o.m.samplesFailed.WithLabelValues(o.endpoint, reason).Add(float64(n))
}
func (o *senderObs) OnSamplesDropped(reason string, n int) {
	o.m.samplesDropped.WithLabelValues(o.endpoint, reason).Add(float64(n))
}
func (o *senderObs) OnRetry() { o.m.retriesTotal.WithLabelValues(o.endpoint).Inc() }
func (o *senderObs) OnLastSend(t time.Time) {
	o.m.lastSendTS.WithLabelValues(o.endpoint).Set(float64(t.Unix()))
}

type breakerObs struct {
	m        *selfMetrics
	endpoint string
}

func (o *breakerObs) OnStateChange(state int) {
	o.m.breakerStateGauge.WithLabelValues(o.endpoint).Set(float64(state))
}
