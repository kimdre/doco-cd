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

See the following Source Code to find out about the currently available metrics:
```go title="Prometheus Collectors"
--8<-- "internal/prometheus/collectors.go:collectors"
```

Duration histograms using `DurationBuckets` have buckets from 5ms up to 10 minutes (the Prometheus default buckets plus 20s, 30s, 60s, 120s, 300s and 600s), so long-running deployments, webhooks and source preparations still get meaningful quantiles.

`deployment_stage_duration_seconds` is labelled by `repository`, `stage` and `outcome` only, because a histogram per deployment and stage produces a large number of series. Use `deployment_duration_seconds` for the duration of individual deployments.

## Grafana Dashboard

An example for a Grafana Dashboard can be found at [Grafana Dashboard #583](https://github.com/kimdre/doco-cd/discussions/583).