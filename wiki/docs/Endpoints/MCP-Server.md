---
tags:
  - Reference
  - Endpoints
  - MCP
---

# MCP Server

Doco-CD exposes a stateless [Model Context Protocol](https://modelcontextprotocol.io/) server at `POST /mcp` using Streamable HTTP transport.
The endpoint uses the main `HTTP_PORT` and follows the main server's TLS configuration.

## Enable the Server

Enable the server with `#!yaml MCP_ENABLED: true` and configure at least one
API key using `API_KEYS` or `API_KEYS_FILE` (see
[REST API Authentication](REST-API.md#authentication)):

```yaml title="docker-compose.yml"
services:
  app:
    environment:
      MCP_ENABLED: "true"
      API_KEYS: |
        - key: replace-with-a-reader-key
          role: read
```

`MCP_ENABLED` defaults to `false`. Enabling it without an API key causes
application configuration validation to fail.

## Authentication

Every request must include the same `x-api-key` header used by the [REST API](REST-API.md#authentication):

```yaml
x-api-key: your-api-key
```

!!! warning
    Inspect keys can read the raw Docker configuration of projects and
    stacks, which can contain secrets. Write and full keys can deploy, stop,
    restart, or scale workloads. A write key can also trigger a deployment
    that indirectly destroys data or a job that runs arbitrary commands. Only
    full keys can directly remove projects or stacks. Protect all API keys,
    use HTTPS, and do not expose `/mcp` to untrusted clients.

The server authenticates every request. It advertises only the tools permitted
by the supplied key's role, and direct calls to other tools are unavailable.
Roles are cumulative; see [REST API roles](REST-API.md#roles).

### Client Configuration Examples

=== "OpenCode"

    Add to `opencode.json` and set `DOCO_CD_API_KEY` in the environment that starts OpenCode:

    ```json title="opencode.json"
    {
      "$schema": "https://opencode.ai/config.json",
      "mcp": {
        "doco-cd": {
          "type": "remote",
          "url": "https://cd.example.com/mcp",
          "headers": {
            "x-api-key": "{env:DOCO_CD_API_KEY}"
          }
        }
      }
    }
    ```

=== "JetBrains (Copilot)"

    Add to `.github/mcp.json` in your repository. The key is read directly from `headers`:

    ```json title=".github/mcp.json"
    {
      "servers": {
        "doco-cd": {
          "type": "http",
          "url": "https://cd.example.com/mcp",
          "headers": {
            "x-api-key": "your-api-key"
          }
        }
      }
    }
    ```

    !!! tip
        Add `mcp.json` to `.gitignore` to avoid committing the key.

=== "VS Code (Copilot)"

    Add to `.vscode/mcp.json`. VS Code prompts for the key on first use and stores it securely:

    ```json title=".vscode/mcp.json"
    {
      "inputs": [
        {
          "type": "promptString",
          "id": "docoCdApiKey",
          "description": "Doco-CD API key",
          "password": true
        }
      ],
      "servers": {
        "doco-cd": {
          "type": "http",
          "url": "https://cd.example.com/mcp",
          "headers": {
            "x-api-key": "${input:docoCdApiKey}"
          }
        }
      }
    }
    ```

## Available Tools

| Tool                    | Minimum role  | Key Parameters                                                                                 | Description                                                                                                          |
|-------------------------|---------------|------------------------------------------------------------------------------------------------|----------------------------------------------------------------------------------------------------------------------|
| `get_health`            | `read`        | None                                                                                           | Verify access to the Docker API.                                                                                     |
| `list_deployment_runs`  | `read`        | `limit` (default 50, max 200), `status` (accepted/running/succeeded/failed/skipped), `trigger` | List recent deployment runs, optionally filtered by status and trigger.                                              |
| `get_deployment_run`    | `read`        | `job_id`                                                                                       | Get a deployment run by job ID.                                                                                      |
| `list_scheduled_jobs`   | `read`        | `stack` (filter), `context`                                                                    | List scheduler-managed jobs and their latest tracked run, optionally filtered by stack or Compose project.           |
| `list_sync_windows`     | `read`        | `repository`, `deployment`, `context`, `manual`                                                | List [sync windows](../Advanced/Sync-Windows.md) and, if a target is given, whether it may be deployed right now.    |
| `list_projects`         | `inspect`     | `all` (include stopped), `context`                                                             | List Docker Compose projects.                                                                                        |
| `get_project`           | `inspect`     | `project_name`, `context`                                                                      | Get the containers of one Docker Compose project.                                                                    |
| `control_project`       | `write`       | `project_name`, `action` (start/stop/restart), `context`, `timeout`                            | Start, stop, or restart a Docker Compose project.                                                                    |
| `destroy_project`       | `full`        | `project_name`, `volumes`, `images`, `context`                                                 | Remove a Docker Compose project and optionally its volumes and images. Reconciliation may recreate managed projects. |
| `list_stacks`           | `inspect`     | `context`                                                                                      | List Docker Swarm stacks and their services.                                                                         |
| `get_stack`             | `inspect`     | `stack_name`, `context`                                                                        | Get the services of one Docker Swarm stack.                                                                          |
| `control_stack`         | `write`       | `stack_name`, `action` (scale/restart/run), `service`, `replicas`                              | Scale, restart, or run matching services in a Docker Swarm stack.                                                    |
| `remove_stack`          | `full`        | `stack_name`, `context`                                                                        | Remove a Docker Swarm stack.                                                                                         |
| `trigger_scheduled_job` | `write`       | `job_name`, `stack`, `context`, `wait` (default `true`)                                        | Trigger one configured scheduled job immediately.                                                                    |
| `trigger_poll`          | `write`       | `configs` (array, max 32), `wait` (default `true`)                                             | Trigger one or more poll configurations immediately.                                                                 |
| `compact_mirrors`       | `write`       | `repository`, `mode` (repack/copy), `max_size`, `wait` (default `false`)                       | [Compact the Git mirrors](../Reference/Artifact-Storage.md#on-demand-compaction) into a single packfile per mirror.  |

Swarm tools are advertised when the key's role permits them. Calls fail if
Docker Swarm features are unavailable or disabled.

!!! note
    Reconciliation may modify or restore managed workloads after MCP operations, including scale, run, stop, restart, destroy, or remove, depending on the configured reconciliation events.

## Long-Running Tools

`trigger_scheduled_job` and `trigger_poll` accept a `wait` argument that defaults to `true`. `compact_mirrors` accepts a `wait` argument that defaults to `false`.

- **`#!yaml wait: true`**: the tool blocks until the operation completes and returns the final status. Application shutdown cancels active runs after a 10-second grace period. Client disconnects or request cancellation cancel the running operation.
- **`#!yaml wait: false`**: the tool returns an `accepted` status and `job_id` immediately. The job continues under the application lifecycle; neither client disconnects nor request cancellation can stop it, but application shutdown does.

Mirror compaction is also cancelled during a self-update handover. A `repack` stops and keeps the old
packfiles; a `copy` finishes the current mirror before stopping.

To poll progress with `#!yaml wait: false`:

1. Call the tool with `#!yaml wait: false` to receive a `job_id`.
2. Call `get_deployment_run` with that `job_id` until `status` is terminal.

| Status      | Terminal | Meaning                                              |
|-------------|----------|------------------------------------------------------|
| `accepted`  | No       | Job queued, not yet started.                         |
| `running`   | No       | Job is actively executing.                           |
| `succeeded` | Yes      | Job completed without error.                         |
| `failed`    | Yes      | Job encountered an error.                            |
| `skipped`   | Yes      | Job was not executed (e.g. no changes detected, deployments [paused](../Deploy-Settings.md#pause-settings), or deferred by a [sync window](../Advanced/Sync-Windows.md)). |

!!! note
    For `trigger_scheduled_job`, `succeeded` means the trigger completed without error. Compose restarts and Swarm reruns can return after Docker accepts the start or update, so this does not guarantee the workload finished successfully or exited with code 0.

## Poll Input

`trigger_poll` requires a `configs` array with at most 32 items and accepts `wait` as an optional top-level field:

```json
{
  "configs": [
    {
      "source": "git",
      "url": "https://github.com/example/application.git",
      "reference": "main"
    }
  ],
  "wait": false
}
```

Each item in `configs` accepts these fields:

| Field         | Required | Description                                                              |
|---------------|----------|--------------------------------------------------------------------------|
| `url`         | Yes      | Git repository or OCI artifact URL.                                      |
| `source`      | No       | `git` (default) or `oci`.                                                |
| `reference`   | No       | Git branch, tag, or commit. OCI references are derived from the URL.     |
| `target`      | No       | Custom deployment configuration target.                                  |
| `deployments` | No       | Inline deployment configurations, overriding the repository's config.    |

The `interval`, `schedule` and `run_once` fields are not accepted; MCP-triggered polls always run once immediately and 
count as manual deployments for [sync windows](../Advanced/Sync-Windows.md#manual-deployments).
They also bypass [deployment retry delay and exhausted-budget pause](../Deploy-Settings.md#manual-retry) for that attempt only,
including `#!yaml limit: 0` with either `#!yaml wait: true` or `#!yaml wait: false`.
A failed manual deployment neither consumes nor refills the automatic retry budget; complete success clears the failure and retry state.
Other deployment safety checks, including source trust and sync windows, still apply.
All configs in one request run with bounded concurrency controlled by `MAX_CONCURRENT_DEPLOYMENTS` (default: 4).

Manual `trigger_poll` calls do **not** bypass [`pause`](../Deploy-Settings.md#pause-settings). Inline deployments accept `"pause": true`.
With `wait=true`, a run whose deployments are all paused returns `status: "skipped"` with the pause reason and no MCP error flag.

## Logging

MCP SDK, authentication, and tool logs include the JSON field `#!yaml component: "mcp"`.
Operation logs also include this field when they use the MCP handler's logger.

Routine MCP SDK messages, including session connections and disconnections, use the `debug` level.
Set `LOG_LEVEL` to `debug` to see them. These messages are hidden at `info` and higher log levels.
Warnings, errors, and application operation logs keep their original levels.

To filter MCP logs in Loki, parse the JSON field:
`#!json {service_name="doco-cd"} | json | component="mcp"`.
Replace the `service_name` value with your deployment's label value. The `component` field does not need to be an indexed Loki label.

## Operational Notes

- The server is stateless. Clients must not depend on server-side MCP sessions between requests.
- Request bodies are limited by `MAX_PAYLOAD_SIZE`.
- Deployment run history is bounded and stored in memory; after a restart, only recoverable retained scheduled
  `one_off` executions can be reconstructed, not completed API history.
- Tool errors can include structured output such as a `job_id`; clients should inspect both the MCP error flag and the structured result fields.
- MCP tool request, error, and duration metrics are exposed by the [Prometheus endpoint](Metrics.md): `doco_cd_mcp_requests_total`, `doco_cd_mcp_errors_total`, and `doco_cd_mcp_request_duration_seconds`.
