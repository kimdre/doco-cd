---
tags:
  - Setup
  - Configuration
---

# Getting Started

???+ note "Use this [docker-compose.yml](https://github.com/kimdre/doco-cd/blob/main/docker-compose.yml) as your starting point"
    ```go title="docker-compose.yml"
    --8<-- "docker-compose.yml"
    ```

Find out about the [Core Concepts](Core-Concepts.md) of Doco-CD to understand how the application works and how to configure it.

You can find all available app settings on the [App Settings](App-Settings.md) wiki page.

If you run the application with Docker Swarm, see the [Swarm Mode](Advanced/Swarm-Mode.md) wiki page for more information.

##  Create a Git Access Token

The Git access token is used to authenticate with your Git provider (GitHub, GitLab, Gitea, etc.) and to clone or fetch your repositories via HTTP.

!!! note
    If you use an SSH URL for your Git repositories, the Git access token is not required.
    Instead, you need to generate an SSH key pair, see [Setup SSH Key](Setup-SSH-Key.md) for more information.

!!! tip
    You can use doco-cd without a Git Access Token if the repositories you want to use for your deployments are publicly accessible. 
    However, it is still recommended to use one in that case to for example avoid rate limits. 

If you set a Git access token, doco-cd will always use it to authenticate with your Git provider. See [Setup Access Token](Setup-Access-Token.md) to create this access token and set the `GIT_ACCESS_TOKEN` environment variable to the access token value.

## Deployment triggers

Doco-CD can be triggered to check for changes to deploy via webhooks or by polling the Git repositories at regular intervals. You can use either method or both methods together.

### Webhooks

Webhooks are event-based triggers that notify doco-cd when there are changes in the repositories. This is the recommended way to trigger deployments as it is more efficient and faster than polling but requires doco-cd to be reachable from the internet (or local network if you self-host your Git provider) and some setup on your Git provider.

If you want to use webhooks, you need to set the `WEBHOOK_SECRET` environment variable to a secure secret and publish the webhook port. See [Setup Webhook](Setup-Webhook.md) for more information.

### Polling

Polling is a time-based trigger that checks the repositories for changes at regular intervals. This method does not require doco-cd to be reachable from the internet but is less efficient and slower than webhooks.

If you want to use polling, you need to set a poll configuration for each repository you want to use for deployments. See [Poll Settings](Poll-Settings.md) for more information.

## Run doco-cd

After you have created the `docker-compose.yml` file, you can run doco-cd with the following command:

```sh
docker compose up -d
```

You can check the logs of the application with the following command:

```sh
docker compose logs -f
```

To be able to reach the application from external Git providers like GitHub or Gitlab, you need to expose the http endpoint of the application to the internet.
You can use a reverse proxy like [NGINX](https://www.nginx.com/), [Traefik](https://traefik.io) or [Caddy](https://caddyserver.com) for this purpose.

### Notes for Podman users

If you are using Podman instead of Docker, you may need to adjust the `docker-compose.yml` file to use the Podman socket instead of the Docker socket:

```diff title="docker-compose.yml"
services:
  app:
    ...
    volumes:
-      - /var/run/docker.sock:/var/run/docker.sock
+      - /var/run/podman/podman.sock:/var/run/docker.sock
    ...
```

## Deploy your first application

To deploy your first application, you need to configure the deployment settings in your Git repository. These settings are defined in a `.doco-cd.yml` file in the root of your repository and specify how the deployment should be performed.
See [Deploy Settings](Deploy-Settings.md) for more information on how to configure the deployment of your applications.

### Example

A simple example of a `.doco-cd.yml` file that deploys a Docker Compose application:

```yaml title=".doco-cd.yml"
name: my-app
working_dir: my-app/
compose_files: 
  - docker-compose.yml
```

## More information

### Using encrypted secrets

Doco-CD supports the encryption of sensitive data in your Git repository files with [SOPS](https://getsops.io/).

See the [Encryption](Advanced/Encryption.md) wiki page for more information on how to use SOPS with Doco-CD.

### Fetch secrets from external secret providers

Doco-CD supports fetching secrets from various external secret management providers like OpenBao, AWS Secrets Manager, Bitwarden, and many more.
See the [External Secrets](External-Secrets/index.md) wiki page for more information on how to use external secret management providers with Doco-CD.

### Pulling images from a private registry

If you want to pull images from a private registry, see [Private Container Registries](Advanced/Private-Container-Registries.md) in the wiki.

### Sending Notifications

Doco-CD supports sending notifications about deployment events to various services. See the [Notifications](Advanced/Notifications.md) wiki page for more information on how to set up notifications.

### Rest API

Doco-CD provides a REST API that allows you to interact with the application programmatically. See the [Rest API](Endpoints/REST-API.md) wiki page.

### Prometheus Metrics

Doco-CD exposes Prometheus metrics that can be used to monitor the application. See the [Prometheus Metrics](Endpoints/Metrics.md) wiki page.