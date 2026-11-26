---
tags:
  - Configuration
  - Deployment
---

# Poll Settings

!!! question "About Polling"
    Polling is a time-based trigger that checks the repositories for changes to deploy at regular intervals. This method does not require doco-cd to be reachable from the internet but is less efficient and slower than webhooks.

## Configuration

| Key                | Type   | Description                                                                                                                     | Default                    |
|--------------------|--------|---------------------------------------------------------------------------------------------------------------------------------|----------------------------|
| `POLL_CONFIG`      | list   | A list/array of poll configurations provided in YAML format (see [Poll Settings](Poll-Settings.md))                             | Ignored when not specified |
| `POLL_CONFIG_FILE` | string | Path to the file inside the container containing the poll configurations in YAML format (see [Poll Settings](Poll-Settings.md)) | Ignored when not specified |

For Git sources, poll jobs also honor global source URL rewrites from [`SOURCE_URL_REWRITES`](Advanced/Source-URL-Rewrites.md). This is useful when polling should clone through an internal host/port instead of the public URL.

[Supported Git transports](Git-Settings.md#supported-transports) include HTTP(S), SSH, [native Git](Advanced/Tips-and-Tricks.md#polling-via-native-git) (`git://`), and [local filesystem](Advanced/Local-Filesystem-Polling.md) repositories.

They must be in the format of a YAML list/array (also called YAML Sequence) and can contain the following settings:

!!! note "Settings without a default value are required."

| Key           | Type                                          | Description                                                                                                                                                                                                                                                                                                                                                                                                                                                 | Default value                       |
|---------------|-----------------------------------------------|-------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|-------------------------------------|
| `source`      | string                                        | Source backend for this poll job. Use `git` (default) for repositories or `oci` for [OCI artifacts](Advanced/OCI/Artifact-Usage.md).                                                                                                                                                                                                                                                                                                                        | `git`                               |
| `url`         | string                                        | Source URL. For `source: git`, this is the Git clone URL (e.g. `https://github.com/kimdre/doco-cd.git` or `git@github.com:kimdre/doco-cd.git`), or a local absolute path mounted into the container (e.g. `/local-repos/my-app`, equivalent to `file:///local-repos/my-app`, see [Polling Local Filesystem Repositories](Advanced/Local-Filesystem-Polling.md)). For `source: oci`, this is the full artifact reference (e.g. `ghcr.io/myorg/config:main`). |                                     |
| `reference`   | string                                        | Source revision used by deployment configs. For Git this is the branch/tag/ref (e.g. `main` or `refs/heads/main`). For [OCI](Advanced/OCI/Polling.md), the tag from `url` is used automatically when present.                                                                                                                                                                                                                                               | `refs/heads/main`                   |
| `interval`    | integer or string                             | Poll interval (min 10s). Supports integer seconds (e.g. `300`), numeric strings treated as seconds (e.g. `"300"`), and [Go duration](https://pkg.go.dev/time#ParseDuration) strings (e.g. `"5m"`, `"1m30s"`). Set to `0` or `"0s"` to disable a poll job. Mutually exclusive with `schedule`.                                                                                                                                                               | `180s` (`0` when `schedule` is set) |
| `schedule`    | string                                        | [Cron schedule](#cron-schedules) to poll at instead of a fixed `interval`, e.g. `"*/15 * * * *"` or `"@daily"`. Evaluated in the timezone of doco-cd (`TZ`). Mutually exclusive with a non-zero `interval`.                                                                                                                                                                                                                                                 | ` ` (Ignored when not specified)    |
| `target`      | string                                        | Similar to the *custom target* [webhook endpoint](Endpoints/Webhook-Listener.md#with-custom-target), used to target a specific deployment config, e.g., `#!yaml target: test` -> `.doco-cd.test.yaml`                                                                                                                                                                                                                                                       | ` ` (Ignored when not specified)    |
| `run_once`    | boolean                                       | Stop the poll job after the first run. Useful if you only want to do the first initial deployment via the poll job but do all future deployments via webhooks                                                                                                                                                                                                                                                                                               | `false`                             |
| `watch`       | boolean                                       | For local filesystem `git` sources only: watch the repository for new commits and trigger a poll immediately instead of waiting for `interval`. Set to `false` to disable and rely on `interval` only. Ignored for non-local Git and `oci` sources and when `schedule` is set. See [Polling Local Filesystem Repositories](Advanced/Local-Filesystem-Polling.md)                                                                                            | `true`                              |
| `deployments` | array of [Deploy Configs](Deploy-Settings.md) | In-line configuration for [deployment settings](Deploy-Settings.md) specific to this poll configuration. Overrides the `.doco-cd.yml` file in the target repository (`url`) if exists.<br>See the [example below](#inline-deploy-configs).                                                                                                                                                                                                                  | `[]`                                |

For Git sources, Doco-CD accepts `http://`, `https://`, `ssh://`, [`git://`](Advanced/Tips-and-Tricks.md#polling-via-native-git)), and [`file://`](Advanced/Local-Filesystem-Polling.md) URLs, plus absolute local paths.

## Cron schedules

Instead of polling every `interval`, a poll job can poll at the times given by a `schedule`.
It accepts the same [schedule formats](Advanced/Job-Scheduling.md#schedule-formats) as scheduled jobs:

- [Cron expressions](https://pkg.go.dev/github.com/robfig/cron#hdr-CRON_Expression_Format) **without** seconds (`minute hour day-of-month month day-of-week`), e.g. `"0 2 * * *"`
- [Predefined schedules](https://pkg.go.dev/github.com/robfig/cron#hdr-Predefined_schedules) like `@hourly` or `@daily`
- [Intervals](https://pkg.go.dev/github.com/robfig/cron#hdr-Intervals) like `@every 30m` (min 10s)

```yaml title="poll-config.yaml"
- url: https://github.com/example/some-repo.git
  schedule: "0 2 * * *" # every day at 02:00
- url: https://github.com/example/other-repo.git
  schedule: "*/15 8-17 * * 1-5" # every 15 minutes during business hours on weekdays
```

- The schedule is evaluated in the timezone of doco-cd, set by the `TZ` environment variable (default: UTC). Prefix it with `#!ini CRON_TZ=<timezone>` to use another timezone, e.g. `#!ini "CRON_TZ=America/New_York 0 2 * * *"`.
- A scheduled poll job does **not** poll at startup. The first poll happens at the next scheduled time. Until then, doco-cd does not [reconcile](Deploy-Settings.md#reconciliation-settings) the stacks of that repository, e.g. it does not restore a container that died after the restart.
- `schedule` and `interval` are mutually exclusive. When `schedule` is set, `interval` defaults to `0`.
- For [local filesystem repositories](Advanced/Local-Filesystem-Polling.md), the repository watcher is disabled when `schedule` is set, so polls only happen at the scheduled times.
- `run_once` ignores `schedule` and polls once at startup. Poll runs triggered via the [REST API](Endpoints/REST-API.md#polling), [MCP](Endpoints/MCP-Server.md), or a [signal](#trigger-a-poll-run-with-a-signal) also ignore it and run immediately.

!!! tip
    A poll `schedule` works well together with [sync windows](Advanced/Sync-Windows.md): schedule a poll for when an allow window opens so that deferred changes are deployed right away.

## Pausing deployments

Set [`pause`](Deploy-Settings.md#pause-settings) in the deployment configuration, not as a poll setting.
Polls keep reading the configuration while paused, so a later poll detects when the deployment is resumed.
Inline deployment definitions replace repository definitions, so their `pause` setting is authoritative. For example:

```yaml title="poll-config.yaml"
- url: https://github.com/example/app.git
  deployments:
    - name: production
      pause: true
```

## Trigger a poll run with a signal

Send `SIGHUP` or `SIGUSR1` to the doco-cd container to run all enabled poll jobs from `POLL_CONFIG` or `POLL_CONFIG_FILE` once, right away.
These commands require access to Docker, but they do not require a doco-cd API key or an open API port.

```sh
docker kill --signal=HUP doco-cd
# or, in the directory of the doco-cd Compose project
docker compose kill --signal=HUP app
```

- The signal does **not** reload `POLL_CONFIG` or `POLL_CONFIG_FILE`.
- `#!yaml interval: 0` disables a poll job unless it has a `schedule`, sets `#!yaml run_once: true`, or uses a local Git repository.
  The signal runs each enabled job once without waiting for its interval or schedule, even if a `run_once` job already ran at startup.
- The run counts as a manual deployment, the same as a poll run triggered via the [REST API](Endpoints/REST-API.md#polling).
  It can pass [sync windows](Advanced/Sync-Windows.md#manual-deployments) that set `manual_sync`, it bypasses the [retry delay and exhausted retry budget](Deploy-Settings.md#manual-retry), and it respects [`pause`](Deploy-Settings.md#pause-settings).
- If another deployment of a stack is in progress, the signal run skips that stack.
- doco-cd runs one signal-triggered poll run at a time. Signals that arrive during a run cause at most one more run.
  doco-cd ignores signals that arrive before startup completes.
- The `polling` log lines of the run show `poll-signal` as the trigger event. The [deployment run](Endpoints/REST-API.md#deployment-runs) has the trigger `poll`.
- In Docker Swarm, run `docker kill` on the node that hosts the doco-cd task. Only the task container you target receives the signal.

## Example

### With `POLL_CONFIG`

#### Using a YAML anchor

With a YAML anchor (See [Fragments](https://docs.docker.com/reference/compose-file/fragments/) and [Extensions](https://docs.docker.com/reference/compose-file/extension/) in Docker Compose), you can define the poll configuration outside the service definition.

```yaml title="docker-compose.yaml" hl_lines="1-6 20"
x-poll-config: &poll-config
  POLL_CONFIG: |
    - url: https://github.com/example/some-repo.git
    - url: https://github.com/example/public-repo.git
      reference: dev
      interval: 300  # or as a Go duration: 5m

services:
  app:
    container_name: doco-cd
    image: ghcr.io/kimdre/doco-cd:latest
    restart: unless-stopped
    ports:
      - "80:80"
    environment:
      TZ: Europe/Berlin
      GIT_ACCESS_TOKEN: xxx
      GIT_AUTH_DOMAINS: "- domains: [github.com]" # hosts that receive the token
      WEBHOOK_SECRET: xxx
      <<: *poll-config
    volumes:
      - /var/run/docker.sock:/var/run/docker.sock
      - data:/data

volumes:
  data:
```

#### Inline configuration

```yaml title="docker-compose.yaml" hl_lines="13-17"
services:
  app:
    container_name: doco-cd
    image: ghcr.io/kimdre/doco-cd:latest
    restart: unless-stopped
    ports:
      - "80:80"
    environment:
      TZ: Europe/Berlin
      GIT_ACCESS_TOKEN: xxx
      GIT_AUTH_DOMAINS: "- domains: [github.com]" # hosts that receive the token
      WEBHOOK_SECRET: xxx
      POLL_CONFIG: |
        - url: https://github.com/example/some-repo.git
        - url: https://github.com/example/public-repo.git
          reference: dev
          interval: 300  # or as a Go duration: 5m
    volumes:
      - /var/run/docker.sock:/var/run/docker.sock
      - data:/data

volumes:
  data:
```

### With `POLL_CONFIG_FILE`

```yaml title="poll-config.yaml"
- url: https://github.com/example/some-repo.git
- url: https://github.com/example/public-repo.git
  reference: dev
  interval: 300 # (1)!
```

1. Or as a Go duration: 5m

```yaml title="docker-compose.yaml" hl_lines="13 17"
services:
  app:
    container_name: doco-cd
    image: ghcr.io/kimdre/doco-cd:latest
    restart: unless-stopped
    ports:
      - "80:80"
    environment:
      TZ: Europe/Berlin
      GIT_ACCESS_TOKEN: xxx
      GIT_AUTH_DOMAINS: "- domains: [github.com]" # hosts that receive the token
      WEBHOOK_SECRET: xxx
      POLL_CONFIG_FILE: /poll-config.yaml
    volumes:
      - /var/run/docker.sock:/var/run/docker.sock
      - data:/data
      - ./poll-config.yaml:/poll-config.yaml:ro

volumes:
  data:
```

#### Using a docker compose config
```yaml title="docker-compose.yaml" hl_lines="13 17-19 24-32"
services:
  app:
    container_name: doco-cd
    image: ghcr.io/kimdre/doco-cd:latest
    restart: unless-stopped
    ports:
      - "80:80"
    environment:
      TZ: Europe/Berlin
      GIT_ACCESS_TOKEN: xxx
      GIT_AUTH_DOMAINS: "- domains: [github.com]" # hosts that receive the token
      WEBHOOK_SECRET: xxx
      POLL_CONFIG_FILE: /poll-config.yml
    volumes:
      - /var/run/docker.sock:/var/run/docker.sock
      - data:/data
    configs: # (1)!
      - source: poll-config.yml
        target: /poll-config.yml

volumes:
  data:
  
configs:
  poll-config.yml:
    content: |
      - url: https://example.com
        reference: main
      - url: https://other-example.com
        interval: 120  # Or as a Go duration: 2m
      - url: https://yet-another-example.com
        reference: dev
```

1. Use with the `POLL_CONFIG_FILE` environment variable.

### OCI Example

```yaml title="poll-config.yaml"
- source: oci
  url: ghcr.io/example/deploy-config:production
  interval: 300 # (1)!
```

1. Or as a Go duration: 5m

See more at [Polling with OCI](Advanced/OCI/Polling.md) and [OCI Artifact Usage](Advanced/OCI/Artifact-Usage.md)

### Local Filesystem Example

```yaml title="poll-config.yaml"
- source: git
  url: /local-repos/my-app
  reference: main
  interval: 60 # (1)!
```

1. Or as a Go duration: 1m

See more at [Polling Local Filesystem Repositories](Advanced/Local-Filesystem-Polling.md)

### Inline Deploy Configs

Inline deployments reuse the same fields as `.doco-cd.yml` files (See [Deploy Settings](Deploy-Settings.md)), including support for external secrets and destroy workflows. The poll job `url` is always used as the deployment source URL.

If the poll config has an inline deployment config and the target repository also contains a `.doco-cd.yml` file, the file will be ignored in favor of the inline deployment config.

Matching Git webhooks also reuse inline deployments. A poll entry matches a webhook when its repository, reference, and optional `target` match the webhook request. Short branch names such as `main` are equivalent to full branch references such as `refs/heads/main`.

!!! warning "Inline configuration takes precedence"
    When one or more inline poll entries match a webhook, their deployments are combined and the repository's `.doco-cd.yml` is ignored. Inline and repository configurations are never merged. Repository configuration discovery is used only when no inline poll entry matches.

    Deployment names must remain unique within each Docker context across all matching inline entries.

```yaml title="Poll Config with inline deploy config"
- url: https://github.com/example/app.git
  reference: refs/heads/main
  interval: 300
  deployments:
    - name: example-app
      working_dir: services/app
      compose_files:
        - compose.yaml
      env_files:
        - .env.production
```