package collector

import (
	"bytes"
	"encoding/json"
	"io"
	"time"
)

// maxUsageLineBytes bounds the memory held while reassembling a single line
// of an SSE or JSON response. Usage-bearing frames are well under a
// kilobyte; anything larger is not one, so the line is discarded rather
// than accumulated.
const maxUsageLineBytes = 1 << 20

// UsageSample carries the token accounting extracted from an OpenAI- or
// Anthropic-compatible response.
//
// Unlike Ollama's native /api/generate and /api/chat responses, neither
// compatibility API reports inference durations. Elapsed is therefore
// measured by the proxy, from the moment upstream response headers arrive
// to the end of the body, and approximates generation wall time rather
// than being a reported decode duration.
type UsageSample struct {
	Model        string
	InputTokens  int64
	OutputTokens int64
	Elapsed      time.Duration
}

// Valid reports whether the sample carries enough information to derive
// throughput.
func (u UsageSample) Valid() bool {
	return u.Model != "" && u.OutputTokens > 0 && u.Elapsed > 0
}

// usageFrame is the union of the response shapes that carry token counts.
//
// OpenAI /v1/chat/completions reports usage.prompt_tokens and
// usage.completion_tokens on the final object (streaming requires
// stream_options.include_usage). Anthropic /v1/messages splits the counts:
// message_start carries message.usage.input_tokens and the model name,
// message_delta carries usage.output_tokens. Both shapes decode into this
// one struct and are merged by usageScanner.
type usageFrame struct {
	Model string `json:"model"`
	Type  string `json:"type"`

	Usage struct {
		// Anthropic
		InputTokens  int64 `json:"input_tokens"`
		OutputTokens int64 `json:"output_tokens"`
		// OpenAI
		PromptTokens     int64 `json:"prompt_tokens"`
		CompletionTokens int64 `json:"completion_tokens"`
	} `json:"usage"`

	// Anthropic message_start nests the model and initial usage one level
	// deeper.
	Message struct {
		Model string `json:"model"`
		Usage struct {
			InputTokens  int64 `json:"input_tokens"`
			OutputTokens int64 `json:"output_tokens"`
		} `json:"usage"`
	} `json:"message"`
}

// usageScanner wraps a compatibility-API response body and extracts token
// counts without buffering the response.
//
// Bytes pass through to the caller untouched while a copy is split on
// newlines. Each line is parsed as JSON after stripping an SSE "data:"
// prefix if present. This handles three wire formats with one code path:
//
//   - a single JSON object (non-streaming),
//   - SSE frames (streaming, both compatibility APIs),
//   - newline-delimited JSON.
//
// Counts are merged across frames because Anthropic streaming reports input
// and output tokens in different events. emit is called at most once, when
// the body reaches EOF or is closed - whichever happens first.
type usageScanner struct {
	src   io.ReadCloser
	start time.Time

	partial  []byte
	overflow bool

	sample UsageSample

	emit    func(UsageSample)
	emitted bool
}

// newUsageScanner returns a ReadCloser that proxies src verbatim and calls
// emit once with the merged usage totals, if the body contained any.
// fallbackModel is used when the response never names the model, which is
// the case for OpenAI-style streaming deltas from some servers.
func newUsageScanner(src io.ReadCloser, fallbackModel string, emit func(UsageSample)) *usageScanner {
	return &usageScanner{
		src:    src,
		start:  time.Now(),
		sample: UsageSample{Model: fallbackModel},
		emit:   emit,
	}
}

// Read passes bytes through from the upstream body, scanning them in flight.
func (s *usageScanner) Read(p []byte) (int, error) {
	n, err := s.src.Read(p)
	if n > 0 {
		s.feed(p[:n])
	}
	if err != nil {
		// io.EOF is the normal end of a complete response; any other error
		// still terminates the body, so finalise on both and report
		// whatever counts were seen up to that point.
		s.finish()
	}
	return n, err
}

// Close finalises extraction (for clients that abandon the stream early)
// and closes the upstream body.
func (s *usageScanner) Close() error {
	s.finish()
	return s.src.Close()
}

// feed splits b on newlines, parsing each completed line.
func (s *usageScanner) feed(b []byte) {
	for {
		i := bytes.IndexByte(b, '\n')
		if i < 0 {
			s.append(b)
			return
		}
		s.append(b[:i])
		s.parseLine()
		b = b[i+1:]
	}
}

// append accumulates a fragment of the current line, dropping the line
// entirely if it exceeds maxUsageLineBytes.
func (s *usageScanner) append(b []byte) {
	if s.overflow {
		return
	}
	if len(s.partial)+len(b) > maxUsageLineBytes {
		s.overflow = true
		s.partial = s.partial[:0]
		return
	}
	s.partial = append(s.partial, b...)
}

// sseData strips an SSE field prefix, returning the payload and whether the
// line carries JSON worth parsing. Comment lines, event/id/retry fields and
// the OpenAI "[DONE]" sentinel are rejected.
func sseData(line []byte) ([]byte, bool) {
	if after, ok := bytes.CutPrefix(line, []byte("data:")); ok {
		payload := bytes.TrimSpace(after)
		if bytes.Equal(payload, []byte("[DONE]")) {
			return nil, false
		}
		return payload, len(payload) > 0
	}
	// Any other SSE field, or a comment, carries no usage.
	if bytes.HasPrefix(line, []byte(":")) ||
		bytes.HasPrefix(line, []byte("event:")) ||
		bytes.HasPrefix(line, []byte("id:")) ||
		bytes.HasPrefix(line, []byte("retry:")) {
		return nil, false
	}
	return line, len(line) > 0
}

// parseLine consumes the buffered line and merges any token counts it
// carries. Non-JSON lines and content deltas are ignored.
func (s *usageScanner) parseLine() {
	line := bytes.TrimSpace(s.partial)
	s.partial = s.partial[:0]
	dropped := s.overflow
	s.overflow = false

	if dropped || len(line) == 0 {
		return
	}

	payload, ok := sseData(line)
	if !ok {
		return
	}

	var f usageFrame
	if err := json.Unmarshal(payload, &f); err != nil {
		return
	}

	if f.Model != "" {
		s.sample.Model = f.Model
	} else if f.Message.Model != "" {
		s.sample.Model = f.Message.Model
	}

	// Counts are merged with max() rather than overwritten: OpenAI repeats
	// cumulative totals on the final frame, while Anthropic reports input
	// once at message_start and output on message_delta. Taking the larger
	// value is correct for both and is robust to frames omitting a field.
	if in := maxInt64(f.Usage.InputTokens, f.Usage.PromptTokens, f.Message.Usage.InputTokens); in > s.sample.InputTokens {
		s.sample.InputTokens = in
	}
	if out := maxInt64(f.Usage.OutputTokens, f.Usage.CompletionTokens, f.Message.Usage.OutputTokens); out > s.sample.OutputTokens {
		s.sample.OutputTokens = out
	}
}

// maxInt64 returns the largest of its arguments.
func maxInt64(vals ...int64) int64 {
	var m int64
	for _, v := range vals {
		if v > m {
			m = v
		}
	}
	return m
}

// finish flushes any trailing line without a newline - the shape of a
// non-streamed single-object response - and emits at most once.
func (s *usageScanner) finish() {
	if s.emitted {
		return
	}
	s.emitted = true

	if len(s.partial) > 0 || s.overflow {
		s.parseLine()
	}

	s.sample.Elapsed = time.Since(s.start)
	if s.sample.Valid() {
		s.emit(s.sample)
	}
}
