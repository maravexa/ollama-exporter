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

// histSampleCount sums the SampleCount across all label combinations for a
// histogram metric family gathered from reg.
func histSampleCount(t *testing.T, reg *prometheus.Registry, name string) uint64 {
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

	count := histSampleCount(t, reg, "ollama_request_duration_seconds")
	if count != 1 {
		t.Errorf("request_duration_seconds sample count = %d after /api/chat, want 1", count)
	}
}

func TestProxy_ExcludedInternalPath_DoesNotRecordMetrics(t *testing.T) {
	defaultExclude := []string{"/", "/api/ps", "/api/tags", "/api/show", "/api/version"}
	proxy, reg := newTestProxy(t, defaultExclude)

	handler := proxy.instrumentedHandler(noop)

	req := httptest.NewRequest(http.MethodGet, "/api/ps", nil)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	count := histSampleCount(t, reg, "ollama_request_duration_seconds")
	if count != 0 {
		t.Errorf("request_duration_seconds sample count = %d after excluded /api/ps, want 0", count)
	}
}

func TestProxy_CustomExcludedPath_DoesNotRecordMetrics(t *testing.T) {
	customExclude := []string{"/api/custom-health"}
	proxy, reg := newTestProxy(t, customExclude)

	handler := proxy.instrumentedHandler(noop)

	req := httptest.NewRequest(http.MethodGet, "/api/custom-health", nil)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	count := histSampleCount(t, reg, "ollama_request_duration_seconds")
	if count != 0 {
		t.Errorf("request_duration_seconds sample count = %d after custom excluded path, want 0", count)
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
