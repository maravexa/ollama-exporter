# Changelog

All notable changes to ollama-exporter are documented here.
Format follows Keep a Changelog (https://keepachangelog.com/en/1.0.0/).
Versioning follows Semantic Versioning (https://semver.org/).

## [Unreleased]

## [0.1.0] - 2026-03-30

### Added
- Poller: scrapes /api/ps and /api/tags on configurable interval
- Poller: model load/unload event tracking via state diffing
- Proxy: transparent reverse proxy with per-request metric extraction
- Proxy: NDJSON streaming support — buffers chunks, extracts final done=true chunk
- Derived metrics: tokens_per_second, prompt_tokens_per_second, kv_cache_pressure_ratio
- Model cache: calls /api/show per model, caches quant and family labels
- Quantization-aware labeling: q4_k_m, q8_0 etc as discrete Prometheus labels
- Graceful shutdown on SIGINT/SIGTERM
- Distroless Docker image, runs as nonroot
- docker-compose with Prometheus and Grafana
