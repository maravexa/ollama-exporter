package collector

import (
	"io"
	"strings"
	"testing"
	"time"
)

// collect runs body through a usageScanner and returns the emitted sample.
// It asserts the scanner is byte-transparent: what the client reads must be
// exactly what upstream sent.
func collect(t *testing.T, body, fallback string) (UsageSample, bool) {
	t.Helper()

	var got UsageSample
	var fired bool

	s := newUsageScanner(io.NopCloser(strings.NewReader(body)), fallback, func(u UsageSample) {
		got = u
		fired = true
	})

	out, err := io.ReadAll(s)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(out) != body {
		t.Fatalf("scanner altered the body:\n got: %q\nwant: %q", out, body)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	return got, fired
}

func TestUsageScanner_AnthropicStreaming(t *testing.T) {
	// message_start carries the model and input tokens; message_delta
	// carries output tokens. Neither frame has both.
	body := strings.Join([]string{
		"event: message_start",
		`data: {"type":"message_start","message":{"model":"qwen3.6:35b","usage":{"input_tokens":41,"output_tokens":1}}}`,
		"",
		"event: content_block_delta",
		`data: {"type":"content_block_delta","delta":{"type":"text_delta","text":"hi"}}`,
		"",
		"event: message_delta",
		`data: {"type":"message_delta","usage":{"output_tokens":128}}`,
		"",
		"event: message_stop",
		`data: {"type":"message_stop"}`,
		"",
	}, "\n")

	got, fired := collect(t, body, "")
	if !fired {
		t.Fatal("expected emit")
	}
	if got.Model != "qwen3.6:35b" {
		t.Errorf("model = %q, want qwen3.6:35b", got.Model)
	}
	if got.InputTokens != 41 {
		t.Errorf("input = %d, want 41", got.InputTokens)
	}
	if got.OutputTokens != 128 {
		t.Errorf("output = %d, want 128", got.OutputTokens)
	}
}

func TestUsageScanner_AnthropicNonStreaming(t *testing.T) {
	body := `{"id":"msg_1","type":"message","model":"qwen3.6:35b",` +
		`"content":[{"type":"text","text":"hi"}],` +
		`"usage":{"input_tokens":12,"output_tokens":34}}`

	got, fired := collect(t, body, "")
	if !fired {
		t.Fatal("expected emit for single-object body with no trailing newline")
	}
	if got.InputTokens != 12 || got.OutputTokens != 34 {
		t.Errorf("got in=%d out=%d, want 12/34", got.InputTokens, got.OutputTokens)
	}
}

func TestUsageScanner_OpenAIStreamingWithDone(t *testing.T) {
	body := strings.Join([]string{
		`data: {"model":"qwen3.6:35b","choices":[{"delta":{"content":"h"}}]}`,
		`data: {"model":"qwen3.6:35b","choices":[{"delta":{"content":"i"}}]}`,
		`data: {"model":"qwen3.6:35b","choices":[],"usage":{"prompt_tokens":7,"completion_tokens":91}}`,
		"data: [DONE]",
		"",
	}, "\n")

	got, fired := collect(t, body, "")
	if !fired {
		t.Fatal("expected emit")
	}
	if got.InputTokens != 7 || got.OutputTokens != 91 {
		t.Errorf("got in=%d out=%d, want 7/91", got.InputTokens, got.OutputTokens)
	}
}

func TestUsageScanner_FallbackModelUsedWhenResponseOmitsIt(t *testing.T) {
	body := `data: {"choices":[],"usage":{"prompt_tokens":3,"completion_tokens":9}}` + "\n"

	got, fired := collect(t, body, "from-request")
	if !fired {
		t.Fatal("expected emit")
	}
	if got.Model != "from-request" {
		t.Errorf("model = %q, want from-request", got.Model)
	}
}

func TestUsageScanner_NoEmitWithoutOutputTokens(t *testing.T) {
	// A content-only stream that never reports usage must not produce a
	// throughput sample, otherwise the gauge would be set from a divide
	// with no numerator.
	body := strings.Join([]string{
		`data: {"model":"m","choices":[{"delta":{"content":"a"}}]}`,
		"data: [DONE]",
		"",
	}, "\n")

	if _, fired := collect(t, body, "m"); fired {
		t.Fatal("expected no emit when the response carries no output token count")
	}
}

func TestUsageScanner_EmitsOnceOnCloseAfterEarlyAbandon(t *testing.T) {
	body := `{"model":"m","usage":{"input_tokens":1,"output_tokens":2}}`

	var calls int
	s := newUsageScanner(io.NopCloser(strings.NewReader(body)), "", func(UsageSample) {
		calls++
	})

	// Read one byte, then abandon the stream the way a disconnecting
	// client would.
	buf := make([]byte, 1)
	if _, err := s.Read(buf); err != nil {
		t.Fatalf("read: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("second close: %v", err)
	}
	if calls > 1 {
		t.Fatalf("emit called %d times, want at most 1", calls)
	}
}

func TestUsageScanner_OversizeLineDropped(t *testing.T) {
	huge := strings.Repeat("x", maxUsageLineBytes+16)
	body := "data: {\"junk\":\"" + huge + "\"}\n" +
		`data: {"model":"m","usage":{"output_tokens":5}}` + "\n"

	got, fired := collect(t, body, "")
	if !fired {
		t.Fatal("expected the following valid frame to still be parsed")
	}
	if got.OutputTokens != 5 {
		t.Errorf("output = %d, want 5", got.OutputTokens)
	}
}

func TestUsageSample_Valid(t *testing.T) {
	cases := []struct {
		name string
		u    UsageSample
		want bool
	}{
		{"complete", UsageSample{Model: "m", OutputTokens: 1, Elapsed: time.Second}, true},
		{"no model", UsageSample{OutputTokens: 1, Elapsed: time.Second}, false},
		{"no tokens", UsageSample{Model: "m", Elapsed: time.Second}, false},
		{"no elapsed", UsageSample{Model: "m", OutputTokens: 1}, false},
	}
	for _, c := range cases {
		if got := c.u.Valid(); got != c.want {
			t.Errorf("%s: Valid() = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestSSEData(t *testing.T) {
	cases := []struct {
		line    string
		payload string
		ok      bool
	}{
		{`data: {"a":1}`, `{"a":1}`, true},
		{`data:{"a":1}`, `{"a":1}`, true},
		{"data: [DONE]", "", false},
		{"event: message_start", "", false},
		{": keepalive comment", "", false},
		{"id: 42", "", false},
		{"retry: 1000", "", false},
		{`{"a":1}`, `{"a":1}`, true},
		{"", "", false},
	}
	for _, c := range cases {
		got, ok := sseData([]byte(c.line))
		if ok != c.ok || string(got) != c.payload {
			t.Errorf("sseData(%q) = (%q, %v), want (%q, %v)", c.line, got, ok, c.payload, c.ok)
		}
	}
}
