package collector

import (
	"bytes"
	"encoding/json"
	"io"

	"github.com/maravexa/ollama-exporter/internal/ollama"
)

// maxTimingLineBytes bounds the memory held while reassembling a single
// NDJSON line that spans multiple reads. Ollama's terminal chunk is well
// under a kilobyte; anything larger is not a timing object, so the line is
// discarded rather than accumulated.
const maxTimingLineBytes = 1 << 20

// timingScanner wraps an Ollama response body and extracts inference timing
// fields without buffering the response.
//
// Bytes are passed through to the caller untouched while a copy is split on
// newlines and each complete line is parsed as JSON. Ollama emits
// newline-delimited JSON when streaming and a single JSON object when not;
// both are handled, because a non-streamed body is simply a stream of one
// line with no trailing newline.
//
// The last object seen with "done": true is handed to emit exactly once,
// when the body reaches EOF or is closed — whichever happens first.
type timingScanner struct {
	src io.ReadCloser

	partial  []byte
	overflow bool

	last     ollama.GenerateResponse
	haveLast bool

	emit    func(ollama.GenerateResponse)
	emitted bool
}

// newTimingScanner returns a ReadCloser that proxies src verbatim and calls
// emit once with the final done=true object, if the body contained one.
func newTimingScanner(src io.ReadCloser, emit func(ollama.GenerateResponse)) *timingScanner {
	return &timingScanner{src: src, emit: emit}
}

// Read passes bytes through from the upstream body, scanning them in flight.
func (s *timingScanner) Read(p []byte) (int, error) {
	n, err := s.src.Read(p)
	if n > 0 {
		s.feed(p[:n])
	}
	if err != nil {
		// io.EOF is the normal end of a complete response; any other error
		// still terminates the body, so finalise on both and report whatever
		// timings were seen up to that point.
		s.finish()
	}
	return n, err
}

// Close finalises extraction (for clients that abandon the stream early) and
// closes the upstream body.
func (s *timingScanner) Close() error {
	s.finish()
	return s.src.Close()
}

// feed splits b on newlines, parsing each completed line.
func (s *timingScanner) feed(b []byte) {
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
// entirely if it exceeds maxTimingLineBytes.
func (s *timingScanner) append(b []byte) {
	if s.overflow {
		return
	}
	if len(s.partial)+len(b) > maxTimingLineBytes {
		s.overflow = true
		s.partial = s.partial[:0]
		return
	}
	s.partial = append(s.partial, b...)
}

// parseLine consumes the buffered line and retains it if it is a done=true
// timing object. Non-JSON lines and intermediate token chunks are ignored.
func (s *timingScanner) parseLine() {
	line := bytes.TrimSpace(s.partial)
	s.partial = s.partial[:0]
	dropped := s.overflow
	s.overflow = false

	if dropped || len(line) == 0 {
		return
	}

	var gen ollama.GenerateResponse
	if err := json.Unmarshal(line, &gen); err != nil {
		return
	}
	if !gen.Done {
		return
	}
	s.last = gen
	s.haveLast = true
}

// finish flushes any trailing line without a newline — the shape of a
// non-streamed single-object response — and emits at most once.
func (s *timingScanner) finish() {
	if s.emitted {
		return
	}
	s.emitted = true

	if len(s.partial) > 0 || s.overflow {
		s.parseLine()
	}
	if s.haveLast {
		s.emit(s.last)
	}
}
