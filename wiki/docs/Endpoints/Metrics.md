---
tags:
  - Reference
  - Endpoints
  - Monitoring
---

# Prometheus Metrics

The application exposes Prometheus metrics at the `/metrics` endpoint. This endpoint provides various metrics about the application's performance and health, which can be scraped by a Prometheus server for monitoring purposes.
When both `HTTP_TLS_CERT_FILE` and `HTTP_TLS_KEY_FILE` are set, the metrics endpoint is served over HTTPS as well.

By default, this endpoint is available on Port `9120`, but can be configured using the `METRICS_PORT` environment variable, see [App Settings](../App-Settings.md#api-and-webhook-settings).

## Available Metrics

The table lists every metric family, its type and description, and the label names attached to its time series. Metric names use the `doco_cd_` prefix.

<!-- metrics-table -->

Most operation-duration histograms have bucket boundaries from 5 milliseconds through 10 minutes: 
Prometheus' default boundaries plus 20, 30, 60, 120, 300, and 600 seconds. 
MCP request duration uses Prometheus' default buckets (5 milliseconds to 10 seconds), while Git mirror compaction duration 
uses 12 exponentially increasing buckets starting at 50 milliseconds. 
Prometheus exposes histogram data as `_bucket`, `_sum`, and `_count` series.

Each histogram `_bucket` series has an additional Prometheus-generated `le` label for the bucket's inclusive upper bound. Bucket counts are cumulative; for each combination of the listed labels, the `le="+Inf"` bucket has the same count as `_count`. The generated `le` label is not listed in the table and does not appear on `_sum` or `_count` series.

`doco_cd_deployment_stage_duration_seconds` uses only the `repository`, `stage`, and `outcome` labels. 
This avoids creating a large number of series; use `doco_cd_deployment_duration_seconds` for individual deployment durations.

## Swarm Service State

When Swarm features are enabled, the `doco_cd_service_desired_tasks`, `doco_cd_service_running_tasks` and `doco_cd_service_update_state` gauges describe the state of the Swarm services deployed by doco-cd.

- The metrics cover services in every Swarm-mode [Docker context](../Advanced/Docker-Contexts.md) that carry the doco-cd manager label.
  Swarm jobs, one-off job services and [scheduled job](../Advanced/Job-Scheduling.md) services are not included, because their tasks stop after each run.
- The `stack` label holds the Swarm stack name. The `service` label holds the service name without the `<stack>_` prefix.
- `doco_cd_service_update_state` is one-hot. The series with the current update state (`updating`, `paused`, `completed`, `rollback_started`, `rollback_paused` or `rollback_completed`) has the value `1`, all others have `0`.
  A service that was never updated has `0` for every state.
- The values are read from Docker when Prometheus scrapes the endpoint and are cached for a few seconds. All contexts are queried at the same time, each with a timeout of 5 seconds.
- Series of removed services disappear on the next refresh. Series of a context that does not answer also disappear until the context answers again.
  Use an `absent()` alert if you need to detect this.

Example alert expressions:

```promql
# Services with fewer running tasks than desired
doco_cd_service_running_tasks < doco_cd_service_desired_tasks

# Paused updates and rollbacks
doco_cd_service_update_state{state=~"paused|rollback_.*"} == 1

# No service state from the "prod" context
absent(doco_cd_service_desired_tasks{context="prod"})
```

## Grafana Dashboard

An example for a Grafana Dashboard can be found at [Grafana Dashboard #583](https://github.com/kimdre/doco-cd/discussions/583).