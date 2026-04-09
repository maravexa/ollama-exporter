package collector

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/maravexa/ollama-exporter/internal/config"
	"github.com/maravexa/ollama-exporter/internal/metrics"
	"github.com/maravexa/ollama-exporter/internal/ollama"
)

// newLifecyclePoller builds a Poller backed by a handler that can vary its
// /api/ps response between calls via the provided slot.
func newLifecyclePoller(t *testing.T, psHandler func(w http.ResponseWriter, r *http.Request)) (*Poller, *metrics.Metrics) {
	t.Helper()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/":
			w.WriteHeader(http.StatusOK)
		case "/api/ps":
			psHandler(w, r)
		case "/api/show":
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(ollama.ShowResponse{
				Details: ollama.ModelDetails{Family: "llama", QuantizationLevel: "Q4_K_M"},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)

	reg := prometheus.NewRegistry()
	m := metrics.New(reg)
	client := ollama.NewClient(srv.URL)
	mc := NewModelCache(client)
	p := NewPoller(&config.Config{PollInterval: 0}, client, m, mc)
	return p, m
}

// psResponse is a helper to write a JSON /api/ps response.
func psResponse(w http.ResponseWriter, models []ollama.RunningModel) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(ollama.PSResponse{Models: models})
}

// TestLifecycle_ModelAppears_LoadEventFired verifies that when a model is
// absent on the first scrape and appears on the second, the load events
// counter increments and the loaded gauge is set to 1.
func TestLifecycle_ModelAppears_LoadEventFired(t *testing.T) {
	model := ollama.RunningModel{Name: "llama3:8b", SizeVRAM: 4096}
	var callCount atomic.Int32

	p, m := newLifecyclePoller(t, func(w http.ResponseWriter, r *http.Request) {
		if callCount.Add(1) == 1 {
			psResponse(w, nil) // first scrape: empty
		} else {
			psResponse(w, []ollama.RunningModel{model}) // second scrape: model appears
		}
	})

	ctx := context.Background()
	p.scrape(ctx) // startup — empty, initializes state
	p.scrape(ctx) // model appears → load event

	events := testutil.ToFloat64(m.ModelLoadEventsTotal.WithLabelValues("llama3:8b"))
	if events != 1 {
		t.Errorf("model_load_events_total = %v, want 1", events)
	}

	loaded := testutil.ToFloat64(m.ModelLoaded.WithLabelValues("llama3:8b", "llama", "q4_k_m"))
	if loaded != 1 {
		t.Errorf("model_loaded gauge = %v, want 1", loaded)
	}
}

// TestLifecycle_ModelDisappears_UnloadEventFired verifies that when a model
// present on the first scrape is gone on the second, the unload events counter
// increments and the loaded gauge drops to 0.
func TestLifecycle_ModelDisappears_UnloadEventFired(t *testing.T) {
	model := ollama.RunningModel{Name: "llama3:8b", SizeVRAM: 4096}
	var callCount atomic.Int32

	p, m := newLifecyclePoller(t, func(w http.ResponseWriter, r *http.Request) {
		if callCount.Add(1) == 1 {
			psResponse(w, []ollama.RunningModel{model}) // startup: already loaded
		} else {
			psResponse(w, nil) // second scrape: model gone
		}
	})

	ctx := context.Background()
	p.scrape(ctx) // startup — model treated as already loaded
	p.scrape(ctx) // model disappears → unload event

	events := testutil.ToFloat64(m.ModelUnloadEventsTotal.WithLabelValues("llama3:8b"))
	if events != 1 {
		t.Errorf("model_unload_events_total = %v, want 1", events)
	}

	loaded := testutil.ToFloat64(m.ModelLoaded.WithLabelValues("llama3:8b", "llama", "q4_k_m"))
	if loaded != 0 {
		t.Errorf("model_loaded gauge = %v, want 0 after unload", loaded)
	}
}

// TestLifecycle_Startup_NoLoadEventFired verifies that models present in the
// very first /api/ps response do not trigger load events counters (we don't
// know when they were loaded before the exporter started).
func TestLifecycle_Startup_NoLoadEventFired(t *testing.T) {
	model := ollama.RunningModel{Name: "llama3:8b", SizeVRAM: 4096}

	p, m := newLifecyclePoller(t, func(w http.ResponseWriter, r *http.Request) {
		psResponse(w, []ollama.RunningModel{model})
	})

	p.scrape(context.Background()) // startup scrape with model already present

	events := testutil.ToFloat64(m.ModelLoadEventsTotal.WithLabelValues("llama3:8b"))
	if events != 0 {
		t.Errorf("model_load_events_total = %v after startup, want 0 (startup models must not fire event counter)", events)
	}

	// The loaded gauge and cumulative total should still be set.
	loaded := testutil.ToFloat64(m.ModelLoaded.WithLabelValues("llama3:8b", "llama", "q4_k_m"))
	if loaded != 1 {
		t.Errorf("model_loaded gauge = %v after startup, want 1", loaded)
	}
}

// TestLifecycle_VRAMGauge verifies that the VRAM gauge reflects the
// size_vram field from /api/ps and drops to 0 after unload.
func TestLifecycle_VRAMGauge(t *testing.T) {
	const wantVRAM = 8_589_934_592 // 8 GiB
	model := ollama.RunningModel{Name: "llama3:70b", SizeVRAM: wantVRAM}
	var callCount atomic.Int32

	p, m := newLifecyclePoller(t, func(w http.ResponseWriter, r *http.Request) {
		if callCount.Add(1) == 1 {
			psResponse(w, []ollama.RunningModel{model})
		} else {
			psResponse(w, nil) // unloaded
		}
	})

	ctx := context.Background()
	p.scrape(ctx)

	vram := testutil.ToFloat64(m.ModelVRAMBytes.WithLabelValues("llama3:70b", "llama", "q4_k_m"))
	if vram != wantVRAM {
		t.Errorf("model_vram_bytes = %v, want %v", vram, wantVRAM)
	}

	p.scrape(ctx) // model unloads

	vramAfter := testutil.ToFloat64(m.ModelVRAMBytes.WithLabelValues("llama3:70b", "llama", "q4_k_m"))
	if vramAfter != 0 {
		t.Errorf("model_vram_bytes = %v after unload, want 0", vramAfter)
	}
}
