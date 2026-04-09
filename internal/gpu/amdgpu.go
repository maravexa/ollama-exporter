// Package gpu implements AMD GPU hardware metrics collection via the Linux
// sysfs interface exposed by the amdgpu kernel driver.
//
// No ROCm userspace packages (rocm-smi, amd-smi, etc.) are required. The
// sysfs paths used here are stable on any Linux system running the amdgpu
// driver (kernel 4.15+).
package gpu

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// Config holds configuration for the AMD GPU collector.
type Config struct {
	// Enabled activates GPU metric collection via sysfs.
	Enabled bool

	// PollInterval controls how often GPU metrics are read from sysfs.
	// If zero, defaults to 15 seconds.
	PollInterval time.Duration

	// SysfsBase is the root of the DRM sysfs tree.
	// Defaults to /sys/class/drm. Override for testing.
	SysfsBase string
}

// gpuMetrics holds all GPU Prometheus metric descriptors. It is intentionally
// unexported — callers interact through the Collector, not the metrics directly.
type gpuMetrics struct {
	Utilization *prometheus.GaugeVec
	Temperature *prometheus.GaugeVec
	VRAMUsed    *prometheus.GaugeVec
	VRAMTotal   *prometheus.GaugeVec
	Power       *prometheus.GaugeVec
	Clock       *prometheus.GaugeVec
}

// device represents a single discovered AMD GPU and its resolved sysfs paths.
type device struct {
	// index is the DRM card index (e.g. 0 for card0).
	index int
	// name is the human-readable product name, or "amdgpu_cardN" if unavailable.
	name string
	// sysfsPath is the device directory: <sysfsBase>/cardN/device
	sysfsPath string
	// hwmonPath is the resolved hwmon subdirectory (empty if not found).
	// Resolved once at startup to avoid repeated Glob calls during polling.
	hwmonPath string
	// vramTotal is cached at startup; the total VRAM rarely changes at runtime.
	vramTotal float64
}

// Collector discovers AMD GPUs via sysfs and polls hardware metrics on a
// configurable interval. It is safe to call Start from a single goroutine.
//
// If no AMD GPUs are found at construction time, Start returns immediately
// without error — GPU metrics are simply absent from the registry.
type Collector struct {
	cfg     Config
	devices []device
	m       gpuMetrics
}

// NewCollector constructs a GPU Collector, registers its Prometheus metrics
// with reg, and discovers AMD GPUs under cfg.SysfsBase.
//
// Construction never fails due to missing hardware: if the sysfs scan finds
// no AMD GPUs, the Collector is returned with an empty device list and a
// warning is logged. The caller should still call Start — it will be a no-op.
func NewCollector(cfg Config, reg prometheus.Registerer) (*Collector, error) {
	if cfg.SysfsBase == "" {
		cfg.SysfsBase = "/sys/class/drm"
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = 15 * time.Second
	}

	m := newGPUMetrics(reg)

	devices, err := discoverDevices(cfg.SysfsBase)
	if err != nil {
		slog.Warn("gpu: sysfs scan failed, GPU metrics disabled", "err", err)
		devices = nil
	}

	switch len(devices) {
	case 0:
		slog.Warn("gpu: no AMD GPUs found under sysfs, GPU metrics disabled",
			"sysfs_base", cfg.SysfsBase)
	default:
		slog.Info("gpu: discovered AMD GPUs", "count", len(devices),
			"sysfs_base", cfg.SysfsBase)
		for _, d := range devices {
			slog.Info("gpu: device ready", "index", d.index, "name", d.name,
				"hwmon", d.hwmonPath)
		}
	}

	return &Collector{cfg: cfg, devices: devices, m: m}, nil
}

// Start runs the GPU poll loop until ctx is cancelled. It is a no-op when the
// Collector has no discovered devices or cfg.Enabled is false.
func (c *Collector) Start(ctx context.Context) {
	if !c.cfg.Enabled || len(c.devices) == 0 {
		return
	}

	ticker := time.NewTicker(c.cfg.PollInterval)
	defer ticker.Stop()

	// Collect once immediately so metrics are available before the first tick.
	c.collect()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.collect()
		}
	}
}

// collect reads all GPU metrics for one poll cycle.
func (c *Collector) collect() {
	for _, d := range c.devices {
		gpu := strconv.Itoa(d.index)
		c.collectUtilization(d, gpu)
		c.collectTemperature(d, gpu)
		c.collectVRAMUsed(d, gpu)
		c.collectVRAMTotal(d, gpu)
		c.collectPower(d, gpu)
		c.collectClocks(d, gpu)
	}
}

func (c *Collector) collectUtilization(d device, gpu string) {
	v, err := readUint64File(filepath.Join(d.sysfsPath, "gpu_busy_percent"))
	if err != nil {
		slog.Debug("gpu: gpu_busy_percent unavailable", "gpu", gpu, "err", err)
		return
	}
	c.m.Utilization.WithLabelValues(gpu, d.name).Set(float64(v))
}

func (c *Collector) collectTemperature(d device, gpu string) {
	if d.hwmonPath == "" {
		return
	}
	// temp1_input is in millidegrees Celsius.
	v, err := readUint64File(filepath.Join(d.hwmonPath, "temp1_input"))
	if err != nil {
		slog.Debug("gpu: temp1_input unavailable", "gpu", gpu, "err", err)
		return
	}
	c.m.Temperature.WithLabelValues(gpu, d.name).Set(float64(v) / 1000.0)
}

func (c *Collector) collectVRAMUsed(d device, gpu string) {
	v, err := readUint64File(filepath.Join(d.sysfsPath, "mem_info_vram_used"))
	if err != nil {
		slog.Debug("gpu: mem_info_vram_used unavailable", "gpu", gpu, "err", err)
		return
	}
	c.m.VRAMUsed.WithLabelValues(gpu, d.name).Set(float64(v))
}

func (c *Collector) collectVRAMTotal(d device, gpu string) {
	// vramTotal is cached at startup; just emit the cached value.
	if d.vramTotal > 0 {
		c.m.VRAMTotal.WithLabelValues(gpu, d.name).Set(d.vramTotal)
	}
}

func (c *Collector) collectPower(d device, gpu string) {
	if d.hwmonPath == "" {
		return
	}
	// power1_average is preferred; fall back to power1_input on cards that
	// only expose instantaneous power (e.g. some Polaris/Vega variants).
	// Values are in microwatts.
	v, err := readUint64File(filepath.Join(d.hwmonPath, "power1_average"))
	if err != nil {
		slog.Debug("gpu: power1_average unavailable, trying power1_input",
			"gpu", gpu, "err", err)
		v, err = readUint64File(filepath.Join(d.hwmonPath, "power1_input"))
		if err != nil {
			slog.Debug("gpu: power unavailable", "gpu", gpu, "err", err)
			return
		}
	}
	c.m.Power.WithLabelValues(gpu, d.name).Set(float64(v) / 1e6)
}

func (c *Collector) collectClocks(d device, gpu string) {
	if mhz, err := parseDPMFile(filepath.Join(d.sysfsPath, "pp_dpm_sclk")); err == nil {
		c.m.Clock.WithLabelValues(gpu, d.name, "gpu").Set(float64(mhz))
	} else {
		slog.Debug("gpu: pp_dpm_sclk unavailable", "gpu", gpu, "err", err)
	}

	if mhz, err := parseDPMFile(filepath.Join(d.sysfsPath, "pp_dpm_mclk")); err == nil {
		c.m.Clock.WithLabelValues(gpu, d.name, "memory").Set(float64(mhz))
	} else {
		slog.Debug("gpu: pp_dpm_mclk unavailable", "gpu", gpu, "err", err)
	}
}

// discoverDevices scans sysfsBase for card* directories that belong to AMD GPUs
// (vendor ID 0x1002). For each qualifying card it resolves the hwmon path and
// caches the total VRAM.
func discoverDevices(sysfsBase string) ([]device, error) {
	entries, err := filepath.Glob(filepath.Join(sysfsBase, "card*"))
	if err != nil {
		return nil, fmt.Errorf("glob %s/card*: %w", sysfsBase, err)
	}

	var devices []device
	for _, entry := range entries {
		base := filepath.Base(entry)
		if !isCardDir(base) {
			// Skip connector directories like card0-HDMI-A-1.
			continue
		}

		devPath := filepath.Join(entry, "device")
		vendor, err := readTrimmed(filepath.Join(devPath, "vendor"))
		if err != nil {
			slog.Debug("gpu: cannot read vendor", "path", entry, "err", err)
			continue
		}
		if vendor != "0x1002" {
			continue // Not AMD.
		}

		idx, err := parseCardIndex(base)
		if err != nil {
			slog.Debug("gpu: cannot parse card index", "name", base, "err", err)
			continue
		}

		name := fmt.Sprintf("amdgpu_card%d", idx)
		if pn, err := readTrimmed(filepath.Join(devPath, "product_name")); err == nil && pn != "" {
			name = pn
		}

		hwmon := resolveHwmon(devPath)

		vramTotal, _ := readUint64File(filepath.Join(devPath, "mem_info_vram_total"))

		devices = append(devices, device{
			index:     idx,
			name:      name,
			sysfsPath: devPath,
			hwmonPath: hwmon,
			vramTotal: float64(vramTotal),
		})
	}

	return devices, nil
}

// isCardDir reports whether name is a bare DRM card directory ("card0",
// "card1", …) as opposed to a connector directory ("card0-HDMI-A-1") or a
// render node ("renderD128").
func isCardDir(name string) bool {
	if !strings.HasPrefix(name, "card") {
		return false
	}
	suffix := name[len("card"):]
	if suffix == "" {
		return false
	}
	for _, c := range suffix {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// parseCardIndex extracts the integer N from a "cardN" name.
func parseCardIndex(name string) (int, error) {
	return strconv.Atoi(name[len("card"):])
}

// resolveHwmon returns the first hwmon* subdirectory under devPath/hwmon/.
// The hwmon index is assigned by the kernel at load time and varies across
// boots and systems; Glob is used to find it once at startup.
func resolveHwmon(devPath string) string {
	matches, err := filepath.Glob(filepath.Join(devPath, "hwmon", "hwmon*"))
	if err != nil || len(matches) == 0 {
		return ""
	}
	return matches[0]
}

// parseDPMFile reads a pp_dpm_sclk or pp_dpm_mclk file and returns the clock
// speed in MHz for the currently active P-state (the line ending with " *").
//
// Example file content:
//
//	0: 300Mhz
//	1: 1000Mhz
//	2: 1750Mhz *
func parseDPMFile(path string) (int, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	return parseDPMContent(string(data))
}

// parseDPMContent is the pure-logic counterpart of parseDPMFile, split out for
// unit testability without touching the filesystem.
func parseDPMContent(content string) (int, error) {
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasSuffix(line, "*") {
			continue
		}
		// Strip the trailing " *" marker.
		line = strings.TrimSpace(strings.TrimSuffix(line, "*"))

		// Remaining format: "N: VALUEMhz" — the MHz value is always the last field.
		parts := strings.Fields(line)
		if len(parts) == 0 {
			continue
		}
		raw := strings.ToLower(parts[len(parts)-1])
		raw = strings.TrimSuffix(raw, "mhz")
		mhz, err := strconv.Atoi(raw)
		if err != nil {
			return 0, fmt.Errorf("parse MHz value from %q: %w", parts[len(parts)-1], err)
		}
		return mhz, nil
	}
	return 0, fmt.Errorf("no active clock entry (marked with *) found")
}

// readUint64File reads a sysfs file that contains a single decimal uint64.
func readUint64File(path string) (uint64, error) {
	s, err := readTrimmed(path)
	if err != nil {
		return 0, err
	}
	return strconv.ParseUint(s, 10, 64)
}

// readTrimmed reads a file and returns its content with surrounding whitespace
// stripped. Sysfs files typically end with a newline.
func readTrimmed(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(data)), nil
}

// newGPUMetrics constructs and registers all GPU Prometheus metrics.
func newGPUMetrics(reg prometheus.Registerer) gpuMetrics {
	m := gpuMetrics{
		Utilization: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: "ollama",
			Name:      "gpu_utilization_percent",
			Help:      "GPU utilization as a percentage (0–100), sourced from gpu_busy_percent in sysfs.",
		}, []string{"gpu", "name"}),

		Temperature: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: "ollama",
			Name:      "gpu_temperature_celsius",
			Help:      "GPU temperature in degrees Celsius, sourced from hwmon temp1_input in sysfs (millidegrees / 1000).",
		}, []string{"gpu", "name"}),

		VRAMUsed: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: "ollama",
			Name:      "gpu_vram_used_bytes",
			Help:      "VRAM currently in use on the GPU, in bytes.",
		}, []string{"gpu", "name"}),

		VRAMTotal: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: "ollama",
			Name:      "gpu_vram_total_bytes",
			Help:      "Total VRAM available on the GPU, in bytes. Cached at startup.",
		}, []string{"gpu", "name"}),

		Power: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: "ollama",
			Name:      "gpu_power_watts",
			Help:      "GPU power draw in watts, sourced from hwmon power1_average or power1_input in sysfs (microwatts / 1 000 000).",
		}, []string{"gpu", "name"}),

		Clock: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: "ollama",
			Name:      "gpu_clock_mhz",
			Help:      `GPU or memory clock speed in MHz. Label "type" is "gpu" (pp_dpm_sclk) or "memory" (pp_dpm_mclk).`,
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
