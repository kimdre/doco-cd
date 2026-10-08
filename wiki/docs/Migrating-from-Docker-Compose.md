---
tags:
  - Setup
  - Deployment
---

# Migrating from Docker Compose

This guide helps you move stacks currently managed manually, through SSH, or with Ansible to Doco-CD.
Read [Core Concepts](Core-Concepts.md) and [Getting Started](Getting-Started.md) first.
This page focuses on the migration itself.

## What changes

| Topic                    | Before                                                           | After                                                                                                                                              |
|--------------------------|------------------------------------------------------------------|----------------------------------------------------------------------------------------------------------------------------------------------------|
| Who runs `compose up`    | You, over SSH or from a CI job                                   | Doco-CD, from the [deployment config](Deploy-Settings.md#deployment-configuration-file)                                                             |
| Where compose files live | A directory on the host, e.g. `/opt/stacks/app`                  | Git, checked out per revision into the [artifact storage](Reference/Artifact-Storage.md)                                                            |
| Where secrets live       | Plaintext `.env` next to the compose file                        | [SOPS-encrypted](Advanced/Encryption.md) in Git, an [external provider](External-Secrets/index.md), or a host path outside the repository           |
| How a change ships       | Edit files on the host, run `docker compose up -d`               | Push to Git, a [webhook](Core-Concepts.md#webhook) or [poll](Core-Concepts.md#polling) triggers the deployment                                      |
| Drift                    | A dead container or a hand-edited file stays that way            | [Reconciliation](Deploy-Settings.md#reconciliation-settings) reacts to container events, the next deployment overwrites files edited on the host    |
| Periodic tasks           | Host `crontab` calling `docker exec` or `docker run`             | [`cd.doco.job.*`](Advanced/Job-Scheduling.md#configuration) service labels                                                                         |
| Project name             | Derived from the directory Compose ran in                        | The `name` field of the deployment config                                                                                                          |

## 1. Inventory the host

Do this before you change anything.
The goal is one written record per stack.

1. List every Compose project the daemon knows, including stopped ones.

    ```sh
    docker compose ls -a
    ```

2. Map containers to their project, working directory and compose files.

    ```sh title="Compose labels per container"
    docker ps -a --format '{{.Names}}' | while read -r c; do
      docker inspect "$c" --format '{{.Name}}
      project={{index .Config.Labels "com.docker.compose.project"}}
      working_dir={{index .Config.Labels "com.docker.compose.project.working_dir"}}
      config_files={{index .Config.Labels "com.docker.compose.project.config_files"}}'
    done
    ```

3. List every bind mount, so you can separate paths that will move from paths that will not.

    ```sh title="Bind mounts per container"
    docker ps -aq | xargs -I{} docker inspect {} \
      --format '{{.Name}}{{range .Mounts}}{{if eq .Type "bind"}} {{.Source}} -> {{.Destination}}{{end}}{{end}}'
    ```

4. Write down per stack:

    - [ ] Compose project name, exactly as `docker compose ls -a` prints it.
    - [ ] Compose file(s) and override files, in the order they are applied.
    - [ ] `.env` and every `env_file`.
    - [ ] Services that set `container_name`.
    - [ ] `restart` policies.
    - [ ] Networks shared with other stacks.
    - [ ] Named volumes and bind mounts, relative and absolute.
    - [ ] Image tags: floating (`latest`) or pinned.
    - [ ] Host crons, systemd units and scripts that touch these containers.
    - [ ] Secrets on disk and who reads them.
    - [ ] Published ports.
    - [ ] Containers started by hand with `docker run`, outside Compose.

## 2. Decide the repository layout

| Layout                                                                                                          | Use when                                          | Cost                                                            |
|-----------------------------------------------------------------------------------------------------------------|---------------------------------------------------|-----------------------------------------------------------------|
| One repository per host                                                                                         | Hosts are unrelated, blast radius must stay small | Shared compose snippets get duplicated                          |
| One repository, one [target](Deploy-Settings.md#multiple-deployment-targets) per host (`.doco-cd.<target>.yml`) | Many hosts, mostly the same stacks                | Every host sees every commit, `target` must be set per instance |
| One repository per team or per blast radius                                                                     | Access control follows teams                      | A stack that moves between teams moves between repositories     |

Inside a repository, give each stack its own directory and list them as separate YAML documents in one
[deployment config](Deploy-Settings.md#multiple-service-deployments).

```yaml title=".doco-cd.yml"
name: proxy
working_dir: proxy
---
name: app
working_dir: app
env_files:
  - .env
  - prod.env
```

!!! warning "The project name is the adoption key"
    `name` becomes the Compose project name.
    It must match the project name the stack runs under today, otherwise Doco-CD creates a second set of containers next to the old ones.
    Renaming a deployed project later is [not possible](Known-Limitations.md#renaming-projects).

Shared networks: declare them `#!yaml external: true` in every stack and create them once on the host, or own them in one small bootstrap stack.

```yaml title="app/docker-compose.yml"
networks:
  edge:
    external: true
```

Image tags:

- Doco-CD deploys what Git says.
- A floating tag such as `latest` in Git does not redeploy when the registry moves, unless you set [`force_image_pull`](Deploy-Settings.md#available-settings) or you pin it to a digest (see below).
- A tag such as `1.4.2` is a readable registry label, but a publisher can move it to a different image. A digest is a SHA-256 identifier for an image manifest that selects a specific image.
- To find a digest, run `docker buildx imagetools inspect ghcr.io/example/app:1.4.2` or copy it from your registry's image details. Add the reported digest after `@` in the Compose `image` value:

    ```yaml title="app/docker-compose.yml"
    services:
      app:
        image: ghcr.io/example/app:1.4.2@sha256:<digest>
    ```

    Replace `<digest>` with the 64-character value reported after `sha256:`. 
    Keep the tag and digest together when upgrading. [Renovate](Advanced/Renovate.md) can open pull requests with updated image references.

Keep compose files identical across environments.
Put the host-specific values in `environment` and `env_files` of the deployment config instead.
Both only feed Compose variable interpolation, nothing reaches a container by itself.
The Compose service still needs `environment:` or `env_file:` entries that reference the values:

```yaml title=".doco-cd.yml"
name: app
environment:
  APP_IMAGE_TAG: "1.4.2"
  APP_LOG_LEVEL: info
```

```yaml title="app/docker-compose.yml"
services:
  app:
    image: ghcr.io/example/app:${APP_IMAGE_TAG}
    environment:
      LOG_LEVEL: ${APP_LOG_LEVEL}
```

## 3. Fix paths and data before the first deploy

Every revision is served from its own immutable artifact directory, so a relative bind mount resolves to a path that changes with the revision.

!!! danger "Never keep persistent data behind a relative bind mount"
    Data a container writes into the [artifact storage](Reference/Artifact-Storage.md) is lost as soon as the service is deployed from a new revision.
    Move it to a named volume or an absolute host path before the first Doco-CD deployment.

| Mount today                              | Do this                                                                                                                                                    |
|------------------------------------------|------------------------------------------------------------------------------------------------------------------------------------------------------------|
| Relative bind mount holding data         | Convert to a named volume or an absolute host path.                                                                                                        |
| Relative bind mount holding config       | Keep it. The service is recreated when the file content changes, or add [`recreate.ignore`](Deploy-Settings.md#prevent-recreation-on-config-secret-or-bind-mount-changes) plus a [signal](Deploy-Settings.md#send-signal-on-ignored-recreation) for a reload in place. |
| Absolute host path                       | Unchanged, the data survives.                                                                                                                              |
| Named volume                             | Survives while project name and volume name stay the same. Set `#!yaml name:` on the volume to decouple it from the project name.                          |
| Relative `env_file`                      | Keep it, it is read from the artifact of the revision.                                                                                                     |

```yaml title="db/docker-compose.yml"
services:
  db:
    image: ghcr.io/example/db:1.2.3
    volumes:
      - db-data:/var/lib/db        # named volume, survives
      - /srv/backups:/backups      # absolute host path, survives
      - ./initdb:/initdb:ro        # relative, read-only config

volumes:
  db-data:
    name: app-db-data
```

!!! danger "Copy the data before you change the mount"
    A new named volume or a new absolute path starts empty.
    A service deployed against it runs with empty state while the old files stay at the old path.

Move data from a relative bind mount, per service:

1. Stop the service: `docker compose stop db`.
2. Make a backup and verify it, e.g. `tar -C /opt/stacks/db -czf /srv/backups/db-data.tgz data && tar -tzf /srv/backups/db-data.tgz > /dev/null`.
3. Copy the data, keeping ownership and permissions.

    ```sh title="Into an absolute host path"
    cp -a /opt/stacks/db/data/. /srv/db-data/
    ```

    ```sh title="Into a named volume"
    docker volume create app-db-data
    docker run --rm -v /opt/stacks/db/data:/from:ro -v app-db-data:/to alpine cp -a /from/. /to/
    ```

4. Verify the copy: `diff -r /opt/stacks/db/data /srv/db-data`, or compare a named volume recursively:

    ```sh title="Verify a named-volume copy"
    docker run --rm \
      -v /opt/stacks/db/data:/from:ro \
      -v app-db-data:/to:ro \
      alpine diff -r /from /to
    ```

    No output and an exit status of `0` mean the directories match.
5. Point the compose file at the new volume or path, then trigger Doco-CD.
6. Delete the old directory only after the service ran on the new mount.

Env files:

- `env_files` are parsed with the same dotenv engine as Docker Compose, see [Dotenv File Format](Deploy-Settings.md#dotenv-file-format).
- Audit existing env files for a literal `$`, e.g. in password hashes: the engine interpolates `${VAR}`, so quote the value with single quotes or write `$$`.
- If a stack uses `include` with a remote compose file, set `#!yaml project_directory: .`, see [Resolving `.env` and other relative paths](Advanced/Tips-and-Tricks.md#resolving-env-and-other-relative-paths).

Secrets, never commit plaintext:

| Option                                                                                   | Notes                                                                                          |
|------------------------------------------------------------------------------------------|------------------------------------------------------------------------------------------------|
| [SOPS with age](Advanced/Encryption.md#usage-with-sops-and-age)                           | One key on the controller, files encrypted in Git.                                             |
| [SOPS with a cloud key service](Advanced/Encryption.md#usage-with-sops-and-cloud-key-services) | No key on disk. At least one `SOPS_*` environment variable must still be set as a marker.  |
| [External secret providers](External-Secrets/index.md)                                    | Values are fetched at deployment time and never stored in Git.                                  |
| `PASS_ENV`                                                                                | Passes the controller's own environment into interpolation, see [App Configuration](Deploy-Settings.md#app-configuration). Use with care. |
| A file under an absolute host path outside the repository                                 | Simplest, but not covered by Git history.                                                      |

## 4. Install the controller

Take the base `docker-compose.yml` from [Getting Started](Getting-Started.md), then decide:

- [ ] Trigger: [polling](Core-Concepts.md#polling) needs no inbound port, [webhooks](Core-Concepts.md#webhook) are faster. Both can run together.
- [ ] Docker access: the socket grants full control, a [socket proxy](Advanced/Docker-API-Permissions.md) restricts it. With a proxy or a remote daemon, set [`DATA_HOST_PATH`](Docker-Settings.md#remote-docker-daemons).
- [ ] Data mount: back `/data` with a named volume or a host path. It holds the artifacts and the decrypted secrets, see [`DATA_MOUNT_PATH`](App-Settings.md#storage-settings).
- [ ] Pin the Doco-CD image tag instead of `latest`, and keep `#!yaml restart: unless-stopped`.
- [ ] Keep the image's [health check](Endpoints/Healthcheck.md) (`#!yaml test: ["CMD", "/doco-cd", "healthcheck"]`).
- [ ] Set `#!yaml LOG_LEVEL: debug` for the first deployments, see [Runtime Settings](App-Settings.md#runtime-settings).
- [ ] Set `target` per host if you use one repository with several targets, so this instance only reads its own config file.

```yaml title="poll-config.yaml"
- url: https://git.example.com/example/deployments.git
  reference: refs/heads/main
  interval: 3m
  target: prod # reads .doco-cd.prod.yml
```

The file is only read when `POLL_CONFIG_FILE` names it and it is mounted into the container:

```yaml title="docker-compose.yml" hl_lines="5 8"
services:
  app:
    image: ghcr.io/kimdre/doco-cd:latest
    environment:
      POLL_CONFIG_FILE: /poll-config.yaml
    volumes:
      - /var/run/docker.sock:/var/run/docker.sock
      - ./poll-config.yaml:/poll-config.yaml:ro
      - data:/data

volumes:
  data:
```

See [With `POLL_CONFIG_FILE`](Poll-Settings.md#with-poll_config_file) for the full example, [Poll Settings](Poll-Settings.md#configuration) for the field list, and [Local Filesystem Polling](Advanced/Local-Filesystem-Polling.md) if the repository lives on the same host.

## 5. Cut a stack over

One stack at a time, least critical first.

1. Check the preconditions.

    - [ ] The project renders from the repository: `docker compose -f app/docker-compose.yml config`.
      This local command does not read `.doco-cd.yml`. If its `environment` or
      `env_files` provide Compose interpolation values, supply the same values
      in your shell or with Docker Compose's `--env-file` option. Otherwise,
      this check can resolve values differently from Doco-CD.
    - [ ] `name` equals the running Compose project name.
    - [ ] No `container_name` collides with another project. Container names are unique per Docker host: `docker ps -a --format '{{.Names}}'`.
    - [ ] Data mounts fixed as in [section 3](#3-fix-paths-and-data-before-the-first-deploy).
    - [ ] Secrets reachable by the controller.
    - [ ] `remove_orphans` defaults to `true`, so containers of that project which are not in the compose files are removed.

2. Pick the adoption path.

    | Situation                                     | Steps                                                                                                                                                                                     |
    |-----------------------------------------------|-------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
    | Same project name                             | Commit and let Doco-CD deploy. Named volumes and absolute host paths are kept.                                                                                                            |
    | Different project name                        | Pin every named volume to its existing name with `#!yaml name:` (`docker volume ls`), run `docker compose down` **without** `-v` on the old project, then deploy.                         |
    | No Compose project, started with `docker run` | There is no project for `docker compose down` to remove. Pin the volumes as above, then `docker stop <container>` and `docker rm <container>` **without** `-v` for each one, then deploy. |

    !!! warning
        Skipping the stop and remove in the last two cases leaves two sets of containers that fight over ports, names and volumes.
        `docker rm` without `-v` keeps every volume on disk, named and anonymous, `docker rm -v` removes the anonymous ones.

    !!! warning "Anonymous volumes are not reused"
        A volume with a 64 character hex name belongs to one container only.
        The replacement Compose service gets a fresh volume, so the data looks gone.
        Find them before the cutover:

        ```sh
        docker inspect <container> --format '{{range .Mounts}}{{if eq .Type "volume"}}{{.Name}} {{.Destination}}{{println}}{{end}}{{end}}'
        ```

        Then either declare a named volume in the compose file with `#!yaml name:` set to that hex name, or copy the content into a new named volume as in [section 3](#3-fix-paths-and-data-before-the-first-deploy).

3. Trigger the deployment: wait for the poll interval, push a commit to a configured webhook, or call the API.

    ```sh
    curl --request POST \
      --url 'https://cd.example.com/v1/api/poll/run?wait=true' \
      --header 'content-type: application/json' \
      --header 'x-api-key: your-api-key' \
      --data '[{"url": "https://git.example.com/example/deployments.git", "target": "prod"}]'
    ```

    See [REST API](Endpoints/REST-API.md#polling) for the request body and [Authentication](Endpoints/REST-API.md#authentication) for `API_SECRET`.
    The API uses the `target` in this request body and ignores the mounted poll config. 
    Use the target configured for this host, or omit it only when deploying `.doco-cd.yml`.

4. Verify.

    - [ ] `docker compose ls` shows the project.
    - [ ] The containers carry the Doco-CD labels: `docker inspect <container> --format '{{index .Config.Labels "cd.doco.deployment.name"}}'`.
    - [ ] `cd.doco.deployment.target.sha` matches the commit you pushed.
    - [ ] The controller log shows the deployment finishing.
    - [ ] A [notification](Advanced/Notifications.md) arrived, if configured.

    !!! note "What gets recreated on adoption"
        Plan for every service of the stack to be recreated on the first deployment, and pick the time window accordingly.
        Services with relative bind mounts or relative `env_file` entries are always recreated, because those host paths move into the artifact storage.
        Any other service is recreated when its resolved configuration differs from the running container.
        From the second deployment on, a service stays on its old artifact while its files are unchanged, see [Unchanged services](Reference/Artifact-Storage.md#unchanged-services).

5. Confirm the second trigger is a no-op.

    Nothing should be recreated.
    Check the run status or the `pre-deploy` stage outcome:

    ```sh title="Recent skipped runs"
    curl --header 'x-api-key: your-api-key' \
      'https://cd.example.com/v1/api/runs?status=skipped&limit=20'
    ```

    The same shows up in [Prometheus metrics](Endpoints/Metrics.md) as
    `#!ini doco_cd_deployment_stage_duration_seconds{stage="pre-deploy",outcome="skipped"}`.

6. Know how to roll back.

    Push a revert commit, that is the normal path.

    !!! warning "Moving the branch back to an older commit is skipped"
        When the revision a run resolves to is an ancestor of the commit that is already deployed, Doco-CD treats the run as stale and skips it, so a newer state is never silently reverted.
        Use a revert commit, or set [`force_recreate`](Deploy-Settings.md#available-settings), which bypasses that guard.
        Remove `force_recreate` again after the rollback deployment, otherwise every following deployment and poll recreates the services.

    Keep the old compose files and env files on the host until the stack has survived one normal change.

7. Remove the leftovers on the host.

    - [ ] The old compose directory.
    - [ ] systemd units and scripts running `docker compose up`.
    - [ ] Host crons for this stack, see [section 6](#6-replace-crons-scripts-and-helpers).
    - [ ] Deploy keys and SSH access of the old pipeline.

## 6. Replace crons, scripts and helpers

| You had                                                | Use instead                                                                                                                                                               |
|--------------------------------------------------------|---------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| Host cron running `docker exec` or `docker run`         | [Scheduled job](Advanced/Job-Scheduling.md) labels on the service: `cd.doco.job.enabled` and `cd.doco.job.schedule`.                                                       |
| A backup cron that needs the app stopped                | [`cd.doco.job.stop_services`](Advanced/Job-Scheduling.md#temporarily-stop-services-during-a-job-run), which takes `service` or `project/service`.                          |
| `docker system prune -a` cron                           | `docker image prune`, plus the built-in [artifact garbage collection](Reference/Artifact-Storage.md#garbage-collection).                                                   |
| Deploy scripts running migrations                       | Init containers, sidecars or Compose lifecycle hooks, see [Pre- / Post-Deployment Scripts](Advanced/Pre-Post-Deployment-Scripts.md). Doco-CD has no shell.                 |
| Watchtower-style tag watching                           | Pinned tags plus [Renovate](Advanced/Renovate.md), so the change is a commit.                                                                                              |
| An autoheal container                                   | [Reconciliation](Deploy-Settings.md#reconciliation-settings) on the `unhealthy`, `die` or `oom` events.                                                                    |
| systemd unit running `docker compose up -d` at boot     | Compose `restart` policies. A poll job with an `interval` also polls at startup, see [Cron schedules](Poll-Settings.md#cron-schedules).                                    |
| `docker compose up` after editing files on the host     | Nothing. Commit the change instead, the next deployment overwrites host edits.                                                                                            |

Scheduled jobs have rules worth knowing before you convert a cron:

- The service `restart` policy must be unset or `no` in standalone Compose, see [Configuration](Advanced/Job-Scheduling.md#configuration).
- A job never runs as a side effect of a deployment, see [Execution modes](Advanced/Job-Scheduling.md#execution-modes).
- In [`restart`](Advanced/Job-Scheduling.md#restart) mode the container is created but not started, so it sits in `created` between runs.

!!! warning "Pruning removes idle job containers"
    A prune that removes stopped containers also removes the idle `created` containers of `restart` mode jobs.
    Prune images only, or scope the prune with filters.

## 7. After the migration

- [ ] Turn on [notifications](Advanced/Notifications.md) so a failed deployment is not silent.
- [ ] Scrape the [Prometheus metrics](Endpoints/Metrics.md) endpoint.
- [ ] Enable [`GIT_COMMIT_STATUS`](Git-Settings.md#commit-status-reporting) so a commit shows whether it is deployed.
- [ ] Restrict production with [sync windows](Advanced/Sync-Windows.md), configured on the controller, not in the deploy config.
- [ ] Enable [reconciliation](Deploy-Settings.md#reconciliation-settings) per stack, starting with the `unhealthy` event.
- [ ] Review the [artifact garbage collection](Reference/Artifact-Storage.md#garbage-collection) defaults against your disk budget.
- [ ] Upgrade the controller by bumping the pinned tag and reading the release notes first. Some releases change on-disk layout, for example [v0.120.0](Reference/Artifact-Storage.md#upgrading-from-v0119x-or-earlier).

## 8. Migration checklist

- [ ] Inventory every Compose project, path, secret and side channel on the host.
- [ ] Decide the repository layout and where the target files live.
- [ ] Note the current Compose project name per stack, it becomes `name`.
- [ ] Move persistent data off relative bind mounts.
- [ ] Pin named volumes with `#!yaml name:` where the project name may differ.
- [ ] Declare shared networks `#!yaml external: true` and create them once.
- [ ] Pin image tags and set up Renovate.
- [ ] Move host-specific values into `environment` and `env_files`.
- [ ] Encrypt or externalize every secret, remove plaintext from the repository.
- [ ] Install the controller with a persistent `/data` mount and a pinned tag.
- [ ] Choose polling, webhooks or both, and set `target` per host.
- [ ] Start with `#!yaml LOG_LEVEL: debug`.
- [ ] Per stack: check preconditions, including `container_name` collisions.
- [ ] Per stack: `docker compose down` without `-v` when the project name changes.
- [ ] Per stack: commit, trigger, verify labels and commit SHA.
- [ ] Per stack: confirm the second trigger changes nothing.
- [ ] Per stack: keep old compose and env files until one normal change succeeded.
- [ ] Convert host crons to scheduled job labels.
- [ ] Replace `docker system prune -a` crons with `docker image prune`.
- [ ] Replace deploy scripts with init containers, sidecars or lifecycle hooks.
- [ ] Remove old systemd units, scripts and deploy keys.
- [ ] Enable notifications, metrics and commit status.
- [ ] Add sync windows and reconciliation where they are needed.
