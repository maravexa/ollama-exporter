package collector

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	io_prometheus_client "github.com/prometheus/client_model/go"
)

// gaugeValue returns the value of the single series in a gauge metric family,
// or -1 when the family has not been observed yet.
func gaugeValue(t *testing.T, reg *prometheus.Registry, name string) float64 {
	t.Helper()
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, mf := range mfs {
		if mf.GetName() == name && mf.GetType() == io_prometheus_client.MetricType_GAUGE {
			for _, m := range mf.GetMetric() {
				return m.GetGauge().GetValue()
			}
		}
	}
	return -1
}

// counterValue sums a counter family across all label combinations.
func counterValue(t *testing.T, reg *prometheus.Registry, name string) float64 {
	t.Helper()
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	var total float64
	for _, mf := range mfs {
		if mf.GetName() == name && mf.GetType() == io_prometheus_client.MetricType_COUNTER {
			for _, m := range mf.GetMetric() {
				total += m.GetCounter().GetValue()
			}
		}
	}
	return total
}

// streamedBody is what Ollama returns for {"stream": true}: one JSON object
// per line, with the timing fields present only on the final done=true line.
const streamedBody = `{"model":"llama3.1","created_at":"2026-07-18T12:00:00Z","response":"Hello","done":false}
{"model":"llama3.1","created_at":"2026-07-18T12:00:01Z","response":" there","done":false}
{"model":"llama3.1","created_at":"2026-07-18T12:00:02Z","response":"","done":true,"total_duration":2000000000,"load_duration":100000000,"prompt_eval_count":16,"prompt_eval_duration":126112000,"eval_count":100,"eval_duration":4000000000}
`

// singleBody is what Ollama returns for {"stream": false}: one JSON object,
// no trailing newline.
const singleBody = `{"model":"llama3.1","created_at":"2026-07-18T12:00:02Z","response":"Hello there","done":true,"total_duration":2000000000,"load_duration":100000000,"prompt_eval_count":16,"prompt_eval_duration":126112000,"eval_count":100,"eval_duration":4000000000}`

// newInferenceResponse builds an *http.Response as the reverse proxy would
// hand it to ModifyResponse for a POST to path.
func newInferenceResponse(path, body string) *http.Response {
	req := httptest.NewRequest(http.MethodPost, path, nil)
	return &http.Response{
		StatusCode: http.StatusOK,
		Request:    req,
		Body:       io.NopCloser(strings.NewReader(body)),
		Header:     http.Header{"Content-Type": []string{"application/x-ndjson"}},
	}
}

// drain consumes and closes a response body the way a downstream client does,
// returning exactly what the client received.
func drain(t *testing.T, resp *http.Response) string {
	t.Helper()
	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if err := resp.Body.Close(); err != nil {
		t.Fatalf("close body: %v", err)
	}
	return string(got)
}

// assertTimings checks the derived metrics produced by both fixture bodies,
// which carry identical timing fields.
func assertTimings(t *testing.T, reg *prometheus.Registry) {
	t.Helper()

	// eval_count 100 / eval_duration 4s = 25 tok/s
	if got := gaugeValue(t, reg, "ollama_tokens_per_second"); got != 25 {
		t.Errorf("ollama_tokens_per_second = %v, want 25", got)
	}
	// prompt_eval_count 16 / prompt_eval_duration 0.126112s
	if got := gaugeValue(t, reg, "ollama_prompt_tokens_per_second"); got < 126 || got > 127 {
		t.Errorf("ollama_prompt_tokens_per_second = %v, want ~126.9", got)
	}
	// 126112000ns / 16 tokens
	if got := gaugeValue(t, reg, "ollama_kv_cache_pressure_ratio"); got != 7882000 {
		t.Errorf("ollama_kv_cache_pressure_ratio = %v, want 7882000", got)
	}
	if got := histSampleCount(t, reg, "ollama_eval_duration_seconds"); got != 1 {
		t.Errorf("ollama_eval_duration_seconds count = %d, want 1", got)
	}
	if got := histSampleCount(t, reg, "ollama_prompt_eval_duration_seconds"); got != 1 {
		t.Errorf("ollama_prompt_eval_duration_seconds count = %d, want 1", got)
	}
	if got := counterValue(t, reg, "ollama_requests_total"); got != 1 {
		t.Errorf("ollama_requests_total = %v, want 1", got)
	}
}

// The regression this PR fixes: a streamed NDJSON response must produce the
// same timing metrics as a non-streamed one.
func TestProxy_StreamingNDJSONResponse_RecordsTimings(t *testing.T) {
	proxy, reg := newTestProxy(t, nil)

	resp := newInferenceResponse("/api/generate", streamedBody)
	if err := proxy.modifyResponse(resp); err != nil {
		t.Fatalf("modifyResponse: %v", err)
	}

	if got := drain(t, resp); got != streamedBody {
		t.Errorf("client body was altered:\ngot  %q\nwant %q", got, streamedBody)
	}

	assertTimings(t, reg)
}

func TestProxy_SingleObjectResponse_RecordsTimings(t *testing.T) {
	proxy, reg := newTestProxy(t, nil)

	resp := newInferenceResponse("/api/generate", singleBody)
	if err := proxy.modifyResponse(resp); err != nil {
		t.Fatalf("modifyResponse: %v", err)
	}

	if got := drain(t, resp); got != singleBody {
		t.Errorf("client body was altered:\ngot  %q\nwant %q", got, singleBody)
	}

	assertTimings(t, reg)
}

func TestProxy_ChatEndpoint_StreamingRecordsTimings(t *testing.T) {
	proxy, reg := newTestProxy(t, nil)

	// /api/chat nests the token under "message" but carries the same timings.
	body := `{"model":"llama3.1","message":{"role":"assistant","content":"hi"},"done":false}
{"model":"llama3.1","done":true,"eval_count":100,"eval_duration":4000000000,"prompt_eval_count":16,"prompt_eval_duration":126112000,"load_duration":100000000}
`
	resp := newInferenceResponse("/api/chat", body)
	if err := proxy.modifyResponse(resp); err != nil {
		t.Fatalf("modifyResponse: %v", err)
	}
	drain(t, resp)

	assertTimings(t, reg)
}

// The proxy must not buffer: bytes written upstream have to be readable
// downstream before the stream terminates, or interactive streaming breaks.
func TestProxy_StreamingResponse_IsNotBuffered(t *testing.T) {
	proxy, _ := newTestProxy(t, nil)

	pr, pw := io.Pipe()
	req := httptest.NewRequest(http.MethodPost, "/api/generate", nil)
	resp := &http.Response{StatusCode: http.StatusOK, Request: req, Body: pr, Header: http.Header{}}

	if err := proxy.modifyResponse(resp); err != nil {
		t.Fatalf("modifyResponse: %v", err)
	}

	first := `{"model":"llama3.1","response":"tok","done":false}` + "\n"
	go func() {
		_, _ = pw.Write([]byte(first))
	}()

	buf := make([]byte, len(first))
	n, err := io.ReadFull(resp.Body, buf)
	if err != nil {
		t.Fatalf("first chunk not readable before stream end: %v", err)
	}
	if string(buf[:n]) != first {
		t.Errorf("first chunk = %q, want %q", string(buf[:n]), first)
	}
	_ = pw.Close()
	_ = resp.Body.Close()
}

// A stream the client abandons part-way has no done=true object, so no
// timings should be recorded — but the proxy must not panic or leak.
func TestProxy_IncompleteStream_RecordsNoTimings(t *testing.T) {
	proxy, reg := newTestProxy(t, nil)

	body := `{"model":"llama3.1","response":"tok","done":false}` + "\n"
	resp := newInferenceResponse("/api/generate", body)
	if err := proxy.modifyResponse(resp); err != nil {
		t.Fatalf("modifyResponse: %v", err)
	}
	drain(t, resp)

	if got := histSampleCount(t, reg, "ollama_eval_duration_seconds"); got != 0 {
		t.Errorf("ollama_eval_duration_seconds count = %d, want 0", got)
	}
	if got := counterValue(t, reg, "ollama_requests_total"); got != 0 {
		t.Errorf("ollama_requests_total = %v, want 0", got)
	}
}

// Non-inference endpoints and non-POST requests must pass through untouched.
func TestProxy_NonInferenceResponse_Untouched(t *testing.T) {
	proxy, reg := newTestProxy(t, nil)

	resp := newInferenceResponse("/api/tags", singleBody)
	if err := proxy.modifyResponse(resp); err != nil {
		t.Fatalf("modifyResponse: %v", err)
	}
	drain(t, resp)

	if got := counterValue(t, reg, "ollama_requests_total"); got != 0 {
		t.Errorf("ollama_requests_total = %v, want 0 for /api/tags", got)
	}
}

// Timings must be emitted exactly once even though both Read (at EOF) and
// Close reach the finaliser.
func TestProxy_TimingsEmittedOnce(t *testing.T) {
	proxy, reg := newTestProxy(t, nil)

	resp := newInferenceResponse("/api/generate", streamedBody)
	if err := proxy.modifyResponse(resp); err != nil {
		t.Fatalf("modifyResponse: %v", err)
	}
	drain(t, resp)
	_ = resp.Body.Close() // double close, as some clients do

	if got := counterValue(t, reg, "ollama_requests_total"); got != 1 {
		t.Errorf("ollama_requests_total = %v, want 1 (emitted exactly once)", got)
	}
}

// A line larger than maxTimingLineBytes is dropped rather than accumulated,
// and must not corrupt parsing of the lines around it.
func TestProxy_OversizedLine_DroppedButStreamStillParsed(t *testing.T) {
	proxy, reg := newTestProxy(t, nil)

	huge := `{"model":"llama3.1","response":"` + strings.Repeat("x", maxTimingLineBytes+16) + `","done":false}`
	body := huge + "\n" +
		`{"model":"llama3.1","done":true,"eval_count":100,"eval_duration":4000000000,"prompt_eval_count":16,"prompt_eval_duration":126112000,"load_duration":100000000}` + "\n"

	resp := newInferenceResponse("/api/generate", body)
	if err := proxy.modifyResponse(resp); err != nil {
		t.Fatalf("modifyResponse: %v", err)
	}
	if got := drain(t, resp); got != body {
		t.Error("client body was altered by oversized-line handling")
	}

	assertTimings(t, reg)
}
