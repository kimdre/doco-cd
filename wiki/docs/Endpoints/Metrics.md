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

`doco_cd_deployment_stage_duration_seconds` uses only the `repository`, `stage`, and `outcome` labels. 
This avoids creating a large number of series; use `doco_cd_deployment_duration_seconds` for individual deployment durations.

## Grafana Dashboard

An example for a Grafana Dashboard can be found at [Grafana Dashboard #583](https://github.com/kimdre/doco-cd/discussions/583).