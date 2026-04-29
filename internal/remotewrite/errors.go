// Package remotewrite implements a Prometheus Remote Write 1.0 client
// for pushing local registry samples to a remote write endpoint.
package remotewrite

import (
	"errors"
	"fmt"
	"time"
)

// ErrNonRetryable indicates the receiver rejected the batch and the
// exporter must not retry. Caller should drop the batch and (optionally)
// record a circuit-breaker failure.
type ErrNonRetryable struct {
	Status int
	Cause  error
}

func (e *ErrNonRetryable) Error() string {
	return fmt.Sprintf("non-retryable remote write error (status=%d): %v", e.Status, e.Cause)
}

func (e *ErrNonRetryable) Unwrap() error { return e.Cause }

// ErrRetryable indicates a transient failure. Caller should back off and
// retry within its retry budget. RetryAfter, when non-zero, is the
// receiver-requested minimum delay before the next attempt (from a
// Retry-After response header on a 429).
type ErrRetryable struct {
	Status     int
	Cause      error
	RetryAfter time.Duration
}

func (e *ErrRetryable) Error() string {
	if e.RetryAfter > 0 {
		return fmt.Sprintf("retryable remote write error (status=%d, retry_after=%s): %v", e.Status, e.RetryAfter, e.Cause)
	}
	return fmt.Sprintf("retryable remote write error (status=%d): %v", e.Status, e.Cause)
}

func (e *ErrRetryable) Unwrap() error { return e.Cause }

// IsRetryable reports whether err is or wraps an ErrRetryable.
func IsRetryable(err error) bool {
	var r *ErrRetryable
	return errors.As(err, &r)
}

// IsNonRetryable reports whether err is or wraps an ErrNonRetryable.
func IsNonRetryable(err error) bool {
	var n *ErrNonRetryable
	return errors.As(err, &n)
}
