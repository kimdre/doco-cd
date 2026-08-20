---
tags:
  - Setup
  - Advanced
  - Docker
---

# Container Registry Authentication

This page covers how to configure authentication for container registries, including private registries. To access a registry that requires authentication, you need to provide the credentials by adding the docker config file `~/.docker/config.json` (see also [`DOCKER_CONFIG`](../Docker-Settings.md#common-environment-variables)) to the doco-cd container.

## Supported credential setups

Doco-CD uses Docker CLI auth resolution. The following setups are supported:

- Inline credentials in `auths` (recommended), either in [mounted `config.json`](#mounting-an-existing-docker-config-file) or via [`DOCKER_AUTH_CONFIG`](#using-docker_auth_config).
- `credsStore` / `credHelpers` based configs **only if** the matching `docker-credential-*` helper binaries are available inside the doco-cd container.

If your host `config.json` references helpers that are not available in the container, registry authentication will fail.

## Mounting an existing docker config file

You can mount your existing `~/.docker/config.json` (see also [`DOCKER_CONFIG`](../Docker-Settings.md#common-environment-variables)) file from the host to the container if you have already added the credentials using `docker login` on the host machine.

??? example "How to add credentials using `docker login`"

    Run `docker login` to add the credentials to the config file:
    
    ```sh
    docker login my.registry.example
    ```

    If the login is successful, the credentials will be stored in the `~/.docker/config.json` file on your host machine. You can then mount this file to the doco-cd container to allow it to access the private registry.

Mount the config file to the container:

```yaml title="docker-compose.yml" hl_lines="7"
services:
  app:
    container_name: doco-cd
    volumes:
      - /var/run/docker.sock:/var/run/docker.sock
      - data:/data
      - ~/.docker/config.json:/root/.docker/config.json:ro # !(1)!
```

1. The `:ro` option mounts the file as read-only.

!!! note "File permissions"
    Make sure the mounted file has the correct permissions and is readable inside the container.

!!! tip "Short-lived registry tokens (ECR, GCR, ACR)"
    Doco-CD re-reads the config file from disk on every registry auth lookup, so credentials refreshed on disk (for example an ECR token rotated by a cron with `aws ecr get-login-password | docker login`) take effect without a restart of doco-cd.

## Using a custom docker config file

1. Encode your credentials to base64 (here we use `printf` to avoid the trailing newline, you can also use `echo -n`):

    ```sh
    printf 'username:password' | base64
    ```

2. Then create a file called `docker-config.json` that contains the authentication information in JSON format:

    ```json title="docker-config.json"
    {
        "auths": {
            "my.registry.example": {
                "auth": "(base64 output here)"
            }
        }
    }
    ```

3. Lastly, add the config file as secret and mount it to `/root/.docker/config.json`:

    ```yaml title="docker-compose.yml" hl_lines="16-18 20-22"
    services:
      app:
        container_name: doco-cd
        image: ghcr.io/kimdre/doco-cd:latest
        restart: unless-stopped
        ports:
          - "80:80" # Webhook endpoint
          - "9120:9120" # Prometheus metrics
        environment:
          TZ: Europe/Berlin
          GIT_ACCESS_TOKEN: xxx
          WEBHOOK_SECRET: xxx
        volumes:
          - /var/run/docker.sock:/var/run/docker.sock
          - data:/data
        secrets:
          - source: docker-config
            target: /root/.docker/config.json
    
    secrets:
      docker-config:
        file: docker-config.json
    
    volumes:
      data:
    ```

## Using `DOCKER_AUTH_CONFIG`

Instead of mounting a file, you can provide credentials directly through `DOCKER_AUTH_CONFIG`:

```yaml title="docker-compose.yml"
services:
  app:
    environment:
      DOCKER_AUTH_CONFIG: |
        {
          "auths": {
            "my.registry.example": {
              "auth": "BASE64_USERNAME_PASSWORD"
            }
          }
        }
```

!!! warning "Less secure than file or secret mounts"
    `DOCKER_AUTH_CONFIG` stores registry credentials directly in an environment variable. In containerized setups, environment variables are often easier to expose accidentally (for example through inspect/debug output, process environment dumps, or logs) than mounted files/secrets with restrictive permissions.
    Prefer mounted secrets or a mounted `config.json` where possible.

## Troubleshooting auth failures

When pull/update logs contain authorization errors (for example `pull access denied`, `unauthorized`, or `access forbidden`):

1. Ensure doco-cd reads the expected config path (`/root/.docker/config.json` by default, or your [`DOCKER_CONFIG`](../Docker-Settings.md#common-environment-variables) override).
2. If config uses `credsStore` / `credHelpers`, ensure required `docker-credential-*` binaries exist in the container at the expected path (for example `/usr/local/bin/<docker-credential-helper>`). If they are missing, either install/mount them in the container or use a different credential setup.
3. Prefer inline `auths` entries for containerized doco-cd deployments when helper binaries are unavailable.
