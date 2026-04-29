package remotewrite

import (
	"context"
	"errors"
	"sync"

	"github.com/prometheus/prometheus/prompb"
)

// ErrQueueClosed is returned by Dequeue after the queue has been Closed
// and no items remain.
var ErrQueueClosed = errors.New("remote write queue closed")

// Queue is a fixed-capacity ring buffer of TimeSeries batches with
// drop-oldest overflow semantics. Enqueue is non-blocking; Dequeue blocks
// until an item is available or the context is cancelled.
//
// Implemented with sync.Mutex + sync.Cond. A buffered channel cannot
// implement drop-oldest cleanly because there is no race-free way to peek
// at the head and discard it from a producer goroutine.
type Queue struct {
	mu       sync.Mutex
	cond     *sync.Cond
	buf      [][]prompb.TimeSeries
	head     int
	size     int
	cap      int
	closed   bool
	observer QueueObserver
}

// NewQueue constructs a Queue with the given capacity (in batches, not
// samples). observer may be nil; a no-op observer is substituted.
func NewQueue(capacity int, observer QueueObserver) *Queue {
	if capacity < 1 {
		capacity = 1
	}
	if observer == nil {
		observer = nopQueueObserver{}
	}
	q := &Queue{
		buf:      make([][]prompb.TimeSeries, capacity),
		cap:      capacity,
		observer: observer,
	}
	q.cond = sync.NewCond(&q.mu)
	observer.SetCapacity(capacity)
	return q
}

// Enqueue adds batch to the queue. If the queue is full, the oldest batch
// is dropped first and the observer is notified with reason="queue_full".
// Enqueue never blocks.
func (q *Queue) Enqueue(batch []prompb.TimeSeries) {
	q.mu.Lock()
	if q.closed {
		q.mu.Unlock()
		return
	}

	var droppedSize int
	if q.size == q.cap {
		dropped := q.buf[q.head]
		droppedSize = len(dropped)
		q.buf[q.head] = nil
		q.head = (q.head + 1) % q.cap
		q.size--
	}

	tail := (q.head + q.size) % q.cap
	q.buf[tail] = batch
	q.size++
	length := q.size

	q.cond.Signal()
	q.mu.Unlock()

	if droppedSize > 0 {
		q.observer.OnDrop("queue_full", droppedSize)
	}
	q.observer.OnEnqueue(length)
}

// Dequeue blocks until a batch is available, returning it, or until ctx is
// cancelled (returns ctx.Err()), or until the queue is closed (returns
// ErrQueueClosed once drained).
func (q *Queue) Dequeue(ctx context.Context) ([]prompb.TimeSeries, error) {
	q.mu.Lock()

	// Wake the waiter if the context is cancelled. We need an explicit
	// goroutine because sync.Cond does not interoperate with channels.
	stopWatch := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			q.mu.Lock()
			q.cond.Broadcast()
			q.mu.Unlock()
		case <-stopWatch:
		}
	}()
	defer close(stopWatch)

	for q.size == 0 && !q.closed && ctx.Err() == nil {
		q.cond.Wait()
	}
	if q.size == 0 {
		q.mu.Unlock()
		if q.closed {
			return nil, ErrQueueClosed
		}
		return nil, ctx.Err()
	}

	batch := q.buf[q.head]
	q.buf[q.head] = nil
	q.head = (q.head + 1) % q.cap
	q.size--
	length := q.size
	q.mu.Unlock()

	q.observer.OnDequeue(length)
	return batch, nil
}

// Len returns the number of batches currently buffered.
func (q *Queue) Len() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.size
}

// Cap returns the configured capacity in batches.
func (q *Queue) Cap() int {
	return q.cap
}

// Close marks the queue closed and wakes any pending Dequeue callers.
// Items already in the queue remain available to be drained.
func (q *Queue) Close() {
	q.mu.Lock()
	q.closed = true
	q.cond.Broadcast()
	q.mu.Unlock()
}
