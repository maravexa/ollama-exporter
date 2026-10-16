package collector

import (
	"context"
	"encoding/json"
	"net/http"
)

// Compatibility APIs expose counts, not native phase durations. Histograms
// distinguish a measured zero (one observation) from missing usage (none).
func (p *Proxy) recordUsage(r *http.Request, path string, f map[string]json.RawMessage) (string, string) {
	model := ""
	if requested, ok := r.Context().Value(requestModelKey{}).(string); ok {
		model = requested
	}
	for _, key := range []string{"model", "message.model"} {
		var v string
		if json.Unmarshal(f[key], &v) == nil && v != "" {
			model = v
		}
	}
	present := false
	for _, key := range []string{"usage.prompt_tokens", "usage.input_tokens", "message.usage.input_tokens", "usage.completion_tokens", "usage.output_tokens", "message.usage.output_tokens"} {
		if raw, ok := f[key]; ok && string(raw) != "null" {
			var count int64
			if json.Unmarshal(raw, &count) != nil || count < 0 {
				return "failed", "invalid_fields"
			}
			present = true
		}
	}
	info := p.mc.Get(context.Background(), model)
	labels := []string{model, info.Family, info.Quant, path}
	p.metrics.RequestsTotal.WithLabelValues(labels...).Inc()
	for _, field := range []struct {
		keys  []string
		input bool
	}{
		{[]string{"usage.prompt_tokens", "usage.input_tokens", "message.usage.input_tokens"}, true},
		{[]string{"usage.completion_tokens", "usage.output_tokens", "message.usage.output_tokens"}, false},
	} {
		for _, key := range field.keys {
			var count int64
			if raw, ok := f[key]; ok && string(raw) != "null" && json.Unmarshal(raw, &count) == nil && count >= 0 {
				if field.input {
					p.metrics.CompatibilityInputTokens.WithLabelValues(labels...).Observe(float64(count))
				} else {
					p.metrics.CompatibilityOutputTokens.WithLabelValues(labels...).Observe(float64(count))
				}
				break
			}
		}
	}
	if !present {
		return "skipped", "usage_absent"
	}
	return "success", "none"
}
