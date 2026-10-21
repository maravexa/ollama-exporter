# CLAUDE.md — ollama-exporter

## Project Purpose
A production-grade Go Prometheus exporter for Ollama LLM inference.
Exposes per-request inference metrics, model lifecycle events, and
derived AI-specific signals (TPS, KV cache pressure inference,
quantization-aware labeling) that no existing exporter provides.

## Architecture
Two collection modes, both active simultaneously:

### Poller (always-on baseline)
- Scrapes `/api/ps` and `/api/tags` on configurable interval
- Provides model inventory, VRAM usage, load/unload event tracking
- Survives proxy failures — Prometheus always gets model state

### Proxy (per-request instrumentation)
- Transparent HTTP reverse proxy in front of Ollama
- Intercepts request/response pairs, extracts timing fields from JSON body
- Derives: tokens-per-second, prompt eval rate, load duration
- Adds microsecond latency; has health-check-based degradation to poller-only mode

## Metric Design Principles
- All metrics namespaced: `ollama_`
- Quantization parsed from model tag into discrete label: `quant="q4_0"`
- Model family parsed into label: `family="llama3"`
- Histograms for latency, not summaries (Prometheus best practice)
- Derived metrics computed at scrape time, not stored
- No cardinality explosions — prompt text never appears in labels

## Key Metrics (Target)
| Metric | Type | Description |
|---|---|---|
| `ollama_up` | Gauge | Ollama API health |
| `ollama_model_loaded` | Gauge | 1 if model currently in VRAM |
| `ollama_model_vram_bytes` | Gauge | VRAM consumed per model |
| `ollama_model_load_total` | Counter | Load events per model |
| `ollama_model_unload_total` | Counter | Unload/eviction events per model |
| `ollama_request_duration_seconds` | Histogram | End-to-end request latency |
| `ollama_load_duration_seconds` | Histogram | Model load time per request |
| `ollama_prompt_eval_duration_seconds` | Histogram | Prompt evaluation time |
| `ollama_eval_duration_seconds` | Histogram | Token generation time |
| `ollama_tokens_per_second` | Gauge | Derived: eval_count/eval_duration |
| `ollama_prompt_tokens_per_second` | Gauge | Derived: prompt_eval_count/prompt_eval_duration |
| `ollama_requests_in_flight` | Gauge | Current concurrent requests |
| `ollama_requests_total` | Counter | Total requests by model and endpoint |
| `ollama_kv_cache_pressure_ratio` | Gauge | Derived: prompt_eval_duration/prompt_eval_count trend |

## Go Conventions
- Minimum Go version: 1.22
- Module path: `github.com/maravexa/ollama-exporter`
- Dependencies: ONLY `github.com/prometheus/client_golang` and stdlib
- No global state — pass dependencies explicitly
- Errors returned, never panicked in library code
- All exported types have godoc comments
- Structured logging via `log/slog` (stdlib, Go 1.21+)

## File Layout
```
ollama-exporter/
├── CLAUDE.md
├── README.md
├── LICENSE
├── Dockerfile
├── docker-compose.yml
├── ollama-exporter.yml
├── ollama-exporter.yml.example
├── go.mod
├── go.sum
├── cmd/
│   └── exporter/
│       └── main.go
├── internal/
│   ├── collector/
│   │   ├── collector.go        # Collector interface + registration
│   │   ├── poller.go           # /api/ps and /api/tags polling
│   │   └── proxy.go            # Reverse proxy + metric extraction
│   ├── metrics/
│   │   └── registry.go         # All metric definitions, no logic
│   ├── ollama/
│   │   ├── client.go           # Typed HTTP client for Ollama API
│   │   └── types.go            # API response structs
│   └── config/
│       └── config.go           # Config loading and validation
├── deploy/
│   ├── prometheus/
│   │   └── ollama-exporter.yml # Scrape config snippet
│   └── grafana/
│       └── dashboard.json      # Starter Grafana dashboard
└── docs/
    └── metrics.md              # Full metric reference
```

## Testing Strategy
- Unit tests for: metric derivation math, model name parsing, config validation
- Integration test: spin up mock Ollama HTTP server, assert metric output
- No test file imports production dependencies not in go.mod
- Run with: `go test ./...`

## Security Properties
- No CGo — pure Go, statically linkable
- Minimal attack surface: stdlib net/http + prometheus client only
- No eval, no reflection-based config, no dynamic code loading
- Dockerfile uses distroless/static base image
- Runs as non-root user (uid 65534)
