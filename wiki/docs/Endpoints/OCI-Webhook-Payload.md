---
tags:
  - OCI
  - Webhooks
  - API
---

# OCI Webhook Payload

The OCI webhook payload is a JSON message that notifies doco-cd about OCI artifact events (pushes, updates, etc.). When an artifact is pushed to an OCI registry, the registry can send a webhook to doco-cd's webhook endpoint at `/v1/webhook` to trigger a deployment.

## Payload Schema

```json
{
  "ref": "string",
  "digest": "string",
  "repository": "string",
  "artifact": "string"
}
```

## Field Definitions

| Field        | Type   | Description                                               | Example                                                                  |
|--------------|--------|-----------------------------------------------------------|--------------------------------------------------------------------------|
| `ref`        | string | The tag or reference name of the artifact                 | `latest`, `main`, `v1.2.3`, `production`                                 |
| `digest`     | string | The OCI digest (image ID) in format `algorithm:hexdigest` | `sha256:7d6c7f8e9a0b1c2d3e4f5a6b7c8d9e0f1a2b3c4d5e6f7a8b9c0d1e2f3a4b5c6` |
| `repository` | string | Repository name without the registry host                 | `kimdre/doco-cd`, `myorg/myapp-config`                                   |
| `artifact`   | string | Full artifact reference with registry host                | `ghcr.io/kimdre/doco-cd:main`, `docker.io/library/ubuntu:latest`         |

## Examples

### GitHub Container Registry

```json
{
  "ref": "main",
  "digest": "sha256:7d6c7f8e9a0b1c2d3e4f5a6b7c8d9e0f1a2b3c4d5e6f7a8b9c0d1e2f3a4b5c6",
  "repository": "kimdre/doco-cd",
  "artifact": "ghcr.io/kimdre/doco-cd:main"
}
```

#### Nested namespace/group path (for example, GitLab-style groups)

```json
{
  "ref": "latest",
  "digest": "sha256:1f2e3d4c5b6a79808f7e6d5c4b3a2910ffeeddccbbaa99887766554433221100",
  "repository": "apps/app1/backend",
  "artifact": "ghcr.io/myorg/apps/app1/backend:latest"
}
```

### Docker Hub

```json
{
  "ref": "latest",
  "digest": "sha256:abc123def456ghi789jkl012mno345pqr678stu901vwx234yz567abc890def",
  "repository": "myusername/myapp-config",
  "artifact": "docker.io/myusername/myapp-config:latest"
}
```

### Private Registry

```json
{
  "ref": "v1.0.0",
  "digest": "sha256:fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210",
  "repository": "internal/deployment-config",
  "artifact": "registry.example.com:5000/internal/deployment-config:v1.0.0"
}
```

### Custom Tag

```json
{
  "ref": "production",
  "digest": "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
  "repository": "myorg/config",
  "artifact": "ghcr.io/myorg/config:production"
}
```

## How doco-cd Uses the Payload

When doco-cd receives an OCI webhook payload:

1. **Verifies the signature** using `WEBHOOK_SECRET` (via `X-Doco-OCI-Signature-256` header)
2. **Extracts the artifact reference** from the `artifact` field
3. **Matches against polling configs** to find deployments using this artifact
4. **Pulls the artifact** from the registry using the `artifact` reference
5. **Validates the artifact layout** (must be doco.v1)
6. **Deploys** based on matching deployment configuration

## Security

OCI webhook payloads are signed with HMAC-SHA256:

**Header**: `X-Doco-OCI-Signature-256`

**Format**: `sha256={hex_encoded_hmac}`

The signature is calculated over the raw request body:

```python
import hmac
import hashlib
import json

payload_bytes = request.get_data()  # Raw body bytes
secret = os.getenv('WEBHOOK_SECRET')

# Calculate HMAC-SHA256
signature = hmac.new(
    secret.encode(),
    payload_bytes,
    hashlib.sha256
).hexdigest()

# Expected header value
expected_header = f"sha256={signature}"
```

For more details, see [Setup Webhook](../Setup-Webhook.md).

## Sending OCI Webhooks

### From GitHub Actions

```yaml
name: Send OCI Webhook

on:
  registry_package:
    types: [published]

jobs:
  notify:
    runs-on: ubuntu-latest
    steps:
      - name: Send webhook to doco-cd
        env:
          WEBHOOK_URL: ${{ secrets.DOCO_WEBHOOK_URL }}
          WEBHOOK_SECRET: ${{ secrets.DOCO_WEBHOOK_SECRET }}
          ARTIFACT_REF: ${{ github.event.registry_package.name }}
          ARTIFACT_TAG: ${{ github.event.registry_package.version }}
          ARTIFACT_DIGEST: ${{ github.event.registry_package.package_version.container_metadata.digest }}
        run: |
          PAYLOAD=$(cat <<EOF
          {
            "ref": "${ARTIFACT_TAG}",
            "digest": "${ARTIFACT_DIGEST}",
            "repository": "${ARTIFACT_REF#*/}",
            "artifact": "ghcr.io/${ARTIFACT_REF}:${ARTIFACT_TAG}"
          }
          EOF
          )
          
          SIGNATURE=$(echo -n "$PAYLOAD" | openssl dgst -sha256 -hmac "$WEBHOOK_SECRET" -hex | cut -d' ' -f2)
          
          curl -X POST "$WEBHOOK_URL" \
            -H "Content-Type: application/json" \
            -H "X-Doco-OCI-Signature-256: sha256=$SIGNATURE" \
            -d "$PAYLOAD"
```

### From a cURL Command

```bash
#!/bin/bash

WEBHOOK_URL="https://doco.example.com/v1/webhook"
WEBHOOK_SECRET="your-webhook-secret"
ARTIFACT="ghcr.io/myorg/config:main"
DIGEST="sha256:abc123..."
REPOSITORY="myorg/config"
REF="main"

# Create JSON payload
PAYLOAD=$(cat <<EOF
{
  "ref": "$REF",
  "digest": "$DIGEST",
  "repository": "$REPOSITORY",
  "artifact": "$ARTIFACT"
}
EOF
)

# Calculate HMAC-SHA256 signature
SIGNATURE=$(echo -n "$PAYLOAD" | openssl dgst -sha256 -hmac "$WEBHOOK_SECRET" -hex | cut -d' ' -f2)

# Send webhook
curl -X POST "$WEBHOOK_URL" \
  -H "Content-Type: application/json" \
  -H "X-Doco-OCI-Signature-256: sha256=$SIGNATURE" \
  -d "$PAYLOAD"
```

### From Docker Registry v2 API

If using a registry that supports webhooks (Docker Hub, Harbor, etc.), configure:

- **Webhook URL**: `https://your-doco-server/v1/webhook`
- **Event**: Image push / artifact push
- **Format**: Configure to send OCI-compliant webhook matching this schema

## Correlation with Polling

OCI webhooks are correlated with polling configurations by artifact reference:

```yaml
POLL_CONFIG: |
  - source: oci
    artifact: ghcr.io/myorg/config:main    # This must match webhook "artifact" field
    layout: doco.v1
    reference: main                         # Used for tagging in logs/notifications
    interval: 300
    deployments:
      - name: production
```

When a webhook arrives with `"artifact": "ghcr.io/myorg/config:main"`, doco-cd:
1. Finds the matching polling configuration (based on artifact reference)
2. Uses the configured deployments
3. Logs the event with the configured `reference` tag

## Best Practices

1. **Always verify signatures** - Validate the `X-Doco-OCI-Signature-256` header
2. **Use HTTPS** - Transmit sensitive data over encrypted connections
3. **Include digests** - Always include the artifact digest in payloads
4. **Match references** - Ensure webhook artifact matches your polling configuration
5. **Document your format** - If using custom webhooks, document the payload schema

## Related Documentation

- [OCI Usage](../OCI-Usage.md) - Complete OCI usage guide
- [Setup Webhook](../Setup-Webhook.md) - General webhook setup
- [Webhook Listener](Webhook-Listener.md) - Webhook endpoint details



