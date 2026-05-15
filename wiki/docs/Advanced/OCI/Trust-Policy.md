---
tags:
  - Advanced
  - OCI
  - Security
---

# OCI Signature Verification and Trust Policies

This page provides comprehensive documentation on configuring OCI artifact signature verification in doco-cd using trust policies.

## Overview

OCI Signature Verification ensures that deployment artifacts are signed by trusted entities before deployment. This is a security best practice that prevents:

- **Unauthorized deployments** - Only signed artifacts can be deployed
- **Tampering** - Modified artifacts would have invalid signatures
- **Compromised registries** - Artifacts from compromised registries without valid signatures are rejected

!!! note "Signature verification is disabled by default" 
    Verification only runs when explicitly enabled via [global configuration](#global-trust-policy) or [per-deployment override](#per-deployment-override).

## Supported Signature Methods

Doco-cd supports two signature verification methods:

1. **Public Key Signatures** - Traditional PKI with public/private key pairs
2. **Keyless Signatures** - OIDC-based verification (e.g., GitHub Actions, Google Service Accounts)

Both can be used together in a single trust policy.

---

## Trust Policy Schema

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

### Available Options

#### Global trust policy

| Key                     | Type   | Description                                                                                                                                                                              |
|-------------------------|--------|------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| `OCI_TRUST_POLICY`      | string | YAML-formatted OCI signature trust policy for verifying artifact signatures. Supports public keys and keyless (OIDC) identities. Verification is disabled unless `enabled: true` is set. |
| `OCI_TRUST_POLICY_FILE` | string | Path to the file containing OCI trust policy YAML (Mutually exclusive with `OCI_TRUST_POLICY`).                                                                                          |

| Key                  | Type       | Default | Description                                                                         |
|----------------------|------------|---------|-------------------------------------------------------------------------------------|
| `enabled`            | boolean    | `false` | Enables OCI signature verification globally. When `false`, verification is skipped. |
| `public_keys`        | \[\]string | `[]`    | List of trusted public keys (PEM format) used by Cosign key verification.           |
| `keyless_identities` | \[\]object | `[]`    | List of trusted keyless identities used for OIDC-based verification.                |

`keyless_identities` object fields:

| Key       | Type   | Required | Description                                                                                           |
|-----------|--------|----------|-------------------------------------------------------------------------------------------------------|
| `issuer`  | string | yes      | OIDC issuer URL (for example `https://token.actions.githubusercontent.com`).                          |
| `subject` | string | yes      | Expected certificate identity subject (supports patterns as interpreted by Cosign identity matching). |

!!! example "Configuring Global Trust Policy"
    === "Environment Variable Configuration"
    
        Provide the trust policy directly as a YAML string:
        
        ```yaml title="docker-compose.yml"
        services:
          doco-cd:
            image: ghcr.io/kimdre/doco-cd:latest
            environment:
              OCI_TRUST_POLICY: |
                enabled: true
                public_keys:
                  - |
                    -----BEGIN PUBLIC KEY-----
                    MFkwEwYHKoZIzj0CAQYIKoZIzj0DAQcDQgAE...
                    -----END PUBLIC KEY-----
        ```
    
    === "File-based Configuration"
    
        For complex policies or sensitive data, use `OCI_TRUST_POLICY_FILE` with a file:
        
        ```yaml title="trust-policy.yaml"
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
    
        ```yaml title="docker-compose.yml"
        services:
          doco-cd:
            image: ghcr.io/kimdre/doco-cd:latest
            environment:
              OCI_TRUST_POLICY_FILE: /etc/doco-cd/trust-policy.yaml
            volumes:
              - ./trust-policy.yaml:/etc/doco-cd/trust-policy.yaml:ro
        ```

#### Per-deployment override

Override the global trust policy for specific [deployments](../../Poll-Settings.md#inline-deploy-configs):

| Key                  | Type     | Default | Description                                                                                             |
|----------------------|----------|---------|---------------------------------------------------------------------------------------------------------|
| `verify`             | boolean  | unset   | Overrides global `enabled` for this deployment only. `true` enforces verification, `false` disables it. |
| `public_keys`        | \[\]string | inherit | Replaces global `public_keys` when set and non-empty.                                                   |
| `keyless_identities` | \[\]object | inherit | Replaces global `keyless_identities` when set and non-empty.                                            |

!!!note "Behavior"

    - If `verify` is unset, the deployment inherits the global `enabled` value. If `verify: true` is set, verification is enforced regardless of the global setting.
    - If both `public_keys` and `keyless_identities` are empty while verification is enabled, verification fails because no trust rules are defined.
    - Per-deployment `public_keys` and `keyless_identities` override global values only when the respective lists are non-empty.

!!! example "Per-deployment Override Example"
    ```yaml
    deployments:
      - name: production
        compose_file: docker-compose.yml
        oci:
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

### Enabling/Disabling Verification

=== "Global enable/disable"

    ```yaml
    OCI_TRUST_POLICY: |
      enabled: true   # or false
    ```

=== "Per-deployment override"

    ```yaml
    deployments:
      - name: production
        oci:
          verify: true   # or false to skip verification
    ```

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

### Configuring Public Keys

=== "Single Public Key"

    ```yaml
    OCI_TRUST_POLICY: |
      enabled: true
      public_keys:
        - |
          -----BEGIN PUBLIC KEY-----
          MFkwEwYHKoZIzj0CAQYIKoZIzj0DAQcDQgAE7bxKm8YvAjGmqKlWaIuQpQ...
          -----END PUBLIC KEY-----
    ```

=== "Multiple Public Keys"
    
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

Use keyless verification for artifacts signed via OIDC providers like GitHub Actions or Google Service Accounts. 
This eliminates the need for managing public keys and allows dynamic trust based on identity claims.

### OIDC Basics

Keyless identities verify that:

1. An OIDC provider (issuer) issued the signature
2. The subject (identity) matches expectations

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

### Multiple OIDC Providers

```yaml
OCI_TRUST_POLICY: |
  enabled: true
  keyless_identities:
    # GitHub Actions CI/CD
    - issuer: https://token.actions.githubusercontent.com
      subject: repo:myorg/config:ref:refs/heads/main
    
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
      - uses: actions/checkout@v6
      
      - name: Build and push artifact
        run: |
          docker build -t ghcr.io/myorg/config:${{ github.ref_name }} .
          docker push ghcr.io/myorg/config:${{ github.ref_name }}
      
      - name: Sign with Cosign
        uses: sigstore/cosign-installer@v4
      
      - run: |
          cosign sign --yes ghcr.io/myorg/config:${{ github.ref_name }}
```

---

## Examples

### GitHub Actions Keyless Signing

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
      POLL_CONFIG: |
        - source: oci
          url: ghcr.io/kimdre/doco-cd-config:main
          reference: main
          interval: 300
          deployments:
            - name: production
              compose_file: docker-compose.yml
```

### Progressive Rollout

Different verification levels for different environments:

```yaml
POLL_CONFIG: |
  - source: oci
    url: ghcr.io/myorg/config:main
    reference: main
    interval: 300
    deployments:
      # Development: No verification required
      - name: development
        compose_file: docker-compose.yml
        oci:
          verify: false
      
      # Staging: Single signer required
      - name: staging
        compose_file: docker-compose.yml
        oci:
          verify: true
          public_keys:
            - |
              -----BEGIN PUBLIC KEY-----
              MFkwEwYHKoZIzj0CAQYIKoZIzj0DAQcDQgAE... (Staging Lead)
              -----END PUBLIC KEY-----
      
      # Production: Multiple signers or GitHub Actions
      - name: production
        compose_file: docker-compose.yml
        oci:
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
2. Check that you're using the correct public key in valid PEM format
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

