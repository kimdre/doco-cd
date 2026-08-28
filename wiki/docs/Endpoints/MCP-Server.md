---
tags:
  - Reference
  - Endpoints
  - MCP
---

# MCP Server

Doco-CD exposes a stateless [Model Context Protocol](https://modelcontextprotocol.io/) server at `POST /mcp` using Streamable HTTP transport and MCP protocol version `2026-07-28`.
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

```text
x-api-key: your-api-key
```

For example, add this remote server to OpenCode's `opencode.json`:

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

Set `DOCO_CD_API_SECRET` in the environment that starts OpenCode. Other Streamable HTTP MCP clients can use the same endpoint and header.

!!! warning
    The MCP server includes destructive tools that can deploy, stop, restart, scale, or remove workloads. Protect `API_SECRET`, use HTTPS, and do not expose `/mcp` to untrusted clients.

## Available Tools

| Tool                    | Description                                                                                                          |
|-------------------------|----------------------------------------------------------------------------------------------------------------------|
| `get_health`            | Verify access to the Docker API.                                                                                     |
| `list_deployment_runs`  | List recent deployment runs, optionally filtered by status and trigger.                                              |
| `get_deployment_run`    | Get a deployment run by `job_id`.                                                                                    |
| `list_scheduled_jobs`   | List scheduler-managed jobs.                                                                                         |
| `list_projects`         | List Docker Compose projects.                                                                                        |
| `get_project`           | Get the containers of one Docker Compose project.                                                                    |
| `control_project`       | Start, stop, or restart a Docker Compose project.                                                                    |
| `destroy_project`       | Remove a Docker Compose project and optionally its volumes and images. Reconciliation may recreate managed projects. |
| `list_stacks`           | List Docker Swarm stacks and their services.                                                                         |
| `get_stack`             | Get the services of one Docker Swarm stack.                                                                          |
| `control_stack`         | Scale, restart, or run matching services in a Docker Swarm stack.                                                    |
| `remove_stack`          | Remove a Docker Swarm stack.                                                                                         |
| `trigger_scheduled_job` | Initiate one configured scheduled job trigger immediately.                                                           |
| `trigger_poll`          | Trigger one or more poll configurations immediately.                                                                 |

Swarm tools are always advertised. Calls fail if Docker Swarm features are unavailable or disabled.

!!! note
    Reconciliation may modify or restore managed workloads after MCP operations, including scale, run, stop, restart, destroy, or remove, depending on the configured reconciliation events.

## Long-Running Tools

`trigger_scheduled_job` and `trigger_poll` accept a `wait` argument that defaults to `true`.

- Use `#!yaml wait: false` for long-running operations. The tool returns an accepted `job_id` immediately.
- Pass the `job_id` to `get_deployment_run` until the status is terminal: `succeeded`, `failed`, or `skipped`.
- With `#!yaml wait: true`, cancelling the client request cancels the running operation. During application shutdown, active HTTP requests receive the server's graceful shutdown period before remaining work is cancelled.
- With `#!yaml wait: false`, the accepted job uses the application lifecycle rather than the completed request lifecycle. Cancelling or disconnecting the client after the response does not cancel the job, but application shutdown does.

For `trigger_scheduled_job`, `succeeded` means that the trigger operation completed without error. Compose restarts and Swarm reruns can return after Docker accepts the start or update, so this status does not guarantee that the scheduled workload finished successfully or that its process exited with code 0.

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

- `source`: `git` (default) or `oci`.
- `url`: Git repository or OCI artifact URL.
- `reference`: optional Git reference. OCI references are derived from the artifact URL.
- `target`: optional custom deployment configuration target.
- `deployments`: optional inline deployment configurations.

The scheduled polling fields `interval` and `run_once` are not accepted because MCP-triggered polls always run once immediately.
Poll configurations in one request run with bounded concurrency controlled by `MAX_CONCURRENT_DEPLOYMENTS` (default: 4).

## Operational Notes

- The server is stateless. Clients must not depend on server-side MCP sessions between requests.
- Request bodies are limited by `MAX_PAYLOAD_SIZE`.
- Deployment run history is stored in memory and is lost when doco-cd restarts.
- Tool errors can include structured output such as a `job_id`; clients should inspect both the MCP error flag and structured result.
- MCP tool request, error, and duration metrics are exposed by the [Prometheus endpoint](Metrics.md): `doco_cd_mcp_requests_total`, `doco_cd_mcp_errors_total`, and `doco_cd_mcp_request_duration_seconds`.
