package gpu

import (
	"context"
	"errors"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// These tests exercise the Apple Silicon poll logic with a MOCKED ioreg reader
// (no macOS or Apple hardware involved), so failure and recovery behaviour is
// covered on every CI platform.

// ioregSample builds minimal ioreg output with the given utilization and an
// optional "In use system memory" value (mem < 0 omits the key).
func ioregSample(util, mem int64) string {
	s := `"PerformanceStatistics" = {"In use system memory (driver)"=0,"Device Utilization %"=` + strconv.FormatInt(util, 10)
	if mem >= 0 {
		s += `,"In use system memory"=` + strconv.FormatInt(mem, 10)
	}
	return s + "}"
}

// mockReader replays a scripted sequence of ioreg results; once exhausted it
// keeps returning the last one. It is safe for concurrent use with run().
type mockReader struct {
	mu    sync.Mutex
	steps []mockStep
	calls int
}

type mockStep struct {
	out string
	err error
}

func (r *mockReader) read(context.Context) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	i := r.calls
	if i >= len(r.steps) {
		i = len(r.steps) - 1
	}
	r.calls++
	return r.steps[i].out, r.steps[i].err
}

func (r *mockReader) callCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls
}

func newTestSampler(t *testing.T, r *mockReader) (*appleSampler, gpuMetrics) {
	t.Helper()
	m := newGPUMetrics(prometheus.NewRegistry())
	return &appleSampler{m: m, name: "Apple M4", total: 32 << 30, read: r.read}, m
}

func TestApplePoll_ZeroMemoryOverwritesPrevious(t *testing.T) {
	r := &mockReader{steps: []mockStep{
		{out: ioregSample(40, 8<<30)},
		{out: ioregSample(0, 0)},
	}}
	s, m := newTestSampler(t, r)

	s.poll(context.Background())
	if got := testutil.ToFloat64(m.VRAMUsed.WithLabelValues("0", "Apple M4")); got != 8<<30 {
		t.Fatalf("VRAMUsed after first poll = %v, want %v", got, 8<<30)
	}
	s.poll(context.Background())
	if got := testutil.ToFloat64(m.VRAMUsed.WithLabelValues("0", "Apple M4")); got != 0 {
		t.Errorf("VRAMUsed after zero reading = %v, want 0 (stale value kept)", got)
	}
}

func TestApplePoll_MissingMemoryDeletesSeries(t *testing.T) {
	r := &mockReader{steps: []mockStep{
		{out: ioregSample(40, 8<<30)},
		{out: ioregSample(40, -1)},
	}}
	s, m := newTestSampler(t, r)

	s.poll(context.Background())
	s.poll(context.Background())
	if n := testutil.CollectAndCount(m.VRAMUsed); n != 0 {
		t.Errorf("VRAMUsed series count after missing key = %d, want 0", n)
	}
	if n := testutil.CollectAndCount(m.Utilization); n != 1 {
		t.Errorf("Utilization series count = %d, want 1", n)
	}
}

func TestApplePoll_FailureClearsAndRecovers(t *testing.T) {
	r := &mockReader{steps: []mockStep{
		{out: ioregSample(40, 8<<30)},
		{err: errors.New("mock: ioreg exited 1")},
		{out: "no accelerator here"},
		{out: ioregSample(55, 9<<30)},
	}}
	s, m := newTestSampler(t, r)
	ctx := context.Background()

	if !s.poll(ctx) {
		t.Fatal("poll 1: want success")
	}
	for i := 2; i <= 3; i++ {
		if s.poll(ctx) {
			t.Fatalf("poll %d: want failure", i)
		}
		if n := testutil.CollectAndCount(m.Utilization); n != 0 {
			t.Errorf("poll %d: Utilization series = %d, want 0 (stale value kept)", i, n)
		}
		if n := testutil.CollectAndCount(m.VRAMUsed); n != 0 {
			t.Errorf("poll %d: VRAMUsed series = %d, want 0 (stale value kept)", i, n)
		}
		if got := testutil.ToFloat64(m.VRAMTotal.WithLabelValues("0", "Apple M4")); got != 32<<30 {
			t.Errorf("poll %d: VRAMTotal = %v, want it kept at %v", i, got, 32<<30)
		}
	}

	if !s.poll(ctx) {
		t.Fatal("poll 4: want recovery")
	}
	if got := testutil.ToFloat64(m.Utilization.WithLabelValues("0", "Apple M4")); got != 55 {
		t.Errorf("Utilization after recovery = %v, want 55", got)
	}
	if got := testutil.ToFloat64(m.VRAMUsed.WithLabelValues("0", "Apple M4")); got != 9<<30 {
		t.Errorf("VRAMUsed after recovery = %v, want %v", got, 9<<30)
	}
}

func TestApplePoll_UnknownTotalNotExported(t *testing.T) {
	r := &mockReader{steps: []mockStep{{out: ioregSample(10, 1<<30)}}}
	s, m := newTestSampler(t, r)
	s.total = 0

	s.poll(context.Background())
	if n := testutil.CollectAndCount(m.VRAMTotal); n != 0 {
		t.Errorf("VRAMTotal series = %d, want 0 when total is unknown", n)
	}
}

func TestAppleRun_RetriesAfterStartupFailure(t *testing.T) {
	// The first two reads (including the immediate startup poll) fail; the
	// loop must keep polling and populate metrics once ioreg recovers.
	r := &mockReader{steps: []mockStep{
		{err: errors.New("mock: ioreg not ready")},
		{err: errors.New("mock: ioreg not ready")},
		{out: ioregSample(30, 2<<30)},
	}}
	s, m := newTestSampler(t, r)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		s.run(ctx, time.Millisecond)
		close(done)
	}()

	deadline := time.After(5 * time.Second)
	for r.callCount() < 4 {
		select {
		case <-deadline:
			cancel()
			t.Fatalf("run made %d reads, want >= 4", r.callCount())
		case <-time.After(time.Millisecond):
		}
	}
	cancel()
	<-done

	if got := testutil.ToFloat64(m.Utilization.WithLabelValues("0", "Apple M4")); got != 30 {
		t.Errorf("Utilization after recovery = %v, want 30", got)
	}
}
