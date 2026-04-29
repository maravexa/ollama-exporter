# ollama-exporter v0.4.0 — Remote Write (Push Mode)

This release adds native **Prometheus Remote Write 1.0** push as an
orthogonal capability alongside the existing pull `/metrics` endpoint.
Ollama metrics can now be delivered directly to Mimir, Grafana Cloud,
Thanos Receive, VictoriaMetrics, or any PRW 1.0–compatible store from
edge / firewalled / homelab nodes that don't run a separate collector.

## Highlights

- **Push mode works alongside pull.** `/metrics` is still served by
  default. Set `metrics_endpoint.enabled: false` for push-only.
- **Hand-rolled PRW 1.0 client.** We import only `prompb` for the
  protobuf types — no `tsdb`, no `scrape`, no `discovery`. The
  exporter binary stays small.
- **Self-observability metrics.** If push breaks, the local `/metrics`
  surface tells you exactly why: send outcome histogram, queue
  length/capacity, retries, circuit breaker state, drop reasons.
- **Bounded in-memory queue with drop-oldest** + exponential backoff
  with full jitter + per-endpoint circuit breaker. Retryable failures
  back off; non-retryable failures trip the breaker.
- **Credentials must be file-referenced.** Plaintext `password:` /
  `bearer_token:` keys are rejected at config load with a clear error
  naming the offending field.

## Quick start

```yaml
remote_write:
  - url: https://mimir.example.com/api/v1/push
    name: primary
    flush_interval: 10s
    bearer_token_file: /run/credentials/exporter/mimir_token
    external_labels:
      cluster: xena
```

Full reference: [docs/remote-write.md](docs/remote-write.md).
Reference configs: [examples/](examples/).

## Should you use this, or run a collector?

If you already run Alloy / Prometheus Agent / OTel Collector, **scrape
the exporter and use the collector's `remote_write`** — you'll get
disk-backed WAL durability, multi-endpoint fan-out, relabeling, and
PRW 2.0 features. v0.4.0 push is for the topology where standing up a
collector is overkill or impractical.

See [docs/alloy-vs-native-push.md](docs/alloy-vs-native-push.md) for
the full decision tree.

## Limitations (called out explicitly)

- **Tier 2 durability only.** In-memory queue with backoff; samples
  buffered during a remote outage are lost on process restart. A
  disk-backed WAL is design-doc territory for v0.5+.
- **Single endpoint per exporter.** The `remote_write` schema is an
  array, but v0.4.0 processes only the first entry and logs a WARN if
  more are configured. Multi-endpoint fan-out is a non-breaking v0.5
  addition.
- **PRW 1.0 only.** PRW 2.0 (native histograms, metadata) is planned;
  the internal `Sender` interface is shaped to absorb it without
  config breakage.
- **No relabeling.** Series go out verbatim with `external_labels`
  merged on top.

## Compatibility

- All existing config files continue to parse and behave identically.
- The legacy top-level `listen_addr` field still works. New configs
  should prefer `metrics_endpoint.listen_address`.
- Adding a `remote_write:` block enables push; omitting it preserves
  the v0.3.x pull-only behavior.

## Upgrade notes

No breaking changes. To enable push, add a `remote_write:` block to
your config. To go push-only, additionally set
`metrics_endpoint.enabled: false`.

The exporter refuses to start if both `metrics_endpoint.enabled` is
false and no `remote_write` endpoints are configured (it would be a
no-op).
