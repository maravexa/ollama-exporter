package collector

import (
	"io"
	"net/http"
	"sync"
	"testing"
)

// Thinking is emitted before answer content. Neither the native thinking field
// nor an OpenAI reasoning delta may delay forwarding until inference completes.
func TestThinkingBeforeAnswerHTTP(t *testing.T) {
	for _, tc := range []struct {
		name, path, contentType, thinking, answer, final string
		compatibility                                    bool
	}{
		{
			name: "native-generate", path: "/api/generate", contentType: "application/x-ndjson",
			thinking: `{"model":"fixture","thinking":"Let me reason: café","response":"","done":false}` + "\n",
			answer:   `{"model":"fixture","thinking":"","response":"42","done":false}` + "\n",
			final:    terminal + "\n",
		},
		{
			name: "native-chat", path: "/api/chat", contentType: "application/x-ndjson",
			thinking: `{"model":"fixture","message":{"role":"assistant","thinking":"Let me reason: café","content":""},"done":false}` + "\n",
			answer:   `{"model":"fixture","message":{"role":"assistant","content":"42"},"done":false}` + "\n",
			final:    terminal + "\n",
		},
		{
			name: "openai-chat", path: "/v1/chat/completions", contentType: "text/event-stream", compatibility: true,
			thinking: "data: " + `{"model":"fixture","choices":[{"delta":{"reasoning":"Let me reason: café"},"finish_reason":null}]}` + "\n\n",
			answer:   "data: " + `{"model":"fixture","choices":[{"delta":{"content":"42"},"finish_reason":null}]}` + "\n\n",
			final:    "data: " + `{"model":"fixture","choices":[],"usage":{"prompt_tokens":16,"completion_tokens":100,"total_tokens":116}}` + "\n\ndata: [DONE]\n\n",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			release := make(chan struct{})
			var once sync.Once
			unblock := func() { once.Do(func() { close(release) }) }
			srv, _, reg := serveProxy(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", tc.contentType)
				_, _ = io.WriteString(w, tc.thinking)
				w.(http.Flusher).Flush()
				select {
				case <-release:
				case <-r.Context().Done():
					return
				}
				// Fragment answer bytes, including JSON syntax, independently of events.
				for _, b := range []byte(tc.answer + tc.final) {
					_, _ = w.Write([]byte{b})
				}
			}))
			defer unblock()
			resp := post(t, srv, tc.path)
			defer resp.Body.Close()
			first := make([]byte, len(tc.thinking))
			if _, err := io.ReadFull(resp.Body, first); err != nil || string(first) != tc.thinking {
				t.Fatalf("thinking event was buffered or changed: %v", err)
			}
			if metric(t, reg, "ollama_requests_total") != 0 || histSampleCount(t, reg, "ollama_compatibility_output_tokens") != 0 {
				t.Fatal("thinking event counted as completed inference")
			}
			t.Log("thinking event reached client before upstream answer gate opened")
			unblock()
			rest, err := io.ReadAll(resp.Body)
			if err != nil || string(rest) != tc.answer+tc.final {
				t.Fatalf("answer/terminal fidelity: %v; body=%q", err, rest)
			}
			waitFinished(t, reg, 1)
			if metric(t, reg, "ollama_requests_total") != 1 {
				t.Fatal("expected exactly one completed inference")
			}
			if tc.compatibility {
				if metric(t, reg, "ollama_compatibility_output_tokens") != 100 || histSampleCount(t, reg, "ollama_eval_duration_seconds") != 0 {
					t.Fatal("incorrect compatibility usage or fabricated native timing")
				}
			} else if metric(t, reg, "ollama_tokens_per_second") != 25 || histSampleCount(t, reg, "ollama_eval_duration_seconds") != 1 {
				t.Fatal("native thinking stream lost terminal timing")
			}
		})
	}
}
