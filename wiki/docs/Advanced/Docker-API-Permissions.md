---
tags:
  - Advanced
  - Docker
  - Security
---

# Docker API Permissions

Doco-CD needs access to the Docker API to deploy your stacks.
By default this is the mounted Docker socket, which grants unrestricted control over the host.

If you would rather place a [Docker socket proxy](#example-socket-proxy-configuration) in front of the daemon, this page documents
which Docker API endpoints doco-cd actually uses, so you can grant only what your setup needs.

!!! warning "A socket proxy limits the blast radius, it does not make access read-only"
    Doco-CD is a deployment tool. It has to create, update and delete containers, images,
    networks and volumes (plus services, configs and secrets in Swarm mode) to do its job.
    A socket proxy is still worth it: it lets you deny endpoints doco-cd never uses, such as
    `/build`, `/exec` and `/plugins`, and it keeps the raw socket out of the container.

## Required endpoints per feature

The tables below list the Docker Engine API endpoints doco-cd calls, grouped by feature.
Paths are shown without the `/v1.x` API version prefix that the client prepends to every request.

### Always required

Every doco-cd installation needs these, regardless of the deployment mode.

| Feature                | Docker API endpoints                                                                     | Access |
|------------------------|------------------------------------------------------------------------------------------|--------|
| Client handshake       | `HEAD /_ping`, `GET /version`                                                            | read   |
| Startup verification   | `GET /info`                                                                              | read   |
| Swarm mode detection   | `GET /info`                                                                              | read   |
| Own container lookup (unless [`DATA_HOST_PATH`](../App-Settings.md#general-settings) is set) | `GET /containers/{id}/json` | read   |
| [Scheduled jobs](Job-Scheduling.md) discovery | `GET /containers/json`, `GET /services` (Swarm), `GET /events`         | read   |
| [Reconciliation](../Core-Concepts.md) watcher | `GET /events`, `GET /containers/json`, `GET /containers/{id}/json`     | read   |

!!! info "`GET /events` is needed by default"
    Both the scheduler (`SCHEDULER_ENABLED`) and reconciliation (`reconciliation.enabled`) are
    enabled by default and subscribe to the Docker event stream. Denying `/events` disables
    automatic drift recovery and scheduled jobs.

### Docker Compose deployments

| Feature              | Docker API endpoints                                                                                                   | Access      |
|----------------------|------------------------------------------------------------------------------------------------------------------------|-------------|
| Project discovery    | `GET /containers/json`, `GET /containers/{id}/json`, `GET /networks`, `GET /volumes`                                    | read        |
| Image pull           | `POST /images/create`, `GET /images/json`, `GET /images/{name}/json`                                                    | read/write  |
| Image drift check    | `GET /distribution/{name}/json`, `GET /images/{name}/json`, `GET /containers/{id}/json`                                 | read        |
| Network creation     | `GET /networks`, `GET /networks/{id}`, `POST /networks/create`                                                          | read/write  |
| Volume creation      | `GET /volumes`, `GET /volumes/{name}`, `POST /volumes/create`                                                           | read/write  |
| Container lifecycle  | `POST /containers/create`, `POST /containers/{id}/start`, `POST /containers/{id}/stop`, `POST /containers/{id}/restart` | read/write  |
| Container recreation | `POST /containers/{id}/rename`, `DELETE /containers/{id}`                                                               | destructive |
| Network attachment   | `POST /networks/{id}/connect`, `POST /networks/{id}/disconnect`                                                         | read/write  |

### Docker Swarm deployments

Used instead of most Compose endpoints when the daemon is a [Swarm manager](Swarm-Mode.md).

| Feature                    | Docker API endpoints                                                                              | Access      |
|----------------------------|----------------------------------------------------------------------------------------------------|-------------|
| Stack discovery            | `GET /services`, `GET /services/{id}`, `GET /tasks`                                                | read        |
| Convergence monitoring     | `GET /services/{id}`, `GET /tasks`, **`GET /nodes`**                                               | read        |
| Overlay networks           | `GET /networks`, `GET /networks/{id}`, `POST /networks/create`                                     | read/write  |
| Service deployment         | `POST /services/create`, `POST /services/{id}/update`                                              | read/write  |
| Configs                    | `GET /configs`, `GET /configs/{id}`, `POST /configs/create`, `POST /configs/{id}/update`           | read/write  |
| Secrets                    | `GET /secrets`, `GET /secrets/{id}`, `POST /secrets/create`, `POST /secrets/{id}/update`           | read/write  |
| Config / secret rotation   | `DELETE /configs/{id}`, `DELETE /secrets/{id}`                                                     | destructive |

!!! warning "`GET /nodes` is easy to miss"
    Doco-CD waits for services to converge, which reads the list of active Swarm nodes.
    Most socket proxies gate this behind a separate `NODES` permission that is **off by default**.
    Without it, every Swarm deployment fails with `403 Forbidden`.

!!! info "Why `POST /configs/{id}/update` is needed"
    Doco-CD appends a content hash to config and secret names (see [Swarm Mode](Swarm-Mode.md#configs-and-secrets)).
    A **new or changed** config is created with `POST /configs/create`.
    Redeploying an **unchanged** config finds the existing hashed name and refreshes it with
    `POST /configs/{id}/update`. Both are required. The same applies to secrets.

### Optional features

These endpoints are only needed when the corresponding setting is enabled.

| Setting                                                              | Default | Additional endpoints                                                                   | Access      |
|----------------------------------------------------------------------|---------|-----------------------------------------------------------------------------------------|-------------|
| [`prune_images`](../Deploy-Settings.md) (Compose)                     | `true`  | `DELETE /images/{name}`                                                                 | destructive |
| [`prune_images`](../Deploy-Settings.md) (Swarm)                       | `true`  | `POST /services/create`, `GET /services`, `POST /services/{id}/update`                  | destructive |
| [`remove_orphans`](../Deploy-Settings.md)                             | `true`  | `DELETE /containers/{id}`, `DELETE /networks/{id}`, `DELETE /services/{id}` (Swarm)     | destructive |
| [`destroy.enabled`](../Deploy-Settings.md)                            | `false` | `POST /containers/{id}/stop`, `DELETE /containers/{id}`, `DELETE /networks/{id}`, Swarm: `DELETE /services/{id}`, `DELETE /configs/{id}`, `DELETE /secrets/{id}` | destructive |
| [`destroy.remove_volumes`](../Deploy-Settings.md)                     | `true`  | `GET /volumes/{name}`, `DELETE /volumes/{name}`                                         | destructive |
| [`destroy.remove_images`](../Deploy-Settings.md)                      | `true`  | `DELETE /images/{name}`                                                                 | destructive |
| [`reconciliation.enabled`](../Deploy-Settings.md)                     | `true`  | `GET /events`, `POST /containers/{id}/restart`                                          | read/write  |
| `SCHEDULER_ENABLED`                                                   | `true`  | See [below](#scheduler_enabled-endpoint-details)                                 | read/write  |
| [`wait_running_jobs`](../Deploy-Settings.md)                          | `true`  | `GET /tasks` (Swarm), `GET /containers/json` (Compose)                                  | read        |
| [REST API](../Endpoints/REST-API.md) (`API_SECRET` set)               | off     | See [below](#rest-api-endpoint-details)                                         | destructive |

#### `SCHEDULER_ENABLED` endpoint details

- Default (`execution_mode: restart`): `POST /containers/{id}/restart`; Swarm: `GET /services/{id}`, `POST /services/{id}/update`
- `execution_mode: one_off`: `POST /containers/create`, `POST /containers/{id}/wait`, `POST /containers/{id}/start`; Swarm: `POST /services/create`, `DELETE /services/{id}`, `GET /tasks`
- `stop_services`: `POST /containers/{id}/stop|start`; Swarm: `POST /services/{id}/update`, `GET /tasks`

#### REST API endpoint details

- Compose project management: `GET /containers/json`, `POST /containers/{id}/start|stop|restart`, `DELETE /containers/{id}`, `DELETE /networks/{id}`, `DELETE /volumes/{name}`, `DELETE /images/{name}`
- Swarm stack/service management: `GET /services`, `POST /services/{id}/update`, `DELETE /services/{id}`, `DELETE /configs/{id}`, `DELETE /secrets/{id}`

!!! danger "Image pruning in Swarm mode bypasses the proxy"
    In Swarm mode, `prune_images` runs `docker image prune` inside a global job service that
    **bind-mounts `/var/run/docker.sock` on every node**. That container talks to the local
    daemon directly, so your socket proxy does not apply to it, and every node needs the raw
    socket available. Set `prune_images: false` if this is unacceptable in your environment.

### Building images

If any of your services use `build:` in their compose file,
you additionally need `POST /build` (and `POST /session` when BuildKit is used).

### Never used

Doco-CD does not call these endpoints, so you can safely deny them:

`POST /auth`, `POST /commit`, `POST /containers/{id}/exec`, `POST /exec/{id}/start`,
`GET /containers/{id}/logs`, `GET /containers/{id}/top`, `POST /containers/{id}/pause`,
`GET /plugins`, `GET /system/df`, `POST /swarm/init`, `POST /swarm/join`

## Example socket proxy configuration

The following example uses [tecnativa/docker-socket-proxy](https://github.com/Tecnativa/docker-socket-proxy),
which maps Docker API resources to environment variables.

!!! info "Other proxies use different variable names"
    Environment variable names differ between socket proxy implementations.
    The **Docker API endpoint tables above are the source of truth** — map them to whatever
    your proxy of choice expects.

```yaml title="docker-compose.yml"
services:
  dockerproxy:
    image: ghcr.io/tecnativa/docker-socket-proxy:latest
    environment:
      # Required by every installation
      PING: 1
      VERSION: 1
      INFO: 1
      EVENTS: 1
      CONTAINERS: 1
      IMAGES: 1
      NETWORKS: 1
      VOLUMES: 1
      DISTRIBUTION: 1
      # Docker Swarm only, omit these on a standalone daemon
      SERVICES: 1
      TASKS: 1
      NODES: 1 # (1)!
      SECRETS: 1
      CONFIGS: 1
      # Required to deploy anything
      POST: 1
      DELETE: 1 # (2)!
      # Endpoints doco-cd never uses
      AUTH: 0
      BUILD: 0
      COMMIT: 0
      EXEC: 0
      PLUGINS: 0
      SESSION: 0
      SWARM: 0
      SYSTEM: 0
    volumes:
      - /var/run/docker.sock:/var/run/docker.sock:ro
    restart: unless-stopped

  doco-cd:
    image: ghcr.io/kimdre/doco-cd:latest
    container_name: doco-cd
    depends_on:
      - dockerproxy
    environment:
      DOCKER_HOST: tcp://dockerproxy:2375 # (3)!
      WEBHOOK_SECRET: xxx
    ports:
      - "80:80"
    volumes:
      - data:/data # (4)!
    cap_drop:
      - ALL
    restart: unless-stopped

volumes:
  data:
```

1. Needed to wait for Swarm services to converge. Without it every Swarm deployment fails with `403 Forbidden`.
2. Required for recreating containers, rotating configs/secrets and `remove_orphans`. Set to `0` only if you accept that redeployments will fail.
3. Point doco-cd at the proxy instead of mounting the socket. See [Docker Settings](../Docker-Settings.md).
4. The Docker socket is no longer mounted here — only the data volume remains.

### Minimal standalone (Compose only) configuration

If you do not use Docker Swarm, drop the Swarm resources:

```yaml title="docker-compose.yml"
services:
  dockerproxy:
    image: ghcr.io/tecnativa/docker-socket-proxy:latest
    environment:
      PING: 1
      VERSION: 1
      INFO: 1
      EVENTS: 1
      CONTAINERS: 1
      IMAGES: 1
      NETWORKS: 1
      VOLUMES: 1
      DISTRIBUTION: 1
      POST: 1
      DELETE: 1
    volumes:
      - /var/run/docker.sock:/var/run/docker.sock:ro
```

## Troubleshooting

If a deployment fails with a `403 Forbidden` error such as:

```text
failed to deploy stack myapp: Error response from daemon:
<html><body><h1>403 Forbidden</h1>
Request forbidden by administrative rules.
</body></html>
```

your proxy is blocking an endpoint that doco-cd needs.
Most socket proxies log the rejected request, so check the proxy logs to find the exact
method and path, then look it up in the [tables above](#required-endpoints-per-feature).

The most common causes are:

| Missing permission | Symptom                                                                |
|--------------------|------------------------------------------------------------------------|
| `NODES`            | Swarm deployments fail while waiting for services to converge          |
| `CONFIGS`          | Swarm stacks using `configs:` fail to deploy                           |
| `SECRETS`          | Swarm stacks using `secrets:` fail to deploy                           |
| `DISTRIBUTION`     | Image update detection falls back or fails                             |
| `POST`             | Nothing can be created; deployments fail immediately                   |
| `DELETE`           | Container recreation, config/secret rotation and `destroy` fail        |
