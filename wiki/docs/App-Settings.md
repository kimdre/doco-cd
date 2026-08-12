---
tags:
  - Configuration
---

# Application Settings

## General Settings

The application can be configured using the following environment variables:

<!-- Sort table with https://sortfilterreordermarkdowntables.com/ -->
| Key                          | Type    | Description                                                                                                                                                                                                                                           | Default                                         |
|------------------------------|---------|-------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|-------------------------------------------------|
| `API_SECRET`                 | string  | Secret that is used to authenticate requests to the REST API (see [REST API](Endpoints/REST-API.md))                                                                                                                                                  | Rest API is disabled when not specified         |
| `API_SECRET_FILE`            | string  | Path to the file containing the API secret (Mutually exclusive with `API_SECRET`).                                                                                                                                                                    |                                                 |
| `DATA_HOST_PATH`             | string  | Optional source path of the deployment data mount as seen by the target Docker daemon. See [Remote Docker daemons](Docker-Settings.md#remote-docker-daemons).                                                                                         | Automatically detected                          |
| `DATA_MOUNT_PATH`            | string  | Destination path of the writable deployment data mount inside the doco-cd container (set this if you do not mount the data volume at `/data`).                                                                                                        | `/data`                                         |
| `DEPLOY_CONFIG_BASE_DIR`     | string  | Relative Path to the directory containing the deployment configuration files **in all repositories**. **NOTE**: This does not affect/alter the `working_dir` path in the deploy config. It must still be relative to the repository root.             | `/`                                             |
| `HTTP_PORT`                  | number  | Port on which the application will listen for incoming webhooks, API requests and [healthchecks](Endpoints/Healthcheck.md)                                                                                                                            | `80`                                            |
| `HTTP_PROXY`                 | string  | HTTP proxy to use for outgoing requests (e.g. `http://username:password@proxy.com:8080`)                                                                                                                                                              | Ignored when not specified                      |
| `LOG_LEVEL`                  | string  | Log level of the app. Possible values: `debug`, `info`, `warn`, `error`                                                                                                                                                                               | `INFO`                                          |
| `MAX_CONCURRENT_DEPLOYMENTS` | number  | Maximum number of concurrent deployments allowed                                                                                                                                                                                                      | `4`                                             |
| `MAX_DEPLOYMENT_LOOP_COUNT`  | number  | When the deployment loop detection should trigger a forced re-deployment on consecutive deployments for the same commit. Set to `0`, to disable the detection logic.                                                                                  | `2`                                             |
| `MAX_PAYLOAD_SIZE`           | number  | The maximum size of the webhook payload in bytes that the HTTP server will accept                                                                                                                                                                     | `1048576` (1MB = 1 * 1024 * 1024)               |
| `METRICS_PORT`               | number  | Port on which the application will expose [Prometheus metrics](Endpoints/Metrics.md)                                                                                                                                                                  | `9120`                                          |
| `OCI_INSECURE_REGISTRIES`    | list    | Comma-separated OCI registry `host[:port]` entries for [Compose includes](https://docs.docker.com/compose/how-tos/multiple-compose-files/include). **TLS verification is disabled** for these registries; use only for trusted registries.                                                                                     | Ignored when not specified                      |
| `PASS_ENV`                   | boolean | Controls whether environment variables from the doco-cd container should be passed to the deployment environment for docker compose variable interpolation. Use with caution, as this may expose sensitive information to the deployment environment. | `false`                                         |
| `POLL_CONFIG`                | list    | A list/array of poll configurations provided in YAML format (see [Poll Settings](Poll-Settings.md))                                                                                                                                                   | Ignored when not specified                      |
| `POLL_CONFIG_FILE`           | string  | Path to the file inside the container containing the poll configurations in YAML format (see [Poll Settings](Poll-Settings.md))                                                                                                                       | Ignored when not specified                      |
| `SCHEDULER_ENABLED`          | boolean | Controls whether this doco-cd instance starts the built-in [job scheduler](Advanced/Job-Scheduling.md). Disable it on secondary/[self-updater](Advanced/Self-Updating.md) instances that should not trigger scheduled jobs.                           | `true`                                          |
| `SECRET_ROTATION_ENABLED` | boolean | Controls whether this doco-cd instance runs deploy-level external secret rotation checks. Disable it on secondary instances sharing the same Docker targets. | `true` |
| `SECRET_ROTATION_INTERVAL_DEFAULT` | string | Global default interval for deploy-level `secret_rotation` checks when `secret_rotation.enabled` is true and no deploy-level `interval` override is set. Uses Go duration format (for example `5m`, `1h`). | `1h` |
| `TZ`                         | string  | The [timezone](https://en.wikipedia.org/wiki/List_of_tz_database_time_zones) used in the container.                                                                                                                                                   | `UTC`                                           |
| `WEBHOOK_SECRET`             | string  | Secret that is used by webhooks for authentication to the application                                                                                                                                                                                 | Webhook endpoint is disabled when not specified |
| `WEBHOOK_SECRET_FILE`        | string  | Path to the file containing the webhook secret (mutually exclusive with `WEBHOOK_SECRET`).                                                                                                                                                           |                                                 |
| `SOURCE_URL_REWRITES` | map of strings | YAML map of git source URL rewrite rules, applied to both webhook and poll deployments. See [Source URL Rewrites](#source-url-rewrites). | Ignored when not specified |
| `SOURCE_URL_REWRITES_FILE` | string | Path to a file containing `SOURCE_URL_REWRITES` YAML (mutually exclusive with `SOURCE_URL_REWRITES`). | |

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

## Pulling images from a private registry

If you want to pull images from a private registry, see [Private Container Registries](Advanced/Private-Container-Registries.md) in the wiki.

## Source URL Rewrites

`SOURCE_URL_REWRITES` (and `SOURCE_URL_REWRITES_FILE`) let you rewrite git source URLs before doco-cd clones them. Rules apply to both webhook- and poll-triggered deployments.

This is useful when your Git provider advertises a public URL (in webhook payloads or poll configs) but doco-cd should clone through an internal network path instead — for example when your Forgejo instance is behind a reverse proxy with a public domain, but is reachable directly over a Docker network.

Two match strategies are supported:

- **URL/URI prefix** — e.g. `https://forgejo.example.com/` or `git@forgejo.example.com:`.
  The matched prefix in the source URL is replaced with the configured target, and the repository path is appended as-is.
    - HTTPS URLs should end with `/` to avoid partial host matches.
    - SCP-style SSH URLs (e.g. `git@host:`) **must** end with `:` — it is the mandatory separator between host and repository path in SCP syntax (`user@host:path/repo.git`).
    - SCP syntax cannot express a port number. Use `ssh://` syntax when targeting a non-standard port (e.g. `"ssh://git@forgejo.internal:2222/"`).
- **Host/domain** — e.g. `forgejo.example.com`. Only the host (and optional port) is replaced; scheme, credentials, and path are preserved.

Rules are matched in order of specificity (longest key first).

!!! example

    ```yaml title="Some possible examples"
    SOURCE_URL_REWRITES:
      # HTTPS → internal HTTP (key ends with / to avoid partial-host matches)
      "https://forgejo.example.com/": "http://forgejo:3000/"
      # Host-only match (replaces host+port, keeps scheme/path)
      "forgejo.example.com": "forgejo:3000"
      # SCP-style SSH → SCP-style SSH (the trailing : is required — it is the SCP host/path separator)
      # OR: SCP-style SSH → ssh:// with non-standard port (SCP syntax cannot carry a port number)
      # Pick one of these two, not both (YAML map keys must be unique):
      # "git@forgejo.example.com:": "git@forgejo.internal:"
      # "git@forgejo.example.com:": "ssh://git@forgejo.internal:2222/"
    ```

    In your doco-cd `docker-compose.yml`, you can set this as follows:

    ```yaml title="docker-compose.yml"
    services:
      app:
        environment:
          SOURCE_URL_REWRITES: |
            # HTTP variant:
            "forgejo.example.com": "forgejo:3000"
            # SSH variant with explicit port:
            "git@forgejo.example.com:": "ssh://git@forgejo:2222/"
    ```
