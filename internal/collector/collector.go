// Package collector wires together the poller, proxy, and GPU collectors.
package collector

import (
	"context"
	"log/slog"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/maravexa/ollama-exporter/internal/config"
	"github.com/maravexa/ollama-exporter/internal/gpu"
	"github.com/maravexa/ollama-exporter/internal/metrics"
	"github.com/maravexa/ollama-exporter/internal/ollama"
)

// Collector owns both collection modes and their shared state.
type Collector struct {
	cfg     *config.Config
	client  *ollama.Client
	metrics *metrics.Metrics
	poller  *Poller
	proxy   *Proxy
	gpu     *gpu.Collector
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

	var gpuCol *gpu.Collector
	if cfg.GPU.Enabled {
		gpuCfg := gpu.Config{
			Enabled:      cfg.GPU.Enabled,
			PollInterval: cfg.GPU.PollInterval,
			SysfsBase:    cfg.GPU.SysfsBase,
		}
		// If no separate GPU poll interval is configured, inherit from the main poller.
		if gpuCfg.PollInterval <= 0 {
			gpuCfg.PollInterval = cfg.PollInterval
		}
		var err error
		gpuCol, err = gpu.NewCollector(gpuCfg, reg)
		if err != nil {
			return nil, err
		}
	}

	return &Collector{
		cfg:     cfg,
		client:  client,
		metrics: m,
		poller:  poller,
		proxy:   proxy,
		gpu:     gpuCol,
	}, nil
}

// Start runs all collection modes until ctx is cancelled.
func (c *Collector) Start(ctx context.Context) {
	if c.gpu != nil {
		go c.gpu.Start(ctx)
	}
	if c.proxy != nil {
		go func() {
			if err := c.proxy.Start(ctx); err != nil {
				slog.Error("proxy stopped", "err", err)
			}
		}()
	}
	c.poller.Start(ctx)
}
