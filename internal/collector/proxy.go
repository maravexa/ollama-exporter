package collector

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"time"

	"github.com/maravexa/ollama-exporter/internal/config"
	"github.com/maravexa/ollama-exporter/internal/metrics"
	"github.com/maravexa/ollama-exporter/internal/ollama"
)

// Proxy is a transparent HTTP reverse proxy that intercepts Ollama API
// traffic and extracts per-request inference metrics from response bodies.
type Proxy struct {
	cfg          *config.Config
	metrics      *metrics.Metrics
	mc           *ModelCache
	server       *http.Server
	excludePaths map[string]struct{}
}

// NewProxy constructs a Proxy. It pre-builds an O(1) lookup set from
// cfg.Proxy.ExcludePaths so the hot path in the request handler is fast.
func NewProxy(cfg *config.Config, _ *ollama.Client, m *metrics.Metrics, mc *ModelCache) *Proxy {
	excluded := make(map[string]struct{}, len(cfg.Proxy.ExcludePaths))
	for _, p := range cfg.Proxy.ExcludePaths {
		excluded[p] = struct{}{}
	}
	return &Proxy{
		cfg:          cfg,
		metrics:      m,
		mc:           mc,
		excludePaths: excluded,
	}
}

// Start runs the proxy HTTP server until ctx is cancelled.
func (p *Proxy) Start(ctx context.Context) error {
	target, err := url.Parse(p.cfg.OllamaURL)
	if err != nil {
		return err
	}

	rp := httputil.NewSingleHostReverseProxy(target)
	rp.ModifyResponse = p.modifyResponse
	// Ollama streams NDJSON chunk-per-token. Flush every write straight
	// through so the proxy does not add buffering latency to interactive
	// streaming clients.
	rp.FlushInterval = -1

	mux := http.NewServeMux()
	mux.Handle("/", p.instrumentedHandler(rp))

	p.server = &http.Server{
		Addr:              p.cfg.Proxy.ListenAddr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() { //nolint:gosec // G118: shutdown ctx must outlive the already-cancelled parent; using parent ctx would make Shutdown return immediately
		<-ctx.Done()
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := p.server.Shutdown(shutCtx); err != nil {
			slog.Error("proxy shutdown", "err", err)
		}
	}()

	slog.Info("proxy listening", "addr", p.cfg.Proxy.ListenAddr, "upstream", p.cfg.OllamaURL)
	if err := p.server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

// instrumentedHandler wraps the reverse proxy to track in-flight requests
// and end-to-end latency before the response body is inspected.
// Requests whose path appears in p.excludePaths are proxied normally but
// generate no Prometheus observations, preventing internal polling calls
// from polluting inference histograms.
func (p *Proxy) instrumentedHandler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		model := r.URL.Query().Get("model")
		endpoint := r.URL.Path

		if r.Method == http.MethodPost &&
			(endpoint == "/api/generate" || endpoint == "/api/chat") &&
			r.Body != nil {
			bodyBytes, err := io.ReadAll(r.Body)
			if err != nil {
				slog.Debug("proxy: failed to read request body", "err", err)
			}
			r.Body = io.NopCloser(bytes.NewReader(bodyBytes))
			r.ContentLength = int64(len(bodyBytes))
			if len(bodyBytes) > 0 {
				var req struct {
					Model string `json:"model"`
				}
				if json.Unmarshal(bodyBytes, &req) == nil && req.Model != "" {
					model = req.Model
				}
			}
		}

		_, excluded := p.excludePaths[endpoint]
		if excluded {
			next.ServeHTTP(w, r)
			return
		}

		p.metrics.RequestsInFlight.WithLabelValues(model, endpoint).Inc()
		defer p.metrics.RequestsInFlight.WithLabelValues(model, endpoint).Dec()

		start := time.Now()
		next.ServeHTTP(w, r)

		info := p.mc.Get(context.Background(), model)
		p.metrics.RequestDuration.
			WithLabelValues(model, info.Family, info.Quant, endpoint).
			Observe(time.Since(start).Seconds())
	})
}

// modifyResponse intercepts the upstream response and installs a streaming
// NDJSON scanner over the body. Ollama replies to /api/generate and /api/chat
// with newline-delimited JSON when "stream" is true (the default for
// `ollama run`, Open WebUI and most clients) and with a single JSON object
// when it is false. In both cases the inference timing fields only appear in
// the object carrying "done": true.
//
// The scanner parses each line as it passes through to the client, so the
// response is never buffered and interactive streaming latency is preserved.
// Timings are recorded once the stream completes.
func (p *Proxy) modifyResponse(resp *http.Response) error {
	if resp.Request == nil || resp.Request.Method != http.MethodPost {
		return nil
	}
	path := resp.Request.URL.Path
	if path != "/api/generate" && path != "/api/chat" {
		return nil
	}
	if resp.Body == nil {
		return nil
	}

	resp.Body = newTimingScanner(resp.Body, func(gen ollama.GenerateResponse) {
		p.recordGenerateMetrics(gen, path)
	})

	return nil
}

// recordGenerateMetrics converts Ollama's nanosecond timing fields into
// histogram observations and derived gauges.
func (p *Proxy) recordGenerateMetrics(gen ollama.GenerateResponse, path string) {
	info := p.mc.Get(context.Background(), gen.Model)
	labels := make([]string, 0, 4)
	labels = append(labels, gen.Model, info.Family, info.Quant)

	const nsToSec = 1e9

	if gen.LoadDuration > 0 {
		p.metrics.LoadDuration.WithLabelValues(labels...).
			Observe(float64(gen.LoadDuration) / nsToSec)
		p.metrics.ModelLoadDurationSeconds.WithLabelValues(gen.Model).
			Observe(float64(gen.LoadDuration) / nsToSec)
	}
	if gen.PromptEvalDuration > 0 {
		p.metrics.PromptEvalDuration.WithLabelValues(labels...).
			Observe(float64(gen.PromptEvalDuration) / nsToSec)
		if gen.PromptEvalCount > 0 {
			tps := float64(gen.PromptEvalCount) / (float64(gen.PromptEvalDuration) / nsToSec)
			p.metrics.PromptTokensPerSec.WithLabelValues(labels...).Set(tps)

			pressure := float64(gen.PromptEvalDuration) / float64(gen.PromptEvalCount)
			p.metrics.KVCachePressureRatio.WithLabelValues(labels...).Set(pressure)
		}
	}
	if gen.EvalDuration > 0 {
		p.metrics.EvalDuration.WithLabelValues(labels...).
			Observe(float64(gen.EvalDuration) / nsToSec)
		if gen.EvalCount > 0 {
			tps := float64(gen.EvalCount) / (float64(gen.EvalDuration) / nsToSec)
			p.metrics.TokensPerSecond.WithLabelValues(labels...).Set(tps)
		}
	}

	p.metrics.RequestsTotal.WithLabelValues(append(labels, path)...).Inc()
}
