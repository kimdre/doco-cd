---
tags:
  - Configuration
---

# Application Settings

The settings are grouped below by the part of the application they configure.

## Runtime Settings

| Key                 | Type    | Description                                                                                                                                                                                                                 | Default |
|---------------------|---------|-----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|---------|
| `LOG_LEVEL`         | string  | Log level of the app. Possible values: `debug`, `info`, `warn`, `error`                                                                                                                                                     | `INFO`  |
| `SCHEDULER_ENABLED` | boolean | Controls whether this doco-cd instance starts the built-in [job scheduler](Advanced/Job-Scheduling.md). Disable it on secondary/[self-updater](Advanced/Self-Updating.md) instances that should not trigger scheduled jobs. | `true`  |
| `TZ`                | string  | The [timezone](https://en.wikipedia.org/wiki/List_of_tz_database_time_zones) used in the container.                                                                                                                         | `UTC`   |

## API and Webhook Settings

| Key                   | Type    | Description                                                                                                                                                                                   | Default                                         |
|-----------------------|---------|-----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|-------------------------------------------------|
| `API_SECRET`          | string  | Secret that is used to authenticate requests to the REST API (see [REST API](Endpoints/REST-API.md))                                                                                          | Rest API is disabled when not specified         |
| `API_SECRET_FILE`     | string  | Path to the file containing the API secret (Mutually exclusive with `API_SECRET`).                                                                                                            |                                                 |
| `METRICS_PORT`        | number  | Port on which the application will expose [Prometheus metrics](Endpoints/Metrics.md)                                                                                                          | `9120`                                          |
| `MCP_ENABLED`         | boolean | Enables the [MCP server](Endpoints/MCP-Server.md) at `/mcp`. Requires an API secret from `API_SECRET` or `API_SECRET_FILE`; requests use the same `x-api-key` authentication as the REST API. | `false`                                         |
| `OPENAPI_ENABLED`     | boolean | Exposes public REST and webhook [OpenAPI specifications and Swagger UIs](Endpoints/REST-API.md#openapi-documentation) under `/openapi/` and `/docs/`.                                         | `false`                                         |
| `WEBHOOK_SECRET`      | string  | Secret that is used by webhooks for authentication to the application                                                                                                                         | Webhook endpoint is disabled when not specified |
| `WEBHOOK_SECRET_FILE` | string  | Path to the file containing the webhook secret (mutually exclusive with `WEBHOOK_SECRET`).                                                                                                    |

## Deployment Settings

| Key                             | Type    | Description                                                                                                                                                                                                                                           | Default |
|---------------------------------|---------|-------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|---------|
| `DEPLOY_CONFIG_BASE_DIR`        | string  | Relative Path to the directory containing the deployment configuration files **in all repositories**. **NOTE**: This does not affect/alter the `working_dir` path in the deploy config. It must still be relative to the repository root.             | `/`     |
| `MAX_CONCURRENT_DEPLOYMENTS`    | number  | Maximum number of concurrent deployments allowed                                                                                                                                                                                                      | `4`     |
| `MAX_CONCURRENT_PREDEPLOYMENTS` | number  | Maximum number of concurrent pre-deployment operations (initialization and change detection) allowed                                                                                                                                                  | `8`     |
| `MAX_DEPLOYMENT_LOOP_COUNT`     | number  | When the deployment loop detection should trigger a forced re-deployment on consecutive deployments for the same commit. Set to `0`, to disable the detection logic.                                                                                  | `2`     |
| `PASS_ENV`                      | boolean | Controls whether environment variables from the doco-cd container should be passed to the deployment environment for docker compose variable interpolation. Use with caution, as this may expose sensitive information to the deployment environment. | `false` |

## HTTP and Network Settings

| Key                      | Type   | Description                                                                                                                                                                                                                                                                                                                                                                                             | Default                                                       |
|--------------------------|--------|---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|---------------------------------------------------------------|
| `HTTP_PORT`              | number | Port on which the application will listen for incoming webhooks, API requests and [healthchecks](Endpoints/Healthcheck.md)                                                                                                                                                                                                                                                                              | `80`                                                          |
| `HTTP_PROXY`             | string | HTTP proxy to use for outgoing requests (e.g. `******proxy.com:8080`)                                                                                                                                                                                                                                                                                                                                   | Ignored when not specified                                    |
| `HTTP_TLS_CERT_FILE`     | string | Path to the PEM certificate file used by the main webhook/API/healthcheck and metrics servers. HTTPS is enabled automatically when both `HTTP_TLS_CERT_FILE` and `HTTP_TLS_KEY_FILE` are set.                                                                                                                                                                                                           |                                                               |
| `HTTP_TLS_KEY_FILE`      | string | Path to the PEM private key file used by the main webhook/API/healthcheck and metrics servers. HTTPS is enabled automatically when both `HTTP_TLS_CERT_FILE` and `HTTP_TLS_KEY_FILE` are set.                                                                                                                                                                                                           |                                                               |
| `MAX_PAYLOAD_SIZE`       | number | The maximum size of the webhook payload in bytes that the HTTP server will accept. For example 1 MB is `1048576` (1 * 1024 * 1024)                                                                                                                                                                                                                                                                      | `1048576`                                                     |
| `TRUSTED_PROXY_HEADER`   | string | HTTP header name containing the client's original IP address. Only used when the remote peer's IP is in `TRUSTED_PROXY_NETWORKS`. Header names are matched case-insensitively. When set to `X-Forwarded-For` (the default), falls back to the [RFC 7239](https://tools.ietf.org/html/rfc7239) `Forwarded` header if `X-Forwarded-For` is absent. A custom header is used exclusively, with no fallback. | `X-Forwarded-For`                                             |
| `TRUSTED_PROXY_NETWORKS` | list   | Comma-separated CIDR ranges that identify trusted proxies. When the remote peer matches one of these ranges, doco-cd reads the client IP from the header specified in `TRUSTED_PROXY_HEADER`.                                                                                                                                                                                                           | `127.0.0.0/8,10.0.0.0/8,172.16.0.0/12,192.168.0.0/16,::1/128` |

## Storage Settings

| Key               | Type   | Description                                                                                                                                                    | Default                |
|-------------------|--------|----------------------------------------------------------------------------------------------------------------------------------------------------------------|------------------------|
| `DATA_HOST_PATH`  | string | Optional source path of the artifact storage mount as seen by the target Docker daemon. See [Remote Docker daemons](Docker-Settings.md#remote-docker-daemons). | Automatically detected |
| `DATA_MOUNT_PATH` | string | Destination path of the writable artifact storage mount inside the doco-cd container (set this if you do not mount the data volume at `/data`).                | `/data`                |

### Artifact Garbage Collection Settings

| Key                             | Type     | Description                                                                                                                                                                                                                                                                             | Default |
|---------------------------------|----------|-----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|---------|
| `ARTIFACT_GC_ENABLED`           | boolean  | Enables the built-in sweeper that removes old, unreferenced published source artifacts (Git revisions/OCI digests) to reclaim disk space and limit how long decrypted SOPS secrets remain on disk. See [Artifact Garbage Collection](Reference/Artifact-Storage.md#garbage-collection). | `true`  |
| `ARTIFACT_GC_INTERVAL`          | duration | How often the artifact garbage collector sweeps for removable artifacts (it also always sweeps once at startup). Accepts a [Go duration](https://pkg.go.dev/time#ParseDuration).                                                                                                        | `10m`   |
| `ARTIFACT_GC_RETENTION_RECORDS` | number   | Number of most-recent unreferenced artifacts kept per repository/artifact, regardless of `ARTIFACT_GC_RETENTION_TTL`.                                                                                                                                                                   | `2`     |
| `ARTIFACT_GC_RETENTION_TTL`     | duration | How long an unreferenced artifact beyond `ARTIFACT_GC_RETENTION_RECORDS` is kept before it becomes eligible for removal. Accepts a [Go duration](https://pkg.go.dev/time#ParseDuration).                                                                                                |         |

## OCI Registry Settings

| Key                       | Type | Description                                                                                                                                                                                                                                | Default                    |
|---------------------------|------|--------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|----------------------------|
| `OCI_INSECURE_REGISTRIES` | list | Comma-separated OCI registry `host[:port]` entries for [Compose includes](https://docs.docker.com/compose/how-tos/multiple-compose-files/include). **TLS verification is disabled** for these registries; use only for trusted registries. | Ignored when not specified |

### Pulling images from a private registry

If you want to pull images from a private registry, see [Container Registry Authentication](Advanced/Container-Registry-Authentication.md).


## Notification Settings

Doco-CD can be configured to send [Notifications](Advanced/Notifications.md) with [Apprise](https://github.com/caronc/apprise) to various services when a deployment is started, finished, failed, or triggered by [reconciliation](Deploy-Settings.md#reconciliation-settings).

Reconciliation-triggered notifications use a short `[R]` marker in the title.  
See [Reconciliation notifications](Advanced/Notifications.md#reconciliation-notifications) for configuration and format details.

## Encrypting sensitive data

Doco-CD supports the encryption of sensitive data in your doco-cd app config and deployment files with [SOPS](https://getsops.io/).

See the [Encryption](Advanced/Encryption.md) wiki page for more information on how to use SOPS with Doco-CD.

## Specifying the settings

You can set the settings directly in the `docker-compose.yml` file with the `environment` option
or in a separate `.env` file with the `env_file` option.

Both options can be used at the same time.

### With `env_file`

Example with `env_file` option:
```yaml title="docker-compose.yml"
services:
  app:
    env_file:
      - .env
```

The settings in the `.env` file must be in the format `#!ini KEY=VALUE` or `#!yaml KEY: VALUE`, one setting per line.

#### Simple example

Example `.env` file:
```ini title=".env"
GIT_ACCESS_TOKEN=xxx
WEBHOOK_SECRET=xxx
```

#### Multiline YAML options

For multiline YAML options like `POLL_CONFIG` and `SOURCE_URL_REWRITES`, the `.env` file format does not support multiline values. Instead, use the corresponding `*_FILE` environment variables to point to separate YAML files:

```ini title=".env"
POLL_CONFIG_FILE=/mnt/poll-config.yaml
SOURCE_URL_REWRITES_FILE=/mnt/source-url-rewrites.yaml
```

Then create the YAML files:

```yaml title="poll-config.yaml"
- url: https://github.com/example/repo1.git
  interval: 300
- url: https://github.com/example/repo2.git
  reference: dev
  interval: 600
```

```yaml title="source-url-rewrites.yaml"
"https://forgejo.example.com/": "http://forgejo:3000/"
"git@forgejo.example.com:": "ssh://git@forgejo.internal:2222/"
```

!!! note "Files must be mounted into the container"
    When using `*_FILE` environment variables, you must mount the specified files into the doco-cd container. For example, if using `/mnt/poll-config.yaml`, ensure it is mounted as a volume in `docker-compose.yml`:
    ```yaml
    services:
      app:
        volumes:
          - ./poll-config.yaml:/mnt/poll-config.yaml:ro
          - ./source-url-rewrites.yaml:/mnt/source-url-rewrites.yaml:ro
    ```

Alternatively, use the `environment` option in `docker-compose.yml` instead of `.env` to set multiline values directly (see below).

### With `environment`

#### Simple example

Example with `environment` option:
```yaml title="docker-compose.yml"
services:
  app:
    environment:
      GIT_ACCESS_TOKEN: xxx
      WEBHOOK_SECRET: xxx
```

#### Multiline YAML options

For multiline YAML options like `POLL_CONFIG` and `SOURCE_URL_REWRITES`, use YAML's literal block scalar (`|`):

```yaml title="docker-compose.yml"
services:
  app:
    environment:
      POLL_CONFIG: |
        - url: https://github.com/example/repo1.git
          interval: 300
        - url: https://github.com/example/repo2.git
          reference: dev
          interval: 600
      SOURCE_URL_REWRITES: |
        "https://forgejo.example.com/": "http://forgejo:3000/"
        "git@forgejo.example.com:": "ssh://git@forgejo.internal:2222/"
```

## Usage with Docker Secrets

The application can also be configured to use [Docker secrets](https://docs.docker.com/engine/swarm/secrets/) for sensitive information like the Git access token and the webhook secret.

!!! note
    Docker secrets are only fully supported in Docker Swarm mode.
    You can still use [Docker secrets in the normal (standalone) mode](https://docs.docker.com/compose/how-tos/use-secrets/), but it is less secure.


To use Docker secrets, you need to create the secrets in Docker and then reference them in the `docker-compose.yml` file.

### Create Docker Secrets
Create Docker secrets (only with Docker Swarm)

```sh
echo "<your Git token>" | docker secret create git_access_token -
echo "<random secret>" | docker secret create webhook_secret -
```

### Reference Docker Secrets in `docker-compose.yml`
```yaml title="docker-compose.yml" hl_lines="10-16 24-29"
services:
  app:
    container_name: doco-cd
    image: ghcr.io/kimdre/doco-cd:latest
    restart: unless-stopped
    ports:
      - "80:80"
    environment:
      TZ: Europe/Berlin
      GIT_ACCESS_TOKEN_FILE: /run/secrets/git_access_token # (1)!
      WEBHOOK_SECRET_FILE: /run/secrets/webhook_secret
    secrets: # (2)!
      - git_access_token
      - webhook_secret
    volumes:
      - /var/run/docker.sock:/var/run/docker.sock
      - data:/data

volumes:
  data:

secrets:
  git_access_token:
    external: true
  webhook_secret:
    external: true
```

1. The file name after the `/run/secrets/` path is the name of the secret
2. Secret names must match with the `secrets:` top-level section below

### Deploy in Docker Swarm mode
To run the application in Docker Swarm mode, you need to use the `docker stack deploy` command instead of `docker compose up`.

```sh
docker stack deploy -c docker-compose.yml doco-cd
```

### Check the logs
To check the logs of the application, you can use the following command:

```sh
docker service logs doco-cd_app
```

### Check the status of the service
To check the status of the service, you can use the following command:

```sh
docker service ps doco-cd_app
```
