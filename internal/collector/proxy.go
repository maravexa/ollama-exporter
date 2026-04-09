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

	mux := http.NewServeMux()
	mux.Handle("/", p.instrumentedHandler(rp))

	p.server = &http.Server{
		Addr:              p.cfg.Proxy.ListenAddr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		<-ctx.Done()
		//nolint:gosec // G118: shutdown context must be independent of the already-cancelled parent
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
	resp.Body.Close()
	if err != nil {
		return err
	}

	// Always restore body and fix framing headers so the downstream client
	// receives correct Content-Length regardless of whether the upstream used
	// chunked transfer encoding or a fixed content-length.
	resp.Body = io.NopCloser(bytes.NewReader(body))
	resp.ContentLength = int64(len(body))
	resp.TransferEncoding = nil // clear chunked encoding if set

	var gen ollama.GenerateResponse
	if err := json.Unmarshal(body, &gen); err != nil {
		return nil // non-Ollama or non-generate response; skip metric recording
	}
	if !gen.Done {
		return nil
	}

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

	return nil
}
