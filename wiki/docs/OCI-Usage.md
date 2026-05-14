---
tags:
  - OCI
  - Configuration
  - Webhooks
  - Polling
---

# OCI Artifact Usage

This page provides comprehensive documentation on using doco-cd with OCI (Open Container Initiative) artifacts, including webhook payloads, trust policies, and artifact packaging conventions.

## Overview

Doco-cd supports pulling deployment configurations from OCI registries (e.g., Docker Hub, GitHub Container Registry, private registries) in addition to Git repositories. This allows you to:

- Store deployment configuration as versioned OCI artifacts
- Use container registries as your source of truth for deployments
- Trigger deployments via OCI webhook events
- Validate artifact signatures before deployment
- Use the same registry infrastructure for both container images and configuration

## Getting Started

To use OCI artifacts with doco-cd, you need to:

1. Package your deployment configuration according to the **doco.v1 layout** specification
2. Push the artifact to an OCI registry
3. Configure doco-cd with either polling or webhooks
4. (Optional) Configure signature verification with OCI_TRUST_POLICY

## Supported OCI Registries

Doco-cd can pull artifacts from any OCI-compliant registry:

- **Docker Hub** (`docker.io`)
- **GitHub Container Registry** (`ghcr.io`)
- **GitLab Container Registry** (`registry.gitlab.com`)
- **Amazon ECR** (`*.dkr.ecr.*.amazonaws.com`)
- **Google Artifact Registry** (`*.pkg.dev`)
- **Azure Container Registry** (`*.azurecr.io`)
- **Private/Self-hosted registries** (supporting OCI Image Spec v1.0+)

!!! note "See [Private Container Registries](Advanced/Private-Container-Registries.md) for authentication to private registries."

---

## `doco.v1` Artifact Layout

The **doco.v1** layout is a strict, versioned specification for packaging deployment configurations as OCI artifacts. It ensures consistency and enables validation.

### Artifact Structure

A doco.v1 artifact must have a root-level deployment configuration file (`.doco-cd.y(a)ml`) in the root (`/`) of the artifact
that includes the `#!yaml layout: doco.v1` field. 

The rest of the artifact can contain any files needed for deployment, as with deployments from Git repository (e.g., compose files, app configuration, assets, scripts), see [Deploy Settings](Deploy-Settings.md).

!!! example "Artifact Layout Examples"

    === "Single Deployment"
        ```
        artifact-root/
        ├── .doco-cd.yaml        # Main deployment config with `layout: doco.v1`
        ├── docker-compose.yml    # Docker Compose configuration
        └── (other files as needed)
        ```
    
    === "Multiple Deployments"
        ```
        artifact-root/
        ├── .doco-cd.yaml        # Main deployment config with `layout: doco.v1`
        ├── web/
        │   ├── .doco-cd.yaml    # Extra deployment config for web service
        │   └── docker-compose.yml
        │   └── config/
        │       └── nginx.conf
        └── app/
            └── docker-compose.yml
            └── app.env
            └── migrations/
                └── 001-init.sql
        ```

### Required Files

#### `.doco-cd.y(a)ml`
**Required** - The main [deployment configuration](Deploy-Settings.md) file.

- **Layout version**: Each deployment configuration must include `#!yaml layout: doco.v1` to indicate it follows the doco.v1 artifact layout specification.
- **Example**:
  ```yaml
  layout: doco.v1
  name: production
  compose_files:
    - docker-compose.yml
  profiles:
    - production
  ```

### Example: Creating a doco.v1 OCI Artifact

Here's a complete example of creating and pushing a doco.v1 OCI artifact:

=== "Step 1: Create the artifact directory"
    ```sh
    mkdir -p artifact
    cd artifact
    
    # Create deployment configuration
    cat > .doco-cd.yaml << 'EOF'
    layout: doco.v1
    name: web-app
    compose_files:
      - docker-compose.yml
    EOF
    
    # Create docker-compose.yml
    cat > docker-compose.yml << 'EOF'
    services:
      app:
        image: myapp:latest
      nginx:
        image: nginx:latest
    EOF
    ```

=== "Step 2: Create the OCI artifact"

    === "Using Docker CLI"
        ```sh
        # Create a minimal Dockerfile inside the artifact directory
        cat > artifact/Dockerfile << 'EOF'
        FROM scratch
        COPY . /
        EOF
        
        # Build and push the artifact
        docker build -t ghcr.io/myorg/myapp-config:main artifact/
        docker push ghcr.io/myorg/myapp-config:main
        ```

    === "Using OCI tools"
        We use [skopeo](https://github.com/containers/skopeo) to copy the directory directly to an OCI registry without needing to create a Docker image:

        ```sh
        # Using skopeo or similar tools
        skopeo copy dir://artifact oci:oras-artifact:latest
        ```

---

## Polling with OCI

Use [Polling](Core-Concepts.md#polling) to periodically check for new versions of OCI artifacts.

### Configuration

Add an OCI [polling configuration](Poll-Settings.md) to `POLL_CONFIG`:

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

### Parameters

| Parameter     | Default   | Description                                                                                                                                       |
|---------------|-----------|---------------------------------------------------------------------------------------------------------------------------------------------------|
| `source`      |           | (required) Must be `oci`.                                                                                                                         |
| `artifact`    |           | (required) Full OCI artifact reference including the tag to pull (e.g., `ghcr.io/myorg/app:main`)                                                 |
| `layout`      | `doco.v1` | OCI artifact layout version (currently only `doco.v1` supported)                                                                                  |
| `interval`    | `180`     | Poll interval in seconds (minimum: 10)                                                                                                            |
| `deployments` |           | (optional) Array of [inline deployment configurations](Poll-Settings.md#inline-deploy-configs). When provided, overrides configs in the artifact. |

### Example: Full Polling Configuration

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

---

## Webhooks with OCI

Use OCI webhooks to trigger immediate deployments when artifacts are pushed/available.

### OCI Webhook Payload Schema

The OCI webhook payload follows this JSON schema:

```json
{
  "source": "oci",
  "digest": "sha256:abcdef1234567890...", // (1)!
  "artifact": "ghcr.io/myorg/myapp-config:latest"
}
```

1. The complete OCI digest of the artifact, including the algorithm (e.g., `sha256:...`). This allows doco-cd to verify the exact artifact version that triggered the webhook.

### Payload Fields

| Field      | Type   | Description                                                                            |
|------------|--------|----------------------------------------------------------------------------------------|
| `source`   | string | Payload discriminator; must be `oci`                                                   |
| `digest`   | string | Complete OCI digest including algorithm (e.g., `sha256:abc123...`)                     |
| `artifact` | string | Full artifact reference including registry (e.g., `ghcr.io/myorg/myapp-config:latest`) |

#### Example Payloads

=== "GitHub Container Registry Webhook"
    ```json
    {
      "source": "oci",
      "digest": "sha256:7d6c7f8e9a0b1c2d3e4f5a6b7c8d9e0f1a2b3c4d5e6f7a8b9c0d1e2f3a4b5c6",
      "artifact": "ghcr.io/kimdre/doco-cd:main"
    }
    ```

=== "Custom OCI Registry Webhook"
    ```json
    {
      "source": "oci",
      "digest": "sha256:abc123def456ghi789jkl012mno345pqr678stu901vwx234yz567abc890def",
      "artifact": "registry.example.com/myorg/config:v1.2.3"
    }
    ```

### Webhook Security

OCI webhooks use HMAC-SHA256 signatures for security:

**Header**: `X-Doco-OCI-Signature-256`

**Format**: `sha256={hex_encoded_hmac}`

Example:
```
X-Doco-OCI-Signature-256: sha256:abc123def456...
```

The signature is calculated using the raw request body and your `WEBHOOK_SECRET`:

=== "Shell"
    ```sh
    # Calculate HMAC signature of the payload using OpenSSL
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

For more information, see [Webhook Endpoint](Setup-Webhook.md).

### Example for sending Webhooks

#### GitHub

=== "Using `workflow_run` event trigger"
    Use GitHub Actions with two workflows:
    
    === "Workflow 1: Build and push artifact"
     
        ```yaml title=".github/workflows/build-oci-artifact.yml"
        name: Build OCI Artifact
        on:
          push:
            tags:
              - 'v*'
        
        jobs:
          build:
            runs-on: ubuntu-latest
            permissions:
              contents: read
              packages: write
            outputs:
              digest: ${{ steps.build.outputs.digest }}
              artifact: ghcr.io/${{ github.repository }}-config:${{ github.ref_name }}
            steps:
              - name: Checkout
                uses: actions/checkout@v4
              
              - name: Set up Docker Buildx
                uses: docker/setup-buildx-action@v3
              
              - name: Login to GHCR
                uses: docker/login-action@v3
                with:
                  registry: ghcr.io
                  username: ${{ github.actor }}
                  password: ${{ secrets.GITHUB_TOKEN }}
              
              - name: Build and push to GHCR
                uses: docker/build-push-action@v5
                id: build
                with:
                  context: .
                  push: true
                  tags: ghcr.io/${{ github.repository }}-config:${{ github.ref_name }}
        ```
    
    === "Workflow 2: Notify doco-cd"
    
        ```yaml title=".github/workflows/notify-doco-cd.yml"
        name: Notify Doco-CD
        on:
          workflow_run:
            workflows:
              - Build OCI Artifact
            types:
              - completed
        
        jobs:
          notify:
            if: ${{ github.event.workflow_run.conclusion == 'success' }}
            runs-on: ubuntu-latest
            steps:
              - name: Download artifact metadata
                uses: actions/github-script@v7
                id: metadata
                with:
                  script: |
                    const run = context.payload.workflow_run;
                    const tag = run.head_branch.replace('refs/tags/', '');
                    console.log(`Tag: ${tag}`);
                    return {
                      tag: tag,
                      repository: context.repo.repo
                    }
              
              - name: Send webhook to doco-cd
                env:
                  WEBHOOK_URL: ${{ secrets.DOCO_WEBHOOK_URL }}
                  WEBHOOK_SECRET: ${{ secrets.DOCO_WEBHOOK_SECRET }}
                  TAG: ${{ github.event.workflow_run.head_branch }}
                  REPO: ${{ github.event.workflow_run.repository.full_name }}
                run: |
                  # Note: In production, you'd want to retrieve the actual digest from the build job
                  # For now, you can use a placeholder and query the registry for the latest digest
                  
                  # Option 1: Query GHCR for the digest of the pushed image
                  DIGEST=$(curl -s -H "Authorization: Bearer ${{ secrets.GITHUB_TOKEN }}" \
                    "https://ghcr.io/v2/$REPO-config/manifests/$TAG" \
                    -H "Accept: application/vnd.docker.distribution.manifest.v2+json" \
                    | jq -r '.config.digest')
                  
                  # Create the JSON payload
                  PAYLOAD='{
                    "source": "oci",
                    "digest": "'$DIGEST'",
                    "artifact": "ghcr.io/'$REPO'-config:'$TAG'"
                  }'
                  
                  # Calculate HMAC signature of the payload
                  SIGNATURE=$(echo -n "$PAYLOAD" | openssl dgst -sha256 -hmac "$WEBHOOK_SECRET" -hex | cut -d' ' -f2)
                  
                  # Send webhook with signature
                  curl -X POST "$WEBHOOK_URL" \
                    -H "X-Doco-OCI-Signature-256: sha256=$SIGNATURE" \
                    -H "Content-Type: application/json" \
                    -d "$PAYLOAD"
        ```
    
=== "Pass digest between workflows using job outputs"
    
    For a cleaner approach, you can store the digest as an artifact from the build workflow and retrieve it in the notify workflow:
    
    === "Save the digest"
        ```yaml title=".github/workflows/build-oci-artifact.yml"
        ...

        - name: Save build metadata
          run: |
            echo "${{ steps.build.outputs.digest }}" > digest.txt
            echo "ghcr.io/${{ github.repository }}-config:${{ github.ref_name }}" > artifact.txt
        
        - name: Upload metadata
          uses: actions/upload-artifact@v4
          with:
            name: build-metadata
            path: |
              digest.txt
              artifact.txt
        ```
    
    === "Download and use it"
        ```yaml title=".github/workflows/notify-doco-cd.yml"
        - name: Download build metadata
          uses: actions/download-artifact@v4
          with:
            name: build-metadata
            path: metadata
        
        - name: Read metadata and notify
          env:
            WEBHOOK_URL: ${{ secrets.DOCO_WEBHOOK_URL }}
            WEBHOOK_SECRET: ${{ secrets.DOCO_WEBHOOK_SECRET }}
          run: |
            DIGEST=$(cat metadata/digest.txt)
            ARTIFACT=$(cat metadata/artifact.txt)
            
            PAYLOAD='{
              "source": "oci",
              "digest": "'$DIGEST'",
              "artifact": "'$ARTIFACT'"
            }'
            
            SIGNATURE=$(echo -n "$PAYLOAD" | openssl dgst -sha256 -hmac "$WEBHOOK_SECRET" -hex | cut -d' ' -f2)
            
            curl -X POST "$WEBHOOK_URL" \
              -H "X-Doco-OCI-Signature-256: sha256=$SIGNATURE" \
              -H "Content-Type: application/json" \
              -d "$PAYLOAD"
        ```

---

## OCI_TRUST_POLICY

The `OCI_TRUST_POLICY` environment variable allows you to configure cryptographic signature verification for OCI artifacts. This ensures artifacts are signed by trusted entities before deployment.

### What is OCI Signature Verification?

- **Purpose**: Verify that OCI artifacts were signed by trusted signers
- **Standard**: Uses Cosign notation and OCI Image Spec 1.0 standards
- **Use Cases**: Compliance, supply chain security, preventing unsigned artifacts

### Configuration Format

The `OCI_TRUST_POLICY` is YAML formatted and can be provided as:

1. **Environment variable** (`OCI_TRUST_POLICY`): YAML string
2. **File** (`OCI_TRUST_POLICY_FILE`): Path to YAML file

### Global Trust Policy

Set the app-level trust policy:

```yaml
# As environment variable
OCI_TRUST_POLICY: |
  enabled: true
  public_keys:
    - |
      -----BEGIN PUBLIC KEY-----
      MFkwEwYHKoZIzj0CAQYIKoZIzj0DAQcDQgAE...
      -----END PUBLIC KEY-----
  keyless_identities:
    - issuer: https://token.actions.githubusercontent.com
      subject: repo:myorg/config:ref:refs/heads/main
```

Or use a file:

```bash
export OCI_TRUST_POLICY_FILE=/etc/doco-cd/trust-policy.yaml
```

### Trust Policy Schema

```yaml
enabled: true                    # Enable/disable verification
public_keys:                     # List of trusted public keys (PEM format)
  - |
    -----BEGIN PUBLIC KEY-----
    ...
    -----END PUBLIC KEY-----
keyless_identities:              # Keyless verification using OIDC
  - issuer: https://issuer.url   # OIDC issuer URL
    subject: repo:org/name:*     # Subject pattern
```

### Public Keys Configuration

For verifying artifacts signed with private keys:

```yaml
OCI_TRUST_POLICY: |
  enabled: true
  public_keys:
    - |
      -----BEGIN PUBLIC KEY-----
      MFkwEwYHKoZIzj0CAQYIKoZIzj0DAQcDQgAE7bxKm8YvAjGmqKlWaIu...
      -----END PUBLIC KEY-----
    - |
      -----BEGIN PUBLIC KEY-----
      MFkwEwYHKoZIzj0CAQYIKoZIzj0DAQcDQgAEAnPYz...
      -----END PUBLIC KEY-----
```

### Keyless Identities Configuration

For verifying artifacts signed via OIDC (e.g., GitHub Actions):

```yaml
OCI_TRUST_POLICY: |
  enabled: true
  keyless_identities:
    # GitHub Actions
    - issuer: https://token.actions.githubusercontent.com
      subject: repo:myorg/myrepo:ref:refs/heads/main
    
    # Chainguard
    - issuer: https://accounts.chainguard.dev
      subject: user@example.com
```

### Per-Deployment Trust Policy Overrides

Override the global trust policy for specific deployments:

```yaml
deployments:
  - name: production
    compose_file: docker-compose.yml
    oci_trust_policy:
      verify: true
      public_keys:
        - |
          -----BEGIN PUBLIC KEY-----
          MFkwEwYHKoZIzj0CAQYIKoZIzj0DAQcDQgAE...
          -----END PUBLIC KEY-----
      keyless_identities:
        - issuer: https://token.actions.githubusercontent.com
          subject: repo:myorg/config:ref:refs/heads/main
```

Or disable verification:

```yaml
deployments:
  - name: staging
    compose_file: docker-compose.yml
    oci_trust_policy:
      verify: false
```

### Complete Example: Trust Policy with Both Methods

```yaml
OCI_TRUST_POLICY: |
  enabled: true
  public_keys:
    - |
      -----BEGIN PUBLIC KEY-----
      MFkwEwYHKoZIzj0CAQYIKoZIzj0DAQcDQgAE7bxKm8YvAjGmqKlWaIuQpQ...
      -----END PUBLIC KEY-----
  keyless_identities:
    - issuer: https://token.actions.githubusercontent.com
      subject: repo:kimdre/doco-cd:*
    - issuer: https://accounts.chainguard.dev
      subject: releases@doco-cd.dev
```

### Debugging Trust Policy

To verify your trust policy is configured correctly:

1. Check doco-cd logs for verification errors:
   ```
   ERROR failed to verify OCI artifact signature: ...
   ```

2. Validate YAML syntax:
   ```bash
   yamllint trust-policy.yaml
   ```

3. Ensure public keys are in valid PEM format

4. Verify OIDC issuer URLs are accessible

---

## Complete Examples

### Poll GitHub Container Registry

Configuration for polling deployment configs from GitHub Container Registry:

```yaml
POLL_CONFIG: |
  - source: oci
    artifact: ghcr.io/kimdre/doco-cd-config:main
    layout: doco.v1
    interval: 300
    deployments:
      - name: doco-production
        compose_files:
          - docker-compose.yml
```

### Signature Verification with GitHub Actions

Build and sign artifacts using Cosign, then verify with trust policy:

```yaml
# In your build workflow (GitHub Actions)
- name: Sign and push artifact
  run: |
    cosign sign --key cosign.key ghcr.io/myorg/config:${{ github.ref_name }}

# In doco-cd configuration
OCI_TRUST_POLICY: |
  enabled: true
  keyless_identities:
    - issuer: https://token.actions.githubusercontent.com
      subject: repo:myorg/config:ref:refs/heads/main

POLL_CONFIG: |
  - source: oci
    artifact: ghcr.io/myorg/config:main
    layout: doco.v1
    interval: 300
    deployments:
      - name: production
        compose_file: docker-compose.yml
```

### Example 4: Private Registry with Authentication

Pull from private registry (credentials from `~/.docker/config.json`):

```bash
# Ensure docker login is configured
docker login registry.example.com -u username -p password

# Then run doco-cd
docker run \
  -v ~/.docker/config.json:/root/.docker/config.json:ro \
  -e POLL_CONFIG='
    - source: oci
      artifact: registry.example.com/internal/config:latest
      layout: doco.v1
      interval: 300
      deployments:
        - name: internal-app
          compose_file: docker-compose.yml
  ' \
  ghcr.io/kimdre/doco-cd:latest
```

---

## Troubleshooting

### Artifact Not Found

**Error**: `failed to resolve OCI artifact: ...not found`

**Solutions**:
- Verify artifact reference is correct: `artifact: ghcr.io/org/repo:tag`
- Check registry credentials are configured
- Ensure the artifact exists in the registry

### Invalid Layout

**Error**: `invalid OCI artifact layout: expected layout=doco.v1 in .doco-cd.yaml`

**Solutions**:
- Add top-level `layout: doco.v1` to `.doco-cd.yaml` or `.doco-cd.yml`
- Ensure `.doco-cd.yml` or `.doco-cd.yaml` exists in artifact root
- Recreate artifact with correct structure

### Signature Verification Failed

**Error**: `failed to verify OCI artifact signature: ...`

**Solutions**:
- Verify public key is in valid PEM format
- Check artifact is actually signed
- Verify issuer URL and subject pattern match certificate
- Try disabling verification temporarily to test: `verify: false`

### Webhook Not Triggered

**Issues**:
- Verify webhook payload format matches schema
- Check `X-Doco-OCI-Signature-256` header matches payload
- Ensure `WEBHOOK_SECRET` matches what registry is using
- Check doco-cd is reachable at configured webhook URL

---

## Related Documentation

- [Setup Webhook](Setup-Webhook.md) - General webhook setup and security
- [Deploy Settings](Deploy-Settings.md) - Deployment configuration options
- [Poll Settings](Poll-Settings.md) - Polling configuration details
- [App Settings](App-Settings.md#oci-configuration) - OCI environment variables
