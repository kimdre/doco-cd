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
| `DATA_MOUNT_PATH`            | string  | Container path of the writable deployment data mount (set this if you do not mount the data volume at `/data`).                                                                                                                                       | `/data`                                         |
| `DEPLOY_CONFIG_BASE_DIR`     | string  | Relative Path to the directory containing the deployment configuration files **in all repositories**. **NOTE**: This does not affect/alter the `working_dir` path in the deploy config. It must still be relative to the repository root.             | `/`                                             |
| `HTTP_PORT`                  | number  | Port on which the application will listen for incoming webhooks, API requests and [healthchecks](Endpoints/Healthcheck.md)                                                                                                                            | `80`                                            |
| `HTTP_PROXY`                 | string  | HTTP proxy to use for outgoing requests (e.g. `http://username:password@proxy.com:8080`)                                                                                                                                                              | Ignored when not specified                      |
| `LOG_LEVEL`                  | string  | Log level of the app. Possible values: `debug`, `info`, `warn`, `error`                                                                                                                                                                               | `INFO`                                          |
| `MAX_CONCURRENT_DEPLOYMENTS` | number  | Maximum number of concurrent deployments allowed                                                                                                                                                                                                      | `4`                                             |
| `MAX_DEPLOYMENT_LOOP_COUNT`  | number  | When the deployment loop detection should trigger a forced re-deployment on consecutive deployments for the same commit. Set to `0`, to disable the detection logic.                                                                                  | `2`                                             |
| `MAX_PAYLOAD_SIZE`           | number  | The maximum size of the webhook payload in bytes that the HTTP server will accept                                                                                                                                                                     | `1048576` (1MB = 1 * 1024 * 1024)               |
| `METRICS_PORT`               | number  | Port on which the application will expose [Prometheus metrics](Endpoints/Metrics.md)                                                                                                                                                                  | `9120`                                          |
| `PASS_ENV`                   | boolean | Controls whether environment variables from the doco-cd container should be passed to the deployment environment for docker compose variable interpolation. Use with caution, as this may expose sensitive information to the deployment environment. | `false`                                         |
| `POLL_CONFIG`                | list    | A list/array of poll configurations provided in YAML format (see [Poll Settings](Poll-Settings.md))                                                                                                                                                   | Ignored when not specified                      |
| `POLL_CONFIG_FILE`           | string  | Path to the file inside the container containing the poll configurations in YAML format (see [Poll Settings](Poll-Settings.md))                                                                                                                       | gnored when not specified                       |
| `TZ`                         | string  | The [timezone](https://en.wikipedia.org/wiki/List_of_tz_database_time_zones) used in the container.                                                                                                                                                   | `UTC`                                           |
| `WEBHOOK_SECRET`             | string  | Secret that is used by webhooks for authentication to the application                                                                                                                                                                                 | Webhook endpoint is disabled when not specified |
| `WEBHOOK_SECRET_FILE`        | string  | Path to the file containing the Git access token (Mutually exclusive with `WEBHOOK_SECRET`).                                                                                                                                                          |                                                 |

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

The settings in the `.env` file should be in the format `KEY=VALUE` or `KEY: VALUE` and separated by a newline.

Example `.env` file:
```yaml title=".env"
GIT_ACCESS_TOKEN: xxx
WEBHOOK_SECRET: xxx
```

### With `environment`

Example with `environment` option:
```yaml title="docker-compose.yml"
services:
  app:
    environment:
      GIT_ACCESS_TOKEN: xxx
      WEBHOOK_SECRET: xxx
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
