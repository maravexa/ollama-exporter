# Changelog

All notable changes to ollama-exporter are documented here.
Format follows Keep a Changelog (https://keepachangelog.com/en/1.0.0/).
Versioning follows Semantic Versioning (https://semver.org/).

## [Unreleased]

## [0.1.2] - 2026-04-21

Consolidates all unreleased changes from the 0.2.0 and 0.3.x development cycle;
those tags were never successfully published (GoReleaser config errors) and the
version number is being rewound to match the project's actual maturity level.

### Added
- `.deb` and `.rpm` installer packages produced automatically on every `v*` tag
  push via GoReleaser nfpms. Both `linux/amd64` and `linux/arm64` are built.
- Systemd service unit installs to `/usr/lib/systemd/system/ollama-exporter.service`;
  the post-install script creates a dedicated `ollama-exporter` system user/group
  and enables the service automatically.
- `/etc/ollama-exporter/ollama-exporter.yml` as the canonical installed config path,
  marked `type: config` in the package so upgrades never overwrite user edits.
- AMD GPU metrics via sysfs: utilization, temperature, VRAM usage, power draw,
  and clock speeds (`ollama_gpu_utilization_percent`, `ollama_gpu_temperature_celsius`,
  `ollama_gpu_vram_used_bytes`, `ollama_gpu_vram_total_bytes`, `ollama_gpu_power_watts`,
  `ollama_gpu_clock_mhz`). Works with the amdgpu kernel driver — no ROCm userspace
  dependency required. Gracefully disabled when no AMD GPUs are found.
- `gpu` config section with `enabled`, `poll_interval`, and `sysfs_base` fields.
- `proxy.exclude_paths` config field: a user-configurable list of paths that bypass metric
  recording while still being forwarded upstream. Defaults to the five internal paths above.
- Model load/unload lifecycle metrics: `ollama_model_load_events_total`,
  `ollama_model_unload_events_total`, `ollama_model_loaded` gauge,
  `ollama_model_vram_bytes`, and `ollama_model_load_duration_seconds` histogram.
  Load/unload events are only counted for transitions observed after startup —
  models present at first scrape are treated as already loaded and do not fire counters.

### Fixed
- Config YAML file parsing was never executed (TODO stub). All config fields —
  including nested `proxy` and `gpu` sections and the `exclude_paths` list — are
  now parsed from the YAML file using a stdlib-only parser.
- `--config` default changed from relative `config.yaml` to absolute
  `/etc/ollama-exporter/ollama-exporter.yml`, matching the installed path.
- `log_level` config field is now applied to the slog handler at startup.
- Filtered internal proxy calls (`/api/ps`, `/api/tags`, `/api/show`, `/api/version`, `/`)
  from `ollama_request_duration_seconds` and related per-request metrics to reduce label
  cardinality noise; these paths still proxy through to Ollama normally.
- VRAM gauge (`ollama_model_vram_bytes`) now resets to 0 on model unload instead of
  retaining the last observed value.
- GoReleaser config: migrated deprecated v1 fields (`archives.builds`→`ids`,
  `archives.format`→`formats`, `nfpms.builds`→`ids`) and removed `release.extra_files`
  glob for nfpm artifacts (caused duplicate-artifact 422 errors on GitHub release upload).

### Changed
- Config file renamed from `config.yaml` to `ollama-exporter.yml`.

## [0.1.1] - 2026-03-30

### Fixed
- Proxy: non-streaming responses (/api/chat stream:false) dropped connection
  due to Content-Length mismatch and chunked Transfer-Encoding bleed-through
- Proxy: bufio.Scanner replaced with io.ReadAll for response body handling

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
