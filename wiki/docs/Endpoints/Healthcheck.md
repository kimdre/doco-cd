---
tags:
  - Reference
  - Endpoints
  - Monitoring
---

# Healthcheck

The Doco-CD image has a Docker health check that runs `/healthcheck` inside the container, which checks against `http://localhost:${HTTP_PORT}/v1/health` by default.
When both `HTTP_TLS_CERT_FILE` and `HTTP_TLS_KEY_FILE` are set, the built-in health check automatically switches to `https://localhost:${HTTP_PORT}/v1/health`.
`/healthcheck` is a small, separate binary that only reads `HTTP_PORT`, `HTTP_TLS_CERT_FILE`, `HTTP_TLS_KEY_FILE` and `LOG_LEVEL`, so it does not load or validate the rest of the configuration.

You can adjust the health check settings in your `docker-compose.yml` file like this.
Leave out `test` to keep using the image's health check command:

```yaml title="docker-compose.yml"
services:
  app:
    container_name: doco-cd
    healthcheck:
      start_period: 15s
      interval: 30s
      timeout: 5s
      retries: 3
```

!!! tip "Use `/healthcheck` instead of `/doco-cd healthcheck`"
    `/doco-cd healthcheck` still works and runs the same check, but it starts the full doco-cd binary.
    Every start runs the initialization code of all its dependencies, which keeps a large part of the binary in the page cache.
    `docker stats` counts that page cache as container memory.
    If your compose file sets `test: ["CMD", "/doco-cd", "healthcheck"]`, remove the `test` line or change it to `test: ["CMD", "/healthcheck"]`.

You can see the health status of the container with the following command:

```sh
docker inspect --format='{{json .State.Health}}' doco-cd
```

See also the [Health Check API Endpoint](REST-API.md#health-check).