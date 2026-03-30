// Package collector wires together the poller and proxy collectors.
package collector

import (
	"context"
	"log/slog"

	"github.com/maravexa/ollama-exporter/internal/config"
	"github.com/maravexa/ollama-exporter/internal/metrics"
	"github.com/maravexa/ollama-exporter/internal/ollama"
	"github.com/prometheus/client_golang/prometheus"
)

// Collector owns both collection modes and their shared state.
type Collector struct {
	cfg     *config.Config
	client  *ollama.Client
	metrics *metrics.Metrics
	poller  *Poller
	proxy   *Proxy
}

// New constructs a Collector, registering all metrics with reg.
func New(cfg *config.Config, reg prometheus.Registerer) (*Collector, error) {
	m := metrics.New(reg)
	client := ollama.NewClient(cfg.OllamaURL)
	mc := NewModelCache(client)

	poller := NewPoller(cfg, client, m, mc)

	var proxy *Proxy
	if cfg.Proxy.Enabled {
		proxy = NewProxy(cfg, client, m, mc)
	}

	return &Collector{
		cfg:     cfg,
		client:  client,
		metrics: m,
		poller:  poller,
		proxy:   proxy,
	}, nil
}

// Start runs both collection modes until ctx is cancelled.
func (c *Collector) Start(ctx context.Context) {
	if c.proxy != nil {
		go func() {
			if err := c.proxy.Start(ctx); err != nil {
				slog.Error("proxy stopped", "err", err)
			}
		}()
	}
	c.poller.Start(ctx)
}
