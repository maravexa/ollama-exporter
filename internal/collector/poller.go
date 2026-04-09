package collector

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/maravexa/ollama-exporter/internal/config"
	"github.com/maravexa/ollama-exporter/internal/metrics"
	"github.com/maravexa/ollama-exporter/internal/ollama"
)

// Poller scrapes /api/ps and /api/tags on a fixed interval,
// tracking model lifecycle events and VRAM state.
type Poller struct {
	cfg     *config.Config
	client  *ollama.Client
	metrics *metrics.Metrics
	mc      *ModelCache

	mu          sync.RWMutex    // protects prevLoaded and initialized
	prevLoaded  map[string]bool // tracks which models were loaded last tick
	initialized bool            // true after the first successful scrape
}

// NewPoller constructs a Poller.
func NewPoller(cfg *config.Config, client *ollama.Client, m *metrics.Metrics, mc *ModelCache) *Poller {
	return &Poller{
		cfg:        cfg,
		client:     client,
		metrics:    m,
		mc:         mc,
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

	p.mu.Lock()
	defer p.mu.Unlock()

	currentLoaded := make(map[string]bool)
	for _, m := range ps.Models {
		info := p.mc.Get(ctx, m.Name)
		labels := []string{m.Name, info.Family, info.Quant}

		p.metrics.ModelLoaded.WithLabelValues(labels...).Set(1)
		p.metrics.ModelVRAMBytes.WithLabelValues(labels...).Set(float64(m.SizeVRAM))

		currentLoaded[m.Name] = true

		if !p.prevLoaded[m.Name] {
			// Model newly appeared — increment the cumulative load counter.
			p.metrics.ModelLoadTotal.WithLabelValues(labels...).Inc()

			// Only fire the lifecycle events counter after the first scrape.
			// On startup we don't know when the model was loaded, so we treat
			// whatever is present as already loaded and skip the event.
			if p.initialized {
				p.metrics.ModelLoadEventsTotal.WithLabelValues(m.Name).Inc()
			}
		}
	}

	// Detect unloads: models present last tick but absent now.
	for name := range p.prevLoaded {
		if !currentLoaded[name] {
			info := p.mc.Get(ctx, name)
			p.metrics.ModelLoaded.WithLabelValues(name, info.Family, info.Quant).Set(0)
			p.metrics.ModelVRAMBytes.WithLabelValues(name, info.Family, info.Quant).Set(0)
			p.metrics.ModelUnloadTotal.WithLabelValues(name, info.Family, info.Quant).Inc()
			p.metrics.ModelUnloadEventsTotal.WithLabelValues(name).Inc()
		}
	}

	p.prevLoaded = currentLoaded
	p.initialized = true
}
