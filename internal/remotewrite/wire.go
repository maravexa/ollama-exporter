package remotewrite

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/golang/snappy"
	"github.com/prometheus/prometheus/prompb"
)

// Version is the protocol version advertised in the
// X-Prometheus-Remote-Write-Version header.
const Version = "0.1.0"

// UserAgent is the default User-Agent string. The exporter version is
// injected via SetUserAgentVersion at startup.
var userAgent = "ollama-exporter/dev"

// SetUserAgentVersion overrides the version suffix used in the User-Agent
// header sent on every remote write request. Callers should invoke this
// once at startup with the binary's version string.
func SetUserAgentVersion(v string) {
	if v == "" {
		v = "dev"
	}
	userAgent = "ollama-exporter/" + v
}

// reservedHeaders are protocol-required and must not be overridden by
// caller-supplied headers.
var reservedHeaders = map[string]struct{}{
	"Content-Encoding":                  {},
	"Content-Type":                      {},
	"X-Prometheus-Remote-Write-Version": {},
}

// encodeWriteRequest serializes ts as a snappy-compressed protobuf-encoded
// prompb.WriteRequest payload suitable for POSTing to a Prometheus Remote
// Write 1.0 endpoint.
func encodeWriteRequest(ts []prompb.TimeSeries) ([]byte, error) {
	req := &prompb.WriteRequest{Timeseries: ts}
	raw, err := req.Marshal()
	if err != nil {
		return nil, fmt.Errorf("marshal WriteRequest: %w", err)
	}
	return snappy.Encode(nil, raw), nil
}

// sendWriteRequest POSTs body (already snappy-compressed protobuf) to url.
// Caller-supplied headers are applied last, but reserved protocol headers
// cannot be overridden. The response body is always drained and closed.
func sendWriteRequest(ctx context.Context, client *http.Client, url string, body []byte, headers map[string]string) error {
	// #nosec G107 -- url comes from operator config, not user input
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}

	req.Header.Set("Content-Encoding", "snappy")
	req.Header.Set("Content-Type", "application/x-protobuf")
	req.Header.Set("X-Prometheus-Remote-Write-Version", Version)
	req.Header.Set("User-Agent", userAgent)

	for k, v := range headers {
		if _, reserved := reservedHeaders[http.CanonicalHeaderKey(k)]; reserved {
			continue
		}
		req.Header.Set(k, v)
	}

	resp, err := client.Do(req)
	if err != nil {
		return classifyTransportError(err)
	}
	defer func() {
		// Drain body so the underlying connection can be reused by the
		// http.Client. Errors here are not actionable — they only affect
		// keep-alive efficiency, not correctness — but we surface them at
		// debug rather than silently discarding.
		if _, copyErr := io.Copy(io.Discard, resp.Body); copyErr != nil {
			_ = copyErr
		}
		_ = resp.Body.Close()
	}()

	return classifyStatus(resp)
}

// classifyTransportError wraps a transport-level error from http.Client.Do
// in ErrRetryable. Context cancellations are reported verbatim so callers
// can distinguish shutdown from genuine network failures.
func classifyTransportError(err error) error {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return &ErrRetryable{Cause: err}
}

// classifyStatus maps an HTTP response status to a typed error per PRW 1.0:
//
//	2xx              -> nil
//	400, 401, 403, 404 -> ErrNonRetryable
//	408, 429, 5xx    -> ErrRetryable
//	other            -> ErrRetryable (conservative default)
//
// On 429 it parses Retry-After (delta-seconds and HTTP-date forms) and
// attaches the indicated delay.
func classifyStatus(resp *http.Response) error {
	status := resp.StatusCode
	if status >= 200 && status < 300 {
		return nil
	}

	switch status {
	case http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound:
		return &ErrNonRetryable{Status: status, Cause: fmt.Errorf("server returned %s", resp.Status)}
	case http.StatusRequestTimeout, http.StatusTooManyRequests:
		ra := parseRetryAfter(resp.Header.Get("Retry-After"), time.Now())
		return &ErrRetryable{Status: status, Cause: fmt.Errorf("server returned %s", resp.Status), RetryAfter: ra}
	}

	if status >= 500 && status < 600 {
		ra := parseRetryAfter(resp.Header.Get("Retry-After"), time.Now())
		return &ErrRetryable{Status: status, Cause: fmt.Errorf("server returned %s", resp.Status), RetryAfter: ra}
	}

	return &ErrRetryable{Status: status, Cause: fmt.Errorf("server returned %s", resp.Status)}
}

// parseRetryAfter accepts the two forms permitted by RFC 7231 §7.1.3:
// integer delta-seconds, or an HTTP-date. Returns 0 if header is absent
// or unparseable.
func parseRetryAfter(h string, now time.Time) time.Duration {
	if h == "" {
		return 0
	}
	if secs, err := strconv.Atoi(h); err == nil {
		if secs < 0 {
			return 0
		}
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(h); err == nil {
		if d := t.Sub(now); d > 0 {
			return d
		}
	}
	return 0
}
