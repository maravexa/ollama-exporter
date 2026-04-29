// Package e2e contains end-to-end tests that exercise the full remote
// write path: a real Pusher draining a real Queue through the wire layer
// into a mock receiver, with metrics surfaced on a live registry.
package e2e

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/prometheus/prompb"

	"github.com/maravexa/ollama-exporter/internal/remotewrite"
	rwtest "github.com/maravexa/ollama-exporter/internal/remotewrite/testutil"
)

func TestE2E_PushDeliversSeries(t *testing.T) {
	receiver := rwtest.New()
	defer receiver.Close()

	reg := prometheus.NewRegistry()
	gauge := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "ollama_e2e",
		Name:      "static_signal",
		Help:      "test signal that ticks each loop",
	}, []string{"flow"})
	reg.MustRegister(gauge)
	gauge.WithLabelValues("default").Set(1)

	pusher, err := remotewrite.Build(remotewrite.BuildOptions{
		Gatherer:   reg,
		Registerer: reg,
		Endpoints: []remotewrite.EndpointBuildSpec{{
			Name:         "primary",
			URL:          receiver.URL,
			InsecureHTTP: true,
			Timeout:      2 * time.Second,
			Retry: remotewrite.RetryBuildSpec{
				MaxAttempts:    3,
				MaxElapsed:     5 * time.Second,
				InitialBackoff: 5 * time.Millisecond,
				MaxBackoff:     50 * time.Millisecond,
			},
			Queue: remotewrite.QueueBuildSpec{Capacity: 100},
		}},
		FlushInterval: 50 * time.Millisecond,
		DrainTimeout:  500 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 800*time.Millisecond)
	defer cancel()
	go func() {
		_ = pusher.Run(ctx)
	}()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if seriesByName(receiver.Received(), "ollama_e2e_static_signal") > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	if got := seriesByName(receiver.Received(), "ollama_e2e_static_signal"); got == 0 {
		t.Fatalf("receiver never observed ollama_e2e_static_signal (request_count=%d)", receiver.RequestCount())
	}

	if v := readCounter(t, reg, "ollama_exporter_remote_write_samples_total", nil); v == 0 {
		t.Errorf("ollama_exporter_remote_write_samples_total is 0; expected > 0")
	}
}

func TestE2E_RecoversFromTransientOutage(t *testing.T) {
	receiver := rwtest.New()
	defer receiver.Close()

	receiver.FailNTimes(15, http.StatusServiceUnavailable)

	reg := prometheus.NewRegistry()
	g := prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: "ollama_e2e", Name: "outage_signal", Help: "test signal",
	})
	reg.MustRegister(g)
	g.Set(42)

	pusher, err := remotewrite.Build(remotewrite.BuildOptions{
		Gatherer:   reg,
		Registerer: reg,
		Endpoints: []remotewrite.EndpointBuildSpec{{
			Name:         "primary",
			URL:          receiver.URL,
			InsecureHTTP: true,
			Timeout:      500 * time.Millisecond,
			Retry: remotewrite.RetryBuildSpec{
				MaxAttempts:    50,
				MaxElapsed:     10 * time.Second,
				InitialBackoff: 5 * time.Millisecond,
				MaxBackoff:     100 * time.Millisecond,
			},
			Queue: remotewrite.QueueBuildSpec{Capacity: 1000},
		}},
		FlushInterval: 100 * time.Millisecond,
		DrainTimeout:  500 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()
	go func() { _ = pusher.Run(ctx) }()

	deadline := time.Now().Add(8 * time.Second)
	delivered := false
	for time.Now().Before(deadline) {
		if seriesByName(receiver.Received(), "ollama_e2e_outage_signal") > 0 {
			delivered = true
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !delivered {
		t.Fatalf("series never delivered after outage; requests=%d", receiver.RequestCount())
	}

	dropped := readCounter(t, reg, "ollama_exporter_remote_write_samples_dropped_total",
		map[string]string{"reason": "queue_full"})
	if dropped != 0 {
		t.Errorf("samples_dropped[queue_full] = %v, want 0 — capacity should have absorbed the outage", dropped)
	}
}

// seriesByName counts series in batch whose __name__ label equals name.
func seriesByName(batch []prompb.TimeSeries, name string) int {
	count := 0
	for _, s := range batch {
		for _, l := range s.Labels {
			if l.Name == "__name__" && l.Value == name {
				count++
			}
		}
	}
	return count
}

// readCounter sums every cell of name on reg whose labels match every k:v
// in match. Returns 0 if the metric is absent.
func readCounter(t *testing.T, reg *prometheus.Registry, name string, match map[string]string) float64 {
	t.Helper()
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	var total float64
	for _, mf := range mfs {
		if mf.GetName() != name {
			continue
		}
		for _, m := range mf.GetMetric() {
			ok := true
			for wantName, wantVal := range match {
				found := false
				for _, lp := range m.GetLabel() {
					if lp.GetName() == wantName && lp.GetValue() == wantVal {
						found = true
						break
					}
				}
				if !found {
					ok = false
					break
				}
			}
			if !ok {
				continue
			}
			if m.Counter != nil {
				total += m.GetCounter().GetValue()
			}
		}
	}
	return total
}
