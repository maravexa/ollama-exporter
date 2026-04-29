package remotewrite

import (
	"math"
	"slices"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/prometheus/prompb"
)

func findSeries(series []prompb.TimeSeries, name string, match map[string]string) *prompb.TimeSeries {
	for i, s := range series {
		var seriesName string
		ok := true
		for _, l := range s.Labels {
			if l.Name == "__name__" {
				seriesName = l.Value
			}
		}
		if seriesName != name {
			continue
		}
		for k, v := range match {
			found := false
			for _, l := range s.Labels {
				if l.Name == k && l.Value == v {
					found = true
					break
				}
			}
			if !found {
				ok = false
				break
			}
		}
		if ok {
			return &series[i]
		}
	}
	return nil
}

func TestGather_Counter(t *testing.T) {
	reg := prometheus.NewRegistry()
	c := prometheus.NewCounterVec(prometheus.CounterOpts{Name: "test_counter"}, []string{"endpoint"})
	reg.MustRegister(c)
	c.WithLabelValues("/api/foo").Add(7)

	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	got, err := Gather(reg, NewLabels(map[string]string{"cluster": "xena"}), now)
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}

	s := findSeries(got, "test_counter", map[string]string{"endpoint": "/api/foo"})
	if s == nil {
		t.Fatal("expected test_counter{endpoint=/api/foo}")
	}
	if s.Samples[0].Value != 7 {
		t.Errorf("value = %v, want 7", s.Samples[0].Value)
	}
	if s.Samples[0].Timestamp != now.UnixMilli() {
		t.Errorf("timestamp = %v, want %v", s.Samples[0].Timestamp, now.UnixMilli())
	}
	// External label merged.
	if findSeries(got, "test_counter", map[string]string{"cluster": "xena"}) == nil {
		t.Error("external label cluster=xena not merged")
	}
}

func TestGather_Histogram(t *testing.T) {
	reg := prometheus.NewRegistry()
	h := prometheus.NewHistogram(prometheus.HistogramOpts{
		Name:    "test_hist",
		Buckets: []float64{0.1, 1, 10},
	})
	reg.MustRegister(h)
	h.Observe(0.5)
	h.Observe(2)
	h.Observe(20)

	got, err := Gather(reg, nil, time.Now())
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}

	if findSeries(got, "test_hist_count", nil) == nil {
		t.Error("missing _count series")
	}
	if findSeries(got, "test_hist_sum", nil) == nil {
		t.Error("missing _sum series")
	}

	bucketCount := 0
	sawInf := false
	for _, s := range got {
		var name string
		for _, l := range s.Labels {
			if l.Name == "__name__" {
				name = l.Value
			}
		}
		if name == "test_hist_bucket" {
			bucketCount++
			for _, l := range s.Labels {
				if l.Name == "le" && l.Value == "+Inf" {
					sawInf = true
				}
			}
		}
	}
	if bucketCount != 4 {
		t.Errorf("expected 4 buckets (3 finite + +Inf), got %d", bucketCount)
	}
	if !sawInf {
		t.Error("expected an le=+Inf bucket series")
	}
}

func TestGather_Summary(t *testing.T) {
	reg := prometheus.NewRegistry()
	s := prometheus.NewSummary(prometheus.SummaryOpts{
		Name:       "test_sum",
		Objectives: map[float64]float64{0.5: 0.05, 0.9: 0.01},
	})
	reg.MustRegister(s)
	for i := 0; i < 100; i++ {
		s.Observe(float64(i))
	}

	got, err := Gather(reg, nil, time.Now())
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}

	if findSeries(got, "test_sum_count", nil) == nil {
		t.Error("missing _count series")
	}
	if findSeries(got, "test_sum_sum", nil) == nil {
		t.Error("missing _sum series")
	}
	if findSeries(got, "test_sum", map[string]string{"quantile": "0.5"}) == nil {
		t.Error("missing quantile=0.5")
	}
	if findSeries(got, "test_sum", map[string]string{"quantile": "0.9"}) == nil {
		t.Error("missing quantile=0.9")
	}
}

func TestGather_ExternalLabelDoesNotOverwriteName(t *testing.T) {
	reg := prometheus.NewRegistry()
	c := prometheus.NewCounter(prometheus.CounterOpts{Name: "test_metric"})
	reg.MustRegister(c)
	c.Inc()

	// External label tries to overwrite __name__ — must not succeed.
	got, err := Gather(reg, NewLabels(map[string]string{"__name__": "evil"}), time.Now())
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	if findSeries(got, "test_metric", nil) == nil {
		t.Error("__name__ was overwritten by external label")
	}
}

func TestGather_LabelsSorted(t *testing.T) {
	reg := prometheus.NewRegistry()
	c := prometheus.NewCounterVec(prometheus.CounterOpts{Name: "test"}, []string{"zeta", "alpha", "mu"})
	reg.MustRegister(c)
	c.WithLabelValues("z", "a", "m").Inc()

	got, err := Gather(reg, NewLabels(map[string]string{"cluster": "x"}), time.Now())
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	for _, s := range got {
		if !slices.IsSortedFunc(s.Labels, func(a, b prompb.Label) int {
			if a.Name < b.Name {
				return -1
			}
			if a.Name > b.Name {
				return 1
			}
			return 0
		}) {
			t.Errorf("labels not sorted: %+v", s.Labels)
		}
	}
}

func TestGather_EmptyRegistry(t *testing.T) {
	reg := prometheus.NewRegistry()
	got, err := Gather(reg, nil, time.Now())
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected empty slice, got %d series", len(got))
	}
}

func TestGather_NaNFiltered(t *testing.T) {
	reg := prometheus.NewRegistry()
	g := prometheus.NewGauge(prometheus.GaugeOpts{Name: "test_nan"})
	reg.MustRegister(g)
	g.Set(math.NaN())

	got, err := Gather(reg, nil, time.Now())
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	if findSeries(got, "test_nan", nil) != nil {
		t.Error("NaN value should be filtered out")
	}
}

func TestGather_LeFormatting(t *testing.T) {
	reg := prometheus.NewRegistry()
	h := prometheus.NewHistogram(prometheus.HistogramOpts{
		Name:    "fmt_hist",
		Buckets: []float64{0.005, 0.5, 5},
	})
	reg.MustRegister(h)
	h.Observe(1)

	got, _ := Gather(reg, nil, time.Now())
	wantLEs := map[string]bool{"0.005": false, "0.5": false, "5": false, "+Inf": false}
	for _, s := range got {
		var name string
		var le string
		for _, l := range s.Labels {
			if l.Name == "__name__" {
				name = l.Value
			}
			if l.Name == "le" {
				le = l.Value
			}
		}
		if name == "fmt_hist_bucket" {
			if _, ok := wantLEs[le]; ok {
				wantLEs[le] = true
			}
		}
	}
	for le, seen := range wantLEs {
		if !seen {
			t.Errorf("expected le=%q bucket", le)
		}
	}
}
