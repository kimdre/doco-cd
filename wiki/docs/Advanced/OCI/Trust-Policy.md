---
tags:
  - Advanced
  - OCI
  - Security
---

# OCI Signature Verification and Trust Policies

!!! example "Experimental Feature"
    OCI artifact support is currently experimental.
    Please [provide feedback and report any issues](../../Contributing/#have-an-issue-idea-or-question) you encounter.

This page provides comprehensive documentation on configuring OCI artifact signature verification in doco-cd using trust policies.

## Overview

OCI Signature Verification ensures that deployment artifacts are signed by trusted entities before deployment. This is a security best practice that prevents:

- **Unauthorized deployments** - Only signed artifacts can be deployed
- **Tampering** - Modified artifacts would have invalid signatures
- **Compromised registries** - Artifacts from compromised registries without valid signatures are rejected

!!! note "Signature verification is disabled by default" 
    Verification only runs when explicitly enabled via [global configuration](#global-configuration) or [per-deployment override](#per-deployment-override).

## Supported Signature Methods

Doco-cd supports two signature verification methods:

1. **Public Key Signatures** - Traditional PKI with public/private key pairs
2. **Keyless Signatures** - OIDC-based verification (e.g., GitHub Actions, Google Service Accounts)

Both can be used together in a single trust policy.

---

## Trust Policy Schema

```yaml
enabled: true                    # Enable/disable verification
ignore_tlog: false               # Skip Rekor transparency log verification (default: false)
public_keys:                     # List of trusted public keys (PEM format)
  - |
    -----BEGIN PUBLIC KEY-----
    ...
    -----END PUBLIC KEY-----
keyless_identities:              # Keyless verification using OIDC
  - issuer: https://issuer.url   # OIDC issuer URL
    subject: https://github.com/org/repo/.github/workflows/build.yml@refs/heads/main   # Exact certificate SAN match (optional)
    subject_regexp: ^https://github.com/org/repo/.+@refs/heads/main$                    # Regex SAN match (optional)
```

### Available Options

#### Global configuration

| Key                      | Type   | Description                                                                                                                                                                              |
|--------------------------|--------|------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| `OCI_TRUST_POLICY`       | string | YAML-formatted OCI signature trust policy for verifying artifact signatures. Supports public keys and keyless (OIDC) identities. Verification is disabled unless `enabled: true` is set. |
| `OCI_TRUST_POLICY_FILE`  | string | Path to the file containing OCI trust policy YAML (Mutually exclusive with `OCI_TRUST_POLICY`).                                                                                          |
| `OCI_VERIFY_MAX_WORKERS` | number | Maximum number of workers used per OCI signature verification. Values below `1` are invalid. Values above `10` are clamped to `10`.                                                      |

!!! note "Verification Parallelism and Memory Usage"
    
    `OCI_VERIFY_MAX_WORKERS` applies per OCI verification call.
    Combined with `MAX_CONCURRENT_DEPLOYMENTS`, total verification parallelism can grow roughly with both settings.

    A higher number of workers can speed up verification for artifacts with many signatures or complex policies, but also increases memory usage.
    
    For example:
    
    - `MAX_CONCURRENT_DEPLOYMENTS=4`
    - `OCI_VERIFY_MAX_WORKERS=2`
    
    can allow up to roughly `8` concurrent verification workers in the worst case.

##### Trust Policy Fields

| Key                  | Type             | Default | Description                                                                         |
|----------------------|------------------|---------|-------------------------------------------------------------------------------------|
| `enabled`            | boolean          | `false` | Enables OCI signature verification globally. When `false`, verification is skipped. |
| `public_keys`        | array of strings | `[]`    | List of trusted public keys (PEM format) used by Cosign key verification.           |
| `keyless_identities` | array of object  | `[]`    | List of trusted keyless identities used for OIDC-based verification.                |
| `ignore_tlog`        | boolean          | `false` | When `true`, skip Rekor transparency log verification. Only use in air-gapped or private environments where signatures are not logged to Rekor. |

##### `keyless_identities` object fields

| Key              | Type   | Description                                                                                                                                                                   |
|------------------|--------|-------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| `issuer`         | string | OIDC issuer URL (for example `https://token.actions.githubusercontent.com`).                                                                                                  |
| `subject`        | string | Exact certificate identity (SAN) match (for example `https://github.com/org/repo/.github/workflows/build.yml@refs/heads/main`). **Mutually exclusive with `subject_regexp`.** |
| `subject_regexp` | string | Regular expression for certificate identity (SAN) matching. Useful when workflow file paths or refs may vary. **Mutually exclusive with `subject`.**                          |

!!! note "Docker Compose escaping for `subject_regexp`"
    If you set `OCI_TRUST_POLICY` inside `docker-compose.yml`, Docker Compose treats`$` characters as [variable interpolation](https://docs.docker.com/reference/compose-file/interpolation/) 
    and removes it before passing the value to doco-cd.
    Use `$$` (double-dollar sign) in regex patterns so a literal `$` reaches doco-cd.

    Example in `docker-compose.yml`:

    ```yaml
    services:
      doco-cd:
        environment:
          OCI_TRUST_POLICY: |
            enabled: true
            keyless_identities:
              - issuer: https://token.actions.githubusercontent.com
                subject_regexp: ^https://github.com/myorg/myrepo/.+@refs/heads/main$$
    ```

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

| Key                  | Type             | Default | Description                                                                                             |
|----------------------|------------------|---------|---------------------------------------------------------------------------------------------------------|
| `verify`             | boolean          | unset   | Can enforce verification (`true`) per deployment. It cannot disable verification when global `enabled: true` is set. |
| `public_keys`        | array of strings | inherit | Replaces global `public_keys` when set and non-empty.                                                   |
| `keyless_identities` | array of objects | inherit | Replaces global `keyless_identities` when set and non-empty.                                            |
| `ignore_tlog`        | boolean          | inherit | When `true`, skip Rekor transparency log verification. Overrides the global `ignore_tlog` setting when set. |

!!!note "Behavior"

    - If `verify` is unset, the deployment inherits the global `enabled` value.
    - If `verify: true` is set, verification is enforced regardless of the global setting.
    - If global `enabled: true` is set, `verify: false` is ignored (no downgrade allowed).
    - If both `public_keys` and `keyless_identities` are empty while verification is enabled, verification fails because no trust rules are defined.
    - Per-deployment `public_keys` and `keyless_identities` override global values only when the respective lists are non-empty.
    - For OCI sources, trust-policy overrides are only accepted from trusted inline poll deployments (`POLL_CONFIG.deployments`). Artifact-contained `.doco-cd.yml` cannot define the policy used to verify itself.

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
              subject: https://github.com/myorg/config:ref:refs/heads/main
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
          verify: true   # `verify: false` is ignored when global enabled=true
    ```

### Ignoring Rekor Transparency Logs

By default, Doco-CD verifies that signatures are logged in the Rekor transparency log when verifying Cosign signatures. This is the security best practice and is enabled by default.

However, in air-gapped, private, or Rekor-independent environments, signatures may not be uploaded to Rekor. In these cases, you can skip transparency log verification while still verifying the signature with the configured public key or keyless identity.

!!! warning "Use with Caution"
    Only skip transparency log verification when operating in a controlled environment where artifacts cannot be tampered with. In public environments, skipping transparency log verification reduces security guarantees.

=== "Global Configuration"

    ```yaml
    OCI_TRUST_POLICY: |
      enabled: true
      ignore_tlog: true
      public_keys:
        - |
          -----BEGIN PUBLIC KEY-----
          MFkwEwYHKoZIzj0CAQYIKoZIzj0DAQcDQgAE...
          -----END PUBLIC KEY-----
    ```

=== "Per-deployment Override"

    ```yaml
    deployments:
      - name: air-gapped-environment
        oci:
          ignore_tlog: true
          public_keys:
            - |
              -----BEGIN PUBLIC KEY-----
              MFkwEwYHKoZIzj0CAQYIKoZIzj0DAQcDQgAE...
              -----END PUBLIC KEY-----
    ```

---

## Public Key Signatures

Use public keys for verifying artifacts signed with private keys.

### Generating Key Pairs

=== "Ed25519 (Recommended)"
    ```bash
    # Generate private key
    openssl genpkey -algorithm Ed25519 -out private.pem

    # Extract public key
    openssl pkey -in private.pem -pubout -out public.pem
    ```

=== "ECDSA P-256"
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

[Cosign](https://github.com/sigstore/cosign) is a popular tool for signing OCI artifacts. 
Use the private key to sign your artifact and the public key for verification.

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
      subject_regexp: ^https://github.com/myorg/config/.+@refs/heads/main$
```

Subject examples:

- `https://github.com/owner/repo/.github/workflows/build.yml@refs/heads/main` - Exact workflow and branch
- `https://github.com/owner/repo/.github/workflows/release.yml@refs/heads/main` - Exact workflow and tag

Subject regexp examples:

- `^https://github.com/owner/repo/.+@refs/heads/main$` - Any workflow file on `main`
- `^https://github.com/owner/repo/.+@refs/tags/v.*$` - Any workflow file for version tags

### Multiple OIDC Providers

```yaml
OCI_TRUST_POLICY: |
  enabled: true
  keyless_identities:
    # GitHub Actions CI/CD
    - issuer: https://token.actions.githubusercontent.com
      subject_regexp: ^https://github.com/myorg/config/.+@refs/heads/main$
    
    # Google Service Account
    - issuer: https://accounts.google.com
      subject: config-signer@company.iam.gserviceaccount.com
```

### Signing with Cosign (GitHub Actions)

```yaml title=".github/workflows/build-and-sign.yml"
name: Build and Sign

permissions:
  contents: read
  packages: write
  id-token: write # needed for signing the images with GitHub OIDC Token

on:
  push:

jobs:
  build:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v6
        with:
          fetch-depth: 1

      - name: Install Cosign
        uses: sigstore/cosign-installer@v4.1.0

      - name: Set up Docker Buildx
        uses: docker/setup-buildx-action@v3.11.1

      - name: Login to GitHub Container Registry
        uses: docker/login-action@v3.4.0
        with:
          registry: ghcr.io
          username: ${{ github.actor }}
          password: ${{ secrets.GITHUB_TOKEN }}

      - id: docker_meta
        uses: docker/metadata-action@v5.7.0
        with:
          images: ghcr.io/my/app # (1)!
          tags: type=sha,format=long

      - name: Build and Push container images
        uses: docker/build-push-action@v6.18.0
        id: build-and-push
        with:
          platforms: linux/amd64
          push: true
          tags: ${{ steps.docker_meta.outputs.tags }}

      - name: Sign OIDC artifact
        env:
          DIGEST: ${{ steps.build-and-push.outputs.digest }}
          TAGS: ${{ steps.docker_meta.outputs.tags }}
        run:
          images="";
          for tag in ${TAGS}; do
          images+="${tag}@${DIGEST} ";
          done;
          cosign sign --yes ${images}
```

1. Change to your image name.

---

## Examples

### GitHub Actions Keyless Signing

Production setup using GitHub Actions to sign artifacts:

```yaml title="Doco-CD docker-compose.yml"
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

```yaml title="Doco-CD docker-compose.yml"
POLL_CONFIG: |
  - source: oci
    url: ghcr.io/myorg/config:main
    reference: main
    interval: 300
    deployments:
      # Development: Inherits global policy
      - name: development
        compose_file: docker-compose.yml
      
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
4. In non-production, temporarily set global `enabled: false` only while debugging policy issues

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
3. Check global policy and deployment-level trust rules (`public_keys` / `keyless_identities`) for mismatches
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
6. **Test rollout safely** - Validate in staging first, then enforce with global `enabled: true`
7. **Monitor verification failures** - Alert on signature verification failures
8. **Use version tags for releases** - Sign releases with specific version tags

