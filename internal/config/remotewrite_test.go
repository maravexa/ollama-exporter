package config

import (
	"strings"
	"testing"
	"time"
)

const validRemoteWriteYAML = `
remote_write:
  - url: "https://mimir.example.com/api/v1/push"
    name: primary
    flush_interval: "10s"
    timeout: "30s"
    queue:
      capacity: 5000
    retry:
      max_attempts: 5
      max_elapsed: "5m"
      initial_backoff: "1s"
      max_backoff: "30s"
    circuit_breaker:
      failure_threshold: 5
      window: "1m"
      cooldown: "30s"
    tls:
      insecure_skip_verify: false
      ca_file: "/etc/ssl/ca.pem"
    basic_auth:
      username: tenant1
      password_file: "/run/credentials/exporter/mimir_password"
    headers:
      X-Scope-OrgID: tenant1
    external_labels:
      cluster: xena
      env: homelab
`

func TestLoad_RemoteWrite_Valid(t *testing.T) {
	path := writeTemp(t, validRemoteWriteYAML)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.RemoteWrite) != 1 {
		t.Fatalf("len(RemoteWrite) = %d, want 1", len(cfg.RemoteWrite))
	}
	rw := cfg.RemoteWrite[0]
	if rw.URL != "https://mimir.example.com/api/v1/push" {
		t.Errorf("URL = %q", rw.URL)
	}
	if rw.Name != "primary" {
		t.Errorf("Name = %q", rw.Name)
	}
	if rw.FlushInterval != 10*time.Second {
		t.Errorf("FlushInterval = %v", rw.FlushInterval)
	}
	if rw.Queue.Capacity != 5000 {
		t.Errorf("Queue.Capacity = %d", rw.Queue.Capacity)
	}
	if rw.Retry.MaxAttempts != 5 || rw.Retry.MaxElapsed != 5*time.Minute {
		t.Errorf("Retry = %+v", rw.Retry)
	}
	if rw.CircuitBreaker.FailureThreshold != 5 {
		t.Errorf("CircuitBreaker.FailureThreshold = %d", rw.CircuitBreaker.FailureThreshold)
	}
	if rw.TLS.CAFile != "/etc/ssl/ca.pem" {
		t.Errorf("TLS.CAFile = %q", rw.TLS.CAFile)
	}
	if rw.BasicAuth == nil || rw.BasicAuth.Username != "tenant1" {
		t.Errorf("BasicAuth = %+v", rw.BasicAuth)
	}
	if rw.BasicAuth.PasswordFile != "/run/credentials/exporter/mimir_password" {
		t.Errorf("PasswordFile = %q", rw.BasicAuth.PasswordFile)
	}
	if rw.Headers["X-Scope-OrgID"] != "tenant1" {
		t.Errorf("Headers = %v", rw.Headers)
	}
	if rw.ExternalLabels["cluster"] != "xena" || rw.ExternalLabels["env"] != "homelab" {
		t.Errorf("ExternalLabels = %v", rw.ExternalLabels)
	}
}

func TestLoad_RemoteWrite_RejectsPlaintextPassword(t *testing.T) {
	yaml := `
remote_write:
  - url: "https://x.example.com/push"
    basic_auth:
      username: u
      password: leaked
`
	path := writeTemp(t, yaml)
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected error for plaintext password")
	}
	if !strings.Contains(err.Error(), "remote_write[0].basic_auth.password") {
		t.Errorf("error should name the field path, got: %v", err)
	}
	if !strings.Contains(err.Error(), "plaintext credentials forbidden") {
		t.Errorf("error should mention plaintext credentials, got: %v", err)
	}
}

func TestLoad_RemoteWrite_RejectsPlaintextBearerToken(t *testing.T) {
	yaml := `
remote_write:
  - url: "https://x.example.com/push"
    bearer_token: leaked
`
	path := writeTemp(t, yaml)
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected error for plaintext bearer_token")
	}
	if !strings.Contains(err.Error(), "remote_write[0].bearer_token") {
		t.Errorf("error should name the field path, got: %v", err)
	}
}

func TestLoad_RemoteWrite_RejectsHTTPWithoutInsecureFlag(t *testing.T) {
	yaml := `
remote_write:
  - url: "http://x.example.com/push"
`
	path := writeTemp(t, yaml)
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected error for plaintext HTTP")
	}
	if !strings.Contains(err.Error(), "insecure_http") {
		t.Errorf("error should mention insecure_http, got: %v", err)
	}
}

func TestLoad_RemoteWrite_AllowsHTTPWithInsecureFlag(t *testing.T) {
	yaml := `
remote_write:
  - url: "http://x.example.com/push"
    insecure_http: true
`
	path := writeTemp(t, yaml)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.RemoteWrite[0].InsecureHTTP {
		t.Error("InsecureHTTP should be true")
	}
}

func TestLoad_RemoteWrite_BasicAuthAndBearerMutuallyExclusive(t *testing.T) {
	yaml := `
remote_write:
  - url: "https://x.example.com/push"
    bearer_token_file: "/etc/token"
    basic_auth:
      username: u
      password_file: "/etc/pw"
`
	path := writeTemp(t, yaml)
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected mutual exclusion error")
	}
	if !strings.Contains(err.Error(), "mutually exclusive") {
		t.Errorf("error should mention mutual exclusion, got: %v", err)
	}
}

func TestLoad_RemoteWrite_RejectsReservedHeaders(t *testing.T) {
	yaml := `
remote_write:
  - url: "https://x.example.com/push"
    headers:
      Authorization: "Basic xxx"
`
	path := writeTemp(t, yaml)
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected reserved-header error")
	}
	if !strings.Contains(err.Error(), "Authorization") {
		t.Errorf("error should name Authorization, got: %v", err)
	}
}

func TestLoad_MetricsEndpointDisabledNoRemoteWriteIsError(t *testing.T) {
	yaml := `
metrics_endpoint:
  enabled: false
`
	path := writeTemp(t, yaml)
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected no-op exporter error")
	}
	if !strings.Contains(err.Error(), "no-op") {
		t.Errorf("error should call out no-op, got: %v", err)
	}
}

func TestLoad_MetricsEndpointDisabledWithRemoteWriteIsValid(t *testing.T) {
	path := writeTemp(t, `
metrics_endpoint:
  enabled: false
`+validRemoteWriteYAML)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.MetricsEndpoint.Enabled {
		t.Error("MetricsEndpoint.Enabled should be false")
	}
}

func TestLoad_MetricsEndpoint_ListenAddressOverride(t *testing.T) {
	yaml := `
metrics_endpoint:
  enabled: true
  listen_address: ":9404"
` + validRemoteWriteYAML
	path := writeTemp(t, yaml)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.MetricsEndpoint.ListenAddress != ":9404" {
		t.Errorf("ListenAddress = %q", cfg.MetricsEndpoint.ListenAddress)
	}
	if cfg.ListenAddr != ":9404" {
		t.Errorf("ListenAddr legacy mirror = %q", cfg.ListenAddr)
	}
}

func TestLoad_RemoteWrite_NegativeDurationRejected(t *testing.T) {
	yaml := `
remote_write:
  - url: "https://x.example.com/push"
    flush_interval: "-1s"
`
	path := writeTemp(t, yaml)
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected error for negative flush_interval")
	}
}

func TestLoad_RemoteWrite_MissingURLRejected(t *testing.T) {
	yaml := `
remote_write:
  - name: missing-url
    flush_interval: "10s"
`
	path := writeTemp(t, yaml)
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected error for missing url")
	}
	if !strings.Contains(err.Error(), "url") {
		t.Errorf("error should mention url, got: %v", err)
	}
}

func TestLoad_RemoteWrite_DefaultsWhenAbsent(t *testing.T) {
	cfg, err := Load("/nonexistent/path/to/config.yml")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.RemoteWrite) != 0 {
		t.Errorf("RemoteWrite should default to empty, got %v", cfg.RemoteWrite)
	}
	if !cfg.MetricsEndpoint.Enabled {
		t.Error("MetricsEndpoint.Enabled should default to true")
	}
}

func TestLoad_RemoteWrite_MultipleEndpointsParse(t *testing.T) {
	yaml := `
remote_write:
  - url: "https://a.example.com/push"
    name: a
  - url: "https://b.example.com/push"
    name: b
`
	path := writeTemp(t, yaml)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.RemoteWrite) != 2 {
		t.Fatalf("len = %d, want 2", len(cfg.RemoteWrite))
	}
	if cfg.RemoteWrite[0].Name != "a" || cfg.RemoteWrite[1].Name != "b" {
		t.Errorf("entries = %+v", cfg.RemoteWrite)
	}
}
