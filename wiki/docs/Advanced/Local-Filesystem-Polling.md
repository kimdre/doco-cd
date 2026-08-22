---
tags:
  - Advanced
  - Configuration
  - Deployment
---

# Polling Local Filesystem Repositories

Some stacks cannot be deployed without a running SCM available (e.g. a self-hosted Gitea/Forgejo instance that should be deployed via doco-cd (hence a chicken-and-egg problem), or a stack that is deployed from a local Git repository that is not hosted on any remote SCM).

For these cases, doco-cd can poll a Git repository that already lives on the local filesystem, e.g. bind-mounted into the container, instead of a remote host.

This reuses the exact same [polling](../Core-Concepts.md#polling) mechanism, deployment pipeline and [auto-discovery](../Deploy-Settings.md#auto-discovery) used for remote Git repositories - the only difference is the `url` scheme.

## Configuration

Mount the local Git repository into the doco-cd container (read-only is recommended, doco-cd never writes to it - it clones into its own working directory), then reference it in a [poll configuration](../Poll-Settings.md) using either an absolute path or a `file://` URL:

```yaml title="docker-compose.yaml" hl_lines="9-12 16"
services:
  app:
    container_name: doco-cd
    image: ghcr.io/kimdre/doco-cd:latest
    restart: unless-stopped
    environment:
      TZ: Europe/Berlin
      POLL_CONFIG: |
        - source: git
          url: file:///local-repos/my-app # Or use `url: /local-repos/my-app`
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

- `url` must be an absolute path inside the container (e.g. `/local-repos/my-app`) or the equivalent `file://` URL (e.g. `file:///local-repos/my-app`).
- Both regular repositories (with a `.git` directory) and bare repositories (e.g. `my-app.git`) are supported, as are linked worktrees and submodule checkouts that use a `.git` file.
- `reference` behaves exactly like it does for remote Git repositories: a branch name, tag name, or commit SHA.
- On every poll interval, doco-cd checks the local repository for new commits on `reference` and, if changed, deploys them using the same pipeline as remote Git polling (including `.doco-cd.yml`/`.doco-cd.*.yml` discovery and [auto-discovery](../Deploy-Settings.md#auto-discovery)).
- No credentials are needed or used for local repositories.

## Webhook-triggered local mirrors

Local repositories do not emit webhooks themselves. If the repository is a local mirror of a GitHub, GitLab, or Forgejo repository, its SCM webhook can trigger deployment from the local mirror instead.

Configure an explicit `SOURCE_URL_REWRITES` rule that transforms the SCM clone URL into the mounted local path:

```yaml title="docker-compose.yaml"
environment:
  SOURCE_URL_REWRITES: |
    https://forgejo.example.com/: file:///local-repos/
```

For example, a webhook whose clone URL is `https://forgejo.example.com/org/my-app.git` then deploys from `file:///local-repos/org/my-app.git`. For safety, doco-cd rejects a `file://` clone URL supplied directly by a webhook payload; it must result from a configured rewrite.

## Limitations

- Local repositories have no native webhook source. Webhook-triggered deployments require an SCM webhook and an explicit `SOURCE_URL_REWRITES` rule as shown above.
- No commit status is posted back to an SCM, since there is none to post to.
- Shallow clones are not supported for local repositories: [`git_depth`](../Deploy-Settings.md) and `GIT_CLONE_DEPTH` are ignored and the repository is always cloned in full. This avoids network transfer, but can still cost disk I/O and storage for large histories.
