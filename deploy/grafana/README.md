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

## Overview and model table

Four summary tiles show reachable/observed Ollama endpoints, loaded model-instance
count, reported model VRAM, and completed requests over 24 hours. Counts and memory
are scoped to the selected datasource's `ollama` job. Disappeared scrape targets
are not counted as observed; use Prometheus target health separately. Latest
per-model throughput values are not summed into a misleading aggregate speed.

The full-width table separates model, exporter instance, quantization, family,
job, loaded state, VRAM, latest decode/prefill throughput and request counts.
Additional scrape labels remain columns and participate in identity matching.
The same model on two exporters (or in two scrape scopes) remains two rows.
The table scrolls horizontally within its panel at narrow widths. Hover a cell
and use its inspection icon to read/copy the full value; names are not shortened
in the queries. Column filters can narrow the displayed rows.

The four title-free stat panels show aggregate sparklines from range queries.
Their numbers use the latest evaluated sample, including explicit Unknown gaps;
they do not carry an older value forward with `lastNotNull`. Range evaluation
uses Grafana's sampling interval, so the final sample may precede the exact range
end by up to one interval. The model table uses instant queries at the range end.
Current `ollama_model_loaded` inventory anchors table rows, including unloaded
models. Optional metrics match **all** identity labels; only the endpoint label
is deliberately aggregated for request counts. Removed inventory rows stay out
of the table even when throughput gauges or historical request counters remain.
Explicit zero remains zero; absent metrics are Unknown, not zero or Unloaded.
The VRAM total is Unknown if any current inventory row lacks memory data.

The Endpoint picker applies to the table and aggregate request counts. Requests
are estimated 24-hour counter increases, not lifetime counts or the dashboard's
selected-range totals. The aggregate request tile includes retained history for
removed models; the table only includes current inventory. An absent request
series is Unknown, rather than evidence that no requests occurred.

The eight existing history panels retain throughput, request duration, inference
phase duration, cache pressure, lifecycle, inflight requests and request rate.
Their historical samples can include models no longer in the current table.

## Configurable table colors

Loaded is green, Unloaded yellow, and missing values display gray Unknown.
Numeric cells use solid backgrounds with these absolute lower bounds:

| Column | Neutral base | Light blue | Blue | Highest band |
| --- | --- | --- | --- | --- |
| Model VRAM | 0 bytes | 1 byte | 4 GiB | 16 GiB, dark blue |
| Latest decode | 0 tok/s | 1 tok/s | 10 tok/s | 30 tok/s, green |
| Latest prefill | 0 tok/s | 1 tok/s | 100 tok/s | 500 tok/s, green |
| Requests over 24h | 0 requests | 1 request | 100 requests | 1,000 requests, dark blue |

VRAM and request counts show magnitude, not capacity utilization or health.
TPS bands are illustrative performance guides: model size, quantization, hardware
and workload determine appropriate expectations. Change **Field overrides →
Thresholds** for each column to suit your deployment. These are display settings,
not alerts or security thresholds. Unknown remains gray and labeled Unknown;
a measured zero remains a numeric zero. Aggregate stat threshold meanings are
unchanged; their backgrounds use those existing colors.

## Validation and distribution

From the repository root:

    go test ./deploy/grafana
    python3 deploy/grafana/query_regressions.py > /tmp/dashboard-tests.yml
    promtool test rules /tmp/dashboard-tests.yml

The query regression generator uses the shipped expressions. It covers duplicate
models across exporters and additional scrape scopes, zero and missing values,
endpoint aggregation/selection, stale inventory with retained metrics, and empty
inventory. It requires an existing Prometheus `promtool`; it installs nothing.
Structural Go tests cover datasource selection, table transforms, inspection and
layout bounds. These checks do not replace a rendered Grafana review.

The previous multi-series stat layout overflowed during a half-width manual
review. The replacement table layout was manually accepted at half width in Grafana
10.4.0 with a synthetic three-model-instance/two-exporter fixture. Both fixture
datasources returned identical numeric values; all aggregate sparklines returned
historical samples. Sparse fixture data includes genuine gaps. This is not a
guarantee for every viewport, Grafana version or label cardinality. Check full-name
inspection, contained scrolling and unknown versus zero values after importing.

For an existing dashboard imported from Grafana.com, import this repository's
updated `dashboard.json` and replace the dashboard with the same UID after
reviewing local customizations. Updating the exporter does not update imported
dashboards. Grafana.com publication is separate; this source update does not
change the published revision.
