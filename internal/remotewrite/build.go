package remotewrite

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// BuildOptions holds the inputs needed to construct a Pusher from a
// RemoteWriteConfig-equivalent struct without coupling this package to the
// internal/config package.
type BuildOptions struct {
	Logger        *slog.Logger
	Gatherer      prometheus.Gatherer
	Registerer    prometheus.Registerer
	Endpoints     []EndpointBuildSpec
	FlushInterval time.Duration
	DrainTimeout  time.Duration
}

// EndpointBuildSpec is the resolved view of one remote_write entry. It
// holds raw credential file paths (resolved at construction time) and
// raw header maps; the build step converts these into typed Sender
// arguments.
type EndpointBuildSpec struct {
	Name              string
	URL               string
	Timeout           time.Duration
	Retry             RetryBuildSpec
	Breaker           BreakerBuildSpec
	Queue             QueueBuildSpec
	TLSCAFile         string
	TLSInsecureVerify bool
	InsecureHTTP      bool
	BasicAuthUser     string
	BasicAuthPassFile string
	BearerTokenFile   string
	Headers           map[string]string
	ExternalLabels    map[string]string
}

type RetryBuildSpec struct {
	MaxAttempts    int
	MaxElapsed     time.Duration
	InitialBackoff time.Duration
	MaxBackoff     time.Duration
}

type BreakerBuildSpec struct {
	FailureThreshold int
	Window           time.Duration
	Cooldown         time.Duration
}

type QueueBuildSpec struct {
	Capacity int
}

// Build assembles a Pusher and its Sender(s) from opts. The returned
// Pusher must be Run by the caller.
//
// v0.4.0: only the first endpoint is wired. If more were configured, the
// caller is expected to have already logged a warning at config load.
func Build(opts BuildOptions) (*Pusher, error) {
	if len(opts.Endpoints) == 0 {
		return nil, errors.New("remotewrite.Build: no endpoints provided")
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.Gatherer == nil {
		return nil, errors.New("remotewrite.Build: gatherer is required")
	}

	metrics := newSelfMetrics(opts.Registerer)

	spec := opts.Endpoints[0]
	endpointLabel := spec.Name
	if endpointLabel == "" {
		endpointLabel = hostFromURL(spec.URL)
	}

	user, pass, token, err := loadCreds(spec)
	if err != nil {
		return nil, err
	}

	httpClient, err := newHTTPClient(spec)
	if err != nil {
		return nil, err
	}

	queueObs := metrics.queueObserver(endpointLabel)
	queueCap := spec.Queue.Capacity
	if queueCap <= 0 {
		queueCap = 10000
	}
	q := NewQueue(queueCap, queueObs)

	breakerObs := metrics.breakerObserver(endpointLabel)
	breaker := NewCircuitBreaker(BreakerConfig{
		FailureThreshold: spec.Breaker.FailureThreshold,
		Window:           spec.Breaker.Window,
		Cooldown:         spec.Breaker.Cooldown,
	}, breakerObs)

	endpointCfg := EndpointConfig{
		Name:           endpointLabel,
		URL:            spec.URL,
		Timeout:        spec.Timeout,
		MaxAttempts:    spec.Retry.MaxAttempts,
		MaxElapsed:     spec.Retry.MaxElapsed,
		InitialBackoff: spec.Retry.InitialBackoff,
		MaxBackoff:     spec.Retry.MaxBackoff,
		Headers:        spec.Headers,
		BasicAuthUser:  user,
		BasicAuthPass:  pass,
		BearerToken:    token,
		ExternalLabels: NewLabels(spec.ExternalLabels),
	}

	senderObs := metrics.senderObserver(endpointLabel)
	sender := NewSender(endpointCfg, q, httpClient, breaker, senderObs, opts.Logger)

	flush := opts.FlushInterval
	if flush <= 0 {
		flush = 10 * time.Second
	}
	pusher := NewPusher(PusherConfig{
		FlushInterval:  flush,
		Gatherer:       opts.Gatherer,
		ExternalLabels: NewLabels(spec.ExternalLabels),
		DrainTimeout:   opts.DrainTimeout,
		Logger:         opts.Logger,
	}, []*Sender{sender})

	return pusher, nil
}

// loadCreds reads credentials from disk. Trailing whitespace is trimmed
// because systemd LoadCredential files often include a newline.
func loadCreds(spec EndpointBuildSpec) (user, pass, token string, err error) {
	if spec.BearerTokenFile != "" {
		b, e := os.ReadFile(spec.BearerTokenFile) //nolint:gosec // file path is operator-supplied via config
		if e != nil {
			return "", "", "", fmt.Errorf("read bearer_token_file %q: %w", spec.BearerTokenFile, e)
		}
		token = strings.TrimRight(string(b), "\r\n\t ")
	}
	if spec.BasicAuthUser != "" {
		user = spec.BasicAuthUser
		if spec.BasicAuthPassFile != "" {
			b, e := os.ReadFile(spec.BasicAuthPassFile) //nolint:gosec // file path is operator-supplied via config
			if e != nil {
				return "", "", "", fmt.Errorf("read password_file %q: %w", spec.BasicAuthPassFile, e)
			}
			pass = strings.TrimRight(string(b), "\r\n\t ")
		}
	}
	return user, pass, token, nil
}

// newHTTPClient builds the per-endpoint HTTP client with HTTP/2 and TLS
// configuration. A single client is reused across all batches so the
// underlying connection is pooled.
func newHTTPClient(spec EndpointBuildSpec) (*http.Client, error) {
	tlsCfg := &tls.Config{
		MinVersion:         tls.VersionTLS12,
		InsecureSkipVerify: spec.TLSInsecureVerify, //nolint:gosec // operator opt-in via config
	}
	if spec.TLSCAFile != "" {
		pem, err := os.ReadFile(spec.TLSCAFile) //nolint:gosec // file path is operator-supplied via config
		if err != nil {
			return nil, fmt.Errorf("read tls.ca_file %q: %w", spec.TLSCAFile, err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("tls.ca_file %q contains no usable certs", spec.TLSCAFile)
		}
		tlsCfg.RootCAs = pool
	}

	transport := &http.Transport{
		TLSClientConfig:       tlsCfg,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          10,
		MaxIdleConnsPerHost:   2,
		IdleConnTimeout:       90 * time.Second,
		ExpectContinueTimeout: time.Second,
	}

	timeout := spec.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	return &http.Client{
		Transport: transport,
		Timeout:   timeout,
	}, nil
}

func hostFromURL(s string) string {
	idx := strings.Index(s, "://")
	if idx < 0 {
		return s
	}
	rest := s[idx+3:]
	if slash := strings.Index(rest, "/"); slash >= 0 {
		rest = rest[:slash]
	}
	return rest
}
