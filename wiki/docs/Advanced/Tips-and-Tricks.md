---
tags:
  - Advanced
  - Docker
---

# Tips and Tricks

Some Tips and Tricks for using application.

## Removing a container service

You can add the `scale: 0` option in the `docker-compose.yml` file to remove a service (container). 
The `scale` option sets the number of containers to run for the service. Setting it to `0` will scale the service down to zero containers.

!!! tip
    If you set the `scale: 0` option to all services in the docker compose file, the entire project will be stopped 
    and removed, excluding any volumes, networks, and images.
    
    To delete volumes, networks, and images, you can use the `destroy` option in the deployment configuration file (See [Destroy settings](Deploy-Settings.md#destroy-settings)).

### Example

In this example, we add the `scale: 0` option to the `webserver` service and push the change to the repository.
The webhook will then trigger the deployment, which will stop and remove the container for the `webserver` service.

```yaml title="docker-compose.yml"
services:
  webserver:
    scale: 0  # Add this line to remove the service remotely
    image: nginx
```
