package gpu

import (
	"regexp"
	"strconv"
)

// ioAccelStats holds the subset of the IOAccelerator PerformanceStatistics
// dictionary that is readable on Apple Silicon without root or CGo.
type ioAccelStats struct {
	// UtilizationPercent is the whole-GPU "Device Utilization %" (0–100).
	UtilizationPercent float64
	// InUseMemoryBytes is "In use system memory" — the GPU's live footprint in
	// the unified memory pool, in bytes.
	InUseMemoryBytes uint64
}

var (
	// Match e.g. `"Device Utilization %"=100` (ioreg emits no spaces around =,
	// but tolerate them for robustness across macOS versions).
	reDeviceUtil = regexp.MustCompile(`"Device Utilization %"\s*=\s*(\d+)`)
	// Match `"In use system memory"=NNN` but NOT `"In use system memory (driver)"=0`
	// — the trailing quote after "memory" prevents the (driver) variant matching.
	reInUseMem = regexp.MustCompile(`"In use system memory"\s*=\s*(\d+)`)
)

// parseIOAccelerator extracts GPU utilization and in-use memory from the output
// of `ioreg -r -d 1 -c IOAccelerator`. It returns ok=false when no utilization
// figure is present (e.g. on non–Apple-Silicon hardware or an unexpected format).
//
// This function is deliberately OS-independent (pure string parsing) so it can
// be unit-tested against captured fixtures on any platform.
func parseIOAccelerator(raw string) (ioAccelStats, bool) {
	var s ioAccelStats

	m := reDeviceUtil.FindStringSubmatch(raw)
	if m == nil {
		return s, false
	}
	util, err := strconv.ParseFloat(m[1], 64)
	if err != nil {
		return s, false
	}
	s.UtilizationPercent = util

	// In-use memory is best-effort: skip it if absent or out of range (a huge
	// value overflowing uint64), keeping utilization usable on its own.
	if mm := reInUseMem.FindStringSubmatch(raw); mm != nil {
		if n, perr := strconv.ParseUint(mm[1], 10, 64); perr == nil {
			s.InUseMemoryBytes = n
		}
	}

	return s, true
}
