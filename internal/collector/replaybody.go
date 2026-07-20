package collector

import (
	"bytes"
	"io"
)

// replayBody is an io.ReadCloser over a fully-buffered request body that
// also retains the bytes for later inspection.
//
// The proxy reads the request body up front to extract the model name, then
// hands a fresh reader upstream. Keeping the buffer addressable lets
// modifyResponse recover the model for compatibility-API responses, which
// never name it in a streaming delta.
type replayBody struct {
	buf []byte
	r   *bytes.Reader
}

// newReplayBody wraps body, which must not be mutated afterwards.
func newReplayBody(body []byte) *replayBody {
	return &replayBody{buf: body, r: bytes.NewReader(body)}
}

func (b *replayBody) Read(p []byte) (int, error) { return b.r.Read(p) }

func (b *replayBody) Close() error { return nil }

var _ io.ReadCloser = (*replayBody)(nil)
