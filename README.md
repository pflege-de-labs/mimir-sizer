# mimir-sizer

Collects the inputs from Grafana Mimir's
[capacity planning guide](https://grafana.com/docs/mimir/latest/manage/run-production-environment/planning-capacity/)
via any Prometheus-compatible query API and prints a Markdown sizing report for
**microservices** and **monolithic** mode, plus the samples/s Alloy sends via
`prometheus.remote_write` and the log entries/bytes per second it sends via `loki.write`.

## Usage

```sh
go build -o mimir-sizer .

# Query an existing Prometheus (or Mimir: --prom-url http://mimir/prometheus --tenant <org>)
kubectl port-forward svc/prometheus 9090 &
./mimir-sizer --prom-url http://localhost:9090 > report.md

# Offline calculator
./mimir-sizer --active-series 2000000 --scrape-interval 30s --qps 50
```

Every input can be set by flag, env (`PROM_URL`, `PROM_TENANT`, `PROM_TOKEN`, `PROM_USERNAME`, `PROM_PASSWORD`)
or YAML config (`--config file.yaml`, default `$XDG_CONFIG_HOME/mimir-sizer/config.yaml`; keys = flag names).
A flag value > 0 wins over a query. `./mimir-sizer --help` lists all flags.

## Where inputs come from

All queried values are the **peak** over `--window` (default 24h, 1m resolution). First query with data wins;
the report states the source of each value.

| Input | Queries, in order |
|---|---|
| Active series | `prometheus_tsdb_head_series`, Alloy `prometheus_remote_write_wal_storage_active_series`, Mimir `cortex_ingester_memory_series / RF` |
| Samples/s | Alloy `prometheus_remote_storage_{samples,histograms}_total` (filtered by `--mimir-url-regex`), Mimir `cortex_distributor_received_samples_total`; else `series / scrape interval` |
| Scrape interval | flag; else measured `series / samples/s`; else 15s. Overriding series or samples/s rescales the other with this interval |
| Queries/s | Mimir `cortex_query_frontend_queries_total{op=~"query\|query_range"}`, Prometheus `prometheus_http_requests_total` |
| Rule evals/s | `cortex_prometheus_rule_evaluations_total`, `prometheus_rule_evaluations_total` |
| Alerts | `alertmanager_alerts_received_total`, `alertmanager_alerts` (and `cortex_` variants) |
| Cache evictions | `memcached_items_evicted_total` |

Alloy throughput needs Alloy's own metrics in the queried source (`prometheus.exporter.self` or a scrape of
`:12345/metrics`). Narrow them with `--alloy-selector 'job="integrations/alloy"'`.

## Caveats

- Inputs that are neither set nor found are `0` (shown as `unknown`), so e.g. querier sizing comes out as 0.
  Set `--qps` for a new setup.
- HA Prometheus pairs or duplicate Alloy scrapes double-count active series and samples/s.
- Ratios are the doc's minimums for one tenant and default config. The `+50%` columns add the doc's headroom for traffic peaks.
  Ruler is sized for internal mode. Store-gateway disk scales with `--retention-days` and the scrape interval
  (about 13 GB per 1M series at 15s/365d).
