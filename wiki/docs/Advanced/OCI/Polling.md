---
tags:
  - OCI
  - Advanced
  - Configuration
---

# Polling with OCI

!!! example "Experimental Feature"
    OCI artifact support is currently experimental and may be unstable and subject to breaking changes.
    Please [provide feedback and report any issues](../../Contributing/#have-an-issue-idea-or-question) you encounter.

Use [Polling](../../Core-Concepts.md#polling) to periodically check for new versions of OCI artifacts.

## Configuration

Add an OCI [polling configuration](../../Poll-Settings.md) to `POLL_CONFIG`:

```yaml
- source: oci
  url: ghcr.io/myorg/myapp-config:main
  interval: 300
  deployments:  # (optional) override deployments defined in artifact
    - name: production
      compose_files:
        - docker-compose.yml
      profiles:
        - production
```

## Parameters

| Parameter     | Default | Description                                                                                                                                             |
|---------------|---------|---------------------------------------------------------------------------------------------------------------------------------------------------------|
| `source`      |         | (required) Must be `oci`.                                                                                                                               |
| `url`         |         | (required) Full OCI artifact reference including the tag to pull (e.g., `ghcr.io/myorg/app:main`)                                                       |
| `interval`    | `180`   | Poll interval in seconds (minimum: 10)                                                                                                                  |
| `deployments` |         | (optional) Array of [inline deployment configurations](../../Poll-Settings.md#inline-deploy-configs). When provided, overrides configs in the artifact. |

## Example: Full Polling Configuration

=== "Deployments defined in the artifact"
    ```yaml title="poll-config.yaml"
    - source: oci
      url: ghcr.io/myorg/config:production
      interval: 300
    - source: oci
      url: ghcr.io/myorg/config:staging
      interval: 180
    ```

=== "Overriding deployments with inline configuration"
    ```yaml title="poll-config.yaml"
    - source: oci
      url: ghcr.io/myorg/config:production
      interval: 300
      deployments:
        - name: web-production
          compose_files:
            - docker-compose.yml
          profiles:
            - production

    - source: oci
      url: ghcr.io/myorg/config:staging
      interval: 180
      deployments:
        - name: web-staging
          compose_files:
            - docker-compose.yml
    ```
