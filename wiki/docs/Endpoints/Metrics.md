---
tags:
  - Reference
  - Endpoints
  - Metrics
---

# Prometheus Metrics

The application exposes Prometheus metrics at the `/metrics` endpoint. This endpoint provides various metrics about the application's performance and health, which can be scraped by a Prometheus server for monitoring purposes.

By default, this endpoint is available on Port `9120`, but can be configured using the `METRICS_PORT` environment variable, see [App Settings](../App-Settings.md#available-settings).

## Available Metrics

See the following Source Code to find out about the currently available metrics:
```go title="Prometheus Collectors"
--8<-- "internal/prometheus/collectors.go:collectors"
```

## Grafana Dashboard

An example for a Grafana Dashboard can be found at [Grafana Dashboard #583](https://github.com/kimdre/doco-cd/discussions/583).