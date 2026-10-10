---
tags:
  - Setup
  - Configuration
---

# Getting Started

Doco-CD watches Git repositories and runs their Docker Compose deployments on a Docker host when a change is detected. You run Doco-CD itself as one Compose project; the applications it manages live in separate Git repositories.

## Before you start

- A host with Docker Engine running and the Docker Compose plugin available. Check with `docker --version`, `docker compose version`, and `docker info`.
- A Git repository that Doco-CD can reach. It can be public or private and hosted by GitHub, GitLab, Gitea, Forgejo, or another Git server; a local repository mounted into the container is also supported.
- A Compose file for the application you want to deploy. You can check its syntax with `docker compose config` before handing it to Doco-CD.

In a conventional install, the controller's `docker-compose.yml` stays on the host and each application repository contains a `.doco-cd.yml` deployment file and its Compose file. For a Git-managed, self-updating install, the controller's Compose file is in a deployment repository and a one-shot bootstrap creates the first managed instance.

!!! warning "Treat the Docker host and deployment repositories as trusted"
    Mounting `/var/run/docker.sock` gives Doco-CD broad control over the Docker host. A deployment can run the Compose configuration and images specified by its Git repository, so only let repositories you trust trigger deployments on this host.

## Choose a deployment trigger

Polling and webhooks are both supported. Choose either one or use both:

| Method                | How it works                                                       | What the home network needs                                                                                                          |
|-----------------------|--------------------------------------------------------------------|--------------------------------------------------------------------------------------------------------------------------------------|
| [Polling](#polling) | Doco-CD checks each configured repository on an interval (3 minutes by default). | No inbound connection to Doco-CD. Remote repositories need outbound access; local repositories can be mounted into the container. |
| [Webhooks](#webhooks) | Your Git provider sends Doco-CD a request when a change is pushed. | The provider must be able to reach Doco-CD over HTTP/HTTPS. A cloud provider such as GitHub usually needs a route from the internet. |

### Webhooks

Webhooks can start a deployment shortly after a push. Set a strong `WEBHOOK_SECRET`, configure the webhook in your Git provider, and route its HTTPS request to Doco-CD's `/v1/webhook` endpoint. For a home server, put the endpoint behind a TLS-enabled reverse proxy; do not expose the metrics port or send the secret over plain HTTP.

See [Setup Webhook](Setup-Webhook.md) for provider-specific steps and [Webhook Listener](Endpoints/Webhook-Listener.md) for endpoint details.

### Polling

Polling checks repositories at a configured interval and does not require Doco-CD to be reachable from the Git provider. The simple first-deployment setup uses an interval; cron schedules are available as an advanced option. A scheduled poll waits until its next scheduled time, so use an interval when following the quickstart.

If your repository is on the Docker host, Doco-CD can poll a local Git repository mounted read-only into its container; see [Polling Local Filesystem Repositories](Advanced/Local-Filesystem-Polling.md).

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

For the standalone Compose install below, the included sample reads `GIT_ACCESS_TOKEN` from a `.env` file next to the controller's Compose file. Create it only for values you need, keep it on the host, set restrictive permissions, and never commit plaintext credentials to Git (see [Using encrypted secrets](#using-encrypted-secrets) below):

```ini title=".env"
# Uncomment and replace for a private HTTPS repository:
# GIT_ACCESS_TOKEN=your_read_only_token
# Uncomment and replace when using webhooks:
# WEBHOOK_SECRET=your_random_webhook_secret
```

Uncomment only the values you need. Generate a webhook secret with `openssl rand -base64 32`. For a public repository, leave the Git token unset. If you use SSH, follow the SSH key guide instead. See [Git Settings](Git-Settings.md#authentication) for per-domain credentials and other authentication options.

On Linux, run `chmod 600 .env` after creating the file so other local users cannot read the credentials.
For the self-updating bootstrap path, the one-shot bootstrap and managed controller need separate access to private-repository credentials; see step 4 in the bootstrap instructions below.

## Start Doco-CD

Choose how you want to manage Doco-CD itself before starting it.

### Bootstrap a self-updating instance from the start

If you want Doco-CD's configuration and upgrades managed through Git, use the one-shot bootstrap on a new host. It creates the first container with the labels needed for Doco-CD to recognize and update its own stack. Starting with the standalone Compose file below and adopting self-updating later requires a migration.

Use the [Bootstrap section](Advanced/Self-Updating.md#bootstrap) together with the [self-updating example](https://github.com/kimdre/doco-cd/tree/main/examples/self-updating):

1. Create a Git repository from the contents of the example's `infra-repo/` directory. Its `.doco-cd.yml` contains separate deployment entries for Doco-CD and the sample app, with Compose files in their own directories. Copy the example's `bootstrap.sh` to the Docker host separately; it is a one-shot bootstrap tool, not part of the managed repository.
2. Replace the example repository URL in `bootstrap.sh` and in `infra-repo/doco-cd/compose.yaml`; use the same branch in both places. Set `TZ` in the managed Compose file to your local time zone. Pin the same Doco-CD release in the bootstrap script and managed Compose file. Keep the Compose image pinned by version and digest, as described in the [self-updating requirements](Advanced/Self-Updating.md#requirements).
3. Keep `SELF_UPDATE_ENABLED: "true"`, a restart policy, and a healthcheck on the managed Doco-CD service. The example leaves off `container_name` and host ports so the default `auto` strategy can use zero-downtime `scale_out`; if either is needed, `auto` uses `applier` and briefly restarts Doco-CD. See [self-update strategies](Advanced/Self-Updating.md#strategies).
4. A public repository needs no Git token. For a private repository, provide read access to both the one-shot bootstrap and the long-running controller. The example script forwards `GIT_ACCESS_TOKEN` from the host environment to the bootstrap container; configure the managed Compose service to read a host-only mounted credential file with `GIT_ACCESS_TOKEN_FILE`. The example uses HTTPS token authentication; SSH or GitHub App setups need their credentials configured for both bootstrap and the managed service. See [Git authentication](Git-Settings.md#authentication) and the [example's private-repository notes](https://github.com/kimdre/doco-cd/blob/main/examples/self-updating/README.md#caveats). Never commit credentials.
5. Run `./bootstrap.sh` on the Docker host once the repository is pushed and reachable. It creates the data volume, clones the repository, deploys Doco-CD and its configured application targets, then exits. Check `docker ps` for the running Doco-CD and app containers; the example app is available from the host at `http://127.0.0.1:8080`. To inspect Doco-CD logs later, use `docker ps` to find its container name, then run `docker logs --tail=100 <container-name>`. After a successful bootstrap, update Doco-CD by committing an image change to the repository; do not run the bootstrap script again for routine updates. If it exits with an error, correct the reported issue before retrying.

Do not run the bootstrap on a host that already has Doco-CD running. Use [Migrating an existing instance](Advanced/Self-Updating.md#migrating-an-existing-instance) instead.

### Alternative: standalone Compose controller

Use this path if you want to manage Doco-CD manually on the host rather than through Git. The controller installed this way is not self-updating; if you later want to adopt that model, follow the migration guide linked above instead of running the bootstrap.

Create a directory on the Docker host and save the sample Compose file there:

```sh
mkdir -p ~/doco-cd
cd ~/doco-cd
curl -fsSL https://raw.githubusercontent.com/kimdre/doco-cd/main/docker-compose.yml -o docker-compose.yml
```

This is the controller's Compose file; keep it separate from the application repository. It defines Doco-CD itself, its Docker socket access, and a named volume for persistent data.

??? note "Full controller Compose sample"
    The downloaded file is the same template shown here. Its polling, webhook, and port settings are commented out until you choose to enable them.

    ```yaml title="docker-compose.yml"
    --8<-- "docker-compose.yml"
    ```

Edit `docker-compose.yml`, set `TZ` to your preferred time zone, and enable the trigger you chose:

- For polling, uncomment `POLL_CONFIG` and replace the sample URL and branch with your deployment repository.
- For webhooks, add `WEBHOOK_SECRET` to `.env`, uncomment its environment setting and the HTTP port mapping, then configure your HTTPS reverse proxy and Git provider. The sample's loopback port mapping is for a proxy running directly on the Docker host; a proxy in another container needs a shared Docker network.
- For both methods, configure both trigger settings.

The sample publishes no ports by default. Polling needs none. Publish the HTTP endpoint only when a webhook or API integration needs it, and expose metrics only on a trusted network if you use them. The application's own published ports are configured in its deployment Compose file and are separate from Doco-CD's ports.

From `~/doco-cd`, check the Compose configuration and start the controller:

```sh
docker compose config --quiet
docker compose up -d
docker compose ps
docker compose logs --tail=100 -f app
```

The `-f` option continues following new log messages; press Ctrl+C to return to the shell without stopping Doco-CD. The named `data` volume stores Doco-CD's persistent data. Keep it when updating or recreating the controller; `docker compose down -v` removes it.

To update the controller when using the sample's `latest` image, pull the new image and recreate the container from the same directory:

```sh
docker compose pull
docker compose up -d
```

If you pinned a version instead, change the image tag first. Updating the image does not remove the `data` volume.

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

If you used the self-updating bootstrap above, the example repository already contains both a Doco-CD target and a sample app target. Add or edit the app under `infra-repo/app/` and push that same repository; see the [self-updating example README](https://github.com/kimdre/doco-cd/blob/main/examples/self-updating/README.md). The separate app-repository walkthrough below is for the standalone Compose controller.

Work from a local checkout of the application repository, not from `~/doco-cd`. If you do not have one yet, create an empty repository on your Git provider and clone it:

```sh
git clone https://github.com/your-user/your-deploy-repo.git
cd your-deploy-repo
```

Use the actual repository URL and branch in the Doco-CD configuration. For polling, the `reference` in `POLL_CONFIG` must name the branch you push to; the sample uses `main`.

Put `.doco-cd.yml` at the root of this Git repository. The Compose file can be in the root or in a subdirectory:

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

In this example, `name` is the Compose project name and should be unique on the Docker host. `working_dir` is relative to the repository root; `compose_files` lists the Compose files inside that directory.

Keep passwords, API keys, and other credentials out of `.doco-cd.yml` and the Git repository. See [Using encrypted secrets](#using-encrypted-secrets) for options.

For a simple test deployment, `compose.yaml` could contain:

```yaml title="compose.yaml"
services:
  hello:
    image: traefik/whoami:v1.10.1
    ports:
      - "127.0.0.1:8080:80"
```

The loopback address makes this test service reachable only from the Docker host at `http://127.0.0.1:8080`. After deployment, run `curl -i http://127.0.0.1:8080` on that host; the response should include `HTTP/1.1 200 OK` and the whoami service details. Change the port binding only if you intend to make the application reachable from your LAN or through a reverse proxy.

From the application repository root, check the Compose file, then commit and push the deployment files:

```sh
docker compose config --quiet
git add .doco-cd.yml compose.yaml
git commit -m "Add Doco-CD deployment config"
git push
```

Polling deploys on its next check; with the sample 180-second interval, a push is normally picked up within about three minutes. A webhook starts deployment after the provider sends the configured event. For the standalone controller, use `docker compose logs -f app` from `~/doco-cd`; for a bootstrapped controller, use `docker logs` as described above. Use `docker compose ls` or `docker ps` to check that the application project is running.

### Troubleshooting the first deployment

| Symptom | What to check |
|---|---|
| Nothing happens after a push | For polling, check the repository URL, branch, and interval in `POLL_CONFIG`. For webhooks, check the provider's delivery history and confirm the HTTPS route reaches `/v1/webhook`. |
| Repository clone or authentication fails | Confirm the repository is reachable from the Docker host. For a private HTTPS repository, verify that the token can read repository contents and is available to the bootstrap/managed service or, for standalone Compose, is set in `.env`; for SSH, follow the [SSH key setup](Setup-SSH-Key.md). |
| Doco-CD cannot find the deployment | Confirm `.doco-cd.yml` is at the repository root, `working_dir` names an existing directory relative to that root, and each `compose_files` entry exists inside it. |
| Compose reports an error or a port is already in use | Run `docker compose config --quiet` in the application repository to check its Compose files. Resolve any host port conflict in the app's Compose file; Doco-CD's controller ports are separate. |

For an existing manually managed Compose project, read [Migrating from Docker Compose](Migrating-from-Docker-Compose.md) before the first Doco-CD deployment so project names, volumes, and bind mounts are preserved.

See [Deploy Settings](Deploy-Settings.md) for all deployment options and [Core Concepts](Core-Concepts.md) to learn how Doco-CD works.

!!! tip "Full working examples"
    The [`examples/`](https://github.com/kimdre/doco-cd/tree/main/examples) directory includes a single repository deployed to one environment, a single repository deployed to two environments, a self-updating Doco-CD instance, and a central deployments repository for many applications and Docker hosts. The [single-repo example](https://github.com/kimdre/doco-cd/tree/main/examples/single-repo-single-env) is a fuller reference; it includes Caddy and expects you to adapt its sample domain and ports.

## More information

### Git servers and authentication

Doco-CD works with GitHub, GitLab, Gitea, Forgejo, and other Git remotes. A hosting forge is not required; see [Using a plain Git server](Advanced/Tips-and-Tricks.md#using-a-plain-git-server-no-forge). For all authentication options, see [Git Settings](Git-Settings.md).

If the clone URL advertised by your Git server is not reachable from the Doco-CD container, see [Source URL Rewrites](Advanced/Source-URL-Rewrites.md).

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

For a new host, use the bootstrap path above to make Doco-CD self-updating from the first install. The [Self-Updating guide](Advanced/Self-Updating.md) covers bootstrap, requirements, migration of existing instances, and the older two-instance alternative.

### Scheduling and notifications

See [Job Scheduling](Advanced/Job-Scheduling.md) for scheduled jobs, [Poll Settings](Poll-Settings.md#cron-schedules) for cron-based polling, and [Sync Windows](Advanced/Sync-Windows.md) for restricting when deployments can run. Doco-CD can also send [notifications](Advanced/Notifications.md) about deployment events.

### REST API and Prometheus metrics

Doco-CD provides a [REST API](Endpoints/REST-API.md) and [Prometheus metrics](Endpoints/Metrics.md) for integrations and monitoring. Keep these endpoints private unless you specifically need to expose them.
