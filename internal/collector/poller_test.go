package collector

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/maravexa/ollama-exporter/internal/config"
	"github.com/maravexa/ollama-exporter/internal/metrics"
	"github.com/maravexa/ollama-exporter/internal/ollama"
)

// newTestPoller builds a Poller wired to a mock Ollama server.
// The handler is responsible for serving /, /api/ps, and /api/show.
func newTestPoller(t *testing.T, handler http.Handler) (*Poller, *metrics.Metrics) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	reg := prometheus.NewRegistry()
	m := metrics.New(reg)
	client := ollama.NewClient(srv.URL)
	mc := NewModelCache(client)

	cfg := &config.Config{}
	p := NewPoller(cfg, client, m, mc)
	return p, m
}

func TestPoller_ModelLoad_IncrementsCounter(t *testing.T) {
	model := ollama.RunningModel{
		Name:     "llama3.1:8b",
		SizeVRAM: 1024,
	}
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/":
			w.WriteHeader(http.StatusOK)
		case "/api/ps":
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(ollama.PSResponse{Models: []ollama.RunningModel{model}})
		case "/api/show":
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(ollama.ShowResponse{
				Details: ollama.ModelDetails{Family: "llama", QuantizationLevel: "Q4_K_M"},
			})
		default:
			http.NotFound(w, r)
		}
	})

	p, m := newTestPoller(t, handler)

	// First scrape: model appears → load counter increments.
	p.scrape(context.Background())

	count := testutil.ToFloat64(m.ModelLoaded.WithLabelValues("llama3.1:8b", "llama", "q4_k_m"))
	if count != 1 {
		t.Errorf("ModelLoaded = %v, want 1", count)
	}

	loadTotal := testutil.ToFloat64(m.ModelLoadTotal.WithLabelValues("llama3.1:8b", "llama", "q4_k_m"))
	if loadTotal != 1 {
		t.Errorf("ModelLoadTotal = %v, want 1", loadTotal)
	}
}

func TestPoller_ModelUnload_IncrementsCounter(t *testing.T) {
	model := ollama.RunningModel{
		Name:     "llama3.1:8b",
		SizeVRAM: 1024,
	}

	// First call returns the model; second call returns empty.
	callCount := 0
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/":
			w.WriteHeader(http.StatusOK)
		case "/api/ps":
			w.Header().Set("Content-Type", "application/json")
			callCount++
			if callCount == 1 {
				json.NewEncoder(w).Encode(ollama.PSResponse{Models: []ollama.RunningModel{model}})
			} else {
				json.NewEncoder(w).Encode(ollama.PSResponse{Models: []ollama.RunningModel{}})
			}
		case "/api/show":
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(ollama.ShowResponse{
				Details: ollama.ModelDetails{Family: "llama", QuantizationLevel: "Q4_K_M"},
			})
		default:
			http.NotFound(w, r)
		}
	})

	p, m := newTestPoller(t, handler)

	// First scrape: model loaded.
	p.scrape(context.Background())
	// Second scrape: model gone → unload counter increments.
	p.scrape(context.Background())

	unloadTotal := testutil.ToFloat64(m.ModelUnloadTotal.WithLabelValues("llama3.1:8b", "llama", "q4_k_m"))
	if unloadTotal != 1 {
		t.Errorf("ModelUnloadTotal = %v, want 1", unloadTotal)
	}
}

func TestProxy_TPSDerivation(t *testing.T) {
	// eval_count=100, eval_duration=2_000_000_000 ns (2s) → TPS = 50.0
	gen := ollama.GenerateResponse{
		Model:        "llama3.1:8b",
		Done:         true,
		EvalCount:    100,
		EvalDuration: 2_000_000_000,
	}
	body, _ := json.Marshal(gen)

	// Build a fake upstream response with the generate JSON.
	fakeResp := &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader(string(body) + "\n")),
		Request: &http.Request{
			Method: http.MethodPost,
			URL:    &url.URL{Path: "/api/generate"},
		},
	}

	// Mock show server so the model cache resolves labels.
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
	defer showSrv.Close()

	reg := prometheus.NewRegistry()
	m := metrics.New(reg)
	mc := NewModelCache(ollama.NewClient(showSrv.URL))

	proxy := &Proxy{
		cfg:     &config.Config{},
		metrics: m,
		mc:      mc,
	}

	if err := proxy.modifyResponse(fakeResp); err != nil {
		t.Fatalf("modifyResponse error: %v", err)
	}

	tps := testutil.ToFloat64(m.TokensPerSecond.WithLabelValues("llama3.1:8b", "llama", "q4_k_m"))
	if tps != 50.0 {
		t.Errorf("tokens_per_second = %v, want 50.0", tps)
	}
}
