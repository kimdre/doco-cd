---
tags:
  - Configuration
  - Deployment
---

# Deployment Settings

Deployments in `Doco-CD` run as concurrent tasks. 
Each deployment is defined by a deployment configuration file (e.g. `.doco-cd.yml`) that controls how it runs. 
Enable `auto_discover` to generate multiple deployments from a single config by scanning subdirectories for Docker Compose files.

Concurrent tasks are grouped by repository and Git reference (e.g. branch or tag). 
Deployments from the same repository but different references run sequentially, while those with the same repository and reference run in parallel. 
See the [App Settings](App-Settings.md) documentation for more information on how to configure the number of concurrent deployments.

## Deployment Configuration File

The deployment configuration file must be placed in the root/base directory of your repository and named one of the following:

- `.doco-cd.yaml`
- `.doco-cd.yml`

If you use polling, you can also specify inline deployment configurations in the poll configuration file.
See [Poll Settings](Poll-Settings.md) and this [example](Poll-Settings.md#inline-deploy-configs) for more information.

## Available Settings

The docker compose deployment can be configured inside the [deployment configuration file](#deployment-configuration-file) using the following settings:

!!! note "Settings without a default value are required."


| Key                | Type             | Description                                                                                                                                                                                                                                                                                                                                                                                                                                                                          | Default value                                                                                                          |
|--------------------|------------------|--------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|------------------------------------------------------------------------------------------------------------------------|
| `name`             | string           | Name of the deployed stack / project / application.                                                                                                                                                                                                                                                                                                                                                                                                                                  |                                                                                                                        |
| `reference`        | string           | Git reference to deploy from, must be either a branch (e.g. `main` or `refs/heads/main`) or tag (e.g. `v1.0.0.` or `refs/tags/v1.0.0`)                                                                                                                                                                                                                                                                                                                                               | - Polling: the reference from the [Poll Config](Poll-Settings.md)<br/>- Webhooks: the reference of the webhook payload |
| `repository_url`   | string           | HTTP clone URL of another repository that contains the docker compose files to be deployed. If specified, the deployment runs from there. Also set `reference` to specify the branch.                                                                                                                                                                                                                                                                                                | ` ` (Ignored when not specified)                                                                                       |
| `working_dir`      | string           | The working directory for the deployment.                                                                                                                                                                                                                                                                                                                                                                                                                                            | `.` (Root/base directory of cloned repository)                                                                         |
| `compose_files`    | array of strings | List of docker-compose and overwrite files to use (in descending order, first file gets read first and following files overwrite/merge previous configs). Unknown/Non-existing files get skipped.                                                                                                                                                                                                                                                                                    | `["compose.yaml", "compose.yml", "docker-compose.yml", "docker-compose.yaml"]`                                         |
| `environment`      | map of strings   | A map of environment variables to use for [variable interpolation](https://docs.docker.com/compose/how-tos/environment-variables/variable-interpolation) in the compose files. Overwrites entries from `env_files` with the same key/name.                                                                                                                                                                                                                                           | `null` (No environment variables)                                                                                      |
| `env_files`        | array of strings | List of dotenv files to use for [variable interpolation](https://docs.docker.com/compose/how-tos/environment-variables/variable-interpolation). Subsequent .env files overwrite each other. If the default `.env` file does not exist, it will be ignored.<br>If `repository_url` is also specified to deploy from a different repo, you can use the `remote:<filepath>` syntax to specify, that the dotenv file is located in the remote repository and should be loaded from there | `[".env"]`                                                                                                             |
| `profiles`         | array of strings | List of [compose profiles](https://docs.docker.com/compose/how-tos/profiles/) to use for the deployment, e.g., ["prod", "debug"].                                                                                                                                                                                                                                                                                                                                                    | `[]`                                                                                                                   |
| `webhook_filter`   | string           | A regular expression to whitelist deployment triggers based on the webhook event payload. See the [Webhook Filter](#webhook-filter) Section below.                                                                                                                                                                                                                                                                                                                                   | ` ` (Ignored when not specified)                                                                                       |
| `remove_orphans`   | boolean          | Remove/Prune containers/services that are not (or no longer) defined in the Compose file.                                                                                                                                                                                                                                                                                                                                                                                            | `true`                                                                                                                 |
| `prune_images`     | boolean          | Prune images that are no longer in use after a deployment. If the image is still used by any other container, it won't get deleted.                                                                                                                                                                                                                                                                                                                                                  | `true`                                                                                                                 |
| `force_recreate`   | boolean          | Forces the recreation/redeployment of containers even if the configuration has not changed.                                                                                                                                                                                                                                                                                                                                                                                          | `false`                                                                                                                |
| `force_image_pull` | boolean          | Always pulls the latest version of the image tags you've specified if a newer version is available.                                                                                                                                                                                                                                                                                                                                                                                  | `false`                                                                                                                |
| `timeout`          | number           | The time in seconds to wait for the deployment to finish before timing out.                                                                                                                                                                                                                                                                                                                                                                                                          | `180`                                                                                                                  |
| `git_depth`        | number           | Limits the number of commits fetched during clone/fetch (shallow clone). `0` means use the global [`GIT_CLONE_DEPTH`](App-Settings.md) value. A positive integer overrides the global setting for this deployment. When a requested ref (tag/SHA) is outside the shallow depth, doco-cd automatically deepens incrementally before falling back to a full fetch. Changing this value on an existing repo triggers an automatic re-clone.                                             | `0` (use global)                                                                                                       |
| `destroy`          | boolean          | (⚠️ Destructive) Remove the deployed compose stack/project and its resources from the Docker host. Can be further configured using the [destroy_opts](#destroy-settings) setting.                                                                                                                                                                                                                                                                                                    | `false`                                                                                                                |
| `auto_discover`    | boolean          | Enables autodiscovery of services to deploy in the working directory by scanning for subdirectories with docker-compose files with the naming `docker-compose.y(a)ml` or `compose.y(a)ml`. Can be further configured using the [auto_discover_opts](#auto-discover-settings) setting.                                                                                                                                                                                                | `false`                                                                                                                |
| `reconciliation`   | object           | Enables event-driven reconciliation for deployments. See [reconciliation settings](#reconciliation-settings) for more details.                                                                                                                                                                                                                                                                                                                                                       | `{enabled: true, events: [die, destroy], restart_timeout: 10}`                                                         |


### Example

#### With default values

When using the default values, most settings can be omitted.

```yaml title=".doco-cd.yml"
name: some-project # (1)!
```

1. Name of the deployed stack/project

#### With custom values

```yaml title=".doco-cd.yml"
name: some-project # (1)!
reference: other-branch # (2)!
working_dir: myapp/deployment # (3)!
compose_files: # (4)!
  - prod.compose.yml
  - service-overwrite.yml
profiles:
  - debug # (5)!
```

1. Name of the deployed stack/project
2. The branch or tag to deploy from
3. The working directory for the deployment, relative to the root of the repository. In this case, doco-cd will look for the compose files in the `myapp/deployment` subdirectory.
4. The list of compose files to use for the deployment in descending order. In this case, doco-cd will first read the `prod.compose.yml` file and then overwrite/merge it with the `service-overwrite.yml` file.
5. Deploys services with the `debug` profile in addition to the core/main services (that have no profiles)

#### From remote repository

When deploying your docker compose stack from a different repository, the `repository_url` setting must be specified. 
The `reference` and `working_dir` are used in this case to specify the branch/tag and subdirectory of the other repository that contains the docker compose files.

You can use the `env_files` setting to define which dotenv files will be loaded from the local and which from the remote repository.
To specify, that a dotenv file should be loaded from the remote repository, use the `remote:<filepath>` syntax.
Entries/Keys, that appear in multiple files, get overwritten by the next occurrence and remote dotenv files have higher priority than local ones.

```yaml title=".doco-cd.yml"
name: some-project # (1)!
repository_url: https://github.com/my-org/myapp.git # (2)!
reference: main # (3)!
working_dir: myapp/docker # (4)!
env_files: # (5)!
  - base.env # (6)!
  - remote:test.env # (7)!
```

1. Name of the deployed stack/project
2. Clone and deploy from this repository instead of the repository where the deployment config file is located.
3. The branch or tag to deploy from in the remote repository (`my-org/myapp`).
4. The working directory for the deployment, relative to the root of the remote repository. In this case, doco-cd will look for the compose files in the `myapp/docker` subdirectory.
5. List of dotenv files to use in descending order. Existing variables get overwritten by the next occurrence. In this case, variables from `test.env` in the remote repository will overwrite variables from `base.env` in the local repository.
6. Read file from local repository
7. Read file from remote repository

```dotenv title="base.env"
TEST=base
HELLO=world
```

```dotenv title="test.env in remote repository"
TEST=changed
```

This will result in the following environment variables being set for the deployment in the remote repository:

```dotenv
TEST=changed
HELLO=world
```

### Auto discover settings

If `auto_discover` is set to `true`, doco-cd will try to auto-discover projects/stacks to deploy by searching for `docker-compose.y(a)ml` or `compose.y(a)ml` files in subdirectories in the working directory (`working_dir`). 
Doco-cd will internally generate new deploy configs based on the directory name and inherits all other settings from the base deploy config inside the `.doco-cd.yml` file or the inline deployment config inside the poll config.
When an app is no longer available in the `working_dir` (e.g. deleted or moved to another directory outside the working dir), doco-cd will automatically remove the deployed project/stack from the docker host.

Specify all auto-discover settings in a nested `auto_discover_opts` object in the deployment configuration file (See example below).

| Key      | Type    | Description                                                                                          | Default value |
|----------|---------|------------------------------------------------------------------------------------------------------|---------------|
| `depth`  | number  | Maximum depth of subdirectories to scan for docker-compose files, set to `0` for no limit            | `0`           |
| `delete` | boolean | Auto-remove obsolete auto-discovered deployments that are no longer present in the working directory | `true`        |

#### Example

With a file structure like this
```
.doco-cd.yml
apps/
├── wordpress/
│   ├── docker-compose.yml
│   └── .env
├── nginx/
│   ├── docker-compose.yaml
│   └── configs/
│       └── nginx.conf
└── misc/
    └── image.png
```

and a `.doco-cd.yml` with the following content:
```yaml title=".doco-cd.yml"
working_dir: apps/
auto_discover: true
auto_discover_opts:
  depth: 1
```

doco-cd would deploy 2 stacks to the docker host:
- wordpress
- nginx

### Build settings

The following settings can be used to build docker images during a deployment (Like `docker compose build` or `docker compose up --build`).

Specify all build-settings in a nested `build_opts` object in the deployment configuration file (See example below).

| Key                | Type           | Description                                                | Default value |
|--------------------|----------------|------------------------------------------------------------|---------------|
| `force_image_pull` | boolean        | Always attempt to pull the latest version of the image     | `false`       |
| `quiet`            | boolean        | Suppress the build output in the logs                      | `false`       |
| `args`             | map of strings | A map of build-time arguments to pass to the build process | `null`        |
| `no_cache`         | boolean        | Disables the use of the cache when building images         | `false`       |

#### Example

```yaml title=".doco-cd.yml"
name: some-project
build_opts:
  force_image_pull: true
  args:
    BUILD_DATE: 2021-01-01
    VCS_REF: 123456
  no_cache: true
```

### Destroy settings

The following settings can be used to configure further how the deployed compose stack/project will be removed (if `destroy` is set to `true`):

Specify all destroy-settings in a nested `destroy_opts` object in the deployment configuration file (See example below).

| Key              | Type    | Description                                                                                                                                                                    | Default value |
|------------------|---------|--------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|---------------|
| `remove_volumes` | boolean | Remove all volumes used by the deployment (always `true` in docker swarm mode)                                                                                                 | `true`        |
| `remove_images`  | boolean | Remove all images used by the deployment (currently not supported in docker swarm mode)                                                                                        | `true`        |
| `remove_dir`     | boolean | Remove the cloned repository in the data directory after the deployment is removed (Setting this to `false` is useful e.g. when you use bind mounts and want to keep the data) | `true`        |

#### Example

```yaml title=".doco-cd.yml"
name: some-project
destroy: true
destroy_opts:
  remove_volumes: true
  remove_images: false
  remove_dir: false
```

### Reconciliation Settings

Reconciliation is an optional event-driven check that compares the currently running Docker services/containers with the expected deployment state.
When configured container events occur, doco-cd either reapplies the deployment or directly restarts the affected container, depending on the event type.

The following settings can be used to configure reconciliation triggers.

| Key               | Type             | Description                                                                                                   | Default value        |
|-------------------|------------------|---------------------------------------------------------------------------------------------------------------|----------------------|
| `enabled`         | boolean          | Enable reconciliation.                                                                                        | `true`               |
| `events`          | array of strings | Docker container/service events that trigger reconciliation. See [supported events](#supported-events) below. | `['die', 'destroy']` |
| `restart_timeout` | number           | Timeout in seconds used when restarting containers for `unhealthy`, `oom`, `kill`, and `stop` events.         | `10`                 |

--8<-- "wiki/docs/_snippets/reconciliation-note.md"

#### Supported Events

Events can be triggered by changes in the container state, configuration updates outside Doco-CD (e.g. via Docker CLI), or health status changes.
The following events are supported as reconciliation triggers in Docker (Standalone) and Docker Swarm deployments:

| Event       | Description                                                            | Standalone | Swarm | Action   |
|-------------|------------------------------------------------------------------------|------------|-------|----------|
| `die`       | The container process exited.                                          | Yes        | No    | Redeploy |
| `destroy`   | The container was removed / service was removed.                       | Yes        | Yes   | Redeploy |
| `update`    | The service/container configuration was updated (for example scaling). | No         | Yes   | Redeploy |
| `stop`      | The container was stopped gracefully.                                  | Yes        | No    | Restart  |
| `kill`      | The container was terminated by a signal.                              | Yes        | No    | Restart  |
| `oom`       | The container was killed because it ran out of memory.                 | Yes        | No    | Restart  |
| `unhealthy` | The container health check status changed to _unhealthy_.              | Yes        | No    | Restart  |

!!! warning
    Broader event sets (for example adding `stop`, `kill`, `oom`, or `unhealthy`) can increase reconciliation trigger frequency.

#### Examples

```yaml title=".doco-cd.yml"
name: some-project
reconciliation:
  enabled: true
  restart_timeout: 30
  events:
    - die
    - stop
    - kill
    - unhealthy
    - oom
```

```yaml title=".doco-cd.yml"
name: some-project
reconciliation:
  enabled: true
  events: [die, destroy]
```

### Webhook Filter

Set `webhook_filter` to a regular expression to whitelist deployment triggers based on the webhook event payload.

Depending on the event, the reference in a webhook payload has a pattern of 

- `refs/heads/<branch>` for branches 
- `refs/tags/<tag>` for tags
- Or no reference at all if the event is not associated with a tag or a branch event

!!! note
    If the `reference` setting value is unset/empty, the reference of the webhook payload will be used for deployments. 
    If `reference` is set, deployments for all events that pass the `webhook_filter`, will always run from that branch or tag.

You can specify the filter explicitly or in a loose form. Explicit regular expressions are recommended.  
See [Go Regular Expressions](https://pkg.go.dev/regexp/syntax) for more information on the syntax.

#### Explicit examples
- Only on events on the main branch: `^refs/heads/main$`
- Only on tag events with semantic versioning: `^refs/tags/v[0-9]+\.[0-9]+\.[0-9]+$`)

#### Loose examples
- Must contain `stable` somewhere in the reference: `stable`

!!! warning
    Loose expressions can allow references that might not be wanted.

E.g. `refs/heads/main` (without `^` and `$`) also allows `refs/heads/main-something`


### Service Labels

#### Avoid service recreation when configs, secrets or bind mounts change

When using docker compose with configs, secrets or bind mounts, changes to these resources will trigger a recreation of the service containers by default.
To avoid this, you can set the `cd.doco.deployment.recreate.ignore` service label to a YAML list of scopes that should be ignored for recreation.

It's a map of scope → items to ignore; `null` or an empty value tells doco-cd to ignore all items in that scope.
It accepts one or more of the following scopes: `configs`, `secrets`, `bindMounts`.

1. `configs` and `secrets` items refer to names defined in the top-level `configs` and `secrets` sections.
2. `bindMounts` items refer to the **target paths** of bind mounts (not the source paths).

##### Example

**Single line YAML value**

!!! example "Quotes are required"
    Quotes are required to prevent YAML parsing errors due to the colons and brackets in the value

```yaml title="docker-compose.yml"
cd.doco.deployment.recreate.ignore: "{configs: [app, nginx], secrets: [db], bindMounts: [/etc/caddy]}"
```

**Multiline YAML value**

Or as a multiline YAML for better readability:

```yaml title="docker-compose.yml"
cd.doco.deployment.recreate.ignore: >-
  {
    configs: [app, nginx],
    secrets: [db],
    bindMounts: [/etc/caddy]
  }
```

Add the `cd.doco.deployment.recreate.ignore.signal` label to send a signal to a service when it is ignored. By default, no signal is sent. This requires `cd.doco.deployment.recreate.ignore` to be set.

Both labels must not be empty if they are present.

```yaml title="docker-compose.yml" hl_lines="8-10"
services:
  caddy:
    image: caddy:2.11.2@sha256:1e40b251ca9639ead7b5cd2cedcc8765adfbabb99450fe23f130eefabf50f4bc
    container_name: caddy
    restart: always
    user: "1000:1000"
    environment: {}
    labels:
      cd.doco.deployment.recreate.ignore: "{bindMounts: [/etc/caddy]}"
      cd.doco.deployment.recreate.ignore.signal: "SIGHUP"
    ports:
      - "443:443"
      - "443:443/udp"
    volumes:
      - "./conf:/etc/caddy"
      - "/data/certs:/certs:ro"
    command:
      [
        "caddy",
        "run",
        "--watch",
        "--config",
        "/etc/caddy/Caddyfile",
        "--adapter",
        "caddyfile",
      ]
    cap_drop:
      - ALL
    cap_add:
      - NET_BIND_SERVICE
```

## Multiple service deployments

Multiple service deployments can be configured in a single deployment config file by specifying multiple YAML documents (separated by `---`).

```yaml title=".doco-cd.yml"
name: app1
working_dir: app1
---
name: app2
working_dir: app2
timeout: 600
---
name: app3
working_dir: app3
compose_files:
  - custom.yml
```

### Example

#### Same directory

All docker compose files are located in the same base directory.

```yaml title=".doco-cd.yml"
name: gitea
compose_files: 
  - gitea.yml
---
name: paperless-ngx
compose_files:
  - paperless.yml
  - paperless-overwrite.yml
```

#### Sub-directories

When docker compose files are located in subdirectories.

```yaml title=".doco-cd.yml"
name: gitea
working_dir: gitea
---
name: paperless-ngx
working_dir: paperless-ngx
compose_files:
  - docker-compose.yml
  - docker-compose.overwrite.yml
```

## Multiple deployment targets

You can specify multiple deployment target configurations in a mono-repo style setup using the application's dynamic webhook path. 
This allows you to deploy to different targets/locations from a single repository.
See [Webhook Listener with custom Target](Endpoints/Webhook-Listener.md#with-custom-target) for more information.

## Running pre- and post-deployment scripts

Doco-CD does not provide a shell environment for security reasons. Therefore, running shell scripts inside the Doco-CD container is not supported.
Instead, you can use init containers, sidecar containers, or compose lifecycle hooks to run scripts in the context of the deployed containers.
See the [Pre- and Post-Deployment Scripts](Advanced/Pre-Post-Deployment-Scripts.md) page for more information on how to set this up.
