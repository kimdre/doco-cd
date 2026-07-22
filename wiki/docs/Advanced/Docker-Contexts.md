---
tags:
  - Advanced
  - Docker
  - Contexts
---

# Docker Contexts

Use the `context` [deployment config](../Deploy-Settings.md#available-settings) option to target a specific [Docker context](https://docs.docker.com/engine/manage-resources/contexts/) instead of the default one.
This lets one doco-cd instance manage and deploy to multiple Docker hosts/clusters.

!!! info "Default Docker context"
    Default Docker context means the local Docker host (usually via the mounted socket `/var/run/docker.sock`).

## 1. Create Docker contexts

Create contexts on the host that runs doco-cd.

```sh
# Example: remote Docker host over TCP
docker context create prod-remote --docker host=tcp://prod-host:2376

# Example: second environment
docker context create staging-remote --docker host=tcp://staging-host:2376
```

## 2. Verify contexts

```sh
docker context ls
docker --context prod-remote info
docker --context staging-remote info
```

## 3. Mount Docker context config into doco-cd

Docker context config must be available in the doco-cd container.

```yaml title="docker-compose.yml" hl_lines="7"
services:
  doco-cd:
    image: ghcr.io/kimdre/doco-cd:latest
    container_name: doco-cd
    volumes:
      - /var/run/docker.sock:/var/run/docker.sock
      - ~/.docker:/root/.docker:ro # (1)!
```

1. Docker contexts + credentials

If you need private registry access, ensure the mounted Docker config includes required auth data (see [Private Container Registries](Private-Container-Registries.md)).

## 4. Reference context in deployment config

```yaml title=".doco-cd.yml" hl_lines="2"
name: myapp-prod
context: prod-remote
reference: main
working_dir: deploy
compose_files:
  - compose.yml
```

If `context` is omitted (or empty), doco-cd uses the default Docker context.

!!! warning "Do not set `DOCKER_HOST` when using `context`"
    If the `DOCKER_HOST` environment variable is set in the doco-cd container, Docker's endpoint resolution takes it over any Docker context, so the `context` option is silently ignored (or errors on conflict).
    Leave `DOCKER_HOST` unset and rely on the mounted socket and Docker contexts instead.

## 5. Use different contexts per deployment

```yaml title=".doco-cd.yml" hl_lines="3 9"
---
name: myapp-staging
context: staging-remote
reference: develop
working_dir: deploy

---
name: myapp-prod
context: prod-remote
reference: main
working_dir: deploy
```

Each deployment uses its own Docker context for deploy, destroy, and cleanup operations.
