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

Enable the server with `#!yaml MCP_ENABLED: true` and provide an API secret using `API_SECRET` or `API_SECRET_FILE` (see [REST API Authentication](REST-API.md#authentication)):

```yaml title="docker-compose.yml"
services:
  app:
    environment:
      MCP_ENABLED: "true"
      API_SECRET: your-api-key
```

`MCP_ENABLED` defaults to `false`. Enabling it without an API secret causes application configuration validation to fail.

## Authentication

Every request must include the same `x-api-key` header used by the [REST API](REST-API.md#authentication):

```yaml
x-api-key: your-api-key
```

!!! warning
    The MCP server includes destructive tools that can deploy, stop, restart, scale, or remove workloads. 
    Protect `API_SECRET`, use HTTPS, and do not expose `/mcp` to untrusted clients.

### Client Configuration Examples

=== "OpenCode"

    Add to `opencode.json` and set `DOCO_CD_API_SECRET` in the environment that starts OpenCode:

    ```json title="opencode.json"
    {
      "$schema": "https://opencode.ai/config.json",
      "mcp": {
        "doco-cd": {
          "type": "remote",
          "url": "https://cd.example.com/mcp",
          "headers": {
            "x-api-key": "{env:DOCO_CD_API_SECRET}"
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

| Tool                    | Key Parameters                                                                                 | Description                                                                                                          |
|-------------------------|------------------------------------------------------------------------------------------------|----------------------------------------------------------------------------------------------------------------------|
| `get_health`            | —                                                                                              | Verify access to the Docker API.                                                                                     |
| `list_deployment_runs`  | `limit` (default 50, max 200), `status` (accepted/running/succeeded/failed/skipped), `trigger` | List recent deployment runs, optionally filtered by status and trigger.                                              |
| `get_deployment_run`    | `job_id`                                                                                       | Get a deployment run by job ID.                                                                                      |
| `list_scheduled_jobs`   | `stack` (filter), `context`                                                                    | List scheduler-managed jobs, optionally filtered by stack or Compose project.                                        |
| `list_projects`         | `all` (include stopped), `context`                                                             | List Docker Compose projects.                                                                                        |
| `get_project`           | `project_name`, `context`                                                                      | Get the containers of one Docker Compose project.                                                                    |
| `control_project`       | `project_name`, `action` (start/stop/restart), `context`, `timeout`                            | Start, stop, or restart a Docker Compose project.                                                                    |
| `destroy_project`       | `project_name`, `volumes`, `images`, `context`                                                 | Remove a Docker Compose project and optionally its volumes and images. Reconciliation may recreate managed projects. |
| `list_stacks`           | `context`                                                                                      | List Docker Swarm stacks and their services.                                                                         |
| `get_stack`             | `stack_name`, `context`                                                                        | Get the services of one Docker Swarm stack.                                                                          |
| `control_stack`         | `stack_name`, `action` (scale/restart/run), `service`, `replicas`                              | Scale, restart, or run matching services in a Docker Swarm stack.                                                    |
| `remove_stack`          | `stack_name`, `context`                                                                        | Remove a Docker Swarm stack.                                                                                         |
| `trigger_scheduled_job` | `job_name`, `stack`, `context`, `wait` (default `true`)                                        | Trigger one configured scheduled job immediately.                                                                    |
| `trigger_poll`          | `configs` (array, max 32), `wait` (default `true`)                                             | Trigger one or more poll configurations immediately.                                                                 |

Swarm tools are always advertised. Calls fail if Docker Swarm features are unavailable or disabled.

!!! note
    Reconciliation may modify or restore managed workloads after MCP operations, including scale, run, stop, restart, destroy, or remove, depending on the configured reconciliation events.

## Long-Running Tools

`trigger_scheduled_job` and `trigger_poll` accept a `wait` argument that defaults to `true`.

- **`#!yaml wait: true`**: the tool blocks until the operation completes and returns the final status. Application shutdown cancels active runs after a 10-second grace period. Client disconnects do **not** cancel the running operation.
- **`#!yaml wait: false`**: the tool returns an `accepted` status and `job_id` immediately. The job continues under the application lifecycle; neither client disconnects nor request cancellation can stop it, but application shutdown does.

To poll progress with `#!yaml wait: false`:

1. Call the tool with `#!yaml wait: false` to receive a `job_id`.
2. Call `get_deployment_run` with that `job_id` until `status` is terminal.

| Status      | Terminal | Meaning                                              |
|-------------|----------|------------------------------------------------------|
| `accepted`  | No       | Job queued, not yet started.                         |
| `running`   | No       | Job is actively executing.                           |
| `succeeded` | Yes      | Job completed without error.                         |
| `failed`    | Yes      | Job encountered an error.                            |
| `skipped`   | Yes      | Job was not executed (e.g. no changes detected).     |

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

The `interval` and `run_once` fields are not accepted; MCP-triggered polls always run once immediately.
All configs in one request run with bounded concurrency controlled by `MAX_CONCURRENT_DEPLOYMENTS` (default: 4).

## Operational Notes

- The server is stateless. Clients must not depend on server-side MCP sessions between requests.
- Request bodies are limited by `MAX_PAYLOAD_SIZE`.
- Deployment run history is stored in memory and is lost when doco-cd restarts.
- Tool errors can include structured output such as a `job_id`; clients should inspect both the MCP error flag and the structured result fields.
- MCP tool request, error, and duration metrics are exposed by the [Prometheus endpoint](Metrics.md): `doco_cd_mcp_requests_total`, `doco_cd_mcp_errors_total`, and `doco_cd_mcp_request_duration_seconds`.
