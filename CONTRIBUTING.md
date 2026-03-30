# Contributing to ollama-exporter

## Requirements

- Go 1.22+
- Docker (optional, for container builds)
- golangci-lint (installed automatically via make lint)

## Local Development

Clone and build:

    git clone https://github.com/maravexa/ollama-exporter
    cd ollama-exporter
    make build

Run against local Ollama:

    make run

Run the full check suite (matches CI):

    make all

## Proxy Mode Testing

Point your Ollama client at the proxy port instead of Ollama directly:

    OLLAMA_HOST=http://localhost:9401 ollama run llama3.1:8b "hello"

Then inspect metrics:

    curl -s http://localhost:9400/metrics | grep ollama_

## Adding Metrics

1. Define the metric in internal/metrics/registry.go
2. Register it in metrics.New()
3. Record observations in the appropriate collector:
   - internal/collector/poller.go for model state metrics
   - internal/collector/proxy.go for per-request metrics
4. Document it in docs/metrics.md
5. Add a unit test for any non-trivial derivation math

## Submitting Changes

- One logical change per PR
- make all must pass before submitting
- Update docs/metrics.md if adding or changing metrics
- Update CHANGELOG.md under [Unreleased]

## Commit Style

    feat: add kv cache pressure metric
    fix: correct quant label parsing for untagged models
    docs: add grafana dashboard setup instructions
    chore: bump golangci-lint to v1.58
