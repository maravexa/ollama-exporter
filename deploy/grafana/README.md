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

Rows 1–2 — Health and Overview
  Ollama Status, Model Loaded, VRAM Usage
  Decode TPS, Prefill TPS, Total Requests (24h)

Row 3 — Throughput
  Decode vs Prefill TPS over time
  Request duration percentiles (p50/p95/p99)

Row 4 — Inference Phases
  Eval duration p95 (decode)
  Prompt eval duration p95 (prefill)
  KV cache pressure ratio

Row 5 — Model Lifecycle
  Load/unload events
  Requests in flight
  Request rate (req/s)

## Overview labels and current values

Overview stat tiles show a concise series name alongside each value: the exporter
instance, plus model and quantization where applicable. Request totals also name
the endpoint. This keeps multiple models/exporters distinguishable without
Grafana falling back to a long Prometheus label set above each value (issue #21).

The overview uses instant queries evaluated at the selected time range's end.
This avoids retaining removed model gauges through `lastNotNull` over historical
samples. Time-series panels still show history. Total Requests remains a 24-hour
increase per model/endpoint, not a lifetime count or a count for the dashboard's
entire selected range; retained counter history can still include removed models.

For an existing dashboard imported from Grafana.com, import this repository's
updated `dashboard.json` and replace the dashboard with the same UID after
reviewing any local customizations. Updating the exporter alone does not update
an imported dashboard, and the Grafana.com listing needs a separate publication.

Configuration regression check (from the repository root):

    go test ./deploy/grafana

Before publishing, import into a real Grafana instance with one and multiple
models/exporters. Verify the overview names and numeric values remain readable,
status mappings still show UP/DOWN and IN VRAM/UNLOADED, and removed gauge series
vanish after Prometheus ingests their disappearance. Check the historical charts
and 24-hour request totals independently. The configuration test does not validate
Grafana rendering or a deployed Grafana.com revision.

## Multiple-series readability

The six overview tiles occupy two rows of three, with explicit 12-pixel label
and 24-pixel value sizes. This gives model, quantization, instance, and endpoint
names more room than the former six-column row, which could shrink names to tiny
text even after raw-label fallback was removed.

An isolated Grafana 10.4.0 / Prometheus 2.51.0 fixture with three model series
across two exporter instances rendered readable overview labels at an 1800-pixel
viewport. All six stat queries returned the expected series and values. This
layout change preserves queries, units, mappings, legends, datasource selection,
and instant-query behavior. It does not certify arbitrary model-name lengths,
higher cardinality, or smaller viewports, and does not update Grafana.com or an
already-imported dashboard. The fixture omitted histogram and lifecycle samples;
its empty historical panels were not evidence of runtime coverage for those data.
