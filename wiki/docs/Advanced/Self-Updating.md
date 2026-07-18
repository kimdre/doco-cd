---
tags:
  - Advanced
  - Deployment
---

# Self-Updating Doco-CD

If you want doco-cd to update itself, run **two doco-cd instances** that manage each other:

- a **main** instance, which handles your application deployments and also deploys the updater instance
- an **updater** instance that updates/re-deploys the main instance

## Overview

The setup can look like this:

1. The **main** instance handles your application deployments and also deploys the updater instance.
2. The **updater** instance watches the repository with [polling](../Poll-Settings.md) and deploys the main instance via a custom target such as `updater`.
3. Scheduled jobs should usually stay on the main instance. Set [`SCHEDULER_ENABLED`](../App-Settings.md) to `false` on the **updater** instance.

## Requirements

- Both instances need access to the same Docker socket.
- Use separate container names, project/stack names, ports, and data volumes.
- Use a separate deployment config for the updater target, e.g. `.doco-cd.updater.yaml`.

## Example layout

```text
.
├── .doco-cd.yaml
├── .doco-cd.updater.yaml
└── doco-cd/
    ├── compose.main.yaml
    └── compose.updater.yaml
```

## Deployment configs

The default deployment config can manage your normal deployments and the updater instance:

```yaml title=".doco-cd.yaml"
name: doco-cd-updater
reference: main
working_dir: ./doco-cd
compose_file: compose.updater.yaml
force_recreate: true

---
name: my-app
reference: main
working_dir: ./my-app
```

The updater target should only deploy the main doco-cd instance:

```yaml title=".doco-cd.updater.yaml"
name: doco-cd
reference: main
working_dir: ./doco-cd
compose_file: compose.main.yaml
```

## Main instance

The main instance is your regular doco-cd deployment. It can receive webhooks, expose the API, and run scheduled jobs.

```yaml title="doco-cd/compose.main.yaml"
services:
  app:
    container_name: doco-cd
    image: ghcr.io/kimdre/doco-cd:latest
    restart: unless-stopped
    environment:
      TZ: Europe/Berlin
      HTTP_PORT: 8034
      GIT_ACCESS_TOKEN: ${GIT_ACCESS_TOKEN}
      WEBHOOK_SECRET: ${WEBHOOK_SECRET}
    volumes:
      - /var/run/docker.sock:/var/run/docker.sock
      - data:/data

volumes:
  data:
```

## Updater instance

The updater instance only needs enough configuration to poll the repository and deploy the `updater` target.

```yaml title="doco-cd-updater/compose.updater.yaml"
services:
  app:
    container_name: doco-cd-updater
    image: ghcr.io/kimdre/doco-cd:latest
    restart: unless-stopped
    environment:
      TZ: Europe/Berlin
      HTTP_PORT: 8000
      GIT_ACCESS_TOKEN: ${GIT_ACCESS_TOKEN}
      POLL_CONFIG: |
        - url: https://github.com/example/infrastructure.git
          reference: main
          interval: 300
          target: updater  # deploys the updater target on change
      SCHEDULER_ENABLED: false  # disable scheduler on updater instance
    volumes:
      - /var/run/docker.sock:/var/run/docker.sock
      - updater-data:/data

volumes:
  updater-data:
```

## Update flow

With the configuration above:

1. A change to `compose.updater.yaml` gets deployed by the main instance through `.doco-cd.yaml`.
2. The updater instance polls the repository.
3. When it detects a change, it applies `.doco-cd.updater.yaml`.
4. That deployment updates the main `doco-cd` instance.

## Notes

!!! warning "Multiple scheduler instances"
    If both doco-cd instances share the same Docker socket and both have the scheduler enabled, they can discover the same scheduled jobs.
    Disable the scheduler on the updater instance with [`SCHEDULER_ENABLED`](../App-Settings.md). See also [Job Scheduling](Job-Scheduling.md) for more details.

!!! tip
    Keep the updater instance as small and stable as possible. It usually only needs polling and does not need public webhook exposure.

!!! tip
    If you use multiple deployment targets in one repository, see [Deployment Settings](../Deploy-Settings.md#deployment-configuration-file) and [Poll Settings](../Poll-Settings.md).
