---
tags:
  - Setup
  - Advanced
  - Deployment
  - Docker
---

# Docker Contexts

Use the `context` [deployment config](../Deploy-Settings.md#available-settings) option to target a specific [Docker context](https://docs.docker.com/engine/manage-resources/contexts/) instead of the default one.
This lets one doco-cd instance manage and deploy to multiple Docker hosts/clusters.

!!! info "Default Docker context"
    Default Docker context means the local Docker host (usually via the mounted socket `/var/run/docker.sock`).

## Supported transports

doco-cd includes an SSH client, so both TCP and SSH Docker contexts work out of the box:

| Transport | Example endpoint                 | Notes                                 |
|-----------|----------------------------------|---------------------------------------|
| TCP       | `tcp://host:2376`                | Plaintext; use TLS for production     |
| TCP+TLS   | `tcp://host:2376` with TLS certs | Secure TCP                            |
| SSH       | `ssh://user@host`                | Uses the `ssh` binary; mount SSH keys |

### SSH context example

```sh
docker context create prod-remote --docker host=ssh://deploy@prod-host
```

Mount your SSH keys into the doco-cd container so the bundled `ssh` binary can authenticate:

```yaml title="docker-compose.yml" hl_lines="7-8"
services:
  doco-cd:
    image: ghcr.io/kimdre/doco-cd:latest
    container_name: doco-cd
    volumes:
      - /var/run/docker.sock:/var/run/docker.sock
      - ~/.docker:/root/.docker:ro
      - ~/.ssh:/root/.ssh:ro # (1)!
```

1. Mount your SSH keys and `known_hosts` for context connections

!!! tip "SSH host verification"
    Docker's SSH transport respects `~/.ssh/known_hosts`. Pre-populate it (or use `StrictHostKeyChecking=accept-new` via `SSH_OPTIONS`) to avoid interactive prompts on first connection.

### SSH key requirements

The SSH key used must:

- Be **passphrase-free** — Docker's SSH transport runs `ssh` non-interactively inside the container; a passphrase prompt will cause the connection to fail with `Permission denied`
- Have the corresponding **public key in `~/.ssh/authorized_keys`** on the remote host for the connecting user
- Be a supported key type (Ed25519 recommended: `ssh-keygen -t ed25519 -f ~/.ssh/id_ed25519_doco-cd -N ""`)

!!! tip "Verify SSH key"
    To verify the key works from inside the container before using it with doco-cd:
    
    ```sh
    docker exec doco-cd ssh -l <user> -o ConnectTimeout=30 -T -- <host> docker system dial-stdio
    ```
    
    If this command hangs or returns output (even garbled binary), the SSH connection and key are working correctly.

!!! tip "Multiple SSH keys"

    Docker does not try all available keys, so you must specify the correct one for each host.  
    If multiple keys are present, or host key prompts block non-interactive SSH, add an [SSH config](https://man.openbsd.org/ssh_config) entry for the host in the container-mounted `~/.ssh/config`.

    ```sshconfig title="~/.ssh/config"
    Host docker-host
        IdentityFile /root/.ssh/doco-cd-control
        IdentitiesOnly yes
        StrictHostKeyChecking no
    ```
    
    Use `Host docker-host` to match the hostname used by your Docker context endpoint (for example `ssh://user@docker-host`).

## 1. Create Docker contexts

Create contexts on the host that runs doco-cd.

```sh
# Example: remote Docker host over TCP
docker context create prod-remote --docker host=tcp://prod-host:2376

# Example: second environment
docker context create staging-remote --docker host=tcp://staging-host:2376
```

## 2. Verify contexts

```sh
docker context ls
docker --context prod-remote info
docker --context staging-remote info
```

## 3. Mount Docker context config into doco-cd

Docker context metadata and the SSH directory must be available in the doco-cd container.

```yaml title="docker-compose.yml" hl_lines="7-8"
services:
  doco-cd:
    image: ghcr.io/kimdre/doco-cd:latest
    container_name: doco-cd
    volumes:
      - /var/run/docker.sock:/var/run/docker.sock
      - ~/.docker:/root/.docker:ro # (1)!
      - ~/.ssh:/root/.ssh:ro       # (2)!
```

1. Docker context metadata and credentials — required for all non-default contexts
2. SSH keys and `known_hosts` — required for SSH contexts only

!!! warning "File permissions"
    The mounted `~/.docker` and `~/.ssh` directories (and all files within them) must be **readable by the user running inside the container** (root by default).
    If you see `permission denied` errors referencing context metadata or SSH keys, fix permissions on the host:

    ```sh
    chmod -R a+r ~/.docker/contexts/
    find ~/.docker/contexts/ -type d -exec chmod a+rx {} \;
    chmod 600 ~/.ssh/id_* # (1)!
    chmod 644 ~/.ssh/known_hosts ~/.ssh/authorized_keys
    ```

    1. Adjust the `chmod` command accordingly, depending on your SSH key filenames.

If you need private registry access, ensure the mounted Docker config includes required auth data (see [Private Container Registries](Private-Container-Registries.md)).

## 4. Reference context in deployment config

```yaml title=".doco-cd.yml" hl_lines="2"
name: myapp-prod
context: prod-remote
reference: main
working_dir: deploy
compose_files:
  - compose.yml
```

If `context` is omitted (or empty), doco-cd uses the default Docker context.

!!! warning "Do not set `DOCKER_HOST` when using `context`"
    If the `DOCKER_HOST` environment variable is set in the doco-cd container, Docker's endpoint resolution takes it over any Docker context, so the `context` option is silently ignored (or errors on conflict).
    Leave `DOCKER_HOST` unset and rely on the mounted socket and Docker contexts instead.

## 5. Use different contexts per deployment

```yaml title=".doco-cd.yml" hl_lines="3 9"
---
name: myapp-staging
context: staging-remote
reference: develop
working_dir: deploy

---
name: myapp-prod
context: prod-remote
reference: main
working_dir: deploy
```

Each deployment uses its own Docker context for deploy, destroy, and cleanup operations.

## Remote host limitations

When deploying to a remote Docker context, the **remote daemon** executes the compose stack. This has important implications for bind mounts, configs, and secrets.

### Bind mounts

Bind mount source paths must exist on the **remote host**, not the doco-cd host:

```yaml
volumes:
  - /data/myapp:/app/data  # ← this path must exist on the remote host
```

Named volumes work fine — they are created on the remote host automatically:

```yaml
volumes:
  mydata:  # ← created on the remote host

services:
  app:
    volumes:
      - mydata:/app/data
```

### Configs and secrets with `file:` sources

Docker Compose resolves `file:` references to absolute paths on the doco-cd host, then sends those paths to the remote daemon. The remote daemon tries to bind-mount them locally — and fails because the files don't exist there.

```yaml
# ✗ Will fail on remote contexts — remote daemon can't access this path
configs:
  app.conf:
    file: app.conf

secrets:
  api_key:
    file: api_key.txt
```

**Alternatives that work with remote contexts:**

=== "Inline content"

    ```yaml
    configs:
      app.conf:
        content: |
          key=value
          other=setting
    ```

=== "Environment variable (secrets)"

    ```yaml
    secrets:
      api_key:
        environment: API_KEY  # read from env var on the remote host
    ```

=== "Docker Swarm"

    Swarm secrets and configs are uploaded to the cluster and distributed automatically — they work correctly with remote contexts.
    See [Swarm Mode](Swarm-Mode.md) for details.
