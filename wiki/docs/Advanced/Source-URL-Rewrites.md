---
tags:
  - Advanced
  - Configuration
  - Deployment
---

# Source URL Rewrites

Source URL rewrites let you rewrite Git source URLs before doco-cd clones them. 
Rules apply to both webhook- and poll-triggered deployments.

This is useful when your Git provider advertises a public URL (in webhook payloads or poll configs) but doco-cd should 
clone through an internal network path instead, for example when your Forgejo instance is behind a reverse proxy with a 
public domain, but is reachable directly over a Docker network.

## Configuration

| Key                        | Type           | Description                                                                                           | Default                    |
|----------------------------|----------------|-------------------------------------------------------------------------------------------------------|----------------------------|
| `SOURCE_URL_REWRITES`      | map of strings | YAML map of git source URL rewrite rules, applied to both webhook and poll deployments.               | Ignored when not specified |
| `SOURCE_URL_REWRITES_FILE` | string         | Path to a file containing `SOURCE_URL_REWRITES` YAML (mutually exclusive with `SOURCE_URL_REWRITES`). |                            |

## Match Strategies

Two match strategies are supported:

- **URL/URI prefix**, e.g. `https://forgejo.example.com/` or `git@forgejo.example.com:`.
    The matched prefix in the source URL is replaced with the configured target, and the repository path is appended as-is.
    - HTTPS URLs should end with `/` to avoid partial host matches.
    - SCP-style SSH URLs (e.g. `git@host:`) **must** end with `:` because it is the mandatory separator between host and repository path in SCP syntax (`user@host:path/repo.git`).
    - SCP syntax cannot express a port number. Use `ssh://` syntax when targeting a non-standard port (e.g. `"ssh://git@forgejo.internal:2222/"`).
- **Host/domain**, e.g. `forgejo.example.com`. Only the host (and optional port) is replaced; scheme, credentials, and path are preserved.

## Rule Matching

Rules are matched in order of specificity (longest key first).

!!! example

    ```yaml title="Some possible examples"
    SOURCE_URL_REWRITES:
      # HTTPS → internal HTTP (key ends with / to avoid partial-host matches)
      "https://forgejo.example.com/": "http://forgejo:3000/"
      # Host-only match (replaces host+port, keeps scheme/path)
      "forgejo.example.com": "forgejo:3000"
      # SCP-style SSH → SCP-style SSH (the trailing : is required because it is the SCP host/path separator)
      # OR: SCP-style SSH → ssh:// with non-standard port (SCP syntax cannot carry a port number)
      # Pick one of these two, not both (YAML map keys must be unique):
      # "git@forgejo.example.com:": "git@forgejo.internal:"
      # "git@forgejo.example.com:": "ssh://git@forgejo.internal:2222/"
    ```

    In your doco-cd `docker-compose.yml`, you can set this as follows:

    ```yaml title="docker-compose.yml"
    services:
      app:
        environment:
          SOURCE_URL_REWRITES: |
            # HTTP variant:
            "forgejo.example.com": "forgejo:3000"
            # SSH variant with explicit port:
            "git@forgejo.example.com:": "ssh://git@forgejo:2222/"
    ```
