---
tags:
  - Advanced
  - Secrets
  - Configuration
---

# Bitwarden Secrets Manager

Bitwarden Secrets Manager runs as a [gRPC plugin container](index.md#plugin-architecture) (`ghcr.io/kimdre/doco-cd-secretprovider-bitwardensecretsmanager`) sitting next to `doco-cd`.

!!! warning
    The Bitwarden Secrets Manager plugin image is not published for ARMv7 architectures (e.g. Raspberry Pi OS 32-bit) because the Bitwarden Go SDK does not support 32-bit ARM.

## Environment Variables

### `doco-cd` container

| Key                             | Value                                                                            |
|---------------------------------|----------------------------------------------------------------------------------|
| `SECRET_PROVIDER`               | `grpc`                                                                           |
| `SECRET_PROVIDER_GRPC_ENDPOINT` | Endpoint of the plugin. Default: `unix:///var/run/doco-cd/secret-provider.sock`. |

### Plugin container (`doco-cd-secretprovider-bitwardensecretsmanager`)

| Key                                 | Value                                                                                                                                                                               | Default                                |
|-------------------------------------|-------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|:---------------------------------------|
| `SECRET_PROVIDER_GRPC_ENDPOINT`     | Endpoint the plugin listens on (must match the value on `doco-cd`).                                                                                                                 | `unix:///var/run/doco-cd/secret-provider.sock` |
| `SECRET_PROVIDER_API_URL`           | US: `https://vault.bitwarden.com/api`</br> EU: `https://vault.bitwarden.eu/api`                                                                                                     | `https://vault.bitwarden.com/api`      |
| `SECRET_PROVIDER_IDENTITY_URL`      | US: `https://vault.bitwarden.com/identity`</br> EU: `https://vault.bitwarden.eu/identity`                                                                                           | `https://vault.bitwarden.com/identity` |
| `SECRET_PROVIDER_ACCESS_TOKEN`      | Access token of a machine account, see the docs for [machine accounts](https://bitwarden.com/help/machine-accounts/) and [access-tokens](https://bitwarden.com/help/access-tokens/) |                                        |
| `SECRET_PROVIDER_ACCESS_TOKEN_FILE` | Path to the file containing the access token inside the container                                                                                                                   |                                        |

### Example compose layout

```yaml title="docker-compose.yml"
services:
  doco-cd:
    image: ghcr.io/kimdre/doco-cd:latest
    environment:
      SECRET_PROVIDER: grpc
    volumes:
      - secret-provider-sock:/var/run/doco-cd
    depends_on:
      - secret-provider

  secret-provider:
    image: ghcr.io/kimdre/doco-cd-secretprovider-bitwardensecretsmanager:latest
    environment:
      SECRET_PROVIDER_ACCESS_TOKEN: ${BWS_ACCESS_TOKEN}
    volumes:
      - secret-provider-sock:/var/run/doco-cd

volumes:
  secret-provider-sock:
```

## Deployment configuration

Add a mapping/reference between the environment variable you want to set in the docker compose project/stack and the ID of the secret in Bitwarden Secrets Manager.

### Example

For example in your `.doco-cd.yml`:

```yaml title=".doco-cd.yml"
name: myapp
external_secrets:
  DB_PASSWORD: 138e3a97-ed58-431c-b366-b35500663411
```
