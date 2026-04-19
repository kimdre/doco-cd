---
tags:
  - Reference
  - Endpoints
  - REST API
---

# REST API

Doco-CD exposes a RESTful API at the `/v1/api` endpoint.

## Authentication

Set the `API_SECRET` or `API_SECRET_FILE` environment variable in the container to enable the API, see [App Settings](../App-Settings.md#available-settings).

Use the `x-api-key` header to authenticate requests to the API using the secret value.

Example:

```sh
curl -H "x-api-key: your_api_key" http://example.com/v1/api/projects
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

### Polling

| Endpoint           | Method | Description                                                    | Query Parameters                                                                        |
|--------------------|--------|----------------------------------------------------------------|-----------------------------------------------------------------------------------------|
| `/v1/api/poll/run` | POST   | Trigger a poll run for all polling targets in the body (JSON). | - `wait` (boolean, default: `true`): Wait for the poll run to finish before responding. |

The request body must be a JSON array of [poll configurations](../Poll-Settings.md), each containing at least a `url` field containing the Git clone URL to the repository.

The fields `run_once` and `interval` will be ignored for poll runs triggered via the API, as they are only relevant for the scheduled poll runs.

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

### Compose Projects

!!! note
    Project management endpoints are only available for compose projects, and will not work for Swarm stacks. To manage Swarm stacks, see the [Swarm Stacks](#swarm-stacks) section below.

| Endpoint                                | Method | Description                               | Query Parameters                                                                                                                                                                           |
|-----------------------------------------|--------|-------------------------------------------|--------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| `/v1/api/projects`                      | GET    | List all deployed compose projects        | - `all` (boolean, default: `false`): Return all projects including inactive ones.                                                                                                          |
| `/v1/api/project/{projectName}`         | GET    | Get details of a project                  |                                                                                                                                                                                            |
| `/v1/api/project/{projectName}`         | DELETE | Remove a project                          | - `volumes` (boolean, default: `true`): remove all associated volumes.<br/>- `images` (boolean, default: `true`): remove all associated images.                                            |
| `/v1/api/project/{projectName}/start`   | POST   | Start a project                           |                                                                                                                                                                                            |
| `/v1/api/project/{projectName}/stop`    | POST   | Stop a project                            |                                                                                                                                                                                            |
| `/v1/api/project/{projectName}/restart` | POST   | Restart a project                         |                                                                                                                                                                                            |

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

## Query Parameters

All endpoints that support query parameters accept the following common parameters:

| Query Parameter | Type    | Description                         |
|-----------------|---------|-------------------------------------|
| `timeout`       | integer | Timeout in seconds (default: `30`). |

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
