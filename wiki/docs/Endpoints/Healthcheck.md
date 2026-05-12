---
tags:
  - Reference
  - Endpoints
  - Monitoring
---

# Healthcheck

The Doco-CD image has a Docker health check that checks against `http://localhost:${HTTP_PORT}/v1/health` inside the container.

You can adjust the health check settings in your `docker-compose.yml` file like this:

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

You can see the health status of the container with the following command:

```sh
docker inspect --format='{{json .State.Health}}' doco-cd
```

See also the [Health Check API Endpoint](REST-API.md#health-check).