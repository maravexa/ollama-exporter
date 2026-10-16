package collector

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type fragmentBody struct {
	text   string
	step   int
	closed int
	final  error
}

func (r *fragmentBody) Read(p []byte) (int, error) {
	if r.text == "" {
		return 0, io.EOF
	}
	n := min(len(p), len(r.text), r.step)
	copy(p, r.text[:n])
	r.text = r.text[n:]
	if r.text == "" {
		return n, r.final
	}
	return n, nil
}
func (r *fragmentBody) Close() error { r.closed++; return nil }
func TestParserBoundariesAndClosure(t *testing.T) {
	body := `{"response":"escaped \\ \" \u1234","nested":{"done":true},"context":[1,{"a":[null,true,false,-1.2e+3]}],"model":"fixture","eval_count":100,"eval_duration":4000000000,"done":true}`
	for _, step := range []int{1, 2, 7, 32768} {
		for _, final := range []error{io.EOF, errors.New("read failed")} {
			t.Run(fmt.Sprint(step, final), func(t *testing.T) {
				src := &fragmentBody{text: body, step: step, final: final}
				calls := 0
				b := newResponseBody(src, false, func(f map[string]json.RawMessage) {
					calls++
					if string(f["eval_count"]) != "100" {
						t.Fatal(f)
					}
				})
				got, err := io.ReadAll(b)
				if string(got) != body || (final != io.EOF && err == nil) {
					t.Fatal("read fidelity/error", err)
				}
				for i := 0; i < 3; i++ {
					_, _ = b.Read(make([]byte, 4))
					_ = b.Close()
				}
				want := 0
				if final == io.EOF {
					want = 1
				}
				if calls != want || src.closed != 1 {
					t.Fatal("observations/close", calls, src.closed)
				}
			})
		}
	}
	src := &fragmentBody{text: terminal, step: 1}
	calls := 0
	b := newResponseBody(src, false, func(map[string]json.RawMessage) { calls++ })
	_, _ = b.Read(make([]byte, 1))
	_ = b.Close()
	if calls != 0 || src.closed != 1 {
		t.Fatal("early close completion")
	}
}
func TestAbortPanicPreserved(t *testing.T) {
	p, reg := newTestProxy(t, nil)
	handler := p.instrumentedHandler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic(http.ErrAbortHandler) }))
	func() {
		defer func() {
			if recover() != http.ErrAbortHandler {
				t.Error("abort panic swallowed or replaced")
			}
		}()
		handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/api/chat", strings.NewReader(`{"model":"fixture"}`)))
	}()
	waitFinished(t, reg, 1)
}
func TestJSONRecognizer(t *testing.T) {
	cases := []string{`{"done":true,}`, `{"a":[1,]}`, `{"a":01}`, `{"a":"\q"}`, `{"a":"\u000z"}`, `{"done":true`, `{"a":]}`, `{"a":[}`, `{"done":true}garbage`}
	for _, s := range cases {
		j := jsonFields{}
		j.feed([]byte(s))
		if j.complete() {
			t.Errorf("accepted invalid JSON %q", s)
		}
	}
}

// Input is generated with MultiReader/LimitReader; neither test nor observer
// materializes the payload. Total allocations are measured, not just RSS.
func BenchmarkBoundedParser(b *testing.B) {
	for _, n := range []int64{1 << 20, 16 << 20, 64 << 20} {
		b.Run(fmt.Sprint(n), func(b *testing.B) {
			b.ReportAllocs()
			b.SetBytes(n)
			block := strings.Repeat("x", 32768)
			buf := make([]byte, 32768)
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				src := io.NopCloser(io.MultiReader(strings.NewReader(`{"response":"`), io.LimitReader(repeatReader(block), n), strings.NewReader(`",`+terminal[1:])))
				calls := 0
				r := newResponseBody(src, false, func(map[string]json.RawMessage) { calls++ })
				if _, err := io.CopyBuffer(io.Discard, r, buf); err != nil {
					b.Fatal(err)
				}
				r.Close()
				if calls != 1 {
					b.Fatal("lost terminal")
				}
			}
		})
	}
}

type repeatReader string

func (r repeatReader) Read(p []byte) (int, error) { return copy(p, string(r)), nil }

// Differential framing validation checks nested grammar and escape handling
// against the standard library. Fuzz with -fuzz=FuzzJSONFields when extending it.
func FuzzJSONFields(f *testing.F) {
	for _, s := range []string{terminal, `{"x":[1,{"z":"a\\\"\u1234"},true,null]}`, `{"a":01}`, `{"a":[1,]}`} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		if len(s) > 8192 || !strings.HasPrefix(strings.TrimSpace(s), "{") {
			t.Skip()
		}
		// Multiple JSON roots are legal NDJSON here; only compare single documents.
		var value map[string]any
		if json.Unmarshal([]byte(s), &value) != nil {
			return
		}
		j := jsonFields{}
		for i := range len(s) {
			j.feed([]byte(s[i : i+1]))
		}
		if !j.complete() {
			t.Fatalf("valid object rejected: %s", s)
		}
	})
}

func TestIgnoredFieldNamesAndDuplicateTypes(t *testing.T) {
	for _, body := range []string{
		`{"model":"fixture","done":true,"done":{}}`,
		`{"model":"fixture","done":true,"eval_duration":1,"eval_duration":[]}`,
	} {
		p, reg := newTestProxy(t, nil)
		resp := &http.Response{StatusCode: 200, Header: make(http.Header), Request: httptest.NewRequest("POST", "/api/chat", nil), Body: io.NopCloser(strings.NewReader(body))}
		_ = p.modifyResponse(resp)
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if metric(t, reg, "ollama_requests_total") != 0 {
			t.Fatal("invalid selected type produced metrics")
		}
	}
	j := jsonFields{}
	j.feed([]byte(`{"usage.completion_tokens":999,"usage":{"completion_tokens":100}}`))
	if string(j.fields["usage.completion_tokens"]) != "100" {
		t.Fatal("dotted ignored field masked usage")
	}
}
func TestObserverDoesNotReadAhead(t *testing.T) {
	src := &fragmentBody{text: terminal, step: 32768}
	b := newResponseBody(src, false, func(map[string]json.RawMessage) { t.Error("premature observation") })
	defer b.Close()
	if src.text != terminal {
		t.Fatal("constructor read source")
	}
	buf := make([]byte, 1)
	_, _ = b.Read(buf)
	if len(src.text) != len(terminal)-1 {
		t.Fatal("observer read ahead")
	}
}

func TestLargeIgnoredNumberAndParserLimits(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		want       bool
	}{
		{"ignored-number", `{"unrelated":1` + strings.Repeat("0", 1<<20) + `,` + terminal[1:], true},
		{"depth-limit", `{"unrelated":` + strings.Repeat("[", 65) + "0" + strings.Repeat("]", 65) + `,` + terminal[1:], false},
		{"selected-limit", `{"model":"` + strings.Repeat("x", 4096) + `","done":true}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			r := newResponseBody(io.NopCloser(strings.NewReader(tc.body)), false, func(map[string]json.RawMessage) { calls++ })
			got, err := io.ReadAll(r)
			r.Close()
			if err != nil || string(got) != tc.body {
				t.Fatal("parser failure affected response", err)
			}
			if (calls == 1) != tc.want {
				t.Fatal("parser limit/ignored scalar", calls)
			}
		})
	}
}
func BenchmarkBoundedContextArray(b *testing.B) {
	for _, n := range []int64{1 << 20, 16 << 20, 64 << 20} {
		b.Run(fmt.Sprint(n), func(b *testing.B) {
			b.ReportAllocs()
			b.SetBytes(n)
			block := strings.Repeat("0,", 16384)
			buf := make([]byte, 32768)
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				src := io.NopCloser(io.MultiReader(strings.NewReader(`{"context":[`), io.LimitReader(repeatReader(block), n), strings.NewReader(`0],`+terminal[1:])))
				calls := 0
				r := newResponseBody(src, false, func(map[string]json.RawMessage) { calls++ })
				if _, err := io.CopyBuffer(io.Discard, r, buf); err != nil {
					b.Fatal(err)
				}
				r.Close()
				if calls != 1 {
					b.Fatal("lost terminal")
				}
			}
		})
	}
}

func TestRequestReadFailureIsObserved(t *testing.T) {
	p, reg := newTestProxy(t, nil)
	req := httptest.NewRequest("POST", "/api/chat", nil)
	src := &fragmentBody{text: `{"model":"fixture"`, step: 32768, final: io.ErrUnexpectedEOF}
	req.Body = src
	rr := httptest.NewRecorder()
	p.instrumentedHandler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("failed request forwarded") })).ServeHTTP(rr, req)
	if rr.Code != 400 || src.closed != 1 {
		t.Fatal("request read failure handling", rr.Code, src.closed)
	}
	waitFinished(t, reg, 1)
	if metric(t, reg, "ollama_requests_total") != 0 {
		t.Fatal("request read failure counted")
	}
}

func TestSSEEveryReadBoundary(t *testing.T) {
	body := ": comment\r\nevent: completion\r\ndata: {\"choices\":[{\"delta\":{\"content\":\"escaped \\\"\"}}],\r\ndata: \"usage\":{\"prompt_tokens\":16,\"completion_tokens\":100}}\r\n\r\ndata: [DONE]"
	for _, step := range []int{1, 2, 3, 7, 32768} {
		t.Run(fmt.Sprint(step), func(t *testing.T) {
			src := &fragmentBody{text: body, step: step, final: io.EOF}
			calls := 0
			r := newResponseBody(src, true, func(f map[string]json.RawMessage) {
				calls++
				if string(f["usage.completion_tokens"]) != "100" {
					t.Fatal("lost fragmented usage")
				}
			})
			got, err := io.ReadAll(r)
			r.Close()
			if err != nil || string(got) != body || calls != 1 || src.closed != 1 {
				t.Fatal("SSE framing/fidelity", err, calls, src.closed)
			}
		})
	}
}

func TestEscapedAnthropicTerminalType(t *testing.T) {
	body := "data: {\"usage\":{\"output_tokens\":100}}\n\ndata: {\"type\":\"message\\u005fstop\"}\n\n"
	calls := 0
	r := newResponseBody(io.NopCloser(strings.NewReader(body)), true, func(map[string]json.RawMessage) { calls++ })
	_, _ = io.Copy(io.Discard, r)
	r.Close()
	if calls != 1 {
		t.Fatal("escaped terminal type not recognized")
	}
}
