# Pre- / Post-Deployment Scripts

In this documentation, we will cover how to run scripts or commands during the deployment and container lifecycle of your Docker Compose services.

!!! question "Why doco-cd does not provide a shell environment for executing scripts"
    For security reasons doco-cd itself does not provide a shell environment for executing scripts. Instead, it relies on the underlying container runtime (e.g., Docker) to execute these scripts within the context of the deployed containers/compose services.

Available options to run scripts/commands during deployment or container lifecycle include:

- [Init Containers](#init-containers)
- [Sidecar Containers](#sidecar-containers)
- [Compose Lifecycle Hooks](#compose-lifecycle-hooks)

## Init Containers

Init containers are containers that run before the main application containers in your Docker Compose setup and complete their tasks before the main containers start. They are useful for performing setup tasks, such as initializing databases, running migrations, or preparing configuration files.

Some common use cases for init containers include:

- Database migrations: Running database migration scripts before the main application starts.
- Running shell scripts to generate configuration files or perform setup tasks.
- Preloading data into databases or caches.

We use the `depends_on` option with the `condition: service_completed_successfully` condition to ensure that the main application container waits for the init container to complete successfully before starting. The init container will run its specified commands and exit with a status code of 0 to indicate success, allowing the main application container to start afterward.

### Example

```yaml title="docker-compose.yml" hl_lines="1-2 4-18 23 28-30"
x-common-env: &common-env
  MYVAR: world  # We will use this variable in both init and app containers

services:
  init:
    image: busybox
    environment:
      <<: *common-env
    entrypoint: "sh -c"  # Use sh -c as the entrypoint to run multiple commands in "command" section
    volumes:
      - ./web:/web
    working_dir: /web
    command:
      - |
        echo Starting pre-deployment script
        echo "Hello $${MYVAR}!" > /web/index.html # Double dollar-sign is required here to use the variable in the shell script 
        echo Finished pre-deployment script
        exit 0  # Exit with code 0 to indicate success, not required if the last command already returns 0 but added here for clarity

  app:
    image: nginx
    environment:
      <<: *common-env
    volumes:
      - ./web:/usr/share/nginx/html:ro
    ports:
      - 8080:80
    depends_on:
      init:
        condition: service_completed_successfully  # Wait for init container to complete/stop with exit code 0
```

- If you have a shell script in your repo for the init stuff, you can remove `entrypoint` and mount the script directly and run it via the `command` option:
  ```yaml title="docker-compose.yml"
  volumes:
    - ./init/:/init
  command: /init/initproject.sh
  ```
  
- If you need commands from the app container, try to use the same image as your app container. Many app images also come with a shell (sh, ash, bash) 

### Troubleshooting

#### container exited (0)

If the deployment fails with an error containing a message like `container <init-container-name> exited (0)`, try to add a short sleep at the end of the init container commands. This is a workaround for a known issue where the init container may exit before the main container starts waiting for it, causing the main container to miss the successful completion of the init container. Adding a short sleep ensures that the init container has time to exit properly before the main container checks its status.

**Example**:
```yaml title="Add a sleep command to the init container in your docker-compose.yml"
entrypoint: ["/bin/sh", "-c"]
command: ["<your-commands-here> && sleep 3"]  # Depending on the complexity of your init commands, you may need to adjust the sleep duration.
```

Related issue: [#1115](https://github.com/kimdre/doco-cd/issues/1115)

## Sidecar Containers
Sidecar containers are additional containers that run alongside your main application containers. They can be used to provide auxiliary services, such as background tasks, metrics collection, or log forwarding.

Some common use cases for sidecar containers include:
- Background tasks, e.g. cron jobs or scheduled tasks
- Metrics collection for monitoring tools like Prometheus
- Log forwarding to external systems

### Example

```yaml title="docker-compose.yml" hl_lines="12-25"
volumes:
  webdata:

services:
  app:
    image: nginx
    ports:
      - "8080:80"
    volumes:
      - webdata:/usr/share/nginx/html:ro

  sidecar:
    image: busybox
    volumes:
      - webdata:/webdata
    depends_on:
      - app
    entrypoint: "sh -c"
    command:
      - |
        while true; do
          echo "Updating web content..."
          echo "The current time is $(date)" > /webdata/index.html 
          sleep 60
        done
```

## Compose Lifecycle Hooks

!!! info "Requires Docker Compose [2.30.0](https://github.com/docker/compose/releases/tag/v2.30.0) or later"

Docker Compose lifecycle hooks allow you to run commands/scripts at specific points in the container lifecycle, such as after starting ([`post_start`](https://docs.docker.com/reference/compose-file/services/#post_start)) or before stopping [`pre_stop`](https://docs.docker.com/reference/compose-file/services/#pre_stop) a container.

### Example

#### Post Start Hook

This example demonstrates how to use the `post_start` hook to set up the correct volume permissions that the application needs.

**How It Works**:

1. **Volume Initialization**: Docker creates the data volume with root ownership
2. **Container Starts**: The container runs with user: 1001
3. **Permission Setup**: Two post-start hooks execute sequentially:
   - First hook changes ownership to user 1001
   - Second hook sets appropriate read/write permissions
4. **Application Runs**: The application can now access the volume with proper permissions

```yaml title="docker-compose.yml"
services:
  app:
    image: backend
    user: "1001"
    volumes:
      - data:/data
    post_start:
      - command: ["chown", "-R", "1001:1001", "/data"]
        user: root
      - command: ["chmod", "-R", "755", "/data"]
        user: root

volumes:
  data:
    driver: local
```

#### Pre Stop Hook

This example demonstrates how to use the `pre_stop` hook to run cleanup tasks before the container stops.

**How It Works**:

1. **Shutdown Initiated**: Container receives shutdown signal (e.g., docker compose down)
2. **Pre-Stop Sequence**: Hooks execute in order:
    1. Flush application cache
    2. Backup important data
    3. Notify monitoring system
3. **Container Stops**: After hooks complete, container proceeds with shutdown or restart

```yaml title="docker-compose.yml"
services:
  app:
    image: backend
    pre_stop:
      - command: ["./scripts/flush_cache.sh"]
      - command: ["./scripts/backup_data.sh"]
      - command: ["curl", "-X", "POST", "http://monitoring.example.com/notify_shutdown"]
```

## Further Reading
- More examples can be found in this guide: [Managing Container Lifecycles with Docker Compose Lifecycle Hooks](https://dev.to/idsulik/managing-container-lifecycles-with-docker-compose-lifecycle-hooks-mjg)
- [Using lifecycle hooks with Compose](https://docs.docker.com/compose/how-tos/lifecycle/)
- [Docker Compose Post Start Hook Documentation](https://docs.docker.com/reference/compose-file/services/#post_start)
- [Docker Compose Pre Stop Hook Documentation](https://docs.docker.com/reference/compose-file/services/#pre_stop)
