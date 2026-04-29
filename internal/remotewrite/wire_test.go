package remotewrite

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/golang/snappy"
	"github.com/prometheus/prometheus/prompb"
)

func sampleSeries(n int) []prompb.TimeSeries {
	ts := make([]prompb.TimeSeries, n)
	for i := 0; i < n; i++ {
		ts[i] = prompb.TimeSeries{
			Labels: []prompb.Label{
				{Name: "__name__", Value: "ollama_requests_total"},
				{Name: "endpoint", Value: "/api/generate"},
				{Name: "model", Value: "llama3:8b-q4"},
				{Name: "family", Value: "llama3"},
				{Name: "quant", Value: "q4_0"},
			},
			Samples: []prompb.Sample{{Value: float64(i), Timestamp: int64(i) * 1000}},
		}
	}
	return ts
}

func TestEncodeWriteRequest_Roundtrip(t *testing.T) {
	in := sampleSeries(3)
	body, err := encodeWriteRequest(in)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}

	raw, err := snappy.Decode(nil, body)
	if err != nil {
		t.Fatalf("snappy decode: %v", err)
	}

	var got prompb.WriteRequest
	if err := got.Unmarshal(raw); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(got.Timeseries) != len(in) {
		t.Fatalf("series count: got %d want %d", len(got.Timeseries), len(in))
	}
	for i := range in {
		if len(got.Timeseries[i].Labels) != len(in[i].Labels) {
			t.Errorf("series %d label count mismatch", i)
		}
		if got.Timeseries[i].Samples[0].Value != in[i].Samples[0].Value {
			t.Errorf("series %d value mismatch", i)
		}
	}
}

func TestEncodeWriteRequest_CompressionShrinksRealisticPayload(t *testing.T) {
	// 1000 series x 5 labels — repeated label names and values compress well.
	in := sampleSeries(1000)
	compressed, err := encodeWriteRequest(in)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}

	// Recover uncompressed size for comparison.
	req := &prompb.WriteRequest{Timeseries: in}
	uncompressed, err := req.Marshal()
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	if len(compressed) >= len(uncompressed) {
		t.Errorf("snappy did not reduce payload: compressed=%d uncompressed=%d", len(compressed), len(uncompressed))
	}
}

func TestSendWriteRequest_HeadersAndBody(t *testing.T) {
	SetUserAgentVersion("0.4.0-test")
	defer SetUserAgentVersion("dev")

	var receivedBody atomic.Pointer[[]byte]
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Content-Encoding"); got != "snappy" {
			t.Errorf("Content-Encoding = %q, want snappy", got)
		}
		if got := r.Header.Get("Content-Type"); got != "application/x-protobuf" {
			t.Errorf("Content-Type = %q, want application/x-protobuf", got)
		}
		if got := r.Header.Get("X-Prometheus-Remote-Write-Version"); got != "0.1.0" {
			t.Errorf("X-Prometheus-Remote-Write-Version = %q, want 0.1.0", got)
		}
		if ua := r.Header.Get("User-Agent"); !strings.HasPrefix(ua, "ollama-exporter/") {
			t.Errorf("User-Agent = %q, want prefix ollama-exporter/", ua)
		}
		if got := r.Header.Get("X-Scope-OrgID"); got != "tenant1" {
			t.Errorf("X-Scope-OrgID = %q, want tenant1", got)
		}
		body, _ := io.ReadAll(r.Body)
		receivedBody.Store(&body)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	body, err := encodeWriteRequest(sampleSeries(2))
	if err != nil {
		t.Fatalf("encode: %v", err)
	}

	err = sendWriteRequest(context.Background(), srv.Client(), srv.URL, body, map[string]string{
		"X-Scope-OrgID": "tenant1",
	})
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	if got := receivedBody.Load(); got == nil || len(*got) == 0 {
		t.Error("server received empty body")
	}
}

func TestSendWriteRequest_RejectsReservedHeaderOverride(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Content-Type"); got != "application/x-protobuf" {
			t.Errorf("Content-Type override leaked through: got %q", got)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	body, _ := encodeWriteRequest(sampleSeries(1))
	err := sendWriteRequest(context.Background(), srv.Client(), srv.URL, body, map[string]string{
		"Content-Type": "text/plain", // must be ignored
	})
	if err != nil {
		t.Fatalf("send: %v", err)
	}
}

func TestClassifyStatus(t *testing.T) {
	cases := []struct {
		name        string
		status      int
		retryAfter  string
		wantRetry   bool
		wantNonRet  bool
		wantWaitGTE time.Duration
	}{
		{"200 ok", 200, "", false, false, 0},
		{"204 ok", 204, "", false, false, 0},
		{"400 bad request", 400, "", false, true, 0},
		{"401 unauthorized", 401, "", false, true, 0},
		{"403 forbidden", 403, "", false, true, 0},
		{"404 not found", 404, "", false, true, 0},
		{"408 request timeout", 408, "", true, false, 0},
		{"429 rate limited", 429, "30", true, false, 30 * time.Second},
		{"500 server error", 500, "", true, false, 0},
		{"503 unavailable", 503, "", true, false, 0},
		{"418 teapot (default retry)", 418, "", true, false, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := http.Header{}
			if tc.retryAfter != "" {
				h.Set("Retry-After", tc.retryAfter)
			}
			resp := &http.Response{
				StatusCode: tc.status,
				Status:     strconv.Itoa(tc.status) + " test",
				Header:     h,
			}
			err := classifyStatus(resp)
			if tc.wantRetry {
				var rerr *ErrRetryable
				if !errors.As(err, &rerr) {
					t.Fatalf("expected ErrRetryable, got %T (%v)", err, err)
				}
				if rerr.RetryAfter < tc.wantWaitGTE {
					t.Errorf("RetryAfter = %v, want >= %v", rerr.RetryAfter, tc.wantWaitGTE)
				}
			} else if tc.wantNonRet {
				var nerr *ErrNonRetryable
				if !errors.As(err, &nerr) {
					t.Fatalf("expected ErrNonRetryable, got %T (%v)", err, err)
				}
			} else {
				if err != nil {
					t.Errorf("expected nil for status %d, got %v", tc.status, err)
				}
			}
		})
	}
}

func TestParseRetryAfter_HTTPDate(t *testing.T) {
	now := time.Date(2026, 4, 29, 12, 0, 0, 0, time.UTC)
	in := now.Add(45 * time.Second).UTC().Format(http.TimeFormat)
	got := parseRetryAfter(in, now)
	if got < 30*time.Second || got > 60*time.Second {
		t.Errorf("parseRetryAfter HTTP-date = %v, want ~45s", got)
	}
}

func TestParseRetryAfter_NegativeOrInvalid(t *testing.T) {
	now := time.Now()
	if d := parseRetryAfter("-5", now); d != 0 {
		t.Errorf("negative delta should be 0, got %v", d)
	}
	if d := parseRetryAfter("not-a-date", now); d != 0 {
		t.Errorf("garbage should be 0, got %v", d)
	}
	if d := parseRetryAfter("", now); d != 0 {
		t.Errorf("empty should be 0, got %v", d)
	}
}

// closeTrackingBody verifies the wire layer drains and closes the response
// body even on non-2xx responses.
type closeTrackingBody struct {
	io.Reader
	closed atomic.Bool
}

func (c *closeTrackingBody) Close() error {
	c.closed.Store(true)
	return nil
}

func TestSendWriteRequest_BodyDrainedOnError(t *testing.T) {
	// Custom RoundTripper that returns a tracked body.
	tracked := &closeTrackingBody{Reader: strings.NewReader("error detail payload")}
	rt := roundTripperFunc(func(_ *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: 500,
			Status:     "500 Internal Server Error",
			Body:       tracked,
			Header:     http.Header{},
		}, nil
	})
	client := &http.Client{Transport: rt}

	body, _ := encodeWriteRequest(sampleSeries(1))
	err := sendWriteRequest(context.Background(), client, "http://example.invalid/push", body, nil)
	if err == nil {
		t.Fatal("expected error from 500 response")
	}
	if !tracked.closed.Load() {
		t.Error("response body was not closed")
	}
	// Verify the reader was drained (Close happens after Copy).
	if _, err := tracked.Reader.(*strings.Reader).ReadByte(); err != io.EOF {
		t.Errorf("body was not drained: %v", err)
	}
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
