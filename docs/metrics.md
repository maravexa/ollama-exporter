# Metric Reference

All metrics are prefixed `ollama_`.

## Labels

| Label | Description | Example |
|---|---|---|
| `model` | Full Ollama model tag | `llama3.1:8b-q4_0` |
| `family` | Parsed model family | `llama3` |
| `quant` | Parsed quantization level | `q4_0` |
| `endpoint` | API path | `/api/generate` |

## Health

### `ollama_up` (Gauge)
1 if Ollama API is reachable, 0 otherwise. Updated every poll interval.

## Model Lifecycle

### `ollama_model_loaded` (Gauge)
1 if model is resident in VRAM. Set to 0 on eviction.

### `ollama_model_vram_bytes` (Gauge)
VRAM consumed by loaded model. Source: /api/ps `size_vram`.

### `ollama_model_load_total` (Counter)
Incremented when a model appears in /api/ps that was absent in the previous scrape.

### `ollama_model_unload_total` (Counter)
Incremented when a model disappears from /api/ps (keep_alive eviction or explicit unload).

## Per-Request Inference (Proxy Mode)

### `ollama_request_duration_seconds` (Histogram)
Wall-clock latency from request receipt to response completion. Includes model load time.

### `ollama_load_duration_seconds` (Histogram)
Time spent loading the model for this request. Near-zero if model was already in VRAM.

### `ollama_prompt_eval_duration_seconds` (Histogram)
Time spent in the prefill phase (processing the input prompt).

### `ollama_eval_duration_seconds` (Histogram)
Time spent in the decode phase (generating output tokens).

## Derived Metrics

### `ollama_tokens_per_second` (Gauge)
Decode throughput: `eval_count / eval_duration`. Primary inference performance indicator.

### `ollama_prompt_tokens_per_second` (Gauge)
Prefill throughput: `prompt_eval_count / prompt_eval_duration`.

### `ollama_kv_cache_pressure_ratio` (Gauge)
`prompt_eval_duration_ns / prompt_eval_count`. Rising values indicate degraded KV cache
efficiency — useful for detecting context length pressure before explicit OOM events.

## Concurrency

### `ollama_requests_in_flight` (Gauge)
Current number of requests being proxied. Correlate with latency to detect contention.

### `ollama_requests_total` (Counter)
Cumulative request count by model, family, quant, and endpoint.
