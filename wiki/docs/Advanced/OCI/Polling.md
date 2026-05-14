---
tags:
  - OCI
  - Advanced
  - Configuration
---

# Polling with OCI

Use [Polling](../../Core-Concepts.md#polling) to periodically check for new versions of OCI artifacts.

## Configuration

Add an OCI [polling configuration](../../Poll-Settings.md) to `POLL_CONFIG`:

```yaml
- source: oci
  artifact: ghcr.io/myorg/myapp-config:main
  layout: doco.v1
  interval: 300
  deployments:  # (optional) override deployments defined in artifact
    - name: production
      compose_files:
        - docker-compose.yml
      profiles:
        - production
```

## Parameters

| Parameter     | Default   | Description                                                                                                                                       |
|---------------|-----------|---------------------------------------------------------------------------------------------------------------------------------------------------|
| `source`      |           | (required) Must be `oci`.                                                                                                                         |
| `artifact`    |           | (required) Full OCI artifact reference including the tag to pull (e.g., `ghcr.io/myorg/app:main`)                                                 |
| `layout`      | `doco.v1` | OCI artifact layout version (currently only `doco.v1` supported)                                                                                  |
| `interval`    | `180`     | Poll interval in seconds (minimum: 10)                                                                                                            |
| `deployments` |           | (optional) Array of [inline deployment configurations](../../Poll-Settings.md#inline-deploy-configs). When provided, overrides configs in the artifact. |

## Example: Full Polling Configuration

=== "Deployments defined in the artifact"
    ```yaml title="poll-config.yaml"
    - source: oci
      layout: doco.v1
      artifact: ghcr.io/myorg/config:production
      interval: 300
    - source: oci
      layout: doco.v1
      artifact: ghcr.io/myorg/config:staging
      interval: 180
    ```

=== "Overriding deployments with inline configuration"
    ```yaml title="poll-config.yaml"
    - source: oci
      layout: doco.v1
      artifact: ghcr.io/myorg/config:production
      interval: 300
      deployments:
        - name: web-production
          compose_files:
            - docker-compose.yml
          profiles:
            - production

    - source: oci
      layout: doco.v1
      artifact: ghcr.io/myorg/config:staging
      interval: 180
      deployments:
        - name: web-staging
          compose_files:
            - docker-compose.yml
    ```
