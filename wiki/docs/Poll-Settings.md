---
tags:
  - Configuration
  - Deployment
---

# Poll Settings

!!! question "About Polling"
    Polling is a time-based trigger that checks the repositories for changes to deploy at regular intervals. This method does not require doco-cd to be reachable from the internet but is less efficient and slower than webhooks.

## Configuration

Poll configurations can be set using the `POLL_CONFIG` environment variable or by providing a file with the `POLL_CONFIG_FILE` environment variable.

They must be in the format of a YAML list/array (also called YAML Sequence) and can contain the following settings:

!!! note "Settings without a default value are required."

| Key           | Type                                          | Description                                                                                                                                                                                                                                               | Default value                    |
|---------------|-----------------------------------------------|-----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|----------------------------------|
| `source`      | string                                        | Source backend for this poll job. Use `git` (default) for repositories or `oci` for [OCI artifacts](Advanced/OCI/Artifact-Usage.md).                                                                                                                      | `git`                            |
| `url`         | string                                        | Source URL. For `source: git`, this is the Git clone URL (e.g. `https://github.com/kimdre/doco-cd.git` or `git@github.com:kimdre/doco-cd.git`). For `source: oci`, this is the full artifact reference (e.g. `ghcr.io/myorg/config:main`).                |                                  |
| `reference`   | string                                        | Source revision used by deployment configs. For Git this is the branch/tag/ref (e.g. `main` or `refs/heads/main`). For [OCI](Advanced/OCI/Polling.md), the tag from `url` is used automatically when present.                                             | `refs/heads/main`                |
| `interval`    | integer or string                             | Poll interval (min 10s). Supports integer seconds (e.g. `300`), numeric strings treated as seconds (e.g. `"300"`), and [Go duration](https://pkg.go.dev/time#ParseDuration) strings (e.g. `"5m"`, `"1m30s"`). Set to `0` or `"0s"` to disable a poll job. | `180s`                           |
| `target`      | string                                        | Similar to the *custom target* [webhook endpoint](Endpoints/Webhook-Listener.md#with-custom-target), used to target a specific deployment config, e.g., `#!yaml target: test` -> `.doco-cd.test.yaml`                                                     | ` ` (Ignored when not specified) |
| `run_once`    | boolean                                       | Stop the poll job after the first run. Useful if you only want to do the first initial deployment via the poll job but do all future deployments via webhooks                                                                                             | `false`                          |
| `deployments` | array of [Deploy Configs](Deploy-Settings.md) | In-line configuration for [deployment settings](Deploy-Settings.md) specific to this poll configuration. Overrides the `.doco-cd.yml` file in the target repository (`url`) if exists.<br>See the [example below](#inline-deploy-configs).                | `[]`                             |

## Example

### With `POLL_CONFIG`

#### Using a YAML anchor

With a YAML anchor (See [Fragments](https://docs.docker.com/reference/compose-file/fragments/) and [Extensions](https://docs.docker.com/reference/compose-file/extension/) in Docker Compose), you can define the poll configuration outside the service definition.

```yaml title="docker-compose.yaml" hl_lines="1-6 19"
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
      WEBHOOK_SECRET: xxx
      <<: *poll-config
    volumes:
      - /var/run/docker.sock:/var/run/docker.sock
      - data:/data

volumes:
  data:
```

#### Inline configuration

```yaml title="docker-compose.yaml" hl_lines="12-16"
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

```yaml title="docker-compose.yaml" hl_lines="12 16"
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
```yaml title="docker-compose.yaml" hl_lines="12 16-18 23-31"
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
        branch: main
      - url: https://other-example.com
        interval: 120  # Or as a Go duration: 2m
      - url: https://yet-another-example.com
        branch: dev
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

### Inline Deploy Configs

Inline deployments reuse the same fields as `.doco-cd.yml` files (See [Deploy Settings](Deploy-Settings.md)), including support for external secrets and destroy workflows. The poll job `url` is always used as the deployment source URL.

If the poll config has an inline deployment config and the target repository also contains a `.doco-cd.yml` file, the file will be ignored in favor of the inline deployment config.

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