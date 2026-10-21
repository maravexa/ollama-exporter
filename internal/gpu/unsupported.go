//go:build !linux && !darwin

// This file provides a no-op GPU backend for platforms without one (e.g.
// Windows, FreeBSD) so the exporter still builds there; GPU collection is
// skipped with a warning.
package gpu

import (
	"context"
	"log/slog"

	"github.com/prometheus/client_golang/prometheus"
)

// Collector is a no-op GPU collector for platforms without a GPU backend.
type Collector struct{}

// NewCollector returns a no-op Collector. No metrics are registered.
func NewCollector(_ Config, _ prometheus.Registerer) (*Collector, error) {
	slog.Warn("gpu: no GPU backend for this platform, GPU metrics disabled")
	return &Collector{}, nil
}

// Start is a no-op.
func (c *Collector) Start(_ context.Context) {}
