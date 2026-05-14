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

!!! note
    Authentication to private registries uses the local Docker credentials (typically in `~/.docker/config.json`).

---

## doco.v1 Artifact Layout

The **doco.v1** layout is a strict, versioned specification for packaging deployment configurations as OCI artifacts. It ensures consistency and enables validation.

### Artifact Structure

A doco.v1 artifact must have the following structure:

```
artifact/
├── .doco-cd.yaml         # OR .doco-cd.yml - Main deployment config with `layout: doco.v1`
├── docker-compose.yml    # (optional) Docker Compose configuration
└── (other files as needed)
```

### Required Files

#### `.doco-cd.yaml` or `.doco-cd.yml`
**Required** - The main deployment configuration file.

- **Format**: YAML
- **Layout version**: Must include top-level `layout: doco.v1`
- **Purpose**: Contains deployment specifications (compose files, profiles, replacements, pre/post scripts, etc.)
- **Example**:
  ```yaml
  layout: doco.v1
  deployments:
    - name: production
      compose_files:
        - docker-compose.yml
      profiles:
        - app
        - db
  ```

For detailed configuration options, see the [Deploy Settings](Deploy-Settings.md) documentation.

### Optional Files

Any additional files can be included and will be extracted to the deployment directory:

- `docker-compose.yml` - Docker Compose configuration
- `.env` files - Environment files for compose variable interpolation
- Scripts for pre/post-deployment hooks
- Documentation or other supporting files

### Example: Creating a doco.v1 Artifact

Here's a complete example of creating and pushing a doco.v1 artifact:

=== "Step 1: Create the artifact directory"
    ```sh
    mkdir -p artifact
    cd artifact
    
    # Create deployment configuration
    cat > .doco-cd.yaml << 'EOF'
    layout: doco.v1
    deployments:
      - name: web-app
        compose_files:
          - docker-compose.yml
        profiles:
          - app
          - nginx
    EOF
    
    # Create docker-compose.yml
    cat > docker-compose.yml << 'EOF'
    version: '3.8'
    services:
      app:
        image: myapp:latest
      nginx:
        image: nginx:latest
    EOF
    ```

=== "Step 2: Create the OCI artifact (using Docker)"
    ```sh
    # Create a TAR archive of the artifact
    tar -czf artifact.tar.gz -C artifact .
    
    # Create a temporary Dockerfile
    cat > Dockerfile << 'EOF'
    FROM scratch
    ADD artifact.tar.gz /
    EOF
    
    # Build and push the artifact
    docker build -t ghcr.io/myorg/myapp-config:main .
    docker push ghcr.io/myorg/myapp-config:main
    ```

=== "Step 3: Create the OCI artifact (using doco-cd tool)"
    If you create a doco-cd tool/script:
    ```sh
    # Using skopeo or similar tools
    skopeo copy dir://artifact oci:oras-artifact:latest
    ```

### Validation Rules

The doco.v1 layout is validated when the artifact is pulled. Validation checks:

1. ✅ At least one of `.doco-cd.yaml` or `.doco-cd.yml` exists
2. ✅ Artifact layout version is `doco.v1` (from top-level `layout` in `.doco-cd.yaml/.yml`)
3. ✅ No path traversal attempts in extracted files
4. ✅ All files are properly extracted from TAR layers

If validation fails, the deployment is rejected with an error.

---

## Polling with OCI

Use polling to periodically check for new versions of OCI artifacts.

### Configuration

Add an OCI polling configuration to `POLL_CONFIG`:

```yaml
- source: oci
  artifact: ghcr.io/myorg/myapp-config:latest
  layout: doco.v1
  reference: main           # git-like reference for tagging
  interval: 300             # poll every 5 minutes
  deployments:
    - name: production
      compose_files:
        - docker-compose.yml
```

### Parameters

| Parameter | Required | Default | Description |
|-----------|----------|---------|-------------|
| `source` | ✅ | - | Must be `oci` |
| `artifact` | ✅ | - | Full OCI artifact reference (e.g., `ghcr.io/myorg/app:main`) |
| `layout` | ❌ | `doco.v1` | OCI artifact layout version (currently only `doco.v1` supported) |
| `reference` | ✅ | - | Reference tag for correlation (e.g., `main`, `production`) |
| `interval` | ❌ | `180` | Poll interval in seconds (minimum: 10) |
| `deployments` | ✅ | - | Array of deployment configurations |

### Example: Full Polling Configuration

```yaml
POLL_CONFIG: |
  - source: oci
    artifact: ghcr.io/myorg/config:latest
    layout: doco.v1
    reference: production
    interval: 300
    deployments:
      - name: web-production
        compose_files:
          - docker-compose.yml
        profiles:
          - webapp
          - redis

  - source: oci
    artifact: ghcr.io/myorg/config:staging
    layout: doco.v1
    reference: staging
    interval: 180
    deployments:
      - name: web-staging
        compose_files:
          - docker-compose.yml
        profiles:
          - webapp
```

---

## Webhooks with OCI

Use OCI webhooks to trigger immediate deployments when artifacts are pushed.

### OCI Webhook Payload Schema

The OCI webhook payload follows this JSON schema:

```json
{
  "ref": "latest",
  "digest": "sha256:abcdef1234567890...",
  "repository": "myorg/myapp-config",
  "artifact": "ghcr.io/myorg/myapp-config:latest"
}
```

### Payload Fields

| Field | Type | Description |
|-------|------|-------------|
| `ref` | string | Tag or reference of the artifact (e.g., `latest`, `main`, `v1.0.0`) |
| `digest` | string | Complete OCI digest including algorithm (e.g., `sha256:abc123...`) |
| `repository` | string | Repository name without registry host (e.g., `myorg/myapp-config`) |
| `artifact` | string | Full artifact reference including registry (e.g., `ghcr.io/myorg/myapp-config:latest`) |

### Example Payloads

=== "GitHub Container Registry Webhook"
    ```json
    {
      "ref": "main",
      "digest": "sha256:7d6c7f8e9a0b1c2d3e4f5a6b7c8d9e0f1a2b3c4d5e6f7a8b9c0d1e2f3a4b5c6",
      "repository": "kimdre/doco-cd",
      "artifact": "ghcr.io/kimdre/doco-cd:main"
    }
    ```

=== "Custom OCI Registry Webhook"
    ```json
    {
      "ref": "v1.2.3",
      "digest": "sha256:abc123def456ghi789jkl012mno345pqr678stu901vwx234yz567abc890def",
      "repository": "myorg/config",
      "artifact": "registry.example.com/myorg/config:v1.2.3"
    }
    ```

### Setting Up OCI Webhooks

Most OCI registries support webhooks. Configuration varies by registry:

=== "GitHub Container Registry"
    Currently, GCR doesn't provide native webhook support. Use GitHub Actions to send webhooks:
    
    ```yaml
    name: Notify Doco-CD
    on:
      push:
        tags:
          - 'v*'
    
    jobs:
      notify:
        runs-on: ubuntu-latest
        steps:
          - name: Send webhook
            env:
              WEBHOOK_URL: ${{ secrets.DOCO_WEBHOOK_URL }}
              WEBHOOK_SECRET: ${{ secrets.DOCO_WEBHOOK_SECRET }}
            run: |
              DIGEST=$(echo -n "artifact-digest" | sha256sum | cut -d' ' -f1)
              curl -X POST "$WEBHOOK_URL" \
                -H "X-Doco-OCI-Signature-256: sha256=$(echo -n '{\"ref\":\"${{ github.ref_name }}\",\"digest\":\"sha256:'$DIGEST'\",\"repository\":\"org/repo\",\"artifact\":\"ghcr.io/org/repo:${{ github.ref_name }}\"}' | openssl dgst -sha256 -hmac "$WEBHOOK_SECRET" -hex | cut -d' ' -f2)" \
                -H "Content-Type: application/json" \
                -d '{
                  "ref": "${{ github.ref_name }}",
                  "digest": "sha256:'$DIGEST'",
                  "repository": "org/repo",
                  "artifact": "ghcr.io/org/repo:${{ github.ref_name }}"
                }'
    ```

=== "Docker Hub"
    Docker Hub supports native webhooks:
    
    1. Go to your repository settings
    2. Click "Webhooks"
    3. Add a new webhook with:
       - **Webhook URL**: `https://your-doco-server/v1/webhook`
       - Configure the payload to match the doco.v1 OCI webhook schema

=== "Harbor / Private Registry"
    Most private registries support webhooks in their admin panel. Configure them to POST to:
    
    ```
    https://your-doco-server/v1/webhook
    ```
    
    Transform the payload to match the doco.v1 schema if needed.

### Webhook Security

OCI webhooks use HMAC-SHA256 signatures for security:

**Header**: `X-Doco-OCI-Signature-256`

**Format**: `sha256={hex_encoded_hmac}`

Example:
```
X-Doco-OCI-Signature-256: sha256:abc123def456...
```

The signature is calculated using the raw request body and your `WEBHOOK_SECRET`:

```python
import hmac
import hashlib

payload = request.get_data()
secret = os.getenv('WEBHOOK_SECRET')
expected_signature = hmac.new(
    secret.encode(),
    payload,
    hashlib.sha256
).hexdigest()
```

For more information, see [Setup Webhook](Setup-Webhook.md).

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

### Example 1: Poll GitHub Container Registry

Configuration for polling deployment configs from GitHub Container Registry:

```yaml
POLL_CONFIG: |
  - source: oci
    artifact: ghcr.io/kimdre/doco-cd-config:main
    layout: doco.v1
    reference: main
    interval: 300
    deployments:
      - name: doco-production
        compose_files:
          - docker-compose.yml
        profiles:
          - app
          - database
```

### Example 2: Webhook from Docker Hub

Accept webhooks from Docker Hub and deploy immediately:

```bash
docker run \
  -e WEBHOOK_SECRET="your-secret" \
  -e POLL_CONFIG='
    - source: oci
      artifact: docker.io/myusername/config:latest
      layout: doco.v1
      reference: latest
      interval: 0          # 0 = webhook only, no polling
      deployments:
        - name: web-app
          compose_file: docker-compose.yml
  ' \
  -p 80:80 \
  ghcr.io/kimdre/doco-cd:latest
```

### Example 3: Signature Verification with GitHub Actions

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
    reference: main
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
      reference: latest
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




