package collector

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"github.com/maravexa/ollama-exporter/internal/config"
	"github.com/maravexa/ollama-exporter/internal/metrics"
	"github.com/maravexa/ollama-exporter/internal/ollama"
)

// Poller scrapes /api/ps and /api/tags on a fixed interval,
// tracking model lifecycle events and VRAM state.
type Poller struct {
	cfg        *config.Config
	client     *ollama.Client
	metrics    *metrics.Metrics
	prevLoaded map[string]bool // tracks which models were loaded last tick
}

// NewPoller constructs a Poller.
func NewPoller(cfg *config.Config, client *ollama.Client, m *metrics.Metrics) *Poller {
	return &Poller{
		cfg:        cfg,
		client:     client,
		metrics:    m,
		prevLoaded: make(map[string]bool),
	}
}

// Start runs the polling loop until ctx is cancelled.
func (p *Poller) Start(ctx context.Context) {
	ticker := time.NewTicker(p.cfg.PollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			p.scrape(ctx)
		}
	}
}

func (p *Poller) scrape(ctx context.Context) {
	// Health check.
	if err := p.client.Health(ctx); err != nil {
		p.metrics.Up.WithLabelValues().Set(0)
		slog.Warn("ollama health check failed", "err", err)
		return
	}
	p.metrics.Up.WithLabelValues().Set(1)

	// Scrape running models.
	ps, err := p.client.PS(ctx)
	if err != nil {
		slog.Warn("failed to scrape /api/ps", "err", err)
		return
	}

	currentLoaded := make(map[string]bool)
	for _, m := range ps.Models {
		family, quant := parseModelName(m.Name)
		labels := []string{m.Name, family, quant}

		p.metrics.ModelLoaded.WithLabelValues(labels...).Set(1)
		p.metrics.ModelVRAMBytes.WithLabelValues(labels...).Set(float64(m.SizeVRAM))

		currentLoaded[m.Name] = true

		if !p.prevLoaded[m.Name] {
			// Model newly appeared — increment load counter.
			p.metrics.ModelLoadTotal.WithLabelValues(labels...).Inc()
		}
	}

	// Detect unloads: models present last tick but absent now.
	for name := range p.prevLoaded {
		if !currentLoaded[name] {
			family, quant := parseModelName(name)
			p.metrics.ModelLoaded.WithLabelValues(name, family, quant).Set(0)
			p.metrics.ModelUnloadTotal.WithLabelValues(name, family, quant).Inc()
		}
	}

	p.prevLoaded = currentLoaded
}

// parseModelName extracts family and quantization labels from an Ollama model tag.
// e.g. "llama3.1:8b-q4_0" → family="llama3", quant="q4_0"
// TODO: expand pattern matching for all Ollama naming conventions.
func parseModelName(name string) (family, quant string) {
	family = "unknown"
	quant = "unknown"

	parts := strings.SplitN(name, ":", 2)
	if len(parts) > 0 {
		base := parts[0]
		// Strip version suffixes like ".1", ".2" for family grouping.
		if idx := strings.Index(base, "."); idx > 0 {
			base = base[:idx]
		}
		family = base
	}

	if len(parts) == 2 {
		tag := parts[1]
		// Quantization appears after the last "-" in the tag.
		if idx := strings.LastIndex(tag, "-"); idx >= 0 {
			quant = tag[idx+1:]
		} else {
			quant = tag
		}
	}

	return family, quant
}
