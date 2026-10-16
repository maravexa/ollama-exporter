package collector

import (
	"encoding/json"
	"io"
	"strings"
	"sync"
)

// responseBody observes only reads made by the proxy. Close never turns a
// partial response into a successful inference. Observations require clean EOF.
type responseBody struct {
	src         io.ReadCloser
	parser      jsonFields
	sse         bool
	prefix      string
	lineData    bool
	lineBytes   bool
	pendingCR   bool
	marker      string
	eventFields map[string]json.RawMessage
	sample      map[string]json.RawMessage
	terminal    bool
	seen        bool
	failed      bool
	ended       bool
	closeOnce   sync.Once
	closeErr    error
	emit        func(map[string]json.RawMessage)
	result      func(string, string)
	resultOnce  sync.Once
	notice      func()
	noticeOnce  sync.Once
	failure     string
}

func newResponseBody(src io.ReadCloser, sse bool, emit func(map[string]json.RawMessage)) *responseBody {
	b := &responseBody{src: src, sse: sse, emit: emit, sample: make(map[string]json.RawMessage)}
	b.parser.emit = func(f map[string]json.RawMessage) {
		b.seen = true
		if b.sse {
			if b.eventFields != nil {
				b.fail("malformed")
			}
			b.eventFields = f
		} else {
			b.merge(f)
		}
	}
	return b
}
func (b *responseBody) merge(f map[string]json.RawMessage) {
	if _, ok := f["error"]; ok {
		b.fail("upstream_error")
		return
	}
	for k, v := range f {
		b.sample[k] = v
	}
	var kind string
	isStop := json.Unmarshal(f["type"], &kind) == nil && kind == "message_stop"
	if string(f["done"]) == "true" || isStop {
		b.terminal = true
	}
}
func (b *responseBody) dispatch() {
	b.noteFieldSkip()
	if strings.TrimSpace(b.marker) == "[DONE]" {
		b.terminal = true
	} else if b.marker != "" {
		b.fail("malformed")
	}
	if !b.parser.complete() {
		b.fail(b.parser.failureReason())
	} else if b.eventFields != nil {
		b.merge(b.eventFields)
	}
	b.eventFields = nil
	b.marker = ""
	b.parser = jsonFields{emit: func(f map[string]json.RawMessage) {
		b.seen = true
		if b.eventFields != nil {
			b.fail("malformed")
		}
		b.eventFields = f
	}}
}
func (b *responseBody) sseByte(c byte) {
	if c == '\n' {
		if !b.lineBytes {
			b.dispatch()
		} else if b.lineData && b.marker == "" {
			b.parser.byte('\n')
		}
		b.prefix = ""
		b.lineData = false
		b.lineBytes = false
		return
	}
	b.lineBytes = true
	if b.lineData {
		if b.marker != "" {
			if len(b.marker) < 32 {
				b.marker += string(c)
			} else {
				b.fail("malformed")
			}
			return
		}
		if c == '[' && b.parser.complete() && b.eventFields == nil {
			b.marker = "["
			return
		}
		b.parser.byte(c)
		return
	}
	if len(b.prefix) < 6 {
		b.prefix += string(c)
	}
	if b.prefix == "data:" {
		b.lineData = true
	}
}
func (b *responseBody) Read(p []byte) (int, error) {
	n, err := b.src.Read(p)
	if !b.ended {
		if b.sse {
			for _, c := range p[:n] {
				if b.pendingCR {
					b.pendingCR = false
					if c != '\n' {
						b.sseByte('\n')
					}
				}
				if c == '\r' {
					b.pendingCR = true
				} else {
					b.sseByte(c)
				}
			}
		} else {
			b.parser.feed(p[:n])
		}
		b.noteFieldSkip()
		if err != nil {
			b.ended = true
			if err == io.EOF {
				if b.sse {
					if b.lineBytes || b.pendingCR {
						b.sseByte('\n')
					}
					b.dispatch()
				}
				if !b.failed && b.parser.complete() && (b.seen || b.terminal) && (!b.sse || b.terminal) {
					b.emit(b.sample)
				} else if b.failed {
					b.report("failed", b.failure)
				} else if !b.parser.complete() {
					b.report("failed", b.parser.failureReason())
				} else {
					b.report("skipped", "incomplete")
				}
			} else {
				b.report("failed", "read_error")
			}
		}
	}
	return n, err
}
func (b *responseBody) Close() error {
	b.closeOnce.Do(func() { b.closeErr = b.src.Close() })
	b.report("skipped", "body_closed")
	return b.closeErr
}

func (b *responseBody) fail(reason string) {
	b.failed = true
	if b.failure == "" {
		b.failure = reason
	}
}
func (b *responseBody) report(outcome, reason string) {
	b.resultOnce.Do(func() {
		if b.result != nil {
			b.result(outcome, reason)
		}
	})
}
func (b *responseBody) noteFieldSkip() {
	if b.parser.keySkipped {
		b.noticeOnce.Do(func() {
			if b.notice != nil {
				b.notice()
			}
		})
	}
}
