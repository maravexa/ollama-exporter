//go:build darwin

// This file implements the macOS / Apple Silicon GPU backend. It reads the
// integrated GPU's utilization and memory footprint from the IOKit registry via
// `ioreg` (class IOAccelerator, key PerformanceStatistics) — no root, no CGo and
// no third-party dependencies, preserving the exporter's static build.
//
// Only utilization and memory are exposed: power, temperature and clocks require
// either root (powermetrics) or the private IOReport framework (CGo) and are
// intentionally omitted. Shared types (Config, metrics) live in gpu.go; the
// OS-independent parser and poll logic live in ioreg.go and applepoll.go.
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

// Collector reads Apple Silicon GPU metrics via ioreg on a fixed interval. The
// chip name and total unified memory are resolved once at construction; each
// poll only shells out to ioreg for utilization and in-use memory.
type Collector struct {
	cfg          Config
	appleSilicon bool // whether the host reported hw.optional.arm64=1 at startup
	s            *appleSampler
}

// NewCollector constructs the Apple Silicon GPU collector and registers its
// Prometheus metrics.
//
// Construction never fails due to missing hardware. On a Mac that is not Apple
// Silicon (sysctl hw.optional.arm64 != 1, e.g. Intel), Start is a no-op. On
// Apple Silicon, ioreg is not probed here: Start polls it immediately and keeps
// retrying on every tick, so a transient failure at startup does not disable
// collection for the lifetime of the process.
func NewCollector(cfg Config, reg prometheus.Registerer) (*Collector, error) {
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = 15 * time.Second
	}

	m := newGPUMetrics(reg)

	appleSilicon := isAppleSilicon()
	if !appleSilicon {
		slog.Warn("gpu: host is not Apple Silicon (hw.optional.arm64 != 1), GPU metrics disabled")
	}

	s := &appleSampler{
		m:     m,
		name:  appleChipName(),
		total: appleUnifiedMemoryBytes(),
		read:  runIOReg,
	}
	return &Collector{cfg: cfg, appleSilicon: appleSilicon, s: s}, nil
}

// Start runs the poll loop until ctx is cancelled. It is a no-op when
// cfg.Enabled is false or the host is not Apple Silicon.
func (c *Collector) Start(ctx context.Context) {
	if !c.cfg.Enabled || !c.appleSilicon {
		return
	}
	c.s.run(ctx, c.cfg.PollInterval)
}

// isAppleSilicon reports whether the host CPU is Apple Silicon. The sysctl
// reads 1 on Apple Silicon even when this binary runs as amd64 under Rosetta 2,
// and is 0 or absent on Intel Macs.
func isAppleSilicon() bool {
	return sysctlString("hw.optional.arm64") == "1"
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
