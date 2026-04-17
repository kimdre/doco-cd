---
tags:
  - Configuration
---

## Available Settings

The application can be configured using the following environment variables:

<!-- Sort table with https://sortfilterreordermarkdowntables.com/ -->
| Key                               | Type    | Description                                                                                                                                                                                                                                           | Default value                                          |
|-----------------------------------|---------|-------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|--------------------------------------------------------|
| `API_SECRET`                      | string  | Secret that is used to authenticate requests to the REST API (see [REST API](Endpoints/REST-API.md))                                                                                                                                                  | ` ` (Rest API is disabled when not specified)          |
| `API_SECRET_FILE`                 | string  | Path to the file containing the API secret (Mutually exclusive with `API_SECRET`).                                                                                                                                                                    | ` `                                                    |
| `AUTH_TYPE`                       | string  | AuthType is the type of authentication to use when cloning repositories via **http**                                                                                                                                                                  | `oauth2`                                               |
| `DEPLOY_CONFIG_BASE_DIR`          | string  | Relative Path to the directory containing the deployment configuration files **in all repositories**. **NOTE**: This does not affect/alter the `working_dir` path in the deploy config. It must still be relative to the repository root.             | `/`                                                    |
| `GIT_ACCESS_TOKEN`                | string  | Access token for cloning repositories (required for private repositories) via **HTTP**, see [Access Token Setup](Setup-Access-Token.md)                                                                                                               | ` ` (Optional for public repositories but recommended) |
| `GIT_ACCESS_TOKEN_FILE`           | string  | Path to the file containing the Git Access Token (Mutually exclusive with `GIT_ACCESS_TOKEN`).                                                                                                                                                        | ` `                                                    |
| `GIT_CLONE_SUBMODULES`            | boolean | Whether Git submodules are cloned too                                                                                                                                                                                                                 | `true`                                                 |
| `HTTP_PORT`                       | number  | Port on which the application will listen for incoming webhooks, API requests and [healthchecks](#Healthcheck)                                                                                                                                        | `80`                                                   |
| `HTTP_PROXY`                      | string  | HTTP proxy to use for outgoing requests (e.g. `http://username:password@proxy.com:8080`)                                                                                                                                                              | ` ` (Ignored when not specified)                       |
| `LOG_LEVEL`                       | string  | Log level of the app. Possible values: `debug`, `info`, `warn`, `error`                                                                                                                                                                               | `INFO`                                                 |
| `MAX_CONCURRENT_DEPLOYMENTS`      | number  | Maximum number of concurrent deployments allowed                                                                                                                                                                                                      | `4`                                                    |
| `MAX_DEPLOYMENT_LOOP_COUNT`       | number  | When the deployment loop detection should trigger a forced re-deployment on consecutive deployments for the same commit. Set to `0`, to disable the detection logic.                                                                                  | `2`                                                    |
| `MAX_PAYLOAD_SIZE`                | number  | The maximum size of the webhook payload in bytes that the HTTP server will accept                                                                                                                                                                     | `1048576` (1MB = 1 * 1024 * 1024)                      |
| `METRICS_PORT`                    | number  | Port on which the application will expose [Prometheus metrics](Endpoints/Metrics.md)                                                                                                                                                                  | `9120`                                                 |
| `PASS_ENV`                        | boolean | Controls whether environment variables from the doco-cd container should be passed to the deployment environment for docker compose variable interpolation. Use with caution, as this may expose sensitive information to the deployment environment. | `false`                                                |
| `POLL_CONFIG`                     | list    | A list/array of poll configurations provided in YAML format (see [Poll Settings](Poll-Settings.md))                                                                                                                                                   | ` ` (Ignored when not specified)                       |
| `POLL_CONFIG_FILE`                | string  | Path to the file inside the container containing the poll configurations in YAML format (see [Poll Settings](Poll-Settings.md))                                                                                                                       | ` ` (Ignored when not specified)                       |
| `SKIP_TLS_VERIFICATION`           | boolean | Skip TLS verification when cloning repositories.                                                                                                                                                                                                      | `false`                                                |
| `SSH_PRIVATE_KEY`                 | string  | The private key used for cloning repositories via SSH, see [SSH Key Setup](Setup-SSH-Key.md)                                                                                                                                                          | ` `                                                    |
| `SSH_PRIVATE_KEY_FILE`            | string  | Path to the file containing the SSH private key                                                                                                                                                                                                       | ` `                                                    |
| `SSH_PRIVATE_KEY_PASSPHRASE`      | string  | Passphrase for the SSH private key (if key was generated with a passphrase)                                                                                                                                                                           | ` `                                                    |
| `SSH_PRIVATE_KEY_PASSPHRASE_FILE` | string  | Path to the file containing the SSH private key passphrase                                                                                                                                                                                            | ` `                                                    |
| `TZ`                              | string  | The timezone used in the container and for timestamps in logs                                                                                                                                                                                         | `UTC`                                                  |
| `WEBHOOK_SECRET`                  | string  | Secret that is used by webhooks for authentication to the application                                                                                                                                                                                 | ` ` (Webhook endpoint is disabled when not specified)  |
| `WEBHOOK_SECRET_FILE`             | string  | Path to the file containing the Git access token (Mutually exclusive with `WEBHOOK_SECRET`).                                                                                                                                                          | ` `                                                    |

## Docker-specific Settings

Settings to configure the Docker client used by doco-cd to interact with the Docker daemon. 
By default, the Docker client will use the settings from the host system.

!!! note "All of these settings are optional."


| Key                     | Type    | Description                                                                                                                                                                       | Default value |
|-------------------------|---------|-----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|---------------|
| `DOCKER_API_VERSION`    | string  | Overwrites the API version that doco-cd will use to connect to the Docker Daemon (e.g. `"1.49"`)                                                                                  |               |
| `DOCKER_CERT_PATH`      | string  | The directory from which to load the TLS certificates ("ca.pem", "cert.pem", "key.pem'). The directory has to be accessible from inside the container, e.g. by using a bind mount |               |
| `DOCKER_HOST`           | string  | The url that doco-cd will use to connect to the Docker Daemon (e.g. `tcp://192.168.0.10:2375`)                                                                                    |               |
| `DOCKER_QUIET_DEPLOY`   | boolean | Disable the status output of Docker Compose deployments (e.g. pull, create, start, healthy) in the application logs                                                               | `true`        |
| `DOCKER_TLS_VERIFY`     | boolean | Enable or disable TLS verification                                                                                                                                                |               |
| `DOCKER_SWARM_FEATURES` | boolean | Enable the use Docker Swarm Mode features if the app has detected that it is running in a Docker Swarm environment                                                                | `true`        |

## Notification Settings

Doco-CD can be configured to send notifications with [Apprise](https://github.com/caronc/apprise) to various services when a deployment is started, finished, or failed.

See the [Notifications](Notifications.md) wiki page for more information on how to configure notifications.

## Encrypting sensitive data

Doco-CD supports the encryption of sensitive data in your doco-cd app config and deployment files with [SOPS](https://getsops.io/).

See the [Encryption](Encryption.md) wiki page for more information on how to use SOPS with Doco-CD.

## Healthcheck

The doco-cd image contains a docker health check checks against `http://localhost:${HTTP_PORT}/v1/health` inside the container.

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
      # The file name after the /run/secrets/ path is the name of the secret
      GIT_ACCESS_TOKEN_FILE: /run/secrets/git_access_token
      WEBHOOK_SECRET_FILE: /run/secrets/webhook_secret
    # The secret names must match with the secrets: section below
    secrets:
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

If you want to pull images from a private registry, see [Private Container Registries](Private-Container-Registries.md) in the wiki.
