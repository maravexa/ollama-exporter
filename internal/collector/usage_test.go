package collector

import (
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestCompatibilityHTTP(t *testing.T) {
	for _, route := range []string{"/v1/chat/completions", "/v1/completions", "/v1/messages"} {
		for _, stream := range []bool{false, true} {
			for _, usage := range []bool{false, true} {
				t.Run(fmt.Sprint(route, stream, usage), func(t *testing.T) {
					body := `{"model":"fixture","choices":[{"text":"hi"}]`
					if usage {
						body += `,"usage":{"prompt_tokens":16,"completion_tokens":100}`
					}
					body += "}"
					if route == "/v1/messages" {
						body = `{"model":"fixture","type":"message","content":[]`
						if usage {
							body += `,"usage":{"input_tokens":16,"output_tokens":100}`
						}
						body += "}"
					}
					if stream {
						if route == "/v1/messages" {
							body = "event: message_start\r\ndata: {\"type\":\"message_start\",\"message\":{\"model\":\"fixture\""
							if usage {
								body += `,"usage":{"input_tokens":16,"output_tokens":0}`
							}
							body += "}}\r\n\r\n"
							if usage {
								body += "event: message_delta\r\ndata: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":100}}\r\n\r\n"
							}
							body += "event: message_stop\r\ndata: {\"type\":\"message_stop\"}\r\n\r\n"
						} else {
							body = "data: {\"model\":\"fixture\",\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\r\n\r\n"
							if usage {
								body += "data: {\"choices\":[],\r\ndata: \"usage\":{\"prompt_tokens\":16,\"completion_tokens\":100}}\r\n\r\n"
							}
							body += "data: [DONE]" // final bytes without trailing newline
						}
					}
					request := `{"model":"fixture","stream":` + fmt.Sprint(stream)
					if usage && route != "/v1/messages" {
						request += `,"stream_options":{"include_usage":true}`
					}
					request += "}"
					srv, _, reg := serveProxy(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						got, _ := io.ReadAll(r.Body)
						if string(got) != request {
							t.Errorf("upstream request changed: %s", got)
						}
						if stream {
							w.Header().Set("Content-Type", "text/event-stream")
						}
						for i := 0; i < len(body); i++ {
							_, _ = io.WriteString(w, body[i:i+1])
							if i%7 == 0 {
								w.(http.Flusher).Flush()
							}
						}
					}))
					resp, err := srv.Client().Post(srv.URL+route, "application/json", strings.NewReader(request))
					if err != nil {
						t.Fatal(err)
					}
					got, err := io.ReadAll(resp.Body)
					resp.Body.Close()
					if err != nil || string(got) != body {
						t.Fatal("SSE fidelity", err)
					}
					waitFinished(t, reg, 1)
					if metric(t, reg, "ollama_requests_total") != 1 {
						t.Fatal("request count")
					}
					want := uint64(0)
					if usage {
						want = 1
					}
					if histSampleCount(t, reg, "ollama_compatibility_output_tokens") != want || histSampleCount(t, reg, "ollama_compatibility_input_tokens") != want {
						t.Fatal("unknown usage became measured zero / usage lost")
					}
					if usage && (metric(t, reg, "ollama_compatibility_output_tokens") != 100 || metric(t, reg, "ollama_compatibility_input_tokens") != 16) {
						t.Fatal("usage counts")
					}
					if metric(t, reg, "ollama_tokens_per_second") != 0 || histSampleCount(t, reg, "ollama_eval_duration_seconds") != 0 || histSampleCount(t, reg, "ollama_prompt_eval_duration_seconds") != 0 {
						t.Fatal("fabricated native timing")
					}
				})
			}
		}
	}
}
func TestCompatibilityZeroAndTruncation(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		count      uint64
	}{
		{"zero", `{"model":"fixture","usage":{"completion_tokens":0}}`, 1},
		{"null", `{"model":"fixture","usage":{"completion_tokens":null}}`, 0},
		{"truncated-sse", "data: {\"model\":\"fixture\",\"usage\":{\"completion_tokens\":100}}\n\n", 0},
		{"bad-sse", "data: {\"model\":\"fixture\",\"usage\":{\"completion_tokens\":100}}garbage\n\ndata: [DONE]\n\n", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, _, reg := serveProxy(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.Contains(tc.name, "sse") {
					w.Header().Set("Content-Type", "text/event-stream")
				}
				_, _ = io.WriteString(w, tc.body)
			}))
			resp := post(t, srv, "/v1/chat/completions")
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			waitFinished(t, reg, 1)
			if histSampleCount(t, reg, "ollama_compatibility_output_tokens") != tc.count {
				t.Fatal("zero/truncation semantics")
			}
		})
	}
}

func TestCompatibilityFallbackAndEmptyBody(t *testing.T) {
	for _, body := range []string{"data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\ndata: [DONE]\n\n", ""} {
		t.Run(fmt.Sprint(len(body)), func(t *testing.T) {
			srv, _, reg := serveProxy(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, body)
			}))
			resp := post(t, srv, "/v1/chat/completions")
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			waitFinished(t, reg, 1)
			want := float64(0)
			if body != "" {
				want = 1
			}
			if metric(t, reg, "ollama_requests_total") != want {
				t.Fatal("fallback/empty completion")
			}
			if histSampleCount(t, reg, "ollama_compatibility_output_tokens") != 0 {
				t.Fatal("fabricated usage")
			}
		})
	}
}
