# Grafana Dashboard

## Quick Import

1. Open Grafana (default: http://localhost:3000)
2. Dashboards -> New -> Import
3. Upload dashboard.json
4. Select your Prometheus datasource
5. Click Import

## Prometheus Setup

Add the ollama-exporter scrape target to your Prometheus config.

Option A — include file (recommended):

    # In prometheus.yml
    scrape_config_files:
      - /etc/prometheus/conf.d/ollama-exporter.yml

    # Then copy:
    cp deploy/prometheus/ollama-exporter.yml /etc/prometheus/conf.d/

Option B — paste directly into prometheus.yml:

    scrape_configs:
      - job_name: 'ollama'
        scrape_interval: 15s
        static_configs:
          - targets: ['localhost:9400']

Reload Prometheus after changes:

    curl -X POST http://localhost:9090/-/reload

Confirm the target is healthy:

    http://localhost:9090/targets

The ollama job should show state: UP within one scrape interval (15s).

## Distributed Setup

If your exporter runs on a different host than Prometheus, replace
localhost with your exporter's hostname or IP in the scrape config:

    static_configs:
      - targets: ['exporter-host.example.com:9400']

## Panels

Row 1 — Health and Overview
  Ollama Status, Model Loaded, VRAM Usage, Decode TPS, Prefill TPS,
  Total Requests (24h)

Row 2 — Throughput
  Decode vs Prefill TPS over time
  Request duration percentiles (p50/p95/p99)

Row 3 — Inference Phases
  Eval duration p95 (decode)
  Prompt eval duration p95 (prefill)
  KV cache pressure ratio

Row 4 — Model Lifecycle
  Load/unload events
  Requests in flight
  Request rate (req/s)
