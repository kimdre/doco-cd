---
tags:
  - OCI
  - Advanced
  - Configuration
---

# Polling with OCI

!!! example "Experimental Feature"
    OCI artifact support is currently experimental.
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

## Fields

See also [Poll Settings](../../Poll-Settings.md) for general polling configuration fields.

| Key           | Type                                                | Description                                                                                                                                             | Default |
|---------------|-----------------------------------------------------|---------------------------------------------------------------------------------------------------------------------------------------------------------|---------|
| `source`      | string                                              | (required) Must be `oci`.                                                                                                                               |         |
| `url`         | string                                              | (required) Full OCI artifact reference including the tag to pull (e.g., `ghcr.io/myorg/app:main`)                                                       |         |
| `interval`    | integer or string                                   | Poll interval (min 10s). Supports integer seconds (`300`), numeric strings (`"300"`), and Go duration strings (`"5m"`, `"1m30s"`).                      | `180s`  |
| `deployments` | array of [Deploy Configs](../../Deploy-Settings.md) | (optional) Array of [inline deployment configurations](../../Poll-Settings.md#inline-deploy-configs). When provided, overrides configs in the artifact. |         |

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
      interval: 3m
      deployments:
        - name: web-staging
          compose_files:
            - docker-compose.yml
    ```
