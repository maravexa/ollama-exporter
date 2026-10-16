package collector

import (
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
)

func extractionResult(t *testing.T, reg *prometheus.Registry, outcome, reason string) {
	t.Helper()
	families, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	total := float64(0)
	for _, f := range families {
		if f.GetName() != "ollama_proxy_extraction_total" {
			continue
		}
		for _, m := range f.Metric {
			labels := map[string]string{}
			for _, l := range m.Label {
				labels[l.GetName()] = l.GetValue()
			}
			if len(labels) != 2 || labels["outcome"] != outcome || labels["reason"] != reason {
				t.Fatalf("unexpected extraction labels: %v", labels)
			}
			total += m.Counter.GetValue()
		}
	}
	if total != 1 {
		t.Fatalf("terminal extraction count=%v, want 1", total)
	}
}

// Encoded limits include the opening/closing JSON quotes, not decoded runes.
func encodedString(n int) string {
	content := n - 2
	return `"` + strings.Repeat(`\u0061`, content/6) + strings.Repeat("a", content%6) + `"`
}
func TestExtractionLimitBoundariesHTTP(t *testing.T) {
	for _, sse := range []bool{false, true} {
		for _, kind := range []string{"depth", "selected", "key"} {
			for _, delta := range []int{-1, 0, 1} {
				t.Run(fmt.Sprint(sse, "/", kind, "/", delta), func(t *testing.T) {
					base := terminal
					path := "/api/chat"
					if sse {
						base = `{"model":"fixture","usage":{"prompt_tokens":16,"completion_tokens":100}}`
						path = "/v1/chat/completions"
					}
					limit := 64
					if kind == "selected" {
						limit = 4096
					}
					if kind == "key" {
						limit = 128
					}
					n := limit + delta
					extra := ""
					switch kind {
					case "depth":
						extra = `"ignored":` + strings.Repeat("[", n-1) + "0" + strings.Repeat("]", n-1)
					case "selected":
						value := encodedString(n)
						if len(value) != n {
							t.Fatal("encoded fixture size")
						}
						extra = `"type":` + value
					case "key":
						key := encodedString(n)
						if len(key) != n {
							t.Fatal("encoded key size")
						}
						extra = key + `:1`
					}
					body := base[:len(base)-1] + "," + extra + "}"
					if sse {
						body = "data: " + body + "\r\n\r\ndata: [DONE]\r\n\r\n"
					}
					srv, _, reg := serveProxy(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						if sse {
							w.Header().Set("Content-Type", "text/event-stream")
						}
						_, _ = io.WriteString(w, body)
					}))
					resp := post(t, srv, path)
					got, err := io.ReadAll(resp.Body)
					resp.Body.Close()
					if err != nil || string(got) != body {
						t.Fatal("limit changed client response", err)
					}
					waitFinished(t, reg, 1)
					outcome, reason := "success", "none"
					if delta == 1 && kind != "key" {
						outcome = "failed"
						reason = "depth_limit"
						if kind == "selected" {
							reason = "selected_value_limit"
						}
					}
					extractionResult(t, reg, outcome, reason)
					want := float64(1)
					if outcome == "failed" {
						want = 0
					}
					if metric(t, reg, "ollama_requests_total") != want {
						t.Fatal("limit completion accounting")
					}
					skip := float64(0)
					if kind == "key" && delta == 1 {
						skip = 1
					}
					if metric(t, reg, "ollama_proxy_extraction_field_skips_total") != skip {
						t.Fatal("key limit skip not observed")
					}
				})
			}
		}
	}
}
func TestExtractionUsageDistinctionsHTTP(t *testing.T) {
	for _, tc := range []struct {
		name, body, outcome, reason string
		observations                uint64
	}{
		{"zero", `{"model":"fixture","usage":{"completion_tokens":0}}`, "success", "none", 1},
		{"absent", `{"model":"fixture"}`, "skipped", "usage_absent", 0},
		{"null", `{"model":"fixture","usage":{"completion_tokens":null}}`, "skipped", "usage_absent", 0},
		{"invalid-usage", `{"model":"fixture","usage":{"completion_tokens":"not-a-count"}}`, "failed", "invalid_fields", 0},
		{"negative", `{"model":"fixture","usage":{"completion_tokens":-1}}`, "failed", "invalid_fields", 0},
		{"malformed", `{"model":"fixture","bad":"\q"}`, "failed", "malformed", 0},
		{"truncated", `{"model":"fixture"`, "failed", "incomplete", 0},
		{"upstream-error", `{"error":"private response contents"}`, "failed", "upstream_error", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, _, reg := serveProxy(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, tc.body) }))
			resp := post(t, srv, "/v1/completions")
			got, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if string(got) != tc.body {
				t.Fatal("response altered")
			}
			waitFinished(t, reg, 1)
			extractionResult(t, reg, tc.outcome, tc.reason)
			if histSampleCount(t, reg, "ollama_compatibility_output_tokens") != tc.observations {
				t.Fatal("absent, zero, or failure conflated")
			}
		})
	}
}
func TestExtractionRepeatedCloseEOF(t *testing.T) {
	for _, early := range []bool{false, true} {
		t.Run(fmt.Sprint(early), func(t *testing.T) {
			p, reg := newTestProxy(t, nil)
			req, _ := http.NewRequest("POST", "http://upstream/api/chat", nil)
			resp := &http.Response{Request: req, StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(terminal))}
			_ = p.modifyResponse(resp)
			if early {
				_, _ = resp.Body.Read(make([]byte, 1))
			} else {
				_, _ = io.Copy(io.Discard, resp.Body)
				for i := 0; i < 3; i++ {
					_, _ = resp.Body.Read(make([]byte, 1))
				}
			}
			for i := 0; i < 3; i++ {
				resp.Body.Close()
			}
			outcome, reason := "success", "none"
			if early {
				outcome = "skipped"
				reason = "body_closed"
			}
			extractionResult(t, reg, outcome, reason)
		})
	}
}

func TestExtractionRoutingDiagnostics(t *testing.T) {
	for _, tc := range []struct {
		method, path   string
		status         int
		excluded, want bool
	}{
		{"POST", "/api/chat", 503, false, true},
		{"GET", "/api/chat", 200, false, false},
		{"POST", "/unaffected/attacker-controlled", 200, false, false},
		{"POST", "/api/chat", 200, true, false},
	} {
		t.Run(fmt.Sprint(tc), func(t *testing.T) {
			srv, p, reg := serveProxy(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, terminal)
			}))
			if tc.excluded {
				p.excludePaths[tc.path] = struct{}{}
			}
			req, _ := http.NewRequest(tc.method, srv.URL+tc.path, strings.NewReader(`{"model":"fixture"}`))
			resp, err := srv.Client().Do(req)
			if err != nil {
				t.Fatal(err)
			}
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if tc.want {
				extractionResult(t, reg, "skipped", "http_status")
			} else if metric(t, reg, "ollama_proxy_extraction_total") != 0 {
				t.Fatal("ineligible response instrumented")
			}
		})
	}
}
func TestRepeatedOversizedFieldNames(t *testing.T) {
	key := encodedString(129)
	body := "data: {" + key + ":0}\n\ndata: {" + key + ":0,\"usage\":{\"completion_tokens\":0}}\n\ndata: [DONE]\n\n"
	srv, _, reg := serveProxy(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, body)
	}))
	resp := post(t, srv, "/v1/completions")
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	waitFinished(t, reg, 1)
	extractionResult(t, reg, "success", "none")
	if metric(t, reg, "ollama_proxy_extraction_field_skips_total") != 1 {
		t.Fatal("field skips counted more than once per response")
	}
}
