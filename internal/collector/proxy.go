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
	cfg     *config.Config
	metrics *metrics.Metrics
	server  *http.Server
}

// NewProxy constructs a Proxy.
func NewProxy(cfg *config.Config, _ *ollama.Client, m *metrics.Metrics) *Proxy {
	return &Proxy{
		cfg:     cfg,
		metrics: m,
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

	mux := http.NewServeMux()
	mux.Handle("/", p.instrumentedHandler(rp))

	p.server = &http.Server{
		Addr:    p.cfg.Proxy.ListenAddr,
		Handler: mux,
	}

	go func() {
		<-ctx.Done()
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = p.server.Shutdown(shutCtx)
	}()

	slog.Info("proxy listening", "addr", p.cfg.Proxy.ListenAddr, "upstream", p.cfg.OllamaURL)
	if err := p.server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

// instrumentedHandler wraps the reverse proxy to track in-flight requests
// and end-to-end latency before the response body is inspected.
func (p *Proxy) instrumentedHandler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		model := r.URL.Query().Get("model") // best-effort; body parsing happens in modifyResponse
		endpoint := r.URL.Path

		p.metrics.RequestsInFlight.WithLabelValues(model, endpoint).Inc()
		defer p.metrics.RequestsInFlight.WithLabelValues(model, endpoint).Dec()

		start := time.Now()
		next.ServeHTTP(w, r)

		family, quant := parseModelName(model)
		p.metrics.RequestDuration.
			WithLabelValues(model, family, quant, endpoint).
			Observe(time.Since(start).Seconds())
	})
}

// modifyResponse intercepts the upstream response, reads Ollama's timing
// fields from the JSON body, records histogram observations, then restores
// the body so the downstream client receives an unmodified response.
func (p *Proxy) modifyResponse(resp *http.Response) error {
	if resp.Request.Method != http.MethodPost {
		return nil
	}
	path := resp.Request.URL.Path
	if path != "/api/generate" && path != "/api/chat" {
		return nil
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	resp.Body = io.NopCloser(bytes.NewReader(body))

	var gen ollama.GenerateResponse
	if err := json.Unmarshal(body, &gen); err != nil || !gen.Done {
		return nil
	}

	family, quant := parseModelName(gen.Model)
	labels := []string{gen.Model, family, quant}

	const nsToSec = 1e9

	if gen.LoadDuration > 0 {
		p.metrics.LoadDuration.WithLabelValues(labels...).
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

	return nil
}
