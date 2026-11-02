package collector

import (
	"fmt"
	"io"
	"net/http"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
)

// Gather without WithLabelValues: observing an absent counter must not create it.
func assertNativeTokens(t *testing.T, reg *prometheus.Registry, prompt, eval float64) {
	t.Helper()
	families, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]float64{"ollama_prompt_tokens_total": prompt, "ollama_eval_tokens_total": eval} {
		var got float64
		for _, family := range families {
			if family.GetName() != name {
				continue
			}
			for _, m := range family.Metric {
				got += m.GetCounter().GetValue()
			}
			if want == 0 && len(family.Metric) > 0 {
				t.Errorf("%s unexpectedly instantiated for zero/missing/failed observation", name)
			}
		}
		if got != want {
			t.Errorf("%s = %v, want %v", name, got, want)
		}
	}
}

func TestNativeTokenCountsAndAccumulation(t *testing.T) {
	for _, path := range []string{"/api/generate", "/api/chat"} {
		for _, stream := range []bool{false, true} {
			for _, tc := range []struct {
				name, counts string
				prompt, eval float64
			}{
				{"positive-without-duration", `,"prompt_eval_count":7,"eval_count":11`, 7, 11},
				{"zero", `,"prompt_eval_count":0,"eval_count":0`, 0, 0},
				{"missing", "", 0, 0},
				{"negative", `,"prompt_eval_count":-7,"eval_count":-11`, 0, 0},
				{"negative-prompt", `,"prompt_eval_count":-7,"eval_count":11`, 0, 11},
				{"negative-eval", `,"prompt_eval_count":7,"eval_count":-11`, 7, 0},
				{"prompt-only", `,"prompt_eval_count":7`, 7, 0},
				{"eval-only", `,"eval_count":11`, 0, 11},
			} {
				t.Run(fmt.Sprint(path, "/", stream, "/", tc.name), func(t *testing.T) {
					body := `{"model":"fixture","done":true` + tc.counts + `}`
					if stream {
						body = `{"model":"fixture","done":false,"response":"chunk"}` + "\n" + body + "\n"
					}
					srv, _, reg := serveProxy(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, body) }))
					for n := 1; n <= 2; n++ {
						resp := post(t, srv, path)
						_, err := io.Copy(io.Discard, resp.Body)
						resp.Body.Close()
						if err != nil {
							t.Fatal(err)
						}
						waitFinished(t, reg, uint64(n))
						assertNativeTokens(t, reg, float64(n)*tc.prompt, float64(n)*tc.eval)
						if metric(t, reg, "ollama_requests_total") != float64(n) {
							t.Fatal("token availability changed completion accounting")
						}
					}
				})
			}
		}
	}
}

func TestNativeTokensExcludedAndInvalid(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		excluded   bool
	}{
		{"excluded", terminal, true},
		{"fractional", `{"model":"fixture","done":true,"prompt_eval_count":1.5,"eval_count":9}`, false},
		{"overflow", `{"model":"fixture","done":true,"prompt_eval_count":9223372036854775808,"eval_count":9}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, p, reg := serveProxy(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, tc.body) }))
			if tc.excluded {
				p.excludePaths["/api/chat"] = struct{}{}
			}
			resp := post(t, srv, "/api/chat")
			_, err := io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if err != nil {
				t.Fatal(err)
			}
			if !tc.excluded {
				waitFinished(t, reg, 1)
			}
			assertNativeTokens(t, reg, 0, 0)
		})
	}
}
