package collector

import (
	"context"
	"encoding/json"
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/maravexa/ollama-exporter/internal/ollama"
)

func TestPollerDeletedInventory(t *testing.T) {
	var stage atomic.Int32
	p, m := newTestPoller(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s := stage.Load()
		switch r.URL.Path {
		case "/":
		case "/api/ps":
			if s == 7 {
				w.Write([]byte(`{}`))
				return
			}
			if s == 6 {
				http.Error(w, "temporary failure", 500)
				return
			}
			if s == 0 || s == 4 {
				json.NewEncoder(w).Encode(map[string]any{"models": []map[string]any{{"name": "model:latest", "size_vram": 1024}}})
			} else {
				w.Write([]byte(`{"models":[]}`))
			}
		case "/api/tags":
			switch s {
			case 1:
				http.Error(w, "temporary failure", 500)
			case 2:
				w.Write([]byte(`{"models":`))
			case 5:
				w.Write([]byte(`{}`))
			default:
				w.Write([]byte(`{"models":[]}`))
			}
		case "/api/show":
			family := "old"
			if s == 4 {
				family = "new"
			}
			json.NewEncoder(w).Encode(ollama.ShowResponse{Details: ollama.ModelDetails{Family: family, QuantizationLevel: "Q4"}})
		}
	}))
	ctx := context.Background()
	p.scrape(ctx) // A loaded model survives even if tags does not list it.
	m.TokensPerSecond.WithLabelValues("model:latest", "old", "q4").Set(10)
	m.PromptTokensPerSec.WithLabelValues("model:latest", "old", "q4").Set(20)
	m.KVCachePressureRatio.WithLabelValues("model:latest", "old", "q4").Set(30)
	for _, s := range []int32{1, 2, 5, 6, 7} {
		stage.Store(s)
		p.scrape(ctx)
		if n := testutil.CollectAndCount(m.ModelLoaded); n != 1 {
			t.Fatalf("failed tags removed gauge: %d", n)
		}
	}
	stage.Store(3)
	p.scrape(ctx)
	for _, g := range []*prometheus.GaugeVec{m.ModelLoaded, m.ModelVRAMBytes, m.TokensPerSecond, m.PromptTokensPerSec, m.KVCachePressureRatio} {
		if n := testutil.CollectAndCount(g); n != 0 {
			t.Errorf("deleted model still exported: %d series", n)
		}
	}
	p.mc.mu.RLock()
	_, cached := p.mc.cache["model:latest"]
	p.mc.mu.RUnlock()
	if cached {
		t.Error("deleted model metadata retained")
	}
	if n := testutil.ToFloat64(m.ModelUnloadEventsTotal.WithLabelValues("model:latest")); n != 1 {
		t.Errorf("unload events = %v, want 1", n)
	}
	// A request that resolved metadata before deletion may finish afterward.
	m.TokensPerSecond.WithLabelValues("model:latest", "old", "q4").Set(99)
	p.scrape(ctx)
	if n := testutil.CollectAndCount(m.TokensPerSecond); n != 0 {
		t.Errorf("late completion survived next inventory poll: %d", n)
	}
	stage.Store(4)
	p.scrape(ctx)
	if n := testutil.CollectAndCount(m.ModelLoaded); n != 1 {
		t.Errorf("recreated model has stale labels: %d", n)
	}
	if n := testutil.ToFloat64(m.ModelLoaded.WithLabelValues("model:latest", "new", "q4")); n != 1 {
		t.Errorf("new metadata gauge = %v", n)
	}
}

func TestPollerRetainsInstalledUnloadedModel(t *testing.T) {
	var running atomic.Bool
	running.Store(true)
	p, m := newTestPoller(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/":
		case "/api/ps":
			if running.Load() {
				w.Write([]byte(`{"models":[{"name":"kept:latest"}]}`))
			} else {
				w.Write([]byte(`{"models":[]}`))
			}
		case "/api/tags":
			w.Write([]byte(`{"models":[{"name":"kept:latest"}]}`))
		case "/api/show":
			w.Write([]byte(`{"details":{"family":"kept","quantization_level":"Q4"}}`))
		}
	}))
	p.scrape(context.Background())
	running.Store(false)
	p.scrape(context.Background())
	if n := testutil.CollectAndCount(m.ModelLoaded); n != 1 {
		t.Fatalf("installed model lost: %d", n)
	}
	if n := testutil.ToFloat64(m.ModelLoaded.WithLabelValues("kept:latest", "kept", "q4")); n != 0 {
		t.Errorf("unloaded gauge=%v", n)
	}
}
