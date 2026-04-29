package remotewrite

import (
	"fmt"
	"log/slog"
	"math"
	"sort"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/prometheus/prompb"
)

// Labels is a sorted, deduplicated list of label name/value pairs used as
// the external_labels set merged onto every emitted series.
//
// We deliberately do not depend on github.com/prometheus/prometheus/model/labels
// because that package transitively pulls github.com/grafana/regexp, which
// violates the exporter's minimal-dependency policy.
type Labels []prompb.Label

// NewLabels returns a Labels built from a string map, sorted by name.
func NewLabels(m map[string]string) Labels {
	out := make(Labels, 0, len(m))
	for k, v := range m {
		out = append(out, prompb.Label{Name: k, Value: v})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Gather collects metrics from g, converts each metric family into one or
// more prompb.TimeSeries values, and stamps every sample with now's
// millisecond timestamp. externalLabels are merged onto every series;
// pre-existing labels with the same name (e.g. __name__) win.
//
// Stale samples are out of scope for v0.4.0 — Prometheus uses a magic NaN
// payload to mark a series as stale; we filter NaN values entirely instead.
func Gather(g prometheus.Gatherer, externalLabels Labels, now time.Time) ([]prompb.TimeSeries, error) {
	families, err := g.Gather()
	if err != nil {
		return nil, fmt.Errorf("gather: %w", err)
	}

	tsMillis := now.UnixMilli()
	var out []prompb.TimeSeries

	for _, mf := range families {
		name := mf.GetName()
		if name == "" {
			continue
		}
		switch mf.GetType() {
		case dto.MetricType_COUNTER:
			for _, m := range mf.GetMetric() {
				v := m.GetCounter().GetValue()
				if math.IsNaN(v) {
					continue
				}
				out = append(out, buildSeries(name, m.GetLabel(), externalLabels, v, tsMillis))
			}
		case dto.MetricType_GAUGE:
			for _, m := range mf.GetMetric() {
				v := m.GetGauge().GetValue()
				if math.IsNaN(v) {
					continue
				}
				out = append(out, buildSeries(name, m.GetLabel(), externalLabels, v, tsMillis))
			}
		case dto.MetricType_UNTYPED:
			for _, m := range mf.GetMetric() {
				v := m.GetUntyped().GetValue()
				if math.IsNaN(v) {
					continue
				}
				out = append(out, buildSeries(name, m.GetLabel(), externalLabels, v, tsMillis))
			}
		case dto.MetricType_HISTOGRAM:
			for _, m := range mf.GetMetric() {
				h := m.GetHistogram()
				if h == nil {
					continue
				}
				out = append(out, buildSeries(name+"_count", m.GetLabel(), externalLabels, float64(h.GetSampleCount()), tsMillis))
				if !math.IsNaN(h.GetSampleSum()) {
					out = append(out, buildSeries(name+"_sum", m.GetLabel(), externalLabels, h.GetSampleSum(), tsMillis))
				}
				sawInf := false
				for _, b := range h.GetBucket() {
					le := strconv.FormatFloat(b.GetUpperBound(), 'f', -1, 64)
					if math.IsInf(b.GetUpperBound(), +1) {
						le = "+Inf"
						sawInf = true
					}
					out = append(out, buildBucketSeries(name+"_bucket", m.GetLabel(), externalLabels, le, float64(b.GetCumulativeCount()), tsMillis))
				}
				if !sawInf {
					out = append(out, buildBucketSeries(name+"_bucket", m.GetLabel(), externalLabels, "+Inf", float64(h.GetSampleCount()), tsMillis))
				}
			}
		case dto.MetricType_SUMMARY:
			for _, m := range mf.GetMetric() {
				s := m.GetSummary()
				if s == nil {
					continue
				}
				out = append(out, buildSeries(name+"_count", m.GetLabel(), externalLabels, float64(s.GetSampleCount()), tsMillis))
				if !math.IsNaN(s.GetSampleSum()) {
					out = append(out, buildSeries(name+"_sum", m.GetLabel(), externalLabels, s.GetSampleSum(), tsMillis))
				}
				for _, q := range s.GetQuantile() {
					if math.IsNaN(q.GetValue()) {
						continue
					}
					qLabel := strconv.FormatFloat(q.GetQuantile(), 'f', -1, 64)
					out = append(out, buildQuantileSeries(name, m.GetLabel(), externalLabels, qLabel, q.GetValue(), tsMillis))
				}
			}
		}
	}

	return out, nil
}

// buildSeries assembles one TimeSeries with __name__ injected, dto labels,
// and external labels merged. Existing labels win on collision.
func buildSeries(name string, dtoLabels []*dto.LabelPair, ext Labels, value float64, tsMillis int64) prompb.TimeSeries {
	lbls := mergeLabels(name, dtoLabels, ext, "", "")
	return prompb.TimeSeries{
		Labels:  lbls,
		Samples: []prompb.Sample{{Value: value, Timestamp: tsMillis}},
	}
}

func buildBucketSeries(name string, dtoLabels []*dto.LabelPair, ext Labels, le string, value float64, tsMillis int64) prompb.TimeSeries {
	lbls := mergeLabels(name, dtoLabels, ext, "le", le)
	return prompb.TimeSeries{
		Labels:  lbls,
		Samples: []prompb.Sample{{Value: value, Timestamp: tsMillis}},
	}
}

func buildQuantileSeries(name string, dtoLabels []*dto.LabelPair, ext Labels, quantile string, value float64, tsMillis int64) prompb.TimeSeries {
	lbls := mergeLabels(name, dtoLabels, ext, "quantile", quantile)
	return prompb.TimeSeries{
		Labels:  lbls,
		Samples: []prompb.Sample{{Value: value, Timestamp: tsMillis}},
	}
}

// mergeLabels builds a sorted label set:
//
//  1. __name__=name (always present)
//  2. dto labels from the metric
//  3. an optional extra (e.g. le=... for buckets, quantile=... for summaries)
//  4. external labels, except where a name already exists
//
// Output is sorted by label name as required by Prometheus Remote Write —
// receivers reject unsorted label sets.
func mergeLabels(name string, dtoLabels []*dto.LabelPair, ext Labels, extraName, extraVal string) []prompb.Label {
	have := make(map[string]struct{}, len(dtoLabels)+len(ext)+2)
	out := make([]prompb.Label, 0, len(dtoLabels)+len(ext)+2)

	out = append(out, prompb.Label{Name: "__name__", Value: name})
	have["__name__"] = struct{}{}

	for _, l := range dtoLabels {
		n := l.GetName()
		if n == "" {
			continue
		}
		if _, ok := have[n]; ok {
			continue
		}
		out = append(out, prompb.Label{Name: n, Value: l.GetValue()})
		have[n] = struct{}{}
	}

	if extraName != "" {
		if _, ok := have[extraName]; !ok {
			out = append(out, prompb.Label{Name: extraName, Value: extraVal})
			have[extraName] = struct{}{}
		}
	}

	for _, l := range ext {
		if _, ok := have[l.Name]; ok {
			slog.Debug("external label collision; existing label wins", "label", l.Name)
			continue
		}
		out = append(out, prompb.Label{Name: l.Name, Value: l.Value})
		have[l.Name] = struct{}{}
	}

	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}
