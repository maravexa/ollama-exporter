package gpu

import (
	"context"
	"log/slog"
	"time"
)

// gpuIndexLabel is the "gpu" label value; Apple Silicon exposes a single
// integrated GPU.
const gpuIndexLabel = "0"

// appleSampler holds the polling and gauge-update logic for the Apple Silicon
// backend. It has no build tag so the failure and recovery paths can be unit
// tested on any platform with a mocked reader; applegpu.go wires it to ioreg.
type appleSampler struct {
	m     gpuMetrics
	name  string  // "name" label value, e.g. "Apple M4"
	total float64 // total unified memory in bytes; 0 if unknown

	// read returns raw `ioreg -r -d 1 -c IOAccelerator` output.
	read func(context.Context) (string, error)

	// healthy and polled track the outcome of the previous poll so state
	// changes are logged once at warn/info level rather than on every tick.
	healthy bool
	polled  bool
}

// run polls immediately and then on every interval tick until ctx is
// cancelled. A failed poll never stops the loop: collection resumes as soon as
// ioreg returns statistics again, including when the very first poll fails.
func (s *appleSampler) run(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	s.poll(ctx)

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.poll(ctx)
		}
	}
}

// poll performs one read and updates the gauges, reporting whether statistics
// were obtained.
//
// When the read fails or yields no statistics, the utilization and in-use
// memory series are deleted rather than left at their last value, so a broken
// probe shows up as absent data instead of a frozen reading. A present zero
// memory reading is exported as 0; a missing memory key deletes that series.
// The total is a static hardware property and is kept across failures.
func (s *appleSampler) poll(ctx context.Context) bool {
	out, err := s.read(ctx)
	if err != nil {
		s.markUnhealthy("ioreg invocation failed", "err", err)
		return false
	}
	stats, ok := parseIOAccelerator(out)
	if !ok {
		s.markUnhealthy("ioreg output had no IOAccelerator performance statistics")
		return false
	}

	s.m.Utilization.WithLabelValues(gpuIndexLabel, s.name).Set(stats.UtilizationPercent)
	if stats.HasInUseMemory {
		s.m.VRAMUsed.WithLabelValues(gpuIndexLabel, s.name).Set(float64(stats.InUseMemoryBytes))
	} else {
		s.m.VRAMUsed.DeleteLabelValues(gpuIndexLabel, s.name)
	}
	if s.total > 0 {
		s.m.VRAMTotal.WithLabelValues(gpuIndexLabel, s.name).Set(s.total)
	}

	if !s.healthy {
		if s.polled {
			slog.Info("gpu: Apple Silicon GPU statistics recovered", "name", s.name)
		} else {
			slog.Info("gpu: Apple Silicon GPU ready", "name", s.name,
				"unified_memory_bytes", int64(s.total),
				"utilization_percent", stats.UtilizationPercent)
		}
	}
	s.healthy, s.polled = true, true
	return true
}

// markUnhealthy clears the per-poll series and logs the failure: at warn level
// on the first failure after a success (or on the first poll), at debug level
// while the failure persists. Collection keeps retrying on every tick.
func (s *appleSampler) markUnhealthy(msg string, args ...any) {
	s.m.Utilization.DeleteLabelValues(gpuIndexLabel, s.name)
	s.m.VRAMUsed.DeleteLabelValues(gpuIndexLabel, s.name)

	if s.healthy || !s.polled {
		slog.Warn("gpu: "+msg+", will retry every poll interval", args...)
	} else {
		slog.Debug("gpu: "+msg, args...)
	}
	s.healthy, s.polled = false, true
}
