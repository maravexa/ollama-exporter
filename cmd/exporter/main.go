// Package main is the entry point for the ollama-exporter binary.
// It wires together configuration, collectors, and the HTTP server.
package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/maravexa/ollama-exporter/internal/collector"
	"github.com/maravexa/ollama-exporter/internal/config"
	"github.com/maravexa/ollama-exporter/internal/remotewrite"
)

// Version is set at build time via ldflags: -X main.Version=<version>
var Version = "dev"

func main() {
	configPath := flag.String("config", "/etc/ollama-exporter/ollama-exporter.yml", "path to config file")
	flag.Parse()

	cfg, err := config.Load(*configPath)
	if err != nil {
		slog.Error("failed to load config", "err", err)
		os.Exit(1)
	}

	level := slog.LevelInfo
	switch strings.ToLower(cfg.LogLevel) {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: level})))
	slog.Info("starting ollama-exporter", "version", Version)

	remotewrite.SetUserAgentVersion(Version)

	reg := prometheus.NewRegistry()

	col, err := collector.New(cfg, reg)
	if err != nil {
		slog.Error("failed to initialize collector", "err", err)
		os.Exit(1)
	}

	pusher, err := buildPusher(cfg, reg)
	if err != nil {
		slog.Error("failed to initialize remote write", "err", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	go col.Start(ctx)

	var wg sync.WaitGroup

	var srv *http.Server
	if cfg.MetricsEndpoint.Enabled {
		mux := http.NewServeMux()
		mux.Handle("/metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{}))
		mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		})
		srv = &http.Server{
			Addr:              cfg.MetricsEndpoint.ListenAddress,
			Handler:           mux,
			ReadHeaderTimeout: 10 * time.Second,
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			slog.Info("metrics server listening", "addr", cfg.MetricsEndpoint.ListenAddress)
			if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				slog.Error("metrics server error", "err", err)
			}
		}()
	} else {
		slog.Info("local /metrics endpoint disabled; running in push-only mode")
	}

	if pusher != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := pusher.Run(ctx); err != nil {
				slog.Error("remote write pusher exited with error", "err", err)
			}
		}()
	}

	<-ctx.Done()
	slog.Info("shutting down")

	if srv != nil {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		if err := srv.Shutdown(shutdownCtx); err != nil {
			slog.Error("server shutdown", "err", err)
		}
		cancel()
	}

	wg.Wait()
}

// buildPusher constructs a remote write Pusher from cfg, applying the
// startup warnings called out in the design (multi-endpoint, insecure
// TLS, plaintext HTTP). Returns (nil, nil) when no endpoints are
// configured.
//
// reg is typed as *prometheus.Registry because the build path needs both
// the Registerer (for self-metrics) and the Gatherer (as the source of
// pushed samples) facets of the same object.
func buildPusher(cfg *config.Config, reg *prometheus.Registry) (*remotewrite.Pusher, error) {
	if len(cfg.RemoteWrite) == 0 {
		return nil, nil
	}

	if len(cfg.RemoteWrite) > 1 {
		slog.Warn("v0.4.0 supports a single remote_write endpoint; using first, ignoring rest. Multi-endpoint planned for v0.5.",
			"using", cfg.RemoteWrite[0].Name,
			"ignored_count", len(cfg.RemoteWrite)-1,
		)
	}

	rw := cfg.RemoteWrite[0]
	if rw.TLS.InsecureSkipVerify {
		slog.Warn("TLS verification disabled — communications with this endpoint are not authenticated", "endpoint", rw.URL)
	}
	if rw.InsecureHTTP {
		slog.Warn("plaintext HTTP transport enabled — credentials and metrics travel unencrypted", "endpoint", rw.URL)
	}

	specs := []remotewrite.EndpointBuildSpec{toBuildSpec(rw)}
	return remotewrite.Build(remotewrite.BuildOptions{
		Logger:        slog.Default(),
		Gatherer:      reg,
		Registerer:    reg,
		Endpoints:     specs,
		FlushInterval: rw.FlushInterval,
		DrainTimeout:  5 * time.Second,
	})
}

func toBuildSpec(rw config.RemoteWriteConfig) remotewrite.EndpointBuildSpec {
	spec := remotewrite.EndpointBuildSpec{
		Name:              rw.Name,
		URL:               rw.URL,
		Timeout:           rw.Timeout,
		TLSCAFile:         rw.TLS.CAFile,
		TLSInsecureVerify: rw.TLS.InsecureSkipVerify,
		InsecureHTTP:      rw.InsecureHTTP,
		BearerTokenFile:   rw.BearerTokenFile,
		Headers:           rw.Headers,
		ExternalLabels:    rw.ExternalLabels,
		Retry: remotewrite.RetryBuildSpec{
			MaxAttempts:    rw.Retry.MaxAttempts,
			MaxElapsed:     rw.Retry.MaxElapsed,
			InitialBackoff: rw.Retry.InitialBackoff,
			MaxBackoff:     rw.Retry.MaxBackoff,
		},
		Breaker: remotewrite.BreakerBuildSpec{
			FailureThreshold: rw.CircuitBreaker.FailureThreshold,
			Window:           rw.CircuitBreaker.Window,
			Cooldown:         rw.CircuitBreaker.Cooldown,
		},
		Queue: remotewrite.QueueBuildSpec{
			Capacity: rw.Queue.Capacity,
		},
	}
	if rw.BasicAuth != nil {
		spec.BasicAuthUser = rw.BasicAuth.Username
		spec.BasicAuthPassFile = rw.BasicAuth.PasswordFile
	}
	return spec
}
