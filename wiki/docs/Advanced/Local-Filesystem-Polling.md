---
tags:
  - Advanced
  - Configuration
  - Deployment
---

# Polling Local Filesystem Repositories

Some stacks cannot be deployed without a running SCM available (e.g. a self-hosted Gitea/Forgejo instance that isn't reachable from doco-cd, or that you don't want to depend on at all).
For these cases, doco-cd can poll a Git repository that already lives on the local filesystem, e.g. bind-mounted into the container, instead of a remote host.

This reuses the exact same [polling](../Core-Concepts.md#polling) mechanism, deployment pipeline and [auto-discovery](../Deploy-Settings.md#auto-discovery) used for remote Git repositories - the only difference is the `url` scheme.

## Configuration

Mount the local Git repository into the doco-cd container (read-only is recommended, doco-cd never writes to it - it clones into its own working directory), then reference it in a [poll configuration](../Poll-Settings.md) using a `file://` URL:

```yaml title="docker-compose.yaml" hl_lines="9-12 15"
services:
  app:
    container_name: doco-cd
    image: ghcr.io/kimdre/doco-cd:latest
    restart: unless-stopped
    environment:
      TZ: Europe/Berlin
      POLL_CONFIG: |
        - source: git
          url: file:///local-repos/my-app
          reference: main
          interval: 60s
    volumes:
      - /var/run/docker.sock:/var/run/docker.sock
      - data:/data
      - /host/path/to/my-app.git:/local-repos/my-app:ro

volumes:
  data:
```

See [Poll Settings](../Poll-Settings.md) for the full list of poll configuration fields.

## How it works

- `url` must be an absolute path inside the container, prefixed with `file://` (e.g. `file:///local-repos/my-app`).
- `reference` behaves exactly like it does for remote Git repositories: a branch name, tag name, or commit SHA.
- On every poll interval, doco-cd checks the local repository for new commits on `reference` and, if changed, deploys them using the same pipeline as remote Git polling (including `.doco-cd.yml`/`.doco-cd.*.yml` discovery and [auto-discovery](../Deploy-Settings.md#auto-discovery)).
- No credentials are needed or used for `file://` URLs.

## Limitations

- Local filesystem sources are poll-only. There is no webhook equivalent, since there is no SCM to send a webhook.
- No commit status is posted back to an SCM, since there is none to post to.
