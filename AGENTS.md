# ollama-exporter

## Project Purpose
A production-grade Go Prometheus exporter for Ollama LLM inference.
Exposes per-request inference metrics, model lifecycle events, and
derived AI-specific signals (TPS, KV cache pressure inference,
quantization-aware labeling) for the local observability stack.

## Architecture
Polling and proxy collection can operate together. Proxy, GPU, metrics-listener and remote-write behavior depend on configuration; consult `internal/config/config.go` and the startup code rather than assuming every feature is enabled.

### Poller (always-on baseline)
- Scrapes `/api/ps` and `/api/tags` on configurable interval
- Provides model inventory, VRAM usage, load/unload event tracking
- Keep poller collection independent of proxy request failures; do not claim availability when the upstream or exporter is down.

### Proxy (per-request instrumentation)
- Transparent HTTP reverse proxy in front of Ollama
- Intercepts request/response pairs, extracts timing fields from JSON body
- Derives: tokens-per-second, prompt eval rate, load duration
- Check `internal/collector/proxy.go`, `responsebody.go`, `streamjson.go` and `usage.go` for native/compatibility streaming behavior. Performance and failover guarantees require measured runtime evidence.

## Metric Design Principles
- All metrics namespaced: `ollama_`
- Quantization parsed from model tag into discrete label: `quant="q4_0"`
- Model family parsed into label: `family="llama3"`
- Histograms for latency, not summaries (Prometheus best practice)
- Preserve metric update semantics in `internal/collector/` and `internal/metrics/registry.go`; request-derived values and scrape-time collection are distinct.
- No cardinality explosions — prompt text never appears in labels

## Metric reference

Use the README metric tables and `internal/metrics/registry.go` for names and semantics. Preserve the distinction between native inference counters and compatibility request usage; prompt text must never become a metric label.

Native completed requests additionally accumulate `ollama_prompt_tokens_total`
and `ollama_eval_tokens_total`. Missing or zero native counts do not increment
these counters. Compatibility usage remains separately represented; never merge
missing and explicit-zero usage or infer native decode speed from proxy wall time.
See `internal/collector/native_tokens_test.go`, `proxy_request_lifecycle_test.go`
and `docs/metrics.md` when modifying terminal-response accounting.

## Go Conventions
- Use the Go version declared in `go.mod` (currently 1.26.10).
- Module path: `github.com/maravexa/ollama-exporter`
- Keep dependencies minimal. `go.mod` is authoritative; the project also uses Prometheus remote-write dependencies and snappy. Do not introduce unnecessary libraries.
- No global state — pass dependencies explicitly
- Errors returned, never panicked in library code
- All exported types have godoc comments
- Structured logging via `log/slog` (stdlib, Go 1.21+)

## Code map

- `cmd/exporter/main.go`: composition and server startup.
- `internal/collector/`: polling, proxy, response parsing and usage accounting.
- `internal/metrics/registry.go`: metric definitions.
- `internal/ollama/`: upstream client and response types.
- `internal/config/`: configuration loading and validation.
- `internal/gpu/`: build-tagged Linux AMD sysfs and macOS Apple Silicon ioreg backends; shared descriptors in `gpu.go`. Apple observations are whole-GPU utilization/unified memory, not per-model inference-only values; unsupported metrics remain absent rather than zero.
- `internal/remotewrite/`: queue, conversion and remote-write delivery.
- `README.md`, `Makefile`, `.github/workflows/`: supported usage, local commands and CI.

## Testing Strategy
- Unit tests for: metric derivation math, model name parsing, config validation
- Integration test: spin up mock Ollama HTTP server, assert metric output
- No test file imports production dependencies not in go.mod
- Run `go test -race ./...`; focus with `-run` and the affected package first. `make test` also writes coverage.out.
- `make build` writes ./ollama-exporter; `make fmt` checks formatting without rewriting; `make vet` and `make lint` run their respective checks.
- `make ci` includes vulnerability checking via `go run ...@latest`, which may download tools and access the network; it does not include the build target. Use it only when those prerequisites/actions are appropriate.

The checked-in workflow also runs `scripts/vulnerability_gate.py` with a pinned
scanner, production-root scans and bounded, hash-checked exceptions. The Makefile's
`@latest` scan is not equivalent to that gate. Do not refresh exception hashes or
expiry dates just to make the check pass; changed exposure needs review. Its scan
entry point installs the scanner and contacts the vulnerability database. These
commands are documented checks, not permission to run hosted CI or network scans.
Grafana dashboard contracts live in `deploy/grafana/dashboard_test.go`; dashboard
queries must preserve inventory/lifecycle meanings and missing-series behavior.

## Security requirements and build conventions
- No CGo — pure Go, statically linkable
- Keep the network and dependency surface minimal; inspect the actual dependency set in go.mod.
- No eval, no reflection-based config, no dynamic code loading
- Preserve non-root container execution; `Dockerfile` is authoritative for the base image and user identity.

## Author Context
This exporter is part of a broader AI safety observability stack:
- Feeds PLG (Prometheus/Loki/Grafana) stack on cyberdeck.vexa.heim
- Complements garak-axis (LLM vulnerability scanning + alignment monitoring)
- Target: expose inference signals correlated with behavioral probe results
- Long-term: quantization/performance data feeds into local AI safety research
