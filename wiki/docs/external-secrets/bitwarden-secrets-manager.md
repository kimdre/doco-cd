---
tags:
  - External Secrets
  - Configuration
---

# Bitwarden Secrets Manager

!!! warning
    Bitwarden Secrets Manager is not available in images for ARMv7 architectures (e.g. Raspberry Pi OS 32-bit).

## Environment Variables

To use Bitwarden Secrets Manager, you need to set the following environment variables:

| Key                                 | Value                                                                                                                                                                               | Default                                |
|-------------------------------------|-------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|:---------------------------------------|
| `SECRET_PROVIDER`                   | `bitwarden_sm`                                                                                                                                                                      |                                        |
| `SECRET_PROVIDER_API_URL`           | US: `https://vault.bitwarden.com/api`</br> EU: `https://vault.bitwarden.eu/api`                                                                                                     | `https://vault.bitwarden.com/api`      |
| `SECRET_PROVIDER_IDENTITY_URL`      | US: `https://vault.bitwarden.com/identity`</br> EU: `https://vault.bitwarden.eu/identity`                                                                                           | `https://vault.bitwarden.com/identity` |
| `SECRET_PROVIDER_ACCESS_TOKEN`      | Access token of a machine account, see the docs for [machine accounts](https://bitwarden.com/help/machine-accounts/) and [access-tokens](https://bitwarden.com/help/access-tokens/) |                                        |
| `SECRET_PROVIDER_ACCESS_TOKEN_FILE` | Path to the file containing the access token inside the container                                                                                                                   |                                        |

## Deployment configuration

Add a mapping/reference between the environment variable you want to set in the docker compose project/stack and the ID of the secret in Bitwarden Secrets Manager.

### Example

For example in your `.doco-cd.yml`:

```yaml title=".doco-cd.yml"
name: myapp
external_secrets:
  DB_PASSWORD: 138e3a97-ed58-431c-b366-b35500663411
```
