---
tags:
  - Advanced
  - Docker
  - Docker Swarm
---

# Tips and Tricks

Some Tips and Tricks for using application.

## Using `include` with remote compose files

Docker Compose's [`include`](https://docs.docker.com/compose/how-tos/multiple-compose-files/include/) directive lets you pull in compose files from remote Git repositories or OCI artifacts directly:

```yaml title="docker-compose.yml"
include:
  - https://github.com/example/app.git#v1.0.0:docker/docker-compose.yml
```

!!! abstract "Compose file reference"
    See the full `include` syntax documentation in the [Docker Compose documentation](https://docs.docker.com/reference/compose-file/include).

### Resolving `.env` and other relative paths

When a remote compose file references relative paths — most commonly `env_file: .env` in a service definition — Docker Compose resolves those paths against the remote include's own directory inside doco-cd's internal cache, **not** against your deployment repository.

This means a deployment like the following will fail with *"env file … not found"* even if `.env` exists in your repository:

```yaml title="docker-compose.yml"
include:
  - https://github.com/immich-app/immich.git#v3.0.3:docker/docker-compose.yml
```

```
env file /var/lib/.../compose-git-cache/.../docker/.env not found
```

**Fix:** use the long `include` form and set `project_directory: .`. This tells Docker Compose to resolve relative paths in the included file against your deployment working directory:

```yaml title="docker-compose.yml" hl_lines="2-3"
include:
  - path: https://github.com/immich-app/immich.git#v3.0.3:docker/docker-compose.yml
    project_directory: .  # resolve .env and other relative paths from your repo root
```

With `project_directory: .`, any `.env` file you place in your repository root (or the directory configured as `working_dir` in `.doco-cd.yml`) will be picked up correctly by the included compose file.

## Removing a container service

=== "Docker Standalone"

    You can add the `scale: 0` option in the `docker-compose.yml` file to remove a service (container). 
    The `scale` option sets the number of containers to run for the service. Setting it to `0` will scale the service down to zero containers.
    
    ```yaml title="docker-compose.yml" hl_lines="3"
    services:
      webserver:
        scale: 0
        image: nginx
    ```

=== "Docker Swarm"

    Add the following line to the `deploy` section of the service in the `docker-compose.yml` file to remove a service (container) in Docker Swarm mode:
    
    ```yaml title="docker-compose.yml" hl_lines="3-4"
    services:
      webserver:
        deploy:
          replicas: 0
        image: nginx
    ```

!!! note
    If you scale down all services in a Docker project or Swarm stack, the entire project will be stopped, 
    but the volumes, configs, secrets, and networks will still exist.

    !!! tip
        To delete volumes, networks, and images, you can use `destroy: true` for the default destructive cleanup behavior, or the full `destroy` object for custom removal options (See [Destroy settings](../Deploy-Settings.md#destroy-settings)).
