---
tags:
  - Advanced
  - Docker
  - Swarm Mode
---

Doco-CD also supports [Docker Swarm mode](https://docs.docker.com/engine/swarm/), which allows you to manage a cluster of Docker engines as a single virtual engine. 
This is useful for deploying applications across multiple nodes and ensuring high availability.

If the Docker daemon is running in Swarm mode, doco-cd will detect this automatically and deploy deployments as Swarm stacks instead of simple Compose projects.

Set `swarm.enabled` in an individual deployment configuration to override this selection: `true` deploys a Swarm stack and `false` deploys a Compose project.
This lets both deployment types run through one doco-cd instance on the same Swarm manager. If omitted, auto-detection is used.

You can overwrite this globally to always deploy as Compose projects while running doco-cd in a Swarm environment by setting the `DOCKER_SWARM_FEATURES` environment variable to `false` (See the [Docker Settings](../Docker-Settings.md)).

The deployment happens the same way as with Docker Compose projects, see the [Deploy Settings](../Deploy-Settings.md)

## Configs and Secrets

When deploying configs or secrets in Swarm mode, doco-cd will add a suffix to the name of configs and secrets. 
This suffix is a shortened SHA-256 hash of the config or secret content, which allows doco-cd to rotate them if their contents change.
After rotating, doco-cd prunes old versions of configs or secrets according to the configured retention.

How many old revisions are kept is configurable via:

- [Deploy config](../Deploy-Settings.md#swarm-settings): `swarm.config_retention` and `swarm.secret_retention`
- [Global config](../Docker-Settings.md#swarm-environment-variables): `DOCKER_SWARM_CONFIG_RETENTION` and `DOCKER_SWARM_SECRET_RETENTION`

Set retention to `-1` to disable automatic pruning for that scope and keep old revisions until you remove them manually.

For example, if you deploy a stack named `myapp` with a config named `db-settings` and the content of the config is `hello world`, doco-cd will create a config named `myapp_db-settings_a948904f` in the Swarm cluster.

If you later change the content of the config to `hello universe`, doco-cd will create a new config named `myapp_db-settings_0b5c6934`, redeploy the service/container with the new config and then prune older config revisions according to the configured retention.

## Image pruning

With `prune_images` enabled, doco-cd runs a global job on every node after a deployment.
It prunes dangling images and removes unused images that are known only by their digest, which is how Swarm pulls service images.
Images that any container still uses, running or stopped, are kept, and so are images pulled within the last hour, which another deployment may be about to use.

Swarm keeps the containers of replaced tasks as task history so that a service can be rolled back, and those containers keep their images in use.
With the default task history limit of 5, the images a deployment replaced therefore stay on the nodes until Swarm drops the old tasks.
To free them on the next prune, lower the limit on a manager node:

```shell
docker swarm update --task-history-limit 1
```

`docker service rollback` still works with a lower limit or with `0`, because Swarm rolls back to the previous service spec and pulls its image again if it is no longer on the node, so the registry has to be reachable.

## Docker API Permissions

Swarm deployments need access to more Docker API resources than Compose deployments, namely
services, tasks, nodes, configs and secrets.

See [Docker API Permissions](Docker-API-Permissions.md) if you run doco-cd behind a Docker socket proxy.
