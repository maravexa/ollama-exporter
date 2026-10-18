package collector

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

// This wrapper changes scheduling only: the transport's excess-byte check
// waits until the client has received the first flushed response event.
type delayedRequestEOF struct {
	io.ReadCloser
	remaining int64
	release   <-chan struct{}
}

func (b *delayedRequestEOF) Read(p []byte) (int, error) {
	if b.remaining == 0 {
		<-b.release
	}
	n, err := b.ReadCloser.Read(p)
	b.remaining -= int64(n)
	return n, err
}

type lifecycleTransport struct {
	base    http.RoundTripper
	release <-chan struct{}
	wrote   chan error
	written chan struct{}
}

func (d lifecycleTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.WithContext(httptrace.WithClientTrace(r.Context(), &httptrace.ClientTrace{WroteRequest: func(i httptrace.WroteRequestInfo) { d.wrote <- i.Err; close(d.written) }}))
	r.Body = &delayedRequestEOF{ReadCloser: r.Body, remaining: r.ContentLength, release: d.release}
	return d.base.RoundTrip(r)
}

func TestRequestEOFAfterResponseFlush(t *testing.T) {
	for _, private := range []bool{false, true} {
		for _, size := range []int{19, 64 << 10, (64 << 10) + 1} {
			t.Run(fmt.Sprintf("private=%t/size=%d", private, size), func(t *testing.T) {
				release := make(chan struct{})
				var once sync.Once
				unblock := func() { once.Do(func() { close(release) }) }
				defer unblock()
				wrote := make(chan error, 1)
				written := make(chan struct{})
				request := `{"model":"fixture"}` + strings.Repeat(" ", size-len(`{"model":"fixture"}`))
				upstreamRead := make(chan string, 1)
				first := "{\"response\":\"first\",\"done\":false}\n"
				up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					got, err := io.ReadAll(r.Body)
					upstreamRead <- fmt.Sprintf("bytes=%d err=%v equal=%t", len(got), err, string(got) == request)
					if err != nil {
						return
					}
					_, _ = io.WriteString(w, first)
					w.(http.Flusher).Flush()
					select {
					case <-r.Context().Done():
						return
					case <-written:
					}
					_, _ = io.WriteString(w, terminal)
				}))
				defer up.Close()
				target, _ := url.Parse(up.URL)
				p, _ := newTestProxy(t, nil)
				rp := p.reverseProxy(target)
				handler := p.instrumentedHandler(rp)
				base := http.DefaultTransport
				if private {
					tr := http.DefaultTransport.(*http.Transport).Clone()
					defer tr.CloseIdleConnections()
					base = tr
				}
				rp.Transport = lifecycleTransport{base: base, release: release, wrote: wrote, written: written}
				srv := httptest.NewServer(handler)
				defer srv.Close()
				defer unblock()
				client := srv.Client()
				client.Timeout = 10 * time.Second
				resp, err := client.Post(srv.URL+"/api/chat", "application/json", strings.NewReader(request))
				if err != nil {
					t.Fatal(err)
				}
				defer resp.Body.Close()
				got := make([]byte, len(first))
				if _, firstErr := io.ReadFull(resp.Body, got); firstErr != nil || string(got) != first {
					unblock()
					t.Fatalf("first event: %v", firstErr)
				}
				unblock()
				rest, err := io.ReadAll(resp.Body)
				select {
				case writeErr := <-wrote:
					if writeErr != nil {
						t.Errorf("transport WroteRequest: %v", writeErr)
					}
				case <-time.After(time.Second):
					t.Error("missing WroteRequest")
				}
				if result := <-upstreamRead; result != fmt.Sprintf("bytes=%d err=<nil> equal=true", len(request)) {
					t.Error(result)
				}
				if err != nil || string(rest) != terminal {
					t.Fatalf("response remainder: bytes=%d err=%v", len(rest), err)
				}
			})
		}
	}
}
