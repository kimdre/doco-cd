---
tags:
  - Configuration
---

# Docker Settings

Settings to configure the Docker client used by Doco-CD to interact with the Docker daemon.
All of these settings are optional and can be set using [environment variables](App-Settings.md#specifying-the-settings) when running the Doco-CD container.

## Common Environment Variables

!!! tip "Docker CLI environment variables are supported"
    Doco-CD supports most of the standard Docker CLI environment variables to configure the Docker client.
    See the [Docker CLI documentation](https://docs.docker.com/engine/reference/commandline/cli/#environment-variables) for more information on available Docker CLI environment variables.  
    The list below contains the most commonly used environment variables that are relevant for Doco-CD.

| Key                     | Type    | Description                                                                                                                                                                                      | Default value |
|-------------------------|---------|--------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|---------------|
| `DOCKER_API_VERSION`    | string  | Overwrites the API version that doco-cd will use to connect to the Docker Daemon (e.g. `"1.49"`)                                                                                                 |               |
| `DOCKER_CERT_PATH`      | string  | The directory from which to load the TLS certificates ("ca.pem", "cert.pem", "key.pem'). The directory has to be accessible from inside the container, e.g. by using a bind mount                |               |
| `DOCKER_HOST`           | string  | The url that doco-cd will use to connect to the Docker Daemon (e.g. `tcp://192.168.0.10:2375`). Do not set this when using [Docker contexts](Advanced/Docker-Contexts.md) in deployment configs. |               |
| `DOCKER_QUIET_DEPLOY`   | boolean | Disable the status output of Docker Compose deployments (e.g. pull, create, start, healthy) in the application logs                                                                              | `true`        |
| `DOCKER_TLS_VERIFY`     | boolean | Enable or disable TLS verification                                                                                                                                                               |               |
| `DOCKER_SWARM_FEATURES` | boolean | Enable the use Docker Swarm Mode features if the app has detected that it is running in a Docker Swarm environment                                                                               | `true`        |

## Docker Contexts

See [Docker Contexts](Advanced/Docker-Contexts.md) for information on how to use Docker contexts with Doco-CD.