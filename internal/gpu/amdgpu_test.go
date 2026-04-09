package gpu

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// ─── mock sysfs helpers ───────────────────────────────────────────────────────

// buildMockSysfs creates a temporary directory tree that mimics
// /sys/class/drm/card0/device/ for a single AMD GPU.
func buildMockSysfs(t *testing.T) string {
	t.Helper()
	root := t.TempDir()

	devPath := filepath.Join(root, "card0", "device")
	mustMkdir(t, devPath)

	// AMD vendor ID.
	mustWriteFile(t, filepath.Join(devPath, "vendor"), "0x1002\n")
	mustWriteFile(t, filepath.Join(devPath, "product_name"), "AMD Radeon RX 6700 XT\n")
	mustWriteFile(t, filepath.Join(devPath, "gpu_busy_percent"), "42\n")
	mustWriteFile(t, filepath.Join(devPath, "mem_info_vram_used"), "4294967296\n")   // 4 GiB
	mustWriteFile(t, filepath.Join(devPath, "mem_info_vram_total"), "12884901888\n") // 12 GiB

	// hwmon subtree (index 3 as the kernel might assign).
	hwmon := filepath.Join(devPath, "hwmon", "hwmon3")
	mustMkdir(t, hwmon)
	mustWriteFile(t, filepath.Join(hwmon, "temp1_input"), "65000\n")        // 65 °C in millidegrees
	mustWriteFile(t, filepath.Join(hwmon, "power1_average"), "120000000\n") // 120 W in microwatts

	// DPM clock files.
	mustWriteFile(t, filepath.Join(devPath, "pp_dpm_sclk"),
		"0: 500Mhz\n1: 1000Mhz\n2: 1750Mhz *\n")
	mustWriteFile(t, filepath.Join(devPath, "pp_dpm_mclk"),
		"0: 96Mhz\n1: 875Mhz *\n")

	return root
}

func mustMkdir(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatalf("MkdirAll(%s): %v", path, err)
	}
}

func mustWriteFile(t *testing.T, path, content string) {
	t.Helper()
	mustMkdir(t, filepath.Dir(path))
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile(%s): %v", path, err)
	}
}

// newTestCollector builds a Collector backed by a mock sysfs root and a fresh
// Prometheus registry.
func newTestCollector(t *testing.T, sysfsRoot string) *Collector {
	t.Helper()
	reg := prometheus.NewRegistry()
	col, err := NewCollector(Config{Enabled: true, SysfsBase: sysfsRoot}, reg)
	if err != nil {
		t.Fatalf("NewCollector: %v", err)
	}
	return col
}

// ─── device discovery ─────────────────────────────────────────────────────────

func TestDiscoverDevices_AMD(t *testing.T) {
	root := buildMockSysfs(t)
	devices, err := discoverDevices(root)
	if err != nil {
		t.Fatalf("discoverDevices: %v", err)
	}
	if len(devices) != 1 {
		t.Fatalf("want 1 device, got %d", len(devices))
	}
	d := devices[0]
	if d.index != 0 {
		t.Errorf("index: want 0, got %d", d.index)
	}
	if d.name != "AMD Radeon RX 6700 XT" {
		t.Errorf("name: want product name, got %q", d.name)
	}
	if d.hwmonPath == "" {
		t.Error("hwmonPath should be resolved at startup")
	}
	if d.vramTotal != 12884901888 {
		t.Errorf("vramTotal: want 12884901888, got %f", d.vramTotal)
	}
}

func TestDiscoverDevices_FallbackName(t *testing.T) {
	root := t.TempDir()
	devPath := filepath.Join(root, "card0", "device")
	mustMkdir(t, devPath)
	mustWriteFile(t, filepath.Join(devPath, "vendor"), "0x1002\n")
	// No product_name file — should fall back to "amdgpu_card0".

	devices, err := discoverDevices(root)
	if err != nil {
		t.Fatalf("discoverDevices: %v", err)
	}
	if len(devices) != 1 {
		t.Fatalf("want 1 device, got %d", len(devices))
	}
	if devices[0].name != "amdgpu_card0" {
		t.Errorf("name: want amdgpu_card0, got %q", devices[0].name)
	}
}

func TestDiscoverDevices_NonAMD(t *testing.T) {
	root := t.TempDir()
	devPath := filepath.Join(root, "card0", "device")
	mustMkdir(t, devPath)
	mustWriteFile(t, filepath.Join(devPath, "vendor"), "0x10de\n") // NVIDIA

	devices, err := discoverDevices(root)
	if err != nil {
		t.Fatalf("discoverDevices: %v", err)
	}
	if len(devices) != 0 {
		t.Errorf("want 0 AMD devices, got %d", len(devices))
	}
}

func TestDiscoverDevices_Empty(t *testing.T) {
	root := t.TempDir()
	devices, err := discoverDevices(root)
	if err != nil {
		t.Fatalf("discoverDevices: %v", err)
	}
	if len(devices) != 0 {
		t.Errorf("want 0 devices in empty dir, got %d", len(devices))
	}
}

func TestDiscoverDevices_SkipsConnectors(t *testing.T) {
	// card0-HDMI-A-1 directories should be skipped.
	root := t.TempDir()

	amdDev := filepath.Join(root, "card0", "device")
	mustMkdir(t, amdDev)
	mustWriteFile(t, filepath.Join(amdDev, "vendor"), "0x1002\n")

	// Connector directory — no device/vendor file, and isCardDir should reject it.
	mustMkdir(t, filepath.Join(root, "card0-HDMI-A-1"))

	devices, err := discoverDevices(root)
	if err != nil {
		t.Fatalf("discoverDevices: %v", err)
	}
	if len(devices) != 1 {
		t.Fatalf("want 1 device, got %d", len(devices))
	}
}

func TestDiscoverDevices_MultipleGPUs(t *testing.T) {
	root := t.TempDir()
	for _, card := range []string{"card0", "card1"} {
		devPath := filepath.Join(root, card, "device")
		mustMkdir(t, devPath)
		mustWriteFile(t, filepath.Join(devPath, "vendor"), "0x1002\n")
	}
	devices, err := discoverDevices(root)
	if err != nil {
		t.Fatalf("discoverDevices: %v", err)
	}
	if len(devices) != 2 {
		t.Errorf("want 2 devices, got %d", len(devices))
	}
}

// ─── metric collection ────────────────────────────────────────────────────────

func TestCollect_AllMetrics(t *testing.T) {
	root := buildMockSysfs(t)
	col := newTestCollector(t, root)

	if len(col.devices) != 1 {
		t.Fatalf("want 1 device, got %d", len(col.devices))
	}

	col.collect()

	gpu := "0"
	name := "AMD Radeon RX 6700 XT"

	assertGauge(t, col.m.Utilization, []string{gpu, name}, 42, "utilization")
	assertGauge(t, col.m.Temperature, []string{gpu, name}, 65.0, "temperature")
	assertGauge(t, col.m.VRAMUsed, []string{gpu, name}, 4294967296, "vram_used")
	assertGauge(t, col.m.VRAMTotal, []string{gpu, name}, 12884901888, "vram_total")
	assertGauge(t, col.m.Power, []string{gpu, name}, 120.0, "power")
	assertGauge(t, col.m.Clock, []string{gpu, name, "gpu"}, 1750, "gpu_clock")
	assertGauge(t, col.m.Clock, []string{gpu, name, "memory"}, 875, "memory_clock")
}

func TestCollect_MissingFiles_NocrashNoPanic(t *testing.T) {
	// Only vendor file present — everything else missing. Should not panic.
	root := t.TempDir()
	devPath := filepath.Join(root, "card0", "device")
	mustMkdir(t, devPath)
	mustWriteFile(t, filepath.Join(devPath, "vendor"), "0x1002\n")

	col := newTestCollector(t, root)

	// Should complete without panic.
	col.collect()
}

func TestCollect_MalformedFiles_NoCrash(t *testing.T) {
	root := t.TempDir()
	devPath := filepath.Join(root, "card0", "device")
	mustMkdir(t, devPath)
	mustWriteFile(t, filepath.Join(devPath, "vendor"), "0x1002\n")
	mustWriteFile(t, filepath.Join(devPath, "gpu_busy_percent"), "not-a-number\n")
	mustWriteFile(t, filepath.Join(devPath, "mem_info_vram_used"), "bad\n")
	mustWriteFile(t, filepath.Join(devPath, "mem_info_vram_total"), "bad\n")
	mustWriteFile(t, filepath.Join(devPath, "pp_dpm_sclk"), "0: 500Mhz\n1: 1000Mhz\n") // no active line
	mustWriteFile(t, filepath.Join(devPath, "pp_dpm_mclk"), "garbage content\n")

	hwmon := filepath.Join(devPath, "hwmon", "hwmon0")
	mustMkdir(t, hwmon)
	mustWriteFile(t, filepath.Join(hwmon, "temp1_input"), "not-a-number\n")
	mustWriteFile(t, filepath.Join(hwmon, "power1_average"), "bad\n")

	col := newTestCollector(t, root)
	col.collect() // must not panic
}

func TestCollect_NoAMDGPUs_Disabled(t *testing.T) {
	// Non-AMD card: collector should have no devices and Start should be a no-op.
	root := t.TempDir()
	devPath := filepath.Join(root, "card0", "device")
	mustMkdir(t, devPath)
	mustWriteFile(t, filepath.Join(devPath, "vendor"), "0x10de\n") // NVIDIA

	col := newTestCollector(t, root)
	if len(col.devices) != 0 {
		t.Errorf("want 0 devices for non-AMD GPU, got %d", len(col.devices))
	}
	// collect is a no-op when devices is empty — just verify it doesn't panic.
	col.collect()
}

func TestCollect_PowerFallback(t *testing.T) {
	// power1_average absent; power1_input should be used instead.
	root := t.TempDir()
	devPath := filepath.Join(root, "card0", "device")
	mustMkdir(t, devPath)
	mustWriteFile(t, filepath.Join(devPath, "vendor"), "0x1002\n")

	hwmon := filepath.Join(devPath, "hwmon", "hwmon0")
	mustMkdir(t, hwmon)
	mustWriteFile(t, filepath.Join(hwmon, "power1_input"), "85000000\n") // 85 W, no power1_average

	col := newTestCollector(t, root)
	col.collect()

	assertGauge(t, col.m.Power, []string{"0", "amdgpu_card0"}, 85.0, "power fallback")
}

func TestCollect_VRAMTotalCached(t *testing.T) {
	// vram_total should be emitted even if the file is deleted after startup.
	root := buildMockSysfs(t)
	col := newTestCollector(t, root)

	// Remove the file to simulate it being unavailable at poll time.
	if err := os.Remove(filepath.Join(root, "card0", "device", "mem_info_vram_total")); err != nil {
		t.Fatal(err)
	}

	col.collect()

	// Cached value must still be emitted.
	assertGauge(t, col.m.VRAMTotal, []string{"0", "AMD Radeon RX 6700 XT"}, 12884901888, "vram_total cached")
}

// ─── isCardDir ────────────────────────────────────────────────────────────────

func TestIsCardDir(t *testing.T) {
	cases := []struct {
		name string
		want bool
	}{
		{"card0", true},
		{"card1", true},
		{"card10", true},
		{"card0-HDMI-A-1", false},
		{"card0-DP-1", false},
		{"card", false},
		{"renderD128", false},
		{"version", false},
		{"", false},
	}
	for _, tc := range cases {
		if got := isCardDir(tc.name); got != tc.want {
			t.Errorf("isCardDir(%q) = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// ─── parseDPMContent ─────────────────────────────────────────────────────────

func TestParseDPMContent(t *testing.T) {
	cases := []struct {
		name    string
		content string
		wantMHz int
		wantErr bool
	}{
		{
			name:    "last entry active",
			content: "0: 500Mhz\n1: 1000Mhz\n2: 1750Mhz *\n",
			wantMHz: 1750,
		},
		{
			name:    "first entry active",
			content: "0: 300Mhz *\n1: 1000Mhz\n2: 1750Mhz\n",
			wantMHz: 300,
		},
		{
			name:    "middle entry active",
			content: "0: 96Mhz\n1: 875Mhz *\n2: 1200Mhz\n",
			wantMHz: 875,
		},
		{
			name:    "MHz uppercase suffix",
			content: "0: 500MHz *\n",
			wantMHz: 500,
		},
		{
			name:    "single entry active",
			content: "0: 875Mhz *",
			wantMHz: 875,
		},
		{
			name:    "no active line",
			content: "0: 300Mhz\n1: 1000Mhz\n",
			wantErr: true,
		},
		{
			name:    "empty content",
			content: "",
			wantErr: true,
		},
		{
			name:    "trailing whitespace on active line",
			content: "0: 500Mhz\n2: 1750Mhz *   \n",
			wantMHz: 1750,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseDPMContent(tc.content)
			if tc.wantErr {
				if err == nil {
					t.Error("want error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.wantMHz {
				t.Errorf("want %d MHz, got %d", tc.wantMHz, got)
			}
		})
	}
}

// ─── helpers ──────────────────────────────────────────────────────────────────

// assertGauge checks that a GaugeVec with the given labels has the expected value.
func assertGauge(t *testing.T, g *prometheus.GaugeVec, labels []string, want float64, metric string) {
	t.Helper()
	got := testutil.ToFloat64(g.WithLabelValues(labels...))
	if got != want {
		t.Errorf("%s: want %f, got %f", metric, want, got)
	}
}
