package collector

import (
	"encoding/json"
	"strings"
)

// jsonFields is a bounded SAX-style JSON recognizer. It retains only selected
// scalar fields, never response text or context arrays. Limits apply to nesting
// (64), field names (128 bytes), and selected scalars (4096 bytes), not objects
// or lines. Exceeding a structural/selected-field limit disables metrics only.
// No goroutine or read-ahead is needed: feed consumes only bytes already read
// by ReverseProxy, retaining its backpressure and body ownership.
type jsonFields struct {
	stack      []jsonFrame
	token      []byte
	mode       byte
	escape     bool
	unicode    int
	number     byte
	literal    string
	literalPos int
	keyToken   bool
	overflow   bool
	bad        bool
	failure    string
	keySkipped bool
	fields     map[string]json.RawMessage
	emit       func(map[string]json.RawMessage)
}
type jsonFrame struct {
	kind  byte
	state byte
	key   string
}

func wanted(path string) bool {
	switch path {
	case "model", "done", "error", "type", "load_duration", "eval_count", "eval_duration", "prompt_eval_count", "prompt_eval_duration", "usage.prompt_tokens", "usage.completion_tokens", "usage.input_tokens", "usage.output_tokens", "message.model", "message.usage.input_tokens", "message.usage.output_tokens":
		return true
	}
	return false
}
func (j *jsonFields) path() string {
	if len(j.stack) > 3 {
		return ""
	}
	for _, f := range j.stack {
		if f.kind != '{' || strings.Contains(f.key, ".") {
			return ""
		}
	}
	switch len(j.stack) {
	case 1:
		return j.stack[0].key
	case 2:
		return j.stack[0].key + "." + j.stack[1].key
	case 3:
		return j.stack[0].key + "." + j.stack[1].key + "." + j.stack[2].key
	}
	return ""
}
func (j *jsonFields) append(c byte) {
	limit := 4096
	if j.keyToken {
		limit = 128
	}
	if len(j.token) < limit {
		j.token = append(j.token, c)
	} else {
		j.overflow = true
	}
}
func (j *jsonFields) scalar() {
	if j.keyToken {
		f := &j.stack[len(j.stack)-1]
		if j.overflow {
			f.key = ""
			j.keySkipped = true
		} else if json.Unmarshal(j.token, &f.key) != nil {
			j.bad = true
		}
		f.state = ':'
	} else {
		path := j.path()
		if wanted(path) {
			if j.overflow || !json.Valid(j.token) {
				if j.overflow {
					j.failure = "selected_value_limit"
				}
				j.bad = true
				return
			}
			j.fields[path] = append(json.RawMessage(nil), j.token...)
		} else if j.mode != '"' && !j.scalarComplete() {
			j.bad = true
			return
		}
		j.valueDone()
	}
	j.token = j.token[:0]
	j.mode = 0
	j.overflow = false
	j.number = 0
	j.literal = ""
	j.literalPos = 0
}
func (j *jsonFields) valueDone() {
	if len(j.stack) == 0 {
		j.bad = true
		return
	}
	j.stack[len(j.stack)-1].state = ','
}
func (j *jsonFields) feed(data []byte) {
	for _, c := range data {
		j.byte(c)
	}
}
func (j *jsonFields) byte(c byte) {
	if j.bad {
		return
	}
	if j.mode == '"' {
		j.append(c)
		if j.unicode > 0 {
			if !strings.ContainsRune("0123456789abcdefABCDEF", rune(c)) {
				j.bad = true
			}
			j.unicode--
			return
		}
		if j.escape {
			j.escape = false
			if c == 'u' {
				j.unicode = 4
			} else if !strings.ContainsRune("\"\\/bfnrt", rune(c)) {
				j.bad = true
			}
			return
		}
		if c == '\\' {
			j.escape = true
			return
		}
		if c == '"' {
			j.scalar()
		} else if c < 32 {
			j.bad = true
		}
		return
	}
	if j.mode == 's' {
		if c == ',' || c == '}' || c == ']' || c == ' ' || c == '\n' || c == '\r' || c == '\t' {
			j.scalar()
			j.byte(c)
		} else {
			j.scalarByte(c)
			j.append(c)
		}
		return
	}
	if c == ' ' || c == '\n' || c == '\r' || c == '\t' {
		return
	}
	if len(j.stack) == 0 {
		if c != '{' {
			j.bad = true
			return
		}
		j.fields = make(map[string]json.RawMessage)
		j.stack = append(j.stack, jsonFrame{kind: '{', state: 'k'})
		return
	}
	f := &j.stack[len(j.stack)-1]
	switch f.state {
	case 'k', 'K':
		if c == '}' && f.state == 'k' {
			j.close(c)
			return
		}
		if c != '"' {
			j.bad = true
			return
		}
		j.keyToken = true
		j.mode = '"'
		j.append(c)
	case ':':
		if c != ':' {
			j.bad = true
			return
		}
		f.state = 'v'
	case ',':
		if c == '}' || c == ']' {
			j.close(c)
			return
		}
		if c != ',' {
			j.bad = true
			return
		}
		if f.kind == '{' {
			f.state = 'K'
		} else {
			f.state = 'v'
		}
	case 'v', 'a':
		if c == ']' && f.state == 'a' {
			j.close(c)
			return
		}
		j.keyToken = false
		switch c {
		case '{', '[':
			if path := j.path(); wanted(path) {
				if c == '{' {
					j.fields[path] = json.RawMessage(`{}`)
				} else {
					j.fields[path] = json.RawMessage(`[]`)
				}
			}
			if len(j.stack) == 64 {
				j.failure = "depth_limit"
				j.bad = true
				return
			}
			state := byte('k')
			if c == '[' {
				state = 'a'
			}
			j.stack = append(j.stack, jsonFrame{kind: c, state: state})
		case '"':
			j.mode = '"'
			j.append(c)
		default:
			if !strings.ContainsRune("-0123456789tfn", rune(c)) {
				j.bad = true
				return
			}
			j.mode = 's'
			j.scalarByte(c)
			j.append(c)
		}
	default:
		j.bad = true
	}
}
func (j *jsonFields) close(c byte) {
	f := j.stack[len(j.stack)-1]
	if (c == '}' && f.kind != '{') || (c == ']' && f.kind != '[') {
		j.bad = true
		return
	}
	j.stack = j.stack[:len(j.stack)-1]
	if len(j.stack) == 0 {
		if j.emit != nil {
			j.emit(j.fields)
		}
	} else {
		j.valueDone()
	}
}
func (j *jsonFields) complete() bool { return !j.bad && len(j.stack) == 0 && j.mode == 0 }

// Scalar grammar is tracked independently of token storage, so even an ignored
// number with millions of digits can be validated without retaining it.
func (j *jsonFields) scalarByte(c byte) {
	digit := c >= '0' && c <= '9'
	if j.literal != "" {
		if j.literalPos >= len(j.literal) || c != j.literal[j.literalPos] {
			j.bad = true
		} else {
			j.literalPos++
		}
		return
	}
	switch j.number {
	case 0:
		switch {
		case c == 't':
			j.literal = "true"
			j.literalPos = 1
		case c == 'f':
			j.literal = "false"
			j.literalPos = 1
		case c == 'n':
			j.literal = "null"
			j.literalPos = 1
		case c == '-':
			j.number = '-'
		case c == '0':
			j.number = '0'
		case digit:
			j.number = 'i'
		default:
			j.bad = true
		}
	case '-':
		if c == '0' {
			j.number = '0'
		} else if digit {
			j.number = 'i'
		} else {
			j.bad = true
		}
	case '0', 'i':
		if c == '.' {
			j.number = '.'
		} else if c == 'e' || c == 'E' {
			j.number = 'e'
		} else if !digit || j.number == '0' {
			j.bad = true
		}
	case '.':
		if digit {
			j.number = 'f'
		} else {
			j.bad = true
		}
	case 'f':
		if c == 'e' || c == 'E' {
			j.number = 'e'
		} else if !digit {
			j.bad = true
		}
	case 'e':
		if c == '+' || c == '-' {
			j.number = 's'
		} else if digit {
			j.number = 'd'
		} else {
			j.bad = true
		}
	case 's':
		if digit {
			j.number = 'd'
		} else {
			j.bad = true
		}
	case 'd':
		if !digit {
			j.bad = true
		}
	}
}
func (j *jsonFields) scalarComplete() bool {
	if j.literal != "" {
		return j.literalPos == len(j.literal)
	}
	return j.number == '0' || j.number == 'i' || j.number == 'f' || j.number == 'd'
}

func (j *jsonFields) failureReason() string {
	if j.failure != "" {
		return j.failure
	}
	if j.bad {
		return "malformed"
	}
	return "incomplete"
}
