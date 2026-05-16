---
tags:
  - OCI
  - Advanced
  - Configuration
---

# Webhooks with OCI

!!! example "Experimental Feature"
    OCI artifact support is currently experimental.
    Please [provide feedback and report any issues](../../Contributing/#have-an-issue-idea-or-question) you encounter.

To trigger immediate deployments when artifacts are pushed/available to an OCI registry,
you can send a webhook to doco-cd's [webhook endpoint](../../Endpoints/Webhook-Listener.md) at `/v1/webhook`.

## Payload Schema

```json
{
  "source": "oci",
  "digest": "string",
  "artifact": "string"
}
```

### Field Definitions

| Field      | Type   | Description                                               | Example                                                                  |
|------------|--------|-----------------------------------------------------------|--------------------------------------------------------------------------|
| `source`   | string | Payload discriminator, must be `oci`                      | `oci`                                                                    |
| `digest`   | string | The OCI digest (image ID) in format `algorithm:hexdigest` | `sha256:7d6c7f8e9a0b1c2d3e4f5a6b7c8d9e0f1a2b3c4d5e6f7a8b9c0d1e2f3a4b5c6` |
| `artifact` | string | Full artifact reference with registry host and tag/digest | `ghcr.io/my/app:main`, `docker.io/library/ubuntu:latest`                 |

### Examples

=== "GitHub Container Registry"

    ```json
    {
      "source": "oci",
      "digest": "sha256:7d6c7f8e9a0b1c2d3e4f5a6b7c8d9e0f1a2b3c4d5e6f7a8b9c0d1e2f3a4b5c6",
      "artifact": "ghcr.io/my/app:main"
    }
    ```

=== "Nested namespace/group path (e.g. GitLab)"

    ```json
    {
      "source": "oci",
      "digest": "sha256:1f2e3d4c5b6a79808f7e6d5c4b3a2910ffeeddccbbaa99887766554433221100",
      "artifact": "registry.example.com/myorg/apps/app1/backend:latest"
    }
    ```

=== "Docker Hub"

    ```json
    {
      "source": "oci",
      "digest": "sha256:abc123def456ghi789jkl012mno345pqr678stu901vwx234yz567abc890def",
      "artifact": "docker.io/myusername/myapp-config:latest"
    }
    ```

=== "Private Registry"

    ```json
    {
      "source": "oci",
      "digest": "sha256:fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210",
      "artifact": "registry.example.com:5000/internal/deployment-config:v1.0.0"
    }
    ```

## Signature Verification

OCI webhook payloads are signed with HMAC-SHA256:

**Header**: `X-Doco-OCI-Signature-256`

**Format**: A hexadecimal string representing the HMAC-SHA256 signature of the raw request body, calculated using your `WEBHOOK_SECRET`.

Example:
```yaml
X-Doco-OCI-Signature-256: abc123def456...
```

=== "Shell"

    ```sh
    SIGNATURE=$(echo -n "$PAYLOAD" | \
    openssl dgst -sha256 -hmac "$WEBHOOK_SECRET" -hex | cut -d' ' -f2)
    ```

=== "Python"

    ```python
    import hmac
    import hashlib
    
    payload = request.get_data() # (1)!
    secret = os.getenv('WEBHOOK_SECRET')
    expected_signature = hmac.new(
        secret.encode(),
        payload,
        hashlib.sha256
    ).hexdigest()
    ```

    1. Use the raw request body bytes for signature calculation to ensure it matches what doco-cd receives.

For more information, see the [Webhook Endpoint Setup](../../Setup-Webhook.md#webhook-endpoint).

## Sending OCI Webhooks

The webhook endpoint accepts POST requests at `/v1/webhook` with the OCI payload schema, see [Webhook Listener](../../Endpoints/Webhook-Listener.md) for details.

Some examples of how to send OCI webhooks from different environments:

=== "GitHub Actions"

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
                "source": "oci",
                "digest": "${ARTIFACT_DIGEST}",
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

=== "cURL Command"

    ```bash
    #!/bin/bash
    
    WEBHOOK_URL="https://doco.example.com/v1/webhook"
    WEBHOOK_SECRET="your-webhook-secret"
    ARTIFACT="ghcr.io/myorg/config:main"
    DIGEST="sha256:abc123..."
    
    # Create JSON payload
    PAYLOAD=$(cat <<EOF
    {
      "source": "oci",
      "digest": "$DIGEST",
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

=== "Registry with Native Webhook Support"

    If using a registry that supports webhooks, configure:
    
    - **Webhook URL**: `https://your-doco-server/v1/webhook`
    - **Event**: Image push / artifact push
    - **Format**: Configure to send OCI-compliant webhook matching the [Payload Schema](#payload-schema)
    - **Secret**: Configure to [sign the payloads](#signature-verification) with the same `WEBHOOK_SECRET` used by doco-cd for verification
