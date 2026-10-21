package gpu

import "testing"

// sampleIOReg is a real `ioreg -r -d 1 -c IOAccelerator` PerformanceStatistics
// block captured on an Apple M4 (utilization edited to 42 to assert an exact
// parse rather than a pinned 100%). Note the decoy "(driver)" memory key.
const sampleIOReg = `
  +-o AGXAcceleratorG16X_B0  <class AGXAcceleratorG16X_B0, id 0x100000abc, registered, matched, active, busy 0>
    {
      "PerformanceStatistics" = {"In use system memory (driver)"=0,"Alloc system memory"=21516763136,"Tiler Utilization %"=100,"recoveryCount"=14,"Renderer Utilization %"=99,"Device Utilization %"=42,"In use system memory"=20681818112}
    }
`

func TestParseIOAcceleratorReal(t *testing.T) {
	s, ok := parseIOAccelerator(sampleIOReg)
	if !ok {
		t.Fatal("expected ok=true for valid ioreg output")
	}
	if s.UtilizationPercent != 42 {
		t.Errorf("UtilizationPercent = %v, want 42", s.UtilizationPercent)
	}
	// Must pick the exact "In use system memory" key, not the "(driver)" decoy.
	if !s.HasInUseMemory || s.InUseMemoryBytes != 20681818112 {
		t.Errorf("InUseMemoryBytes = %d, want 20681818112", s.InUseMemoryBytes)
	}
}

func TestParseIOAcceleratorMissing(t *testing.T) {
	if _, ok := parseIOAccelerator("no accelerator here"); ok {
		t.Error("expected ok=false when no Device Utilization is present")
	}
}

func TestParseIOAcceleratorUtilOnly(t *testing.T) {
	// Utilization present but no in-use memory line: ok=true, memory absent.
	s, ok := parseIOAccelerator(`"Device Utilization %"=7`)
	if !ok {
		t.Fatal("expected ok=true")
	}
	if s.UtilizationPercent != 7 || s.HasInUseMemory {
		t.Errorf("got util=%v hasMem=%v, want util=7 hasMem=false", s.UtilizationPercent, s.HasInUseMemory)
	}
}

func TestParseIOAcceleratorZeroMemory(t *testing.T) {
	// A present zero is a real reading, distinct from a missing key.
	s, ok := parseIOAccelerator(`"Device Utilization %"=0,"In use system memory"=0`)
	if !ok {
		t.Fatal("expected ok=true")
	}
	if !s.HasInUseMemory || s.InUseMemoryBytes != 0 {
		t.Errorf("got hasMem=%v mem=%d, want hasMem=true mem=0", s.HasInUseMemory, s.InUseMemoryBytes)
	}
}
