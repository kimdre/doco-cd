---
tags:
  - Advanced
  - OCI
  - Security
  - Verification
---

# OCI Signature Verification and Trust Policies

This page provides comprehensive documentation on configuring OCI artifact signature verification in doco-cd using trust policies.

## Overview

OCI Signature Verification ensures that deployment artifacts are signed by trusted entities before deployment. This is a security best practice that prevents:

- **Unauthorized deployments** - Only signed artifacts can be deployed
- **Tampering** - Modified artifacts would have invalid signatures
- **Compromised registries** - Artifacts from compromised registries without valid signatures are rejected

## Supported Signature Methods

Doco-cd supports two signature verification methods:

1. **Public Key Signatures** - Traditional PKI with public/private key pairs
2. **Keyless Signatures** - OIDC-based verification (e.g., GitHub Actions, Chainguard)

Both can be used together in a single trust policy.

---

## Configuration Methods

### Environment Variable: `OCI_TRUST_POLICY`

Provide the trust policy directly as a YAML string:

```bash
export OCI_TRUST_POLICY='
enabled: true
public_keys:
  - |
    -----BEGIN PUBLIC KEY-----
    MFkwEwYHKoZIzj0CAQYIKoZIzj0DAQcDQgAE...
    -----END PUBLIC KEY-----
'
```

### File-based Configuration: `OCI_TRUST_POLICY_FILE`

For complex policies or sensitive data, use a file:

```bash
export OCI_TRUST_POLICY_FILE=/etc/doco-cd/trust-policy.yaml
```

Contents of `/etc/doco-cd/trust-policy.yaml`:

```yaml
enabled: true
public_keys:
  - |
    -----BEGIN PUBLIC KEY-----
    MFkwEwYHKoZIzj0CAQYIKoZIzj0DAQcDQgAE...
    -----END PUBLIC KEY-----
keyless_identities:
  - issuer: https://token.actions.githubusercontent.com
    subject: repo:myorg/config:*
```

### Docker Compose

```yaml
services:
  doco-cd:
    image: ghcr.io/kimdre/doco-cd:latest
    environment:
      OCI_TRUST_POLICY_FILE: /etc/doco-cd/trust-policy.yaml
    volumes:
      - ./trust-policy.yaml:/etc/doco-cd/trust-policy.yaml:ro
```

---

## Trust Policy Schema

### Full Schema

```yaml
enabled: true                    # Boolean - Enable/disable verification
public_keys:                     # List[string] - PEM-encoded public keys
  - |
    -----BEGIN PUBLIC KEY-----
    ...
    -----END PUBLIC KEY-----
keyless_identities:              # List[object] - OIDC keyless identities
  - issuer: string               # OIDC issuer URL
    subject: string              # Subject pattern (wildcard supported)
```

### Enabling/Disabling Verification

**Global enable/disable**:

```yaml
OCI_TRUST_POLICY: |
  enabled: true   # or false
```

**Per-deployment override**:

```yaml
deployments:
  - name: production
    oci_trust_policy:
      verify: true   # or false to skip verification
```

### Default Behavior

- If `OCI_TRUST_POLICY` is not set: **Verification is disabled** (no signatures checked)
- If `enabled: false`: **Verification is disabled** (explicitly)
- If `enabled: true` but no keys/identities: **All artifacts pass** (warning in logs)
- If `enabled: true` with keys/identities: **Only signed artifacts pass**

---

## Public Key Signatures

Use public keys for verifying artifacts signed with private keys.

### Generating Key Pairs

=== "ECDSA P-256 (Recommended)"
    ```bash
    # Generate private key
    openssl ecparam -name prime256v1 -genkey -noout -out private.pem
    
    # Extract public key
    openssl ec -in private.pem -pubout -out public.pem
    
    # Sign artifact with Cosign
    cosign sign --key private.pem registry.example.com/myapp:latest
    ```

=== "RSA"
    ```bash
    # Generate private key
    openssl genrsa -out private.pem 4096
    
    # Extract public key
    openssl rsa -in private.pem -pubout -out public.pem
    ```

=== "Ed25519"
    ```bash
    # Generate private key
    openssl genpkey -algorithm Ed25519 -out private.pem
    
    # Extract public key
    openssl pkey -in private.pem -pubout -out public.pem
    ```

### Single Public Key

```yaml
OCI_TRUST_POLICY: |
  enabled: true
  public_keys:
    - |
      -----BEGIN PUBLIC KEY-----
      MFkwEwYHKoZIzj0CAQYIKoZIzj0DAQcDQgAE7bxKm8YvAjGmqKlWaIuQpQ...
      -----END PUBLIC KEY-----
```

### Multiple Public Keys

Trust any signature from multiple signers:

```yaml
OCI_TRUST_POLICY: |
  enabled: true
  public_keys:
    - |
      -----BEGIN PUBLIC KEY-----
      MFkwEwYHKoZIzj0CAQYIKoZIzj0DAQcDQgAE7bxKm8YvAjGmqKlWaIuQpQ...
      -----END PUBLIC KEY-----
    - |
      -----BEGIN PUBLIC KEY-----
      MFkwEwYHKoZIzj0CAQYIKoZIzj0DAQcDQgAEAnPYz...
      -----END PUBLIC KEY-----
    - |
      -----BEGIN PUBLIC KEY-----
      MFkwEwYHKoZIzj0CAQYIKoZIzj0DAQcDQgAXkL9F...
      -----END PUBLIC KEY-----
```

### Signing Artifacts with Cosign

```bash
# Sign with private key
cosign sign --key private.pem ghcr.io/myorg/config:latest

# Verify signature
cosign verify --key public.pem ghcr.io/myorg/config:latest
```

---

## Keyless Identities (OIDC)

Use keyless verification for artifacts signed via OIDC providers like GitHub Actions or Chainguard.

### OIDC Basics

Keyless identities verify that:
1. An OIDC provider (issuer) issued the signature
2. The subject (identity) matches expectations

Common OIDC providers:
- **GitHub Actions**: `https://token.actions.githubusercontent.com`
- **Chainguard**: `https://accounts.chainguard.dev`
- **Google**: `https://accounts.google.com`

### GitHub Actions

Verify artifacts signed by GitHub Actions workflows:

```yaml
OCI_TRUST_POLICY: |
  enabled: true
  keyless_identities:
    - issuer: https://token.actions.githubusercontent.com
      subject: repo:myorg/config:*
```

Subject patterns:
- `repo:owner/repo:ref:refs/heads/main` - Specific branch
- `repo:owner/repo:ref:refs/tags/v*` - All version tags
- `repo:owner/repo:*` - Any ref in repository

### Chainguard

Verify artifacts signed by Chainguard identity:

```yaml
OCI_TRUST_POLICY: |
  enabled: true
  keyless_identities:
    - issuer: https://accounts.chainguard.dev
      subject: user@example.com
```

### Multiple OIDC Providers

```yaml
OCI_TRUST_POLICY: |
  enabled: true
  keyless_identities:
    # GitHub Actions CI/CD
    - issuer: https://token.actions.githubusercontent.com
      subject: repo:myorg/config:ref:refs/heads/main
    
    # Chainguard for automated updates
    - issuer: https://accounts.chainguard.dev
      subject: ci@company.com
    
    # Google Service Account
    - issuer: https://accounts.google.com
      subject: config-signer@company.iam.gserviceaccount.com
```

### Signing with Cosign (GitHub Actions)

```yaml
# .github/workflows/build-and-sign.yml
name: Build and Sign

on:
  push:
    branches:
      - main
    tags:
      - 'v*'

jobs:
  build:
    runs-on: ubuntu-latest
    permissions:
      id-token: write
      contents: read
    steps:
      - uses: actions/checkout@v4
      
      - name: Build and push artifact
        run: |
          docker build -t ghcr.io/myorg/config:${{ github.ref_name }} .
          docker push ghcr.io/myorg/config:${{ github.ref_name }}
      
      - name: Sign with Cosign
        uses: sigstore/cosign-installer@v3
      
      - run: |
          cosign sign --yes \
            ghcr.io/myorg/config:${{ github.ref_name }}
        env:
          COSIGN_EXPERIMENTAL: 1
```

---

## Per-Deployment Trust Policy Overrides

Override the global trust policy for specific deployments.

### Override to Enable Verification

```yaml
POLL_CONFIG: |
  - source: oci
    artifact: ghcr.io/myorg/config:main
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
```

### Override to Disable Verification

```yaml
POLL_CONFIG: |
  - source: oci
    artifact: ghcr.io/myorg/test-config:main
    deployments:
      - name: staging
        compose_file: docker-compose.yml
        oci_trust_policy:
          verify: false
```

### Override with Different Keys

```yaml
POLL_CONFIG: |
  - source: oci
    artifact: ghcr.io/myorg/config:main
    deployments:
      - name: production
        compose_file: docker-compose.yml
        oci_trust_policy:
          verify: true
          public_keys:
            - |
              -----BEGIN PUBLIC KEY-----
              (production signing key)
              -----END PUBLIC KEY-----
      
      - name: staging
        compose_file: docker-compose.yml
        oci_trust_policy:
          verify: true
          public_keys:
            - |
              -----BEGIN PUBLIC KEY-----
              (staging signing key)
              -----END PUBLIC KEY-----
```

---

## Complete Examples

### Example 1: GitHub Actions Keyless Signing

Production setup using GitHub Actions to sign artifacts:

```yaml
# .doco-cd Docker Compose
services:
  doco-cd:
    environment:
      OCI_TRUST_POLICY: |
        enabled: true
        keyless_identities:
          - issuer: https://token.actions.githubusercontent.com
            subject: repo:kimdre/doco-cd:ref:refs/heads/main

# .doco-cd polling config
POLL_CONFIG: |
  - source: oci
    artifact: ghcr.io/kimdre/doco-cd-config:main
    reference: main
    interval: 300
    deployments:
      - name: production
        compose_file: docker-compose.yml
```

### Example 2: Multiple Signing Keys

Support multiple team members/signers:

```yaml
OCI_TRUST_POLICY: |
  enabled: true
  public_keys:
    - |
      -----BEGIN PUBLIC KEY-----
      MFkwEwYHKoZIzj0CAQYIKoZIzj0DAQcDQgAE7bxKm8YvAjGmqKlWaIuQpQ... (DevOps Team)
      -----END PUBLIC KEY-----
    - |
      -----BEGIN PUBLIC KEY-----
      MFkwEwYHKoZIzj0CAQYIKoZIzj0DAQcDQgAEAnPYz... (Security Team)
      -----END PUBLIC KEY-----
    - |
      -----BEGIN PUBLIC KEY-----
      MFkwEwYHKoZIzj0CAQYIKoZIzj0DAQcDQgAXkL9F... (Lead Architect)
      -----END PUBLIC KEY-----
```

### Example 3: Hybrid (Public Keys + Keyless)

Trust both traditional signatures and GitHub Actions:

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
      subject: repo:myorg/config:ref:refs/heads/*
```

### Example 4: Progressive Rollout

Different verification levels for different environments:

```yaml
POLL_CONFIG: |
  - source: oci
    artifact: ghcr.io/myorg/config:main
    reference: main
    interval: 300
    deployments:
      # Development: No verification required
      - name: development
        compose_file: docker-compose.yml
        oci_trust_policy:
          verify: false
      
      # Staging: Single signer required
      - name: staging
        compose_file: docker-compose.yml
        oci_trust_policy:
          verify: true
          public_keys:
            - |
              -----BEGIN PUBLIC KEY-----
              MFkwEwYHKoZIzj0CAQYIKoZIzj0DAQcDQgAE... (Staging Lead)
              -----END PUBLIC KEY-----
      
      # Production: Multiple signers or GitHub Actions
      - name: production
        compose_file: docker-compose.yml
        oci_trust_policy:
          verify: true
          public_keys:
            - |
              -----BEGIN PUBLIC KEY-----
              MFkwEwYHKoZIzj0CAQYIKoZIzj0DAQcDQgAE... (Head of DevOps)
              -----END PUBLIC KEY-----
            - |
              -----BEGIN PUBLIC KEY-----
              MFkwEwYHKoZIzj0CAQYIKoZIzj0DAQcDQgAE... (Compliance Officer)
              -----END PUBLIC KEY-----
          keyless_identities:
            - issuer: https://token.actions.githubusercontent.com
              subject: repo:myorg/config:ref:refs/tags/v*
```

---

## Troubleshooting

### Verification Failed: "Invalid Signature"

**Cause**: Artifact signature doesn't match public key or was corrupted

**Solutions**:
1. Verify artifact was actually signed: `cosign verify --key public.pem artifact:tag`
2. Check that you're using the correct public key
3. Ensure artifact hasn't been modified since signing
4. Try disabling verification temporarily: `verify: false`

### Verification Failed: "No Matching Identity"

**Cause**: OIDC subject doesn't match expected pattern

**Solutions**:
1. Check OIDC issuer URL is correct
2. Verify subject pattern matches (wildcards, etc.)
3. Check certificate subject matches pattern
4. Print certificate details: `cosign verify ghcr.io/myorg/image:tag`

### Public Key Format Error

**Cause**: Public key is not in valid PEM format

**Solutions**:
1. Verify key starts with `-----BEGIN PUBLIC KEY-----`
2. Check key ends with `-----END PUBLIC KEY-----`
3. Ensure proper indentation (no leading spaces)
4. Validate with: `openssl pkey -in public.pem -text -noout`

### Artifact Passes Verification But Shouldn't

**Cause**: Trust policy is not enabled or is misconfigured

**Solutions**:
1. Check `enabled: true` in policy
2. Verify public keys or keyless identities are configured
3. Check deployment-level overrides aren't disabling verification
4. Review logs for detailed verification status

### No Signatures Found on Artifact

**Cause**: Artifact was not signed before pushing

**Solutions**:
1. Sign artifact before pushing: `cosign sign --key private.pem ghcr.io/myorg/image:tag`
2. Update your CI/CD pipeline to sign artifacts
3. Verify signature exists: `cosign verify --key public.pem ghcr.io/myorg/image:tag`

### OIDC Certificate Verification Failed

**Cause**: OIDC provider certificate is invalid or issuer URL is wrong

**Solutions**:
1. Verify OIDC issuer URL is accessible and correct
2. Check issuer certificate is valid
3. Ensure your system time is correct (certificate validity window)
4. For GitHub Actions, issuer must be exactly: `https://token.actions.githubusercontent.com`

---

## Best Practices

1. **Enable verification in production** - Always verify signatures for production deployments
2. **Use keyless for CI/CD** - Keyless identities reduce key management burden
3. **Separate keys by role** - Use different keys for different teams/environments
4. **Rotate keys regularly** - Plan for key rotation and maintain key history
5. **Document your policy** - Maintain clear documentation of your trust setup
6. **Test before enforcing** - Test verification with `verify: false` first
7. **Monitor verification failures** - Alert on signature verification failures
8. **Use version tags for releases** - Sign releases with specific version tags

---

## Related Documentation

- [OCI Usage](../OCI-Usage.md) - Complete OCI usage guide
- [OCI Webhook Payload](../Endpoints/OCI-Webhook-Payload.md) - Webhook payload schema
- [App Settings](../App-Settings.md) - Environment variable reference

