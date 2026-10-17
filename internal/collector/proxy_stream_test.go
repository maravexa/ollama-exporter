package collector

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

const terminal = `{"model":"fixture","done":true,"eval_count":100,"eval_duration":4000000000,"prompt_eval_count":16,"prompt_eval_duration":125000000,"load_duration":100000000}`

// Uses the production ReverseProxy configuration, including immediate flushing.
func serveProxy(t *testing.T, upstream http.Handler, configure ...func(*httputil.ReverseProxy)) (*httptest.Server, *Proxy, *prometheus.Registry) {
	t.Helper()
	up := httptest.NewServer(upstream)
	t.Cleanup(up.Close)
	p, reg := newTestProxy(t, nil)
	target, _ := url.Parse(up.URL)
	rp := p.reverseProxy(target)
	for _, fn := range configure {
		fn(rp)
	}
	srv := httptest.NewServer(p.instrumentedHandler(rp))
	t.Cleanup(srv.Close)
	return srv, p, reg
}
func metric(t *testing.T, reg *prometheus.Registry, name string) float64 {
	t.Helper()
	fs, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	var total float64
	for _, f := range fs {
		if f.GetName() == name {
			for _, m := range f.Metric {
				switch {
				case m.Gauge != nil:
					total += m.Gauge.GetValue()
				case m.Counter != nil:
					total += m.Counter.GetValue()
				case m.Histogram != nil:
					total += m.Histogram.GetSampleSum()
				}
			}
		}
	}
	return total
}
func waitFinished(t *testing.T, reg *prometheus.Registry, count uint64) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for {
		if histSampleCount(t, reg, "ollama_request_duration_seconds") == count && metric(t, reg, "ollama_requests_in_flight") == 0 {
			return
		}
		select {
		case <-deadline:
			t.Fatal("request did not finish")
		case <-tick.C:
		}
	}
}
func post(t *testing.T, srv *httptest.Server, path string) *http.Response {
	t.Helper()
	return postWithTimeout(t, srv, path, 10*time.Second)
}
func postWithTimeout(t *testing.T, srv *httptest.Server, path string, timeout time.Duration) *http.Response {
	t.Helper()
	client := srv.Client()
	client.Timeout = timeout
	resp, err := client.Post(srv.URL+path, "application/json", strings.NewReader(`{"model":"fixture"}`))
	if err != nil {
		t.Fatal(err)
	}
	return resp
}
func TestNativeHTTP(t *testing.T) {
	for _, path := range []string{"/api/generate", "/api/chat"} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprint(path, stream), func(t *testing.T) {
				body := terminal
				if stream {
					body = `{"response":"thinking","done":false}` + "\n" + body + "\r\n"
				}
				srv, _, reg := serveProxy(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Type", "application/x-ndjson")
					w.Header().Set("X-End-To-End", "yes")
					w.Header().Set("Trailer", "X-Final")
					w.WriteHeader(http.StatusCreated)
					for i := 0; i < len(body); i += 7 {
						end := min(i+7, len(body))
						_, _ = io.WriteString(w, body[i:end])
					}
					w.Header().Set("X-Final", "complete")
				}))
				resp := post(t, srv, path)
				got, err := io.ReadAll(resp.Body)
				resp.Body.Close()
				if err != nil || string(got) != body {
					t.Fatalf("body fidelity: %v", err)
				}
				if resp.StatusCode != 201 || resp.Header.Get("X-End-To-End") != "yes" || resp.Trailer.Get("X-Final") != "complete" {
					t.Fatalf("headers/status/trailers: %v", resp)
				}
				waitFinished(t, reg, 1)
				if metric(t, reg, "ollama_tokens_per_second") != 25 || metric(t, reg, "ollama_prompt_tokens_per_second") != 128 || metric(t, reg, "ollama_requests_total") != 1 || histSampleCount(t, reg, "ollama_eval_duration_seconds") != 1 {
					t.Fatal("incorrect/excess observations")
				}
				fs, _ := reg.Gather()
				for _, f := range fs {
					if f.GetName() == "ollama_requests_total" {
						labels := map[string]string{}
						for _, l := range f.Metric[0].Label {
							labels[l.GetName()] = l.GetValue()
						}
						if labels["endpoint"] != path || labels["model"] != "fixture" || labels["family"] != "llama" || labels["quant"] != "q4_k_m" {
							t.Fatal(labels)
						}
					}
				}
			})
		}
	}
}
func TestFirstTokenBeforeCompletion(t *testing.T) {
	for _, path := range []string{"/api/generate", "/api/chat", "/v1/chat/completions", "/v1/completions", "/v1/messages"} {
		t.Run(path, func(t *testing.T) {
			release := make(chan struct{})
			var once sync.Once
			unblock := func() { once.Do(func() { close(release) }) }
			defer unblock()
			first := `{"response":"thinking","done":false}` + "\n"
			last := terminal
			if strings.HasPrefix(path, "/v1/") {
				first = "data: {\"model\":\"fixture\",\"choices\":[]}\n\n"
				last = "data: [DONE]\n\n"
			}
			srv, _, reg := serveProxy(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasPrefix(path, "/v1/") {
					w.Header().Set("Content-Type", "text/event-stream")
				}
				_, _ = io.WriteString(w, first)
				w.(http.Flusher).Flush()
				select {
				case <-release:
				case <-r.Context().Done():
					return
				}
				_, _ = io.WriteString(w, last)
			}))
			resp := post(t, srv, path)
			defer resp.Body.Close()
			got := make([]byte, len(first))
			if _, err := io.ReadFull(resp.Body, got); err != nil || string(got) != first {
				t.Fatalf("gated first event: %v", err)
			}
			t.Log("client received flushed first event while upstream completion gate remained closed")
			unblock()
			_, err := io.Copy(io.Discard, resp.Body)
			if err != nil {
				t.Fatal(err)
			}
			waitFinished(t, reg, 1)
		})
	}
}
func TestLargeNativeObjects(t *testing.T) {
	for _, size := range []int{(1 << 20) - 1, 1 << 20, (1 << 20) + 1, 4 << 20} {
		for _, contextArray := range []bool{false, true} {
			for _, stream := range []bool{false, true} {
				t.Run(fmt.Sprint(size, "/", contextArray, "/", stream), func(t *testing.T) {
					prefix := `{"response":"`
					suffix := `",` + terminal[1:]
					if contextArray {
						prefix = `{"context":[`
						suffix = `0],` + terminal[1:]
					}
					n := size - len(prefix) - len(suffix)
					padding := strings.Repeat("x", n)
					if contextArray {
						padding = strings.Repeat("0,", n/2)
						if n%2 != 0 {
							padding += " "
						}
					}
					body := prefix + padding + suffix
					if len(body) != size {
						t.Fatalf("fixture %d != %d", len(body), size)
					}
					if stream {
						body = `{"done":false,"response":"first"}` + "\n" + body
					}
					srv, _, reg := serveProxy(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, body) }))
					// Large arrays need more time under race and atomic coverage instrumentation.
					resp := postWithTimeout(t, srv, "/api/generate", 60*time.Second)
					got, err := io.ReadAll(resp.Body)
					resp.Body.Close()
					if err != nil || string(got) != body {
						t.Fatal("large body fidelity", err)
					}
					waitFinished(t, reg, 1)
					if metric(t, reg, "ollama_requests_total") != 1 || metric(t, reg, "ollama_tokens_per_second") != 25 {
						t.Fatal("lost large-object metrics")
					}
				})
			}
		}
	}
}
func TestIncompleteAndUnaffectedHTTP(t *testing.T) {
	for _, tc := range []struct {
		name, path, body string
		status           int
	}{
		{"truncated", "/api/generate", terminal[:len(terminal)-1], 200},
		{"malformed", "/api/chat", `{"done":true,"response":"bad\q"}`, 200},
		{"error", "/api/chat", `{"error":"failed"}`, 200},
		{"http-error", "/api/generate", terminal, 500},
		{"unfinished", "/api/chat", `{"done":false}`, 200},
		{"unaffected", "/api/tags", terminal, 200},
		{"count-tokens", "/v1/messages/count_tokens", terminal, 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, _, reg := serveProxy(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			}))
			resp := post(t, srv, tc.path)
			got, err := io.ReadAll(resp.Body)
			resp.Body.Close()
			if err != nil || string(got) != tc.body || resp.StatusCode != tc.status {
				t.Fatal("fidelity", err)
			}
			waitFinished(t, reg, 1)
			if metric(t, reg, "ollama_requests_total") != 0 || histSampleCount(t, reg, "ollama_eval_duration_seconds") != 0 {
				t.Fatal("fabricated completion")
			}
		})
	}
}
func TestUpstreamReadFailureHTTP(t *testing.T) {
	srv, _, reg := serveProxy(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "1000")
		_, _ = io.WriteString(w, terminal)
		w.(http.Flusher).Flush()
	}))
	resp := post(t, srv, "/api/generate")
	_, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err == nil {
		t.Fatal("proxy swallowed upstream abort")
	}
	waitFinished(t, reg, 1)
	if metric(t, reg, "ollama_requests_total") != 0 {
		t.Fatal("failed read counted as completion")
	}
}
func TestCancellationHTTP(t *testing.T) {
	cancelled := make(chan struct{})
	srv, _, reg := serveProxy(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"done":false}`+"\n")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
		close(cancelled)
	}))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, srv.URL+"/api/chat", strings.NewReader(`{"model":"fixture"}`))
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_, err = bufio.NewReader(resp.Body).ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	resp.Body.Close()
	select {
	case <-cancelled:
	case <-time.After(5 * time.Second):
		t.Fatal("upstream not cancelled")
	}
	waitFinished(t, reg, 1)
	if metric(t, reg, "ollama_requests_total") != 0 {
		t.Fatal("cancelled completion counted")
	}
}
func TestConcurrentHTTPStreams(t *testing.T) {
	srv, _, reg := serveProxy(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, terminal) }))
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, err := srv.Client().Post(srv.URL+"/api/chat", "application/json", strings.NewReader(`{"model":"fixture"}`))
			if err != nil {
				t.Error(err)
				return
			}
			_, err = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	waitFinished(t, reg, 20)
	if metric(t, reg, "ollama_requests_total") != 20 || histSampleCount(t, reg, "ollama_eval_duration_seconds") != 20 {
		t.Fatal("concurrent observations")
	}
}

func TestFieldOrderAndLargeCompatibilityHTTP(t *testing.T) {
	for _, path := range []string{"/api/chat", "/v1/completions"} {
		for _, sse := range []bool{false, true} {
			if path == "/api/chat" && sse {
				continue
			}
			t.Run(fmt.Sprint(path, sse), func(t *testing.T) {
				body := terminal[:len(terminal)-1] + `,"response":"` + strings.Repeat(`escaped\"\\\u0061`, 90000) + `","context":[1,2,3]}`
				if path == "/v1/completions" {
					body = `{"usage":{"prompt_tokens":16,"completion_tokens":100},"choices":[{"text":"` + strings.Repeat(`escaped\"\\\u0061`, 90000) + `"}],"model":"fixture"}`
					if sse {
						body = "data: " + body + "\r\n\r\ndata: [DONE]\r\n\r\n"
					}
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
					t.Fatal("large escaped body", err)
				}
				waitFinished(t, reg, 1)
				if metric(t, reg, "ollama_requests_total") != 1 {
					t.Fatal("field order lost terminal")
				}
				if path == "/v1/completions" && metric(t, reg, "ollama_compatibility_output_tokens") != 100 {
					t.Fatal("large SSE lost usage")
				}
			})
		}
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type closedBody struct {
	io.ReadCloser
	closed chan struct{}
	once   sync.Once
}

func (b *closedBody) Close() error {
	err := b.ReadCloser.Close()
	b.once.Do(func() { close(b.closed) })
	return err
}
func TestReadFailureClosesUpstream(t *testing.T) {
	for _, path := range []string{"/api/chat", "/v1/chat/completions", "/v1/messages"} {
		t.Run(path, func(t *testing.T) {
			closed := make(chan struct{})
			body := terminal
			if path == "/v1/chat/completions" {
				body = "data: {\"usage\":{\"completion_tokens\":100}}\n\ndata: [DONE]\n\n"
			}
			if path == "/v1/messages" {
				body = "data: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":100}}\n\ndata: {\"type\":\"message_stop\"}\n\n"
			}
			srv, _, reg := serveProxy(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasPrefix(path, "/v1/") {
					w.Header().Set("Content-Type", "text/event-stream")
				}
				w.Header().Set("Content-Length", fmt.Sprint(len(body)+100))
				_, _ = io.WriteString(w, body)
				w.(http.Flusher).Flush()
			}), func(rp *httputil.ReverseProxy) {
				rp.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
					resp, err := http.DefaultTransport.RoundTrip(r)
					if err == nil {
						resp.Body = &closedBody{ReadCloser: resp.Body, closed: closed}
					}
					return resp, err
				})
			})
			resp := post(t, srv, path)
			_, err := io.ReadAll(resp.Body)
			resp.Body.Close()
			if err == nil {
				t.Fatal("upstream abort swallowed")
			}
			waitFinished(t, reg, 1)
			select {
			case <-closed:
			case <-time.After(5 * time.Second):
				t.Fatal("upstream body leaked")
			}
			if metric(t, reg, "ollama_requests_total") != 0 || histSampleCount(t, reg, "ollama_compatibility_output_tokens") != 0 {
				t.Fatal("failed body produced completion")
			}
		})
	}
}
func TestBoundedRequestPrefix(t *testing.T) {
	for _, lateModel := range []bool{false, true} {
		t.Run(fmt.Sprint(lateModel), func(t *testing.T) {
			request := `{"model":"fixture","prompt":"` + strings.Repeat("x", 128<<10) + `"}`
			if lateModel {
				request = `{"prompt":"` + strings.Repeat("x", 128<<10) + `","model":"fixture"}`
			}
			srv, _, reg := serveProxy(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				got, _ := io.ReadAll(r.Body)
				if string(got) != request {
					t.Error("large request changed")
				}
				_, _ = io.WriteString(w, terminal)
			}))
			resp, err := srv.Client().Post(srv.URL+"/api/chat", "application/json", strings.NewReader(request))
			if err != nil {
				t.Fatal(err)
			}
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			waitFinished(t, reg, 1)
			if metric(t, reg, "ollama_requests_total") != 1 {
				t.Fatal("large request lost completion")
			}
			want := "fixture"
			if lateModel {
				want = ""
			}
			fs, _ := reg.Gather()
			for _, f := range fs {
				if f.GetName() == "ollama_request_duration_seconds" {
					for _, l := range f.Metric[0].Label {
						if l.GetName() == "model" && l.GetValue() != want {
							t.Fatal("prefix model label", l)
						}
					}
				}
			}
		})
	}
}
func TestExcludedInferenceHTTP(t *testing.T) {
	srv, p, reg := serveProxy(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, terminal) }))
	p.excludePaths["/api/chat"] = struct{}{}
	resp := post(t, srv, "/api/chat")
	got, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(got) != terminal {
		t.Fatal("excluded response changed")
	}
	if histSampleCount(t, reg, "ollama_request_duration_seconds") != 0 || histSampleCount(t, reg, "ollama_eval_duration_seconds") != 0 {
		t.Fatal("excluded extraction")
	}
}
