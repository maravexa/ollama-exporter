// Package config handles loading and validating exporter configuration.
package config

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"time"
)

// Config holds all runtime configuration for the exporter.
type Config struct {
	// OllamaURL is the base URL of the Ollama API.
	OllamaURL string

	// ListenAddr is the address the metrics HTTP server binds to.
	// Deprecated: superseded by MetricsEndpoint.ListenAddress; retained for
	// backward compatibility with existing config files.
	ListenAddr string

	// PollInterval controls how often the poller scrapes /api/ps and /api/tags.
	PollInterval time.Duration

	// MetricsEndpoint controls the local /metrics HTTP listener. When
	// MetricsEndpoint.Enabled is false the listener is not started, enabling
	// push-only deployments where remote_write is the only output.
	MetricsEndpoint MetricsEndpointConfig

	// Proxy holds proxy mode configuration.
	Proxy ProxyConfig

	// GPU holds AMD GPU hardware metric collection configuration.
	GPU GPUConfig

	// RemoteWrite holds zero or more Prometheus Remote Write endpoint
	// configurations. v0.4.0 processes only the first entry; multi-endpoint
	// fan-out is planned for v0.5.
	RemoteWrite []RemoteWriteConfig

	// LogLevel controls structured log verbosity.
	LogLevel string
}

// MetricsEndpointConfig configures the local /metrics HTTP listener.
type MetricsEndpointConfig struct {
	// Enabled is true by default. Set to false for push-only deployments.
	Enabled bool

	// ListenAddress overrides the legacy top-level ListenAddr if set.
	ListenAddress string
}

// GPUConfig holds configuration for AMD GPU hardware metric collection.
type GPUConfig struct {
	Enabled      bool
	PollInterval time.Duration
	SysfsBase    string
}

// ProxyConfig holds configuration for the transparent proxy mode.
type ProxyConfig struct {
	Enabled      bool
	ListenAddr   string
	ExcludePaths []string
}

// RemoteWriteConfig configures a single Prometheus Remote Write endpoint.
//
// Credentials must be referenced via *_file fields; plaintext password and
// bearer_token keys are rejected at config load.
type RemoteWriteConfig struct {
	URL             string
	Name            string
	FlushInterval   time.Duration
	Timeout         time.Duration
	InsecureHTTP    bool
	Queue           QueueConfig
	Retry           RetryConfig
	CircuitBreaker  CircuitBreakerConfig
	TLS             TLSConfig
	BasicAuth       *BasicAuthConfig
	BearerTokenFile string
	Headers         map[string]string
	ExternalLabels  map[string]string
}

// QueueConfig sizes the in-memory ring buffer for a single endpoint.
type QueueConfig struct {
	Capacity int // batches, not samples
}

// RetryConfig bounds the per-batch retry budget.
type RetryConfig struct {
	MaxAttempts    int
	MaxElapsed     time.Duration
	InitialBackoff time.Duration
	MaxBackoff     time.Duration
}

// CircuitBreakerConfig governs the per-endpoint circuit breaker.
type CircuitBreakerConfig struct {
	FailureThreshold int
	Window           time.Duration
	Cooldown         time.Duration
}

// TLSConfig configures TLS verification for the endpoint.
type TLSConfig struct {
	InsecureSkipVerify bool
	CAFile             string
}

// BasicAuthConfig holds HTTP Basic auth credentials. Only PasswordFile is
// honored; an inline `password` key triggers a load-time error.
type BasicAuthConfig struct {
	Username     string
	PasswordFile string
}

// Load reads configuration from path, applies env-var overrides, and
// validates the result. A missing file is not fatal — defaults are used.
func Load(path string) (*Config, error) {
	cfg := defaults()

	data, err := os.ReadFile(path) //nolint:gosec // path is operator-supplied via --config flag
	if err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("reading config %s: %w", path, err)
	}
	if err == nil {
		tree, parseErr := parseYAMLTree(data)
		if parseErr != nil {
			return nil, fmt.Errorf("parsing config %s: %w", path, parseErr)
		}
		if applyErr := applyTree(cfg, tree); applyErr != nil {
			return nil, fmt.Errorf("applying config %s: %w", path, applyErr)
		}
	}

	if v := os.Getenv("OLLAMA_URL"); v != "" {
		cfg.OllamaURL = v
	}
	if v := os.Getenv("LISTEN_ADDR"); v != "" {
		cfg.ListenAddr = v
	}

	// Honor MetricsEndpoint.ListenAddress as the canonical listener; if a
	// caller only set the legacy ListenAddr we mirror it through.
	if cfg.MetricsEndpoint.ListenAddress == "" {
		cfg.MetricsEndpoint.ListenAddress = cfg.ListenAddr
	} else {
		cfg.ListenAddr = cfg.MetricsEndpoint.ListenAddress
	}

	if err := cfg.validate(); err != nil {
		return nil, fmt.Errorf("invalid config: %w", err)
	}

	return cfg, nil
}

func defaults() *Config {
	return &Config{
		OllamaURL:    "http://localhost:11434",
		ListenAddr:   ":9400",
		PollInterval: 15 * time.Second,
		LogLevel:     "info",
		MetricsEndpoint: MetricsEndpointConfig{
			Enabled:       true,
			ListenAddress: "",
		},
		Proxy: ProxyConfig{
			Enabled:    true,
			ListenAddr: ":9401",
			ExcludePaths: []string{
				"/",
				"/api/ps",
				"/api/tags",
				"/api/show",
				"/api/version",
			},
		},
		GPU: GPUConfig{
			Enabled:   true,
			SysfsBase: "/sys/class/drm",
		},
	}
}

func (c *Config) validate() error {
	if c.OllamaURL == "" {
		return errors.New("ollama_url must not be empty")
	}
	if c.PollInterval < time.Second {
		return errors.New("poll_interval must be >= 1s")
	}

	for i := range c.RemoteWrite {
		if err := validateRemoteWrite(i, &c.RemoteWrite[i]); err != nil {
			return err
		}
	}

	if !c.MetricsEndpoint.Enabled && len(c.RemoteWrite) == 0 {
		return errors.New("metrics_endpoint.enabled is false and no remote_write endpoints configured: exporter would be a no-op")
	}

	return nil
}

func validateRemoteWrite(idx int, rw *RemoteWriteConfig) error {
	pathPrefix := fmt.Sprintf("remote_write[%d]", idx)

	if rw.URL == "" {
		return fmt.Errorf("%s.url is required", pathPrefix)
	}
	u, err := url.Parse(rw.URL)
	if err != nil {
		return fmt.Errorf("%s.url is not a valid URL: %w", pathPrefix, err)
	}
	switch u.Scheme {
	case "https":
	case "http":
		if !rw.InsecureHTTP {
			return fmt.Errorf("%s.url uses http://; set %s.insecure_http: true to allow plaintext transport", pathPrefix, pathPrefix)
		}
	default:
		return fmt.Errorf("%s.url scheme must be https (or http with insecure_http: true), got %q", pathPrefix, u.Scheme)
	}

	if rw.BasicAuth != nil && rw.BearerTokenFile != "" {
		return fmt.Errorf("%s: basic_auth and bearer_token_file are mutually exclusive", pathPrefix)
	}

	for k := range rw.Headers {
		ck := http.CanonicalHeaderKey(k)
		switch ck {
		case "Authorization", "Content-Encoding", "Content-Type", "X-Prometheus-Remote-Write-Version":
			return fmt.Errorf("%s.headers must not contain reserved header %q (set via auth fields or omit)", pathPrefix, k)
		}
	}

	if rw.FlushInterval < 0 {
		return fmt.Errorf("%s.flush_interval must be positive, got %s", pathPrefix, rw.FlushInterval)
	}
	if rw.FlushInterval > 0 && rw.FlushInterval < time.Second {
		return fmt.Errorf("%s.flush_interval must be >= 1s, got %s", pathPrefix, rw.FlushInterval)
	}
	if rw.Timeout < 0 {
		return fmt.Errorf("%s.timeout must be positive", pathPrefix)
	}
	if rw.Retry.MaxAttempts < 0 {
		return fmt.Errorf("%s.retry.max_attempts must be positive", pathPrefix)
	}
	if rw.Retry.InitialBackoff < 0 {
		return fmt.Errorf("%s.retry.initial_backoff must be positive", pathPrefix)
	}
	if rw.Retry.MaxBackoff < 0 {
		return fmt.Errorf("%s.retry.max_backoff must be positive", pathPrefix)
	}
	if rw.Retry.MaxElapsed < 0 {
		return fmt.Errorf("%s.retry.max_elapsed must be positive", pathPrefix)
	}
	if rw.CircuitBreaker.Window < 0 {
		return fmt.Errorf("%s.circuit_breaker.window must be positive", pathPrefix)
	}
	if rw.CircuitBreaker.Cooldown < 0 {
		return fmt.Errorf("%s.circuit_breaker.cooldown must be positive", pathPrefix)
	}
	if rw.Queue.Capacity < 0 {
		return fmt.Errorf("%s.queue.capacity must be positive", pathPrefix)
	}

	return nil
}

// applyTree walks the parsed YAML tree and populates cfg. Unknown keys are
// silently ignored to keep config files forward-compatible.
func applyTree(cfg *Config, root *node) error {
	if root == nil || !root.isMap() {
		return nil
	}
	for _, e := range root.mapVal {
		switch e.key {
		case "ollama_url":
			cfg.OllamaURL = e.value.asString()
		case "listen_addr":
			cfg.ListenAddr = e.value.asString()
		case "poll_interval":
			d, err := parseDuration(e.value.asString())
			if err != nil {
				return fmt.Errorf("poll_interval: %w", err)
			}
			cfg.PollInterval = d
		case "log_level":
			cfg.LogLevel = e.value.asString()
		case "metrics_endpoint":
			if err := applyMetricsEndpoint(&cfg.MetricsEndpoint, e.value); err != nil {
				return err
			}
		case "proxy":
			if err := applyProxy(&cfg.Proxy, e.value); err != nil {
				return err
			}
		case "gpu":
			if err := applyGPU(&cfg.GPU, e.value); err != nil {
				return err
			}
		case "remote_write":
			if err := applyRemoteWrite(&cfg.RemoteWrite, e.value); err != nil {
				return err
			}
		}
	}
	return nil
}

func applyMetricsEndpoint(m *MetricsEndpointConfig, n *node) error {
	if n == nil || !n.isMap() {
		return nil
	}
	for _, e := range n.mapVal {
		switch e.key {
		case "enabled":
			b, err := strconv.ParseBool(e.value.asString())
			if err != nil {
				return fmt.Errorf("metrics_endpoint.enabled: %w", err)
			}
			m.Enabled = b
		case "listen_address":
			m.ListenAddress = e.value.asString()
		}
	}
	return nil
}

func applyProxy(p *ProxyConfig, n *node) error {
	if n == nil || !n.isMap() {
		return nil
	}
	for _, e := range n.mapVal {
		switch e.key {
		case "enabled":
			b, err := strconv.ParseBool(e.value.asString())
			if err != nil {
				return fmt.Errorf("proxy.enabled: %w", err)
			}
			p.Enabled = b
		case "listen_addr":
			if v := e.value.asString(); v != "" {
				p.ListenAddr = v
			}
		case "exclude_paths":
			if e.value == nil || !e.value.isList() {
				continue
			}
			p.ExcludePaths = nil
			for _, item := range e.value.listVal {
				p.ExcludePaths = append(p.ExcludePaths, item.asString())
			}
		}
	}
	return nil
}

func applyGPU(g *GPUConfig, n *node) error {
	if n == nil || !n.isMap() {
		return nil
	}
	for _, e := range n.mapVal {
		switch e.key {
		case "enabled":
			b, err := strconv.ParseBool(e.value.asString())
			if err != nil {
				return fmt.Errorf("gpu.enabled: %w", err)
			}
			g.Enabled = b
		case "poll_interval":
			if v := e.value.asString(); v != "" {
				d, err := parseDuration(v)
				if err != nil {
					return fmt.Errorf("gpu.poll_interval: %w", err)
				}
				g.PollInterval = d
			}
		case "sysfs_base":
			if v := e.value.asString(); v != "" {
				g.SysfsBase = v
			}
		}
	}
	return nil
}

func applyRemoteWrite(out *[]RemoteWriteConfig, n *node) error {
	if n == nil || !n.isList() {
		return nil
	}
	for i, item := range n.listVal {
		rw, err := parseRemoteWriteEntry(i, item)
		if err != nil {
			return err
		}
		*out = append(*out, rw)
	}
	return nil
}

func parseRemoteWriteEntry(idx int, n *node) (RemoteWriteConfig, error) {
	rw := RemoteWriteConfig{
		Headers:        map[string]string{},
		ExternalLabels: map[string]string{},
	}
	if n == nil || !n.isMap() {
		return rw, fmt.Errorf("remote_write[%d] is not a map", idx)
	}
	pathPrefix := fmt.Sprintf("remote_write[%d]", idx)

	for _, e := range n.mapVal {
		switch e.key {
		case "url":
			rw.URL = e.value.asString()
		case "name":
			rw.Name = e.value.asString()
		case "flush_interval":
			d, err := parseDuration(e.value.asString())
			if err != nil {
				return rw, fmt.Errorf("%s.flush_interval: %w", pathPrefix, err)
			}
			rw.FlushInterval = d
		case "timeout":
			d, err := parseDuration(e.value.asString())
			if err != nil {
				return rw, fmt.Errorf("%s.timeout: %w", pathPrefix, err)
			}
			rw.Timeout = d
		case "insecure_http":
			b, err := strconv.ParseBool(e.value.asString())
			if err != nil {
				return rw, fmt.Errorf("%s.insecure_http: %w", pathPrefix, err)
			}
			rw.InsecureHTTP = b
		case "bearer_token":
			return rw, fmt.Errorf("plaintext credentials forbidden; use bearer_token_file (offending field: %s.bearer_token)", pathPrefix)
		case "bearer_token_file":
			rw.BearerTokenFile = e.value.asString()
		case "queue":
			if err := applyQueue(&rw.Queue, e.value, pathPrefix); err != nil {
				return rw, err
			}
		case "retry":
			if err := applyRetry(&rw.Retry, e.value, pathPrefix); err != nil {
				return rw, err
			}
		case "circuit_breaker":
			if err := applyBreaker(&rw.CircuitBreaker, e.value, pathPrefix); err != nil {
				return rw, err
			}
		case "tls":
			if err := applyTLS(&rw.TLS, e.value, pathPrefix); err != nil {
				return rw, err
			}
		case "basic_auth":
			ba, err := applyBasicAuth(e.value, pathPrefix)
			if err != nil {
				return rw, err
			}
			rw.BasicAuth = ba
		case "headers":
			if e.value != nil && e.value.isMap() {
				for _, h := range e.value.mapVal {
					rw.Headers[h.key] = h.value.asString()
				}
			}
		case "external_labels":
			if e.value != nil && e.value.isMap() {
				for _, h := range e.value.mapVal {
					rw.ExternalLabels[h.key] = h.value.asString()
				}
			}
		}
	}

	return rw, nil
}

func applyQueue(q *QueueConfig, n *node, pathPrefix string) error {
	if n == nil || !n.isMap() {
		return nil
	}
	for _, e := range n.mapVal {
		if e.key == "capacity" {
			v, err := strconv.Atoi(e.value.asString())
			if err != nil {
				return fmt.Errorf("%s.queue.capacity: %w", pathPrefix, err)
			}
			q.Capacity = v
		}
	}
	return nil
}

func applyRetry(r *RetryConfig, n *node, pathPrefix string) error {
	if n == nil || !n.isMap() {
		return nil
	}
	for _, e := range n.mapVal {
		switch e.key {
		case "max_attempts":
			v, err := strconv.Atoi(e.value.asString())
			if err != nil {
				return fmt.Errorf("%s.retry.max_attempts: %w", pathPrefix, err)
			}
			r.MaxAttempts = v
		case "max_elapsed":
			d, err := parseDuration(e.value.asString())
			if err != nil {
				return fmt.Errorf("%s.retry.max_elapsed: %w", pathPrefix, err)
			}
			r.MaxElapsed = d
		case "initial_backoff":
			d, err := parseDuration(e.value.asString())
			if err != nil {
				return fmt.Errorf("%s.retry.initial_backoff: %w", pathPrefix, err)
			}
			r.InitialBackoff = d
		case "max_backoff":
			d, err := parseDuration(e.value.asString())
			if err != nil {
				return fmt.Errorf("%s.retry.max_backoff: %w", pathPrefix, err)
			}
			r.MaxBackoff = d
		}
	}
	return nil
}

func applyBreaker(b *CircuitBreakerConfig, n *node, pathPrefix string) error {
	if n == nil || !n.isMap() {
		return nil
	}
	for _, e := range n.mapVal {
		switch e.key {
		case "failure_threshold":
			v, err := strconv.Atoi(e.value.asString())
			if err != nil {
				return fmt.Errorf("%s.circuit_breaker.failure_threshold: %w", pathPrefix, err)
			}
			b.FailureThreshold = v
		case "window":
			d, err := parseDuration(e.value.asString())
			if err != nil {
				return fmt.Errorf("%s.circuit_breaker.window: %w", pathPrefix, err)
			}
			b.Window = d
		case "cooldown":
			d, err := parseDuration(e.value.asString())
			if err != nil {
				return fmt.Errorf("%s.circuit_breaker.cooldown: %w", pathPrefix, err)
			}
			b.Cooldown = d
		}
	}
	return nil
}

func applyTLS(t *TLSConfig, n *node, pathPrefix string) error {
	if n == nil || !n.isMap() {
		return nil
	}
	for _, e := range n.mapVal {
		switch e.key {
		case "insecure_skip_verify":
			v, err := strconv.ParseBool(e.value.asString())
			if err != nil {
				return fmt.Errorf("%s.tls.insecure_skip_verify: %w", pathPrefix, err)
			}
			t.InsecureSkipVerify = v
		case "ca_file":
			t.CAFile = e.value.asString()
		}
	}
	return nil
}

func applyBasicAuth(n *node, pathPrefix string) (*BasicAuthConfig, error) {
	if n == nil || !n.isMap() {
		return nil, nil
	}
	ba := &BasicAuthConfig{}
	for _, e := range n.mapVal {
		switch e.key {
		case "username":
			ba.Username = e.value.asString()
		case "password":
			return nil, fmt.Errorf("plaintext credentials forbidden; use password_file (offending field: %s.basic_auth.password)", pathPrefix)
		case "password_file":
			ba.PasswordFile = e.value.asString()
		}
	}
	return ba, nil
}

// parseDuration is a thin wrapper that surfaces empty strings as a
// helpful error rather than silently accepting them.
func parseDuration(s string) (time.Duration, error) {
	if s == "" {
		return 0, errors.New("empty duration")
	}
	return time.ParseDuration(s)
}
