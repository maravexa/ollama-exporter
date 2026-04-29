# Alloy (or any collector) vs. native push

`ollama-exporter` v0.4.0 can push metrics directly via Prometheus
Remote Write — but in many topologies, that's not the right tool for
the job. This document is the honest version of which path to pick.

## Use a collector (Alloy, Prometheus Agent, OTel Collector)

Recommended for **most production deployments**.

A collector that scrapes the exporter and remote_writes onward gives
you everything the native push mode deliberately does not:

- **Disk-backed WAL durability** — samples survive collector restarts
  and remote outages of arbitrary length.
- **Multi-endpoint fan-out** — write to Mimir and Grafana Cloud and a
  local Thanos at the same time.
- **Relabeling and filtering** — drop high-cardinality series, rewrite
  labels, route by content.
- **Service discovery** — scrape every exporter in a fleet via
  Kubernetes / Consul / static config without per-host configuration.
- **Native histograms, metadata, exemplars** — PRW 2.0 features land
  in collectors first.
- **Battle-tested operations** — backpressure, scrape limits,
  cardinality alerts, the works.

If you are already running any of these, scrape the exporter on `:9400`
and use the collector's `remote_write` block. The exporter's push mode
is redundant in this topology and should stay disabled.

A reference Alloy config that scrapes the exporter and forwards to
Mimir lives at `examples/alloy/config.alloy`.

## Use the exporter's native push mode

Recommended for **single-node, no-collector deployments**.

The native push mode exists for the topology where standing up a
collector is overkill or impractical:

- **Edge / homelab nodes** running one or two services where the
  blast radius of an extra daemon outweighs the durability gains.
- **Firewalled inference hosts** with outbound-only network access,
  where pull-based scraping is not feasible.
- **Air-gapped lab environments** where you control both ends and
  want one process per machine.

The tradeoffs are explicit:

- Tier 2 durability — in-memory queue only. Samples buffered during a
  remote outage are lost if the exporter restarts.
- Single endpoint per exporter (v0.4.0).
- PRW 1.0 only.
- No relabeling, no service discovery, no fan-out.

If any of those constraints bite, you've outgrown native push and
should run a collector.

## Decision tree

```
Already running Alloy / Prometheus / OTel?
├── Yes ─▶ Scrape the exporter. Disable remote_write here.
└── No
    └── Can you tolerate sample loss across exporter restarts?
        ├── No ─▶ Run a collector. Even a single-node Alloy is fine.
        └── Yes
            └── One endpoint, no relabeling needed?
                ├── Yes ─▶ Use the exporter's native push mode.
                └── No  ─▶ Run a collector.
```
