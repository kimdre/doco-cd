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

## Auto-discovery cleanup ownership

Tags such as `production` and `staging` share a repository-based artifact store, but are independent
auto-discovery owners. Cleanup requires the same registry/repository, stable tag, and deployment config
target (when available). Matching targets do not allow one tag to clean up another tag's stacks.
A tag moving to a new digest does not change its ownership.

The recorded `cd.doco.source.url` tag is authoritative, including when `repository_url` selects a Git
deployment repository. For a digest-only recorded URL, cleanup can use
`cd.doco.deployment.target.ref` only when the source type is `oci` and the deployment working directory
belongs to that OCI store. A Git deployment branch, digest, or legacy bare digest hash is not a stable
OCI owner. Missing or ambiguous ownership preserves the stack and logs a warning; a config target
alone does not resolve that ambiguity. Digest-only requests likewise need an explicitly retained stable
source reference; the webhook schema has no separate field for one.

For webhooks, send the stable tag in `artifact` and pin the revision separately in `digest`
(see [OCI Webhooks](Webhooks.md#payload-schema)). A digest-only webhook does not establish the tag owner
and cannot clean up stacks belonging to a tagged poll.

Explicit `destroy` requests instead prove repository ownership, not tag ownership: other tags or digests
of the same source may be destroyed. The guard checks canonical source URLs, including registry/host
and source type, and the configured `repository_url`/config-source relationship. A foreign registry or
Git source cannot match through a shared hostless name. Legacy labels with a valid source URL can omit
the source-type label; without a URL, both an explicit source type and the full host-qualified repository
name are required. Ambiguous or missing identity produces a deployment conflict rather than authorizing
destruction.

An OCI config using a Git `repository_url` has one intentional cross-type label relationship: polling
records the Git deployment's type/name while the source URL still identifies the OCI config. The guard
permits that configured relationship, but rejects other contradictory URL/type combinations rather
than falling back to a matching short name.
