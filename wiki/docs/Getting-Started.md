---
tags:
  - Setup
  - Configuration
---

# Getting Started

Doco-CD watches Git repositories and runs their Docker Compose deployments on a Docker host when a change is detected. You run Doco-CD itself as one Compose project; the applications it manages live in separate Git repositories.

## Before you start

- A host with Docker Engine running and the Docker Compose plugin available. Check with `docker --version`, `docker compose version`, and `docker info`.
- A Git repository that the host can reach. It can be public or private and hosted by GitHub, GitLab, Gitea, Forgejo, or another Git server.
- A Compose file for the application you want to deploy. You can check its syntax with `docker compose config` before handing it to Doco-CD.

The controller's `docker-compose.yml` runs Doco-CD on the host. The application repository contains a `.doco-cd.yml` deployment file and the application's Compose file. Doco-CD checks out that repository and deploys the Compose project through the Docker socket.

!!! warning "Treat the Docker host and deployment repositories as trusted"
    Mounting `/var/run/docker.sock` gives Doco-CD broad control over the Docker host. A deployment can run the Compose configuration and images specified by its Git repository, so only let repositories you trust trigger deployments on this host.

## Choose a deployment trigger

Polling and webhooks are both supported. Choose either one or use both:

| Method | How it works | What the home network needs |
|---|---|---|
| [Polling](#polling) | Doco-CD checks each configured repository on an interval. | No inbound connection to Doco-CD; the host needs outbound access to Git. |
| [Webhooks](#webhooks) | Your Git provider sends Doco-CD a request when a change is pushed. | The provider must be able to reach Doco-CD over HTTP/HTTPS. A cloud provider such as GitHub usually needs a route from the internet. |

### Webhooks

Webhooks can start a deployment shortly after a push. Set a strong `WEBHOOK_SECRET`, configure the webhook in your Git provider, and route its HTTPS request to Doco-CD's `/v1/webhook` endpoint. For a home server, put the endpoint behind a TLS-enabled reverse proxy; do not expose the metrics port or send the secret over plain HTTP.

See [Setup Webhook](Setup-Webhook.md) for provider-specific steps and [Webhook Listener](Endpoints/Webhook-Listener.md) for endpoint details.

### Polling

Polling checks repositories at a configured interval and does not require Doco-CD to be reachable from the Git provider. The simple first-deployment setup uses an interval; cron schedules are available as an advanced option. A scheduled poll waits until its next scheduled time, so use an interval when following the quickstart.

The poll configuration contains the repository URL and optional branch and interval:

```yaml title="POLL_CONFIG"
- url: https://github.com/your-user/your-deploy-repo.git
  reference: main
  interval: 180
```

See [Poll Settings](Poll-Settings.md) for `POLL_CONFIG`, `POLL_CONFIG_FILE`, scheduled polling, and other options.

## Set up Git authentication

Public repositories can be cloned without credentials. For private repositories:

- For an `https://` URL, use a token with read access to repository contents. See [Setup Access Token](Setup-Access-Token.md) for provider-specific permissions and configuration.
- For an SSH URL such as `git@github.com:owner/repo.git`, configure a deploy key or another SSH key. See [Setup SSH Key](Setup-SSH-Key.md).
- For GitHub, a GitHub App is another authentication option; see [GitHub App authentication](Git-Settings.md#github-apps).

The included Compose sample reads `GIT_ACCESS_TOKEN` from a `.env` file next to the controller's Compose file. Keep that file on the host, set restrictive permissions, and never commit plaintext credentials to Git (see [Using encrypted secrets](#using-encrypted-secrets) below):

```ini title=".env"
# Uncomment and replace for a private HTTPS repository:
# GIT_ACCESS_TOKEN=your_read_only_token
# Uncomment and replace when using webhooks:
# WEBHOOK_SECRET=your_random_webhook_secret
```

Uncomment only the values you need. Generate a webhook secret with `openssl rand -base64 32`. For a public repository, leave the Git token unset. If you use SSH, follow the SSH key guide instead. See [Git Settings](Git-Settings.md#authentication) for per-domain credentials and other authentication options.

On Linux, run `chmod 600 .env` after creating the file so other local users cannot read the credentials.

## Start Doco-CD

Create a directory on the Docker host and save the sample Compose file there:

```sh
mkdir -p ~/doco-cd
cd ~/doco-cd
curl -fsSL https://raw.githubusercontent.com/kimdre/doco-cd/main/docker-compose.yml -o docker-compose.yml
```

Edit `docker-compose.yml`, set `TZ` to your preferred time zone, and enable the trigger you chose:

- For polling, uncomment `POLL_CONFIG` and replace the sample URL and branch with your deployment repository.
- For webhooks, add `WEBHOOK_SECRET` to `.env`, uncomment its environment setting and the HTTP port mapping, then configure your HTTPS reverse proxy and Git provider. The sample's loopback port mapping is for a proxy running directly on the Docker host; a proxy in another container needs a shared Docker network.
- For both methods, configure both trigger settings.

The sample publishes no ports by default. Polling needs none. Publish the HTTP endpoint only when a webhook or API integration needs it, and expose metrics only on a trusted network if you use them. The application's own published ports are configured in its deployment Compose file and are separate from Doco-CD's ports.

Start the controller and inspect its status and logs:

```sh
docker compose up -d
docker compose ps
docker compose logs -f
```

The named `data` volume stores Doco-CD's persistent data. Keep it when updating or recreating the controller; `docker compose down -v` removes it.

!!! tip "Use a pinned version if you prefer controlled upgrades"
    Replace the `latest` tag with a release version without the leading `v` (for example, `0.124.0`). See the [available container tags](https://github.com/kimdre/doco-cd/pkgs/container/doco-cd).

### Restricting Docker access

Mounting the Docker socket grants Doco-CD broad control over the Docker host. If you prefer to restrict this, see [Docker API Permissions](Advanced/Docker-API-Permissions.md) for the endpoints Doco-CD uses and an example Docker socket proxy setup.

### Notes for Podman users

If you are using Podman instead of Docker, you may need to adjust the Compose file to use the Podman socket instead of the Docker socket:

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

Put the deployment configuration in the root of the Git repository that Doco-CD watches. The Compose file can be in the root or in a subdirectory:

```text
my-app/
├── .doco-cd.yml
└── compose.yaml
```

```yaml title=".doco-cd.yml"
name: my-app
working_dir: .
compose_files:
  - compose.yaml
```

For a simple test deployment, `compose.yaml` could contain:

```yaml title="compose.yaml"
services:
  hello:
    image: traefik/whoami:v1.10.1
    ports:
      - "127.0.0.1:8080:80"
```

The loopback address makes this test service reachable only from the Docker host at `http://127.0.0.1:8080`. Change the port binding only if you intend to make the application reachable from your LAN or through a reverse proxy.

Check the Compose file, then commit and push the deployment files to the branch configured by polling or the branch that sends webhooks:

```sh
docker compose config
git add .doco-cd.yml compose.yaml
git commit -m "Add Doco-CD deployment config"
git push
```

Polling deploys after its next interval; a webhook deploys when the provider sends the configured event. On the Docker host, use `docker compose logs -f` in the Doco-CD directory to follow deployment logs, and `docker compose ls` or `docker ps` to check that the application project is running.

If no deployment starts, check that the poll URL/branch or webhook delivery is correct, that the repository contains `.doco-cd.yml` at its root, and that `working_dir` and `compose_files` point to existing files. For an existing manually managed Compose project, read [Migrating from Docker Compose](Migrating-from-Docker-Compose.md) before the first Doco-CD deployment so project names, volumes, and bind mounts are preserved.

See [Deploy Settings](Deploy-Settings.md) for all deployment options and [Core Concepts](Core-Concepts.md) to learn how Doco-CD works.

!!! tip "Full working examples"
    The [`examples/`](https://github.com/kimdre/doco-cd/tree/main/examples) directory includes a single repository deployed to one environment, a single repository deployed to two environments, a self-updating Doco-CD instance, and a central deployments repository for many applications and Docker hosts. The [single-repo example](https://github.com/kimdre/doco-cd/tree/main/examples/single-repo-single-env) is a fuller reference; it includes Caddy and expects you to adapt its sample domain and ports.

## More information

### Git servers and authentication

Doco-CD works with GitHub, GitLab, Gitea, Forgejo, and other Git remotes. A hosting forge is not required; see [Using a plain Git server](Advanced/Tips-and-Tricks.md#using-a-plain-git-server-no-forge). For all authentication options, see [Git Settings](Git-Settings.md).

### Deploying to multiple Docker hosts

Use [Docker Contexts](Advanced/Docker-Contexts.md) to deploy to multiple independent Docker hosts without requiring Docker Swarm. For a Swarm deployment, see [Swarm Mode](Advanced/Swarm-Mode.md).

### Migrating an existing Compose project

See [Migrating from Docker Compose](Migrating-from-Docker-Compose.md) before adopting a stack that is already running on the host.

### Using encrypted secrets

Doco-CD supports encrypting sensitive data in Git with [SOPS](https://getsops.io/). See [Encryption](Advanced/Encryption.md) for setup details.

### Fetching secrets from external providers

Doco-CD supports external secret providers such as OpenBao, AWS Secrets Manager, Azure Key Vault, Bitwarden, and many more. See [External Secrets](External-Secrets/index.md) for the full list and setup instructions.

### Pulling images from a private registry

If you need to pull images from a private registry, see [Container Registry Authentication](Advanced/Container-Registry-Authentication.md).

### Self-updating Doco-CD

Current releases support self-updating from a single Doco-CD instance when explicitly enabled. The older two-instance setup remains an alternative. See [Self-Updating](Advanced/Self-Updating.md) and the [self-updating example](https://github.com/kimdre/doco-cd/tree/main/examples/self-updating).

### Scheduling and notifications

See [Job Scheduling](Advanced/Job-Scheduling.md) for scheduled jobs, [Poll Settings](Poll-Settings.md#cron-schedules) for cron-based polling, and [Sync Windows](Advanced/Sync-Windows.md) for restricting when deployments can run. Doco-CD can also send [notifications](Advanced/Notifications.md) about deployment events.

### REST API and Prometheus metrics

Doco-CD provides a [REST API](Endpoints/REST-API.md) and [Prometheus metrics](Endpoints/Metrics.md) for integrations and monitoring. Keep these endpoints private unless you specifically need to expose them.
