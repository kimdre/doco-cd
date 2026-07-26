---
tags:
  - Setup
  - Configuration
---

# Getting Started

This guide walks you through the shortest path from "I have a repository" to "doco-cd is deploying it".

If you are new to [GitOps](Core-Concepts.md#how-doco-cd-works), the idea is simple: you describe how an app should run in your Git repository, and doco-cd watches that Git repository for changes and deploys them automatically.

## Before you start

You need:

- A machine with [Docker](Core-Concepts.md#how-doco-cd-works) installed
- A Git repository for your applications
- Either:
    - a [Git access token](Setup-Access-Token.md) for HTTPS access, or
    - an [SSH key](Setup-SSH-Key.md) if you use `git@...` repository URLs

If you are new to Docker, the important part is that doco-cd runs as a container and needs access to the Docker daemon through `/var/run/docker.sock`. That is what lets it start, update, and stop your app containers.

## 1. Start with the sample Compose file

Use this [docker-compose.yml](https://github.com/kimdre/doco-cd/blob/main/docker-compose.yml) as your starting point:

```yaml title="docker-compose.yml"
--8<-- "docker-compose.yml"
```

For a first setup, you usually only need to change:

- `GIT_ACCESS_TOKEN`
- `WEBHOOK_SECRET` if you use webhooks
- the optional [poll configuration](Poll-Settings.md) if you prefer polling

!!! tip
    To use a specific version instead of `latest`, replace the tag with a release number without the leading `v`, for example `ghcr.io/kimdre/doco-cd:0.80.0`.

## 2. Set up Git access

Doco-cd needs to clone or fetch the repositories it deploys.

Use a [Git access token](Setup-Access-Token.md) if your repositories are reached over HTTPS. See that page for examples.

If you use SSH URLs such as `git@github.com:owner/repo.git`, use [SSH keys](Setup-SSH-Key.md) instead.

!!! tip
    Public repositories do not strictly need a token, but using one is still recommended to avoid rate limits.

## 3. Choose how deployments are triggered

Doco-cd can watch repositories in two ways:

### Webhooks

Webhooks send a signal when something changes. This is the fastest option, but your doco-cd instance must be reachable from your Git provider. See [Setup Webhook](Setup-Webhook.md).

### Polling

Polling checks for changes on a schedule. This is easier to start with if your instance is only on a private network or your firewall is not ready yet. See [Poll Settings](Poll-Settings.md).

If you are unsure, start with polling first and switch to webhooks later. See [Poll Settings](Poll-Settings.md) and [Setup Webhook](Setup-Webhook.md).

## 4. Run doco-cd

After you have saved the Compose file, start the container:

```sh
docker compose up -d
```

To watch the logs:

```sh
docker compose logs -f
```

If you use Podman instead of Docker, replace the Docker socket mount with the Podman socket:

```diff title="docker-compose.yml"
services:
  app:
    ...
    volumes:
-      - /var/run/docker.sock:/var/run/docker.sock
+      - /var/run/podman/podman.sock:/var/run/docker.sock
    ...
```

## 5. Tell doco-cd what to deploy

In the application repository, add a `.doco-cd.yml` file in the repository root. This file tells doco-cd which Compose file to deploy and where it lives. See [Deploy Settings](Deploy-Settings.md) for the full list of options.

```yaml title=".doco-cd.yml"
name: my-app
working_dir: my-app/
compose_files:
  - docker-compose.yml
```

## Next steps

- Read [Core Concepts](Core-Concepts.md) if you want a better mental model of how doco-cd works.
- Check [App Settings](App-Settings.md#general-settings) for all available environment variables.
- See [Swarm Mode](Advanced/Swarm-Mode.md) if you deploy Docker Swarm stacks instead of Compose apps.
