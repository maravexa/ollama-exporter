package remotewrite

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// Pusher is the top-level coordinator: it ticks at a fixed flush interval,
// gathers metrics from the local registry, and fans the resulting batch
// out to each configured Sender.
//
// v0.4.0 supports a single sender, but the slice signature is preserved
// so multi-endpoint fan-out can land in v0.5 without an API break.
type Pusher struct {
	interval     time.Duration
	gatherer     prometheus.Gatherer
	extLbls      Labels
	senders      []*Sender
	logger       *slog.Logger
	drainTimeout time.Duration
}

// PusherConfig configures a Pusher.
type PusherConfig struct {
	FlushInterval  time.Duration
	Gatherer       prometheus.Gatherer
	ExternalLabels Labels
	DrainTimeout   time.Duration
	Logger         *slog.Logger
}

// NewPusher constructs a Pusher with the given senders. At least one
// sender is required.
func NewPusher(cfg PusherConfig, senders []*Sender) *Pusher {
	if cfg.FlushInterval <= 0 {
		cfg.FlushInterval = 10 * time.Second
	}
	if cfg.DrainTimeout <= 0 {
		cfg.DrainTimeout = 5 * time.Second
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	return &Pusher{
		interval:     cfg.FlushInterval,
		gatherer:     cfg.Gatherer,
		extLbls:      cfg.ExternalLabels,
		senders:      senders,
		logger:       cfg.Logger,
		drainTimeout: cfg.DrainTimeout,
	}
}

// Run starts every Sender's loop and the Pusher's flush ticker. Blocks
// until ctx is cancelled, then drains in-flight batches with a bounded
// timeout before returning.
func (p *Pusher) Run(ctx context.Context) error {
	senderCtx, cancelSenders := context.WithCancel(context.Background())
	defer cancelSenders()

	var wg sync.WaitGroup
	for _, s := range p.senders {
		wg.Add(1)
		go func(s *Sender) {
			defer wg.Done()
			s.Run(senderCtx)
		}(s)
	}

	ticker := time.NewTicker(p.interval)
	defer ticker.Stop()

	// Flush immediately so an early shutdown doesn't lose the first scrape.
	p.flush()

	for {
		select {
		case <-ctx.Done():
			p.shutdown(cancelSenders, &wg)
			return nil
		case <-ticker.C:
			p.flush()
		}
	}
}

// flush collects the registry and enqueues a copy on every sender.
func (p *Pusher) flush() {
	if p.gatherer == nil || len(p.senders) == 0 {
		return
	}
	batch, err := Gather(p.gatherer, p.extLbls, time.Now())
	if err != nil {
		p.logger.Error("gather metrics", "err", err)
		return
	}
	if len(batch) == 0 {
		return
	}
	for _, s := range p.senders {
		s.queue.Enqueue(batch)
	}
}

// shutdown gives senders a bounded window to drain their queues.
func (p *Pusher) shutdown(cancelSenders context.CancelFunc, wg *sync.WaitGroup) {
	for _, s := range p.senders {
		s.queue.Close()
	}

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		p.logger.Info("remote write drain complete")
	case <-time.After(p.drainTimeout):
		p.logger.Warn("remote write drain timeout exceeded; cancelling in-flight sends", "timeout", p.drainTimeout)
		cancelSenders()
		<-done
	}
}
