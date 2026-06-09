---
tags:
  - Configuration
  - Deployment
---

# Deployment Settings

Deployments in `Doco-CD` run as concurrent tasks. 
Each deployment is defined by a deployment configuration file (e.g. `.doco-cd.yml`) that controls how it runs. 

Concurrent tasks are grouped by repository and Git reference (e.g. branch or tag). 
Deployments from the same repository but different references run sequentially, while those with the same repository and reference run in parallel. 
See the [App Settings](App-Settings.md) documentation for more information on how to configure the number of concurrent deployments.

## Deployment Sources

Doco-CD supports deploying from the following sources:

| Source                                         | Description                                                                                      |
|------------------------------------------------|--------------------------------------------------------------------------------------------------|
| Git Repository                                 | Deploy directly from a Git repository.                                                           |
| [OCI Artifact](Advanced/OCI/Artifact-Usage.md) | Deploy from an OCI artifact that contains all necessary configurations and files for deployment. |

## Deployment Configuration File

The deployment configuration file must be placed in the root/base directory of your repository and named one of the following:

- `.doco-cd.yaml`
- `.doco-cd.yml`

When using a custom target (for example `nas`), the file name must match `.doco-cd.<target>.y(a)ml`.

If you use polling, you can also specify inline deployment configurations in the poll configuration file.
See [Poll Settings](Poll-Settings.md) and this [example](Poll-Settings.md#inline-deploy-configs) for more information.

## Available Settings

The docker compose deployment can be configured inside the [deployment configuration file](#deployment-configuration-file) using the following settings:

!!! note "Settings without a default value are required."


| Key                 | Type              | Description                                                                                                                                                                                                                                                                                                                                                                                                                                                                          | Default value                                                                                                          |
|---------------------|-------------------|--------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|------------------------------------------------------------------------------------------------------------------------|
| `name`              | string            | Name of the deployed stack / project / application.                                                                                                                                                                                                                                                                                                                                                                                                                                  |                                                                                                                        |
| `version`           | string            | Optional deployment config/artifact version for OCI-backed deployments. Currently only `doco.v1` is supported.                                                                                                                                                                                                                                                                                                                                                                       | `doco.v1`                                                                                                              |
| `reference`         | string            | Git reference to deploy from, must be either a branch (e.g. `main` or `refs/heads/main`) or tag (e.g. `v1.0.0.` or `refs/tags/v1.0.0`)                                                                                                                                                                                                                                                                                                                                               | - Polling: the reference from the [Poll Config](Poll-Settings.md)<br/>- Webhooks: the reference of the webhook payload |
| `repository_url`    | string            | HTTP clone URL of another repository that contains the docker compose files to be deployed. If specified, the deployment runs from there. Also set `reference` to specify the branch.                                                                                                                                                                                                                                                                                                | ` ` (Ignored when not specified)                                                                                       |
| `working_dir`       | string            | The working directory for the deployment.                                                                                                                                                                                                                                                                                                                                                                                                                                            | `.` (Root/base directory of cloned repository)                                                                         |
| `compose_files`     | array of strings  | List of docker-compose and overwrite files to use (in descending order, first file gets read first and following files [overwrite/merge](https://docs.docker.com/reference/compose-file/merge/) previous configs). Unknown/Non-existing files get skipped.                                                                                                                                                                                                                           | `["compose.yaml", "compose.yml", "docker-compose.yml", "docker-compose.yaml"]`                                         |
| `environment`       | map of strings    | A map of environment variables to use for [variable interpolation](https://docs.docker.com/compose/how-tos/environment-variables/variable-interpolation) in the compose files. Overwrites entries from `env_files` with the same key/name.                                                                                                                                                                                                                                           | `null` (No environment variables)                                                                                      |
| `env_files`         | array of strings  | List of dotenv files to use for [variable interpolation](https://docs.docker.com/compose/how-tos/environment-variables/variable-interpolation). Subsequent .env files overwrite each other. If the default `.env` file does not exist, it will be ignored.<br>If `repository_url` is also specified to deploy from a different repo, you can use the `remote:<filepath>` syntax to specify, that the dotenv file is located in the remote repository and should be loaded from there | `[".env"]`                                                                                                             |
| `profiles`          | array of strings  | List of [compose profiles](https://docs.docker.com/compose/how-tos/profiles/) to use for the deployment, e.g., `#!yaml ["prod", "debug"]`.                                                                                                                                                                                                                                                                                                                                           | `[]`                                                                                                                   |
| `webhook_filter`    | string            | A regular expression to whitelist deployment triggers based on the webhook event payload. See the [Webhook Filter](#webhook-filter) Section below.                                                                                                                                                                                                                                                                                                                                   | ` ` (Ignored when not specified)                                                                                       |
| `remove_orphans`    | boolean           | Remove/Prune containers/services that are not (or no longer) defined in the Compose file.                                                                                                                                                                                                                                                                                                                                                                                            | `true`                                                                                                                 |
| `prune_images`      | boolean           | Prune images that are no longer in use after a deployment. If the image is still used by any other container, it won't get deleted.                                                                                                                                                                                                                                                                                                                                                  | `true`                                                                                                                 |
| `force_recreate`    | boolean           | Forces the recreation/redeployment of containers even if the configuration has not changed.                                                                                                                                                                                                                                                                                                                                                                                          | `false`                                                                                                                |
| `wait_running_jobs` | boolean           | Wait for currently running [scheduled jobs](Advanced/Job-Scheduling.md) to finish before deployment starts.                                                                                                                                                                                                                                                                                                                                                                          | `true`                                                                                                                 |
| `force_image_pull`  | boolean           | Always pulls the latest version of the image tags you've specified if a newer version is available.                                                                                                                                                                                                                                                                                                                                                                                  | `false`                                                                                                                |
| `timeout`           | number            | The time in seconds to wait for the deployment to finish before timing out.                                                                                                                                                                                                                                                                                                                                                                                                          | `180`                                                                                                                  |
| `git_depth`         | number            | Limits the number of commits fetched during clone/fetch (shallow clone). `0` means use the global [`GIT_CLONE_DEPTH`](App-Settings.md) value. A positive integer overrides the global setting for this deployment. When a requested ref (tag/SHA) is outside the shallow depth, doco-cd automatically deepens incrementally before falling back to a full fetch. Changing this value on an existing repo triggers an automatic re-clone.                                             | `0` (use global)                                                                                                       |
| `destroy`           | boolean \| object | (⚠️ Destructive) Configure stack/project destruction behavior. Use `destroy: true` as shorthand to enable destruction with default options, or use `destroy.enabled: true` inside the object form to customize removal behavior. See [Destroy settings](#destroy-settings).                                                                                                                                                                                                          | see [Destroy settings](#destroy-settings)                                                                              |
| `auto_discovery`    | boolean \| object | Enables [autodiscovery](#auto-discovery) of services to deploy in the working directory by scanning for subdirectories with Docker Compose files (see the `compose_files` setting). Use `auto_discovery: true` as shorthand to enable it with default options, or use the object form to customize settings such as `depth` and `delete`. See [Auto-Discovery Settings](#auto-discovery-settings).                                                                                   | see [Auto-Discovery Settings](#auto-discovery-settings)                                                                |
| `reconciliation`    | boolean \| object | Enables event-driven reconciliation for deployments. Use `reconciliation: true` as shorthand to enable reconciliation with default options, or use the object form to customize settings. See [reconciliation settings](#reconciliation-settings).                                                                                                                                                                                                                                   | see [Reconciliation Settings](#reconciliation-settings)                                                                |
| `hooks`             | object            | HTTP webhook hooks fired after a deployment succeeds or fails. See [Hook Settings](#hook-settings).                                                                                                                                                                                                                                                                                                                                                                                  | `null` (No hooks)                                                                                                      |


!!! example

    === "With default values"
    
        When using the default values, most settings can be omitted.
        
        ```yaml title=".doco-cd.yml"
        name: some-project # (1)!
        ```
    
        1. Name of the deployed stack/project
    
    === "With custom values"
    
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
    
    === "From remote repository"
    
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

### Auto-Discovery

If `auto_discovery` is enabled, doco-cd will try to find projects/stacks to deploy by searching for docker compose files 
(see the `compose_files` setting) in subdirectories in the working directory (`working_dir`). 

Doco-cd will internally generate new deploy configs based on the directory name and inherits all other settings from the 
base deploy config inside the `.doco-cd.yml` file or the inline deployment config inside the poll config.

When an app is no longer available in the `working_dir` (e.g. deleted or moved to another directory outside the working dir), 
doco-cd will automatically remove the deployed project/stack from the docker host.

#### Auto-Discovery settings

`auto_discovery` accepts either a boolean or a nested object in the deployment configuration file. 
Use `auto_discovery: true` to enable it with defaults, or use the object form below to customize the settings.

| Key              | Type    | Description                                                                                          | Default value |
|------------------|---------|------------------------------------------------------------------------------------------------------|---------------|
| `enabled`        | boolean | Enables auto-discovery of services to deploy in the working directory                                | `false`       |
| `depth`          | number  | Maximum depth of subdirectories to scan for docker-compose files, set to `0` for no limit            | `0`           |
| `delete`         | boolean | Auto-remove obsolete auto-discovered deployments that are no longer present in the working directory | `true`        |
| `remove_volumes` | boolean | Remove volumes of auto-discovered deployments when they are deleted                                  | `false`       |
| `remove_images`  | boolean | Remove images of auto-discovered deployments when they are deleted                                   | `true`        |

??? example "Auto-discovery Setup Example"
    <div class="grid cards" markdown>

    - With a file structure like this
      ``` title="File structure"
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
    
    - And a `.doco-cd.yml` with the following content:

        === "Default settings"

            ```yaml title=".doco-cd.yml"
            auto_discovery: true
            ```

        === "Custom settings"

            ```yaml title=".doco-cd.yml"
            working_dir: apps/
            auto_discovery:
              enabled: true
              depth: 1
            ```
    </div>

    Doco-cd would deploy 2 stacks to the docker host: `wordpress` and `nginx`

#### Controlling resource cleanup on stack deletion

When auto-discovered stacks are deleted (e.g., because the compose file was removed from the repository), 
you can control whether volumes and images are removed using the `remove_volumes` and `remove_images` settings
in the `auto_discovery` configuration:

```yaml title=".doco-cd.yml"
working_dir: apps/
auto_discovery:
  enabled: true
  delete: true
  remove_volumes: false # (1)!
  remove_images: true # (2)!
```

1. Keep volumes when stacks are auto-deleted
2. Remove images when stacks are auto-deleted

??? note "Default behavior"
    By default, `remove_volumes` is set to `false`, meaning volumes are **preserved** when auto-discovered stacks are deleted.
    This is a safer default to prevent accidental data loss (e.g., databases). Set `remove_volumes: true` if you want volumes to be removed when stacks are auto-deleted.

#### Nested config overrides

For each auto-discovered compose directory, doco-cd also checks for a local [deployment config file](#deployment-configuration-file) in that directory.

If a nested config file exists, doco-cd merges it on top of the discovered deployment config.
When using [custom webhook targets](Endpoints/Webhook-Listener.md#with-custom-target), nested config files always use the standard [naming convention](#deployment-configuration-file) (`.doco-cd.y(a)ml`), not the custom target name.

!!! warning "Nested `.doco-cd.yml` files must contain exactly one YAML document."
    If a nested file contains multiple documents (`#!yaml ---`), auto-discovery fails for that run with an error.

##### Merge behavior

- Maps are merged key-by-key (`external_secrets`, `environment`, `build.args`)
- Slices replace the base value when the nested value is non-empty
- Scalar values override the base value when the nested value is non-zero/non-empty
- Nested objects (such as `build`, `destroy`, `reconciliation`) are merged recursively

##### Non-overridable Fields

The following fields are always inherited from the base/root deployment config:

- `reference`
- `repository_url`
- `auto_discovery`
- `git_depth`

#### Example

!!! example

    ``` title="File structure"
    .doco-cd.yml
    apps/
    ├── wordpress/
    │   ├── .doco-cd.yml
    │   ├── docker-compose.yml
    │   └── .env
    ├── nginx/
    │   ├── .doco-cd.yml
    │   └── docker-compose.yaml
    └── misc/
        └── image.png
    ```

    ```yaml title=".doco-cd.yml (root)"
    working_dir: apps/
    auto_discovery:
      enabled: true
      depth: 1
    external_secrets:
      SHARED_SECRET: "op://vault/shared/field"
    ```

    ```yaml title="apps/wordpress/.doco-cd.yml"
    name: wordpress-prod
    external_secrets:
      WORDPRESS_SECRET_1: "op://vault/item/field"
    environment:
      WP_ENV: production
    ```

    Result for discovered `wordpress` deployment:

    - `name` becomes `wordpress-prod`
    - `working_dir` remains auto-discovered (`apps/wordpress/`)  unless explicitly overridden
    - `external_secrets` contains both `SHARED_SECRET` and `WORDPRESS_SECRET_1`

### Build settings

The following settings can be used to build docker images during a deployment (Like `docker compose build` or `docker compose up --build`).

See also the [Compose Build Specification](https://docs.docker.com/reference/compose-file/build/) for more information on building docker images with compose.

Specify all build-settings in a nested `build` object in the deployment configuration file (See example below).

| Key                | Type           | Description                                                | Default value |
|--------------------|----------------|------------------------------------------------------------|---------------|
| `force_image_pull` | boolean        | Always attempt to pull the latest version of the image     | `false`       |
| `quiet`            | boolean        | Suppress the build output in the logs                      | `false`       |
| `args`             | map of strings | A map of build-time arguments to pass to the build process | `null`        |
| `no_cache`         | boolean        | Disables the use of the cache when building images         | `false`       |

!!! example
    ```yaml title=".doco-cd.yml"
    name: some-project
    build:
      force_image_pull: true
      args:
        BUILD_DATE: 2021-01-01
        VCS_REF: 123456
      no_cache: true
    ```

### Destroy settings

The following settings can be used to configure how the deployed compose stack/project will be removed.

`destroy` accepts either a boolean or a nested object in the deployment configuration file. Use `destroy: true` to enable destructive removal with default options, or use the object form below to customize which resources are removed.

| Key              | Type    | Description                                                                                                                                                                                                                                                                       | Default value |
|------------------|---------|-----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|---------------|
| `enabled`        | boolean | Enable destructive removal of the deployment and its resources.                                                                                                                                                                                                                   | `false`       |
| `remove_volumes` | boolean | Remove all volumes used by the deployment (always `true` in docker swarm mode)                                                                                                                                                                                                    | `true`        |
| `remove_images`  | boolean | Remove all images used by the deployment (currently not supported in docker swarm mode)                                                                                                                                                                                           | `true`        |
| `remove_dir`     | boolean | Remove the cloned repository in the data directory after the deployment is removed (Setting this to `false` is useful e.g. when you use bind mounts with relative paths and want to keep the data or if you have multiple services in the same repo and only wish to destroy one) | `true`        |

!!! example

    === "Boolean with default options"

        ```yaml title=".doco-cd.yml"
        name: some-project
        destroy: true
        ```
    
        This shorthand enables destruction with the default options (`remove_volumes: true`, `remove_images: true`, `remove_dir: true`).

    === "Object with custom options"

        ```yaml title=".doco-cd.yml"
        name: some-project
        destroy:
          enabled: true
          remove_volumes: true
          remove_images: false
          remove_dir: false
        ```

### Reconciliation Settings

Reconciliation is an optional event-driven check that compares the currently running Docker services/containers with the expected deployment state.
When configured container events occur, doco-cd either reapplies the deployment or directly restarts the affected container, depending on the event type.

!!! warning
    The currently implemented state will be lost when doco-cd gets restarted and will be restored in the next poll or webhook event.

`reconciliation` accepts either a boolean or a nested object in the deployment configuration file. Use `reconciliation: true` to enable it with defaults, or use the object form below to customize the settings.

The following settings can be used to configure reconciliation triggers.

| Key               | Type             | Description                                                                                                                   | Default value   |
|-------------------|------------------|-------------------------------------------------------------------------------------------------------------------------------|-----------------|
| `enabled`         | boolean          | Enable reconciliation.                                                                                                        | `true`          |
| `events`          | array of strings | Docker container/service events that trigger reconciliation. See [supported events](#supported-events) below.                 | `["unhealthy"]` |
| `restart_timeout` | number           | Timeout in seconds used when restarting containers for reconciliation [events](#supported-events) that trigger a restart.     | `10`            |
| `restart_signal`  | string           | Signal used for reconciliation restarts. If not set, the `StopSignal` of the container image is used (defaults to `SIGTERM`). |                 |
| `restart_limit`   | number           | Maximum number of automatic restarts allowed for a container in the restart window. Set to `0` to disable suppression.        | `5`             |
| `restart_window`  | number           | Time window in seconds used with `restart_limit` to detect flapping health checks.                                            | `300`           |

--8<-- "wiki/includes/reconciliation-note.md"

#### Supported Events

Events can be triggered by changes in the container state, configuration updates outside Doco-CD (e.g. via Docker CLI), or health status changes.
The following events are supported as reconciliation triggers in Docker (Standalone) and Docker Swarm deployments:

=== "Docker Standalone"
    
    | Event       | Description                                              | Action   |
    |-------------|----------------------------------------------------------|----------|
    | `die`       | The container process exited                             | Redeploy |
    | `destroy`   | The container was removed                                | Redeploy |
    | `stop`      | The container was stopped gracefully                     | Restart  |
    | `kill`      | The container was terminated by a signal                 | Restart  |
    | `oom`       | The container was killed because it ran out of memory    | Restart  |
    | `unhealthy` | The container health check status changed to _unhealthy_ | Restart  |


    !!! info "Flapping health checks"
        For `unhealthy` events, doco-cd suppresses further automatic restarts after `restart_limit` restarts within `restart_window` seconds (See [reconciliation settings](#reconciliation-settings)).

    !!! warning "Overlapping events"
        Some events, like the `die` event, also get triggered when a container is restarted, stopped or killed, so make sure to 
        configure the events according to the desired behavior.

        To prevent unexpected behavior, doco-cd suppresses follow-up events for a container after the first event 
        that triggered a reconciliation for that container until the reconciliation process is finished.

        ??? example
            If both `die` and `stop` events are configured, and a container is stopped, the `stop` event will also trigger a `die` event. 
            However, doco-cd will only react to the first event (e.g. `stop`) and suppress the follow-up `die` event.

=== "Docker Swarm Mode"
    
    | Event     | Description                                                 | Action   |
    |-----------|-------------------------------------------------------------|----------|
    | `destroy` | The service was removed                                     | Redeploy |
    | `update`  | The service configuration was updated (for example scaling) | Redeploy |

#### Examples

=== "Boolean with default options"

    Enable reconciliation with default options:

    ```yaml title=".doco-cd.yml"
    name: some-project
    reconciliation: true
    ```

=== "Object with custom options"

    ```yaml title=".doco-cd.yml"
    name: some-project
    reconciliation:
      enabled: true
      restart_timeout: 30
      restart_signal: SIGQUIT
      restart_limit: 5
      restart_window: 300
      events:
        - destroy
        - unhealthy
    ```

### Hook Settings

Hooks send an HTTP request to one or more endpoints when a deployment finishes, so you can trigger external systems (CI pipelines, alerting, dashboards) on the outcome.

!!! info "Hooks vs. Notifications"
    Hooks are raw HTTP calls configured per deployment in the deployment config. For ready-made messages to chat/email services, use [Notifications](Advanced/Notifications.md) instead.

`hooks` is a nested object with two independent lists. Each list contains one or more hook entries:

| Key          | Type                  | Description                                                  | Default value |
|--------------|-----------------------|--------------------------------------------------------------|---------------|
| `on_success` | array of hook entries | Hooks called after the deployment completes successfully.    | `[]`          |
| `on_failure` | array of hook entries | Hooks called when the deployment fails.                      | `[]`          |

Each hook entry accepts:

| Key       | Type           | Description                                                        | Default value |
|-----------|----------------|--------------------------------------------------------------------|---------------|
| `url`     | string         | Target endpoint. Must be a valid `http`/`https` URL.               |               |
| `method`  | string         | HTTP method to use.                                                | `POST`        |
| `headers` | map of strings | Additional request headers (e.g. an authorization token).         | `null`        |

The request body is JSON (`Content-Type: application/json`) with the following fields:

| Field        | Description                                            |
|--------------|--------------------------------------------------------|
| `event`      | `success` or `failure`.                                |
| `repository` | Source repository / OCI artifact name.                 |
| `stack`      | Deployment / stack name.                               |
| `revision`   | Deployed reference and commit (e.g. `main (abc1234)`). |
| `job_id`     | Unique job identifier.                                 |
| `images`     | Resolved image references (`name:tag`) of the changed services. Omitted when no service images are available. |
| `error`      | Failure reason (present on `failure` events only).     |

!!! note "Hooks are best-effort"
    A hook request has a 10 second timeout. A non-2xx response or an unreachable endpoint is logged but does **not** fail the deployment.

!!! example

    ```yaml title=".doco-cd.yml"
    name: some-project
    hooks:
      on_success:
        - url: https://ci.example.com/notify # (1)!
          headers:
            Authorization: Bearer xxx
      on_failure:
        - url: https://alerts.example.com/webhook # (2)!
          method: PUT
    ```

    1. Called with an `event: success` payload after a successful deployment.
    2. Called with an `event: failure` payload (including the `error` field) when the deployment fails.

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

!!! example
    === "Explicit regular expression"

        - Only on events on the main branch: `^refs/heads/main$`
        - Only on tag events with semantic versioning: `^refs/tags/v[0-9]+\.[0-9]+\.[0-9]+$`

    === "Loose regular expression"
        - Must contain `stable` somewhere in the reference: `stable`

        !!! warning
            Loose expressions can allow references that might not be wanted.

            E.g. `refs/heads/main` (without `^` and `$`) also allows `refs/heads/main-something`

### Prevent recreation on config, secret or bind mount changes

When using docker compose with configs, secrets or bind mounts, changes to these resources will trigger a recreation of the service containers by default.
To avoid this, you can set the `cd.doco.deployment.recreate.ignore` service label to a YAML list of scopes that should be ignored for recreation.

It's a map of scope → items to ignore; `null` or an empty value tells doco-cd to ignore all items in that scope.
It accepts one or more of the following scopes: `configs`, `secrets`, `bindMounts`.

1. `configs` and `secrets` items refer to names defined in the top-level `configs` and `secrets` sections.
2. `bindMounts` items refer to the **target paths** of bind mounts (not the source paths).

!!! example

    === "Single line YAML value"
    
        !!! warning "Quotes are required"
            Quotes are required to prevent YAML parsing errors due to the colons and brackets in the value
        
        ```yaml title="docker-compose.yml"
        cd.doco.deployment.recreate.ignore: "{configs: [app, nginx], secrets: [db], bindMounts: [/etc/caddy]}"
        ```

    === "Multiline YAML value"

        !!! tip "Use multiline YAML for better readability"

        ```yaml title="docker-compose.yml"
        cd.doco.deployment.recreate.ignore: >-
          {
            configs: [app, nginx],
            secrets: [db],
            bindMounts: [/etc/caddy]
          }
        ```

#### Send signal on ignored recreation

Add the `cd.doco.deployment.recreate.ignore.signal` label to send a signal to a service when it is ignored. 
By default, no signal is sent. This requires [`cd.doco.deployment.recreate.ignore`](#prevent-recreation-on-config-secret-or-bind-mount-changes) to be set.

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

### Wait for running scheduled jobs before deployment

By default, deployments wait for currently running [scheduled jobs](Advanced/Job-Scheduling.md) to finish before recreating/updating containers.
This helps prevent long-running jobs from being interrupted by a deployment.

Use `wait_running_jobs` (default: `true`) in the deployment config to control the default behavior for that deployment target.

```yaml title=".doco-cd.yml"
name: some-project
wait_running_jobs: true
```

You can override this per scheduled job service with the `cd.doco.job.wait_running_jobs` label (see [Job Scheduling](Advanced/Job-Scheduling.md#configuration)).

Precedence:

- Service label `cd.doco.job.wait_running_jobs` (if set)
- Deployment default `wait_running_jobs`

## Multiple service deployments

Multiple service deployments can be configured in a single deployment config file by specifying multiple YAML documents (separated by `#!yaml ---`).

!!! example

    === "Basic"

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

    === "Same directory"

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

    === "Sub-directories"

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
