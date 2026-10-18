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

	rp := p.reverseProxy(target)

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

// reverseProxy centralizes production response copying and flushing settings.
func (p *Proxy) reverseProxy(target *url.URL) *httputil.ReverseProxy {
	rp := httputil.NewSingleHostReverseProxy(target)
	rp.ModifyResponse = p.modifyResponse
	rp.FlushInterval = -1
	return rp
}

// instrumentedHandler wraps the reverse proxy to track in-flight requests
// and handling latency around response forwarding.
// Requests whose path appears in p.excludePaths are proxied normally but
// generate no Prometheus observations, preventing internal polling calls
// from polluting inference histograms.
func (p *Proxy) instrumentedHandler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		model := r.URL.Query().Get("model")
		endpoint := r.URL.Path
		start := time.Now()
		var requestReadErr error

		if r.Method == http.MethodPost &&
			inferencePath(endpoint) &&
			r.Body != nil {
			body := &eofReader{Reader: r.Body}
			bodyBytes, err := io.ReadAll(io.LimitReader(body, 64<<10))
			if err != nil {
				slog.Debug("proxy: failed to read request body", "err", err)
				_ = r.Body.Close()
				requestReadErr = err
			}
			r.Body = &prefixBody{Reader: io.MultiReader(bytes.NewReader(bodyBytes), body), source: r.Body}
			if len(bodyBytes) > 0 {
				var req struct {
					Model string `json:"model"`
				}
				fields := jsonFields{}
				fields.feed(bodyBytes)
				if json.Unmarshal(fields.fields["model"], &req.Model) == nil && req.Model != "" {
					model = req.Model
				}
			}
		}

		_, excluded := p.excludePaths[endpoint]
		if excluded {
			if requestReadErr != nil {
				http.Error(w, "failed to read request body", http.StatusBadRequest)
				return
			}
			next.ServeHTTP(w, r)
			return
		}

		p.metrics.RequestsInFlight.WithLabelValues(model, endpoint).Inc()
		defer p.metrics.RequestsInFlight.WithLabelValues(model, endpoint).Dec()

		defer func() {
			elapsed := time.Since(start).Seconds()
			info := p.mc.Get(context.Background(), model)
			p.metrics.RequestDuration.WithLabelValues(model, info.Family, info.Quant, endpoint).Observe(elapsed)
		}()
		if requestReadErr != nil {
			http.Error(w, "failed to read request body", http.StatusBadRequest)
			return
		}
		r = r.WithContext(context.WithValue(r.Context(), requestModelKey{}, model))
		next.ServeHTTP(w, r)
	})
}

// eofReader avoids rereading an exhausted inbound body after net/http has
// closed it while flushing a response. The transport may check for excess
// request bytes after it has already received the upstream response.
type eofReader struct {
	io.Reader
	eof bool
}

func (r *eofReader) Read(p []byte) (int, error) {
	if r.eof {
		return 0, io.EOF
	}
	n, err := r.Reader.Read(p)
	r.eof = err == io.EOF
	return n, err
}

type prefixBody struct {
	io.Reader
	source io.Closer
}

func (b *prefixBody) Close() error { return b.source.Close() }

type requestModelKey struct{}

func inferencePath(path string) bool {
	switch path {
	case "/api/generate", "/api/chat", "/v1/chat/completions", "/v1/completions", "/v1/messages":
		return true
	}
	return false
}

// modifyResponse installs a byte-transparent observer without changing framing.
func (p *Proxy) modifyResponse(resp *http.Response) error {
	path := resp.Request.URL.Path
	if resp.Request.Method != http.MethodPost || !inferencePath(path) {
		return nil
	}
	if _, excluded := p.excludePaths[path]; excluded {
		return nil
	}
	report := func(outcome, reason string) { p.metrics.ExtractionResults.WithLabelValues(outcome, reason).Inc() }
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		report("skipped", "http_status")
		return nil
	}
	compatibility := strings.HasPrefix(path, "/v1/")
	var body *responseBody
	body = newResponseBody(resp.Body, compatibility && strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream"), func(f map[string]json.RawMessage) {
		if compatibility {
			outcome, reason := p.recordUsage(resp.Request, path, f)
			body.report(outcome, reason)
			return
		}
		var gen ollama.GenerateResponse
		data, err := json.Marshal(f)
		if err != nil || json.Unmarshal(data, &gen) != nil {
			body.report("failed", "invalid_fields")
			return
		}
		if !gen.Done {
			body.report("skipped", "incomplete")
			return
		}
		p.recordGenerateMetrics(gen, path)
		body.report("success", "none")
	})
	body.result = report
	body.notice = func() { p.metrics.ExtractionFieldSkips.WithLabelValues("field_name_limit").Inc() }
	resp.Body = body
	return nil
}

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
