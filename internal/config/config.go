// Package config handles loading and validating exporter configuration.
package config

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"strconv"
	"strings"
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
// A missing file is not fatal; defaults are used and a warning is logged to stderr.
func Load(path string) (*Config, error) {
	cfg := defaults()

	data, err := os.ReadFile(path) //nolint:gosec // path is operator-supplied via --config flag
	if err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("reading config %s: %w", path, err)
	}
	if err == nil {
		if parseErr := parseYAML(data, cfg); parseErr != nil {
			return nil, fmt.Errorf("parsing config %s: %w", path, parseErr)
		}
	}

	// Environment variable overrides applied after file values.
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

// parseYAML parses a minimal YAML subset into cfg. Only fields explicitly present
// in the file override the existing cfg values; absent fields retain their defaults.
//
// Supported format:
//   - Top-level scalar key: value pairs
//   - Two nested sections: proxy: and gpu: (2-space indent)
//   - Block list under proxy.exclude_paths (4-space indent, "- item")
//   - Inline comments after " #"
//   - Quoted and unquoted string values
//
// Inline lists (exclude_paths: ["/", ...]) are not supported.
func parseYAML(data []byte, cfg *Config) error {
	type section int
	const (
		sectionTop   section = iota
		sectionProxy section = iota
		sectionGPU   section = iota
	)

	current := sectionTop
	inExcludePaths := false

	scanner := bufio.NewScanner(bytes.NewReader(data))
	for scanner.Scan() {
		raw := scanner.Text()

		// Strip inline comment (YAML: space then hash signals a comment).
		if ci := strings.Index(raw, " #"); ci >= 0 {
			raw = raw[:ci]
		}

		trimmed := strings.TrimSpace(raw)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") || trimmed == "---" {
			continue
		}

		indent := len(raw) - len(strings.TrimLeft(raw, " \t"))

		// Block list item (indent >= 4, starts with "- ").
		if inExcludePaths && indent >= 4 && strings.HasPrefix(trimmed, "- ") {
			cfg.Proxy.ExcludePaths = append(cfg.Proxy.ExcludePaths, unquote(trimmed[2:]))
			continue
		}

		// Any non-list line exits the exclude_paths list context.
		inExcludePaths = false

		// Section header: zero indent, no spaces in key, ends with ":".
		if indent == 0 && strings.HasSuffix(trimmed, ":") && !strings.Contains(trimmed[:len(trimmed)-1], " ") {
			switch trimmed {
			case "proxy:":
				current = sectionProxy
			case "gpu:":
				current = sectionGPU
			default:
				current = sectionTop
			}
			continue
		}

		// Key: value pair.
		ci := strings.Index(trimmed, ":")
		if ci < 0 {
			continue
		}
		key := trimmed[:ci]
		val := unquote(strings.TrimSpace(trimmed[ci+1:]))

		switch current {
		case sectionTop:
			if err := applyTopField(cfg, key, val); err != nil {
				return err
			}
		case sectionProxy:
			if key == "exclude_paths" {
				cfg.Proxy.ExcludePaths = nil // reset; block list items follow
				inExcludePaths = true
				continue
			}
			if err := applyProxyField(&cfg.Proxy, key, val); err != nil {
				return err
			}
		case sectionGPU:
			if err := applyGPUField(&cfg.GPU, key, val); err != nil {
				return err
			}
		}
	}
	return scanner.Err()
}

func applyTopField(cfg *Config, key, val string) error {
	if val == "" {
		return nil
	}
	switch key {
	case "ollama_url":
		cfg.OllamaURL = val
	case "listen_addr":
		cfg.ListenAddr = val
	case "poll_interval":
		d, err := time.ParseDuration(val)
		if err != nil {
			return fmt.Errorf("poll_interval: %w", err)
		}
		cfg.PollInterval = d
	case "log_level":
		cfg.LogLevel = val
	}
	return nil
}

func applyProxyField(p *ProxyConfig, key, val string) error {
	switch key {
	case "enabled":
		b, err := strconv.ParseBool(val)
		if err != nil {
			return fmt.Errorf("proxy.enabled: %w", err)
		}
		p.Enabled = b
	case "listen_addr":
		if val != "" {
			p.ListenAddr = val
		}
	}
	return nil
}

func applyGPUField(g *GPUConfig, key, val string) error {
	switch key {
	case "enabled":
		b, err := strconv.ParseBool(val)
		if err != nil {
			return fmt.Errorf("gpu.enabled: %w", err)
		}
		g.Enabled = b
	case "poll_interval":
		if val != "" {
			d, err := time.ParseDuration(val)
			if err != nil {
				return fmt.Errorf("gpu.poll_interval: %w", err)
			}
			g.PollInterval = d
		}
	case "sysfs_base":
		if val != "" {
			g.SysfsBase = val
		}
	}
	return nil
}

// unquote removes surrounding double quotes from s if present.
func unquote(s string) string {
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		return s[1 : len(s)-1]
	}
	return s
}
