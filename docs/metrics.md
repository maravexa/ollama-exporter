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
Wall-clock proxy handling time from handler entry through response forwarding
or abort, including request-prefix inspection. Includes model load time; excludes metadata lookup. Recorded
exactly once even when ReverseProxy aborts after an upstream read failure.

### `ollama_load_duration_seconds` (Histogram)
Time spent loading the model for this request. Near-zero if model was already in VRAM.

### `ollama_prompt_eval_duration_seconds` (Histogram)
Time spent in the prefill phase (processing the input prompt).

### `ollama_eval_duration_seconds` (Histogram)
Time spent in the decode phase (generating output tokens).

## Derived Metrics

### `ollama_tokens_per_second` (Gauge)
Native decode throughput: `eval_count / (eval_duration_ns / 1e9)`. Compatibility
routes do not populate this metric.

### `ollama_prompt_tokens_per_second` (Gauge)
Prefill throughput: `prompt_eval_count / prompt_eval_duration`.

### `ollama_kv_cache_pressure_ratio` (Gauge)
`prompt_eval_duration_ns / prompt_eval_count`. Rising values indicate degraded KV cache
efficiency — useful for detecting context length pressure before explicit OOM events.

## Concurrency

### `ollama_requests_in_flight` (Gauge)
Current number of requests being proxied. Correlate with latency to detect contention.

### `ollama_requests_total` (Counter)
Completed native (`done:true`) and compatibility inferences by model, family,
quant, and endpoint. Requires clean upstream EOF and, for SSE, `[DONE]` or
`message_stop`. Compatibility completions count even when usage is absent.
Failed, truncated, cancelled, non-2xx, and excluded requests do not increment it.

## Compatibility Usage (Proxy Mode)

POST `/v1/chat/completions`, `/v1/completions`, and `/v1/messages` support JSON
and SSE usage extraction. `/v1/messages/count_tokens` is excluded from inference
accounting. Labels are `model`, `family`, `quant`, and `endpoint`.

### `ollama_compatibility_input_tokens` (Histogram)
Reported primary prompt/input tokens per completed compatibility request: OpenAI
`usage.prompt_tokens`, or Anthropic `usage.input_tokens` (preferring final usage
over provisional `message_start` usage). Separate `cache_read_input_tokens` and
`cache_creation_input_tokens` are not added or exported, so this is an API-specific
raw count rather than a normalized total across APIs. In the Ollama 0.40.3 live
run, Anthropic reported 5 primary input + 15 cache-read tokens where OpenAI reported
20 prompt tokens. See [the live results](live-validation.txt).

### `ollama_compatibility_output_tokens` (Histogram)
Reported completion/output tokens per completed compatibility request.

For each histogram, `_sum` accumulates reported tokens and `_count` counts
requests reporting that field. Missing/null usage produces no observation;
explicit zero produces a zero observation. Stream usage availability varies by
Ollama version and route. OpenAI clients can request `stream_options.include_usage`
when supported. The exporter never modifies upstream requests to obtain usage.
Anthropic streams combine input usage from `message_start` with final output
usage from `message_delta`, awaiting `message_stop` and EOF before observing.

These metrics have no native duration equivalent. Use `rate(..._sum[5m])` for
aggregate known token throughput across requests over a scrape window; neither
that value nor proxy wall time measures per-request native decode performance.
Native phase durations, TPS, and KV cache pressure remain native-only. See the
[README](../README.md#compatibility-routes-and-usage) for parsing limits and
request model lookup behavior.


## Extraction diagnostics (Proxy Mode)

`ollama_proxy_extraction_total{outcome,reason}` counts exactly one terminal result
per eligible POST inference response. Excluded paths and unrelated routes/methods
are not observed. Labels contain only fixed strings; no model, path, upstream
error text, generated content, or arbitrary field name is included.

| Outcome | Reason | Meaning |
|---|---|---|
| `success` | `none` | Native terminal metrics or at least one valid compatibility usage count recorded; explicit zero is valid |
| `skipped` | `usage_absent` | Completed compatibility inference with all counts missing/null; completion still counted |
| `skipped` | `incomplete` | No complete inference/terminal marker in an otherwise valid response |
| `skipped` | `body_closed` | Observer closed before reaching EOF; no completion synthesized |
| `skipped` | `http_status` | Non-2xx inference response forwarded without extraction |
| `failed` | `depth_limit` | JSON nesting exceeds 64 containers, including the root |
| `failed` | `selected_value_limit` | A selected scalar exceeds 4096 encoded bytes |
| `failed` | `malformed` | Invalid JSON or SSE data framing |
| `failed` | `incomplete` | EOF inside JSON or an SSE JSON event |
| `failed` | `invalid_fields` | Selected native field types or reported compatibility counts are invalid |
| `failed` | `upstream_error` | A response object reports an error |
| `failed` | `read_error` | Upstream body read ended with an error other than EOF |

`ollama_proxy_extraction_field_skips_total{reason="field_name_limit"}` separately
counts responses with one or more ignored field names beyond 128 encoded bytes,
once per response even across multiple SSE events. This may accompany successful
extraction of other fields. Both string limits include JSON quotes and escapes;
they count encoded bytes rather than decoded characters. Exactly 64 levels,
4096-byte selected scalars, and 128-byte field names remain supported. Oversized
ignored strings/numbers/arrays have no total size limit. Exceeding a parser limit
never changes the client response. These counters do not log response contents.
Invalid reported compatibility counts now reject completion/token observations
rather than silently treating invalid values as absent usage.

### Deleted models

On each successful `/api/ps` and `/api/tags` inventory poll, models absent from
both snapshots lose their cached metadata and current-state series:
`ollama_model_loaded`, `ollama_model_vram_bytes`, `ollama_tokens_per_second`,
`ollama_prompt_tokens_per_second`, and `ollama_kv_cache_pressure_ratio`.
An installed but unloaded model retains its zero loaded/VRAM gauges. A model
still running remains visible even if the tag snapshot does not list it.
Failed or missing inventory data never triggers removal. Explicit empty model
arrays are valid empty inventories. A concurrent inference completion can briefly
reintroduce a gauge; the next successful inventory poll removes it again.

Cumulative counters, histograms, and request-in-flight bookkeeping are preserved:
model deletion does not erase request history or reset totals. These series can
still name deleted models, and Prometheus retains previously scraped history.
To display current model state, use instant queries rather than reducing a
historical range with `lastNotNull`. Metadata for a deleted and later recreated
model is fetched again on its next use.
