package remotewrite

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/prometheus/prompb"
)

type recordingObserver struct {
	mu          sync.Mutex
	enqueued    []int
	dequeued    []int
	drops       []dropRecord
	capacitySet int
}

type dropRecord struct {
	reason string
	n      int
}

func (r *recordingObserver) OnEnqueue(l int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.enqueued = append(r.enqueued, l)
}
func (r *recordingObserver) OnDequeue(l int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.dequeued = append(r.dequeued, l)
}
func (r *recordingObserver) OnDrop(reason string, n int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.drops = append(r.drops, dropRecord{reason: reason, n: n})
}
func (r *recordingObserver) SetCapacity(c int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.capacitySet = c
}

func mkBatch(id int) []prompb.TimeSeries {
	return []prompb.TimeSeries{{
		Labels:  []prompb.Label{{Name: "__name__", Value: "x"}, {Name: "id", Value: itoa(id)}},
		Samples: []prompb.Sample{{Value: float64(id), Timestamp: int64(id)}},
	}}
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	neg := i < 0
	if neg {
		i = -i
	}
	var b [20]byte
	pos := len(b)
	for i > 0 {
		pos--
		b[pos] = byte('0' + i%10)
		i /= 10
	}
	if neg {
		pos--
		b[pos] = '-'
	}
	return string(b[pos:])
}

func batchID(b []prompb.TimeSeries) string {
	for _, l := range b[0].Labels {
		if l.Name == "id" {
			return l.Value
		}
	}
	return ""
}

func TestQueue_DropOldestOnOverflow(t *testing.T) {
	obs := &recordingObserver{}
	q := NewQueue(3, obs)

	q.Enqueue(mkBatch(1))
	q.Enqueue(mkBatch(2))
	q.Enqueue(mkBatch(3))
	q.Enqueue(mkBatch(4)) // should evict batch 1

	if obs.capacitySet != 3 {
		t.Errorf("SetCapacity not called with 3, got %d", obs.capacitySet)
	}
	if len(obs.drops) != 1 || obs.drops[0].reason != "queue_full" {
		t.Errorf("expected one drop with reason queue_full, got %+v", obs.drops)
	}

	ctx := context.Background()
	got := []string{}
	for i := 0; i < 3; i++ {
		b, err := q.Dequeue(ctx)
		if err != nil {
			t.Fatalf("dequeue: %v", err)
		}
		got = append(got, batchID(b))
	}
	want := []string{"2", "3", "4"}
	if !equal(got, want) {
		t.Errorf("dequeued ids = %v, want %v (oldest dropped)", got, want)
	}
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestQueue_DequeueBlocksUntilEnqueue(t *testing.T) {
	q := NewQueue(2, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	done := make(chan []prompb.TimeSeries, 1)
	go func() {
		b, err := q.Dequeue(ctx)
		if err != nil {
			done <- nil
			return
		}
		done <- b
	}()

	time.Sleep(20 * time.Millisecond)
	q.Enqueue(mkBatch(42))

	select {
	case b := <-done:
		if b == nil || batchID(b) != "42" {
			t.Errorf("got %v, want batch 42", b)
		}
	case <-time.After(time.Second):
		t.Fatal("dequeue did not unblock after enqueue")
	}
}

func TestQueue_DequeueRespectsContext(t *testing.T) {
	q := NewQueue(2, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := q.Dequeue(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
}

func TestQueue_CloseUnblocksDequeue(t *testing.T) {
	q := NewQueue(2, nil)
	done := make(chan error, 1)
	go func() {
		_, err := q.Dequeue(context.Background())
		done <- err
	}()
	time.Sleep(20 * time.Millisecond)
	q.Close()
	select {
	case err := <-done:
		if !errors.Is(err, ErrQueueClosed) {
			t.Errorf("err = %v, want ErrQueueClosed", err)
		}
	case <-time.After(time.Second):
		t.Fatal("dequeue did not unblock on Close")
	}
}

func TestQueue_ConcurrentProducersConsumers(t *testing.T) {
	q := NewQueue(64, nil)
	const producers = 8
	const consumers = 4
	const perProducer = 200

	var produced atomic.Int64
	var consumed atomic.Int64

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var wg sync.WaitGroup
	for p := 0; p < producers; p++ {
		wg.Add(1)
		go func(p int) {
			defer wg.Done()
			for i := 0; i < perProducer; i++ {
				q.Enqueue(mkBatch(p*1000 + i))
				produced.Add(1)
			}
		}(p)
	}

	consumerDone := make(chan struct{})
	var cwg sync.WaitGroup
	for c := 0; c < consumers; c++ {
		cwg.Add(1)
		go func() {
			defer cwg.Done()
			for {
				_, err := q.Dequeue(ctx)
				if err != nil {
					return
				}
				consumed.Add(1)
			}
		}()
	}

	wg.Wait()
	// Allow consumers to drain.
	for range time.NewTicker(10 * time.Millisecond).C {
		if q.Len() == 0 {
			break
		}
	}
	cancel()
	go func() {
		cwg.Wait()
		close(consumerDone)
	}()
	<-consumerDone

	if produced.Load() != int64(producers*perProducer) {
		t.Errorf("produced = %d, want %d", produced.Load(), producers*perProducer)
	}
	if consumed.Load() == 0 {
		t.Error("no batches consumed")
	}
	// Drop-oldest semantics mean consumed <= produced; just ensure no panics.
}
