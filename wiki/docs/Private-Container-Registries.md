# Accessing Private Container Registries

To access a private container registry, you need to provide the credentials by adding the docker config file `~/.docker/config.json` to the doco-cd container.

1. Encode your credentials to base64 (here we use `printf` to avoid the trailing newline, you can also use `echo -n`):

    ```sh
    printf 'username:password' | base64
    ```

2. Then create a file called `docker-config.json` that contains the authentication information in JSON format:

    ```json
    {
        "auths": {
            "my.registry.example": {
                "auth": "(base64 output here)"
            }
        }
    }
    ```

3. Lastly, add the config file as secret and mount it to `/root/.docker/config.json`:

    ```yaml
    services:
      app:
        container_name: doco-cd
        image: ghcr.io/kimdre/doco-cd:latest
        restart: unless-stopped
        ports:
          - "80:80" # Webhook endpoint
          - "9120:9120" # Prometheus metrics
        environment:
          TZ: Europe/Berlin
          GIT_ACCESS_TOKEN: xxx
          WEBHOOK_SECRET: xxx
        volumes:
          - /var/run/docker.sock:/var/run/docker.sock
          - data:/data
        secrets:
          - source: docker-config
            target: /root/.docker/config.json
    
    secrets:
      docker-config:
        file: docker-config.json
    
    volumes:
      data:
    ```