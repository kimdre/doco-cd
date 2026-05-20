---
tags:
  - Advanced
  - Secrets
  - Configuration
---

# Infisical

Infisical runs as a [gRPC plugin container](index.md#plugin-architecture) (`ghcr.io/kimdre/doco-cd-secretprovider-infisical`) sitting next to `doco-cd`.

## Environment Variables

### `doco-cd` container

| Key                             | Value                                                                            |
|---------------------------------|----------------------------------------------------------------------------------|
| `SECRET_PROVIDER`               | `grpc`                                                                           |
| `SECRET_PROVIDER_GRPC_ENDPOINT` | Endpoint of the plugin. Default: `unix:///var/run/doco-cd/secret-provider.sock`. |

### Plugin container (`doco-cd-secretprovider-infisical`)

| Key                                  | Value                                                                                                                                                    |
|--------------------------------------|----------------------------------------------------------------------------------------------------------------------------------------------------------|
| `SECRET_PROVIDER_GRPC_ENDPOINT`      | Endpoint the plugin listens on (must match the value on `doco-cd`).                                                                                      |
| `SECRET_PROVIDER_SITE_URL`           | The URL of the Infisical site (e.g. `https://app.infisical.com`, `https://eu.infisical.com` or your self-hosted instance URL)                            |
| `SECRET_PROVIDER_CLIENT_ID`          | The Client ID of a machine account, see the docs for [machine accounts](https://infisical.com/docs/documentation/platform/identities/machine-identities) |
| `SECRET_PROVIDER_CLIENT_SECRET`      | The Client Secret of a machine account ([Universal Auth](https://infisical.com/docs/documentation/platform/identities/universal-auth))                   |
| `SECRET_PROVIDER_CLIENT_SECRET_FILE` | Path to the file containing the client secret inside the container                                                                                       |

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
    image: ghcr.io/kimdre/doco-cd-secretprovider-infisical:latest
    environment:
      SECRET_PROVIDER_SITE_URL: https://app.infisical.com
      SECRET_PROVIDER_CLIENT_ID: ${INFISICAL_CLIENT_ID}
      SECRET_PROVIDER_CLIENT_SECRET: ${INFISICAL_CLIENT_SECRET}
    volumes:
      - secret-provider-sock:/var/run/doco-cd

volumes:
  secret-provider-sock:
```

## Deployment configuration

Add a mapping/reference between the environment variable you want to set in the docker compose project/stack and the reference to the secret in Infisical.

A valid secret reference should use the syntax:
`projectId:env:[/some/path/]key`

!!! warning
    Machine accounts can only access projects for which you have granted read permissions.

### Example

For example in your `.doco-cd.yml`:

```yaml title=".doco-cd.yml"
name: myapp
external_secrets:
  TEST_PASSWORD: 0db45926-c97c-40d4-a3aa-fefd5d5fb492:dev:DATABASE_URL
  OTHER_PASSWORD: "0db45926-c97c-40d4-a3aa-fefd5d5fb492:dev:/Test/Sub/TEST_SECRET"
  USERNAME: 0db45926-c97c-40d4-a3aa-fefd5d5fb492:dev:Test/Sub/TEST_SECRET
```
