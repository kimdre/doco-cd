# Polling configuration file



> Polling is a time-based trigger that checks the repositories for changes to deploy at regular intervals. This method does not require doco-cd to be reachable from the internet but is less efficient and slower than webhooks.

Poll configurations can be set using the `POLL_CONFIG` environment variable or by providing a file with the `POLL_CONFIG_FILE` environment variable.

They must be in the format of a YAML list/array (also called YAML Sequence) and can contain the following settings:

!!! note
    Settings without a default value are required.


| Key           | Type                                       | Description                                                                                                                                                                                                                             | Default value                    |
|---------------|--------------------------------------------|-----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|----------------------------------|
| `url`         | string                                     | The HTTP/SSH clone URL of the repository to poll for changes (e.g. `https://github.com/kimdre/doco-cd.git` or `git@github.com:kimdre/doco-cd.git`)                                                                                      |                                  |
| `reference`   | string                                     | The Git branch or tag to use for the deployment (e.g. `main` or `refs/heads/main`) or tag (e.g. `v1.0.0.` or `refs/tags/v1.0.0`)                                                                                                        | `refs/heads/main`                |
| `interval`    | integer                                    | The interval in seconds at which the repository will be polled for changes (set to `0` to disable polling this Git repository)                                                                                                          | `180`                            |
| `target`      | string                                     | Similar to the *custom target* [webhook endpoint](Endpoints.md#with-custom-target), used to target a specific deployment config, e.g., "test" -> .doco-cd.test.yaml                                                                        | ` ` (Ignored when not specified) |
| `run_once`    | boolean                                    | Stop the poll job after the first run. Useful if you only want to do the first initial deployment via the poll job but do all future deployments via webhooks                                                                           | `false`                          |
| `deployments` | array of [Deploy Configs](Deploy-Settings.md) | In-line configuration for [deployment settings](Deploy-Settings.md) specific to this poll configuration. Overrides the `.doco-cd.yml` file in the target repository (`url`) if exists.<br>See the [example below](#inline-deploy-configs). | `[]`                             |

## Example

### With `POLL_CONFIG`

#### Using a YAML anchor

With a YAML anchor, you can define the poll configuration outside the service definition.

```yaml
# docker-compose.yaml 
x-poll-config: &poll-config
  POLL_CONFIG: |
    - url: https://github.com/example/some-repo.git
    - url: https://github.com/example/public-repo.git
      reference: dev
      interval: 300

services:
  app:
    container_name: doco-cd
    image: ghcr.io/kimdre/doco-cd:latest
    restart: unless-stopped
    ports:
      - "80:80"
    environment:
      TZ: Europe/Berlin
      GIT_ACCESS_TOKEN: xxx
      WEBHOOK_SECRET: xxx
      <<: *poll-config
    volumes:
      - /var/run/docker.sock:/var/run/docker.sock
      - data:/data

volumes:
  data:
```

#### Inline configuration

```yaml
# docker-compose.yaml 
services:
  app:
    container_name: doco-cd
    image: ghcr.io/kimdre/doco-cd:latest
    restart: unless-stopped
    ports:
      - "80:80"
    environment:
      TZ: Europe/Berlin
      GIT_ACCESS_TOKEN: xxx
      WEBHOOK_SECRET: xxx
      POLL_CONFIG: |
        - url: https://github.com/example/some-repo.git
        - url: https://github.com/example/public-repo.git
          reference: dev
          interval: 300
    volumes:
      - /var/run/docker.sock:/var/run/docker.sock
      - data:/data

volumes:
  data:
```

### With `POLL_CONFIG_FILE`

```yaml
# poll-config.yaml
- url: https://github.com/example/some-repo.git
- url: https://github.com/example/public-repo.git
  reference: dev
  interval: 300
```

```yaml
# docker-compose.yaml 
services:
  app:
    container_name: doco-cd
    image: ghcr.io/kimdre/doco-cd:latest
    restart: unless-stopped
    ports:
      - "80:80"
    environment:
      TZ: Europe/Berlin
      GIT_ACCESS_TOKEN: xxx
      WEBHOOK_SECRET: xxx
      POLL_CONFIG_FILE: /poll-config.yaml
    volumes:
      - /var/run/docker.sock:/var/run/docker.sock
      - data:/data
      - ./poll-config.yaml:/poll-config.yaml:ro

volumes:
  data:
```

#### Using a docker compose config
```yaml
# docker-compose.yaml
services:
  app:
    container_name: doco-cd
    image: ghcr.io/kimdre/doco-cd:latest
    restart: unless-stopped
    ports:
      - "80:80"
    environment:
      TZ: Europe/Berlin
      GIT_ACCESS_TOKEN: xxx
      WEBHOOK_SECRET: xxx
      POLL_CONFIG_FILE: /poll-config.yml
    volumes:
      - /var/run/docker.sock:/var/run/docker.sock
      - data:/data
    configs: # Use with POLL_CONFIG_FILE
      - source: poll-config.yml
        target: /poll-config.yml

volumes:
  data:
  
configs:
  poll-config.yml:
    content: |
      - url: https://example.com
        branch: main
      - url: https://other-example.com
        interval: 120
      - url: https://yet-another-example.com
        branch: dev
```

### Inline Deploy Configs

Inline deployments reuse the same fields as `.doco-cd.yml` files (See [Deploy Settings](Deploy-Settings.md)), including support for external secrets and destroy workflows. The poll job `url` is always used as the deployment source.

If the poll config has an inline deploy config and the target repository also contains a `.doco-cd.yml` file, the file will be ignored in favor of the inline deploy config.

```yaml
- url: https://github.com/example/app.git
  reference: refs/heads/main
  interval: 300
  deployments:
    - name: example-app
      working_dir: services/app
      compose_files:
        - compose.yaml
      env_files:
        - .env.production
```