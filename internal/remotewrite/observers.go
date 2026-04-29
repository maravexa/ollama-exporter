package remotewrite

import "time"

// QueueObserver receives queue lifecycle events. Prompt 5 plugs a real
// Prometheus-backed implementation in here; tests pass a stub or nopObserver.
type QueueObserver interface {
	OnEnqueue(length int)
	OnDequeue(length int)
	// OnDrop is fired when a batch is evicted due to capacity pressure.
	// samplesDropped is the number of TimeSeries lost so the operator can
	// distinguish a single big batch from many small ones.
	OnDrop(reason string, samplesDropped int)
	// SetCapacity is called once at queue construction.
	SetCapacity(capacity int)
}

// SenderObserver receives per-batch outcome events from a Sender.
type SenderObserver interface {
	OnSendOutcome(outcome string, duration time.Duration)
	OnSamplesSent(n int)
	OnSamplesFailed(reason string, n int)
	OnSamplesDropped(reason string, n int)
	OnRetry()
	OnLastSend(t time.Time)
}

// BreakerObserver receives circuit breaker state changes.
type BreakerObserver interface {
	// OnStateChange is invoked when the breaker transitions between
	// closed (0), open (1), and half-open (2).
	OnStateChange(state int)
}

// nopQueueObserver is a no-op implementation used in tests and as a
// safe default when wiring is incomplete.
type nopQueueObserver struct{}

func (nopQueueObserver) OnEnqueue(int)      {}
func (nopQueueObserver) OnDequeue(int)      {}
func (nopQueueObserver) OnDrop(string, int) {}
func (nopQueueObserver) SetCapacity(int)    {}

type nopSenderObserver struct{}

func (nopSenderObserver) OnSendOutcome(string, time.Duration) {}
func (nopSenderObserver) OnSamplesSent(int)                   {}
func (nopSenderObserver) OnSamplesFailed(string, int)         {}
func (nopSenderObserver) OnSamplesDropped(string, int)        {}
func (nopSenderObserver) OnRetry()                            {}
func (nopSenderObserver) OnLastSend(time.Time)                {}

type nopBreakerObserver struct{}

func (nopBreakerObserver) OnStateChange(int) {}
