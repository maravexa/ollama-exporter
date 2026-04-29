# Remote Write (Push Mode)

`ollama-exporter` v0.4.0 ships a built-in **Prometheus Remote Write 1.0**
client. Instead of (or alongside) exposing `/metrics` for Prometheus to
scrape, the exporter can push its samples to any compatible receiver:
Mimir, Grafana Cloud, Thanos Receive, VictoriaMetrics, or stock
Prometheus' `--web.enable-remote-write-receiver`.

## When to use this vs. a collector

See [alloy-vs-native-push.md](alloy-vs-native-push.md) for the long
version. Short version:

| Situation | Recommended path |
|---|---|
| You already run a Prometheus / Alloy / OpenTelemetry collector | **Scrape and let the collector remote_write.** Ignore push mode. |
| Edge / firewalled / homelab node, no collector available | **Use this exporter's push mode.** |
| You need disk-backed durability across restarts | **Use a collector.** v0.4.0 is in-memory only. |
| You need fan-out to multiple endpoints, relabeling, or PRW 2.0 native histograms today | **Use a collector.** v0.4.0 supports a single endpoint, no relabeling, PRW 1.0. |

## Configuration

```yaml
metrics_endpoint:
  enabled: true              # default true; false = push-only
  listen_address: ":9400"

remote_write:
  - url: https://mimir.example.com/api/v1/push
    name: primary            # used as the `endpoint` label; falls back to URL host
    flush_interval: 10s
    timeout: 30s
    insecure_http: false     # true required for plain http://
    queue:
      capacity: 10000        # batches, not samples
    retry:
      max_attempts: 10
      max_elapsed: 5m
      initial_backoff: 1s
      max_backoff: 30s
    circuit_breaker:
      failure_threshold: 5
      window: 1m
      cooldown: 30s
    tls:
      insecure_skip_verify: false
      ca_file: /etc/ssl/ca.pem
    basic_auth:
      username: tenant1
      password_file: /run/credentials/exporter/mimir_password
    # OR (mutually exclusive with basic_auth):
    bearer_token_file: /run/credentials/exporter/mimir_token
    headers:
      X-Scope-OrgID: tenant1
    external_labels:
      cluster: xena
      env: homelab
```

### Credentials

**Plaintext `password:` and `bearer_token:` keys are rejected at config
load.** This is by design: secrets do not belong in the config file. Use
`password_file` and `bearer_token_file` and stage credentials via
systemd `LoadCredential=`, Kubernetes mounted secrets, or
`/run/credentials/`.

`basic_auth` and `bearer_token_file` are mutually exclusive.

### Reserved headers

These are managed by the protocol layer and cannot be overridden:
`Authorization`, `Content-Encoding`, `Content-Type`,
`X-Prometheus-Remote-Write-Version`. Setting them under `headers:`
fails at config load.

### Multi-endpoint

The schema accepts an array, but v0.4.0 processes only the first entry
and logs a WARN if more are configured. Multi-endpoint fan-out is
planned for v0.5 as a non-breaking addition.

### Push-only mode

Set `metrics_endpoint.enabled: false` to skip binding the local HTTP
listener entirely. The exporter still requires at least one
`remote_write` endpoint in this mode (otherwise it would be a no-op
and refuses to start).

## Self-observability metrics

These are exposed on `/metrics` (when enabled) so you can debug a
broken push pipeline locally:

| Metric | Type | Labels |
|---|---|---|
| `ollama_exporter_remote_write_samples_total` | counter | `endpoint` |
| `ollama_exporter_remote_write_samples_failed_total` | counter | `endpoint`, `reason` |
| `ollama_exporter_remote_write_samples_dropped_total` | counter | `endpoint`, `reason` |
| `ollama_exporter_remote_write_send_duration_seconds` | histogram | `endpoint`, `outcome` |
| `ollama_exporter_remote_write_queue_length` | gauge | `endpoint` |
| `ollama_exporter_remote_write_queue_capacity` | gauge | `endpoint` |
| `ollama_exporter_remote_write_last_send_timestamp_seconds` | gauge | `endpoint` |
| `ollama_exporter_remote_write_retries_total` | counter | `endpoint` |
| `ollama_exporter_remote_write_circuit_breaker_state` | gauge (0=closed, 1=open, 2=half-open) | `endpoint` |

`reason` is one of `queue_full`, `non_retryable`, `retry_budget_exhausted`,
`breaker_open`, `shutdown_drain`. `outcome` is one of `success`,
`retryable_error`, `non_retryable_error`, `timeout`.

## Behavior under failure

- **Network blip / 5xx / 408 / 429 / connection refused:** retried with
  exponential backoff and full jitter, capped by `max_attempts` and
  `max_elapsed`. 429 with `Retry-After` is honored (capped at
  `max_backoff` to prevent a misbehaving server from stalling the
  sender indefinitely).
- **400 / 401 / 403 / 404:** dropped without retry. The receiver is
  telling you the payload is wrong or you are not authorized; retrying
  will not help. The circuit breaker takes one step toward open.
- **Sustained non-retryable failures:** the circuit breaker opens after
  `failure_threshold` failures within `window`. While open, batches are
  dropped with `reason="breaker_open"`. After `cooldown` the breaker
  goes half-open and probes once. Retryable failures (5xx, network)
  do **not** count against the breaker — that's what backoff is for.
- **Queue overflow:** when the bounded in-memory queue is full,
  the **oldest** batch is dropped and `samples_dropped_total{reason="queue_full"}`
  increments. Capacity is in batches, not samples.
- **Shutdown:** SIGINT/SIGTERM cancels the context, the queue is
  closed, and senders drain remaining batches with a 5s budget before
  the process exits.

## Durability

v0.4.0 is **Tier 2 durability**: bounded in-memory queue with
exponential backoff. Samples buffered during a remote outage are lost
if the exporter process restarts. A disk WAL is design-doc territory
for v0.5+.

If you cannot tolerate this, run a collector (Alloy, Prometheus Agent,
OTel Collector with the prometheusremotewrite exporter) — they all
ship disk-backed WALs.

## Limitations (v0.4.0)

- **Single endpoint** — multi-endpoint fan-out planned for v0.5.
- **PRW 1.0 only** — PRW 2.0 (native histograms, metadata) planned;
  the `Sender` interface is shaped to absorb it without config breakage.
- **No relabeling** — series are sent verbatim from the local registry
  with `external_labels` merged on top.
- **No metadata** — `MetricMetadata` is not emitted (the receiver
  infers types from the series name pattern, which is what most
  receivers do anyway).

## Troubleshooting

**Nothing arrives at the receiver.**
1. Check `ollama_exporter_remote_write_samples_total` on `/metrics` —
   if it's incrementing, the exporter believes it sent successfully.
2. Check `ollama_exporter_remote_write_samples_failed_total` by
   `reason` to see whether the failure is auth (401/403 →
   `non_retryable`), payload (400 → `non_retryable`), connectivity
   (connect refused, TLS handshake failure → `retry_budget_exhausted`),
   or back-pressure (`queue_full`).
3. Check `ollama_exporter_remote_write_circuit_breaker_state`. If it's
   1 (open), the breaker has tripped and is dropping batches until
   cooldown elapses.

**Receiver returns 400 with "out of bounds" or "out of order".**
Usually means the receiver's tenant has a tighter ingestion staleness
window than your `flush_interval`. Lower `flush_interval` or check
the receiver's `-ingester.max-out-of-order-time-window`.

**TLS verification fails with a self-signed certificate.**
Set `tls.ca_file` to the receiver's CA bundle. Use
`tls.insecure_skip_verify: true` only if you accept the risk — the
exporter logs a WARN at startup naming the affected endpoint.
