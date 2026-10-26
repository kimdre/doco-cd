---
tags:
  - Configuration
  - Deployment
---

# Deployment Settings

Deployments in `Doco-CD` run as concurrent tasks. 
Each deployment is defined by a deployment configuration file (e.g. `.doco-cd.yml`) that controls how it runs. 

Deployments for different stacks - even the same repository, or the same repository and reference - run in parallel, up to the configured concurrency limit.
Only deployments of the *same* stack are ever serialized, so one repository can never block deployments of an unrelated stack.

## App Configuration

Use the following environment variables to configure the behavior of the doco-cd application itself. These settings are global and affect all deployments.

| Key                             | Type    | Description                                                                                                                                                                                                                                           | Default |
|---------------------------------|---------|-------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|---------|
| `DEPLOY_CONFIG_BASE_DIR`        | string  | Directory containing deployment configuration files, relative to the root of every Git repository or OCI artifact. Does not change `working_dir`, which remains relative to the source root.                                                          | `/`     |
| `MAX_CONCURRENT_DEPLOYMENTS`    | number  | Maximum number of concurrent deployments allowed                                                                                                                                                                                                      | `4`     |
| `MAX_CONCURRENT_PREDEPLOYMENTS` | number  | Maximum number of concurrent pre-deployment operations (initialization and change detection) allowed                                                                                                                                                  | `8`     |
| `MAX_DEPLOYMENT_LOOP_COUNT`     | number  | When the deployment loop detection should trigger a forced re-deployment on consecutive deployments for the same commit. Set to `0`, to disable the detection logic.                                                                                  | `2`     |
| `PASS_ENV`                      | boolean | Controls whether environment variables from the doco-cd container should be passed to the deployment environment for docker compose variable interpolation. Use with caution, as this may expose sensitive information to the deployment environment. | `false` |

## Deployment Sources

Doco-CD supports deploying from the following sources:

| Source                                         | Description                                                                                      |
|------------------------------------------------|--------------------------------------------------------------------------------------------------|
| Git Repository                                 | Deploy directly from a Git repository.                                                           |
| [OCI Artifact](Advanced/OCI/Artifact-Usage.md) | Deploy from an OCI artifact that contains all necessary configurations and files for deployment. |

## Deployment Configuration File

The deployment configuration file must be placed in `DEPLOY_CONFIG_BASE_DIR`, relative to your Git repository or OCI artifact root, and named one of the following:

- `.doco-cd.yaml`
- `.doco-cd.yml`

When using a custom target (for example `nas`), the file name must match `.doco-cd.<target>.y(a)ml`.

The default base directory is the source root. With `#!ini DEPLOY_CONFIG_BASE_DIR=deploy/`, place the configuration in
`deploy/.doco-cd.y(a)ml`. Lookup does not fall back to the root. `working_dir` remains relative to the source root,
and Compose file paths remain relative to `working_dir`.

If you use polling, you can also specify inline deployment configurations in the poll configuration file.
See [Poll Settings](Poll-Settings.md) and this [example](Poll-Settings.md#inline-deploy-configs) for more information.

### Editor autocomplete and validation

Doco-CD provides a [JSON Schema](schemas/deploy.schema.json) for deployment YAML files.
Compatible editors can suggest settings and values, display descriptions and defaults on hover, and flag unknown settings or incorrect value types.
The schema is generated from the deployment configuration's Go types, field descriptions, and defaults.
Its `x-doco-cd-version` metadata identifies the Doco-CD release or source revision used to generate it.

=== "VS Code and YAML language-server clients"

    In VS Code, install the [YAML extension by Red Hat](https://marketplace.visualstudio.com/items?itemName=redhat.vscode-yaml).
    Add this comment at the top of your deployment file:

    ```yaml title=".doco-cd.yml" hl_lines="1"
    # yaml-language-server: $schema=https://doco.cd/latest/schemas/deploy.schema.json
    name: my-app
    working_dir: deploy
    ```

    This also works in other editors configured to use [yaml-language-server](https://github.com/redhat-developer/yaml-language-server).
    Alternatively, associate the schema with all supported deployment filenames in your editor settings:

    ```json title=".vscode/settings.json"
    {
      "yaml.schemas": {
        "https://doco.cd/latest/schemas/deploy.schema.json": [
          "**/.doco-cd.yml",
          "**/.doco-cd.yaml",
          "**/.doco-cd.*.yml",
          "**/.doco-cd.*.yaml"
        ]
      }
    }
    ```

=== "JetBrains IDEs"

    Add the JetBrains schema comment at the top of your deployment file:

    ```yaml title=".doco-cd.yml"
    # $schema: https://doco.cd/latest/schemas/deploy.schema.json
    name: my-app
    ```

    Alternatively, open **Settings > Languages & Frameworks > Schemas and DTDs > JSON Schema Mappings**.
    Add the schema URL and map it to `.doco-cd.yml`, `.doco-cd.yaml`, `.doco-cd.*.yml`, and `.doco-cd.*.yaml`.
    Use this explicit mapping if your IDE version does not recognize the comment.

Schema association comments are ignored by doco-cd and do not change deployment behavior.
The schema describes one deployment mapping per YAML document, including files that separate deployments with `---`.

#### Choosing a schema version

| Schema URL                                                    | Use                                               |
|---------------------------------------------------------------|---------------------------------------------------|
| `https://doco.cd/latest/schemas/deploy.schema.json`           | The latest stable release                         |
| `https://doco.cd/v<major>.<minor>/schemas/deploy.schema.json` | Settings for a specific release series            |
| `https://doco.cd/next/schemas/deploy.schema.json`             | Unreleased/development version from `main` branch |

??? example "Schema versioning example"

    If you are using doco-cd v0.125.1, use the schema URL `https://doco.cd/v0.125/schemas/deploy.schema.json` to match the release series.
    If you are using doco-cd v0.126.0, use the schema URL `https://doco.cd/v0.126/schemas/deploy.schema.json`.

Choose the release-series URL matching your doco-cd version when using an older release.
Documentation URLs are versioned by major/minor series, not immutable patch releases.
The `next` schema becomes available when these changes are published to the development docs;
`latest` and release-series URLs become available with releases that include the schema.
For offline use, download the schema and associate a local file instead of the URL.

!!! note "Editor validation does not replace runtime validation"

    The same filenames are used for full deployments and partial [auto-discovery overrides](#auto-discovery).
    Therefore, the schema allows omitted fields, including `name`; doco-cd still checks required fields at runtime.
    Editor diagnostics also cannot check repository files, remote Docker contexts, secret-store availability, inherited cross-field constraints, or duplicate stack names across YAML documents.
    Fixed settings objects reject unknown keys in the editor, while environment variables, build arguments, and secret maps allow custom keys.

## Available Settings

The docker compose deployment can be configured inside the [deployment configuration file](#deployment-configuration-file) using the following settings:

!!! note "Settings without a default value are required."


| Key                 | Type                  | Description                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                    | Default value                                                                                                          |
|---------------------|-----------------------|--------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|------------------------------------------------------------------------------------------------------------------------|
| `name`              | string                | Name of the deployed stack / project / application.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                            |                                                                                                                        |
| `version`           | string                | Optional deployment config/artifact version for OCI-backed deployments. Currently only `doco.v1` is supported.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                 | `doco.v1`                                                                                                              |
| `reference`         | string                | Git reference to deploy from, must be either a branch (e.g. `main` or `refs/heads/main`) or tag (e.g. `v1.0.0.` or `refs/tags/v1.0.0`)                                                                                                                                                                                                                                                                                                                                                                                                                                                                                         | - Polling: the reference from the [Poll Config](Poll-Settings.md)<br/>- Webhooks: the reference of the webhook payload |
| `repository_url`    | string                | Git clone URL of another repository that contains the docker compose files to be deployed. Supports HTTP(S), SSH, native Git, and local filesystem URLs (see [Supported transports](Git-Settings.md#supported-transports)). If specified, the deployment runs from there. Also set `reference` to specify the branch.                                                                                                                                                                                                                                                                                                          | ` ` (Ignored when not specified)                                                                                       |
| `context`           | string                | [Docker context](Advanced/Docker-Contexts.md) to use for this deployment. By specifying a context, you can deploy a stack/project to a specific (remote) Docker host.                                                                                                                                                                                                                                                                                                                                                                                                                                                          | ` ` (Default Docker context)                                                                                           |
| `working_dir`       | string                | The working directory for the deployment.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                      | `.` (Root/base directory of cloned repository)                                                                         |
| `compose_files`     | array of strings      | List of docker-compose and overwrite files to use (in descending order, first file gets read first and following files [overwrite/merge](https://docs.docker.com/reference/compose-file/merge/) previous configs). Unknown/Non-existing files get skipped.                                                                                                                                                                                                                                                                                                                                                                     | `#!yaml ["compose.yaml", "compose.yml", "docker-compose.yml", "docker-compose.yaml"]`                                  |
| `environment`       | map of strings        | A map of environment variables to use for [variable interpolation](https://docs.docker.com/compose/how-tos/environment-variables/variable-interpolation) in the compose files. Overwrites entries from `env_files` with the same key/name.                                                                                                                                                                                                                                                                                                                                                                                     | `null` (No environment variables)                                                                                      |
| `env_files`         | array of strings      | List of dotenv files to use for [variable interpolation](https://docs.docker.com/compose/how-tos/environment-variables/variable-interpolation). Subsequent .env files overwrite each other and variables resolved from an earlier file are available for substitution in a later one (see [dotenv file format](#dotenv-file-format)). If the default `.env` file does not exist, it will be ignored.<br>If `repository_url` is also specified to deploy from a different repo, you can use the `remote:<filepath>` syntax to specify, that the dotenv file is located in the remote repository and should be loaded from there | `#!yaml [".env"]`                                                                                                      |
| `profiles`          | array of strings      | List of [compose profiles](https://docs.docker.com/compose/how-tos/profiles/) to use for the deployment, e.g., `#!yaml ["prod", "debug"]`.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                     | `[]`                                                                                                                   |
| `webhook_filter`    | string                | A regular expression to whitelist deployment triggers based on the webhook event payload. See the [Webhook Filter](#webhook-filter) Section below.                                                                                                                                                                                                                                                                                                                                                                                                                                                                             | ` ` (Ignored when not specified)                                                                                       |
| `remove_orphans`    | boolean               | Remove/Prune containers/services that are not (or no longer) defined in the Compose file.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                      | `true`                                                                                                                 |
| `prune_images`      | boolean               | Prune images that are no longer in use after a deployment. If the image is still used by any other container, it won't get deleted.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                            | `true`                                                                                                                 |
| `swarm`             | object                | Docker Swarm-specific settings. See [Swarm settings](#swarm-settings).                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                         | see [Swarm settings](#swarm-settings)                                                                                  |
| `force_recreate`    | boolean               | Forces the recreation/redeployment of containers even if the configuration has not changed.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                    | `false`                                                                                                                |
| `wait_running_jobs` | boolean               | Wait for currently running [scheduled jobs](Advanced/Job-Scheduling.md) to finish before deployment starts.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                    | `true`                                                                                                                 |
| `force_image_pull`  | boolean               | Always pulls the latest version of the image tags you've specified if a newer version is available.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                            | `false`                                                                                                                |
| `timeout`           | number \| Go duration | The maximum time to wait for the deployment to finish before timing out. Numeric values are seconds; duration strings (for example, `90s` or `3m`) must resolve to whole seconds.                                                                                                                                                                                                                                                                                                                                                                                                                                              | `3m`                                                                                                                   |
| `git_depth`         | number                | Limits the number of commits fetched during clone/fetch (shallow clone). `0` means use the global [`GIT_CLONE_DEPTH`](Git-Settings.md#general) value. A positive integer overrides the global setting for this deployment. When a requested ref (tag/SHA) is outside the shallow depth, doco-cd automatically deepens incrementally before falling back to a full fetch. Changing this value on an existing repo triggers an automatic re-clone.                                                                                                                                                                               | `0` (use global)                                                                                                       |
| `destroy`           | boolean \| object     | (⚠️ Destructive) Configure stack/project destruction behavior. Use `#!yaml destroy: true` as shorthand to enable destruction with default options, or use `#!yaml destroy.enabled: true` inside the object form to customize removal behavior. See [Destroy settings](#destroy-settings).                                                                                                                                                                                                                                                                                                                                      | see [Destroy settings](#destroy-settings)                                                                              |
| `auto_discovery`    | boolean \| object     | Enables [autodiscovery](#auto-discovery) of services to deploy in the working directory by scanning for subdirectories with Docker Compose files (see the `compose_files` setting). Use `auto_discovery: true` as shorthand to enable it with default options, or use the object form to customize settings such as `depth` and `delete`. See [Auto-Discovery Settings](#auto-discovery-settings).                                                                                                                                                                                                                             | see [Auto-Discovery Settings](#auto-discovery-settings)                                                                |
| `retry`             | object                | Limits automatic retries of a failed desired revision/configuration and optionally delays retries. See [Retry settings](#retry-settings).                                                                                                                                                                                                                                                                                                                                                                                                                                                                                      | Unlimited retries, no delay                                                                                            |
| `pause`             | boolean               | Freeze deployment updates without stopping workloads. Last-deployed recovery still follows `reconciliation.enabled`. See [Pause settings](#pause-settings). | `false` |
| `external_secrets_paths` | array of objects | Provider paths to fetch all secrets from in bulk, so you do not have to list every secret in `external_secrets`. Each entry has a `path`, an optional `prefix` and an optional `recursive` flag. See [Fetching Secrets in Bulk](External-Secrets/index.md#fetching-secrets-in-bulk). | `[]` |
| `reconciliation`    | boolean \| object     | Enables event-driven reconciliation for deployments. Use `#!yaml reconciliation: true` as shorthand to enable reconciliation with default options, or use the object form to customize settings. See [reconciliation settings](#reconciliation-settings).                                                                                                                                                                                                                                                                                                                                                                      | see [Reconciliation Settings](#reconciliation-settings)                                                                |


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
    
        ```ini title="base.env"
        TEST=base
        HELLO=world
        ```
        
        ```ini title="test.env in remote repository"
        TEST=changed
        ```
        
        This will result in the following environment variables being set for the deployment in the remote repository:
        
        ```ini
        TEST=changed
        HELLO=world
        ```

### Dotenv File Format

Doco-CD parses `env_files` using the same dotenv engine as Docker Compose itself ([`compose-go`](https://github.com/compose-spec/compose-go)), so the syntax 
(comments, quoting, `export`, variable substitution, etc.) matches the [Compose `.env` file reference](https://docs.docker.com/compose/how-tos/environment-variables/variable-interpolation/#env-file).

Relative `env_files` paths are resolved against the `working_dir`. Absolute paths are read as given from the doco-cd 
container filesystem, for example a secret file that you mounted into the container. 
Paths are not confined to the repository (see the [trust model](Security.md#trust-model)).

One notable difference from a plain `.env` file: variables resolved from an earlier `env_files` entry (or from `environment`) 
can be referenced with `${VAR}` in a later one, e.g. to conditionally override or pass through a value:

```ini title="base.env"
TEST_PATH=/data
```

```ini title="override.env"
TEST_PATH=${TEST_PATH} # keeps the value from base.env unless overridden here
```

### Auto-Discovery

If `auto_discovery` is enabled, doco-cd will try to find projects/stacks to deploy by searching for docker compose files 
(see the `compose_files` setting) in subdirectories in the working directory (`working_dir`). 

Doco-cd will internally generate new deploy configs based on the directory name and inherits all other settings from the 
base deploy config inside the `.doco-cd.yml` file or the inline deployment config inside the poll config.

Doco-cd scans the revision the deployment job resolved, so the discovered stacks always match the deployed revision.
If the deploy config scans another `reference` or `repository_url` (also supported in inline poll configs),
doco-cd fetches and publishes that revision first and scans the published files, so nested config files may be
encrypted with [SOPS](Advanced/Encryption.md) or be symlinks, just like in the job's own repository.

Scheduled runs and certificate-rotation reloads read the deployment configuration from its recorded config-source
artifact. A deployment's `reference` may differ from that artifact's revision; discovery resolves that reference
separately rather than assuming it points to the config-source revision.

When `auto_discovery.delete` is set to `true` and an app is no longer available in the `working_dir` (e.g. deleted or
moved to another directory outside the working dir), doco-cd will remove the deployed project/stack from the docker host.
See [Removing obsolete stacks](#removing-obsolete-stacks).

#### Auto-Discovery settings

`auto_discovery` accepts either a boolean or a nested object in the deployment configuration file. 
Use `#!yaml auto_discovery: true` to enable it with defaults, or use the object form below to customize the settings.

| Key              | Type    | Description                                                                                                     | Default value |
|------------------|---------|-----------------------------------------------------------------------------------------------------------------|---------------|
| `enabled`        | boolean | Enables auto-discovery of services to deploy in the working directory                                           | `false`       |
| `depth`          | number  | Maximum depth of subdirectories to scan for docker-compose files, set to `0` for no limit. Must not be negative | `0`           |
| `delete`         | boolean | Auto-remove obsolete auto-discovered deployments that are no longer present in the working directory            | `false`       |
| `remove_volumes` | boolean | Remove volumes of auto-discovered deployments when they are deleted                                             | `false`       |
| `remove_images`  | boolean | Remove images of auto-discovered deployments when they are deleted                                              | `true`        |

??? example "Auto-discovery Setup Example"
    <div class="grid cards" markdown>

    - With a file structure like this
      ```tree title="File structure"
      .doco-cd.yml
      apps/
        wordpress/
          docker-compose.yml
          .env
        nginx/
          docker-compose.yaml
          configs/
            nginx.conf
        misc/
          image.png
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
    This is a safer default to prevent accidental data loss (e.g., databases). Set `#!yaml remove_volumes: true` if you want volumes to be removed when stacks are auto-deleted.

#### Removing obsolete stacks

Before deploying, doco-cd looks for stacks it auto-discovered from the same repository (for OCI artifacts, the same
registry/repository and stable source tag) and [deployment target](#multiple-deployment-targets)
that no deployment config of the run has anymore, neither a discovered nor an explicit one. Stopped stacks are included.
An OCI tag moving to a new digest keeps its ownership, but independently polled tags cannot remove each other's stacks.
Missing or ambiguous stable-reference ownership preserves the stack; see
[OCI cleanup ownership](Advanced/OCI/Polling.md#auto-discovery-cleanup-ownership).

The `delete`, `remove_volumes` and `remove_images` settings of an obsolete stack are taken from the current auto-discovery
config whose `working_dir` and `depth` cover the stack's directory. If several do, all of them must allow the removal.
If none does, e.g. because the auto-discovery config was removed, the settings the stack was deployed with are used.
They are stored in the `cd.doco.deployment.auto_discovery.config` label of the stack; a stack with an empty or unreadable label is kept.
The current discovery settings still apply when a scan finds no stacks, including when the last stack was removed.
Failed scans do not authorize removal.

An obsolete stack is also kept if

- the [webhook filter](#webhook-filter) of its auto-discovery config does not match the webhook event,
- it runs a newer commit than the one doco-cd scanned, e.g. because a newer deployment finished first,
- its ownership or Git ancestry cannot be established, e.g. because the mirror lacks the deployed commit,
- it is redeployed while doco-cd removes obsolete stacks, or
- a [sync window](Advanced/Sync-Windows.md) blocks the removal.

When several references scan the same directory, the newer-commit guard uses the scan of the stack's deployed
reference, not an older scan of a different reference. Missing reference metadata, or no current scan of the
deployed reference, retains the conservative ancestry guard. All covering configs must still permit removal.

Reconciliation uses the latest request's discovery ownership and removal settings, even when a webhook filter or
sync window defers deployment. Recovery of a deferred stack still uses its previously deployed config and source.

Failed removals, inconclusive revision checks, and sync-window deferrals are retried automatically with capped backoff,
even without another webhook or source change. Each retry reads the current source configuration and checks Docker
state and removal permissions again. Restoring a stack or disabling deletion prevents the pending removal.
Actual cleanup errors fail the triggering run but do not stop unrelated deployments.

Pending retries are kept only in memory. Restarting doco-cd clears them until the source is processed again.
Cleanup retries do not deploy newly discovered stacks or restore missing workloads.

!!! note
    As the settings are stored in a label, changing `delete`, `remove_volumes` or `remove_images` recreates the containers
    or services of the discovered stacks on their next deployment.

#### Nested config overrides

For each auto-discovered compose directory, doco-cd also checks for a local [deployment config file](#deployment-configuration-file) in that directory.

If a nested config file exists, doco-cd merges it on top of the discovered deployment config.
When using [custom webhook targets](Endpoints/Webhook-Listener.md#with-custom-target), nested config files always use the standard [naming convention](#deployment-configuration-file) (`.doco-cd.y(a)ml`), not the custom target name.

!!! warning "Nested `.doco-cd.yml` files must contain exactly one YAML document."
    If a nested file contains multiple documents (`#!yaml ---`), auto-discovery fails for that run with an error.

##### Merge behavior

- Maps are merged key-by-key (`external_secrets`, `environment`, `build.args`)
- Slices replace the base value when the nested value is non-empty (this includes `external_secrets_files` and `external_secrets_paths`)
- Scalar values override the base value when the key is set in the nested config, even to `false`, `0` or an empty string.
  Keys set to `null` keep the base value.
- Nested objects (such as `build`, `destroy`, `retry`, `reconciliation`) are merged recursively.
  The boolean shorthand for `destroy`, `auto_discovery`, and `reconciliation` only sets the object's `enabled` field.
- YAML merge keys (`<<`) are resolved before these overrides are applied: explicit keys replace merged values,
  and earlier entries in a merge sequence take precedence over later ones. A replacement does not retain the
  shadowed mapping's children; an explicit `null` shadows the merged value and keeps the base setting.

!!! example "Disable a setting of the base config"
    ```yaml title="apps/postgres/.doco-cd.yml"
    remove_orphans: false
    destroy:
      remove_volumes: false # (1)!
    ```

    1. Keeps the volumes of this stack, even if the base config removes them.

##### Non-overridable Fields

The following fields are always inherited from the base/root deployment config:

- `reference`
- `repository_url`
- `auto_discovery`
- `git_depth`

#### Example

!!! example

    ```tree title="File structure"
    .doco-cd.yml
    apps/
      wordpress/
        .doco-cd.yml
        docker-compose.yml
        .env
      nginx/
        .doco-cd.yml
        docker-compose.yaml
      misc/
        image.png
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

`destroy` accepts either a boolean or a nested object in the deployment configuration file. Use `#!yaml destroy: true` to enable destructive removal with default options, or use the object form below to customize which resources are removed.

doco-cd only destroys a stack that it deployed from the same source: the repository containing the deployment configuration file,
the repository set in [`repository_url`](#available-settings), or, for [OCI artifacts](Advanced/OCI/Artifact-Usage.md), the same artifact repository
regardless of its tag or digest. The ownership check includes the canonical source URL, registry/host, and source type.
Legacy labels without a source URL must include both a source type and the full host-qualified repository name;
hostless names alone are ambiguous. Missing, conflicting, or ambiguous ownership skips destruction and is reported as a conflict.

| Key              | Type    | Description                                                                                                           | Default value |
|------------------|---------|-----------------------------------------------------------------------------------------------------------------------|---------------|
| `enabled`        | boolean | Enable destructive removal of the deployment and its resources.                                                       | `false`       |
| `remove_volumes` | boolean | Remove all volumes used by the deployment (in docker swarm mode only the volumes on the node doco-cd is connected to) | `true`        |
| `remove_images`  | boolean | Remove all images used by the deployment (currently not supported in docker swarm mode)                               | `true`        |

!!! example

    === "Boolean with default options"

        ```yaml title=".doco-cd.yml"
        name: some-project
        destroy: true
        ```
    
        This shorthand enables destruction with the default options (`#!yaml remove_volumes: true`, `#!yaml remove_images: true`).

    === "Object with custom options"

        ```yaml title=".doco-cd.yml"
        name: some-project
        destroy:
          enabled: true
          remove_volumes: true
          remove_images: false
        ```

### Pause Settings

Use `#!yaml pause: true` to freeze deployment changes without stopping or removing existing workloads.

With [`reconciliation.enabled`](#reconciliation-settings), doco-cd still restores the last-deployed configuration and revision.
It never deploys a newer revision or a changed configuration while paused.
Pause does not enable reconciliation or change its behavior when it is disabled.
For a full deployment-and-recovery freeze, combine `#!yaml pause: true` with `#!yaml reconciliation: false`:

```yaml title=".doco-cd.yml"
name: production
pause: true
reconciliation: false
```

!!! note
    doco-cd keeps the last-deployed state in memory. If doco-cd starts while a deployment is already paused,
    it does not reconcile that deployment until it is resumed. Docker restart policies and Swarm healing still apply.

#### Triggers and operational boundaries

- Pause takes effect on the next trigger that successfully resolves a valid, trusted configuration, not immediately when a Git file changes.
  The configuration source continues to be fetched so a cleared flag can be detected.
- All deployment triggers honor pause: polling, webhooks, local watchers, and manual REST/MCP polls.
  It blocks first deployment, updates, mode migration, deployment-driven destruction and cleanup, and automatic certificate reissuance/redeployment.
  Manual retry privileges, `force_recreate`, image-pull settings, and `destroy` do not bypass pause.
  Effective [inline deployment definitions](Poll-Settings.md#inline-deploy-configs) replace repository definitions and remain authoritative.
- Operations already admitted to mutation finish normally. Pause does not abort a running replacement or destruction halfway through.
- Pause is independent of [sync windows](Advanced/Sync-Windows.md); an open window or manual bypass does not override it.
  After resume, normal change detection, source trust, sync windows, and retry policies apply; paused revisions are not queued or replayed.
- Existing [scheduled jobs](Advanced/Job-Scheduling.md) keep running from their deployed definitions, even with `#!yaml pause: true`.
  Updates to those definitions wait while deployment updates are paused.
- Explicit Docker, [REST API](Endpoints/REST-API.md), and [MCP](Endpoints/MCP-Server.md) lifecycle controls (start, stop, restart, scale, destroy, or remove) remain available.
  Docker restart policies and native Swarm healing are unchanged, including with `#!yaml reconciliation: false`.

Pause differs from [preserving a service's running state](#preserve-a-services-running-state), stopping workloads,
[retry suppression](#retry-settings), or time-based sync windows.
Runs whose deployments are all paused are reported as `skipped`, not failed; mixed runs succeed if active work succeeds,
and real failures take precedence.

### Retry Settings

`retry` controls automatic retries after a failure in the deploy, post-deploy, or cleanup stage for both Compose projects and Swarm stacks.
Omitting `retry` preserves unlimited automatic retries without an additional delay.

| Key           | Type                  | Description                                                                                                                                                                                        | Default value       |
|---------------|-----------------------|----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|---------------------|
| `limit`       | integer               | Maximum number of automatic retries after the original attempt. `0` disables automatic retries; a negative value is invalid.                                                                       | Omitted (unlimited) |
| `delay`       | number \| Go duration | Fixed minimum delay since the preceding actual failure. Numeric values are seconds; Go duration strings must resolve to whole seconds. Omitted or `0` means no delay; a negative value is invalid. | `0`                 |

Use a finite budget together with a positive delay to limit repeated interruptions from a broken release.

!!!example "Retry with a delay"
    For example, this allows the original attempt plus at most two automatic retries:
    
    ```yaml title=".doco-cd.yml"
    name: example
    retry:
      limit: 2
      delay: 5m
    ```
    
    Each automatic retry is eligible five minutes after the preceding actual failure, on the next existing trigger.
    The delay does not schedule a retry itself and skipped or deferred triggers do not move its deadline or consume retries.
    A delay with an omitted budget is valid for unlimited, delayed retries; a finite budget with zero delay is also valid.
    
Once the budget is exhausted, automatic deployment attempts for that same desired revision/configuration remain paused.
The budget does not refill after the retry delay expires.
Doco-CD continues resolving the source so a new Git revision, OCI digest, effective configuration, or desired image identity can receive a fresh budget without the old delay.
A different error message does not reset the budget. Complete deployment success clears the failure and retry state.

The `retry` object inherits omitted settings from the base config.  
For example, a nested `#!yaml retry: {limit: 0}` disables automatic retries for that discovered deployment while
retaining the base delay. An explicit `#!yaml delay: 0` removes the inherited delay.

#### Manual retry

An explicit deployment trigger through the [REST polling endpoint](Endpoints/REST-API.md#polling) or the
[MCP `trigger_poll` tool](Endpoints/MCP-Server.md#poll-input) bypasses both delay and exhaustion for that attempt only,
including when `#!yaml limit: 0`. This applies with either `#!yaml wait: true` or `#!yaml wait: false`.
A failed manual attempt updates the failure details and retry-delay deadline but neither consumes nor refills the automatic retry budget;
an exhausted budget stays exhausted and a successful manual attempt clears the state.
Manual retries still obey sync windows, source trust checks, and other deployment safety checks.

Polling, webhooks, local watcher triggers, and full redeploy reconciliation share the automatic budget for the affected target.
Webhooks and reconciliation are not manual retries. Setting `#!yaml force_recreate: true` does not bypass the policy.
While an automatic retry is suppressed, the desired release remains failed even if Swarm has rolled back to a healthy older release.

!!! warning "In-memory retry state"
    The retry budget, delay, and automatic pause currently exist only in memory. Restarting Doco-CD resets them.
    Regular change detection then applies again; a restart does not guarantee that another deployment will occur.

#### Scope

Source-fetch, initialization, and pre-deploy failures retain their existing retry behavior and do not consume this budget.
Direct container restarts, scheduled jobs, and destroy operations are outside this policy.
In particular, `retry.limit` is separate from [reconciliation](#reconciliation-settings)'s container `restart_limit` and `restart_window`.

### Reconciliation Settings

Reconciliation is an optional event-driven check that compares the currently running Docker services/containers with the expected deployment state.
When configured container events occur, doco-cd either reapplies the deployment or directly restarts the affected container, depending on the event type.
Full redeploy reconciliation obeys the deployment's [retry policy](#retry-settings) but direct container restarts do not.

!!! warning
    The currently implemented state will be lost when doco-cd gets restarted and will be restored in the next poll or webhook event.

`reconciliation` accepts either a boolean or a nested object in the deployment configuration file. Use `#!yaml reconciliation: true` to enable it with defaults, or use the object form below to customize the settings.

The following settings can be used to configure reconciliation triggers.

| Key               | Type                  | Description                                                                                                                                                                                | Default value   |
|-------------------|-----------------------|--------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|-----------------|
| `enabled`         | boolean               | Enable reconciliation.                                                                                                                                                                     | `true`          |
| `events`          | array of strings      | Docker container/service events that trigger reconciliation. See [supported events](#supported-events) below.                                                                              | `["unhealthy"]` |
| `restart_timeout` | number \| Go duration | Timeout used when restarting containers for reconciliation [events](#supported-events) that trigger a restart. Numeric values are seconds; duration strings must resolve to whole seconds. | `10s`           |
| `restart_signal`  | string                | Signal used for reconciliation restarts. If not set, the `StopSignal` of the container image is used (defaults to `SIGTERM`).                                                              |                 |
| `restart_limit`   | number                | Maximum number of automatic restarts allowed for a container in the restart window. Set to `0` to disable suppression.                                                                     | `5`             |
| `restart_window`  | number \| Go duration | Time window used with `restart_limit` to detect flapping health checks. Numeric values are seconds; duration strings must resolve to whole seconds.                                        | `5m`            |

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
        For `unhealthy` events, doco-cd suppresses further automatic restarts after `restart_limit` restarts within the configured `restart_window` (see [reconciliation settings](#reconciliation-settings)).

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

### Preserve a service's running state

Docker Compose services are started automatically after they are created or updated. 
To let an external tool control a service's lifecycle, set the `cd.doco.deployment.autostart` service label to `false`.

```yaml title="docker-compose.yml"
services:
  on-demand:
    image: example/on-demand:latest
    labels:
      cd.doco.deployment.autostart: "false"
```

The label defaults to `true`. When it is `false`:

- A service without an existing container is created but not started.
- A stopped service remains stopped when it is recreated or updated.
- An existing stopped service does not cause a deployment on an otherwise unchanged poll, unless some of its containers are missing.
- A running service is restarted normally when it is recreated or updated.
- A service that remains stopped is excluded from deployment readiness checks.

The label controls only deployment-triggered startup; 
manual actions, restart policies, reconciliation, and external lifecycle tools can still start or stop the service.

Dependencies on an opted-out stopped service are not started or waited for during deployment. 
Ensure dependent services can tolerate that service being unavailable until an external tool starts it.

!!! note
    This label applies to Docker (Standalone) deployments. 
    In Docker Swarm mode, use `deploy.replicas: 0` to deploy a managed service without running tasks.

### Prevent recreation on config, secret or bind mount changes

When using docker compose with configs, secrets or bind mounts, changes to these resources will trigger a recreation of the service containers by default.
To avoid this, you can set the `cd.doco.deployment.recreate.ignore` service label to a YAML list of scopes that should be ignored for recreation.

It's a map of scope → items to ignore; `null` or an empty value tells doco-cd to ignore all items in that scope.
It accepts one or more of the following scopes: `configs`, `secrets`, `bindMounts`.

1. `configs` and `secrets` items refer to names defined in the top-level `configs` and `secrets` sections.
2. `bindMounts` items refer to the **target paths** of bind mounts (not the source paths).

Ignored files and directories from the repository are mounted from a [live copy](Reference/Artifact-Storage.md#live-files)
that doco-cd updates in place on each deployment, so the service sees the changed content without being recreated
(e.g. to reload it on a [signal](#send-signal-on-ignored-recreation) or by watching the files).
A service is recreated once when it starts or stops using live copies, e.g. after adding or changing this label.

!!! note
    Live copies are only used for Docker (Standalone) deployments.

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

### Swarm settings

The following settings can be configured in the nested `swarm` object:

| Key                | Type    | Description                                                                                                                                                                                                                                                                             | Default value       |
|--------------------|---------|-----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|---------------------|
| `enabled`          | boolean | `true` deploys a Docker Swarm stack. `false` deploys a Docker Compose project. When omitted, the Docker context determines the mode. An explicit `true` fails if the context is not a Swarm manager or [`DOCKER_SWARM_FEATURES=false`](Docker-Settings.md#swarm-environment-variables). | unset (auto-detect) |
| `config_retention` | number  | Number of old Swarm config revisions to keep per resource (excluding the active revision). `-1` disables automatic pruning. If unset, global [`DOCKER_SWARM_CONFIG_RETENTION`](Docker-Settings.md#swarm-environment-variables) is used.                                                 | unset (use global)  |
| `secret_retention` | number  | Number of old Swarm secret revisions to keep per resource (excluding the active revision). `-1` disables automatic pruning. If unset, global [`DOCKER_SWARM_SECRET_RETENTION`](Docker-Settings.md#swarm-environment-variables) is used.                                                 | unset (use global)  |

Use `swarm.enabled` to select the deployment type for one stack:

```yaml title=".doco-cd.yml"
name: netbird
swarm:
  enabled: false
```

When the value of `enabled` changes for a doco-cd-managed deployment, doco-cd removes the previous-mode project or stack before deploying the new mode.
Its volumes are retained during this migration.

#### Keep old Swarm configs/secrets

In Swarm mode, doco-cd creates content-hashed config/secret names and rotates them when content changes.

Use `swarm.config_retention` and `swarm.secret_retention` to keep old revisions:

- `0` keeps no old revisions (only active ones remain)
- `1` keeps one old revision per resource type
- `-1` disables automatic pruning for that resource type
- if unset, the corresponding global Docker setting is used:
    - [`DOCKER_SWARM_CONFIG_RETENTION`](Docker-Settings.md#swarm-environment-variables)
    - [`DOCKER_SWARM_SECRET_RETENTION`](Docker-Settings.md#swarm-environment-variables)

```yaml title=".doco-cd.yml"
name: some-project
swarm:
  config_retention: 2
  secret_retention: 1
```

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
