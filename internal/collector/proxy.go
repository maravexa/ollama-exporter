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
	"strings"
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
			(endpoint == "/api/generate" || endpoint == "/api/chat" || strings.HasPrefix(endpoint, "/v1/")) &&
			r.Body != nil {
			bodyBytes, err := io.ReadAll(r.Body)
			if err != nil {
				slog.Debug("proxy: failed to read request body", "err", err)
			} else if len(bodyBytes) > 0 {
				r.Body = io.NopCloser(bytes.NewReader(bodyBytes))
				r.ContentLength = int64(len(bodyBytes))

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
		p.metrics.ProxyRequestDurationSeconds.
			WithLabelValues(model, info.Family, info.Quant, endpoint).
			Observe(time.Since(start).Seconds())
	})
}

// inferenceRequest holds the parsed timing fields extracted from an upstream response.
type inferenceRequest struct {
	DurationSeconds  float64
	LoadDuration     float64
	PromptDuration   float64
	EvalDuration     float64
	PromptEvalNs     int64 // nanoseconds for KV cache pressure derivation
	PromptTokens     int64
	CompletionTokens int64
}

// modifyResponse intercepts the upstream response, reads Ollama's timing
// fields from the JSON body, records histogram observations, then restores
// the body so the downstream client receives an unmodified response.
func (p *Proxy) modifyResponse(resp *http.Response) error {
	if resp.Request.Method != http.MethodPost {
		return nil
	}
	path := resp.Request.URL.Path
	if (path != "/api/generate" && path != "/api/chat" && !strings.HasPrefix(path, "/v1/")) || resp.Body == nil {
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

	var parsed inferenceRequest
	var model string
	var info ModelInfo

	switch {
	case path == "/api/generate" || path == "/api/chat":
		model, info, parsed, err = p.parseNative(body)
	case strings.HasPrefix(path, "/v1/"):
		model, info, parsed, err = p.parseOpenAI(body)
	default:
		return nil // no instrumentation for this endpoint
	}

	if err != nil {
		return err
	} else if model == "" || (parsed.DurationSeconds == 0 && parsed.CompletionTokens == 0) {
		return nil // skip metrics when no meaningful data to record
	}

	modelLabels := make([]string, 0, 4)
	modelLabels = append(modelLabels, model, info.Family, info.Quant)
	endpointLabels := append(modelLabels, path)

	p.metrics.UpstreamRequestDurationSeconds.WithLabelValues(endpointLabels...).Observe(parsed.DurationSeconds)
	if parsed.LoadDuration > 0 {
		p.metrics.LoadDuration.WithLabelValues(modelLabels...).Observe(parsed.LoadDuration)
		p.metrics.ModelLoadDurationSeconds.WithLabelValues(model).Observe(parsed.LoadDuration)
	}
	if parsed.PromptDuration > 0 {
		p.metrics.PromptEvalDuration.WithLabelValues(modelLabels...).Observe(parsed.PromptDuration)
	}
	if parsed.EvalDuration > 0 {
		p.metrics.EvalDuration.WithLabelValues(modelLabels...).Observe(parsed.EvalDuration)
	}
	if parsed.PromptTokens > 0 && parsed.PromptDuration > 0 {
		p.metrics.PromptTokensPerSec.
			WithLabelValues(modelLabels...).Set(float64(parsed.PromptTokens) / parsed.PromptDuration)
	}
	if parsed.CompletionTokens > 0 && parsed.EvalDuration > 0 {
		p.metrics.TokensPerSecond.
			WithLabelValues(modelLabels...).Set(float64(parsed.CompletionTokens) / parsed.EvalDuration)
	}
	// KV cache pressure uses nanoseconds per token (native API only; zero for v1).
	if parsed.PromptEvalNs > 0 && parsed.PromptTokens > 0 {
		p.metrics.KVCachePressureRatio.WithLabelValues(modelLabels...).Set(
			float64(parsed.PromptEvalNs) / float64(parsed.PromptTokens),
		)
	}

	p.metrics.RequestsTotal.WithLabelValues(endpointLabels...).Inc()

	return nil
}

// parseNative parses timing fields from a native /api/generate or /api/chat response.
func (p *Proxy) parseNative(body []byte) (model string, info ModelInfo, parsed inferenceRequest, err error) {
	var gen ollama.GenerateResponse
	if err = json.Unmarshal(body, &gen); err != nil {
		return //nolint:nilerr // non-generate/streaming responses must not fail the proxy
	}
	if !gen.Done {
		return "", ModelInfo{}, inferenceRequest{}, nil
	}

	model = gen.Model
	info = p.mc.Get(context.Background(), model)

	const nsToSec = 1e9
	parsed.DurationSeconds = float64(gen.TotalDuration) / nsToSec
	if gen.LoadDuration > 0 {
		parsed.LoadDuration = float64(gen.LoadDuration) / nsToSec
	}
	if gen.PromptEvalDuration > 0 {
		parsed.PromptDuration = float64(gen.PromptEvalDuration) / nsToSec
		parsed.PromptEvalNs = gen.PromptEvalDuration
		parsed.PromptTokens = gen.PromptEvalCount
	}
	if gen.EvalDuration > 0 {
		parsed.EvalDuration = float64(gen.EvalDuration) / nsToSec
		parsed.CompletionTokens = gen.EvalCount
	}

	return model, info, parsed, nil
}

// parseOpenAI parses timing fields from an OpenAI-compatible /v1/* response.
func (p *Proxy) parseOpenAI(body []byte) (model string, info ModelInfo, parsed inferenceRequest, err error) {
	var openaiResp ollama.OpenAIChatCompletionResponse
	if err = json.Unmarshal(body, &openaiResp); err != nil {
		return
	}

	usage := openaiResp.Usage
	msToSec := 1e3 // convert milliseconds to seconds

	parsed.PromptDuration = float64(usage.PromptTime) / msToSec
	parsed.EvalDuration = float64(usage.CompletionTime) / msToSec
	parsed.DurationSeconds = float64(usage.TotalTime) / msToSec
	parsed.PromptTokens = usage.PromptTokens
	parsed.CompletionTokens = usage.CompletionTokens

	// Model name must come from the request body — not all v1 responses include it.
	var reqModel string
	if len(body) > 0 {
		var req struct {
			Model string `json:"model"`
		}
		if json.Unmarshal(body, &req) == nil && req.Model != "" {
			reqModel = req.Model
		}
	}
	if reqModel == "" {
		reqModel = "_" // Prometheus convention for missing label values
	}

	info = p.mc.Get(context.Background(), reqModel)
	return reqModel, info, parsed, nil
}
