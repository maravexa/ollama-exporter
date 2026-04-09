// Package metrics defines all Prometheus metric descriptors for the exporter.
// No collection logic lives here — this package is pure metric registration.
package metrics

import "github.com/prometheus/client_golang/prometheus"

// Metrics holds all registered Prometheus collectors.
type Metrics struct {
	// Health
	Up *prometheus.GaugeVec

	// Model lifecycle
	ModelLoaded             *prometheus.GaugeVec
	ModelVRAMBytes          *prometheus.GaugeVec
	ModelLoadTotal          *prometheus.CounterVec
	ModelUnloadTotal        *prometheus.CounterVec
	ModelLoadEventsTotal    *prometheus.CounterVec
	ModelUnloadEventsTotal  *prometheus.CounterVec
	ModelLoadDurationSeconds *prometheus.HistogramVec

	// Per-request inference (proxy mode)
	RequestDuration      *prometheus.HistogramVec
	LoadDuration         *prometheus.HistogramVec
	PromptEvalDuration   *prometheus.HistogramVec
	EvalDuration         *prometheus.HistogramVec
	TokensPerSecond      *prometheus.GaugeVec
	PromptTokensPerSec   *prometheus.GaugeVec
	RequestsInFlight     *prometheus.GaugeVec
	RequestsTotal        *prometheus.CounterVec
	KVCachePressureRatio *prometheus.GaugeVec
}

// New constructs and registers all metrics with the given registry.
func New(reg prometheus.Registerer) *Metrics {
	m := &Metrics{
		Up: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: "ollama",
			Name:      "up",
			Help:      "1 if the Ollama API is reachable, 0 otherwise.",
		}, []string{}),

		ModelLoaded: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: "ollama",
			Name:      "model_loaded",
			Help:      "1 if the model is currently resident in VRAM.",
		}, []string{"model", "family", "quant"}),

		ModelVRAMBytes: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: "ollama",
			Name:      "model_vram_bytes",
			Help:      "VRAM consumed by the loaded model in bytes.",
		}, []string{"model", "family", "quant"}),

		ModelLoadTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "ollama",
			Name:      "model_load_total",
			Help:      "Cumulative number of times this model has been loaded into VRAM.",
		}, []string{"model", "family", "quant"}),

		ModelUnloadTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "ollama",
			Name:      "model_unload_total",
			Help:      "Cumulative number of times this model has been evicted from VRAM.",
		}, []string{"model", "family", "quant"}),

		ModelLoadEventsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "ollama",
			Name:      "model_load_events_total",
			Help:      "Number of model load transitions detected since exporter startup. Not incremented for models already loaded at startup.",
		}, []string{"model"}),

		ModelUnloadEventsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "ollama",
			Name:      "model_unload_events_total",
			Help:      "Number of model unload transitions detected since exporter startup.",
		}, []string{"model"}),

		ModelLoadDurationSeconds: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: "ollama",
			Name:      "model_load_duration_seconds",
			Help:      "Time spent loading a model, sourced from the load_duration field in proxied /api/generate and /api/chat responses.",
			Buckets:   []float64{0.5, 1, 2, 5, 10, 20, 30, 60, 120},
		}, []string{"model"}),

		RequestDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: "ollama",
			Name:      "request_duration_seconds",
			Help:      "End-to-end latency of requests proxied through the exporter.",
			Buckets:   prometheus.ExponentialBuckets(0.1, 2, 12),
		}, []string{"model", "family", "quant", "endpoint"}),

		LoadDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: "ollama",
			Name:      "load_duration_seconds",
			Help:      "Time spent loading the model for a request, in seconds.",
			Buckets:   prometheus.ExponentialBuckets(0.001, 4, 10),
		}, []string{"model", "family", "quant"}),

		PromptEvalDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: "ollama",
			Name:      "prompt_eval_duration_seconds",
			Help:      "Time spent evaluating the prompt (prefill phase), in seconds.",
			Buckets:   prometheus.ExponentialBuckets(0.01, 2, 12),
		}, []string{"model", "family", "quant"}),

		EvalDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: "ollama",
			Name:      "eval_duration_seconds",
			Help:      "Time spent generating tokens (decode phase), in seconds.",
			Buckets:   prometheus.ExponentialBuckets(0.05, 2, 12),
		}, []string{"model", "family", "quant"}),

		TokensPerSecond: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: "ollama",
			Name:      "tokens_per_second",
			Help:      "Derived decode throughput: eval_count / eval_duration.",
		}, []string{"model", "family", "quant"}),

		PromptTokensPerSec: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: "ollama",
			Name:      "prompt_tokens_per_second",
			Help:      "Derived prefill throughput: prompt_eval_count / prompt_eval_duration.",
		}, []string{"model", "family", "quant"}),

		RequestsInFlight: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: "ollama",
			Name:      "requests_in_flight",
			Help:      "Number of requests currently being processed (proxy mode only).",
		}, []string{"model", "endpoint"}),

		RequestsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "ollama",
			Name:      "requests_total",
			Help:      "Total number of requests proxied, by model and endpoint.",
		}, []string{"model", "family", "quant", "endpoint"}),

		KVCachePressureRatio: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: "ollama",
			Name:      "kv_cache_pressure_ratio",
			Help:      "Derived KV cache pressure: prompt_eval_duration_ns / prompt_eval_count. Rising values suggest cache misses.",
		}, []string{"model", "family", "quant"}),
	}

	reg.MustRegister(
		m.Up,
		m.ModelLoaded,
		m.ModelVRAMBytes,
		m.ModelLoadTotal,
		m.ModelUnloadTotal,
		m.ModelLoadEventsTotal,
		m.ModelUnloadEventsTotal,
		m.ModelLoadDurationSeconds,
		m.RequestDuration,
		m.LoadDuration,
		m.PromptEvalDuration,
		m.EvalDuration,
		m.TokensPerSecond,
		m.PromptTokensPerSec,
		m.RequestsInFlight,
		m.RequestsTotal,
		m.KVCachePressureRatio,
	)

	return m
}
