package collector

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/maravexa/ollama-exporter/internal/ollama"
)

func newMockShowServer(t *testing.T, resp ollama.ShowResponse, callCount *atomic.Int32) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/show" && r.Method == http.MethodPost {
			callCount.Add(1)
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(resp)
			return
		}
		http.NotFound(w, r)
	}))
}

func TestModelCache_CacheMiss(t *testing.T) {
	var calls atomic.Int32
	srv := newMockShowServer(t, ollama.ShowResponse{
		Details: ollama.ModelDetails{
			Family:            "llama",
			QuantizationLevel: "Q4_K_M",
		},
	}, &calls)
	defer srv.Close()

	mc := NewModelCache(ollama.NewClient(srv.URL))
	info := mc.Get(context.Background(), "llama3.1:8b-q4_k_m")

	if calls.Load() != 1 {
		t.Errorf("Show called %d times, want 1", calls.Load())
	}
	if info.Family != "llama" {
		t.Errorf("family = %q, want %q", info.Family, "llama")
	}
}

func TestModelCache_CacheHit(t *testing.T) {
	var calls atomic.Int32
	srv := newMockShowServer(t, ollama.ShowResponse{
		Details: ollama.ModelDetails{
			Family:            "llama",
			QuantizationLevel: "Q4_K_M",
		},
	}, &calls)
	defer srv.Close()

	mc := NewModelCache(ollama.NewClient(srv.URL))
	ctx := context.Background()

	mc.Get(ctx, "llama3.1:8b-q4_k_m")
	mc.Get(ctx, "llama3.1:8b-q4_k_m")

	if calls.Load() != 1 {
		t.Errorf("Show called %d times on second access, want 1 (cache hit)", calls.Load())
	}
}

func TestModelCache_ShowError_Fallback(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unavailable", http.StatusInternalServerError)
	}))
	defer srv.Close()

	mc := NewModelCache(ollama.NewClient(srv.URL))
	info := mc.Get(context.Background(), "llama3.1:8b-q4_0")

	if info.Quant != "unknown" {
		t.Errorf("quant = %q, want %q on show error", info.Quant, "unknown")
	}
	// Family falls back to parseModelName
	if info.Family != "llama3" {
		t.Errorf("family = %q, want %q on show error fallback", info.Family, "llama3")
	}
}

func TestModelCache_QuantNormalization(t *testing.T) {
	var calls atomic.Int32
	srv := newMockShowServer(t, ollama.ShowResponse{
		Details: ollama.ModelDetails{
			Family:            "llama",
			QuantizationLevel: "Q4_K_M",
		},
	}, &calls)
	defer srv.Close()

	mc := NewModelCache(ollama.NewClient(srv.URL))
	info := mc.Get(context.Background(), "llama3.1:8b-q4_k_m")

	if info.Quant != "q4_k_m" {
		t.Errorf("quant = %q, want %q (lowercase normalized)", info.Quant, "q4_k_m")
	}
}
