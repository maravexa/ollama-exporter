// Package gpu implements GPU hardware metrics collection for the Ollama
// exporter. Two backends are provided behind build tags, both registering the
// same ollama_gpu_* metric names so dashboards are portable across vendors:
//
//   - Linux (amdgpu.go):    AMD GPUs via the amdgpu sysfs interface.
//   - macOS (applegpu.go):  Apple Silicon GPUs via ioreg (IOAccelerator).
//
// Shared configuration and metric descriptors live in this file so they are
// defined once regardless of which backend is compiled in.
package gpu

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// Config holds configuration for the GPU collector.
type Config struct {
	// Enabled activates GPU metric collection.
	Enabled bool

	// PollInterval controls how often GPU metrics are read.
	// If zero, defaults to 15 seconds.
	PollInterval time.Duration

	// SysfsBase is the root of the DRM sysfs tree (Linux/AMD backend only).
	// Defaults to /sys/class/drm. Ignored by the macOS backend.
	SysfsBase string
}

// gpuMetrics holds all GPU Prometheus metric descriptors. It is intentionally
// unexported — callers interact through the Collector, not the metrics directly.
//
// Not every backend populates every metric: the macOS backend only sets
// Utilization, VRAMUsed and VRAMTotal, since temperature, power and clocks are
// not available on Apple Silicon without root (powermetrics) or CGo (IOReport).
// Unset GaugeVecs simply emit no series.
type gpuMetrics struct {
	Utilization *prometheus.GaugeVec
	Temperature *prometheus.GaugeVec
	VRAMUsed    *prometheus.GaugeVec
	VRAMTotal   *prometheus.GaugeVec
	Power       *prometheus.GaugeVec
	Clock       *prometheus.GaugeVec
}

// newGPUMetrics constructs and registers all GPU Prometheus metrics.
func newGPUMetrics(reg prometheus.Registerer) gpuMetrics {
	m := gpuMetrics{
		Utilization: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: "ollama",
			Name:      "gpu_utilization_percent",
			Help:      "GPU utilization as a percentage (0–100). Linux/AMD: gpu_busy_percent. macOS/Apple: IOAccelerator \"Device Utilization %\" (whole-GPU, includes display/compositor work).",
		}, []string{"gpu", "name"}),

		Temperature: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: "ollama",
			Name:      "gpu_temperature_celsius",
			Help:      "GPU temperature in degrees Celsius, sourced from hwmon temp1_input in sysfs (millidegrees / 1000). Linux/AMD only.",
		}, []string{"gpu", "name"}),

		VRAMUsed: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: "ollama",
			Name:      "gpu_vram_used_bytes",
			Help:      "VRAM currently in use on the GPU, in bytes. macOS/Apple: IOAccelerator \"In use system memory\" (unified memory).",
		}, []string{"gpu", "name"}),

		VRAMTotal: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: "ollama",
			Name:      "gpu_vram_total_bytes",
			Help:      "Total VRAM available on the GPU, in bytes. Cached at startup. macOS/Apple: total unified memory (hw.memsize).",
		}, []string{"gpu", "name"}),

		Power: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: "ollama",
			Name:      "gpu_power_watts",
			Help:      "GPU power draw in watts, sourced from hwmon power1_average or power1_input in sysfs (microwatts / 1 000 000). Linux/AMD only.",
		}, []string{"gpu", "name"}),

		Clock: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: "ollama",
			Name:      "gpu_clock_mhz",
			Help:      `GPU or memory clock speed in MHz. Label "type" is "gpu" (pp_dpm_sclk) or "memory" (pp_dpm_mclk). Linux/AMD only.`,
		}, []string{"gpu", "name", "type"}),
	}

	reg.MustRegister(
		m.Utilization,
		m.Temperature,
		m.VRAMUsed,
		m.VRAMTotal,
		m.Power,
		m.Clock,
	)

	return m
}
