package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeTemp(t *testing.T, content string) string {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "config-*.yml")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(content); err != nil {
		t.Fatal(err)
	}
	f.Close()
	return f.Name()
}

func TestLoad_MissingFile_UsesDefaults(t *testing.T) {
	cfg, err := Load(filepath.Join(t.TempDir(), "nonexistent.yml"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.OllamaURL != "http://localhost:11434" {
		t.Errorf("OllamaURL = %q, want default", cfg.OllamaURL)
	}
	if cfg.ListenAddr != ":9400" {
		t.Errorf("ListenAddr = %q, want :9400", cfg.ListenAddr)
	}
	if cfg.PollInterval != 15*time.Second {
		t.Errorf("PollInterval = %v, want 15s", cfg.PollInterval)
	}
	if !cfg.Proxy.Enabled {
		t.Error("Proxy.Enabled should default to true")
	}
	if !cfg.GPU.Enabled {
		t.Error("GPU.Enabled should default to true")
	}
}

func TestLoad_YAMLOverridesSelected(t *testing.T) {
	yaml := `
ollama_url: "http://remote:11434"
listen_addr: ":9500"
poll_interval: "30s"
log_level: "debug"

proxy:
  enabled: false
  listen_addr: ":9501"
  exclude_paths:
    - "/custom"

gpu:
  enabled: false
  sysfs_base: "/tmp/drm"
`
	path := writeTemp(t, yaml)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if cfg.OllamaURL != "http://remote:11434" {
		t.Errorf("OllamaURL = %q", cfg.OllamaURL)
	}
	if cfg.ListenAddr != ":9500" {
		t.Errorf("ListenAddr = %q", cfg.ListenAddr)
	}
	if cfg.PollInterval != 30*time.Second {
		t.Errorf("PollInterval = %v", cfg.PollInterval)
	}
	if cfg.LogLevel != "debug" {
		t.Errorf("LogLevel = %q", cfg.LogLevel)
	}
	if cfg.Proxy.Enabled {
		t.Error("Proxy.Enabled should be false")
	}
	if cfg.Proxy.ListenAddr != ":9501" {
		t.Errorf("Proxy.ListenAddr = %q", cfg.Proxy.ListenAddr)
	}
	if len(cfg.Proxy.ExcludePaths) != 1 || cfg.Proxy.ExcludePaths[0] != "/custom" {
		t.Errorf("ExcludePaths = %v", cfg.Proxy.ExcludePaths)
	}
	if cfg.GPU.Enabled {
		t.Error("GPU.Enabled should be false")
	}
	if cfg.GPU.SysfsBase != "/tmp/drm" {
		t.Errorf("SysfsBase = %q", cfg.GPU.SysfsBase)
	}
}

func TestLoad_PartialYAML_DefaultsPreserved(t *testing.T) {
	yaml := `
ollama_url: "http://other:11434"
`
	path := writeTemp(t, yaml)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Overridden
	if cfg.OllamaURL != "http://other:11434" {
		t.Errorf("OllamaURL = %q", cfg.OllamaURL)
	}
	// Preserved defaults
	if cfg.ListenAddr != ":9400" {
		t.Errorf("ListenAddr = %q, want default :9400", cfg.ListenAddr)
	}
	if !cfg.Proxy.Enabled {
		t.Error("Proxy.Enabled should still be true (default)")
	}
	if len(cfg.Proxy.ExcludePaths) == 0 {
		t.Error("ExcludePaths should retain defaults when not specified")
	}
}

func TestLoad_EnvVarWinsOverYAML(t *testing.T) {
	yaml := `ollama_url: "http://fromfile:11434"`
	path := writeTemp(t, yaml)

	t.Setenv("OLLAMA_URL", "http://fromenv:11434")

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.OllamaURL != "http://fromenv:11434" {
		t.Errorf("OllamaURL = %q, env var should win", cfg.OllamaURL)
	}
}

func TestLoad_InvalidDuration(t *testing.T) {
	path := writeTemp(t, `poll_interval: "not-a-duration"`)
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected error for invalid duration")
	}
}

func TestLoad_InvalidBool(t *testing.T) {
	path := writeTemp(t, "proxy:\n  enabled: notabool\n")
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected error for invalid bool")
	}
}

func TestLoad_Comments_Ignored(t *testing.T) {
	yaml := `
# full-line comment
ollama_url: "http://localhost:11434" # inline comment
listen_addr: ":9400"
# poll_interval: "999s"
`
	path := writeTemp(t, yaml)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.PollInterval != 15*time.Second {
		t.Errorf("commented-out poll_interval should not be applied, got %v", cfg.PollInterval)
	}
}

func TestLoad_GPUPollInterval(t *testing.T) {
	yaml := `
gpu:
  enabled: true
  poll_interval: "5s"
`
	path := writeTemp(t, yaml)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.GPU.PollInterval != 5*time.Second {
		t.Errorf("GPU.PollInterval = %v, want 5s", cfg.GPU.PollInterval)
	}
}
