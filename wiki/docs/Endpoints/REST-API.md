---
tags:
  - Reference
  - Endpoints
  - REST API
---

# REST API

Doco-CD exposes a RESTful API at the `/v1/api` endpoint.
Set both `HTTP_TLS_CERT_FILE` and `HTTP_TLS_KEY_FILE` if you want doco-cd itself to serve the API over HTTPS.

## OpenAPI documentation

Set `OPENAPI_ENABLED=true` to expose the public [OpenAPI](https://www.openapis.org/) 3.2 documentation. The feature is disabled by default.
Protected operations describe their required permission and the least-privileged role that grants it.

| API surface     | OpenAPI document         | Swagger UI        |
|-----------------|--------------------------|-------------------|
| REST and health | `/openapi/rest.json`     | `/docs/`          |
| Webhooks        | `/openapi/webhooks.json` | `/docs/webhooks/` |

## Authentication

Configure one or more keys with `API_KEYS` to enable the API. Each entry must
have a unique, nonempty `key` and either a `role` or an explicit `permissions`
list. A key must be a valid HTTP header value without leading or trailing
spaces or tabs:

```yaml title="docker-compose.yml"
services:
  app:
    environment:
      API_KEYS: |
        - key: replace-with-a-reader-key
          role: read
        - key: replace-with-an-inspector-key
          role: inspect
        - key: replace-with-a-writer-key
          role: write
        - key: replace-with-an-admin-key
          role: full
        - key: replace-with-a-poller-key
          permissions:
            - poll:trigger
            - runs:read
```

Alternatively, set `API_KEYS_FILE` to the path of a file containing the same
YAML list. The file may be plaintext or [SOPS-encrypted](../Advanced/Encryption.md).
Provide the relevant SOPS decryption key to the container when using an
encrypted file. Set only one of `API_KEYS` and `API_KEYS_FILE`.
An empty list disables the REST API.

!!! note "SOPS cannot encrypt a top-level YAML list as a structured YAML file." 
    Encrypt the list as binary data instead:
    
    ```sh
    sops encrypt --age <age-public-key> --input-type binary --output-type binary \
      api-keys.yaml > api-keys.enc
    ```

    Mount `api-keys.enc` as a secret or file and set `API_KEYS_FILE` to its path
    inside the container. Keep the unencrypted file out of Git.

Doco-CD loads the keys only at startup. A restart does not apply changes to `API_KEYS`
or other environment variables. Recreate the container instead, for example
with `docker compose up -d` or a [self-update](../Advanced/Self-Updating.md)
if doco-cd manages its own container. Changes to the content of a mounted `API_KEYS_FILE` take effect at the next start.

!!! warning "Migration from API_SECRET"
    `API_SECRET` and `API_SECRET_FILE` are no longer supported. Remove both
    variables from the container environment, even if they are empty, and
    configure `API_KEYS` or `API_KEYS_FILE` instead. To preserve the previous
    full access, assign the old key the `full` role. Doco-CD fails to
    start while either legacy variable is present. See the
    [self-update migration guidance](../Advanced/Self-Updating.md#api-key-migration)
    if doco-cd manages its own container.

Use the `x-api-key` header to authenticate requests with one configured key.

Example:

```sh
curl -H "x-api-key: your_api_key" http://example.com/v1/api/projects
```

Missing or invalid credentials return `401 Unauthorized`. A valid key without
the required permission returns `403 Forbidden`. The health, webhook, metrics,
profiling, and public OpenAPI endpoints have separate access rules.

### Roles

Roles are cumulative: `inspect` includes `read`, `write` includes `inspect`,
and `full` includes all other roles.

| Minimum role | REST operations                                                                                          |
|--------------|----------------------------------------------------------------------------------------------------------|
| `read`       | GET operations for deployment runs, scheduled jobs, and sync windows.                                    |
| `inspect`    | GET operations that list or get Compose projects and Swarm stacks.                                       |
| `write`      | POST operations to control projects or stacks, trigger scheduled jobs or polls, and compact Git mirrors. |
| `full`       | DELETE operations that remove Compose projects or Swarm stacks.                                          |

### Explicit permissions

Instead of a role, a key can specify a nonempty `permissions` list. Every
permission must be known and appear only once. `role` and `permissions` are
mutually exclusive, even if one is empty or null. Explicit permissions grant
only the listed capabilities; they do not imply other permissions. For example,
add `runs:read` to `poll:trigger` if the client must check deployment runs.

| Permission          | Minimum preset role | Capability                                                             |
|---------------------|---------------------|------------------------------------------------------------------------|
| `health:read`       | `read`              | Use the MCP `get_health` tool; `/v1/health` has separate access rules. |
| `jobs:read`         | `read`              | List scheduled jobs.                                                   |
| `jobs:trigger`      | `write`             | Trigger a scheduled job.                                               |
| `mirrors:compact`   | `write`             | Compact Git mirrors.                                                   |
| `poll:trigger`      | `write`             | Trigger poll configurations.                                           |
| `projects:control`  | `write`             | Start, stop, restart, or force-recreate Compose projects.              |
| `projects:delete`   | `full`              | Remove Compose projects.                                               |
| `projects:inspect`  | `inspect`           | List and inspect Compose projects.                                     |
| `runs:read`         | `read`              | List and inspect deployment runs.                                      |
| `stacks:control`    | `write`             | Scale, restart, or run Swarm services.                                 |
| `stacks:delete`     | `full`              | Remove Swarm stacks.                                                   |
| `stacks:inspect`    | `inspect`           | List and inspect Swarm stacks.                                         |
| `sync_windows:read` | `read`              | List sync windows and their status.                                    |

Roles and explicit permissions also control [MCP tools](MCP-Server.md#available-tools).
They restrict operation types across all resources and Docker contexts, not
individual projects, stacks, jobs, or the effects of deployments and jobs.

!!! warning "Privileged permissions"
    Project and stack inspection exposes raw Docker configuration, including
    container commands, labels, mounts, and Swarm service environment
    variables, which can contain secrets. Grant `projects:inspect` or
    `stacks:inspect` only to trusted clients; use `runs:read` for clients that
    only need deployment status.

    A key with `poll:trigger` can deploy arbitrary workloads or trigger a poll
    containing `destroy: true`. A key with `jobs:trigger` can run configured
    jobs that execute arbitrary commands. These operations can indirectly
    destroy data or grant host access. `projects:delete` and `stacks:delete`
    additionally allow direct removal. Read permissions can expose deployment
    metadata.

## Query Parameters

Management endpoints support these common query parameters:

| Query Parameter | Type    | Description                                                                          |
|-----------------|---------|--------------------------------------------------------------------------------------|
| `context`       | string  | Docker context for project, stack, and scheduled-job endpoints (default: `default`). |
| `timeout`       | integer | Timeout in seconds (default: `30`).                                                  |

## Docker context selection

Project, stack, and scheduled-job endpoints accept one optional `context` query parameter. If it is omitted or set to `default`, the endpoint uses the default Docker context. 
Named contexts must exist in the Docker CLI context store available to doco-cd.

These endpoints return the selected external context name in the `X-Doco-CD-Context` response header. Their JSON response shapes do not change.

```sh
curl -i -H "x-api-key: your_api_key" \
  "http://example.com/v1/api/projects?context=remote"
```

## Endpoints


### Health Check

Doco-CD exposes a health check endpoint at `/v1/health` that, if the application is healthy, returns a `200` status code and the following JSON response:

```json
{
  "details": "healthy"
}
```

If the application is not healthy, the endpoint returns a `503` status code and the following JSON response:

```json
{
  "details": "unhealthy",
  "error": "some error message"
}
```

### Deployment Runs

The API tracks deployment-related runs (for example webhook-triggered deployments, API-triggered poll runs, and automatic, manual, or recovered scheduled-job executions) in memory.
Use these endpoints to inspect the current status and recent history by `job_id`.
Each run's `deployments` collection reports the resolved stack and Docker context targets. A single poll or webhook run can contain targets from multiple contexts.
Automatic attempts suppressed by a [deployment retry policy](../Deploy-Settings.md#retry-settings) are skipped, not successful.
The per-target reason distinguishes `delay` from `budget_exhausted` and suppression retains the failed desired release's state.
Runs whose deployments are all [paused](../Deploy-Settings.md#pause-settings) are also `skipped`, with a pause reason.
Mixed paused/active runs succeed if active work succeeds; any real failure takes precedence.

| Endpoint              | Method | Description                           | Query Parameters                                                                                                                                                                                                                      |
|-----------------------|--------|---------------------------------------|---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| `/v1/api/runs`        | GET    | List recent tracked deployment runs   | - `limit` (integer, default: `50`, max: `200`)<br/>- `status` (string, optional): `accepted`, `running`, `succeeded`, `failed`, `skipped`<br/>- `trigger` (string, optional): `webhook`, `poll`, `scheduled_job`, `mirror_compaction` |
| `/v1/api/run/{jobID}` | GET    | Get details for a specific run/job ID |                                                                                                                                                                                                                                       |

History is bounded by the configured per-trigger limit (currently 50 runs in the application) and a seven-day TTL.
All run timestamps use doco-cd's configured timezone (`TZ`), including the UTC offset for each timestamp.
Accepted and running records are protected from eviction until they become terminal. History is not persisted across
doco-cd restarts; retained `one_off` scheduled executions can be reconstructed during recovery.

#### Example Requests

```sh
curl --request GET \
  --url 'https://cd.example.com/v1/api/runs?status=running&limit=20' \
  --header 'x-api-key: your-api-key'
```

```sh
curl --request GET \
  --url 'https://cd.example.com/v1/api/run/550e8400-e29b-41d4-a716-446655440000' \
  --header 'x-api-key: your-api-key'
```

### Polling

| Endpoint           | Method | Description                                                    | Query Parameters                                                                        |
|--------------------|--------|----------------------------------------------------------------|-----------------------------------------------------------------------------------------|
| `/v1/api/poll/run` | POST   | Trigger a poll run for all polling targets in the body (JSON). | - `wait` (boolean, default: `true`): Wait for the poll run to finish before responding. |

The request body must be a JSON array of [poll configurations](../Poll-Settings.md), each containing at least a `url` field containing the Git clone URL to the repository.

The fields `run_once`, `interval` and `schedule` will be ignored for poll runs triggered via the API, as they are only relevant for the scheduled poll runs.
They are still validated when the request is parsed: a configuration that sets both a `schedule` and a non-zero `interval` is rejected with `400 Bad Request`, because the two are mutually exclusive.

Poll runs triggered via the API count as manual deployments for [sync windows](../Advanced/Sync-Windows.md#manual-deployments). If a sync window defers every deployment of the run, the run status is `skipped` and, with `wait=true`, the endpoint responds with `202 Accepted` and a message like `deployment of my-app deferred by sync window business-hours until 2026-01-05T08:00:00+01:00`.

API-triggered polls also bypass the deployment's [retry delay and exhausted-budget pause](../Deploy-Settings.md#manual-retry)
for that attempt only, including `#!yaml limit: 0`. This applies to both synchronous (`#!ini wait=true`) and asynchronous (`#!ini wait=false`) runs.
A failed manual deployment does not consume or refill the automatic retry budget but a complete success clears the failure and retry state.
Source trust checks, sync windows, and other safety checks still apply. Automatic webhooks and redeploy reconciliation do not receive this bypass.

Manual polls do **not** bypass [`pause`](../Deploy-Settings.md#pause-settings). Inline deployments accept `"pause": true`.
With `wait=true`, a poll whose deployments are all paused returns `202 Accepted` with the pause reason as `content`.

#### Example Request

Minimal example using the default settings:

```sh
curl --request POST \
  --url 'https://cd.example.com/v1/api/poll/run?wait=true' \
  --header 'content-type: application/json' \
  --header 'x-api-key: your-api-key' \
  --data '[
  {
    "url": "https://github.com/your/repo.git",
  }
]'
```

Example with custom reference and inline deployment configuration:

```sh
curl --request POST \
  --url 'https://cd.example.com/v1/api/poll/run?wait=true' \
  --header 'content-type: application/json' \
  --header 'x-api-key: your-api-key' \
  --data '[
  {
    "url": "https://github.com/your/repo.git",
    "reference": "dev",
    "deployments": [
      {
        "name": "my-app",
        "working_dir": "/app",
        "env_files": [
          ".env"
        ]
      }
    ]
  }
]'
```

### Scheduled Jobs

| Endpoint                    | Method | Description                                                                      | Query Parameters                                                                                                                                                           |
|-----------------------------|--------|----------------------------------------------------------------------------------|----------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| `/v1/api/jobs`              | GET    | List all discovered [scheduled jobs](../Advanced/Job-Scheduling.md)              | - `stack` (string, optional): Return scheduled jobs only for one stack/project.                                                                                            |
| `/v1/api/job/{jobName}/run` | POST   | Trigger a configured [scheduled job](../Advanced/Job-Scheduling.md) immediately. | - `stack` (string, optional): Limit matching to a specific stack/project.<br/>- `wait` (boolean, default: `true`): Wait for the triggered run to finish before responding. |

For Compose `one_off` jobs, `status` is `running` while an execution is active. Otherwise, if the source service
container is inactive (`created`, `exited`, or `dead`), `status` shows the most recent recorded one-off exit status,
such as `exited (0)` or `exited (7)`, instead of an older exit status of the source container. An active source
container keeps its own state. Exit results are kept in memory. If no result is available, `status` falls back to the
source container's state. This occurs after a doco-cd restart without a recovered execution, or after a run that failed
without an exit code, for example because the image pull failed. Compose `restart` jobs use the source container's state.

If a job sets a [job timeout](../Advanced/Job-Scheduling.md#job-timeout), the `timeout` field contains the configured
value, for example `2h0m0s`. A run that exceeds the timeout fails with the message `exceeded timeout of <timeout>`.
The `timeout` field is omitted when no timeout is configured.

??? question "What is the `jobName` for a scheduled job?"
    `jobName` is the runtime name of the scheduled target:

    - Docker (standalone): the container name (for example `my-stack-backup-1`)
    - Docker Swarm: the service name (for example `my-stack_backup`)

??? question "How do I find the `jobName` for a scheduled job?"
    You can get the `jobName` from the scheduler logs (`"job":"..."`) or from `GET /v1/api/jobs`.

- If multiple jobs share the same `jobName`, provide `stack` to disambiguate for the run endpoint.
- If the same job and stack names exist on multiple Docker contexts, provide `context`.
- If the matched job is disabled, the run endpoint returns a conflict response.

**Common run endpoint outcomes**

- `200 OK`: run triggered and completed (`wait=true`).
- `202 Accepted`: run accepted and running in background (`wait=false`).
- `404 Not Found`: no scheduled job matched `jobName` (and optional `stack`).
- `409 Conflict`: matched job is disabled or the selection is ambiguous.

#### Example Request

=== "Trigger a job by container name"

    ```sh
    curl --request POST \
      --url 'https://cd.example.com/v1/api/job/my-stack-backup-1/run?wait=true' \
      --header 'x-api-key: your-api-key'
    ```

=== "Trigger a job by service name in a specific stack"

    ```sh
    curl --request POST \
      --url 'https://cd.example.com/v1/api/job/backup/run?stack=my-stack&wait=true' \
      --header 'x-api-key: your-api-key'
    ```

=== "List all scheduled jobs for a specific stack"

    ```sh
    curl --request GET \
      --url 'https://cd.example.com/v1/api/jobs?stack=my-stack' \
      --header 'x-api-key: your-api-key'
    ```

### Sync Windows

| Endpoint               | Method | Description                                                                                                                      | Query Parameters                                                                                                                                                                                                                                                                                                                                                                     |
|------------------------|--------|----------------------------------------------------------------------------------------------------------------------------------|--------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| `/v1/api/sync-windows` | GET    | List the configured [sync windows](../Advanced/Sync-Windows.md) and, if a target is given, whether it may be deployed right now. | - `repository` (string, optional): Repository name to evaluate, e.g. `github.com/acme/app`.<br/>- `deployment` (string, optional): Deployment (Compose project or Swarm stack) name to evaluate.<br/>- `context` (string, optional): Docker context name to evaluate (default: `default`).<br/>- `origin` (string, optional): `automatic` (default) or `manual` (API/MCP poll runs). |

The response lists every window with its settings and current state (`active`, `active_since`, `active_until` and `next_start`).
If at least one of `repository`, `deployment` or `context` is given, the response also contains a `decision` for that target:

| Field             | Description                                                                                                |
|-------------------|------------------------------------------------------------------------------------------------------------|
| `allowed`         | Whether the target may be deployed right now.                                                              |
| `manual_override` | `true` if the deployment is only allowed because it is manual and every blocking window has `manual_sync`. |
| `windows`         | Names of the windows that block (or, with `manual_override`, would block) the deployment.                  |
| `next_open`       | Earliest time the target may be deployed again, if it is blocked and the time is known.                    |

#### Example Request

```sh
curl --request GET \
  --url 'https://cd.example.com/v1/api/sync-windows?repository=github.com/acme/app&deployment=web' \
  --header 'x-api-key: your-api-key'
```

```json title="Response"
{
  "content": {
    "time": "2026-01-03T10:00:00+01:00",
    "windows": [
      {
        "name": "weekend-freeze",
        "kind": "deny",
        "schedule": "0 18 * * 5",
        "duration": "62h0m0s",
        "timezone": "Europe/Berlin",
        "repositories": [],
        "deployments": [],
        "contexts": [],
        "manual_sync": false,
        "active": true,
        "active_since": "2026-01-02T18:00:00+01:00",
        "active_until": "2026-01-05T08:00:00+01:00",
        "next_start": "2026-01-09T18:00:00+01:00"
      }
    ],
    "decision": {
      "allowed": false,
      "manual_override": false,
      "windows": ["weekend-freeze"],
      "next_open": "2026-01-05T08:00:00+01:00"
    }
  },
  "job_id": "550e8400-e29b-41d4-a716-446655440000"
}
```

### Storage

| Endpoint                  | Method | Description                                                                                                                               | Query Parameters                                                                                                                                                                                                                                                                                                                                                                                                                                                          |
|---------------------------|--------|-------------------------------------------------------------------------------------------------------------------------------------------|---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| `/v1/api/storage/compact` | POST   | [Compact the Git mirrors](../Reference/Artifact-Storage.md#on-demand-compaction) in the data directory into a single packfile per mirror. | - `repository` (string, optional): Only compact the mirrors of this repository, e.g. `github.com/acme/app` or a clone URL.<br/>- `mode` (string, default: `repack`): `repack` re-encodes all objects, `copy` only consolidates the packfiles.<br/>- `max_size` (integer, default: `268435456`): Size in bytes above which `repack` skips a mirror. `0` disables the limit.<br/>- `wait` (boolean, default: `false`): Wait for the compaction to finish before responding. |

The compaction is tracked as a run with the trigger `mirror_compaction`. With `wait=true`, the response contains
the finished run. Its `status` is `succeeded` if at least one mirror was compacted and `skipped` if none was, and
its `message` summarizes the result of every mirror.

**Common outcomes**

- `200 OK`: compaction finished (`wait=true`).
- `202 Accepted`: compaction started in the background (`wait=false`). Use the `job_id` with `/v1/api/run/{jobID}`.
- `400 Bad Request`: unknown `mode` or invalid `max_size`.
- `404 Not Found`: no Git mirror exists, or none of the given `repository`.
- `409 Conflict`: another compaction is running. The `job_id` of the response is the one of that run.
- `500 Internal Server Error`: a mirror failed to compact (`wait=true`). The response contains the summary.
- `503 Service Unavailable`: doco-cd is shutting down and cancelled the compaction or did not start it.

#### Example Request

```sh
curl --request POST \
  --url 'https://cd.example.com/v1/api/storage/compact?wait=true' \
  --header 'x-api-key: your-api-key'
```

```json title="Response"
{
  "content": {
    "job_id": "550e8400-e29b-41d4-a716-446655440000",
    "trigger": "mirror_compaction",
    "status": "succeeded",
    "message": "repack of 3 mirrors: 2 compacted, 1 skipped_busy; packfiles 22.2 MiB -> 12.1 MiB",
    "created_at": "2026-01-03T10:00:00.120391+01:00",
    "started_at": "2026-01-03T10:00:00.120584+01:00",
    "finished_at": "2026-01-03T10:00:09.412873+01:00",
    "updated_at": "2026-01-03T10:00:09.412873+01:00"
  },
  "job_id": "550e8400-e29b-41d4-a716-446655440000"
}
```

### Compose Projects

!!! note
    Project management endpoints are only available for compose projects, and will not work for Swarm stacks. To manage Swarm stacks, see the [Swarm Stacks](#swarm-stacks) section below.

| Endpoint                                 | Method | Description                         | Query Parameters                                                                                                                                           |
|------------------------------------------|--------|-------------------------------------|------------------------------------------------------------------------------------------------------------------------------------------------------------|
| `/v1/api/projects`                       | GET    | List all deployed compose projects  | - `all` (boolean, default: `false`): Return all projects including inactive ones.                                                                          |
| `/v1/api/project/{projectName}`          | GET    | Get details of a project            |                                                                                                                                                            |
| `/v1/api/project/{projectName}`          | DELETE | Remove a project                    | - `volumes` (boolean, default: `true`): remove all associated volumes.<br/>- `images` (boolean, default: `true`): remove all associated images.            |
| `/v1/api/project/{projectName}/start`    | POST   | Start a project                     |                                                                                                                                                            |
| `/v1/api/project/{projectName}/stop`     | POST   | Stop a project                      |                                                                                                                                                            |
| `/v1/api/project/{projectName}/restart`  | POST   | Restart a project                   |                                                                                                                                                            |
| `/v1/api/project/{projectName}/recreate` | POST   | Force-recreate a project or service | - `service` (string, optional): Name of the service to recreate.<br/>- `timeout` (integer, default: `30`): Time in seconds to wait for containers to stop. |

Managed recreation uses the revision recorded on the deployment, not the latest
branch or tag. For legacy Git deployments from the same repository as their deploy
config, it exports the deployed commit from the cached mirror if necessary and
moves the recreated services to the [artifact layout](../Reference/Artifact-Storage.md#upgrading-from-v0119x-or-earlier).
There is no need to stop the project before recreating it.

Managed recreation returns `409 Conflict` when the cached Git/OCI source cannot
be verified or the deployed Git commit cannot be exported from the mirror. Run a
normal deployment with `#!yaml force_recreate: true` to refresh the source cache and
deployment metadata. To see the error response in scripts, use
`curl --fail-with-body` rather than `curl --fail`, and check its exit status before
reporting success.

### Swarm Stacks

!!! note 
    Stack management endpoints are only available if Doco-CD is running in a Docker Swarm environment.

| Endpoint                            | Method | Description                                                                                                       | Query Parameters                                                                                                                                                                           |
|-------------------------------------|--------|-------------------------------------------------------------------------------------------------------------------|--------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| `/v1/api/stacks`                    | GET    | List all deployed Swarm stacks                                                                                    |                                                                                                                                                                                            |
| `/v1/api/stack/{stackName}`         | GET    | Get details of a Swarm stack                                                                                      |                                                                                                                                                                                            |
| `/v1/api/stack/{stackName}`         | DELETE | Remove a Swarm stack                                                                                              |                                                                                                                                                                                            |
| `/v1/api/stack/{stackName}/scale`   | POST   | Rescale a Swarm stack or service                                                                                  | - `replicas` (integer): Scale to n replicas.<br/>- `service` (string, optional): Name of service to scale.<br/>- `wait` (boolean, default: `true`): Wait for service to be running/healthy | 
| `/v1/api/stack/{stackName}/restart` | POST   | Restart/Redeploy a Swarm stack or service                                                                         | - `service` (string, optional): Name of service to restart.                                                                                                                                | 
| `/v1/api/stack/{stackName}/run`     | POST   | Trigger one or all [jobs](https://docs.docker.com/reference/cli/docker/service/create/#running-as-a-job) in stack | - `service` (string, optional): Name of the job service to run.                                                                                                                            |

## Example Request

### List all deployed compose projects

```sh
curl -H "x-api-key: your_api_key" "http://example.com/v1/api/projects?all=true"
```

### Remove a specific project without removing associated volumes

```sh
curl -X DELETE -H "x-api-key: your_api_key" "http://example.com/v1/api/project/my_project?volumes=false"
```

### Restart a specific project with a custom timeout

```sh
curl -X POST -H "x-api-key: your_api_key" "http://example.com/v1/api/project/my_project/restart?timeout=60"
```

### Force-recreate all services in a Compose project

```sh
curl -X POST -H "x-api-key: your_api_key" "http://example.com/v1/api/project/my_project/recreate"
```

### Force-recreate a specific Compose service

```sh
curl -X POST -H "x-api-key: your_api_key" "http://example.com/v1/api/project/my_project/recreate?service=web"
```
