---
tags:
  - Advanced
  - Docker
  - Docker Swarm
---

# Tips and Tricks

Some Tips and Tricks for using application.

## Removing a container service

### Docker Standalone

You can add the `scale: 0` option in the `docker-compose.yml` file to remove a service (container). 
The `scale` option sets the number of containers to run for the service. Setting it to `0` will scale the service down to zero containers.

```yaml title="docker-compose.yml" hl_lines="3"
services:
  webserver:
    scale: 0  # Add this line to remove the service remotely
    image: nginx
```

!!! tip
    If you set the `scale: 0` option to all services in the docker compose file, the entire project will be stopped 
    and removed, excluding any volumes, networks, and images.
    
    To delete volumes, networks, and images, you can use the `destroy` option in the deployment configuration file (See [Destroy settings](Deploy-Settings.md#destroy-settings)).

### Docker Swarm

Add the following line to the `deploy` section of the service in the `docker-compose.yml` file to remove a service (container) in Docker Swarm mode:

```yaml title="docker-compose.yml" hl_lines="3-4"
services:
  webserver:
    deploy:
      replicas: 0  # Add this line to remove the service remotely
    image: nginx
```
