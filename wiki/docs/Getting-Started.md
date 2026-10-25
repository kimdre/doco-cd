---
tags:
  - Setup
  - Configuration
---

# Getting Started

Doco-CD runs on your Docker host. It watches Git repositories for changes and uses Docker to deploy or update the
Compose applications described in them. Doco-CD runs in its own container, separate from the containers of the
applications it deploys.

This is how it works:

1. You keep your Compose files, plus a small `.doco-cd.yml` file, in a Git repository.
2. You push a change. Doco-CD notices it by polling the repository or through a webhook from your Git provider.
3. Doco-CD pulls the repository and updates your applications on the Docker host, much like running `docker compose up -d` yourself.

## Choose how to run Doco-CD

Pick one option and follow only its steps. Doco-CD deploys your applications the same way in both setups. The difference is how you manage Doco-CD itself:

|                                        | Option A: self-updating (recommended for a new install) | Option B: standalone                                                             |
|----------------------------------------|---------------------------------------------------------|----------------------------------------------------------------------------------|
| Where Doco-CD's own Compose file lives | In your deployment repository                           | On the Docker host                                                               |
| How you update Doco-CD                 | Commit a new image version. Doco-CD replaces itself.    | Run `docker compose pull` and `docker compose up -d`.                            |
| Setup effort                           | Edit two files and run a one-time bootstrap script      | Download and edit one Compose file                                               |
| Switching to the other option later    | Not needed                                              | Requires a [migration](Advanced/Self-Updating.md#migrating-an-existing-instance) |

Follow [Option A](#option-a-self-updating-install) or [Option B](#option-b-standalone-install) below.
Run the bootstrap in Option A only on a host where Doco-CD is not installed yet.
To convert an existing installation to self-updating, follow [the migration steps](Advanced/Self-Updating.md#migrating-an-existing-instance).

!!! tip "Short version of Option A: Self-updating install (recommended)"
    1. Create an empty Git repository on your Git provider and clone it to your computer.
    2. Copy the contents of
       [`examples/self-updating/infra-repo`](https://github.com/kimdre/doco-cd/tree/main/examples/self-updating/infra-repo)
       into the repository.
    3. Open `doco-cd/compose.yaml` in the repository. Set `url` in `POLL_CONFIG` to your repository URL, and set `TZ` to your time zone.
    4. Commit and push the repository to your Git provider.
    5. For a private repository, also complete step 3 of [Option A](#option-a-self-updating-install).
    6. On the Docker host, download [`bootstrap.sh`](https://github.com/kimdre/doco-cd/blob/main/examples/self-updating/bootstrap.sh)
       to a folder outside the repository, for example your home folder. The script is not part of your repository.
    7. In `bootstrap.sh`, set `REPO_URL` to the same repository URL as in step 3.
    8. On the Docker host, make the script executable and run it once: `chmod +x bootstrap.sh && ./bootstrap.sh`.
    9. Check that the Doco-CD container runs with `docker ps`.

    Do not run `bootstrap.sh` again after this. Doco-CD updates itself from the repository.

    The rest of this page explains each step in detail.

## Before you start

### What you need

- A Docker host with Docker Engine and the Docker Compose plugin. Check with

    ```shell
    docker --version
    docker compose version
    docker info
    ```

    `docker info` must print details about the server. If it reports `permission denied`, run the commands with `sudo`
    or add your user to the `docker` group.

- A Git repository that holds your Compose files. It can be public or private and hosted by GitHub, GitLab, Gitea,
  Forgejo, or another Git server. A local repository that you mount into the container also works.
  You need permission to push to it.

- For configuration options see [App Settings](App-Settings.md) and [Deploy Settings](Deploy-Settings.md).

### Terms used in this guide

- **Deployment repository:** The Git repository that holds your Compose files and the `.doco-cd.yml` file.
  It can be a dedicated repository or the repository that also holds the source code of your application.
- **Deployment configuration:** The `.doco-cd.yml` file in the root of the deployment repository.
  It tells Doco-CD which applications to deploy and where their Compose files are.
- **Compose project:** One application started with Docker Compose. Doco-CD deploys each entry of the
  deployment configuration as one Compose project.
- See also the [Core Concepts](Core-Concepts.md) page for an overview of Doco-CD's architecture and terminology.

!!! note "Treat the Docker host and deployment repositories as trusted"
    The default setup mounts `/var/run/docker.sock`, which gives Doco-CD broad control over the Docker host.
    A deployment runs the Compose configuration and images specified in its Git repository, so only let repositories you trust trigger deployments on this host.

    ??? tip "Limit Docker API access with a proxy"
        You can put a Docker socket proxy between Doco-CD and Docker to limit which API endpoints it can call.
        See [Docker API Permissions](Advanced/Docker-API-Permissions.md) for the required endpoints and a proxy example.
        For self-updating Doco-CD, keep the proxy in a separate Compose project; see [Self-Updating](Advanced/Self-Updating.md#requirements).

## Choose a deployment trigger

Polling and webhooks are both supported. Choose either one or use both.
If you are not sure, start with polling. It needs no network setup, and you can add webhooks later.

| Method                | How it works                                                                     | What the home network needs                                                                                                          |
|-----------------------|----------------------------------------------------------------------------------|--------------------------------------------------------------------------------------------------------------------------------------|
| [Polling](#polling)   | Doco-CD checks each configured repository on an interval (3 minutes by default). | No inbound connection to Doco-CD. Remote repositories need outbound access; local repositories can be mounted into the container.    |
| [Webhooks](#webhooks) | Your Git provider sends Doco-CD a request when a change is pushed.               | The provider must be able to reach Doco-CD over HTTP/HTTPS. A cloud provider such as GitHub usually needs a route from the internet. |

### Webhooks

Webhooks can start a deployment shortly after a push. Set a strong `WEBHOOK_SECRET`, configure the webhook in your Git provider,
and route its HTTPS request to Doco-CD's `/v1/webhook` endpoint. Put the endpoint behind a TLS-enabled reverse proxy (e.g. Traefik or Caddy).
Do not expose the metrics port or send the secret over plain HTTP.

See [Setup Webhook](Setup-Webhook.md) for provider-specific steps and [Webhook Listener](Endpoints/Webhook-Listener.md) for endpoint details.

### Polling

Polling checks repositories at a configured interval and does not require Doco-CD to be reachable from the Git provider.
Doco-CD runs the first poll right after it starts.
This guide uses an `interval`. Cron schedules are an advanced option. A scheduled poll waits until its next scheduled time,
so it does not deploy anything right after startup.

If your repository is on the Docker host, Doco-CD can poll a local Git repository mounted read-only into its container; see [Polling Local Filesystem Repositories](Advanced/Local-Filesystem-Polling.md).

The poll configuration contains the repository URL and an optional branch and interval:

```yaml title="POLL_CONFIG"
- url: https://github.com/your-user/your-deploy-repo.git
  reference: main # (1)!
  interval: 3m # (2)!
```

1. The branch (or tag) to deploy from. Doco-CD uses `main` by default.
2. The interval can be a Go duration (`3m`) or an integer value in seconds (`180`). The minimum is 10 seconds.

See [Poll Settings](Poll-Settings.md) for `POLL_CONFIG`, `POLL_CONFIG_FILE`, scheduled polling, and other options.

## Install Doco-CD

### Option A: Self-updating install

Use this option on a new host. It uses a one-time bootstrap script to create the first Doco-CD container from a Git repository,
with the labels Doco-CD needs to recognize and update its own stack.
After that, you manage Doco-CD like any other application: change its Compose file in Git and push.

!!! note "Starting with [Option B](#option-b-standalone-install) and adopting self-updating later requires a [migration](Advanced/Self-Updating.md#migrating-an-existing-instance)."

This option builds on the [self-updating example](https://github.com/kimdre/doco-cd/tree/main/examples/self-updating).
See the [Bootstrap section](Advanced/Self-Updating.md#bootstrap) of the Self-Updating guide for background.

1. **Create your deployment repository from the example.** Download the example and copy its files:

    ```sh
    git clone --depth 1 https://github.com/kimdre/doco-cd.git doco-cd-src
    cp -r doco-cd-src/examples/self-updating/infra-repo your-deploy-repo
    cp doco-cd-src/examples/self-updating/bootstrap.sh bootstrap.sh
    ```

    `your-deploy-repo/` becomes your deployment repository. It contains:

    - `.doco-cd.yml` with two entries: one for Doco-CD itself and one for a sample application.
    - `doco-cd/compose.yaml`, the Compose file of Doco-CD itself.
    - `app/compose.yaml`, a sample Nginx application.

    `bootstrap.sh` is a one-time script. Keep it outside the repository and run it later on the Docker host.
    You can delete the `doco-cd-src` folder after the copy.

    Now create an empty repository at your Git provider and note its clone URL. Do not add a README, license, or `.gitignore` yet.
    The first commit comes from the files you copied. If the provider creates a commit first, Git treats it as a separate history and rejects the initial push.
    You can add a README after the first push.

2. **Set your values.** Edit these settings:

    | File                                    | Setting                                    | Set it to                                                                                                   |
    |-----------------------------------------|--------------------------------------------|-------------------------------------------------------------------------------------------------------------|
    | `bootstrap.sh`                          | `REPO_URL`                                 | The clone URL of your deployment repository                                                                 |
    | `bootstrap.sh`                          | `reference: main` inside `POLL_CONFIG`     | Your branch, if it is not `main`                                                                            |
    | `your-deploy-repo/doco-cd/compose.yaml` | `url` and `reference` inside `POLL_CONFIG` | The same URL and branch as in `bootstrap.sh`                                                                |
    | `your-deploy-repo/doco-cd/compose.yaml` | `interval` inside `POLL_CONFIG`            | The example uses 300 seconds (5 minutes). Change it if you prefer another interval (minimum 10 seconds).    |
    | `your-deploy-repo/doco-cd/compose.yaml` | `TZ`                                       | Your [time zone](https://en.wikipedia.org/wiki/List_of_tz_database_time_zones), for example `Europe/Berlin` |

    Leave the rest as it is. The example already pins the Doco-CD version with a digest in both files.
    Keep `SELF_UPDATE_ENABLED`, the restart policy, and the healthcheck, because self-updates need them.
    Do not add `container_name` or host ports. They force a short restart for each update instead of a zero-downtime update;
    see [self-update strategies](Advanced/Self-Updating.md#strategies).

3. **Required for private repositories: give Doco-CD a Git access token.** A public repository needs no token (but it is still recommended).
    Both the one-time bootstrap container and the Doco-CD container that keeps running need the token.
    Keep the token out of the repository. Save it in a file on the Docker host that only root can read:

    ```sh
    sudo mkdir -p /opt/doco-cd
    echo 'your_read_only_token' | sudo tee /opt/doco-cd/git-access-token > /dev/null
    sudo chmod 600 /opt/doco-cd/git-access-token
    ```

    Then uncomment the three `git_access_token` blocks marked in `your-deploy-repo/doco-cd/compose.yaml`.
    They mount the file into the container and set `GIT_ACCESS_TOKEN_FILE`.
    Also uncomment `GIT_AUTH_DOMAINS` and replace `github.com` with your Git host. Doco-CD sends the token only to these hosts.
    The bootstrap script reads the token from your shell, so export it before you run the script in step 5:

    ```sh
    export GIT_ACCESS_TOKEN="$(sudo cat /opt/doco-cd/git-access-token)"
    ```

    SSH keys and GitHub Apps need their credentials configured in both places as well.
    See [Git authentication](Git-Settings.md#authentication) and the [example's notes on private repositories](https://github.com/kimdre/doco-cd/blob/main/examples/self-updating/README.md#caveats).
    To keep the token itself in Git, encrypted with SOPS or fetched from an external secret provider, see [Setup Git Access Token](Setup-Access-Token.md#setting-up-the-token).

4. **Push the repository.**

    ```sh
    cd your-deploy-repo
    git init -b main
    git add .
    git commit -m "Add Doco-CD and a sample app"
    git remote add origin https://github.com/your-user/your-deploy-repo.git
    git push -u origin main
    ```

    Use the clone URL from step 1. Git asks for your own credentials here. These are separate from Doco-CD's read-only token.

5. **Run the bootstrap on the Docker host.**

    ```sh
    chmod +x bootstrap.sh
    ./bootstrap.sh
    ```

    The script creates the data volume and starts a temporary container. That container clones your repository,
    deploys Doco-CD and the sample application, and then exits.
    If Docker needs `sudo` on your host, run `sudo --preserve-env=GIT_ACCESS_TOKEN ./bootstrap.sh` instead.
    If the script exits with an error, fix the reported problem and run it again.

6. **Check the result.**

    ```sh
    docker ps
    curl -i http://127.0.0.1:8080
    ```

    `docker ps` lists a Doco-CD container (its name contains `doco-cd`) and the sample app container.
    `curl` returns `HTTP/1.1 200 OK` and the Nginx welcome page. The sample app is bound to the loopback address,
    so it is reachable only from the Docker host itself. To read the Doco-CD logs, find the container name and follow its log:

    ```sh
    docker ps --filter name=doco-cd
    docker logs --tail=100 -f <container-name>
    ```

    A healthy start shows `polling repository` followed by `job completed successfully`.
    Press Ctrl+C to stop following the log. This does not stop Doco-CD.

From now on, do not run the bootstrap script again.
To update Doco-CD, change the image tag and digest in `doco-cd/compose.yaml` and push.
A tool such as [Renovate](Advanced/Renovate.md) can open these version updates for you.
To deploy your own applications, replace the sample app in `app/` or add more entries; see [Deploy your own applications](#deploy-your-own-applications).

!!! note "Webhooks with the self-updating install"
    The example uses polling. To add webhooks, set `WEBHOOK_SECRET_FILE` in `doco-cd/compose.yaml` the same way as the token file,
    and route the webhook requests through a reverse proxy that shares a Docker network with Doco-CD.
    If you publish a host port for Doco-CD instead, each Doco-CD update causes a short restart; see [self-update strategies](Advanced/Self-Updating.md#strategies).

!!! warning "Do not run the bootstrap on a host that already has Doco-CD running."
    [Migrate the existing instance](Advanced/Self-Updating.md#migrating-an-existing-instance) instead.

### Option B: Standalone install

Use this option if you prefer to keep Doco-CD's Compose file on the Docker host and update Doco-CD yourself.
In this setup, Doco-CD does not update its own Compose project.

!!! note "If you want self-updating later, follow the [migration guide](Advanced/Self-Updating.md#migrating-an-existing-instance). Do not run the bootstrap."

#### Step 1: Create the deployment repository

Create an empty repository on your Git provider and clone it on any machine that has Git.
Do this before you start Doco-CD. Doco-CD polls the repository as soon as it starts, so an empty repository makes the first poll fail.

```sh
git clone https://github.com/your-user/your-deploy-repo.git
cd your-deploy-repo
```

Add a `.doco-cd.yml` file to the root of the repository:

```yaml title=".doco-cd.yml"
name: my-app # (1)!
```

1. The name of the Compose project. Use lowercase letters, numbers, hyphens, or underscores, and make it unique on the Docker host.
   Choose it with care. Docker cannot rename a project later; see [Known Limitations](Known-Limitations.md#renaming-projects).

By default, Doco-CD looks for `compose.yaml`, `compose.yml`, `docker-compose.yml`, or `docker-compose.yaml` in the root of the repository.
If your Compose file is in a subdirectory, add `working_dir: <directory>`. To use other file names, add `compose_files`.
See [Deploy Settings](Deploy-Settings.md) for all options. The repository then looks like this:

```tree
your-deploy-repo/
    .doco-cd.yml
    compose.yaml
```

For a simple test deployment, `compose.yaml` can contain:

```yaml title="compose.yaml"
services:
  hello:
    image: traefik/whoami:v1.10.1
    ports:
      - "8080:80"
```

If Docker Compose is installed on this machine, check the Compose file with `docker compose config --quiet`.
Then commit and push both files:

```sh
git add .doco-cd.yml compose.yaml
git commit -m "Add Doco-CD deployment config"
git branch -M main
git push -u origin main
```

Git asks for your own credentials here. These are separate from Doco-CD's read-only token.
Doco-CD polls the branch named in `POLL_CONFIG` (the sample uses `main`), so push to that branch.

#### Step 2: Install Doco-CD on the Docker host

Create a directory on the Docker host and save the sample Compose file there.
This file lives on the host. Do not put it in your deployment repository.

```sh
mkdir -p ~/doco-cd
cd ~/doco-cd
curl -fsSL https://raw.githubusercontent.com/kimdre/doco-cd/main/docker-compose.yml -o docker-compose.yml
```

The file defines Doco-CD itself, its Docker socket access, and a named volume for persistent data.

??? note "Full Doco-CD Compose file"
    The downloaded file is the same template shown here. Its polling, webhook, and port settings are commented out until you choose to enable them.

    ```yaml title="docker-compose.yml"
    --8<-- "docker-compose.yml"
    ```

Edit `docker-compose.yml`, set `TZ` to your preferred time zone, and enable the trigger you chose:

- For polling, uncomment `POLL_CONFIG` and replace the sample URL and branch with your deployment repository.
- For webhooks, add `WEBHOOK_SECRET` to `.env`, uncomment its environment setting and the HTTP port mapping, then configure your HTTPS reverse proxy and Git provider. The sample's loopback port mapping is for a proxy running directly on the Docker host. A proxy in another container needs a shared Docker network.
- To use both methods, enable both settings.

The sample publishes no Doco-CD ports by default. Polling needs none. Publish Doco-CD's HTTP endpoint only when a webhook or API integration needs it, and expose metrics only on a trusted network if you use them.
The ports of your applications are separate. They are set in the Compose files of your deployment repository.

If you need a `.env` file for credentials, create it now as described in [Set up Git authentication](#set-up-git-authentication).
Then check the configuration and start Doco-CD:

```sh
docker compose config --quiet
docker compose up -d
docker compose ps
docker compose logs --tail=100 -f app
```

The `-f` option follows new log messages. Press Ctrl+C to return to the shell without stopping Doco-CD.

#### Step 3: Check that it works

- `docker compose ps` shows the `app` service as `healthy` within about a minute. It first shows `starting`.
- The log shows `polling repository` followed by `job completed successfully`.
- `docker compose ls` lists the project `my-app` as running.
- On the Docker host, `curl -i http://127.0.0.1:8080` returns `HTTP/1.1 200 OK` and the details of the whoami service.

Polling deploys on its next check. Doco-CD also polls right after it starts, so the application is usually up within a minute or two.
Later pushes are picked up within the poll interval (3 minutes with the sample).
A webhook starts a deployment after the provider sends the configured event.

#### Update Doco-CD

If you use the sample's `latest` image, pull the new image and recreate the container from `~/doco-cd`:

```sh
docker compose pull
docker compose up -d
```

If you pinned a version instead, change the image tag first in the compose file, then run the same commands. 
The container restarts with the new version.

!!! tip "Recommended: Use a pinned version to control upgrades"
    It is safer to pin a specific version of Doco-CD instead of using `latest`. 
    This avoids unexpected changes when a new image is published.
    Replace the `latest` tag with a release version without the leading `v` (for example, `0.125.1`).  
    See the [available container tags](https://github.com/kimdre/doco-cd/pkgs/container/doco-cd).

#### Notes for Podman users

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

## Set up Git authentication

Public repositories can be cloned without credentials. For private repositories:

- For an `https://` URL, use a token with read access to repository contents. See [Setup Access Token](Setup-Access-Token.md) for provider-specific permissions and configuration.
- For an SSH URL such as `git@github.com:owner/repo.git`, configure a deploy key or another SSH key. See [Setup SSH Key](Setup-SSH-Key.md).
- For GitHub, a GitHub App is another authentication option; see [GitHub App authentication](Git-Settings.md#github-apps).

Never commit plaintext credentials to Git. See [Using encrypted secrets](#using-encrypted-secrets) and [Fetching secrets from external providers](#fetching-secrets-from-external-providers) for alternatives.

How you pass the credentials to Doco-CD depends on the option you chose:

- **Option A:** Use a token file as described in [step 3 of Option A](#option-a-self-updating-install).
- **Option B:** The sample Compose file reads `GIT_ACCESS_TOKEN` and `WEBHOOK_SECRET` from a `.env` file next to it.
  Create that file with only the values you need:

    ```ini title=".env"
    # For a private HTTPS repository:
    GIT_ACCESS_TOKEN=your_read_only_token
    GIT_AUTH_DOMAINS=- domains: [github.com]
    # For webhooks:
    WEBHOOK_SECRET=your_random_webhook_secret
    ```

    Generate a secure webhook secret with `openssl rand -base64 32`. On Linux, run `chmod 600 .env` so other local users cannot read it.
    If you use SSH, follow the [SSH key setup](Setup-SSH-Key.md) instead.

!!! note "This `.env` file is only for Doco-CD itself"
    It sits next to Doco-CD's own Compose file on the Docker host. It is not the `.env` file that Doco-CD can read from your deployment repository for your applications.

See [Git Settings](Git-Settings.md#authentication) for per-domain credentials and other authentication options.

## Troubleshooting the first deployment

| Symptom                                                   | What to check                                                                                                                                                                                                                                                                 |
|-----------------------------------------------------------|-------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| Nothing happens after a push                              | For polling, check the repository URL, branch, and interval in `POLL_CONFIG`, then wait for the next interval. For webhooks, check the provider's delivery history and confirm the HTTPS route reaches `/v1/webhook`.                                                         |
| Repository clone or authentication fails                  | Confirm the repository is reachable from the Docker host. For a private HTTPS repository, check that the token can read repository contents and that Doco-CD received it (Option A: the token file; Option B: `.env`). For SSH, follow the [SSH key setup](Setup-SSH-Key.md). |
| The log says `configuration file not found in repository` | Confirm `.doco-cd.yml` is in the root of the repository and that you pushed it to the branch Doco-CD polls. If your Compose file is in a subdirectory, check that `working_dir` names it relative to the repository root.                                                     |
| Compose reports an error or a port is already in use      | Run `docker compose config --quiet` in your deployment repository to check its Compose files. Resolve any host port conflict in the application's Compose file. Doco-CD's own ports are separate from the ports of your applications.                                         |
| The Doco-CD container restarts or stays `unhealthy`       | Read its log. Doco-CD checks its settings at startup and names the setting it cannot use, for example `POLL_CONFIG` with invalid YAML, both `GIT_ACCESS_TOKEN` and `GIT_ACCESS_TOKEN_FILE` set, or a token without `GIT_AUTH_DOMAINS`.                                     |
| The bootstrap script fails (Option A)                     | Read the error that the script prints. Check the repository URL, the branch, and the token. Fix the cause and run the script again.                                                                                                                                           |

For more detail in the log, set `LOG_LEVEL: debug` in Doco-CD's environment. See [App Settings](App-Settings.md) for all settings.

## Deploy your own applications

Once the first deployment works, move your own applications into the deployment repository.

!!! warning "Keep persistent data out of the repository folder"
    Doco-CD deploys every revision from its own copy of the repository. Data that an application writes into a relative bind mount such as `./data:/data` is lost when the next revision is deployed.
    Use named volumes or absolute host paths for data that must stay, for example `/srv/my-app/config:/config`. See [Artifact Storage](Reference/Artifact-Storage.md).

- **Add more applications:** Put each application in its own directory with its own Compose file.
  Then list them in `.doco-cd.yml` as separate YAML documents divided by `---`, or let Doco-CD find them with auto-discovery.
  See [Multiple service deployments](Deploy-Settings.md#multiple-service-deployments) and [Auto-Discovery](Deploy-Settings.md#auto-discovery).
- **Move an application that is already running:** Read [Migrating from Docker Compose](Migrating-from-Docker-Compose.md) first. It explains how to keep project names, volumes, and bind mounts.
- **Keep secrets out of Git:** Do not commit passwords or API keys in plaintext. Use [SOPS encryption](#using-encrypted-secrets) or [external secrets](#fetching-secrets-from-external-providers).
- **Remove an application:** Deleting its files from Git does not remove its containers. Use the [destroy settings](Deploy-Settings.md#destroy-settings).

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

The [Self-Updating guide](Advanced/Self-Updating.md) covers bootstrap, requirements, update strategies, migration of existing instances, and the older two-instance alternative.

### Scheduling and notifications

See [Job Scheduling](Advanced/Job-Scheduling.md) for scheduled jobs, [Poll Settings](Poll-Settings.md#cron-schedules) for cron-based polling, and [Sync Windows](Advanced/Sync-Windows.md) for restricting when deployments can run. Doco-CD can also send [notifications](Advanced/Notifications.md) about deployment events.

### REST API and Prometheus metrics

Doco-CD provides a [REST API](Endpoints/REST-API.md) and [Prometheus metrics](Endpoints/Metrics.md) for integrations and monitoring. Keep these endpoints private unless you specifically need to expose them.
