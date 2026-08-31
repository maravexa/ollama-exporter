//go:build darwin

// This file implements the macOS / Apple Silicon GPU backend. It reads the
// integrated GPU's utilization and memory footprint from the IOKit registry via
// `ioreg` (class IOAccelerator, key PerformanceStatistics) — no root, no CGo and
// no third-party dependencies, preserving the exporter's static build.
//
// Only utilization and memory are exposed: power, temperature and clocks require
// either root (powermetrics) or the private IOReport framework (CGo) and are
// intentionally omitted. Shared types (Config, metrics) live in gpu.go; the
// OS-independent parser lives in ioreg.go.
package gpu

import (
	"context"
	"log/slog"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// ioregTimeout bounds each ioreg/sysctl invocation so a stuck call cannot stall
// the poll loop.
const ioregTimeout = 5 * time.Second

// gpuIndexLabel is the "gpu" label value; Apple Silicon exposes a single
// integrated GPU.
const gpuIndexLabel = "0"

// Collector reads Apple Silicon GPU metrics via ioreg on a fixed interval. The
// chip name and total unified memory are resolved once at construction; each
// poll only shells out to ioreg for utilization and in-use memory.
type Collector struct {
	cfg   Config
	m     gpuMetrics
	name  string  // e.g. "Apple M4"
	total float64 // total unified memory in bytes, used as VRAM total
	found bool    // whether a GPU accelerator was detected at startup
}

// NewCollector constructs the Apple Silicon GPU collector, registers its
// Prometheus metrics, and probes ioreg once to confirm a GPU is present.
//
// Construction never fails due to missing hardware: if ioreg yields no
// IOAccelerator performance statistics, the Collector is returned with
// found=false and Start becomes a no-op — mirroring the Linux backend.
func NewCollector(cfg Config, reg prometheus.Registerer) (*Collector, error) {
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = 15 * time.Second
	}

	m := newGPUMetrics(reg)
	name := appleChipName()
	total := appleUnifiedMemoryBytes()

	found := false
	if out, err := runIOReg(context.Background()); err != nil {
		slog.Warn("gpu: ioreg invocation failed, GPU metrics disabled", "err", err)
	} else if stats, ok := parseIOAccelerator(out); ok {
		found = true
		slog.Info("gpu: Apple Silicon GPU ready", "name", name,
			"unified_memory_bytes", int64(total),
			"utilization_percent", stats.UtilizationPercent)
	} else {
		slog.Warn("gpu: no IOAccelerator performance statistics found, GPU metrics disabled")
	}

	return &Collector{cfg: cfg, m: m, name: name, total: total, found: found}, nil
}

// Start runs the poll loop until ctx is cancelled. It is a no-op when
// cfg.Enabled is false or no GPU was detected at construction.
func (c *Collector) Start(ctx context.Context) {
	if !c.cfg.Enabled || !c.found {
		return
	}

	ticker := time.NewTicker(c.cfg.PollInterval)
	defer ticker.Stop()

	// Collect once immediately so metrics exist before the first tick.
	c.collect(ctx)

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.collect(ctx)
		}
	}
}

// collect performs one ioreg read and updates the gauges.
func (c *Collector) collect(ctx context.Context) {
	out, err := runIOReg(ctx)
	if err != nil {
		slog.Debug("gpu: ioreg read failed", "err", err)
		return
	}
	stats, ok := parseIOAccelerator(out)
	if !ok {
		slog.Debug("gpu: ioreg output had no performance statistics")
		return
	}

	c.m.Utilization.WithLabelValues(gpuIndexLabel, c.name).Set(stats.UtilizationPercent)
	if stats.InUseMemoryBytes > 0 {
		c.m.VRAMUsed.WithLabelValues(gpuIndexLabel, c.name).Set(float64(stats.InUseMemoryBytes))
	}
	if c.total > 0 {
		c.m.VRAMTotal.WithLabelValues(gpuIndexLabel, c.name).Set(c.total)
	}
}

// runIOReg returns the output of `ioreg -r -d 1 -c IOAccelerator`, bounded by
// ioregTimeout so a stuck call cannot block the poll loop.
func runIOReg(ctx context.Context) (string, error) {
	cctx, cancel := context.WithTimeout(ctx, ioregTimeout)
	defer cancel()
	out, err := exec.CommandContext(cctx, "ioreg", "-r", "-d", "1", "-c", "IOAccelerator").Output()
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// appleChipName returns a human-readable chip name (e.g. "Apple M4") from
// sysctl, falling back to "Apple GPU".
func appleChipName() string {
	if v := sysctlString("machdep.cpu.brand_string"); v != "" {
		return v
	}
	return "Apple GPU"
}

// appleUnifiedMemoryBytes returns total physical (unified) memory in bytes,
// used as the GPU's VRAM total since Apple Silicon shares one memory pool.
func appleUnifiedMemoryBytes() float64 {
	if v := sysctlString("hw.memsize"); v != "" {
		if n, err := strconv.ParseUint(v, 10, 64); err == nil {
			return float64(n)
		}
	}
	return 0
}

// sysctlString runs `sysctl -n <key>` and returns the trimmed value, or "" on error.
func sysctlString(key string) string {
	ctx, cancel := context.WithTimeout(context.Background(), ioregTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "sysctl", "-n", key).Output() //nolint:gosec // G204: key is an internal constant, never user input
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}
