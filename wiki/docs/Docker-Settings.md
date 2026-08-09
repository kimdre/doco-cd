---
tags:
  - Configuration
---

# Docker Settings

These settings control how Doco-CD connects to the Docker daemon.
Most users can leave them at the defaults, and you can set them with [environment variables](App-Settings.md#specifying-the-settings) when running the Doco-CD container.

## Common Environment Variables

!!! tip "Docker CLI environment variables are supported"
    Doco-CD supports most of the standard Docker CLI environment variables to configure the Docker client.
    See the [Docker CLI documentation](https://docs.docker.com/engine/reference/commandline/cli/#environment-variables) for more information on available Docker CLI environment variables.  
    The list below contains the most commonly used environment variables that are relevant for Doco-CD.

| Key                     | Type    | Description                                                                                                                                                                                      | Default value           |
|-------------------------|---------|--------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|-------------------------|
| `DOCKER_API_VERSION`    | string  | Overwrites the API version that doco-cd will use to connect to the Docker Daemon (e.g. `"1.49"`)                                                                                                 |                         |
| `DOCKER_CERT_PATH`      | string  | The directory from which to load the TLS certificates ("ca.pem", "cert.pem", "key.pem'). The directory has to be accessible from inside the container, e.g. by using a bind mount                |                         |
| `DOCKER_CONFIG`         | string  | Overrides the Docker CLI config directory. Doco-CD then reads Docker credentials from `<DOCKER_CONFIG>/config.json`.                                                                             | `~/.docker/config.json` |
| `DOCKER_HOST`           | string  | The url that doco-cd will use to connect to the Docker Daemon (e.g. `tcp://192.168.0.10:2375`). Do not set this when using [Docker contexts](Advanced/Docker-Contexts.md) in deployment configs. |                         |
| `DOCKER_QUIET_DEPLOY`   | boolean | Disable the status output of Docker Compose deployments (e.g. pull, create, start, healthy) in the application logs                                                                              | `true`                  |
| `DOCKER_TLS_VERIFY`     | boolean | Enable or disable TLS verification                                                                                                                                                               |                         |
| `DOCKER_SWARM_FEATURES` | boolean | Enable the use Docker Swarm Mode features if the app has detected that it is running in a Docker Swarm environment                                                                               | `true`                  |

## Remote Docker Daemons

By default, Doco-CD inspects its own container to discover the source of the writable data mount. That inspection is not possible when `DOCKER_HOST` points to a daemon that does not run the Doco-CD container. Set [`DATA_HOST_PATH`](App-Settings.md#general-settings) to bypass this inspection.

The two data path settings describe opposite sides of the mount:

- `DATA_MOUNT_PATH` is the destination inside the Doco-CD container, such as `/data`.
- `DATA_HOST_PATH` is the source path visible in the target Docker daemon's host namespace, such as `/srv/doco-cd-data`.

For example, Doco-CD can run on one host while `DOCKER_HOST` connects to a socket proxy on another Docker host:

```yaml title="docker-compose.yml"
services:
  doco-cd:
    image: ghcr.io/kimdre/doco-cd:latest
    environment:
      DOCKER_HOST: tcp://docker-host.example.com:2375
      DATA_MOUNT_PATH: /data
      DATA_HOST_PATH: /srv/doco-cd-data
    volumes:
      # Shared storage that is also mounted at this path on the remote Docker host.
      - /srv/doco-cd-data:/data
```

The socket proxy on `docker-host.example.com` must expose the Docker API at the configured address. The `/srv/doco-cd-data` path must exist on that remote Docker host and contain the same shared deployment data mounted at `/data` in the Doco-CD container. Configuring `DATA_HOST_PATH` only tells Doco-CD which daemon-visible source path to use; it does not create, mount, synchronize, or share that storage.

!!! warning "Swarm storage is not shared automatically"
    A host path is node-local. `DATA_HOST_PATH` does not make the directory available on other Swarm nodes. If workloads can run on multiple nodes, provide shared storage or ensure the same path and data are mounted on every eligible node.

## Docker Contexts

See [Docker Contexts](Advanced/Docker-Contexts.md) for information on how to use Docker contexts with Doco-CD.

## Docker API Permissions

See [Docker API Permissions](Advanced/Docker-API-Permissions.md) for the list of Docker API endpoints Doco-CD uses
and how to run it behind a Docker socket proxy.
