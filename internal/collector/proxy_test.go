package collector

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	io_prometheus_client "github.com/prometheus/client_model/go"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/maravexa/ollama-exporter/internal/config"
	"github.com/maravexa/ollama-exporter/internal/metrics"
	"github.com/maravexa/ollama-exporter/internal/ollama"
)

// nopReadCloser wraps an io.Reader to implement io.ReadCloser with a no-op Close.
type nopReadCloser struct {
	*bytes.Reader
}

func (nopReadCloser) Close() error { return nil }

// histSampleCount sums the SampleCount across all label combinations for a
// histogram metric family gathered from reg.
func histSampleCount(t *testing.T, reg *prometheus.Registry, name string) uint64 { //nolint:unparam // always receives the histogram metric name at call site
	t.Helper()
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, mf := range mfs {
		if mf.GetName() == name && mf.GetType() == io_prometheus_client.MetricType_HISTOGRAM {
			var total uint64
			for _, m := range mf.GetMetric() {
				total += m.GetHistogram().GetSampleCount()
			}
			return total
		}
	}
	return 0
}

// proxyHistSampleCount is a convenience wrapper for the proxy-side metric.
func proxyHistSampleCount(t *testing.T, reg *prometheus.Registry) uint64 {
	t.Helper()
	return histSampleCount(t, reg, "ollama_proxy_request_duration_seconds")
}

// upstreamHistSampleCount is a convenience wrapper for the upstream-reported metric.
func upstreamHistSampleCount(t *testing.T, reg *prometheus.Registry) uint64 {
	t.Helper()
	return histSampleCount(t, reg, "ollama_upstream_request_duration_seconds")
}

// newTestProxy builds a Proxy with the given exclude paths wired to a noop
// show server (model cache falls back gracefully on 404).
func newTestProxy(t *testing.T, excludePaths []string) (*Proxy, *prometheus.Registry) {
	t.Helper()

	// Show server returns a valid response so model cache labels are populated.
	showSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/show" {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(ollama.ShowResponse{
				Details: ollama.ModelDetails{Family: "llama", QuantizationLevel: "Q4_K_M"},
			})
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(showSrv.Close)

	reg := prometheus.NewRegistry()
	m := metrics.New(reg)
	mc := NewModelCache(ollama.NewClient(showSrv.URL))

	excluded := make(map[string]struct{}, len(excludePaths))
	for _, p := range excludePaths {
		excluded[p] = struct{}{}
	}

	proxy := &Proxy{
		cfg:          &config.Config{},
		metrics:      m,
		mc:           mc,
		excludePaths: excluded,
	}
	return proxy, reg
}

// noop is a trivial upstream handler that always returns 200.
var noop = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
})

func TestProxy_InferencePath_RecordsMetrics(t *testing.T) {
	defaultExclude := []string{"/", "/api/ps", "/api/tags", "/api/show", "/api/version"}
	proxy, reg := newTestProxy(t, defaultExclude)

	handler := proxy.instrumentedHandler(noop)

	body, _ := json.Marshal(map[string]string{"model": "llama3:8b"})
	req := httptest.NewRequest(http.MethodPost, "/api/chat", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	count := proxyHistSampleCount(t, reg)
	if count != 1 {
		t.Errorf("proxy_request_duration_seconds sample count = %d after /api/chat, want 1", count)
	}
	got := upstreamHistSampleCount(t, reg)
	if got != 0 {
		t.Errorf("upstream_request_duration_seconds sample count = %d (should be 0 — no response body inspected in this test)", got)
	}
}

func TestProxy_ExcludedInternalPath_DoesNotRecordMetrics(t *testing.T) {
	defaultExclude := []string{"/", "/api/ps", "/api/tags", "/api/show", "/api/version"}
	proxy, reg := newTestProxy(t, defaultExclude)

	handler := proxy.instrumentedHandler(noop)

	req := httptest.NewRequest(http.MethodGet, "/api/ps", nil)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	got1 := proxyHistSampleCount(t, reg)
	got2 := upstreamHistSampleCount(t, reg)
	if got1 != 0 {
		t.Errorf("proxy_request_duration_seconds sample count = %d (want 0)", got1)
	}
	if got2 != 0 {
		t.Errorf("upstream_request_duration_seconds sample count = %d (want 0)", got2)
	}
}

func TestProxy_CustomExcludedPath_DoesNotRecordMetrics(t *testing.T) {
	customExclude := []string{"/api/custom-health"}
	proxy, reg := newTestProxy(t, customExclude)

	handler := proxy.instrumentedHandler(noop)

	req := httptest.NewRequest(http.MethodGet, "/api/custom-health", nil)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	got1 := proxyHistSampleCount(t, reg)
	got2 := upstreamHistSampleCount(t, reg)
	if got1 != 0 {
		t.Errorf("proxy_request_duration_seconds sample count = %d (want 0)", got1)
	}
	if got2 != 0 {
		t.Errorf("upstream_request_duration_seconds sample count = %d (want 0)", got2)
	}
}

func TestProxy_ExcludedPath_StillProxiesRequest(t *testing.T) {
	// Ensure excluded paths are still forwarded to the upstream (not dropped).
	defaultExclude := []string{"/", "/api/ps", "/api/tags", "/api/show", "/api/version"}
	proxy, _ := newTestProxy(t, defaultExclude)

	handler := proxy.instrumentedHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTeapot) // distinctive sentinel value
	}))

	req := httptest.NewRequest(http.MethodGet, "/api/ps", nil)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusTeapot {
		t.Errorf("upstream status = %d, want %d (request must still be proxied)", rr.Code, http.StatusTeapot)
	}
}

// v1UsageBody constructs a JSON byte slice for an Ollama /v1/* usage response.
func v1UsageBody(id, obj, model string, promptTokens, completionTokens, totalTime, promptTime, completionTime int64) []byte {
	b, _ := json.Marshal(map[string]interface{}{
		"id":     id,
		"object": obj,
		"model":  model,
		"usage": map[string]interface{}{
			"prompt_tokens":     promptTokens,
			"completion_tokens": completionTokens,
			"total_tokens":      promptTokens + completionTokens,
			"total_time":        float64(totalTime),
			"prompt_time":       float64(promptTime),
			"completion_time":   float64(completionTime),
		},
	})
	return b
}

// makeV1Resp builds a fake http.Response whose Body contains the given v1-style JSON.
func makeV1Resp(path string, body []byte) *http.Response {
	return &http.Response{
		Status:        "200 OK",
		StatusCode:    http.StatusOK,
		Body:          nopReadCloser{bytes.NewReader(body)},
		ContentLength: int64(len(body)),
		Request:       httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body)),
	}
}

func TestProxy_V1ChatCompletionPath_RecordsMetrics(t *testing.T) {
	defaultExclude := []string{"/", "/api/ps", "/api/tags", "/api/show", "/api/version"}
	proxy, reg := newTestProxy(t, defaultExclude)

	reqBody, _ := json.Marshal(map[string]string{"model": "llama3:8b"})
	handler := proxy.instrumentedHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(reqBody)))

	count := proxyHistSampleCount(t, reg)
	if count != 1 {
		t.Errorf("proxy_request_duration_seconds sample count = %d, want 1", count)
	}

	resp := makeV1Resp("/v1/chat/completions", v1UsageBody("chatcmpl-123", "chat.completion", "llama3:8b", 10, 50, 2000, 300, 1700))

	if err := proxy.modifyResponse(resp); err != nil {
		t.Fatalf("modifyResponse: %v", err)
	}

	defer resp.Body.Close()

	gotProxy := proxyHistSampleCount(t, reg)
	gotUpstream := upstreamHistSampleCount(t, reg)
	if gotProxy != 1 { // only the handler fired once
		t.Errorf("proxy_request_duration_seconds sample count = %d, want 1", gotProxy)
	}
	if gotUpstream != 1 { // only modifyResponse recorded this
		t.Errorf("upstream_request_duration_seconds sample count = %d, want 1", gotUpstream)
	}

	// Verify tokens_per_second was set.
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	foundTPS := false
	for _, mf := range mfs {
		if mf.GetName() == "ollama_tokens_per_second" {
			foundTPS = true
			got := mf.GetMetric()[0].GetGauge().GetValue()
			want := float64(50) / 1.7 // completion_tokens / completion_time_in_seconds
			if got != want {
				t.Errorf("tokens_per_second = %f, want %f", got, want)
			}
		}
	}
	if !foundTPS {
		t.Error("ollama_tokens_per_second not found in metrics")
	}
}

func TestProxy_V1CompletionsPath_RecordsMetrics(t *testing.T) {
	defaultExclude := []string{"/", "/api/ps", "/api/tags", "/api/show", "/api/version"}
	proxy, reg := newTestProxy(t, defaultExclude)

	reqBody, _ := json.Marshal(map[string]string{"model": "mistral:7b"})
	handler := proxy.instrumentedHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/v1/completions", bytes.NewReader(reqBody)))

	resp := makeV1Resp("/v1/completions", v1UsageBody("cmpl-456", "text_completion", "mistral:7b", 5, 100, 5000, 200, 4800))

	if err := proxy.modifyResponse(resp); err != nil {
		t.Fatalf("modifyResponse: %v", err)
	}

	defer resp.Body.Close()

	gotProxy := proxyHistSampleCount(t, reg)
	gotUpstream := upstreamHistSampleCount(t, reg)
	if gotProxy != 1 {
		t.Errorf("proxy_request_duration_seconds sample count = %d, want 1", gotProxy)
	}
	if gotUpstream != 1 {
		t.Errorf("upstream_request_duration_seconds sample count = %d, want 1", gotUpstream)
	}
}

func TestProxy_V1StreamResponse_DoesNotBreakProxy(t *testing.T) {
	defaultExclude := []string{"/", "/api/ps", "/api/tags", "/api/show", "/api/version"}
	proxy, reg := newTestProxy(t, defaultExclude)

	handler := proxy.instrumentedHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	reqBody, _ := json.Marshal(map[string]string{"model": "llama3:8b"})
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(reqBody)))

	gotProxy := proxyHistSampleCount(t, reg)
	if gotProxy != 1 {
		t.Errorf("proxy_request_duration_seconds sample count = %d, want 1", gotProxy)
	}

	// Streaming SSE chunk has no usage field — JSON parse should fail gracefully.
	sseBody, _ := json.Marshal(map[string]interface{}{
		"id":      "chatcmpl-123",
		"object":  "chat.completion.chunk",
		"choices": []map[string]interface{}{{"delta": map[string]string{"content": "hello"}}},
	})

	resp := makeV1Resp("/v1/chat/completions", sseBody)

	if err := proxy.modifyResponse(resp); err != nil {
		t.Fatalf("modifyResponse on SSE chunk returned error: %v", err)
	}

	defer resp.Body.Close()

	gotUpstreamAfter := upstreamHistSampleCount(t, reg)
	if gotUpstreamAfter != 0 { // should remain unchanged — no usage in SSE chunks
		t.Errorf("upstream_request_duration_seconds sample count = %d (want 0 — SSE has no usage)", gotUpstreamAfter)
	}
	gotProxyAfter := proxyHistSampleCount(t, reg)
	if gotProxyAfter != gotProxy { // should remain unchanged
		t.Errorf("proxy_request_duration_seconds sample count after SSE = %d, want %d (no change)", gotProxyAfter, gotProxy)
	}
}

func TestProxy_V1MissingModel_Fallback(t *testing.T) {
	defaultExclude := []string{"/", "/api/ps", "/api/tags", "/api/show", "/api/version"}
	proxy, reg := newTestProxy(t, defaultExclude)

	handler := proxy.instrumentedHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	// Request body has no "model" field.
	reqBody, _ := json.Marshal(map[string]string{"prompt": "hello"})
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(reqBody)))

	gotProxy := proxyHistSampleCount(t, reg)
	if gotProxy != 1 {
		t.Errorf("proxy_request_duration_seconds sample count = %d, want 1", gotProxy)
	}

	resp := makeV1Resp("/v1/chat/completions", v1UsageBody("chatcmpl-123", "chat.completion", "", 5, 20, 1000, 100, 900))

	if err := proxy.modifyResponse(resp); err != nil {
		t.Fatalf("modifyResponse: %v", err)
	}

	defer resp.Body.Close()

	gotUpstream := upstreamHistSampleCount(t, reg)
	if gotUpstream != 1 { // model falls back to "_" when request/response both lack it
		t.Errorf("upstream_request_duration_seconds sample count = %d (want 1 — model falls back to _)", gotUpstream)
	}
	gotProxyAfter := proxyHistSampleCount(t, reg)
	if gotProxyAfter != gotProxy { // should remain unchanged after modifyResponse
		t.Errorf("proxy_request_duration_seconds count changed to %d after modifyResponse (want %d)", gotProxyAfter, gotProxy)
	}
}

func TestProxy_V1ArbitraryPath_PrefixMatch(t *testing.T) {
	defaultExclude := []string{"/", "/api/ps", "/api/tags", "/api/show", "/api/version"}
	proxy, reg := newTestProxy(t, defaultExclude)

	reqBody, _ := json.Marshal(map[string]string{"model": "nomic-embed-text"})
	handler := proxy.instrumentedHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	// Hit an arbitrary /v1/embeddings path - should be matched by HasPrefix.
	path := "/v1/embeddings"
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, path, bytes.NewReader(reqBody)))

	gotProxy := proxyHistSampleCount(t, reg)
	if gotProxy != 1 {
		t.Errorf("proxy_request_duration_seconds sample count = %d, want 1 (handler should match /v1/* prefix)", gotProxy)
	}

	// Now send a v1 response with usage data via modifyResponse.
	resp := makeV1Resp(path, v1UsageBody("emb-789", "list_embeddings", "nomic-embed-text", 12, 30, 1500, 250, 1250))

	if err := proxy.modifyResponse(resp); err != nil {
		t.Fatalf("modifyResponse: %v", err)
	}

	defer resp.Body.Close()

	gotUpstream := upstreamHistSampleCount(t, reg)
	if gotUpstream != 1 { // modifyResponse recorded this
		t.Errorf("upstream_request_duration_seconds sample count = %d, want 1", gotUpstream)
	}

	// Verify model label is correctly extracted as "nomic-embed-text".
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	foundUpstream := false
	for _, mf := range mfs {
		if mf.GetName() == "ollama_upstream_request_duration_seconds" {
			for _, m := range mf.GetMetric() {
				labels := m.GetLabel()
				hasModel := false
				modelName := ""
				for _, l := range labels {
					if l.GetName() == "model" {
						hasModel = true
						modelName = l.GetValue()
					}
				}
				if hasModel && modelName != "nomic-embed-text" {
					t.Errorf("model label = %q, want nomic-embed-text", modelName)
				}
			}
			foundUpstream = true
		}
	}
	if !foundUpstream {
		t.Error("ollama_upstream_request_duration_seconds not found in metrics")
	}
}
