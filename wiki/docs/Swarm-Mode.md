---
tags:
  - Advanced
  - Docker
  - Swarm Mode
---

## Overview

Doco-CD also supports [Docker Swarm mode](https://docs.docker.com/engine/swarm/), which allows you to manage a cluster of Docker engines as a single virtual engine. 
This is useful for deploying applications across multiple nodes and ensuring high availability.

If the Docker daemon is running in Swarm mode, doco-cd will detect this automatically and deploy everything as Swarm stacks instead of simple Compose projects.

You can overwrite this to always deploy as Compose projects while running doco-cd in a Swarm environment by setting the `DOCKER_SWARM_FEATURES` environment variable to `false` (See the [Docker-specific App Settings](App-Settings.md#docker-specific-settings)).

The deployment happens the same way as with Docker Compose projects, see the [Deploy Settings](Deploy-Settings.md)

## Configs and Secrets

When deploying configs or secrets in Swarm mode, doco-cd will add a suffix to the name of configs and secrets. 
This suffix is a shortened sha256 hash of the config or secret content, which allows doco-cd to rotate them if their contents change.
After rotating, doco-cd will also clean up old versions of the configs or secrets to prevent clutter.

For example, if you deploy a stack named `myapp` with a config named `db-settings` and the content of the config is `hello world`, doco-cd will create a config named `myapp_db-settings_a948904f` in the Swarm cluster.

If you later change the content of the config to `hello universe`, doco-cd will create a new config named `myapp_db-settings_0b5c6934`, redeploy the service/container with the new config and remove the old config `myapp_db-settings_a948904f` from the Swarm cluster.