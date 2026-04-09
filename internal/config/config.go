// Package config handles loading and validating exporter configuration.
package config

import (
	"fmt"
	"os"
	"time"
)

// Config holds all runtime configuration for the exporter.
type Config struct {
	// OllamaURL is the base URL of the Ollama API.
	OllamaURL string

	// ListenAddr is the address the metrics HTTP server binds to.
	ListenAddr string

	// PollInterval controls how often the poller scrapes /api/ps and /api/tags.
	PollInterval time.Duration

	// Proxy holds proxy mode configuration.
	Proxy ProxyConfig

	// GPU holds AMD GPU hardware metric collection configuration.
	GPU GPUConfig

	// LogLevel controls structured log verbosity.
	LogLevel string
}

// GPUConfig holds configuration for AMD GPU hardware metric collection via sysfs.
// No ROCm userspace packages are required — only the amdgpu kernel driver.
type GPUConfig struct {
	// Enabled activates GPU metric collection via the amdgpu sysfs interface.
	Enabled bool

	// PollInterval controls how often GPU metrics are read from sysfs.
	// If zero, the main PollInterval is used.
	PollInterval time.Duration

	// SysfsBase is the root of the DRM sysfs tree.
	// Defaults to /sys/class/drm. Override for testing.
	SysfsBase string
}

// ProxyConfig holds configuration for the transparent proxy mode.
type ProxyConfig struct {
	// Enabled activates the reverse proxy listener.
	Enabled bool

	// ListenAddr is the address the proxy HTTP server binds to.
	ListenAddr string

	// ExcludePaths is a list of request paths for which the proxy will
	// forward traffic but skip all Prometheus metric recording. Use this
	// to suppress noise from internal polling calls (/api/ps, /api/tags,
	// etc.) that would otherwise inflate label cardinality.
	ExcludePaths []string
}

// Load reads configuration from the given YAML file path,
// applying environment variable overrides where present.
// TODO: implement YAML parsing via encoding/json or a minimal YAML parser.
func Load(path string) (*Config, error) {
	cfg := defaults()

	// Environment variable overrides.
	if v := os.Getenv("OLLAMA_URL"); v != "" {
		cfg.OllamaURL = v
	}
	if v := os.Getenv("LISTEN_ADDR"); v != "" {
		cfg.ListenAddr = v
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
		return fmt.Errorf("ollama_url must not be empty")
	}
	if c.PollInterval < time.Second {
		return fmt.Errorf("poll_interval must be >= 1s")
	}
	return nil
}
